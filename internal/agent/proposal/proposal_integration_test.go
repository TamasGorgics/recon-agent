//go:build integration

package proposal_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
