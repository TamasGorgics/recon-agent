//go:build integration

package proposal_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// The integration fixture is deliberately small and deterministic. It creates
// the accounts, scope, batch, external records, balanced ledger transaction,
// case, break, and agent run directly; read evidence is produced only by real
// tools.Runner calls, so the proposal evidence check exercises the same
// tool_calls rows production will write.

const (
	integrationPeriodID = "11111111-1111-4111-8111-111111111111"
	otherCaseID         = "99999999-9999-4999-8999-999999999999"

	accountClearing = "it_clearing_usd"
	accountRevenue  = "it_revenue_usd"
	accountFees     = "it_fees_usd"
	accountSuspense = "it_suspense_usd"
	accountCash     = "it_cash_usd"
	accountInactive = "it_inactive_usd"
	accountFeesEUR  = "it_fees_eur"
)

var integrationSeq atomic.Int64

func integrationTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

func integrationSignature(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

type proposalWorld struct {
	DB     *testdb.DB
	App    *pgxpool.Pool
	Agent  *pgxpool.Pool
	Ledger config.LedgerConfig
	Clock  *clock.Fake

	Registry *tools.Registry

	ScopeID     string
	MatchRunID  string
	BatchID     string
	LedgerTxnID string
	LedgerTxn   string
	ExternalRef string
	DuplicateOf string
	BatchRef    string
}

type proposalScenario struct {
	CaseID  string
	BreakID string
}

func newProposalWorld(t *testing.T) *proposalWorld {
	t.Helper()
	ctx := context.Background()
	d := testdb.New(t)

	world := &proposalWorld{
		DB:    d,
		App:   openRolePool(t, d, db.RoleApp),
		Agent: openRolePool(t, d, db.RoleAgentRO),
		Ledger: config.LedgerConfig{AdjustmentAccounts: []string{
			accountFees, accountSuspense, accountInactive, accountFeesEUR,
		}},
		Clock: clock.NewFake(integrationTime(t, "2026-01-02T03:04:05Z")),
	}

	clearingID := insertIntegrationAccount(t, d, accountClearing, "asset", "USD", "active")
	insertIntegrationAccount(t, d, accountRevenue, "revenue", "USD", "active")
	insertIntegrationAccount(t, d, accountFees, "expense", "USD", "active")
	insertIntegrationAccount(t, d, accountSuspense, "liability", "USD", "active")
	insertIntegrationAccount(t, d, accountCash, "asset", "USD", "active")
	insertIntegrationAccount(t, d, accountInactive, "expense", "USD", "inactive")
	insertIntegrationAccount(t, d, accountFeesEUR, "expense", "EUR", "active")

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD')
		 RETURNING id::text`, clearingID).Scan(&world.ScopeID); err != nil {
		t.Fatalf("create scope: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, config_hash)
		 VALUES ($1::uuid, '2026-01-01T00:00:00Z', '2026-01-01T00:00:01Z', 'succeeded', 'proposal-it')
		 RETURNING id::text`, world.ScopeID).Scan(&world.MatchRunID); err != nil {
		t.Fatalf("create match run: %v", err)
	}

	seq := integrationSeq.Add(1)
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, status, row_count, declared_row_count)
		 VALUES ('psp_a', 2000, 'USD', $1, 'ingested', 2, 2)
		 RETURNING id::text`, fmt.Sprintf("proposal-it-batch-%d", seq)).Scan(&world.BatchID); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	world.BatchRef, _ = tools.BatchRef(world.BatchID)

	insertIntegrationExternalRecord(t, d, world.BatchID, "psp_a", "rec-1", 1000, "2026-01-02")
	insertIntegrationExternalRecord(t, d, world.BatchID, "psp_a", "rec-2", 1000, "2026-01-02")
	world.ExternalRef = "ext:psp_a:rec-1"
	world.DuplicateOf = "ext:psp_a:rec-2"

	world.LedgerTxnID = insertIntegrationLedgerTxn(t, d, clearingID)
	world.LedgerTxn, _ = tools.LedgerTxnRef(world.LedgerTxnID)

	registry := tools.NewRegistry()
	if err := tools.RegisterPart1(registry, tools.Deps{Agent: world.Agent, Clock: world.Clock}); err != nil {
		t.Fatalf("RegisterPart1: %v", err)
	}
	if err := proposal.Register(registry, proposal.Deps{
		App:              world.App,
		AgentExec:        world.Agent,
		Clock:            world.Clock,
		Ledger:           world.Ledger,
		WriteOffAccounts: []string{accountSuspense},
	}); err != nil {
		t.Fatalf("proposal.Register: %v", err)
	}
	world.Registry = registry
	return world
}

func openRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	rolePool, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(rolePool.Close)
	return rolePool.Pool
}

func insertIntegrationAccount(t *testing.T, d *testdb.DB, code, accountType, currency, status string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, $3, $4)
		 RETURNING id::text`, code, accountType, currency, status).Scan(&id); err != nil {
		t.Fatalf("create account %s: %v", code, err)
	}
	return id
}

func insertIntegrationExternalRecord(t *testing.T, d *testdb.DB, batchID, source, recordID string, amountMinor int64, valueDate string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ($1, $2, 'charge', $3, 'USD', 0,
		         '2026-01-02T00:00:00Z', $4::date, $5::uuid, $6::jsonb, $7)
		 RETURNING id::text`,
		source, recordID, amountMinor, valueDate, batchID,
		`{"reference":"`+recordID+`"}`, integrationSignature("raw-"+recordID)).Scan(&id); err != nil {
		t.Fatalf("create external record %s/%s: %v", source, recordID, err)
	}
	return id
}

// insertIntegrationLedgerTxn writes one balanced charge inside a database
// transaction because FIN-10 is a deferred constraint.
func insertIntegrationLedgerTxn(t *testing.T, d *testdb.DB, clearingID string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin ledger txn: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	seq := integrationSeq.Add(1)
	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ('2026-01-02', 'charge', $1, $2, 'proposal integration fixture', $3::uuid)
		 RETURNING id::text`,
		fmt.Sprintf("proposal-it-txn-%d", seq), fmt.Sprintf("hash-%d", seq), integrationPeriodID).Scan(&txnID); err != nil {
		t.Fatalf("create ledger txn: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries
		     (txn_id, account_id, direction, amount_minor, currency, posted_at)
		 VALUES ($1::uuid, $2::uuid, 'debit', 1000, 'USD', '2026-01-02T00:00:00Z')`,
		txnID, clearingID); err != nil {
		t.Fatalf("create debit entry: %v", err)
	}
	var revenueID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM accounts WHERE code = $1`, accountRevenue).Scan(&revenueID); err != nil {
		t.Fatalf("load revenue account: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries
		     (txn_id, account_id, direction, amount_minor, currency, posted_at)
		 VALUES ($1::uuid, $2::uuid, 'credit', 1000, 'USD', '2026-01-02T00:00:00Z')`,
		txnID, revenueID); err != nil {
		t.Fatalf("create credit entry: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ledger txn: %v", err)
	}
	return txnID
}

func (w *proposalWorld) newScenario(t *testing.T) *proposalScenario {
	t.Helper()
	return w.newScenarioWithStatus(t, "open")
}

func (w *proposalWorld) newScenarioWithStatus(t *testing.T, status string) *proposalScenario {
	t.Helper()
	ctx := context.Background()
	scenario := &proposalScenario{}
	if err := w.DB.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES (0, 500, 'USD', $1, 0, 0, '2026-01-02T00:00:00Z')
		 RETURNING id::text`, status).Scan(&scenario.CaseID); err != nil {
		t.Fatalf("create case: %v", err)
	}

	seq := integrationSeq.Add(1)
	refs := fmt.Sprintf(`[{"source":"psp_a","source_record_id":"rec-1"}]`)
	if err := w.DB.Pool.QueryRow(ctx,
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids, external_refs,
		      delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, 'amount_mismatch', ARRAY[$4::uuid],
		         $5::jsonb, 500, 'USD', 'open', '2026-01-02T00:00:00Z')
		 RETURNING id::text`,
		integrationSignature(fmt.Sprintf("proposal-it-break-%d", seq)),
		w.ScopeID, w.MatchRunID, w.LedgerTxnID, refs).Scan(&scenario.BreakID); err != nil {
		t.Fatalf("create break: %v", err)
	}
	if _, err := w.DB.Pool.Exec(ctx,
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, '2026-01-02T00:00:00Z')`,
		scenario.CaseID, scenario.BreakID); err != nil {
		t.Fatalf("attach break to case: %v", err)
	}
	return scenario
}

