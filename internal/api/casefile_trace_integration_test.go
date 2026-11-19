//go:build integration

// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
	"github.com/TamasGorgics/recon-agent/internal/traceability"
)

// seededTraceCase is one applied adjustment plus every link the case-file trace
// resolves. ledgerTxnID is empty when the application has no ledger transaction.
type seededTraceCase struct {
	caseID           string
	breakID          string
	agentRunID       string
	proposalID       string
	approverID       string
	approvalID       string
	ledgerTxnID      string
	configHash       string
	configYAML       string
	traceID          string
	appliedAt        time.Time
	decidedAt        time.Time
	executorRunID    string
	executorAuditSeq int64
}

// seedAppliedAdjustment inserts a realistic applied adjustment chain directly
// through the maintenance pool: account/scope/run/break/case/agent run,
// policy decision and config version, approval with approver, applied proposal,
// optional balanced ledger transaction, and the executor audit entry.
func (f *apiFixture) seedAppliedAdjustment(t *testing.T, withLedger bool) seededTraceCase {
	t.Helper()
	ctx := context.Background()
	seeded := seededTraceCase{
		appliedAt:     time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
		executorRunID: "executor-run-api-v2",
		traceID:       "trace-api-v2",
	}
	seeded.decidedAt = seeded.appliedAt.Add(5 * time.Minute)

	var debitID, creditID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency)
		 VALUES ('api_v2_fee_expense', 'API v2 fee expense', 'expense', 'USD')
		 RETURNING id::text`).Scan(&debitID); err != nil {
		t.Fatalf("seed v2 debit account: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency)
		 VALUES ('api_v2_suspense', 'API v2 suspense', 'liability', 'USD')
		 RETURNING id::text`).Scan(&creditID); err != nil {
		t.Fatalf("seed v2 credit account: %v", err)
	}

	var scopeID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'api_v2_psp', 'USD') RETURNING id::text`,
		debitID).Scan(&scopeID); err != nil {
		t.Fatalf("seed v2 scope: %v", err)
	}
	var runID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'api-v2-config')
		 RETURNING id::text`, scopeID, seeded.appliedAt.Add(-time.Hour)).Scan(&runID); err != nil {
		t.Fatalf("seed v2 match run: %v", err)
	}

	signature := sha256.Sum256([]byte("api-v2-applied-adjustment"))
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO breaks (signature, scope_id, match_run_id, side, ledger_txn_ids,
		                     external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, 'external_only', '{}'::uuid[],
		         '[{"source":"api_v2_psp","source_record_id":"ext-v2-1"}]'::jsonb,
		         500, 'USD', 'in_case', $4)
		 RETURNING id::text`,
		hex.EncodeToString(signature[:]), scopeID, runID,
		seeded.appliedAt.Add(-time.Hour)).Scan(&seeded.breakID); err != nil {
		t.Fatalf("seed v2 break: %v", err)
	}

	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts,
		                    created_at, resolved_at, resolution_type)
		 VALUES (2, 500, 'USD', 'resolved', 0, 0, $1, $1, 'adjustment_applied')
		 RETURNING id::text`, seeded.appliedAt.Add(-time.Hour)).Scan(&seeded.caseID); err != nil {
		t.Fatalf("seed v2 case: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`,
		seeded.caseID, seeded.breakID, seeded.appliedAt.Add(-time.Hour)); err != nil {
		t.Fatalf("seed v2 case break: %v", err)
	}

	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at, ended_at,
		                         outcome, tokens_in, tokens_out, cost_usd, trace_id)
		 VALUES ($1::uuid, 'deepseek-v4.1-flash', 'v1', $2, $2, 'proposal',
		         1200, 300, 0.000420, $3)
		 RETURNING id::text`,
		seeded.caseID, seeded.appliedAt.Add(-30*time.Minute), seeded.traceID).Scan(&seeded.agentRunID); err != nil {
		t.Fatalf("seed v2 agent run: %v", err)
	}

	seeded.configYAML = "schema_version: 1\n# api v2 fixture\nthresholds:\n  single_approval_minor: 0\n"
	seeded.configHash = policy.HashYAML([]byte(seeded.configYAML))
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO policy_config_versions (hash, yaml, loaded_at)
		 VALUES ($1, $2, $3)`, seeded.configHash, seeded.configYAML,
		seeded.appliedAt.Add(-40*time.Minute)); err != nil {
		t.Fatalf("seed v2 policy config: %v", err)
	}
	decision := policy.Decision{
		SchemaVersion: 1,
		Tier:          policy.TierT1,
		Rule:          policy.CodeDefaultSingleApproval,
		Reasons:       []string{policy.CodeDefaultSingleApproval},
		ConfigHash:    seeded.configHash,
	}
	decisionJSON, err := decision.JSON()
	if err != nil {
		t.Fatalf("marshal policy decision: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO proposals (case_id, agent_run_id, type, payload, rationale, evidence,
		                        confidence, verification, risk_tier, policy_decision,
		                        status, version, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'adjustment',
		         '{"lines":[{"account_code":"api_v2_fee_expense","direction":"debit","amount_minor":500,"currency":"USD"},{"account_code":"api_v2_suspense","direction":"credit","amount_minor":500,"currency":"USD"}]}'::jsonb,
		         'api v2 applied adjustment', '[]'::jsonb, 0.9000,
		         '{"schema_version":1,"pattern":"fee_variance","passed":true}'::jsonb,
		         'T1', $3::jsonb, 'applied', 0, 'api-v2-state-hash', 'api-v2-idem', $4)
		 RETURNING id::text`,
		seeded.caseID, seeded.agentRunID, string(decisionJSON),
		seeded.appliedAt.Add(-20*time.Minute)).Scan(&seeded.proposalID); err != nil {
		t.Fatalf("seed v2 proposal: %v", err)
	}

	if withLedger {
		seeded.ledgerTxnID = f.seedAppliedLedgerTxn(t, debitID, creditID, seeded.appliedAt)
	}
	var ledgerArg any
	if seeded.ledgerTxnID != "" {
		ledgerArg = seeded.ledgerTxnID
	}
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, $2::uuid, $3, $4)`,
		seeded.proposalID, ledgerArg, seeded.appliedAt, seeded.executorRunID); err != nil {
		t.Fatalf("seed v2 applied proposal: %v", err)
	}

	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO approvers (display_name, role, active)
		 VALUES ('Alice API Approver', 'approver', true) RETURNING id::text`).Scan(&seeded.approverID); err != nil {
		t.Fatalf("seed v2 approver: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO approvals (proposal_id, channel, external_ref, approver_id,
		                        decision, decided_at, signature_valid)
		 VALUES ($1::uuid, 'slack', 'slack-api-v2', $2::uuid, 'approved', $3, true)
		 RETURNING id::text`,
		seeded.proposalID, seeded.approverID, seeded.decidedAt).Scan(&seeded.approvalID); err != nil {
		t.Fatalf("seed v2 approval: %v", err)
	}

	actor, err := audit.SystemActor("executor")
	if err != nil {
		t.Fatalf("build executor actor: %v", err)
	}
	after := map[string]any{
		"status":          "applied",
		"ledger_txn_id":   ledgerArg,
		"applied_at":      seeded.appliedAt.UTC().Format(time.RFC3339),
		"executor_run_id": seeded.executorRunID,
	}
	record := testutil.AppendAudit(t, ctx, f.db.Pool, audit.Entry{
		Actor:      actor,
		Action:     "executor.applied",
		EntityType: "proposal",
		EntityID:   seeded.proposalID,
		Before:     map[string]any{"status": "approved"},
		After:      after,
		Timestamp:  seeded.appliedAt,
	})
	seeded.executorAuditSeq = record.Seq
	return seeded
}

