//go:build integration

// covers: FIN-52
package integrity_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/integrity"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

const (
	fixtureNow      = "2026-06-15T12:00:00Z"
	fixtureRunDate  = "2026-06-15"
	fixtureValueDay = "2026-06-01"
	sentinelMemo    = "SENTINEL_RAW_MEMO_DO_NOT_PERSIST_7f3a"
)

var fixtureSeq atomic.Int64

// fixture is a purpose-built clean world: one balanced and matched clearing
// transaction, one applied adjustment with its full trail, a valid audit chain,
// and one statement that ties. Tests corrupt exactly what they assert.
type fixture struct {
	db       *testdb.DB
	clock    *clock.Fake
	ledger   *ledger.Service
	accounts map[string]string

	periodID    string
	scopeID     string
	batchID     string
	matchRunID  string
	externalID  string
	chargeTxnID string
	statementID string

	proposalID      string
	adjustmentTxnID string
	breakID         string
	caseID          string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 8})
	ctx := context.Background()
	clk := clock.NewFake(mustTime(t, fixtureNow))
	ledgerSvc := ledger.NewService(clk, config.LedgerConfig{
		AdjustmentAccounts: []string{"fees_expense", "suspense"},
	})

	f := &fixture{
		db:       d,
		clock:    clk,
		ledger:   ledgerSvc,
		accounts: make(map[string]string),
	}
	for _, account := range []struct{ code, typ string }{
		{"psp_clearing_usd", "asset"},
		{"fees_expense", "expense"},
		{"suspense", "liability"},
	} {
		f.insertAccount(t, account.code, account.typ)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT id::text FROM periods WHERE status = 'open' ORDER BY start_date LIMIT 1`).
		Scan(&f.periodID); err != nil {
		t.Fatalf("read open period: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD') RETURNING id::text`,
		f.accounts["psp_clearing_usd"]).Scan(&f.scopeID); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	f.batchID = f.insertBatch(t, "psp_a", "USD")
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'integrity-fixture')
		 RETURNING id::text`, f.scopeID, clk.Now()).Scan(&f.matchRunID); err != nil {
		t.Fatalf("insert match run: %v", err)
	}

	f.externalID = f.insertExternal(t, "clean-charge-1", 10000, fixtureValueDay)
	f.chargeTxnID = f.postLedger(t, "fixture-charge-1", ledger.KindCharge, fixtureValueDay, []ledger.Line{
		{AccountID: f.accounts["psp_clearing_usd"], Direction: ledger.Debit, AmountMinor: 10000, Currency: "USD"},
		{AccountID: f.accounts["fees_expense"], Direction: ledger.Credit, AmountMinor: 10000, Currency: "USD"},
	})
	f.insertMatch(t, f.scopeID, f.externalID, f.chargeTxnID, 10000)

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO statement_balances
		     (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
		 VALUES ($1::uuid, $2::date, 0, 10000, 'USD', $3::uuid)
		 RETURNING id::text`, f.scopeID, fixtureRunDate, f.batchID).Scan(&f.statementID); err != nil {
		t.Fatalf("insert statement: %v", err)
	}

	f.seedAppliedProposal(t)
	return f
}

func (f *fixture) insertAccount(t *testing.T, code, typ string) {
	t.Helper()
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, 'USD', 'active') RETURNING id::text`,
		code, typ).Scan(&id); err != nil {
		t.Fatalf("insert account %q: %v", code, err)
	}
	f.accounts[code] = id
}

func (f *fixture) insertBatch(t *testing.T, source, currency string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%s-%d", source, currency, fixtureSeq.Add(1))))
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ($1, 0, $2, $3, 0, 0) RETURNING id::text`,
		source, currency, hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	return id
}

func (f *fixture) insertExternal(t *testing.T, recordID string, amountMinor int64, valueDate string) string {
	t.Helper()
	return f.insertExternalFromSource(t, "psp_a", recordID, amountMinor, valueDate)
}

