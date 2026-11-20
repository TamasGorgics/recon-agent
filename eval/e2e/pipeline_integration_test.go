//go:build integration

// covers: FIN-33

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentproposal "github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

type testEnv struct {
	t        *testing.T
	tdb      *testdb.DB
	appRole  *db.RolePool
	execRole *db.RolePool
	app      *pgxpool.Pool
	exec     *pgxpool.Pool
	clock    *clock.Fake

	scopeID    string
	clearingID string
	feesID     string
	suspenseID string
	batchID    string
	matchRunID string
	source     string
	currency   string
	asOf       time.Time
	caseSvc    *cases.Service
	sequence   int
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	tdb := testdb.NewWithOptions(t, testdb.Options{MaxConns: 8})
	appRole, err := db.OpenRole(ctx, db.RoleApp, tdb.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(appRole.Close)
	execRole, err := db.OpenRole(ctx, db.RoleExecutor, tdb.DSNFor(db.RoleExecutor))
	if err != nil {
		t.Fatalf("open executor pool: %v", err)
	}
	t.Cleanup(execRole.Close)

	e := &testEnv{
		t:        t,
		tdb:      tdb,
		appRole:  appRole,
		execRole: execRole,
		app:      appRole.Pool,
		exec:     execRole.Pool,
		clock:    clock.NewFake(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)),
		source:   "psp_e2e",
		currency: "USD",
		asOf:     time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC),
	}
	e.clearingID = e.insertAccount("psp_clearing_usd", "asset", "USD")
	e.feesID = e.insertAccount("fees_expense", "expense", "USD")
	e.suspenseID = e.insertAccount("suspense", "liability", "USD")
	if err := tdb.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, $3) RETURNING id::text`,
		e.clearingID, e.source, e.currency).Scan(&e.scopeID); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	e.batchID = e.insertBatch(e.source, e.currency)
	if _, err := tdb.Pool.Exec(ctx,
		`INSERT INTO statement_balances
		     (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
		 VALUES ($1::uuid, $2::date, 0, 0, $3, $4::uuid)`,
		e.scopeID, e.asOf.Format("2006-01-02"), e.currency, e.batchID); err != nil {
		t.Fatalf("insert statement balance: %v", err)
	}
	if err := tdb.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'e2e-test')
		 RETURNING id::text`, e.scopeID, e.clock.Now()).Scan(&e.matchRunID); err != nil {
		t.Fatalf("insert match run: %v", err)
	}
	if _, err := tdb.Pool.Exec(ctx,
		`UPDATE system_switches SET enabled = true WHERE name = 'auto_apply_enabled'`); err != nil {
		t.Fatalf("enable auto apply: %v", err)
	}
	actor, err := audit.SystemActor("cases")
	if err != nil {
		t.Fatalf("cases actor: %v", err)
	}
	e.caseSvc, err = cases.NewService(e.app, e.clock, actor)
	if err != nil {
		t.Fatalf("new case service: %v", err)
	}
	return e
}

func (e *testEnv) insertAccount(code, accountType, currency string) string {
	e.t.Helper()
	var id string
	if err := e.tdb.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, $3, 'active') RETURNING id::text`,
		code, accountType, currency).Scan(&id); err != nil {
		e.t.Fatalf("insert account %s: %v", code, err)
	}
	return id
}

func (e *testEnv) insertBatch(source, currency string) string {
	e.t.Helper()
	e.sequence++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%s-%d", source, currency, e.sequence)))
	var id string
	if err := e.tdb.Pool.QueryRow(context.Background(),
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ($1, 0, $2, $3, 0, 0) RETURNING id::text`,
		source, currency, hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		e.t.Fatalf("insert batch: %v", err)
	}
	return id
}

