//go:build integration

package invariants_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const testNow = "2026-06-15T12:00:00Z"

var fixtureSeq atomic.Int64

// world is one isolated database with a real recon_executor pool, a fixed
// clock, the fixture accounts/scope/batch/match-run, and the executor service.
type world struct {
	db       *testdb.DB
	pool     *pgxpool.Pool
	ledger   *ledger.Service
	clock    *clock.Fake
	accounts map[string]string
	scopeID  string
	batchID  string
	matchRun string
	service  *executor.Service
}

// seededCase is one break/case/proposal graph created by seedCase.
type seededCase struct {
	caseID      string
	breakID     string
	proposalID  string
	externalRef string
	ledgerTxnID string
}

// appliedWorld is the valid applied adjustment + link fixture plus one
// approved bystander proposal that was never applied.
type appliedWorld struct {
	adjustment *seededCase
	link       *seededCase
	bystander  *seededCase
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

func newWorld(t *testing.T) *world {
	t.Helper()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 8})
	ctx := context.Background()
	clk := clock.NewFake(mustTime(t, testNow))
	ledgerSvc := ledger.NewService(clk, config.LedgerConfig{
		AdjustmentAccounts: []string{"fees_expense", "suspense", "fx_gain_loss", "rounding"},
	})

	w := &world{
		db:       d,
		ledger:   ledgerSvc,
		clock:    clk,
		accounts: make(map[string]string),
	}
	for _, account := range []struct{ code, typ string }{
		{"psp_clearing_usd", "asset"},
		{"fees_expense", "expense"},
		{"suspense", "liability"},
		{"fx_gain_loss", "revenue"},
	} {
		w.insertAccount(t, account.code, account.typ, "USD")
	}

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD') RETURNING id::text`,
		w.accounts["psp_clearing_usd"]).Scan(&w.scopeID); err != nil {
		t.Fatalf("insert scope: %v", err)
	}

	sum := sha256.Sum256([]byte("invariants-fixture-batch"))
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ('psp_a', 0, 'USD', $1, 0, 0) RETURNING id::text`,
		hex.EncodeToString(sum[:])).Scan(&w.batchID); err != nil {
		t.Fatalf("insert settlement batch: %v", err)
	}

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'invariants-fixture')
		 RETURNING id::text`, w.scopeID, clk.Now()).Scan(&w.matchRun); err != nil {
		t.Fatalf("insert match run: %v", err)
	}

	rolePool, err := db.OpenRole(ctx, db.RoleExecutor, d.DSNFor(db.RoleExecutor))
	if err != nil {
		t.Fatalf("open recon_executor pool: %v", err)
	}
	t.Cleanup(rolePool.Close)
	w.pool = rolePool.Pool

	stateSvc, err := state.NewService(w.pool, clk)
	if err != nil {
		t.Fatalf("new state service: %v", err)
	}
	svc, err := executor.NewService(executor.Options{
		Pool:          w.pool,
		Ledger:        ledgerSvc,
		State:         stateSvc,
		Clock:         clk,
		ExecutorRunID: "executor:invariants:run-1",
		BatchSize:     4,
		PollInterval:  time.Millisecond,
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new executor service: %v", err)
	}
	w.service = svc
	return w
}