// insertExternalFromSource seeds one ingested record under an explicit source
// so a test can attribute a match to a different scope than the record's own
// source.
func (f *fixture) insertExternalFromSource(t *testing.T, source, recordID string, amountMinor int64, valueDate string) string {
	t.Helper()
	raw := fmt.Sprintf(`{"memo":%q}`, sentinelMemo)
	sum := sha256.Sum256([]byte(raw))
	kind := "charge"
	if amountMinor < 0 {
		kind = "payout"
	}
	date := mustDate(t, valueDate)
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ($1, $2, $3, $4, 'USD', 0, $5, $6::date, $7::uuid, $8::jsonb, $9)
		 RETURNING id::text`,
		source, recordID, kind, amountMinor, date.Add(12*time.Hour), valueDate,
		f.batchID, raw, hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert external record %q/%q: %v", source, recordID, err)
	}
	return id
}

// insertStatementScope adds an asset clearing account, a scope, and one
// zero-amount statement that ties at the fixture run date. It is used to prove
// cross-scope attribution and that one broken scope does not stop the others.
func (f *fixture) insertStatementScope(t *testing.T, source string) string {
	t.Helper()
	f.insertAccount(t, source+"_clearing_usd", "asset")
	var scopeID, batchID string
	ctx := context.Background()
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, 'USD') RETURNING id::text`,
		f.accounts[source+"_clearing_usd"], source).Scan(&scopeID); err != nil {
		t.Fatalf("insert scope %q: %v", source, err)
	}
	batchID = f.insertBatch(t, source, "USD")
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO statement_balances
		     (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
		 VALUES ($1::uuid, $2::date, 0, 0, 'USD', $3::uuid)`,
		scopeID, fixtureRunDate, batchID); err != nil {
		t.Fatalf("insert statement for scope %q: %v", source, err)
	}
	return scopeID
}

// setStatementClosing updates the one fixture statement's closing amount.
func (f *fixture) setStatementClosing(t *testing.T, statementID string, closingMinor int64) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE statement_balances SET closing_minor = $2 WHERE id = $1::uuid`,
		statementID, closingMinor); err != nil {
		t.Fatalf("set statement closing: %v", err)
	}
}

// deleteStatement removes the fixture statement so the scope is skipped.
func (f *fixture) deleteStatement(t *testing.T, statementID string) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`DELETE FROM statement_balances WHERE id = $1::uuid`, statementID); err != nil {
		t.Fatalf("delete statement: %v", err)
	}
}