// newCase creates one external-only break/case with an observable delta.
func (e *testEnv) newCase(amount int64) (caseID, ref string) {
	e.t.Helper()
	ctx := context.Background()
	e.sequence++
	recordID := fmt.Sprintf("e2e-rec-%02d", e.sequence)
	if _, err := e.tdb.Pool.Exec(ctx,
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ($1, $2, 'charge', $3, $4, 0, $5, $6::date, $7::uuid,
		         '{}'::jsonb, $8)`,
		e.source, recordID, amount, e.currency, e.clock.Now(),
		e.asOf.Format("2006-01-02"), e.batchID, "e2e-raw-"+recordID); err != nil {
		e.t.Fatalf("insert external record: %v", err)
	}
	created, err := e.caseSvc.CreateBreaks(ctx, cases.CreateBreaksRequest{
		ScopeID:    e.scopeID,
		MatchRunID: e.matchRunID,
		Breaks: []match.BreakCandidate{{
			ScopeID:         e.scopeID,
			Side:            match.SideExternalOnly,
			ExternalRecords: []match.RecordRef{{Source: e.source, SourceRecordID: recordID}},
			Currency:        e.currency,
			DeltaMinor:      amount,
		}},
	})
	if err != nil {
		e.t.Fatalf("create break/case: %v", err)
	}
	if created.CreatedBreaks != 1 || len(created.Cases) != 1 {
		e.t.Fatalf("created breaks/cases = %d/%d, want 1/1", created.CreatedBreaks, len(created.Cases))
	}
	return created.Cases[0].ID, externalRef(e.source, recordID)
}

func (e *testEnv) newProposal(caseID, proposalType string, payload []byte,
	evidence []string, patternVerified *string) string {
	e.t.Helper()
	ctx := context.Background()
	resolved, err := agentproposal.ResolveState(ctx, e.tdb.Pool, caseID, evidence)
	if err != nil {
		e.t.Fatalf("resolve state: %v", err)
	}
	stateHash, err := agentproposal.Hash(resolved)
	if err != nil {
		e.t.Fatalf("hash state: %v", err)
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		e.t.Fatalf("marshal evidence: %v", err)
	}
	var agentRunID string
	if err := e.tdb.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at, trace_id)
		 VALUES ($1::uuid, 'e2e-model', 'e2e-prompt-v1', $2, $3) RETURNING id::text`,
		caseID, e.clock.Now(), fmt.Sprintf("trace-e2e-%d", e.sequence)).Scan(&agentRunID); err != nil {
		e.t.Fatalf("insert agent run: %v", err)
	}
	proposalID, err := agentproposal.NewUUID()
	if err != nil {
		e.t.Fatalf("generate proposal id: %v", err)
	}
	var pattern any
	if patternVerified != nil {
		pattern = *patternVerified
	}
	if _, err := e.tdb.Pool.Exec(ctx,
		`INSERT INTO proposals
		     (id, case_id, agent_run_id, type, payload, rationale, evidence, confidence,
		      pattern_verified, status, version, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::jsonb, 'e2e test', $6::jsonb,
		         0.9000, $7, 'proposed', 0, $8, $9, $10)`,
		proposalID, caseID, agentRunID, proposalType, string(payload),
		string(evidenceJSON), pattern, stateHash, "e2e-"+proposalID, e.clock.Now()); err != nil {
		e.t.Fatalf("insert proposal: %v", err)
	}
	return proposalID
}

func (e *testEnv) pipeline(verifier approval.Verifier) *Pipeline {
	e.t.Helper()
	p, err := NewHarness(context.Background(), HarnessOptions{
		Admin:      e.tdb.Pool,
		App:        e.app,
		Exec:       e.exec,
		Clock:      e.clock,
		PolicyPath: "../../policy.yaml",
		Verifier:   verifier,
	})
	if err != nil {
		e.t.Fatalf("new harness: %v", err)
	}
	return p
}

func (e *testEnv) count(query string, args ...any) int {
	e.t.Helper()
	var count int
	if err := e.tdb.Pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		e.t.Fatalf("count query: %v", err)
	}
	return count
}