func (w *world) insertAccount(t *testing.T, code, typ, currency string) {
	t.Helper()
	var id string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, $3, 'active') RETURNING id::text`,
		code, typ, currency).Scan(&id); err != nil {
		t.Fatalf("insert account %q: %v", code, err)
	}
	w.accounts[code] = id
}

// seedCase creates one external reference, an optional posted ledger
// transaction, the break/case graph, and an approved proposal with a matching
// state hash. Adjustment proposals get the canonical lines payload; link
// proposals get the singular-ledger-txn payload naming the seeded transaction.
func (w *world) seedCase(t *testing.T, proposalType string, withLedger bool) *seededCase {
	t.Helper()
	ctx := context.Background()
	fixture := &seededCase{}

	recordID := fmt.Sprintf("rec-%d", fixtureSeq.Add(1))
	fixture.externalRef = "ext:psp_a:" + recordID
	w.insertExternalRecord(t, recordID, "2026-02-03", 1234)

	ledgerTxnIDs := []string{}
	if withLedger {
		fixture.ledgerTxnID = w.insertLedgerTxn(t, "2026-01-15")
		ledgerTxnIDs = append(ledgerTxnIDs, fixture.ledgerTxnID)
	}

	fixture.breakID = w.insertBreak(t, ledgerTxnIDs, recordID)
	fixture.caseID = w.insertCase(t, fixture.breakID)

	evidence := []string{
		"case:" + fixture.caseID,
		fixture.externalRef,
	}
	if fixture.ledgerTxnID != "" {
		evidence = append(evidence, "ledger_txn:"+fixture.ledgerTxnID)
	}
	resolved, err := proposal.ResolveState(ctx, w.db.Pool, fixture.caseID, evidence)
	if err != nil {
		t.Fatalf("resolve proposal state: %v", err)
	}
	stateHash, err := proposal.Hash(resolved)
	if err != nil {
		t.Fatalf("hash proposal state: %v", err)
	}

	proposalID, err := proposal.NewUUID()
	if err != nil {
		t.Fatalf("generate proposal id: %v", err)
	}
	fixture.proposalID = proposalID
	payload, err := proposalPayload(t, proposalType, fixture.externalRef, fixture.ledgerTxnID)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if _, err := w.db.Pool.Exec(ctx,
		`INSERT INTO proposals
		     (id, case_id, agent_run_id, type, payload, rationale, evidence, confidence,
		      status, version, state_hash, idempotency_key, created_at, expires_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::jsonb, 'invariants fixture',
		         $6::jsonb, 0.9000, $7, 0, $8, $9, $10, $11)`,
		proposalID, fixture.caseID, w.agentRunID(t, fixture.caseID), proposalType,
		string(payload), string(evidenceJSON), "approved", stateHash,
		"invariants-fixture-"+proposalID, w.clock.Now(), nil); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	if _, err := w.db.Pool.Exec(ctx,
		`UPDATE cases SET status = 'proposal_pending', version = version + 1 WHERE id = $1::uuid`,
		fixture.caseID); err != nil {
		t.Fatalf("move case to proposal_pending: %v", err)
	}
	return fixture
}

func proposalPayload(t *testing.T, proposalType, externalRef, ledgerTxnID string) ([]byte, error) {
	t.Helper()
	switch proposalType {
	case "adjustment", "write_off":
		return json.Marshal(map[string]any{
			"lines": []map[string]any{
				{"account_code": "fees_expense", "direction": "debit", "amount_minor": 500, "currency": "USD"},
				{"account_code": "suspense", "direction": "credit", "amount_minor": 500, "currency": "USD"},
			},
		})
	case "link":
		return json.Marshal(map[string]any{
			"external_ref":  externalRef,
			"ledger_txn_id": ledgerTxnID,
		})
	default:
		t.Fatalf("unsupported proposal type %q", proposalType)
		return nil, nil
	}
}

func (w *world) agentRunID(t *testing.T, caseID string) string {
	t.Helper()
	var id string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'fixture-model', 'v1', $2) RETURNING id::text`,
		caseID, w.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("insert agent run: %v", err)
	}
	return id
}