// seedAppliedLedgerTxn inserts one balanced adjustment transaction and returns
// its id. The seeded open period covers the value date.
func (f *apiFixture) seedAppliedLedgerTxn(t *testing.T, debitID, creditID string, postedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin v2 ledger fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns (created_at, booked_at, value_date, kind,
		                          idempotency_key, request_hash, description, period_id)
		 VALUES ($1, $1, '2026-11-18', 'adjustment', 'api-v2-ledger',
		         'api-v2-request-hash', 'API v2 applied adjustment',
		         '11111111-1111-4111-8111-111111111111')
		 RETURNING id::text`, postedAt).Scan(&txnID); err != nil {
		t.Fatalf("insert v2 ledger txn: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor,
		                             currency, posted_at, external_ref)
		 VALUES ($1::uuid, $2::uuid, 'debit', 500, 'USD', $3, 'api-v2-ref'),
		        ($1::uuid, $4::uuid, 'credit', 500, 'USD', $3, 'api-v2-ref')`,
		txnID, debitID, postedAt, creditID); err != nil {
		t.Fatalf("insert v2 ledger entries: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit v2 ledger fixture: %v", err)
	}
	return txnID
}

// getBody issues a viewer-authenticated GET and returns the status and body.
func (f *apiFixture) getBody(t *testing.T, target string) (int, []byte) {
	t.Helper()
	response := f.get(t, target)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", target, err)
	}
	return response.StatusCode, body
}