func (e *testEnv) insertEvalCase(scenarioID string) string {
	e.t.Helper()
	ctx := context.Background()
	var runID string
	if err := e.tdb.Pool.QueryRow(ctx,
		`INSERT INTO eval_runs
		     (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('e2e', 'test', 'e2e-dataset', 'e2e-model', 'e2e-prompt', 1, $1)
		 RETURNING id::text`, e.clock.Now()).Scan(&runID); err != nil {
		e.t.Fatalf("insert eval run: %v", err)
	}
	if _, err := e.tdb.Pool.Exec(ctx,
		`INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated, detail)
		 VALUES ($1::uuid, $2, 'none', true, false, false, '{}'::jsonb)`,
		runID, scenarioID); err != nil {
		e.t.Fatalf("insert eval case result: %v", err)
	}
	return runID
}

type passedPatternVerifier struct {
	pattern string
}

func (v passedPatternVerifier) Verify(context.Context, string) (policy.Verification, error) {
	return policy.Verification{Pattern: v.pattern, Passed: true, Details: json.RawMessage(`{"schema_version":1}`)}, nil
}

func adjustmentPayload(amount int64) []byte {
	document, _ := json.Marshal(map[string]any{
		"lines": []map[string]any{
			{"account_code": "fees_expense", "direction": "debit", "amount_minor": amount, "currency": "USD"},
			{"account_code": "suspense", "direction": "credit", "amount_minor": amount, "currency": "USD"},
		},
	})
	return document
}

func assertTieout(t *testing.T, got *TieOut, expected tieout.Result) {
	t.Helper()
	if got == nil {
		t.Fatal("tie-out is nil")
	}
	want := tieOutFrom(expected)
	if *got != *want {
		t.Fatalf("tie-out = %+v, want %+v", *got, *want)
	}
}

func TestPipelineT1ApproveAppliesAndTieouts(t *testing.T) {
	env := newTestEnv(t)
	caseID, _ := env.newCase(500)
	proposalID := env.newProposal(caseID, "adjustment", adjustmentPayload(500),
		[]string{"case:" + caseID}, nil)
	pipeline := env.pipeline(nil)

	result, err := pipeline.RunCase(context.Background(), Input{
		ProposalID: proposalID,
		CaseID:     caseID,
		ScopeID:    env.scopeID,
		AsOf:       env.asOf,
	})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if result.RequestStatus != RequestStatusApplied || !result.Applied {
		t.Fatalf("result status/applied = %s/%v, want applied/true", result.RequestStatus, result.Applied)
	}
	if result.PolicyTier == nil || *result.PolicyTier != "T1" {
		t.Fatalf("policy tier = %v, want T1", result.PolicyTier)
	}
	if result.ApproverAction != ActionApprove || result.ApproverRule != "exact_match" {
		t.Fatalf("approver = %s/%s, want approve/exact_match", result.ApproverAction, result.ApproverRule)
	}
	if result.LedgerTxnID == "" {
		t.Fatal("applied result has no ledger txn id")
	}
	expected, err := tieout.Compute(context.Background(), env.app, env.scopeID, env.asOf)
	if err != nil {
		t.Fatalf("oracle tie-out: %v", err)
	}
	assertTieout(t, result.Tieout, expected)

	if got := env.count(`SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`, proposalID); got != 1 {
		t.Fatalf("approvals = %d, want 1", got)
	}
	var channel, decision, approverID string
	if err := env.tdb.Pool.QueryRow(context.Background(),
		`SELECT channel, decision, approver_id::text FROM approvals WHERE proposal_id = $1::uuid`,
		proposalID).Scan(&channel, &decision, &approverID); err != nil {
		t.Fatalf("read approval: %v", err)
	}
	if channel != "e2e" || decision != "approved" || approverID != approvedID {
		t.Fatalf("approval = %s/%s/%s, want e2e/approved/%s", channel, decision, approverID, approvedID)
	}
	if got := env.count(`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, proposalID); got != 1 {
		t.Fatalf("applied_proposals = %d, want 1", got)
	}
	if got := env.count(`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`, "proposal:"+proposalID); got != 1 {
		t.Fatalf("ledger txns = %d, want 1", got)
	}
	if got := env.count(`SELECT count(*) FROM outbox WHERE idempotency_key = $1`, "approval.request:"+proposalID); got != 1 {
		t.Fatalf("approval.request outbox = %d, want 1", got)
	}
	var actorType, actorID string
	if err := env.tdb.Pool.QueryRow(context.Background(),
		`SELECT actor_type, actor_id FROM audit_log
		  WHERE action = $1 AND entity_type = 'proposal' AND entity_id = $2`,
		approval.ActionApprovalDecided, proposalID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("read approval.decided actor: %v", err)
	}
	if actorType != "human" || actorID != approvedID {
		t.Fatalf("approval.decided actor = %s:%s, want human:%s", actorType, actorID, approvedID)
	}
}

func TestPipelineRejectDoesNotApply(t *testing.T) {
	env := newTestEnv(t)
	caseID, _ := env.newCase(500)
	proposalID := env.newProposal(caseID, "adjustment", adjustmentPayload(400),
		[]string{"case:" + caseID}, nil)
	pipeline := env.pipeline(nil)

	result, err := pipeline.RunCase(context.Background(), Input{
		ProposalID: proposalID,
		CaseID:     caseID,
		ScopeID:    env.scopeID,
		AsOf:       env.asOf,
	})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if result.RequestStatus != RequestStatusRejected || result.Applied {
		t.Fatalf("result = %s applied=%v, want rejected/not applied", result.RequestStatus, result.Applied)
	}
	if result.ApproverAction != ActionReject || result.ApproverRule != "amount_mismatch" {
		t.Fatalf("approver = %s/%s, want reject/amount_mismatch", result.ApproverAction, result.ApproverRule)
	}
	if result.Tieout == nil {
		t.Fatal("rejected case has no tie-out")
	}
	if got := env.count(`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, proposalID); got != 0 {
		t.Fatalf("applied_proposals = %d, want 0", got)
	}
	if got := env.count(`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`, "proposal:"+proposalID); got != 0 {
		t.Fatalf("ledger txns = %d, want 0", got)
	}
	if got := env.count(`SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid AND decision = 'rejected'`, proposalID); got != 1 {
		t.Fatalf("rejected approvals = %d, want 1", got)
	}
}