// newCompositeScenario attaches one external-only break to a fresh case. The
// composite legs are deliberately not attached to the case; that is the shape a
// payout-to-N charges link must support.
func (w *proposalWorld) newCompositeScenario(t *testing.T, externalRecordID string) *proposalScenario {
	t.Helper()
	ctx := context.Background()
	scenario := &proposalScenario{}
	if err := w.DB.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES (0, 3000, 'USD', 'open', 0, 0, '2026-01-02T00:00:00Z')
		 RETURNING id::text`).Scan(&scenario.CaseID); err != nil {
		t.Fatalf("create composite case: %v", err)
	}

	seq := integrationSeq.Add(1)
	refs := fmt.Sprintf(`[{"source":"psp_a","source_record_id":%q}]`, externalRecordID)
	if err := w.DB.Pool.QueryRow(ctx,
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids, external_refs,
		      delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, 'external_only', ARRAY[]::uuid[],
		         $4::jsonb, 3000, 'USD', 'open', '2026-01-02T00:00:00Z')
		 RETURNING id::text`,
		integrationSignature(fmt.Sprintf("proposal-it-composite-break-%d", seq)),
		w.ScopeID, w.MatchRunID, refs).Scan(&scenario.BreakID); err != nil {
		t.Fatalf("create composite break: %v", err)
	}
	if _, err := w.DB.Pool.Exec(ctx,
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, '2026-01-02T00:00:00Z')`,
		scenario.CaseID, scenario.BreakID); err != nil {
		t.Fatalf("attach composite break to case: %v", err)
	}
	return scenario
}

// addPhantomLedgerBreak attaches a ledger-only break naming a UUID that has no
// ledger_txns row. Breaks do not foreign-key their ID arrays, so get_case can
// legitimately return evidence for a record that later fails existence
// validation.
func (w *proposalWorld) addPhantomLedgerBreak(t *testing.T, scenario *proposalScenario, ledgerTxnID string) {
	t.Helper()
	seq := integrationSeq.Add(1)
	var breakID string
	if err := w.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids, external_refs,
		      delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, 'ledger_only', ARRAY[$4::uuid],
		         '[]'::jsonb, 1000, 'USD', 'open', '2026-01-02T00:00:00Z')
		 RETURNING id::text`,
		integrationSignature(fmt.Sprintf("proposal-it-phantom-break-%d", seq)),
		w.ScopeID, w.MatchRunID, ledgerTxnID).Scan(&breakID); err != nil {
		t.Fatalf("create phantom break: %v", err)
	}
	if _, err := w.DB.Pool.Exec(context.Background(),
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, '2026-01-02T00:00:00Z')`,
		scenario.CaseID, breakID); err != nil {
		t.Fatalf("attach phantom break to case: %v", err)
	}
}

// insertIntegrationLedgerTxnOnAccounts writes one balanced transaction whose
// entries never touch the reconciliation scope's ledger account. It models a
// ledger leg from another scope.
func insertIntegrationLedgerTxnOnAccounts(t *testing.T, d *testdb.DB, debitCode, creditCode string, amountMinor int64) string {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin out-of-scope ledger txn: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	seq := integrationSeq.Add(1)
	var txnID, debitID, creditID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ('2026-01-02', 'charge', $1, $2, 'proposal out-of-scope fixture', $3::uuid)
		 RETURNING id::text`,
		fmt.Sprintf("proposal-it-out-of-scope-txn-%d", seq), fmt.Sprintf("hash-out-of-scope-%d", seq),
		integrationPeriodID).Scan(&txnID); err != nil {
		t.Fatalf("create out-of-scope ledger txn: %v", err)
	}
	for _, item := range []struct {
		code string
		id   *string
	}{{debitCode, &debitID}, {creditCode, &creditID}} {
		if err := tx.QueryRow(ctx, `SELECT id::text FROM accounts WHERE code = $1`, item.code).Scan(item.id); err != nil {
			t.Fatalf("load account %s: %v", item.code, err)
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries
		     (txn_id, account_id, direction, amount_minor, currency, posted_at)
		 VALUES ($1::uuid, $2::uuid, 'debit', $3, 'USD', '2026-01-02T00:00:00Z'),
		        ($1::uuid, $4::uuid, 'credit', $3, 'USD', '2026-01-02T00:00:00Z')`,
		txnID, debitID, amountMinor, creditID); err != nil {
		t.Fatalf("create out-of-scope entries: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit out-of-scope ledger txn: %v", err)
	}
	return txnID
}

func (w *proposalWorld) insertRun(t *testing.T, caseID string) string {
	t.Helper()
	var runID string
	if err := w.App.QueryRow(context.Background(),
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', '2026-01-02T03:04:05Z')
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		t.Fatalf("create agent run: %v", err)
	}
	return runID
}

type proposalSession struct {
	t      *testing.T
	world  *proposalWorld
	caseID string
	runID  string
	runner *tools.Runner
	run    *tools.RunContext
	seq    int
}

func (w *proposalWorld) newSession(t *testing.T, scenario *proposalScenario) *proposalSession {
	t.Helper()
	runID := w.insertRun(t, scenario.CaseID)
	run := tools.NewRunContext(runID, scenario.CaseID, w.Agent, w.Clock)
	return &proposalSession{
		t:      t,
		world:  w,
		caseID: scenario.CaseID,
		runID:  runID,
		runner: tools.NewRunner(w.Registry, tools.NewDBRecorder(w.App), w.Clock),
		run:    run,
	}
}

// freshContext returns a session for the same run with a new RunContext whose
// in-memory evidence collector is empty, positioned after the calls already
// recorded for the run. It models replaying the run in a new process.
func (s *proposalSession) freshContext() *proposalSession {
	s.t.Helper()
	return &proposalSession{
		t:      s.t,
		world:  s.world,
		caseID: s.caseID,
		runID:  s.runID,
		runner: tools.NewRunner(s.world.Registry, tools.NewDBRecorder(s.world.App), s.world.Clock),
		run:    tools.NewRunContext(s.runID, s.caseID, s.world.Agent, s.world.Clock),
		seq:    s.seq,
	}
}

func (s *proposalSession) call(name string, args any) map[string]any {
	s.t.Helper()
	s.seq++
	raw, err := s.runner.Call(context.Background(), s.run, s.seq, name, mustJSON(s.t, args))
	if err != nil {
		s.t.Fatalf("%s Call(): %v", name, err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		s.t.Fatalf("%s result is not JSON: %v\n%s", name, err, raw)
	}
	return envelope
}

// seedEvidence drives the real get_case read tool so successful tool_calls
// rows exist for the case, its ledger transaction, and the break's external
// record.
func (s *proposalSession) seedEvidence() []string {
	s.t.Helper()
	envelope := s.call("get_case", map[string]any{"case_id": s.caseID})
	if code := integrationEnvelopeErrorCode(envelope); code != "" {
		s.t.Fatalf("get_case error = %q: %v", code, envelope)
	}
	refs := s.run.RetrievedRefs()
	if len(refs) == 0 {
		s.t.Fatal("get_case returned no evidence refs")
	}
	return refs
}

// seedLedgerEvidence fetches the case and then each referenced ledger
// transaction through the real read tool so the run's tool_calls rows authorize
// the citation.
func (s *proposalSession) seedLedgerEvidence(txnIDs ...string) []string {
	s.t.Helper()
	s.seedEvidence()
	for _, txnID := range txnIDs {
		envelope := s.call("lookup_ledger_txn", map[string]any{"txn_id": txnID})
		if code := integrationEnvelopeErrorCode(envelope); code != "" {
			s.t.Fatalf("lookup_ledger_txn %s error = %q: %v", txnID, code, envelope)
		}
	}
	return s.run.RetrievedRefs()
}

func withoutRef(refs []string, drop string) []string {
	filtered := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref != drop {
			filtered = append(filtered, ref)
		}
	}
	return filtered
}

func (s *proposalSession) baseArgs(evidence []string) map[string]any {
	return map[string]any{
		"case_id":    s.caseID,
		"rationale":  "checked against the settlement break and ledger transaction",
		"evidence":   evidence,
		"confidence": 0.9,
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return raw
}

func integrationEnvelopeErrorCode(envelope map[string]any) string {
	errObject, ok := envelope["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObject["code"].(string)
	return code
}

func integrationEnvelopeErrorMessage(envelope map[string]any) string {
	errObject, ok := envelope["error"].(map[string]any)
	if !ok {
		return ""
	}
	message, _ := errObject["message"].(string)
	return message
}

func integrationEnvelopeData(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no data object: %v", envelope)
	}
	return data
}

func (w *proposalWorld) proposalCountByRun(t *testing.T, runID string) int {
	t.Helper()
	var count int
	if err := w.DB.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM proposals WHERE agent_run_id = $1::uuid`, runID).Scan(&count); err != nil {
		t.Fatalf("count proposals by run: %v", err)
	}
	return count
}