// clearAppliedLedger is the owner-side applied-proposal corruption: it removes
// the ledger transaction reference the invariant requires.
func (f *fixture) clearAppliedLedger(t *testing.T) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE applied_proposals SET ledger_txn_id = NULL WHERE proposal_id = $1::uuid`,
		f.proposalID); err != nil {
		t.Fatalf("clear applied ledger: %v", err)
	}
}

// deleteExecutorAudit removes the executor.applied audit row with audit
// triggers disabled, then restores them before commit.
func (f *fixture) deleteExecutorAudit(t *testing.T) {
	t.Helper()
	f.disableAuditTriggers(t, func(ctx context.Context, tx pgx.Tx) {
		tag, err := tx.Exec(ctx,
			`DELETE FROM audit_log
			  WHERE action = 'executor.applied' AND entity_type = 'proposal' AND entity_id = $1`,
			f.proposalID)
		if err != nil {
			t.Fatalf("delete executor.applied audit: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("deleted %d executor.applied audit rows, want 1", tag.RowsAffected())
		}
	})
}

// postLedger posts a balanced transaction through the real ledger service, so
// it carries its ledger.post audit row and satisfies the applied-trail
// invariants.
func (f *fixture) postLedger(t *testing.T, key string, kind ledger.Kind, valueDate string, lines []ledger.Line) string {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture ledger tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	actor, err := audit.SystemActor("integrity-fixture")
	if err != nil {
		t.Fatalf("fixture actor: %v", err)
	}
	result, err := f.ledger.Post(ctx, tx, ledger.PostRequest{
		IdempotencyKey: key,
		Kind:           kind,
		ValueDate:      mustDate(t, valueDate),
		Description:    "integrity fixture",
		Actor:          actor,
		Lines:          lines,
	})
	if err != nil {
		t.Fatalf("post fixture ledger txn %q: %v", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture ledger txn %q: %v", key, err)
	}
	return result.ID
}

func (f *fixture) insertMatch(t *testing.T, scopeID, externalID, ledgerTxnID string, amountMinor int64) {
	t.Helper()
	ctx := context.Background()
	var matchID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO matches
		     (scope_id, external_record_id, rule, confidence, match_run_id, status, created_at)
		 VALUES ($1::uuid, $2::uuid, 'exact', 1.0, $3::uuid, 'active', $4)
		 RETURNING id::text`,
		scopeID, externalID, f.matchRunID, f.clock.Now()).Scan(&matchID); err != nil {
		t.Fatalf("insert match: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO match_legs
		     (match_id, ledger_txn_id, amount_minor, currency, status)
		 VALUES ($1::uuid, $2::uuid, $3, 'USD', 'active')`,
		matchID, ledgerTxnID, amountMinor); err != nil {
		t.Fatalf("insert match leg: %v", err)
	}
}

// seedAppliedProposal creates the valid FIN-52 applied trail: an applied
// adjustment, its owned ledger transaction, resolved break/case, the four
// required audit rows, and the proposal.applied outbox row.
func (f *fixture) seedAppliedProposal(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := f.db.Pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&f.proposalID); err != nil {
		t.Fatalf("generate proposal id: %v", err)
	}

	signature := sha256.Sum256([]byte("integrity-break-" + f.proposalID))
	refs, err := json.Marshal([]map[string]string{{
		"source":           "psp_a",
		"source_record_id": "clean-charge-1",
	}})
	if err != nil {
		t.Fatalf("marshal break refs: %v", err)
	}
	f.adjustmentTxnID = f.postLedger(t, "proposal:"+f.proposalID, ledger.KindAdjustment, fixtureValueDay, []ledger.Line{
		{AccountID: f.accounts["fees_expense"], Direction: ledger.Debit, AmountMinor: 500, Currency: "USD"},
		{AccountID: f.accounts["suspense"], Direction: ledger.Credit, AmountMinor: 500, Currency: "USD"},
	})

	var agentRunID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO cases (value_minor, currency, status, version, attempts, created_at, resolved_at)
		 VALUES (500, 'USD', 'resolved', 0, 0, $1, $1) RETURNING id::text`,
		f.clock.Now()).Scan(&f.caseID); err != nil {
		t.Fatalf("insert resolved case: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'fixture-model', 'v1', $2) RETURNING id::text`,
		f.caseID, f.clock.Now()).Scan(&agentRunID); err != nil {
		t.Fatalf("insert agent run: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids, external_refs,
		      delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, 'amount_mismatch', $4::text[]::uuid[],
		         $5::jsonb, 500, 'USD', 'resolved', $6)
		 RETURNING id::text`,
		hex.EncodeToString(signature[:]), f.scopeID, f.matchRunID,
		[]string{f.adjustmentTxnID}, string(refs), f.clock.Now()).Scan(&f.breakID); err != nil {
		t.Fatalf("insert resolved break: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`, f.caseID, f.breakID, f.clock.Now()); err != nil {
		t.Fatalf("insert case break: %v", err)
	}

	payload := `{"lines":[{"account_code":"fees_expense","direction":"debit","amount_minor":500,"currency":"USD"}]}`
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO proposals
		     (id, case_id, agent_run_id, type, payload, rationale, evidence, confidence,
		      status, version, state_hash, idempotency_key, created_at, expires_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'adjustment', $4::jsonb,
		         'integrity fixture', '[]'::jsonb, 0.9000, 'applied', 0,
		         'fixture-state-hash', $5, $6, NULL)`,
		f.proposalID, f.caseID, agentRunID, payload, "fixture-proposal-"+f.proposalID,
		f.clock.Now()); err != nil {
		t.Fatalf("insert applied proposal: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, $2::uuid, $3, 'executor:integrity:fixture')`,
		f.proposalID, f.adjustmentTxnID, f.clock.Now()); err != nil {
		t.Fatalf("insert applied record: %v", err)
	}

	f.appendAudit(t, "executor.applied", "proposal", f.proposalID, nil)
	f.appendAudit(t, "proposal.transition", "proposal", f.proposalID, map[string]any{"status": "applied"})
	f.appendAudit(t, "break.resolve", "break", f.breakID, map[string]any{"proposal_id": f.proposalID})
	f.appendAudit(t, "case.resolve", "case", f.caseID, map[string]any{"proposal_id": f.proposalID})

	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES ('proposal', $1, 'proposal.applied', '{}'::jsonb,
		         'proposal.applied:' || $1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		f.proposalID); err != nil {
		t.Fatalf("insert proposal.applied outbox row: %v", err)
	}
}