func (w *world) insertExternalRecord(t *testing.T, recordID, valueDate string, amountMinor int64) {
	t.Helper()
	sum := sha256.Sum256([]byte("invariants-ext-" + recordID))
	kind := "charge"
	if amountMinor < 0 {
		kind = "payout"
	}
	if _, err := w.db.Pool.Exec(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ('psp_a', $1, $2, $3, 'USD', 0, $4, $5::date,
		         $6::uuid, '{}'::jsonb, $7)`,
		recordID, kind, amountMinor, w.clock.Now(), valueDate, w.batchID,
		hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("insert external record %q: %v", recordID, err)
	}
}

// insertLedgerTxn posts a balanced fixture transaction and returns its id.
func (w *world) insertLedgerTxn(t *testing.T, valueDate string) string {
	t.Helper()
	return w.postLedgerTxn(t, fmt.Sprintf("invariants-fixture-txn-%d", fixtureSeq.Add(1)), valueDate)
}

// postLedgerTxn posts a balanced fixture transaction with an explicit
// idempotency key. The key is the ledger side of the applied-record ownership
// rule (proposal:<proposal id> for a real apply).
func (w *world) postLedgerTxn(t *testing.T, idempotencyKey, valueDate string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture ledger tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	actor, err := audit.SystemActor("fixture")
	if err != nil {
		t.Fatalf("fixture actor: %v", err)
	}
	result, err := w.ledger.Post(ctx, tx, ledger.PostRequest{
		IdempotencyKey: idempotencyKey,
		Kind:           ledger.KindCharge,
		ValueDate:      mustDate(t, valueDate),
		Description:    "invariants fixture ledger transaction",
		Actor:          actor,
		Lines: []ledger.Line{
			{AccountID: w.accounts["psp_clearing_usd"], Direction: ledger.Debit, AmountMinor: 500, Currency: "USD"},
			{AccountID: w.accounts["fx_gain_loss"], Direction: ledger.Credit, AmountMinor: 500, Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("post fixture ledger txn: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture ledger txn: %v", err)
	}
	return result.ID
}

// reverseLedgerTxn reverses a fixture transaction through the real ledger
// service and returns the reversal transaction id. The reversal audit action
// is ledger.reverse (FIN-11), unlike the ledger.post of the original.
func (w *world) reverseLedgerTxn(t *testing.T, txnID string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture reversal tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	actor, err := audit.SystemActor("fixture")
	if err != nil {
		t.Fatalf("fixture actor: %v", err)
	}
	result, err := w.ledger.Reverse(ctx, tx, ledger.ReverseRequest{
		TxnID:          txnID,
		Reason:         "invariants fixture reversal",
		IdempotencyKey: fmt.Sprintf("invariants-fixture-reversal-%d", fixtureSeq.Add(1)),
		Actor:          actor,
	})
	if err != nil {
		t.Fatalf("reverse fixture ledger txn: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture reversal: %v", err)
	}
	if result.Kind != ledger.KindReversal {
		t.Fatalf("reversal kind = %q, want %q", result.Kind, ledger.KindReversal)
	}
	if result.ReversalOf == nil || *result.ReversalOf != txnID {
		t.Fatalf("reversal_of = %v, want %s", result.ReversalOf, txnID)
	}
	return result.ID
}

func (w *world) insertBreak(t *testing.T, ledgerTxnIDs []string, recordID string) string {
	t.Helper()
	side := "external_only"
	if len(ledgerTxnIDs) > 0 {
		side = "amount_mismatch"
	}
	signature := sha256.Sum256([]byte(fmt.Sprintf("invariants-break-%d", fixtureSeq.Add(1))))
	refs, err := json.Marshal([]map[string]string{{
		"source":           "psp_a",
		"source_record_id": recordID,
	}})
	if err != nil {
		t.Fatalf("marshal break external refs: %v", err)
	}
	if ledgerTxnIDs == nil {
		ledgerTxnIDs = []string{}
	}
	var id string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, $5::text[]::uuid[],
		         $6::jsonb, 500, 'USD', 'in_case', $7)
		 RETURNING id::text`,
		hex.EncodeToString(signature[:]), w.scopeID, w.matchRun, side,
		ledgerTxnIDs, string(refs), w.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("insert break: %v", err)
	}
	return id
}

func (w *world) insertCase(t *testing.T, breakID string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := w.db.Pool.QueryRow(ctx,
		`INSERT INTO cases (value_minor, currency, status, version, attempts, created_at)
		 VALUES (500, 'USD', 'in_agent', 0, 0, $1) RETURNING id::text`,
		w.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("insert case: %v", err)
	}
	if _, err := w.db.Pool.Exec(ctx,
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`, id, breakID, w.clock.Now()); err != nil {
		t.Fatalf("insert case_break: %v", err)
	}
	return id
}

// apply runs the executor and requires a committed apply.
func (w *world) apply(t *testing.T, proposalID string) executor.Result {
	t.Helper()
	result, err := w.service.Apply(context.Background(), proposalID)
	if err != nil {
		t.Fatalf("apply %s: %v", proposalID, err)
	}
	if result.Status != executor.StatusApplied {
		t.Fatalf("apply %s status = %q, want %q (failure %q)",
			proposalID, result.Status, executor.StatusApplied, result.FailureReason)
	}
	return result
}

// seedAppliedWorld applies one adjustment and one link proposal. A third
// approved proposal stays unapplied for the applied-status corruption test.
func (w *world) seedAppliedWorld(t *testing.T) *appliedWorld {
	t.Helper()
	fixtures := &appliedWorld{
		adjustment: w.seedCase(t, "adjustment", false),
		link:       w.seedCase(t, "link", true),
		bystander:  w.seedCase(t, "adjustment", false),
	}
	w.apply(t, fixtures.adjustment.proposalID)
	w.apply(t, fixtures.link.proposalID)
	return fixtures
}

// appliedLedgerTxn returns the applied record's ledger transaction id, or "".
func (w *world) appliedLedgerTxn(t *testing.T, proposalID string) string {
	t.Helper()
	var id *string
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT ledger_txn_id::text FROM applied_proposals WHERE proposal_id = $1::uuid`,
		proposalID).Scan(&id); err != nil {
		t.Fatalf("read applied ledger txn for %s: %v", proposalID, err)
	}
	if id == nil {
		return ""
	}
	return *id
}

