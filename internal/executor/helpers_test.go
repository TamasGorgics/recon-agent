//go:build integration

// covers: FIN-20, FIN-21, FIN-23

package executor_test

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
// clock, the apply accounts/scope/batch/match-run, and the executor service.
type world struct {
	db        *testdb.DB
	pool      *pgxpool.Pool
	service   *executor.Service
	ledgerSvc *ledger.Service
	clock     *clock.Fake
	accounts  map[string]string
	scopeID   string
	batchID   string
	matchRun  string
}

// testLine is one adjustment/write-off payload line.
type testLine struct {
	AccountCode string
	Direction   string
	AmountMinor int64
	Currency    string
}

// caseSpec seeds one approved proposal with a case, break(s), and evidence.
type caseSpec struct {
	proposalType string
	lines        []testLine
	evidence     []string

	// withExternal seeds a psp_a external record and references it from the
	// break; externalValueDate is its value_date and externalAmount its signed
	// minor amount (default 1234).
	withExternal      bool
	externalValueDate string
	externalAmount    int64
	// withLedger seeds a posted ledger transaction and references it from the
	// break; ledgerValueDate is its value_date.
	withLedger      bool
	ledgerValueDate string
	// citedLedger seeds a posted ledger transaction and cites it as
	// ledger_txn: evidence without adding it to the break ledger_txn_ids.
	citedLedger bool
	// externalRefMissing writes a break external_refs entry with no matching
	// external record, to exercise the clock-date fallback.
	externalRefMissing bool

	proposalStatus string
	expiresAt      *time.Time
	// skipProposalPending leaves the case in_agent after the proposal insert
	// (the default moves it to proposal_pending like the agent loop does).
	skipProposalPending bool
}

// caseFixture is the seeded graph a test can mutate and assert on.
type caseFixture struct {
	caseID           string
	breakID          string
	proposalID       string
	externalRecordID string
	ledgerTxnID      string
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
		db:        d,
		ledgerSvc: ledgerSvc,
		clock:     clk,
		accounts:  make(map[string]string),
	}
	for _, account := range []struct{ code, typ string }{
		{"psp_clearing_usd", "asset"},
		{"fees_expense", "expense"},
		{"suspense", "liability"},
		{"fx_gain_loss", "revenue"},
		{"rounding", "expense"},
	} {
		w.insertAccount(t, account.code, account.typ, "USD")
	}

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD') RETURNING id::text`,
		w.accounts["psp_clearing_usd"]).Scan(&w.scopeID); err != nil {
		t.Fatalf("insert scope: %v", err)
	}

	sum := sha256.Sum256([]byte("executor-fixture-batch"))
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ('psp_a', 0, 'USD', $1, 0, 0) RETURNING id::text`,
		hex.EncodeToString(sum[:])).Scan(&w.batchID); err != nil {
		t.Fatalf("insert settlement batch: %v", err)
	}

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'executor-fixture')
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
		ExecutorRunID: "executor:test:run-1",
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

// newCase seeds one case/break/proposal graph and returns the IDs.
func (w *world) newCase(t *testing.T, spec caseSpec) *caseFixture {
	t.Helper()
	ctx := context.Background()
	if spec.proposalType == "" {
		spec.proposalType = "adjustment"
	}
	if spec.lines == nil {
		spec.lines = []testLine{
			{AccountCode: "fees_expense", Direction: "debit", AmountMinor: 500, Currency: "USD"},
			{AccountCode: "suspense", Direction: "credit", AmountMinor: 500, Currency: "USD"},
		}
	}
	if spec.externalValueDate == "" {
		spec.externalValueDate = "2026-02-03"
	}
	if spec.externalAmount == 0 {
		spec.externalAmount = 1234
	}
	if spec.ledgerValueDate == "" {
		spec.ledgerValueDate = "2026-01-15"
	}
	if spec.proposalStatus == "" {
		spec.proposalStatus = "approved"
	}
	if !spec.withExternal && !spec.withLedger && !spec.citedLedger && !spec.externalRefMissing {
		spec.withExternal = true
	}

	fixture := &caseFixture{}
	var (
		ledgerTxnIDs []string
		externalRefs = []storedBreakRef{}
	)
	if spec.withExternal {
		recordID := fmt.Sprintf("rec-%d", fixtureSeq.Add(1))
		fixture.externalRecordID = w.insertExternalRecord(t, recordID, spec.externalValueDate, spec.externalAmount)
		externalRefs = append(externalRefs, storedBreakRef{Source: "psp_a", SourceRecordID: recordID})
	}
	if spec.withLedger || spec.citedLedger {
		fixture.ledgerTxnID = w.insertLedgerTxn(t, spec.ledgerValueDate)
		if spec.withLedger {
			ledgerTxnIDs = append(ledgerTxnIDs, fixture.ledgerTxnID)
		}
	}
	if spec.externalRefMissing {
		externalRefs = append(externalRefs, storedBreakRef{
			Source:         "psp_a",
			SourceRecordID: fmt.Sprintf("missing-%d", fixtureSeq.Add(1)),
		})
	}

	side := "external_only"
	if spec.withLedger && spec.withExternal {
		side = "amount_mismatch"
	} else if spec.withLedger {
		side = "ledger_only"
	}
	fixture.breakID = w.insertBreak(t, side, ledgerTxnIDs, externalRefs, 500)
	fixture.caseID = w.insertCase(t, []string{fixture.breakID}, "in_agent")

	if spec.evidence == nil {
		spec.evidence = []string{"case:" + fixture.caseID}
		if fixture.externalRecordID != "" {
			var record string
			if err := w.db.Pool.QueryRow(ctx,
				`SELECT source_record_id FROM external_records WHERE id = $1::uuid`,
				fixture.externalRecordID).Scan(&record); err != nil {
				t.Fatalf("read external record id: %v", err)
			}
			spec.evidence = append(spec.evidence, "ext:psp_a:"+record)
		}
		if fixture.ledgerTxnID != "" {
			spec.evidence = append(spec.evidence, "ledger_txn:"+fixture.ledgerTxnID)
		}
	}

	resolved, err := proposal.ResolveState(ctx, w.db.Pool, fixture.caseID, spec.evidence)
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
	payload, err := json.Marshal(map[string]any{"lines": payloadLines(spec.lines)})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	evidence, err := json.Marshal(spec.evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if _, err := w.db.Pool.Exec(ctx,
		`INSERT INTO proposals
		     (id, case_id, agent_run_id, type, payload, rationale, evidence, confidence,
		      status, version, state_hash, idempotency_key, created_at, expires_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::jsonb, 'executor fixture',
		         $6::jsonb, 0.9000, $7, 0, $8, $9, $10, $11)`,
		proposalID, fixture.caseID, w.agentRunID(t, fixture.caseID), spec.proposalType,
		string(payload), string(evidence), spec.proposalStatus, stateHash,
		"executor-fixture-"+proposalID, w.clock.Now(), spec.expiresAt); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	if !spec.skipProposalPending {
		w.transitionCase(t, fixture.caseID, "proposal_pending")
	}
	return fixture
}

func payloadLines(lines []testLine) []map[string]any {
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		out = append(out, map[string]any{
			"account_code": line.AccountCode,
			"direction":    line.Direction,
			"amount_minor": line.AmountMinor,
			"currency":     line.Currency,
		})
	}
	return out
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

func (w *world) insertExternalRecord(t *testing.T, recordID, valueDate string, amountMinor int64) string {
	t.Helper()
	sum := sha256.Sum256([]byte("executor-ext-" + recordID))
	kind := "charge"
	if amountMinor < 0 {
		kind = "payout"
	}
	var id string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ('psp_a', $1, $2, $3, 'USD', 0, $4, $5::date,
		         $6::uuid, '{}'::jsonb, $7)
		 RETURNING id::text`,
		recordID, kind, amountMinor, w.clock.Now(), valueDate, w.batchID,
		hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert external record %q: %v", recordID, err)
	}
	return id
}