// TestAPICaseFileTraceV2Integration walks a viewer-authenticated case-file v2
// response from the applied adjustment back to the approver, policy YAML, and
// balanced ledger transaction.
//
// covers: FIN-32
func TestAPICaseFileTraceV2Integration(t *testing.T) {
	f := newAPIFixture(t)
	seeded := f.seedAppliedAdjustment(t, true)

	status, body := f.getBody(t, "/v1/cases/"+seeded.caseID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var document CaseFile
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode case file: %v", err)
	}
	if document.SchemaVersion != 2 {
		t.Fatalf("schema_version = %d, want 2", document.SchemaVersion)
	}
	if document.Case.ID != seeded.caseID {
		t.Fatalf("case id = %s, want %s", document.Case.ID, seeded.caseID)
	}
	if len(document.Breaks) != 1 || document.Breaks[0].ID != seeded.breakID {
		t.Fatalf("breaks = %+v", document.Breaks)
	}
	if len(document.Proposals) != 1 || document.Proposals[0].ID != seeded.proposalID {
		t.Fatalf("proposals = %+v", document.Proposals)
	}
	if len(document.AgentRuns) != 1 || document.AgentRuns[0].ID != seeded.agentRunID {
		t.Fatalf("agent runs = %+v", document.AgentRuns)
	}

	if document.Trace.Case.ID != seeded.caseID {
		t.Fatalf("trace case = %+v", document.Trace.Case)
	}
	if document.Trace.Missing == nil || len(document.Trace.Missing) != 0 {
		t.Fatalf("trace missing = %v, want []", document.Trace.Missing)
	}
	if len(document.Trace.Chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(document.Trace.Chains))
	}
	chain := document.Trace.Chains[0]
	if chain.Missing == nil || len(chain.Missing) != 0 {
		t.Fatalf("chain missing = %v, want []", chain.Missing)
	}
	if chain.LedgerTxnID == nil || *chain.LedgerTxnID != seeded.ledgerTxnID {
		t.Fatalf("chain ledger_txn_id = %v, want %s", chain.LedgerTxnID, seeded.ledgerTxnID)
	}
	if chain.AppliedProposal == nil ||
		chain.AppliedProposal.ProposalID != seeded.proposalID ||
		chain.AppliedProposal.ExecutorRunID != seeded.executorRunID ||
		!chain.AppliedProposal.AppliedAt.Equal(seeded.appliedAt) {
		t.Fatalf("applied proposal = %+v", chain.AppliedProposal)
	}
	if chain.Proposal == nil || chain.Proposal.ID != seeded.proposalID ||
		chain.Proposal.CaseID != seeded.caseID || chain.Proposal.Type != "adjustment" ||
		chain.Proposal.Status != "applied" {
		t.Fatalf("proposal = %+v", chain.Proposal)
	}
	if chain.Case == nil || chain.Case.ID != seeded.caseID {
		t.Fatalf("chain case = %+v", chain.Case)
	}
	if len(chain.Breaks) != 1 || chain.Breaks[0].ID != seeded.breakID {
		t.Fatalf("chain breaks = %+v", chain.Breaks)
	}
	if chain.AgentRun == nil || chain.AgentRun.ID != seeded.agentRunID {
		t.Fatalf("agent run = %+v", chain.AgentRun)
	}
	if chain.AgentRun.TraceID == nil || *chain.AgentRun.TraceID != seeded.traceID {
		t.Fatalf("agent run trace_id = %v, want %s", chain.AgentRun.TraceID, seeded.traceID)
	}
	if len(chain.Verification) == 0 {
		t.Fatal("verification is missing")
	}
	if chain.Policy == nil {
		t.Fatal("policy is missing")
	}
	if chain.Policy.RiskTier == nil || *chain.Policy.RiskTier != string(policy.TierT1) {
		t.Fatalf("policy risk_tier = %v, want T1", chain.Policy.RiskTier)
	}
	if chain.Policy.Rule != policy.CodeDefaultSingleApproval {
		t.Fatalf("policy rule = %q", chain.Policy.Rule)
	}
	if len(chain.Policy.Reasons) != 1 || chain.Policy.Reasons[0] != policy.CodeDefaultSingleApproval {
		t.Fatalf("policy reasons = %v", chain.Policy.Reasons)
	}
	if chain.Policy.ConfigHash != seeded.configHash {
		t.Fatalf("policy config_hash = %q, want %q", chain.Policy.ConfigHash, seeded.configHash)
	}
	if chain.Policy.ConfigYAML == nil || *chain.Policy.ConfigYAML != seeded.configYAML {
		t.Fatalf("policy config_yaml = %v, want %q", chain.Policy.ConfigYAML, seeded.configYAML)
	}
	if chain.Policy.ConfigLoadedAt == nil {
		t.Fatal("policy config_loaded_at is missing")
	}
	if len(chain.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(chain.Approvals))
	}
	approval := chain.Approvals[0]
	if approval.Decision != "approved" || !approval.DecidedAt.Equal(seeded.decidedAt) {
		t.Fatalf("approval = %+v", approval)
	}
	if approval.Approver.ID != seeded.approverID ||
		approval.Approver.DisplayName != "Alice API Approver" ||
		approval.Approver.Role != "approver" {
		t.Fatalf("approver = %+v", approval.Approver)
	}
	if chain.Executor.Audit == nil {
		t.Fatal("executor audit is missing")
	}
	if chain.Executor.Audit.Seq != seeded.executorAuditSeq {
		t.Fatalf("executor audit seq = %d, want %d", chain.Executor.Audit.Seq, seeded.executorAuditSeq)
	}
	var executorAfter map[string]any
	if err := json.Unmarshal(chain.Executor.Audit.After, &executorAfter); err != nil {
		t.Fatalf("decode executor after: %v", err)
	}
	if executorAfter["ledger_txn_id"] != seeded.ledgerTxnID ||
		executorAfter["executor_run_id"] != seeded.executorRunID {
		t.Fatalf("executor after = %v", executorAfter)
	}
	if chain.LedgerTxn == nil || chain.LedgerTxn.ID != seeded.ledgerTxnID {
		t.Fatalf("ledger txn = %+v", chain.LedgerTxn)
	}
	if chain.LedgerTxn.Kind != "adjustment" || chain.LedgerTxn.ValueDate != "2026-11-18" {
		t.Fatalf("ledger txn = %+v", chain.LedgerTxn)
	}
	if len(chain.LedgerEntries) != 2 {
		t.Fatalf("ledger entries = %+v", chain.LedgerEntries)
	}
	amounts := map[string]int64{}
	codes := map[string]string{}
	for _, entry := range chain.LedgerEntries {
		amounts[entry.Direction] = entry.AmountMinor
		codes[entry.Direction] = entry.AccountCode
		if entry.TxnID != seeded.ledgerTxnID {
			t.Fatalf("entry txn id = %s, want %s", entry.TxnID, seeded.ledgerTxnID)
		}
	}
	if amounts["debit"] != 500 || amounts["credit"] != 500 {
		t.Fatalf("entry amounts = %v", amounts)
	}
	if codes["debit"] != "api_v2_fee_expense" || codes["credit"] != "api_v2_suspense" {
		t.Fatalf("entry account codes = %v", codes)
	}
}