func (w *proposalWorld) proposalCountByCase(t *testing.T, caseID string) int {
	t.Helper()
	var count int
	if err := w.DB.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM proposals WHERE case_id = $1::uuid`, caseID).Scan(&count); err != nil {
		t.Fatalf("count proposals by case: %v", err)
	}
	return count
}

type storedProposal struct {
	ID              string
	CaseID          string
	RunID           string
	Type            string
	Payload         []byte
	Rationale       string
	Evidence        []byte
	Confidence      string
	PatternClaimed  *string
	PatternVerified *string
	Verification    []byte
	RiskTier        *string
	PolicyDecision  []byte
	Status          string
	Version         int
	StateHash       string
	IdempotencyKey  string
	ExpiresAt       *time.Time
	Supersedes      *string
}

func (w *proposalWorld) loadProposalByRun(t *testing.T, runID string) storedProposal {
	t.Helper()
	var row storedProposal
	if err := w.DB.Pool.QueryRow(context.Background(),
		`SELECT id::text, case_id::text, agent_run_id::text, type, payload, rationale,
		        evidence, confidence::text, pattern_claimed, pattern_verified,
		        verification, risk_tier, policy_decision, status, version,
		        state_hash, idempotency_key, expires_at, supersedes_proposal_id::text
		   FROM proposals
		  WHERE agent_run_id = $1::uuid`, runID).Scan(
		&row.ID, &row.CaseID, &row.RunID, &row.Type, &row.Payload,
		&row.Rationale, &row.Evidence, &row.Confidence, &row.PatternClaimed,
		&row.PatternVerified, &row.Verification, &row.RiskTier,
		&row.PolicyDecision, &row.Status, &row.Version, &row.StateHash,
		&row.IdempotencyKey, &row.ExpiresAt, &row.Supersedes,
	); err != nil {
		t.Fatalf("load proposal for run %s: %v", runID, err)
	}
	return row
}

func (w *proposalWorld) assertLastToolCallError(t *testing.T, runID, wantCode string) {
	t.Helper()
	var code string
	if err := w.App.QueryRow(context.Background(),
		`SELECT COALESCE(error, '') FROM tool_calls
		  WHERE agent_run_id = $1::uuid
		  ORDER BY seq DESC LIMIT 1`, runID).Scan(&code); err != nil {
		t.Fatalf("read last tool call: %v", err)
	}
	if code != wantCode {
		t.Fatalf("last tool call error = %q, want %q", code, wantCode)
	}
}

type caseSnapshot struct {
	Status           string
	Version          int
	Attempts         int
	EscalationReason string
	ResolutionType   string
}

func (w *proposalWorld) caseSnapshot(t *testing.T, caseID string) caseSnapshot {
	t.Helper()
	var snapshot caseSnapshot
	if err := w.DB.Pool.QueryRow(context.Background(),
		`SELECT status, version, attempts, COALESCE(escalation_reason, ''),
		        COALESCE(resolution_type, '')
		   FROM cases WHERE id = $1::uuid`, caseID).Scan(
		&snapshot.Status, &snapshot.Version, &snapshot.Attempts,
		&snapshot.EscalationReason, &snapshot.ResolutionType,
	); err != nil {
		t.Fatalf("snapshot case: %v", err)
	}
	return snapshot
}

func TestProposalFlowHappyPaths(t *testing.T) {
	world := newProposalWorld(t)

	tests := []struct {
		name     string
		tool     string
		fill     func(args map[string]any)
		wantType string
		pattern  string
	}{
		{
			name:     "adjustment",
			tool:     "propose_adjustment",
			wantType: "adjustment",
			pattern:  "rounding",
			fill: func(args map[string]any) {
				args["pattern"] = "rounding"
				args["lines"] = []map[string]any{
					{"account_code": accountFees, "direction": "debit", "amount_minor": 250, "currency": "USD"},
					{"account_code": accountSuspense, "direction": "credit", "amount_minor": 250, "currency": "USD"},
				}
			},
		},
		{
			name:     "write-off",
			tool:     "propose_write_off",
			wantType: "write_off",
			fill: func(args map[string]any) {
				args["lines"] = []map[string]any{
					{"account_code": accountFees, "direction": "debit", "amount_minor": 120, "currency": "USD"},
					{"account_code": accountSuspense, "direction": "credit", "amount_minor": 120, "currency": "USD"},
				}
			},
		},
		{
			name:     "link",
			tool:     "propose_link",
			wantType: "link",
			fill: func(args map[string]any) {
				args["ledger_txn_id"] = world.LedgerTxnID
				args["external_ref"] = world.ExternalRef
			},
		},
		{
			name:     "duplicate",
			tool:     "mark_duplicate",
			wantType: "duplicate",
			fill: func(args map[string]any) {
				args["external_ref"] = world.ExternalRef
				args["duplicate_of"] = world.DuplicateOf
			},
		},
		{
			name:     "request-info",
			tool:     "request_info",
			wantType: "request_info",
			fill: func(args map[string]any) {
				args["question"] = "Which invoice does this payout settle?"
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			scenario := world.newScenario(t)
			session := world.newSession(t, scenario)
			refs := session.seedEvidence()
			args := session.baseArgs(refs)
			test.fill(args)

			envelope := session.call(test.tool, args)
			if code := integrationEnvelopeErrorCode(envelope); code != "" {
				t.Fatalf("%s error = %q (%s): %v", test.tool, code, integrationEnvelopeErrorMessage(envelope), envelope)
			}
			data := integrationEnvelopeData(t, envelope)
			if data["status"] != "recorded" {
				t.Fatalf("%s status = %v, want recorded", test.tool, data["status"])
			}
			key, _ := data["idempotency_key"].(string)
			if len(key) != 64 {
				t.Fatalf("%s idempotency_key = %q, want 64 hex chars", test.tool, key)
			}
			if len(data) != 2 {
				t.Fatalf("%s result leaked fields: %v", test.tool, data)
			}
			for _, hidden := range []string{"payload", "rationale", "evidence", "state_hash"} {
				if _, ok := data[hidden]; ok {
					t.Fatalf("%s result leaked %q: %v", test.tool, hidden, data)
				}
			}

			row := world.loadProposalByRun(t, session.runID)
			if row.Type != test.wantType {
				t.Fatalf("stored type = %q, want %q", row.Type, test.wantType)
			}
			if row.Status != "proposed" {
				t.Fatalf("stored status = %q, want proposed", row.Status)
			}
			if row.Version != 0 {
				t.Fatalf("stored version = %d, want 0", row.Version)
			}
			if row.RiskTier != nil || row.PatternVerified != nil ||
				row.Verification != nil || row.PolicyDecision != nil {
				t.Fatalf("owner-only fields were populated: risk=%v pattern_verified=%v verification=%s policy=%s",
					row.RiskTier, row.PatternVerified, row.Verification, row.PolicyDecision)
			}
			if test.pattern == "" {
				if row.PatternClaimed != nil {
					t.Fatalf("pattern_claimed = %q, want NULL", *row.PatternClaimed)
				}
			} else if row.PatternClaimed == nil || *row.PatternClaimed != test.pattern {
				t.Fatalf("pattern_claimed = %v, want %q", row.PatternClaimed, test.pattern)
			}
			if row.IdempotencyKey != key {
				t.Fatalf("stored key = %q, result key = %q", row.IdempotencyKey, key)
			}
			if row.Confidence != "0.9000" {
				t.Fatalf("stored confidence = %q, want 0.9000", row.Confidence)
			}
			world.assertLastToolCallError(t, session.runID, "")

			// The stored state hash must match the canonical projection of the
			// state that the cited evidence resolves to right now.
			state, err := proposal.ResolveState(context.Background(), world.Agent, scenario.CaseID, refs)
			if err != nil {
				t.Fatalf("ResolveState(): %v", err)
			}
			hash, err := proposal.Hash(state)
			if err != nil {
				t.Fatalf("Hash(): %v", err)
			}
			if row.StateHash != hash {
				t.Fatalf("stored state_hash = %q, want %q", row.StateHash, hash)
			}
		})
	}
}

func (w *proposalWorld) accountID(t *testing.T, code string) string {
	t.Helper()
	var id string
	if err := w.DB.Pool.QueryRow(context.Background(),
		`SELECT id::text FROM accounts WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("load account %s: %v", code, err)
	}
	return id
}