func TestPipelineT0SkipsApproverAndAutoApplies(t *testing.T) {
	env := newTestEnv(t)
	caseID, _ := env.newCase(500)
	pattern := "fee_variance"
	proposalID := env.newProposal(caseID, "adjustment", adjustmentPayload(500),
		[]string{"case:" + caseID}, &pattern)
	pipeline := env.pipeline(passedPatternVerifier{pattern: pattern})

	result, err := pipeline.RunCase(context.Background(), Input{
		ProposalID: proposalID,
		CaseID:     caseID,
		ScopeID:    env.scopeID,
		AsOf:       env.asOf,
	})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if result.PolicyTier == nil || *result.PolicyTier != "T0" {
		t.Fatalf("policy tier = %v, want T0", result.PolicyTier)
	}
	if result.ApproverAction != ActionSkipT0 || result.ApproverRule != "t0_auto_approved" {
		t.Fatalf("approver = %s/%s, want skip_t0/t0_auto_approved", result.ApproverAction, result.ApproverRule)
	}
	if !result.Applied || result.RequestStatus != RequestStatusApplied {
		t.Fatalf("result applied/status = %v/%s, want true/applied", result.Applied, result.RequestStatus)
	}
	if got := env.count(`SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`, proposalID); got != 0 {
		t.Fatalf("T0 approvals = %d, want 0 (approver must not decide)", got)
	}
	if got := env.count(`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, proposalID); got != 1 {
		t.Fatalf("applied_proposals = %d, want 1", got)
	}
}

func TestPipelineNoProposalAbstains(t *testing.T) {
	env := newTestEnv(t)
	caseID, _ := env.newCase(500)
	pipeline := env.pipeline(nil)
	result, err := pipeline.RunCase(context.Background(), Input{
		CaseID:  caseID,
		ScopeID: env.scopeID,
		AsOf:    env.asOf,
	})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if result.Proposal != ProposalNone || result.RequestStatus != RequestStatusNone {
		t.Fatalf("result proposal/status = %s/%s, want none/none", result.Proposal, result.RequestStatus)
	}
	if result.ApproverAction != ActionAbstain || result.ApproverRule != "no_proposal" {
		t.Fatalf("approver = %s/%s, want abstain/no_proposal", result.ApproverAction, result.ApproverRule)
	}
	if result.Applied || result.Tieout != nil {
		t.Fatalf("no-proposal result applied/tieout = %v/%v, want false/nil", result.Applied, result.Tieout)
	}
	if got := env.count(`SELECT count(*) FROM approvals`); got != 0 {
		t.Fatalf("approvals = %d, want 0", got)
	}
	if got := env.count(`SELECT count(*) FROM applied_proposals`); got != 0 {
		t.Fatalf("applied_proposals = %d, want 0", got)
	}
}

func TestPipelineFailClosedAndPersistsPartialResult(t *testing.T) {
	env := newTestEnv(t)
	caseID, _ := env.newCase(500)
	proposalID := env.newProposal(caseID, "adjustment", adjustmentPayload(500),
		[]string{"case:" + caseID}, nil)
	pipeline := env.pipeline(nil)

	// The apply succeeds, then tie-out fails because the as-of date has no
	// statement balance. The pipeline must persist the partial result and fail
	// closed with ErrE2EFailed.
	result, err := pipeline.RunCase(context.Background(), Input{
		ProposalID: proposalID,
		CaseID:     caseID,
		ScopeID:    env.scopeID,
		AsOf:       env.asOf.AddDate(0, 0, 1),
	})
	if !errors.Is(err, ErrE2EFailed) {
		t.Fatalf("RunCase error = %v, want ErrE2EFailed", err)
	}
	if !result.Failed || result.Error == "" {
		t.Fatalf("result failed/error = %v/%q, want true/non-empty", result.Failed, result.Error)
	}
	if !result.Applied || result.RequestStatus != RequestStatusApplied {
		t.Fatalf("partial result applied/status = %v/%s, want true/applied", result.Applied, result.RequestStatus)
	}

	runID := env.insertEvalCase("scenario-fail")
	if err := WriteCaseDetail(context.Background(), env.tdb.Pool, runID, "scenario-fail", result); err != nil {
		t.Fatalf("WriteCaseDetail: %v", err)
	}
	// Idempotent overwrite of the same key.
	if err := WriteCaseDetail(context.Background(), env.tdb.Pool, runID, "scenario-fail", result); err != nil {
		t.Fatalf("second WriteCaseDetail: %v", err)
	}
	var raw []byte
	if err := env.tdb.Pool.QueryRow(context.Background(),
		`SELECT detail->'e2e' FROM eval_case_results
		  WHERE eval_run_id = $1::uuid AND scenario_id = $2`,
		runID, "scenario-fail").Scan(&raw); err != nil {
		t.Fatalf("read persisted detail: %v", err)
	}
	var persisted Result
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("decode persisted detail: %v", err)
	}
	if !persisted.Failed || persisted.Error == "" || persisted.Schema != SchemaVersion {
		t.Fatalf("persisted result = %+v", persisted)
	}
	if persisted.RequestStatus != RequestStatusApplied || !persisted.Applied {
		t.Fatalf("persisted partial result lost apply evidence: %+v", persisted)
	}
}

func TestPipelineResumeIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	caseID, _ := env.newCase(500)
	proposalID := env.newProposal(caseID, "adjustment", adjustmentPayload(500),
		[]string{"case:" + caseID}, nil)
	pipeline := env.pipeline(nil)
	input := Input{ProposalID: proposalID, CaseID: caseID, ScopeID: env.scopeID, AsOf: env.asOf}

	first, err := pipeline.RunCase(context.Background(), input)
	if err != nil {
		t.Fatalf("first RunCase: %v", err)
	}
	second, err := pipeline.RunCase(context.Background(), input)
	if err != nil {
		t.Fatalf("second RunCase: %v", err)
	}
	if !second.Applied || second.RequestStatus != RequestStatusApplied {
		t.Fatalf("resume result = %+v, want applied", second)
	}
	if first.LedgerTxnID == "" || second.LedgerTxnID != first.LedgerTxnID {
		t.Fatalf("ledger txn ids = %q / %q, want the same non-empty id", first.LedgerTxnID, second.LedgerTxnID)
	}
	if second.ApproverAction != ActionApprove || second.ApproverRule != "exact_match" {
		t.Fatalf("resume approver = %s/%s, want reconstructed approve/exact_match",
			second.ApproverAction, second.ApproverRule)
	}
	if got := env.count(`SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`, proposalID); got != 1 {
		t.Fatalf("approvals after resume = %d, want 1", got)
	}
	if got := env.count(`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, proposalID); got != 1 {
		t.Fatalf("applied_proposals after resume = %d, want 1", got)
	}
	if got := env.count(`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`, "proposal:"+proposalID); got != 1 {
		t.Fatalf("ledger txns after resume = %d, want 1", got)
	}
	if got := env.count(`SELECT count(*) FROM outbox WHERE idempotency_key = $1`, "approval.request:"+proposalID); got != 1 {
		t.Fatalf("approval.request outbox after resume = %d, want 1", got)
	}
	if got := env.count(`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_id = $2`,
		approval.ActionApprovalDecided, proposalID); got != 1 {
		t.Fatalf("approval.decided rows after resume = %d, want 1", got)
	}
}