func (w *world) insertLedgerTxn(t *testing.T, valueDate string) string {
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
	result, err := w.ledgerSvc.Post(ctx, tx, ledger.PostRequest{
		IdempotencyKey: fmt.Sprintf("executor-fixture-txn-%d", fixtureSeq.Add(1)),
		Kind:           ledger.KindCharge,
		ValueDate:      mustDate(t, valueDate),
		Description:    "executor fixture ledger transaction",
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

type storedBreakRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

func (w *world) insertBreak(t *testing.T, side string, ledgerTxnIDs []string, externalRefs []storedBreakRef, delta int64) string {
	t.Helper()
	ctx := context.Background()
	signature := sha256.Sum256([]byte(fmt.Sprintf("executor-break-%d", fixtureSeq.Add(1))))
	refs, err := json.Marshal(externalRefs)
	if err != nil {
		t.Fatalf("marshal break external refs: %v", err)
	}
	if ledgerTxnIDs == nil {
		ledgerTxnIDs = []string{}
	}
	var id string
	if err := w.db.Pool.QueryRow(ctx,
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, $5::text[]::uuid[],
		         $6::jsonb, $7, 'USD', 'in_case', $8)
		 RETURNING id::text`,
		hex.EncodeToString(signature[:]), w.scopeID, w.matchRun, side,
		ledgerTxnIDs, string(refs), delta, w.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("insert break: %v", err)
	}
	return id
}

func (w *world) insertCase(t *testing.T, breakIDs []string, status string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := w.db.Pool.QueryRow(ctx,
		`INSERT INTO cases (value_minor, currency, status, version, attempts, created_at)
		 VALUES (500, 'USD', $1, 0, 0, $2) RETURNING id::text`,
		status, w.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("insert case: %v", err)
	}
	for _, breakID := range breakIDs {
		if _, err := w.db.Pool.Exec(ctx,
			`INSERT INTO case_breaks (case_id, break_id, created_at)
			 VALUES ($1::uuid, $2::uuid, $3)`, id, breakID, w.clock.Now()); err != nil {
			t.Fatalf("insert case_break: %v", err)
		}
	}
	return id
}

func (w *world) transitionCase(t *testing.T, caseID, status string) {
	t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE cases SET status = $2, version = version + 1 WHERE id = $1::uuid`,
		caseID, status); err != nil {
		t.Fatalf("transition fixture case to %s: %v", status, err)
	}
}

// options returns the executor dependency set for this world.
func (w *world) options() executor.Options {
	stateSvc, err := state.NewService(w.pool, w.clock)
	if err != nil {
		panic(err)
	}
	return executor.Options{
		Pool:          w.pool,
		Ledger:        w.ledgerSvc,
		State:         stateSvc,
		Clock:         w.clock,
		ExecutorRunID: "executor:test:run-1",
		BatchSize:     4,
		PollInterval:  time.Millisecond,
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	}
}

// withAdjustmentAccounts builds an executor service whose ledger allowlist is
// exactly codes, for deterministic ledger-error tests.
func (w *world) withAdjustmentAccounts(t *testing.T, codes ...string) *executor.Service {
	t.Helper()
	stateSvc, err := state.NewService(w.pool, w.clock)
	if err != nil {
		t.Fatalf("new state service: %v", err)
	}
	opts := w.options()
	opts.Ledger = ledger.NewService(w.clock, config.LedgerConfig{AdjustmentAccounts: codes})
	opts.State = stateSvc
	svc, err := executor.NewService(opts)
	if err != nil {
		t.Fatalf("new restricted executor service: %v", err)
	}
	return svc
}

func (w *world) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := w.db.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func (w *world) oneString(t *testing.T, query string, args ...any) string {
	t.Helper()
	var value string
	if err := w.db.Pool.QueryRow(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("scalar query %q: %v", query, err)
	}
	return value
}

func (w *world) status(t *testing.T, caseID string) string {
	t.Helper()
	return w.oneString(t, `SELECT status FROM cases WHERE id = $1::uuid`, caseID)
}

func (w *world) breakStatus(t *testing.T, breakID string) string {
	t.Helper()
	return w.oneString(t, `SELECT status FROM breaks WHERE id = $1::uuid`, breakID)
}

func (w *world) assertNoApplySideEffects(t *testing.T, fixture *caseFixture) {
	t.Helper()
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, fixture.proposalID); got != 0 {
		t.Errorf("applied_proposals rows = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`, "proposal:"+fixture.proposalID); got != 0 {
		t.Errorf("ledger_txns for proposal = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+fixture.proposalID); got != 0 {
		t.Errorf("outbox rows for proposal = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'executor.applied' AND entity_id = $1`,
		fixture.proposalID); got != 0 {
		t.Errorf("executor.applied audit rows = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'ledger.post' AND entity_id IN
		     (SELECT id::text FROM ledger_txns WHERE idempotency_key = $1)`,
		"proposal:"+fixture.proposalID); got != 0 {
		t.Errorf("ledger.post audit rows = %d, want 0", got)
	}
	if got := w.breakStatus(t, fixture.breakID); got != "in_case" {
		t.Errorf("break status = %s, want in_case", got)
	}
	if got := w.status(t, fixture.caseID); got != "proposal_pending" {
		t.Errorf("case status = %s, want proposal_pending", got)
	}
}

func (w *world) assertAppliedOnce(t *testing.T, fixture *caseFixture, wantLedgerTxnID string) {
	t.Helper()
	var (
		status  string
		version int
	)
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT status, version FROM proposals WHERE id = $1::uuid`, fixture.proposalID).
		Scan(&status, &version); err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if status != "applied" {
		t.Errorf("proposal status = %s, want applied", status)
	}
	if version != 1 {
		t.Errorf("proposal version = %d, want 1", version)
	}
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, fixture.proposalID); got != 1 {
		t.Errorf("applied_proposals rows = %d, want 1", got)
	}
	if got := w.oneString(t,
		`SELECT COALESCE(ledger_txn_id::text, '') FROM applied_proposals WHERE proposal_id = $1::uuid`,
		fixture.proposalID); got != wantLedgerTxnID {
		t.Errorf("applied ledger_txn_id = %q, want %q", got, wantLedgerTxnID)
	}
	if got := w.count(t,
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+fixture.proposalID); got != 1 {
		t.Errorf("outbox rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'executor.applied' AND entity_id = $1`,
		fixture.proposalID); got != 1 {
		t.Errorf("executor.applied audit rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'proposal.transition' AND entity_id = $1`,
		fixture.proposalID); got != 1 {
		t.Errorf("proposal.transition audit rows = %d, want 1", got)
	}
	if got := w.breakStatus(t, fixture.breakID); got != "resolved" {
		t.Errorf("break status = %s, want resolved", got)
	}
	if got := w.status(t, fixture.caseID); got != "resolved" {
		t.Errorf("case status = %s, want resolved", got)
	}
	var resolutionType *string
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT resolution_type FROM cases WHERE id = $1::uuid`, fixture.caseID).
		Scan(&resolutionType); err != nil {
		t.Fatalf("read case resolution_type: %v", err)
	}
	if resolutionType == nil || *resolutionType == "" {
		t.Error("case resolution_type is empty, want the proposal type")
	}
}