type linkPayload struct {
	LedgerTxnID  *string  `json:"ledger_txn_id"`
	LedgerTxnIDs []string `json:"ledger_txn_ids"`
	ExternalRef  string   `json:"external_ref"`
}

func decodeLinkPayload(t *testing.T, row storedProposal) linkPayload {
	t.Helper()
	var payload linkPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("decode link payload: %v\n%s", err, row.Payload)
	}
	return payload
}

// TestProposalCompositeLink covers the M2-08 follow-up: an exact set of ledger
// transaction IDs is validated and stored, while singular links keep their old
// payload shape.
func TestProposalCompositeLink(t *testing.T) {
	world := newProposalWorld(t)
	clearingID := world.accountID(t, accountClearing)

	t.Run("three legs records exact set", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		session := world.newSession(t, scenario)
		ids := []string{
			insertIntegrationLedgerTxn(t, world.DB, clearingID),
			insertIntegrationLedgerTxn(t, world.DB, clearingID),
			insertIntegrationLedgerTxn(t, world.DB, clearingID),
		}
		refs := session.seedLedgerEvidence(ids...)
		args := session.baseArgs(refs)
		args["ledger_txn_ids"] = []string{ids[2], ids[0], ids[1]}
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != "" {
			t.Fatalf("propose_link error = %q: %v", code, envelope)
		}
		if status := integrationEnvelopeData(t, envelope)["status"]; status != "recorded" {
			t.Fatalf("status = %v, want recorded", status)
		}
		row := world.loadProposalByRun(t, session.runID)
		if row.Type != "link" {
			t.Fatalf("stored type = %q, want link", row.Type)
		}
		payload := decodeLinkPayload(t, row)
		want := append([]string(nil), ids...)
		sort.Strings(want)
		if !slices.Equal(payload.LedgerTxnIDs, want) {
			t.Fatalf("stored ledger set = %v, want %v", payload.LedgerTxnIDs, want)
		}
		if payload.LedgerTxnID != nil {
			t.Fatalf("plural payload contains singular ledger_txn_id: %v", *payload.LedgerTxnID)
		}
		if payload.ExternalRef != world.ExternalRef {
			t.Fatalf("external_ref = %q, want %q", payload.ExternalRef, world.ExternalRef)
		}
		state, err := proposal.ResolveState(context.Background(), world.Agent, scenario.CaseID, refs)
		if err != nil {
			t.Fatalf("ResolveState(): %v", err)
		}
		hash, err := proposal.Hash(state)
		if err != nil {
			t.Fatalf("Hash(): %v", err)
		}
		if row.StateHash != hash {
			t.Fatalf("stored state_hash = %q, want %q", row.StateHash, hash)
		}
	})

	t.Run("one-element plural is stored plural", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		session := world.newSession(t, scenario)
		txnID := insertIntegrationLedgerTxn(t, world.DB, clearingID)
		refs := session.seedLedgerEvidence(txnID)
		args := session.baseArgs(refs)
		args["ledger_txn_ids"] = []string{txnID}
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != "" {
			t.Fatalf("propose_link error = %q: %v", code, envelope)
		}
		payload := decodeLinkPayload(t, world.loadProposalByRun(t, session.runID))
		if payload.LedgerTxnID != nil {
			t.Fatal("one-element plural payload contains ledger_txn_id")
		}
		if !slices.Equal(payload.LedgerTxnIDs, []string{txnID}) {
			t.Fatalf("one-element plural set = %v, want [%s]", payload.LedgerTxnIDs, txnID)
		}
	})

	t.Run("singular payload unchanged", func(t *testing.T) {
		scenario := world.newScenario(t)
		session := world.newSession(t, scenario)
		refs := session.seedEvidence()
		args := session.baseArgs(refs)
		args["ledger_txn_id"] = world.LedgerTxnID
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != "" {
			t.Fatalf("propose_link error = %q: %v", code, envelope)
		}
		payload := decodeLinkPayload(t, world.loadProposalByRun(t, session.runID))
		if payload.LedgerTxnID == nil || *payload.LedgerTxnID != world.LedgerTxnID {
			t.Fatalf("singular payload = %+v", payload)
		}
		if payload.LedgerTxnIDs != nil {
			t.Fatalf("singular payload contains ledger_txn_ids: %v", payload.LedgerTxnIDs)
		}
	})

	t.Run("duplicate plural rejected", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		session := world.newSession(t, scenario)
		txnID := insertIntegrationLedgerTxn(t, world.DB, clearingID)
		refs := session.seedLedgerEvidence(txnID)
		args := session.baseArgs(refs)
		args["ledger_txn_ids"] = []string{txnID, txnID}
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeInvalidArguments {
			t.Fatalf("error = %q, want invalid_arguments: %v", code, envelope)
		}
		if count := world.proposalCountByRun(t, session.runID); count != 0 {
			t.Fatalf("rejected proposal wrote %d rows", count)
		}
	})

	t.Run("case-variant duplicate rejected", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		session := world.newSession(t, scenario)
		txnID := insertIntegrationLedgerTxn(t, world.DB, clearingID)
		refs := session.seedLedgerEvidence(txnID)
		args := session.baseArgs(refs)
		args["ledger_txn_ids"] = []string{strings.ToUpper(txnID), txnID}
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeInvalidArguments {
			t.Fatalf("error = %q, want invalid_arguments: %v", code, envelope)
		}
		if count := world.proposalCountByRun(t, session.runID); count != 0 {
			t.Fatalf("rejected proposal wrote %d rows", count)
		}
	})

	t.Run("leg not fetched rejected", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		session := world.newSession(t, scenario)
		txnID := insertIntegrationLedgerTxn(t, world.DB, clearingID)
		refs := session.seedEvidence()
		args := session.baseArgs(append(refs, "ledger_txn:"+txnID))
		args["ledger_txn_ids"] = []string{txnID}
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeEvidenceNotRetrieved {
			t.Fatalf("error = %q, want evidence_not_retrieved: %v", code, envelope)
		}
		if count := world.proposalCountByRun(t, session.runID); count != 0 {
			t.Fatalf("rejected proposal wrote %d rows", count)
		}
	})

	t.Run("leg fetched but not cited rejected", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		session := world.newSession(t, scenario)
		txnID := insertIntegrationLedgerTxn(t, world.DB, clearingID)
		refs := session.seedLedgerEvidence(txnID)
		args := session.baseArgs(withoutRef(refs, "ledger_txn:"+txnID))
		args["ledger_txn_ids"] = []string{txnID}
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeBusinessRuleViolation {
			t.Fatalf("error = %q, want business_rule_violation: %v", code, envelope)
		}
		if message := integrationEnvelopeErrorMessage(envelope); !strings.Contains(message, "not_in_evidence") {
			t.Fatalf("message = %q, want not_in_evidence", message)
		}
		if count := world.proposalCountByRun(t, session.runID); count != 0 {
			t.Fatalf("rejected proposal wrote %d rows", count)
		}
	})

	t.Run("external record not in case break rejected", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		session := world.newSession(t, scenario)
		txnID := insertIntegrationLedgerTxn(t, world.DB, clearingID)
		session.seedLedgerEvidence(txnID)
		lookup := session.call("lookup_external_record", map[string]any{"source": "psp_a", "record_id": "rec-2"})
		if code := integrationEnvelopeErrorCode(lookup); code != "" {
			t.Fatalf("lookup_external_record error = %q: %v", code, lookup)
		}
		args := session.baseArgs(session.run.RetrievedRefs())
		args["ledger_txn_ids"] = []string{txnID}
		args["external_ref"] = "ext:psp_a:rec-2"

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeBusinessRuleViolation {
			t.Fatalf("error = %q, want business_rule_violation: %v", code, envelope)
		}
		if message := integrationEnvelopeErrorMessage(envelope); !strings.Contains(message, "not_in_case_scope") {
			t.Fatalf("message = %q, want not_in_case_scope", message)
		}
		if count := world.proposalCountByRun(t, session.runID); count != 0 {
			t.Fatalf("rejected proposal wrote %d rows", count)
		}
	})

	t.Run("other scope leg rejected", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		session := world.newSession(t, scenario)
		inScope := insertIntegrationLedgerTxn(t, world.DB, clearingID)
		outOfScope := insertIntegrationLedgerTxnOnAccounts(t, world.DB, accountRevenue, accountSuspense, 1000)
		refs := session.seedLedgerEvidence(inScope, outOfScope)
		args := session.baseArgs(refs)
		args["ledger_txn_ids"] = []string{inScope, outOfScope}
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeBusinessRuleViolation {
			t.Fatalf("error = %q, want business_rule_violation: %v", code, envelope)
		}
		if message := integrationEnvelopeErrorMessage(envelope); !strings.Contains(message, "not_in_case_scope") {
			t.Fatalf("message = %q, want not_in_case_scope", message)
		}
		if count := world.proposalCountByRun(t, session.runID); count != 0 {
			t.Fatalf("rejected proposal wrote %d rows", count)
		}
	})

	t.Run("nonexistent leg rejected", func(t *testing.T) {
		scenario := world.newCompositeScenario(t, "rec-1")
		phantomID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		world.addPhantomLedgerBreak(t, scenario, phantomID)
		session := world.newSession(t, scenario)
		refs := session.seedEvidence()
		args := session.baseArgs(refs)
		args["ledger_txn_ids"] = []string{phantomID}
		args["external_ref"] = world.ExternalRef

		envelope := session.call("propose_link", args)
		if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeBusinessRuleViolation {
			t.Fatalf("error = %q, want business_rule_violation: %v", code, envelope)
		}
		if message := integrationEnvelopeErrorMessage(envelope); !strings.Contains(message, "ledger_txn_not_found") {
			t.Fatalf("message = %q, want ledger_txn_not_found", message)
		}
		if count := world.proposalCountByRun(t, session.runID); count != 0 {
			t.Fatalf("rejected proposal wrote %d rows", count)
		}
	})
}

