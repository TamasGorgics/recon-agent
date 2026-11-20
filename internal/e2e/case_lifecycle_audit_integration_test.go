//go:build integration

// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentproposal "github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/approval"
	proposalstate "github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/policy/verify"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
	"github.com/TamasGorgics/recon-agent/internal/traceability"
)

const (
	// e2eNow is the fixed injected clock for the whole flow.
	e2eNow = "2026-06-15T12:00:00Z"
	// e2eValueDate is the break's external value date, inside the seeded open
	// period (2020-01-01..2030-12-31).
	e2eValueDate = "2026-06-10"
	// e2eTTL is the approval service's proposal lifetime.
	e2eTTL = 48 * time.Hour
	// auditFailureTrigger is the test-only trigger that injects the
	// same-transaction executor audit failure.
	auditFailureTrigger = "m4_13_e2e_executor_audit_failure"
	// e2eApproverName is the seeded human approver's display name.
	e2eApproverName = "M4-13 E2E Approver"
	// e2eTraceID is the agent run's persisted trace id.
	e2eTraceID = "trace-m4-13-e2e"
)

// flow owns one isolated database and the real services the e2e drives.
type flow struct {
	t  *testing.T
	db *testdb.DB

	app      *pgxpool.Pool
	exec     *pgxpool.Pool
	clock    *clock.Fake
	caseSvc  *cases.Service
	approval *approval.Service
	applySvc *executor.Service
	ledger   *ledger.Service

	scopeID    string
	batchID    string
	matchRunID string
	external   match.RecordRef

	breakID    string
	caseID     string
	agentRunID string
	proposalID string
	approverID string
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

// newFlow seeds the non-audited inputs (chart of accounts, scope, batch, match
// run, external record, approver) and constructs the real services. Every
// audited mutation after this point runs through a production service.
func newFlow(t *testing.T) *flow {
	t.Helper()
	ctx := context.Background()

	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 8})
	clk := clock.NewFake(mustParseTime(t, e2eNow))
	f := &flow{t: t, db: d, clock: clk}

	appRole, err := db.OpenRole(ctx, db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(appRole.Close)
	f.app = appRole.Pool

	execRole, err := db.OpenRole(ctx, db.RoleExecutor, d.DSNFor(db.RoleExecutor))
	if err != nil {
		t.Fatalf("open recon_executor pool: %v", err)
	}
	t.Cleanup(execRole.Close)
	f.exec = execRole.Pool

	clearingID := f.insertAccount(ctx, "psp_clearing_usd", "asset")
	f.insertAccount(ctx, "fees_expense", "expense")
	f.insertAccount(ctx, "suspense", "liability")

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD') RETURNING id::text`,
		clearingID).Scan(&f.scopeID); err != nil {
		t.Fatalf("insert scope: %v", err)
	}

	batchSum := sha256.Sum256([]byte("m4-13-e2e-batch"))
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ('psp_a', 0, 'USD', $1, 0, 0) RETURNING id::text`,
		hex.EncodeToString(batchSum[:])).Scan(&f.batchID); err != nil {
		t.Fatalf("insert settlement batch: %v", err)
	}

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'm4-13-e2e')
		 RETURNING id::text`, f.scopeID, clk.Now()).Scan(&f.matchRunID); err != nil {
		t.Fatalf("insert match run: %v", err)
	}

	const recordID = "e2e-rec-1"
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ('psp_a', $1, 'charge', 500, 'USD', 0, $2, $3::date, $4::uuid,
		         '{}'::jsonb, 'm4-13-e2e-external')`,
		recordID, clk.Now(), e2eValueDate, f.batchID); err != nil {
		t.Fatalf("insert external record: %v", err)
	}
	f.external = match.RecordRef{Source: "psp_a", SourceRecordID: recordID}

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO approvers (display_name, role, active)
		 VALUES ($1, 'approver', true) RETURNING id::text`,
		e2eApproverName).Scan(&f.approverID); err != nil {
		t.Fatalf("insert approver: %v", err)
	}

	caseActor, err := audit.SystemActor("cases")
	if err != nil {
		t.Fatalf("cases actor: %v", err)
	}
	f.caseSvc, err = cases.NewService(f.app, clk, caseActor)
	if err != nil {
		t.Fatalf("new case service: %v", err)
	}

	f.ledger = ledger.NewService(clk, config.DefaultLedgerConfig())

	proposalStateSvc, err := proposalstate.NewService(f.app, clk)
	if err != nil {
		t.Fatalf("new proposal state service: %v", err)
	}
	verifierSvc, err := verify.NewService(f.app, verify.OptionsFromConfig(config.VerifierConfig{}))
	if err != nil {
		t.Fatalf("new verifier service: %v", err)
	}
	limitsSvc, err := limits.NewService(limits.Options{
		Pool:   f.app,
		Clock:  clk,
		Config: config.DefaultAutoApplyLimitsConfig(),
	})
	if err != nil {
		t.Fatalf("new limits service: %v", err)
	}
	loaded, err := policy.LoadFile("../../policy.yaml")
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	engine, err := policy.NewEngine(loaded, policy.Options{})
	if err != nil {
		t.Fatalf("new policy engine: %v", err)
	}
	f.approval, err = approval.NewService(approval.Options{
		Pool:     f.app,
		Clock:    clk,
		State:    proposalStateSvc,
		Verifier: approval.VerifierAdapter{Service: verifierSvc},
		Limits:   limitsSvc,
		Policy:   approval.Policy{Engine: engine, Loaded: loaded},
		TTL:      e2eTTL,
	})
	if err != nil {
		t.Fatalf("new approval service: %v", err)
	}

	executorStateSvc, err := proposalstate.NewService(f.exec, clk)
	if err != nil {
		t.Fatalf("new executor state service: %v", err)
	}
	f.applySvc, err = executor.NewService(executor.Options{
		Pool:          f.exec,
		Ledger:        f.ledger,
		State:         executorStateSvc,
		Clock:         clk,
		ExecutorRunID: "executor:m4-13-e2e",
		BatchSize:     4,
		PollInterval:  time.Millisecond,
		Sleep:         func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("new executor service: %v", err)
	}
	return f
}