func (f *fixture) appendAudit(t *testing.T, action, entityType, entityID string, after any) {
	t.Helper()
	actor, err := audit.SystemActor("integrity-fixture")
	if err != nil {
		t.Fatalf("fixture actor: %v", err)
	}
	testutil.AppendAudit(t, context.Background(), f.db.Pool, audit.Entry{
		Actor:      actor,
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		After:      after,
		Timestamp:  f.clock.Now(),
	})
}

// appPool opens the production app role pool for Run; Run never uses the
// maintenance pool.
func (f *fixture) appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	rolePool, err := db.OpenRole(context.Background(), db.RoleApp, f.db.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(rolePool.Close)
	return rolePool.Pool
}

// run executes the job through recon_app with the fixture clock and run date.
func (f *fixture) run(t *testing.T, opts integrity.Options) (integrity.Result, error) {
	t.Helper()
	if opts.Clock == nil {
		opts.Clock = f.clock
	}
	if opts.RunDate.IsZero() {
		opts.RunDate = integrity.NewDate(mustDate(t, fixtureRunDate))
	}
	return integrity.Run(context.Background(), f.appPool(t), opts)
}

func (f *fixture) mustRun(t *testing.T, opts integrity.Options) integrity.Result {
	t.Helper()
	result, err := f.run(t, opts)
	if err != nil {
		t.Fatalf("integrity run: %v", err)
	}
	return result
}

// tamper executes mutate in a transaction with triggers disabled, then
// restores session_replication_role before commit. It is the frozen seed path
// for ledger and audit corruption that the app role cannot perform.
func (f *fixture) tamper(t *testing.T, mutate func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tamper tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	mutate(ctx, tx)
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = origin`); err != nil {
		t.Fatalf("restore triggers: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tamper tx: %v", err)
	}
}

// disableAuditTriggers runs mutate with row/statement triggers disabled on
// audit_log, then restores them. It does not touch session_replication_role, so
// normal table constraints still apply.
func (f *fixture) disableAuditTriggers(t *testing.T, mutate func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin audit tamper tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_log DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable audit triggers: %v", err)
	}
	mutate(ctx, tx)
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_log ENABLE TRIGGER USER`); err != nil {
		t.Fatalf("re-enable audit triggers: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit audit tamper tx: %v", err)
	}
}

// checkByName returns one check result, failing when the report shape is wrong.
func checkByName(t *testing.T, report integrity.Report, name integrity.CheckName) integrity.CheckResult {
	t.Helper()
	if len(report.Checks) != len(integrity.CheckOrder) {
		t.Fatalf("report has %d checks, want %d", len(report.Checks), len(integrity.CheckOrder))
	}
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("report has no %q check", name)
	return integrity.CheckResult{}
}

// requireProblemCode asserts that name carries code under the given check.
func requireProblemCode(t *testing.T, report integrity.Report, name integrity.CheckName, code string) integrity.Problem {
	t.Helper()
	check := checkByName(t, report, name)
	if check.Status != integrity.StatusFailed {
		t.Fatalf("check %q status = %q, want failed", name, check.Status)
	}
	for _, problem := range check.Problems {
		if problem.Code == code {
			return problem
		}
	}
	t.Fatalf("check %q has no problem code %q: %+v", name, code, check.Problems)
	return integrity.Problem{}
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

func mustDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		t.Fatalf("parse date %q: %v", value, err)
	}
	return parsed
}