// TestAPICaseFileTraceV2UnlinkedApplicationIntegration proves an applied
// application without a ledger transaction stays in the trace with
// ledger_txn_id null and the ledger_txn marker while the request stays 200.
//
// covers: FIN-32
func TestAPICaseFileTraceV2UnlinkedApplicationIntegration(t *testing.T) {
	f := newAPIFixture(t)
	seeded := f.seedAppliedAdjustment(t, false)

	status, body := f.getBody(t, "/v1/cases/"+seeded.caseID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	var raw struct {
		SchemaVersion int `json:"schema_version"`
		Trace         struct {
			Chains []struct {
				LedgerTxnID json.RawMessage `json:"ledger_txn_id"`
				LedgerTxn   json.RawMessage `json:"ledger_txn"`
				Missing     []string        `json:"missing"`
			} `json:"chains"`
			Missing []string `json:"missing"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode raw case file: %v", err)
	}
	if raw.SchemaVersion != 2 || len(raw.Trace.Chains) != 1 {
		t.Fatalf("raw = %+v", raw)
	}
	if string(raw.Trace.Chains[0].LedgerTxnID) != "null" {
		t.Fatalf("ledger_txn_id = %s, want null", raw.Trace.Chains[0].LedgerTxnID)
	}
	if string(raw.Trace.Chains[0].LedgerTxn) != "null" {
		t.Fatalf("ledger_txn = %s, want null", raw.Trace.Chains[0].LedgerTxn)
	}
	if len(raw.Trace.Chains[0].Missing) != 1 ||
		raw.Trace.Chains[0].Missing[0] != traceability.MissingLedgerTxn {
		t.Fatalf("chain missing = %v", raw.Trace.Chains[0].Missing)
	}
	if len(raw.Trace.Missing) != 1 || raw.Trace.Missing[0] != traceability.MissingLedgerTxn {
		t.Fatalf("trace missing = %v", raw.Trace.Missing)
	}

	var document CaseFile
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode case file: %v", err)
	}
	chain := document.Trace.Chains[0]
	if chain.LedgerTxnID != nil || chain.LedgerTxn != nil {
		t.Fatalf("chain = %+v, want unlinked", chain)
	}
	if chain.LedgerEntries == nil || len(chain.LedgerEntries) != 0 {
		t.Fatalf("ledger entries = %v, want []", chain.LedgerEntries)
	}
}

// TestAPICaseFileV2NotFoundAndNoNewRouteIntegration keeps the viewer 404
// behavior and proves the trace section did not introduce a route.
//
// covers: FIN-32
func TestAPICaseFileV2NotFoundAndNoNewRouteIntegration(t *testing.T) {
	f := newAPIFixture(t)

	response := f.get(t, "/v1/cases/00000000-0000-4000-8000-000000000000")
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown case status = %d, want 404", response.StatusCode)
	}
	assertErrorCode(t, response, codeNotFound)

	response = f.get(t, "/v1/cases/00000000-0000-4000-8000-000000000000/trace")
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("trace route status = %d, want 404 (no route)", response.StatusCode)
	}
}