func (f *flow) insertAccount(ctx context.Context, code, accountType string) string {
	f.t.Helper()
	var id string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, 'USD', 'active') RETURNING id::text`,
		code, accountType).Scan(&id); err != nil {
		f.t.Fatalf("insert account %s: %v", code, err)
	}
	return id
}

// TestCaseLifecycleAuditCompleteness drives the real break -> proposal ->
// approval -> executor apply -> ledger flow and asserts the complete audit
// registry, the ledger-to-approver trace, the intact hash chain, and the
// same-transaction rollback of a failed apply.
func TestCaseLifecycleAuditCompleteness(t *testing.T) {
	ctx := context.Background()
	f := newFlow(t)

	created, err := f.caseSvc.CreateBreaks(ctx, cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: f.matchRunID,
		Breaks: []match.BreakCandidate{{
			ScopeID:         f.scopeID,
			Side:            match.SideExternalOnly,
			ExternalRecords: []match.RecordRef{f.external},
			Currency:        "USD",
			DeltaMinor:      500,
		}},
	})
	if err != nil {
		t.Fatalf("CreateBreaks: %v", err)
	}
	if created.CreatedBreaks != 1 || len(created.Breaks) != 1 || len(created.Cases) != 1 {
		t.Fatalf("CreateBreaks result = %+v, want one new break and one new case", created)
	}
	f.breakID = created.Breaks[0].ID
	f.caseID = created.Cases[0].ID
	if got := created.Cases[0].Status; got != cases.StatusOpen {
		t.Fatalf("created case status = %s, want %s", got, cases.StatusOpen)
	}

	f.insertProposalFixture(ctx)

	requested, err := f.approval.Request(ctx, f.proposalID)
	if err != nil {
		t.Fatalf("approval.Request: %v", err)
	}
	if requested.Tier != policy.TierT1 || requested.Status != string(proposalstate.StatusPendingApproval) {
		t.Fatalf("Request result = %+v, want T1 pending_approval", requested)
	}
	if requested.Channel != approval.ChannelSlack {
		t.Fatalf("Request channel = %q, want %q", requested.Channel, approval.ChannelSlack)
	}
	if got := f.count(ctx, `SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"approval.request:"+f.proposalID); got != 1 {
		t.Fatalf("approval.request outbox rows = %d, want 1", got)
	}

	decided, err := f.approval.Decide(ctx, f.proposalID, approval.DecisionApproved, f.approverID, "e2e")
	if err != nil {
		t.Fatalf("approval.Decide: %v", err)
	}
	if decided.Status != string(proposalstate.StatusApproved) || decided.Replayed {
		t.Fatalf("Decide result = %+v, want a fresh approved decision", decided)
	}
	var actorType, actorID string
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT actor_type, actor_id FROM audit_log
		  WHERE action = $1 AND entity_type = 'proposal' AND entity_id = $2`,
		approval.ActionApprovalDecided, f.proposalID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("read approval.decided actor: %v", err)
	}
	if actorType != "human" || actorID != f.approverID {
		t.Fatalf("approval.decided actor = %s:%s, want human:%s", actorType, actorID, f.approverID)
	}
	if got := f.count(ctx, `SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`,
		f.proposalID); got != 1 {
		t.Fatalf("approvals rows = %d, want 1", got)
	}

	// The pre-apply registry covers every service mutation committed so far.
	preApply := f.expectedAuditRegistry("")
	testutil.AssertAuditCompleteness(t, f.app, preApply)
	testutil.AssertAuditCompletenessExact(t, f.app, preApply, f.scopeIDs()...)
	preRows, preHead := f.auditCounts(ctx)

	// Same-transaction proof: the trigger aborts the apply after the ledger,
	// applied_proposals, break/case resolution, and two audit writes all ran in
	// the serializable apply transaction. The whole transaction must roll back.
	f.installAuditFailureTrigger(ctx)
	t.Cleanup(func() { f.dropAuditFailureTrigger(context.Background()) })

	if _, err := f.applySvc.Apply(ctx, f.proposalID); err == nil {
		t.Fatal("Apply with injected executor.applied audit failure succeeded, want a rollback error")
	} else if !strings.Contains(err.Error(), "m4-13 e2e injected executor.applied audit failure") {
		t.Fatalf("Apply error = %v, want the injected audit failure", err)
	}
	f.assertNoApplySideEffects(ctx, preApply, preRows, preHead)

	f.dropAuditFailureTrigger(ctx)
	applied, err := f.applySvc.Apply(ctx, f.proposalID)
	if err != nil {
		t.Fatalf("retry Apply: %v", err)
	}
	if applied.Status != executor.StatusApplied || applied.Replayed || applied.LedgerTxnID == "" {
		t.Fatalf("retry Apply result = %+v, want applied with a ledger txn", applied)
	}

	want := f.expectedAuditRegistry(applied.LedgerTxnID)
	testutil.AssertAuditCompleteness(t, f.app, want)
	testutil.AssertAuditCompletenessExact(t, f.app, want, f.scopeIDs()...)
	f.assertAuditRowCounts(ctx, applied.LedgerTxnID)

	f.assertTraceToApprover(ctx, applied.LedgerTxnID)

	report, err := audit.Verify(ctx, f.app, 1)
	if err != nil {
		t.Fatalf("audit.Verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("audit.Verify findings = %+v, want an intact chain", report.Problems)
	}
	rows, _ := f.auditCounts(ctx)
	if report.HeadSeq != int64(rows) {
		t.Fatalf("audit.Verify head = %d, want %d", report.HeadSeq, rows)
	}
}

// insertProposalFixture writes the agent run and the proposed proposal. These
// are the only fixture-only mutations: agent_runs and proposals are both
// written by the real agent path without an audit row (the agent-run insert is
// explicitly unaudited in M2-01; the proposal tool inserts no audit either).
// Everything the proposal triggers afterwards runs through real services.
func (f *flow) insertProposalFixture(ctx context.Context) {
	f.t.Helper()
	f.insertProposalFixtureWithLines(ctx, []map[string]any{
		{"account_code": "fees_expense", "direction": "debit", "amount_minor": 500, "currency": "USD"},
		{"account_code": "suspense", "direction": "credit", "amount_minor": 500, "currency": "USD"},
	})
}

// insertProposalFixtureWithLines writes the agent run and a proposed adjustment
// with the supplied balanced payload lines. The fixture-only mutations are the
// same as insertProposalFixture; a gate can use it to give the applied entry a
// real effect on the scope's clearing account without changing the real
// approval, executor, audit, or ledger paths.
func (f *flow) insertProposalFixtureWithLines(ctx context.Context, lines []map[string]any) {
	f.t.Helper()
	evidence := []string{"case:" + f.caseID}
	resolved, err := agentproposal.ResolveState(ctx, f.db.Pool, f.caseID, evidence)
	if err != nil {
		f.t.Fatalf("resolve proposal state: %v", err)
	}
	stateHash, err := agentproposal.Hash(resolved)
	if err != nil {
		f.t.Fatalf("hash proposal state: %v", err)
	}

	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at, trace_id)
		 VALUES ($1::uuid, 'e2e-model', 'e2e-prompt-v1', $2, $3) RETURNING id::text`,
		f.caseID, f.clock.Now(), e2eTraceID).Scan(&f.agentRunID); err != nil {
		f.t.Fatalf("insert agent run: %v", err)
	}

	payload, err := json.Marshal(map[string]any{"lines": lines})
	if err != nil {
		f.t.Fatalf("marshal proposal payload: %v", err)
	}
	encodedEvidence, err := json.Marshal(evidence)
	if err != nil {
		f.t.Fatalf("marshal proposal evidence: %v", err)
	}

	proposalID, err := agentproposal.NewUUID()
	if err != nil {
		f.t.Fatalf("generate proposal id: %v", err)
	}
	f.proposalID = proposalID
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO proposals
		     (id, case_id, agent_run_id, type, payload, rationale, evidence,
		      confidence, status, version, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'adjustment', $4::jsonb,
		         'm4-13 e2e fixture', $5::jsonb, 0.9000, 'proposed', 0, $6, $7, $8)`,
		f.proposalID, f.caseID, f.agentRunID, string(payload), string(encodedEvidence),
		stateHash, "m4-13-e2e-"+f.proposalID, f.clock.Now()); err != nil {
		f.t.Fatalf("insert proposal: %v", err)
	}
}

// expectedAuditRegistry is the explicit, stable set of distinct audit triples
// the flow's real services write. proposal.transition appears once because the
// helper deduplicates triples; it is written three times (Request, Decide,
// Apply) and the row-count assertion below pins those rows individually.
func (f *flow) expectedAuditRegistry(ledgerTxnID string) []testutil.Expectation {
	expected := []testutil.Expectation{
		{Action: cases.ActionBreakCreate, EntityType: cases.EntityBreak, EntityID: f.breakID},
		{Action: cases.ActionBreakInCase, EntityType: cases.EntityBreak, EntityID: f.breakID},
		{Action: cases.ActionCaseCreate, EntityType: cases.EntityCase, EntityID: f.caseID},
		{Action: approval.ActionProposalPolicy, EntityType: proposalstate.EntityProposal, EntityID: f.proposalID},
		{Action: proposalstate.ActionProposalTransition, EntityType: proposalstate.EntityProposal, EntityID: f.proposalID},
		{Action: approval.ActionApprovalDecided, EntityType: proposalstate.EntityProposal, EntityID: f.proposalID},
	}
	if ledgerTxnID == "" {
		return expected
	}
	return append(expected,
		testutil.Expectation{Action: executor.ActionBreakResolved, EntityType: executor.EntityBreak, EntityID: f.breakID},
		testutil.Expectation{Action: casestate.ActionCaseTransition, EntityType: casestate.EntityCase, EntityID: f.caseID},
		testutil.Expectation{Action: executor.ActionCaseResolved, EntityType: executor.EntityCase, EntityID: f.caseID},
		testutil.Expectation{Action: executor.ActionApplied, EntityType: executor.EntityProposal, EntityID: f.proposalID},
		testutil.Expectation{Action: ledger.ActionPost, EntityType: ledger.EntityTxn, EntityID: ledgerTxnID},
	)
}

// scopeIDs are every entity this flow created. Exact mode ignores unrelated
// audit rows outside this set while still checking every missing expectation
// globally.
func (f *flow) scopeIDs() []string {
	return []string{f.breakID, f.caseID, f.agentRunID, f.proposalID}
}

// assertNoApplySideEffects proves the failed apply rolled back every domain,
// ledger, outbox, and audit write of the serializable transaction.
func (f *flow) assertNoApplySideEffects(ctx context.Context, expected []testutil.Expectation, preRows int, preHead int64) {
	f.t.Helper()
	if got := f.count(ctx, `SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, f.proposalID); got != 0 {
		f.t.Errorf("applied_proposals rows after failed apply = %d, want 0", got)
	}
	if got := f.count(ctx, `SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`, "proposal:"+f.proposalID); got != 0 {
		f.t.Errorf("ledger txns after failed apply = %d, want 0", got)
	}
	if got := f.count(ctx,
		`SELECT count(*) FROM ledger_entries le
		   JOIN ledger_txns lt ON lt.id = le.txn_id
		  WHERE lt.idempotency_key = $1`, "proposal:"+f.proposalID); got != 0 {
		f.t.Errorf("ledger entries after failed apply = %d, want 0", got)
	}
	if got := f.count(ctx, `SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+f.proposalID); got != 0 {
		f.t.Errorf("proposal.applied outbox rows after failed apply = %d, want 0", got)
	}
	if got := f.count(ctx, `SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"approval.request:"+f.proposalID); got != 1 {
		f.t.Errorf("approval.request outbox rows after failed apply = %d, want 1", got)
	}

	var (
		status  string
		version int
	)
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT status, version FROM proposals WHERE id = $1::uuid`, f.proposalID).
		Scan(&status, &version); err != nil {
		f.t.Fatalf("read proposal after failed apply: %v", err)
	}
	if status != string(proposalstate.StatusApproved) || version != 2 {
		f.t.Errorf("proposal after failed apply = %s v%d, want approved v2", status, version)
	}

	var (
		caseStatus   string
		caseVersion  int
		resolvedAt   *time.Time
		resolutionTp *string
	)
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT status, version, resolved_at, resolution_type FROM cases WHERE id = $1::uuid`,
		f.caseID).Scan(&caseStatus, &caseVersion, &resolvedAt, &resolutionTp); err != nil {
		f.t.Fatalf("read case after failed apply: %v", err)
	}
	if caseStatus != string(cases.StatusOpen) || caseVersion != 0 || resolvedAt != nil || resolutionTp != nil {
		f.t.Errorf("case after failed apply = %s v%d resolved_at=%v resolution_type=%v, want open v0 unresolved",
			caseStatus, caseVersion, resolvedAt, resolutionTp)
	}

	var breakStatus string
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT status FROM breaks WHERE id = $1::uuid`, f.breakID).Scan(&breakStatus); err != nil {
		f.t.Fatalf("read break after failed apply: %v", err)
	}
	if breakStatus != string(cases.BreakStatusInCase) {
		f.t.Errorf("break after failed apply = %s, want in_case", breakStatus)
	}

	rows, head := f.auditCounts(ctx)
	if rows != preRows || head != preHead {
		f.t.Errorf("audit rows/head after failed apply = %d/%d, want unchanged %d/%d", rows, head, preRows, preHead)
	}
	testutil.AssertAuditCompletenessExact(f.t, f.app, expected, f.scopeIDs()...)
}

// assertAuditRowCounts pins the physical row count behind each expected triple
// so a duplicated write cannot hide behind triple deduplication.
func (f *flow) assertAuditRowCounts(ctx context.Context, ledgerTxnID string) {
	f.t.Helper()
	rows := map[testutil.Expectation]int{
		{Action: cases.ActionBreakCreate, EntityType: cases.EntityBreak, EntityID: f.breakID}:                              1,
		{Action: cases.ActionBreakInCase, EntityType: cases.EntityBreak, EntityID: f.breakID}:                              1,
		{Action: cases.ActionCaseCreate, EntityType: cases.EntityCase, EntityID: f.caseID}:                                 1,
		{Action: approval.ActionProposalPolicy, EntityType: proposalstate.EntityProposal, EntityID: f.proposalID}:          1,
		{Action: proposalstate.ActionProposalTransition, EntityType: proposalstate.EntityProposal, EntityID: f.proposalID}: 3,
		{Action: approval.ActionApprovalDecided, EntityType: proposalstate.EntityProposal, EntityID: f.proposalID}:         1,
		{Action: executor.ActionBreakResolved, EntityType: executor.EntityBreak, EntityID: f.breakID}:                      1,
		{Action: casestate.ActionCaseTransition, EntityType: casestate.EntityCase, EntityID: f.caseID}:                     1,
		{Action: executor.ActionCaseResolved, EntityType: executor.EntityCase, EntityID: f.caseID}:                         1,
		{Action: executor.ActionApplied, EntityType: executor.EntityProposal, EntityID: f.proposalID}:                      1,
		{Action: ledger.ActionPost, EntityType: ledger.EntityTxn, EntityID: ledgerTxnID}:                                   1,
	}
	for expectation, want := range rows {
		got := f.count(ctx,
			`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_type = $2 AND entity_id = $3`,
			expectation.Action, expectation.EntityType, expectation.EntityID)
		if got != want {
			f.t.Errorf("audit rows for %s = %d, want %d", expectation, got, want)
		}
	}
}

// assertTraceToApprover walks the committed ledger txn back through the applied
// proposal, case, break, agent run, verification, policy, approval, executor
// audit, and ledger entries to the human approver.
func (f *flow) assertTraceToApprover(ctx context.Context, ledgerTxnID string) {
	f.t.Helper()
	f.assertTraceToApproverIdentity(ctx, ledgerTxnID, f.approverID, e2eApproverName)
}

// assertTraceToApproverIdentity walks the committed ledger txn back through the
// applied proposal, case, break, agent run, verification, policy, approval,
// executor audit, and ledger entries to the expected approver identity.
func (f *flow) assertTraceToApproverIdentity(ctx context.Context, ledgerTxnID, approverID, displayName string) {
	f.t.Helper()
	trace, err := traceability.ByLedgerTxn(ctx, f.app, ledgerTxnID)
	if err != nil {
		f.t.Fatalf("traceability.ByLedgerTxn: %v", err)
	}
	if trace.LedgerTxn == nil || trace.LedgerTxn.ID != ledgerTxnID {
		f.t.Fatalf("trace root = %+v, want ledger txn %s", trace.LedgerTxn, ledgerTxnID)
	}
	if len(trace.Chains) != 1 {
		f.t.Fatalf("trace chains = %d, want 1", len(trace.Chains))
	}
	chain := trace.Chains[0]
	if len(chain.Missing) != 0 {
		f.t.Fatalf("trace chain missing = %v, want none", chain.Missing)
	}
	if chain.AppliedProposal == nil || chain.AppliedProposal.ProposalID != f.proposalID {
		f.t.Fatalf("trace applied proposal = %+v, want %s", chain.AppliedProposal, f.proposalID)
	}
	if chain.Proposal == nil || chain.Proposal.ID != f.proposalID {
		f.t.Fatalf("trace proposal = %+v, want %s", chain.Proposal, f.proposalID)
	}
	if chain.Case == nil || chain.Case.ID != f.caseID {
		f.t.Fatalf("trace case = %+v, want %s", chain.Case, f.caseID)
	}
	if len(chain.Breaks) != 1 || chain.Breaks[0].ID != f.breakID {
		f.t.Fatalf("trace breaks = %+v, want %s", chain.Breaks, f.breakID)
	}
	if chain.AgentRun == nil || chain.AgentRun.ID != f.agentRunID ||
		chain.AgentRun.TraceID == nil || *chain.AgentRun.TraceID != e2eTraceID {
		f.t.Fatalf("trace agent run = %+v, want id %s trace_id %s", chain.AgentRun, f.agentRunID, e2eTraceID)
	}
	if len(chain.Verification) == 0 {
		f.t.Fatal("trace verification is empty, want the persisted verifier document")
	}
	if chain.Policy == nil || chain.Policy.RiskTier == nil || *chain.Policy.RiskTier != string(policy.TierT1) {
		f.t.Fatalf("trace policy = %+v, want risk tier T1", chain.Policy)
	}
	if chain.Policy.ConfigYAML == nil || len(*chain.Policy.ConfigYAML) == 0 {
		f.t.Fatal("trace policy config_yaml is empty, want the loaded document")
	}
	if len(chain.Approvals) != 1 {
		f.t.Fatalf("trace approvals = %d, want 1", len(chain.Approvals))
	}
	appr := chain.Approvals[0]
	if appr.Approver.ID != approverID || appr.Approver.DisplayName != displayName ||
		appr.Approver.Role != "approver" || appr.Decision != string(approval.DecisionApproved) {
		f.t.Fatalf("trace approval = %+v, want approver %s (%s) with an approved decision",
			appr, approverID, displayName)
	}
	if chain.Executor.Audit == nil {
		f.t.Fatal("trace executor audit is nil, want executor.applied")
	}
	if chain.LedgerTxn == nil || chain.LedgerTxn.ID != ledgerTxnID {
		f.t.Fatalf("trace chain ledger txn = %+v, want %s", chain.LedgerTxn, ledgerTxnID)
	}
	if len(chain.LedgerEntries) != 2 {
		f.t.Fatalf("trace ledger entries = %d, want 2", len(chain.LedgerEntries))
	}
}

// auditCounts returns the committed audit row count and the append-only chain
// head seq. Both must be unchanged after a rolled-back apply.
func (f *flow) auditCounts(ctx context.Context) (rows int, head int64) {
	f.t.Helper()
	if err := f.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&rows); err != nil {
		f.t.Fatalf("count audit rows: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(seq), 0) FROM audit_chain_head`).Scan(&head); err != nil {
		f.t.Fatalf("read audit head: %v", err)
	}
	return rows, head
}