// TestProposalExactReplayAndSecondProposal covers FIN-12: the same request in
// one run is an idempotent replay, while a different request is rejected
// without a second row.
//
// covers: FIN-12
func TestProposalExactReplayAndSecondProposal(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	session := world.newSession(t, scenario)
	refs := session.seedEvidence()

	args := session.baseArgs(refs)
	args["lines"] = []map[string]any{
		{"account_code": accountFees, "direction": "debit", "amount_minor": 250, "currency": "USD"},
		{"account_code": accountSuspense, "direction": "credit", "amount_minor": 250, "currency": "USD"},
	}

	first := session.call("propose_adjustment", args)
	if code := integrationEnvelopeErrorCode(first); code != "" {
		t.Fatalf("first proposal error = %q: %v", code, first)
	}
	firstData := integrationEnvelopeData(t, first)
	key, _ := firstData["idempotency_key"].(string)
	if firstData["status"] != "recorded" || len(key) != 64 {
		t.Fatalf("first proposal = %v", firstData)
	}

	replay := session.call("propose_adjustment", args)
	if code := integrationEnvelopeErrorCode(replay); code != "" {
		t.Fatalf("replay error = %q: %v", code, replay)
	}
	replayData := integrationEnvelopeData(t, replay)
	if replayData["status"] != "already_recorded" {
		t.Fatalf("replay status = %v, want already_recorded", replayData["status"])
	}
	if replayData["idempotency_key"] != key {
		t.Fatalf("replay key = %v, want %s", replayData["idempotency_key"], key)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 1 {
		t.Fatalf("proposals after replay = %d, want 1", count)
	}

	different := session.baseArgs(refs)
	different["lines"] = []map[string]any{
		{"account_code": accountFees, "direction": "debit", "amount_minor": 260, "currency": "USD"},
		{"account_code": accountSuspense, "direction": "credit", "amount_minor": 260, "currency": "USD"},
	}
	rejected := session.call("propose_adjustment", different)
	if code := integrationEnvelopeErrorCode(rejected); code != tools.CodeProposalAlreadyExists {
		t.Fatalf("second proposal error = %q, want proposal_already_exists: %v", code, rejected)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 1 {
		t.Fatalf("proposals after rejection = %d, want 1", count)
	}
	if session.run.State().ProposalKey() != key {
		t.Fatalf("run proposal slot = %q, want %q", session.run.State().ProposalKey(), key)
	}
	world.assertLastToolCallError(t, session.runID, tools.CodeProposalAlreadyExists)
}

// TestProposalExactReplayWithFreshRunContext covers exact replay after a
// restart: the run's successful tool_calls rows are the only evidence
// authority, so a new RunContext with empty in-memory refs must still accept
// the same proposal and return already_recorded with one row. A ref absent from
// both memory and the database is still rejected.
func TestProposalExactReplayWithFreshRunContext(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	session := world.newSession(t, scenario)
	refs := session.seedEvidence()

	args := session.baseArgs(refs)
	args["question"] = "Which invoice does this payout settle?"

	first := session.call("request_info", args)
	if code := integrationEnvelopeErrorCode(first); code != "" {
		t.Fatalf("first proposal error = %q: %v", code, first)
	}
	firstData := integrationEnvelopeData(t, first)
	key, _ := firstData["idempotency_key"].(string)
	if firstData["status"] != "recorded" || len(key) != 64 {
		t.Fatalf("first proposal = %v", firstData)
	}

	replay := session.freshContext()
	if got := replay.run.RetrievedRefs(); len(got) != 0 {
		t.Fatalf("fresh RunContext refs = %v, want empty", got)
	}
	replayed := replay.call("request_info", args)
	if code := integrationEnvelopeErrorCode(replayed); code != "" {
		t.Fatalf("fresh-context replay error = %q: %v", code, replayed)
	}
	replayData := integrationEnvelopeData(t, replayed)
	if replayData["status"] != "already_recorded" {
		t.Fatalf("fresh-context replay status = %v, want already_recorded", replayData["status"])
	}
	if replayData["idempotency_key"] != key {
		t.Fatalf("fresh-context replay key = %v, want %s", replayData["idempotency_key"], key)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 1 {
		t.Fatalf("proposals after fresh-context replay = %d, want 1", count)
	}
	if got := replay.run.RetrievedRefs(); len(got) != 0 {
		t.Fatalf("fresh RunContext gained refs from the replay: %v", got)
	}

	// A new empty context must still reject a ref that the database cannot
	// authorize, regardless of what the in-memory collector would say.
	missing := replay.freshContext()
	absent := missing.baseArgs([]string{"case:" + scenario.CaseID, "ext:psp_a:absent"})
	absent["question"] = "Which invoice does this payout settle?"
	rejected := missing.call("request_info", absent)
	if code := integrationEnvelopeErrorCode(rejected); code != tools.CodeEvidenceNotRetrieved {
		t.Fatalf("database-absent ref error = %q, want evidence_not_retrieved: %v", code, rejected)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 1 {
		t.Fatalf("proposals after absent-ref rejection = %d, want 1", count)
	}
	world.assertLastToolCallError(t, session.runID, tools.CodeEvidenceNotRetrieved)
}

// TestProposalExactReplayAfterCaseMovedOn proves idempotency wins over case
// state: once a proposal is recorded, replaying the same key/payload returns
// already_recorded even after the case has moved to proposal_pending.
func TestProposalExactReplayAfterCaseMovedOn(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	session := world.newSession(t, scenario)
	refs := session.seedEvidence()

	args := session.baseArgs(refs)
	args["question"] = "Which invoice does this payout settle?"

	first := session.call("request_info", args)
	if code := integrationEnvelopeErrorCode(first); code != "" {
		t.Fatalf("first proposal error = %q: %v", code, first)
	}
	firstData := integrationEnvelopeData(t, first)
	key, _ := firstData["idempotency_key"].(string)
	if firstData["status"] != "recorded" || len(key) != 64 {
		t.Fatalf("first proposal = %v", firstData)
	}

	// The loop or executor advances the case after the proposal is recorded.
	if _, err := world.DB.Pool.Exec(context.Background(),
		`UPDATE cases SET status = 'proposal_pending', version = version + 1
		  WHERE id = $1::uuid`, scenario.CaseID); err != nil {
		t.Fatalf("move case on: %v", err)
	}

	replay := session.freshContext()
	replayed := replay.call("request_info", args)
	if code := integrationEnvelopeErrorCode(replayed); code != "" {
		t.Fatalf("replay after case moved on = %q, want success: %v", code, replayed)
	}
	replayData := integrationEnvelopeData(t, replayed)
	if replayData["status"] != "already_recorded" {
		t.Fatalf("replay status = %v, want already_recorded", replayData["status"])
	}
	if replayData["idempotency_key"] != key {
		t.Fatalf("replay key = %v, want %s", replayData["idempotency_key"], key)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 1 {
		t.Fatalf("proposals after replay = %d, want 1", count)
	}
	if got := replay.run.State().ProposalKey(); got != key {
		t.Fatalf("replay run proposal slot = %q, want %q", got, key)
	}
	world.assertLastToolCallError(t, session.runID, "")
}

// TestProposalOnePerRunDatabaseInvariant proves the durable invariant: a fresh
// RunContext (empty in-memory slot) with a different payload for the same run
// is rejected by proposals_one_proposal_per_run and writes no second row.
func TestProposalOnePerRunDatabaseInvariant(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	session := world.newSession(t, scenario)
	refs := session.seedEvidence()

	firstArgs := session.baseArgs(refs)
	firstArgs["question"] = "First question?"
	first := session.call("request_info", firstArgs)
	if code := integrationEnvelopeErrorCode(first); code != "" {
		t.Fatalf("first proposal error = %q: %v", code, first)
	}
	firstData := integrationEnvelopeData(t, first)
	firstKey, _ := firstData["idempotency_key"].(string)
	if firstData["status"] != "recorded" || len(firstKey) != 64 {
		t.Fatalf("first proposal = %v", firstData)
	}

	second := session.freshContext()
	secondArgs := second.baseArgs(refs)
	secondArgs["question"] = "A different question?"
	rejected := second.call("request_info", secondArgs)
	if code := integrationEnvelopeErrorCode(rejected); code != tools.CodeProposalAlreadyExists {
		t.Fatalf("second proposal error = %q, want proposal_already_exists: %v", code, rejected)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 1 {
		t.Fatalf("proposals after DB-invariant rejection = %d, want 1", count)
	}
	row := world.loadProposalByRun(t, session.runID)
	if row.IdempotencyKey != firstKey {
		t.Fatalf("stored key = %q, want the first proposal %q", row.IdempotencyKey, firstKey)
	}
	world.assertLastToolCallError(t, session.runID, tools.CodeProposalAlreadyExists)
}

// TestProposalRejectsTruncatedEvidence covers the row-cap case: a search result
// keeps up to 50 real refs but marks truncated=true, and none of those refs may
// authorize a citation. The in-memory collector holds them, so only the
// database authority check can reject the proposal.
func TestProposalRejectsTruncatedEvidence(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	session := world.newSession(t, scenario)

	for i := 0; i < 51; i++ {
		insertIntegrationExternalRecord(t, world.DB, world.BatchID, "psp_a",
			fmt.Sprintf("TRUNC-%02d", i), 3000, "2026-01-02")
	}
	search := session.call("search_external_records", map[string]any{
		"amount_minor": 3000,
		"currency":     "USD",
		"from":         "2026-01-02",
		"to":           "2026-01-02",
	})
	if code := integrationEnvelopeErrorCode(search); code != "" {
		t.Fatalf("search_external_records error = %q: %v", code, search)
	}
	if search["truncated"] != true {
		t.Fatalf("search truncated = %v, want true", search["truncated"])
	}
	var cited string
	if refs, ok := search["refs"].([]any); ok {
		for _, item := range refs {
			ref, isString := item.(string)
			if isString && strings.HasPrefix(ref, "ext:") {
				cited = ref
				break
			}
		}
	}
	if cited == "" {
		t.Fatalf("truncated search returned no external refs: %v", search["refs"])
	}
	if len(session.run.RetrievedRefs()) == 0 {
		t.Fatal("truncated search refs were not collected in memory")
	}

	args := session.baseArgs([]string{cited})
	args["question"] = "Which invoice does this payout settle?"
	rejected := session.call("request_info", args)
	if code := integrationEnvelopeErrorCode(rejected); code != tools.CodeEvidenceNotRetrieved {
		t.Fatalf("truncated-evidence error = %q, want evidence_not_retrieved: %v", code, rejected)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 0 {
		t.Fatalf("truncated-evidence proposal wrote %d rows", count)
	}
	world.assertLastToolCallError(t, session.runID, tools.CodeEvidenceNotRetrieved)
}

// TestProposalActiveConflictUsesAgentRole proves the partial unique index is
// hit through the real recon_agent_ro login and maps to the sentinel.
func TestProposalActiveConflictUsesAgentRole(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)

	var currentUser string
	var member bool
	if err := world.Agent.QueryRow(context.Background(),
		`SELECT current_user, pg_has_role(current_user, 'recon_agent_ro', 'MEMBER')`).
		Scan(&currentUser, &member); err != nil {
		t.Fatalf("inspect agent pool: %v", err)
	}
	if !member {
		t.Fatalf("agent pool current_user = %q, not a recon_agent_ro member", currentUser)
	}

	existingRun := world.insertRun(t, scenario.CaseID)
	evidence, _ := json.Marshal([]string{"case:" + scenario.CaseID})
	if _, err := world.DB.Pool.Exec(context.Background(),
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, evidence, confidence,
		      status, version, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'request_info', '{"question":"existing"}'::jsonb,
		         'pre-existing active proposal', $3::jsonb, 0.5, 'proposed', 0, $4,
		         'pre-existing-active-proposal', now())`,
		scenario.CaseID, existingRun, string(evidence), strings.Repeat("a", 64)); err != nil {
		t.Fatalf("insert pre-existing active proposal: %v", err)
	}

	session := world.newSession(t, scenario)
	refs := session.seedEvidence()
	args := session.baseArgs(refs)
	args["question"] = "Which invoice does this payout settle?"

	envelope := session.call("request_info", args)
	if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeActiveProposalExists {
		t.Fatalf("active conflict error = %q, want active_proposal_exists: %v", code, envelope)
	}
	if count := world.proposalCountByCase(t, scenario.CaseID); count != 1 {
		t.Fatalf("proposals for case = %d, want the pre-existing one only", count)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 0 {
		t.Fatalf("proposals for new run = %d, want 0", count)
	}
	if session.run.State().ProposalKey() != "" {
		t.Fatal("active-proposal conflict consumed the run's proposal slot")
	}
	world.assertLastToolCallError(t, session.runID, tools.CodeActiveProposalExists)

	// The same failure is observable as the exported sentinel when the handler
	// is called directly.
	tool, ok := world.Registry.Lookup("request_info")
	if !ok {
		t.Fatal("request_info is not registered")
	}
	if _, err := tool.Call(context.Background(), session.run, mustJSON(t, args)); !errors.Is(err, proposal.ErrActiveProposalExists) {
		t.Fatalf("direct handler error = %v, want ErrActiveProposalExists", err)
	}
}

// TestProposalRejections covers the deterministic money/account/scope
// rejections against the real database. Every rejection is recorded and writes
// no proposal.
//
// covers: FIN-13
func TestProposalRejections(t *testing.T) {
	world := newProposalWorld(t)

	tests := []struct {
		name       string
		tool       string
		fill       func(session *proposalSession, args map[string]any)
		wantCode   string
		wantReason string
	}{
		{
			name: "evidence not retrieved",
			tool: "request_info",
			fill: func(session *proposalSession, args map[string]any) {
				args["question"] = "Which invoice does this payout settle?"
				args["evidence"] = []string{"case:" + session.caseID}
			},
			wantCode: tools.CodeEvidenceNotRetrieved,
		},
		{
			name: "wrong case",
			tool: "request_info",
			fill: func(session *proposalSession, args map[string]any) {
				args["question"] = "Which invoice does this payout settle?"
				args["case_id"] = otherCaseID
			},
			wantCode: tools.CodeCaseMismatch,
		},
		{
			name: "case not open",
			tool: "request_info",
			fill: func(session *proposalSession, args map[string]any) {
				args["question"] = "Which invoice does this payout settle?"
			},
			wantCode: tools.CodeCaseNotOpen,
		},
		{
			name: "inactive account",
			tool: "propose_adjustment",
			fill: func(session *proposalSession, args map[string]any) {
				args["lines"] = []map[string]any{
					{"account_code": accountInactive, "direction": "debit", "amount_minor": 100, "currency": "USD"},
					{"account_code": accountSuspense, "direction": "credit", "amount_minor": 100, "currency": "USD"},
				}
			},
			wantCode:   tools.CodeBusinessRuleViolation,
			wantReason: "account_inactive",
		},
		{
			name: "non-allowlisted account",
			tool: "propose_adjustment",
			fill: func(session *proposalSession, args map[string]any) {
				args["lines"] = []map[string]any{
					{"account_code": accountCash, "direction": "debit", "amount_minor": 100, "currency": "USD"},
					{"account_code": accountSuspense, "direction": "credit", "amount_minor": 100, "currency": "USD"},
				}
			},
			wantCode:   tools.CodeBusinessRuleViolation,
			wantReason: "account_not_allowed",
		},
		{
			name: "wrong account currency",
			tool: "propose_adjustment",
			fill: func(session *proposalSession, args map[string]any) {
				args["lines"] = []map[string]any{
					{"account_code": accountFeesEUR, "direction": "debit", "amount_minor": 100, "currency": "USD"},
					{"account_code": accountSuspense, "direction": "credit", "amount_minor": 100, "currency": "USD"},
				}
			},
			wantCode:   tools.CodeBusinessRuleViolation,
			wantReason: "currency_mismatch",
		},
		{
			name: "above break delta",
			tool: "propose_adjustment",
			fill: func(session *proposalSession, args map[string]any) {
				args["lines"] = []map[string]any{
					{"account_code": accountFees, "direction": "debit", "amount_minor": 501, "currency": "USD"},
					{"account_code": accountSuspense, "direction": "credit", "amount_minor": 501, "currency": "USD"},
				}
			},
			wantCode:   tools.CodeBusinessRuleViolation,
			wantReason: "exceeds_delta",
		},
		{
			name: "unbalanced entry",
			tool: "propose_adjustment",
			fill: func(session *proposalSession, args map[string]any) {
				args["lines"] = []map[string]any{
					{"account_code": accountFees, "direction": "debit", "amount_minor": 250, "currency": "USD"},
					{"account_code": accountSuspense, "direction": "credit", "amount_minor": 249, "currency": "USD"},
				}
			},
			wantCode:   tools.CodeBusinessRuleViolation,
			wantReason: "unbalanced",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			status := "open"
			if test.wantCode == tools.CodeCaseNotOpen {
				status = "waiting_info"
			}
			scenario := world.newScenarioWithStatus(t, status)
			session := world.newSession(t, scenario)
			var refs []string
			if test.name == "evidence not retrieved" {
				// Cite a canonical ref the run never fetched.
				refs = []string{"case:" + scenario.CaseID}
			} else {
				// Every other rejection still runs after genuine evidence so
				// the later pipeline stage is the one that rejects it.
				refs = session.seedEvidence()
			}
			args := session.baseArgs(refs)
			test.fill(session, args)

			envelope := session.call(test.tool, args)
			if code := integrationEnvelopeErrorCode(envelope); code != test.wantCode {
				t.Fatalf("%s error = %q, want %q: %v", test.tool, code, test.wantCode, envelope)
			}
			if test.wantReason != "" {
				message := integrationEnvelopeErrorMessage(envelope)
				if !strings.Contains(message, test.wantReason) {
					t.Fatalf("error message = %q, want reason %q", message, test.wantReason)
				}
			}
			if count := world.proposalCountByRun(t, session.runID); count != 0 {
				t.Fatalf("rejected proposal wrote %d rows", count)
			}
			world.assertLastToolCallError(t, session.runID, test.wantCode)
		})
	}

	// The database check is authoritative after a restart: a ref in the
	// in-memory collector is still rejected when its successful tool_calls row
	// is gone.
	t.Run("database is authoritative", func(t *testing.T) {
		scenario := world.newScenario(t)
		session := world.newSession(t, scenario)
		refs := session.seedEvidence()
		if _, err := world.DB.Pool.Exec(context.Background(),
			`DELETE FROM tool_calls WHERE agent_run_id = $1::uuid`, session.runID); err != nil {
			t.Fatalf("delete read tool calls: %v", err)
		}
		args := session.baseArgs(refs)
		args["question"] = "Which invoice does this payout settle?"
		envelope := session.call("request_info", args)
		if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeEvidenceNotRetrieved {
			t.Fatalf("database-authoritative error = %q, want evidence_not_retrieved: %v", code, envelope)
		}
		world.assertLastToolCallError(t, session.runID, tools.CodeEvidenceNotRetrieved)
	})
}

func TestFlaggedRunRejectsProposals(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	session := world.newSession(t, scenario)
	refs := session.seedEvidence()
	session.run.State().AddFlags("instruction_override")

	args := session.baseArgs(refs)
	args["question"] = "Which invoice does this payout settle?"
	envelope := session.call("request_info", args)
	if code := integrationEnvelopeErrorCode(envelope); code != tools.CodeInjectionFlagged {
		t.Fatalf("flagged proposal error = %q, want injection_flagged: %v", code, envelope)
	}
	if count := world.proposalCountByRun(t, session.runID); count != 0 {
		t.Fatalf("flagged proposal wrote %d rows", count)
	}
	world.assertLastToolCallError(t, session.runID, tools.CodeInjectionFlagged)
}

func TestEscalateWritesNoProposalAndWorksWhenFlagged(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	before := world.caseSnapshot(t, scenario.CaseID)

	session := world.newSession(t, scenario)
	session.run.State().AddFlags("instruction_override")

	envelope := session.call("escalate", map[string]any{
		"case_id": scenario.CaseID,
		"reason":  "  ambiguous\u200b evidence  ",
		"summary": "Two records match.\u200b",
	})
	if code := integrationEnvelopeErrorCode(envelope); code != "" {
		t.Fatalf("escalate error = %q: %v", code, envelope)
	}
	data := integrationEnvelopeData(t, envelope)
	if data["status"] != "escalated" {
		t.Fatalf("escalate status = %v, want escalated", data["status"])
	}
	escalation := session.run.State().Escalation()
	if escalation == nil {
		t.Fatal("escalate did not set the run escalation")
	}
	if escalation.Reason != "ambiguous evidence" || escalation.Summary != "Two records match." {
		t.Fatalf("escalation = %+v, want sanitized strings", escalation)
	}
	if count := world.proposalCountByCase(t, scenario.CaseID); count != 0 {
		t.Fatalf("escalate wrote %d proposals", count)
	}
	if after := world.caseSnapshot(t, scenario.CaseID); after != before {
		t.Fatalf("escalate changed the case: before=%+v after=%+v", before, after)
	}
	world.assertLastToolCallError(t, session.runID, "")
}

// TestProposalInsertOwnerColumns asserts the row shape that only the owner
// pool can read: status/version defaults, NULL policy/verification fields, the
// claimed pattern, sorted evidence, and the canonical payload.
func TestProposalInsertOwnerColumns(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	session := world.newSession(t, scenario)
	refs := session.seedEvidence()

	args := session.baseArgs(refs)
	args["pattern"] = "fee_variance"
	args["confidence"] = 0.12345
	args["lines"] = []map[string]any{
		{"account_code": accountSuspense, "direction": "credit", "amount_minor": 250, "currency": "USD"},
		{"account_code": accountFees, "direction": "debit", "amount_minor": 250, "currency": "USD"},
	}
	envelope := session.call("propose_adjustment", args)
	if code := integrationEnvelopeErrorCode(envelope); code != "" {
		t.Fatalf("proposal error = %q: %v", code, envelope)
	}
	row := world.loadProposalByRun(t, session.runID)

	if _, err := tools.ParseUUID(row.ID); err != nil {
		t.Fatalf("proposal id = %q is not a client UUID: %v", row.ID, err)
	}
	if row.Status != "proposed" || row.Version != 0 {
		t.Fatalf("status/version = %q/%d, want proposed/0", row.Status, row.Version)
	}
	if row.RiskTier != nil || row.PatternVerified != nil || row.Verification != nil || row.PolicyDecision != nil {
		t.Fatalf("policy fields not NULL: %+v", row)
	}
	if row.PatternClaimed == nil || *row.PatternClaimed != "fee_variance" {
		t.Fatalf("pattern_claimed = %v, want fee_variance", row.PatternClaimed)
	}
	if row.Confidence != "0.1235" {
		t.Fatalf("confidence = %q, want rounded 0.1235", row.Confidence)
	}
	if row.ExpiresAt != nil || row.Supersedes != nil {
		t.Fatalf("expiry/supersedes not NULL: %v/%v", row.ExpiresAt, row.Supersedes)
	}

	var evidence []string
	if err := json.Unmarshal(row.Evidence, &evidence); err != nil {
		t.Fatalf("decode stored evidence: %v", err)
	}
	wantEvidence := tools.SortedUnique(refs)
	if len(evidence) != len(wantEvidence) {
		t.Fatalf("stored evidence = %v, want %v", evidence, wantEvidence)
	}
	for i := range wantEvidence {
		if evidence[i] != wantEvidence[i] {
			t.Fatalf("stored evidence = %v, want %v", evidence, wantEvidence)
		}
	}

	var payload struct {
		Lines []struct {
			AccountCode string `json:"account_code"`
			Direction   string `json:"direction"`
			AmountMinor int64  `json:"amount_minor"`
			Currency    string `json:"currency"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("decode stored payload: %v", err)
	}
	if len(payload.Lines) != 2 || payload.Lines[0].AccountCode != accountFees {
		t.Fatalf("stored payload lines are not canonical: %s", row.Payload)
	}
}

// TestStateHashStableAcrossCaseVersionEdit proves the canonical projection
// excludes cases.version while a money field still changes the hash.
func TestStateHashStableAcrossCaseVersionEdit(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	refs := []string{"case:" + scenario.CaseID, world.LedgerTxn, world.ExternalRef}

	state, err := proposal.ResolveState(context.Background(), world.Agent, scenario.CaseID, refs)
	if err != nil {
		t.Fatalf("ResolveState(): %v", err)
	}
	canonical, err := proposal.Canonical(state)
	if err != nil {
		t.Fatalf("Canonical(): %v", err)
	}
	if strings.Contains(string(canonical), `"version"`) {
		t.Fatalf("canonical state includes cases.version: %s", canonical)
	}
	base, err := proposal.Hash(state)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}

	if _, err := world.DB.Pool.Exec(context.Background(),
		`UPDATE cases SET version = version + 7 WHERE id = $1::uuid`, scenario.CaseID); err != nil {
		t.Fatalf("bump case version: %v", err)
	}
	versioned, err := proposal.ResolveState(context.Background(), world.Agent, scenario.CaseID, refs)
	if err != nil {
		t.Fatalf("ResolveState() after version bump: %v", err)
	}
	versionHash, err := proposal.Hash(versioned)
	if err != nil {
		t.Fatalf("Hash() after version bump: %v", err)
	}
	if versionHash != base {
		t.Fatalf("cases.version changed the state hash: %s != %s", versionHash, base)
	}

	if _, err := world.DB.Pool.Exec(context.Background(),
		`UPDATE breaks SET delta_minor = -501 WHERE id = $1::uuid`, scenario.BreakID); err != nil {
		t.Fatalf("change break delta: %v", err)
	}
	moneyChanged, err := proposal.ResolveState(context.Background(), world.Agent, scenario.CaseID, refs)
	if err != nil {
		t.Fatalf("ResolveState() after money change: %v", err)
	}
	moneyHash, err := proposal.Hash(moneyChanged)
	if err != nil {
		t.Fatalf("Hash() after money change: %v", err)
	}
	if moneyHash == base {
		t.Fatal("changing the break delta did not change the state hash")
	}
}

// TestResolveStateAllRefTypes exercises the projections for every supported
// evidence ref against the real v_agent_* views. Unknown refs must fail before
// an insert instead of silently producing a partial state.
func TestResolveStateAllRefTypes(t *testing.T) {
	world := newProposalWorld(t)
	scenario := world.newScenario(t)
	ctx := context.Background()

	var fxID string
	if err := world.DB.Pool.QueryRow(ctx,
		`INSERT INTO fx_rates (base, quote, rate, as_of, source)
		 VALUES ('USD', 'EUR', '0.923456789012', '2026-01-02T00:00:00Z', 'integration')
		 RETURNING id::text`).Scan(&fxID); err != nil {
		t.Fatalf("create fx rate: %v", err)
	}
	fxRef, err := tools.FXRef("USD", "EUR", "2026-01-02")
	if err != nil {
		t.Fatalf("FXRef(): %v", err)
	}
	var scheduleID string
	if err := world.DB.Pool.QueryRow(ctx,
		`INSERT INTO fee_schedules
		     (source, kind, currency, pct_bps, fixed_minor, valid_from)
		 VALUES ('psp_a', 'processing', 'USD', 250, 10, '2026-01-01')
		 RETURNING id::text`).Scan(&scheduleID); err != nil {
		t.Fatalf("create fee schedule: %v", err)
	}
	feeRef, err := tools.FeeScheduleRef(scheduleID)
	if err != nil {
		t.Fatalf("FeeScheduleRef(): %v", err)
	}

	refs := []string{
		"case:" + scenario.CaseID,
		world.LedgerTxn,
		world.ExternalRef,
		world.BatchRef,
		fxRef,
		feeRef,
	}
	state, err := proposal.ResolveState(ctx, world.Agent, scenario.CaseID, refs)
	if err != nil {
		t.Fatalf("ResolveState(): %v", err)
	}
	if len(state.Records) != len(refs) {
		t.Fatalf("records = %d, want %d", len(state.Records), len(refs))
	}
	kinds := make(map[string]string, len(state.Records))
	for _, record := range state.Records {
		kinds[record.Ref] = record.Kind
	}
	wantKinds := map[string]string{
		"case:" + scenario.CaseID: "case",
		world.LedgerTxn:           "ledger_txn",
		world.ExternalRef:         "ext",
		world.BatchRef:            "batch",
		fxRef:                     "fx",
		feeRef:                    "fee_schedule",
	}
	for ref, want := range wantKinds {
		if kinds[ref] != want {
			t.Fatalf("record %s kind = %q, want %q", ref, kinds[ref], want)
		}
	}

	first, err := proposal.Canonical(state)
	if err != nil {
		t.Fatalf("Canonical(): %v", err)
	}
	second, err := proposal.Canonical(state)
	if err != nil {
		t.Fatalf("Canonical() second call: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("canonical state is not deterministic")
	}
	if _, err := proposal.Hash(state); err != nil {
		t.Fatalf("Hash(): %v", err)
	}

	if _, err := proposal.ResolveState(ctx, world.Agent, scenario.CaseID, []string{"ext:psp_a:missing"}); err == nil {
		t.Fatal("ResolveState() with an unknown ref = nil error, want failure before insert")
	}
}