// deleteApplied removes the applied record for one proposal.
func (w *world) deleteApplied(t *testing.T, proposalID string) {
	t.Helper()
	tag, err := w.db.Pool.Exec(context.Background(),
		`DELETE FROM applied_proposals WHERE proposal_id = $1::uuid`, proposalID)
	if err != nil {
		t.Fatalf("delete applied record: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("deleted %d applied rows, want 1", tag.RowsAffected())
	}
}

// setAppliedLedger points an applied record at ledgerTxnID ("" means NULL).
func (w *world) setAppliedLedger(t *testing.T, proposalID, ledgerTxnID string) {
	t.Helper()
	var value any
	if ledgerTxnID != "" {
		value = ledgerTxnID
	}
	tag, err := w.db.Pool.Exec(context.Background(),
		`UPDATE applied_proposals SET ledger_txn_id = $2::uuid WHERE proposal_id = $1::uuid`,
		proposalID, value)
	if err != nil {
		t.Fatalf("update applied ledger txn: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("updated %d applied rows, want 1", tag.RowsAffected())
	}
}

// insertAppliedOn writes an applied record for proposalID with a NULL ledger
// transaction id.
func (w *world) insertAppliedOn(t *testing.T, proposalID string) {
	t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(),
		`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, NULL, $2, 'executor:invariants:corrupt')`,
		proposalID, w.clock.Now()); err != nil {
		t.Fatalf("insert applied record: %v", err)
	}
}

func (w *world) setBreakStatus(t *testing.T, breakID, status string) {
	t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE breaks SET status = $2 WHERE id = $1::uuid`, breakID, status); err != nil {
		t.Fatalf("set break status: %v", err)
	}
}

func (w *world) setCaseStatus(t *testing.T, caseID, status string) {
	t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE cases SET status = $2 WHERE id = $1::uuid`, caseID, status); err != nil {
		t.Fatalf("set case status: %v", err)
	}
}

func (w *world) deleteOutboxRow(t *testing.T, proposalID string) {
	t.Helper()
	tag, err := w.db.Pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE idempotency_key = $1`, "proposal.applied:"+proposalID)
	if err != nil {
		t.Fatalf("delete outbox row: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("deleted %d outbox rows, want 1", tag.RowsAffected())
	}
}

func (w *world) markOutboxDead(t *testing.T, proposalID string) {
	t.Helper()
	tag, err := w.db.Pool.Exec(context.Background(),
		`UPDATE outbox SET dead = true WHERE idempotency_key = $1`,
		"proposal.applied:"+proposalID)
	if err != nil {
		t.Fatalf("mark outbox dead: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("updated %d outbox rows, want 1", tag.RowsAffected())
	}
}

// deleteAuditRows removes matching audit rows by temporarily disabling the
// append-only delete trigger. The maintenance pool owns the test database, and
// the trigger is restored before the helper returns.
func (w *world) deleteAuditRows(t *testing.T, action, entityID, afterStatus string) {
	t.Helper()
	ctx := context.Background()
	if _, err := w.db.Pool.Exec(ctx,
		`ALTER TABLE audit_log DISABLE TRIGGER audit_log_reject_delete`); err != nil {
		t.Fatalf("disable audit delete trigger: %v", err)
	}
	defer func() {
		if _, err := w.db.Pool.Exec(context.Background(),
			`ALTER TABLE audit_log ENABLE TRIGGER audit_log_reject_delete`); err != nil {
			t.Errorf("re-enable audit delete trigger: %v", err)
		}
	}()

	query := `DELETE FROM audit_log WHERE action = $1 AND entity_id = $2`
	args := []any{action, entityID}
	if afterStatus != "" {
		query += ` AND after->>'status' = $3`
		args = append(args, afterStatus)
	}
	tag, err := w.db.Pool.Exec(ctx, query, args...)
	if err != nil {
		t.Fatalf("delete audit rows: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("deleted %d audit rows, want 1", tag.RowsAffected())
	}
}