func (f *flow) count(ctx context.Context, query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// installAuditFailureTrigger installs a test-only BEFORE INSERT trigger that
// aborts the apply transaction when the executor writes its summary audit row,
// after the ledger txn, applied_proposals row, break/case resolution, and the
// proposal transition audit have already run in that same transaction.
func (f *flow) installAuditFailureTrigger(ctx context.Context) {
	f.t.Helper()
	if _, err := f.db.Pool.Exec(ctx, `
CREATE OR REPLACE FUNCTION `+auditFailureTrigger+`() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.action = 'executor.applied' THEN
        RAISE EXCEPTION 'm4-13 e2e injected executor.applied audit failure';
    END IF;
    RETURN NEW;
END;
$$`); err != nil {
		f.t.Fatalf("create audit failure function: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`CREATE TRIGGER `+auditFailureTrigger+`
		 BEFORE INSERT ON audit_log
		 FOR EACH ROW EXECUTE FUNCTION `+auditFailureTrigger+`()`); err != nil {
		f.t.Fatalf("create audit failure trigger: %v", err)
	}
}

func (f *flow) dropAuditFailureTrigger(ctx context.Context) {
	f.t.Helper()
	if _, err := f.db.Pool.Exec(ctx,
		`DROP TRIGGER IF EXISTS `+auditFailureTrigger+` ON audit_log`); err != nil {
		f.t.Fatalf("drop audit failure trigger: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`DROP FUNCTION IF EXISTS `+auditFailureTrigger+`()`); err != nil {
		f.t.Fatalf("drop audit failure function: %v", err)
	}
}