func TestPipelineHiddenTruthDoesNotChangeDecision(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	firstCase, _ := env.newCase(500)
	secondCase, _ := env.newCase(500)
	firstProposal := env.newProposal(firstCase, "adjustment", adjustmentPayload(500),
		[]string{"case:" + firstCase}, nil)
	secondProposal := env.newProposal(secondCase, "adjustment", adjustmentPayload(500),
		[]string{"case:" + secondCase}, nil)

	truths := []struct {
		scenario string
		mode     string
		tier     string
		caseID   string
	}{
		{"scenario-truth-a", "fee_variance", "T0", firstCase},
		{"scenario-truth-b", "reference_typo", "T2", secondCase},
	}
	for _, truth := range truths {
		if _, err := env.tdb.Pool.Exec(ctx,
			`INSERT INTO eval_truth.scenarios
			     (scenario_id, split, mode, difficulty, break_signature,
			      truth_schema_version, truth, loaded_at)
			 VALUES ($1, 'test', $2, 'realistic', '', '1', $3::jsonb, $4)`,
			truth.scenario, truth.mode, fmt.Sprintf(`{"expected_tier":%q}`, truth.tier),
			env.clock.Now()); err != nil {
			t.Fatalf("insert hidden truth %s: %v", truth.scenario, err)
		}
		if _, err := env.tdb.Pool.Exec(ctx,
			`INSERT INTO eval_truth.scenario_cases
			     (scenario_id, case_id, synthetic, scenario_time, created_at)
			 VALUES ($1, $2::uuid, false, $3, $3)`,
			truth.scenario, truth.caseID, env.clock.Now()); err != nil {
			t.Fatalf("insert hidden truth mapping %s: %v", truth.scenario, err)
		}
	}

	pipeline := env.pipeline(nil)
	outcomes := make([]Result, 0, 2)
	for _, input := range []Input{
		{ProposalID: firstProposal, CaseID: firstCase, ScopeID: env.scopeID, AsOf: env.asOf},
		{ProposalID: secondProposal, CaseID: secondCase, ScopeID: env.scopeID, AsOf: env.asOf},
	} {
		result, err := pipeline.RunCase(ctx, input)
		if err != nil {
			t.Fatalf("RunCase with hidden truth: %v", err)
		}
		outcomes = append(outcomes, result)
	}
	if outcomes[0].ApproverAction != outcomes[1].ApproverAction ||
		outcomes[0].ApproverRule != outcomes[1].ApproverRule ||
		outcomes[0].RequestStatus != outcomes[1].RequestStatus {
		t.Fatalf("truth-only change altered the decision: %+v vs %+v", outcomes[0], outcomes[1])
	}
	for _, outcome := range outcomes {
		if outcome.ApproverAction != ActionApprove || outcome.ApproverRule != "exact_match" {
			t.Fatalf("outcome = %s/%s, want approve/exact_match", outcome.ApproverAction, outcome.ApproverRule)
		}
	}
}

const approvedID = "00000000-0000-4000-8000-00000000e2e1"
