//go:build integration

// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package traceability_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
	"github.com/TamasGorgics/recon-agent/internal/traceability"
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "traceability: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "traceability: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

const traceFixtureNow = "2026-11-19T12:00:00Z"

var traceFixtureSeq atomic.Int64

type fixture struct {
	t      *testing.T
	d      *testdb.DB
	app    *pgxpool.Pool
	debit  string
	credit string
	scope  string
	run    string
}

type seededChain struct {
	caseID      string
	breakID     string
	agentRunID  string
	proposalID  string
	ledgerTxnID string // empty when the application is unlinked
	configYAML  string
}

type seedOptions struct {
	// caseID reuses an existing case; empty creates one.
	caseID string
	// appliedAt orders chains. Zero means fixture now + sequence.
	appliedAt time.Time
	// Each omit* flag removes exactly one link.
	omitBreaks        bool
	omitTraceID       bool
	omitVerification  bool
	omitPolicy        bool
	omitPolicyConfig  bool
	omitApprovals     bool
	omitExecutorAudit bool
	omitLedgerTxn     bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 8})
	ctx := context.Background()
	app, err := db.OpenRole(ctx, db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(app.Close)

	f := &fixture{t: t, d: d, app: app.Pool}
	f.debit = f.insertAccount("trace_fees_expense", "expense")
	f.credit = f.insertAccount("trace_suspense", "liability")
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'trace_psp', 'USD') RETURNING id::text`,
		f.debit).Scan(&f.scope); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs
		     (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'trace-fixture')
		 RETURNING id::text`, f.scope, f.now()).Scan(&f.run); err != nil {
		t.Fatalf("insert match run: %v", err)
	}
	return f
}

func (f *fixture) now() time.Time {
	parsed, err := time.Parse(time.RFC3339, traceFixtureNow)
	if err != nil {
		f.t.Fatalf("parse fixture time: %v", err)
	}
	return parsed.UTC()
}

func (f *fixture) insertAccount(code, kind string) string {
	f.t.Helper()
	var id string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, 'USD', 'active') RETURNING id::text`,
		code, kind).Scan(&id); err != nil {
		f.t.Fatalf("insert account %q: %v", code, err)
	}
	return id
}

func (f *fixture) seedChain(opts seedOptions) *seededChain {
	f.t.Helper()
	n := traceFixtureSeq.Add(1)
	chain := &seededChain{}
	appliedAt := opts.appliedAt
	if appliedAt.IsZero() {
		appliedAt = f.now().Add(time.Duration(n) * time.Second)
	}

	caseID := opts.caseID
	if caseID == "" {
		caseID = f.insertCase()
	}
	chain.caseID = caseID

	if !opts.omitBreaks {
		chain.breakID = f.insertBreak(n, caseID)
	}

	chain.agentRunID = f.insertAgentRun(caseID, opts.omitTraceID)

	configYAML := fmt.Sprintf("schema_version: 1\n# trace fixture chain %d\n", n)
	chain.configYAML = configYAML
	configHash := policy.HashYAML([]byte(configYAML))
	if !opts.omitPolicyConfig {
		if _, err := f.d.Pool.Exec(context.Background(),
			`INSERT INTO policy_config_versions (hash, yaml, loaded_at)
			 VALUES ($1, $2, $3)`, configHash, configYAML,
			f.now().Add(time.Duration(n)*time.Second)); err != nil {
			f.t.Fatalf("insert policy config version: %v", err)
		}
	}

	chain.proposalID = f.insertProposal(caseID, chain.agentRunID, n,
		configHash, opts.omitPolicy, opts.omitVerification)

	if !opts.omitLedgerTxn {
		chain.ledgerTxnID = f.insertLedgerTxn(n)
	}
	f.insertApplied(chain.proposalID, chain.ledgerTxnID, appliedAt)
	if !opts.omitApprovals && !opts.omitPolicy {
		f.insertApproval(chain.proposalID, appliedAt.Add(5*time.Minute))
	}
	if !opts.omitExecutorAudit {
		f.appendExecutorAudit(chain.proposalID, chain.ledgerTxnID, appliedAt)
	}
	return chain
}

func (f *fixture) insertCase() string {
	f.t.Helper()
	var id string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO cases
		     (priority, value_minor, currency, status, version, attempts,
		      created_at, resolved_at, resolution_type)
		 VALUES (1, 500, 'USD', 'resolved', 0, 0, $1, $1, 'adjustment_applied')
		 RETURNING id::text`, f.now()).Scan(&id); err != nil {
		f.t.Fatalf("insert case: %v", err)
	}
	return id
}

func (f *fixture) insertBreak(n int64, caseID string) string {
	f.t.Helper()
	sum := sha256.Sum256([]byte(fmt.Sprintf("trace-fixture-break-%d", n)))
	refs := `[{"source":"trace_psp","source_record_id":"ext-` +
		fmt.Sprintf("%d", n) + `"}]`
	var id string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, 'external_only', ARRAY[]::uuid[],
		         $4::jsonb, 500, 'USD', 'in_case', $5)
		 RETURNING id::text`,
		hex.EncodeToString(sum[:]), f.scope, f.run, refs, f.now()).Scan(&id); err != nil {
		f.t.Fatalf("insert break: %v", err)
	}
	if _, err := f.d.Pool.Exec(context.Background(),
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`, caseID, id, f.now()); err != nil {
		f.t.Fatalf("insert case break: %v", err)
	}
	return id
}

func (f *fixture) insertAgentRun(caseID string, omitTraceID bool) string {
	f.t.Helper()
	var traceID any
	if !omitTraceID {
		traceID = "trace-" + fmt.Sprintf("%d", traceFixtureSeq.Load())
	}
	var id string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO agent_runs
		     (case_id, model, prompt_version, started_at, ended_at, outcome, trace_id)
		 VALUES ($1::uuid, 'deepseek-v4.1-flash', 'v1', $2, $2, 'proposal', $3)
		 RETURNING id::text`, caseID, f.now(), traceID).Scan(&id); err != nil {
		f.t.Fatalf("insert agent run: %v", err)
	}
	return id
}

func (f *fixture) insertProposal(caseID, runID string, n int64,
	configHash string, omitPolicy, omitVerification bool) string {
	f.t.Helper()
	var (
		riskTier     any
		decision     any
		verification any
	)
	if !omitPolicy {
		tier := policy.TierT1
		rule := policy.CodeDefaultSingleApproval
		doc := policy.Decision{
			SchemaVersion: 1,
			Tier:          tier,
			Rule:          rule,
			Reasons:       []string{rule},
			ConfigHash:    configHash,
		}
		encoded, err := doc.JSON()
		if err != nil {
			f.t.Fatalf("marshal policy decision: %v", err)
		}
		riskTier = string(tier)
		decision = string(encoded)
	}
	if !omitVerification {
		verification = `{"schema_version":1,"pattern":"fee_variance","passed":true}`
	}

	var id string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, evidence, confidence,
		      verification, risk_tier, policy_decision, status, version, state_hash,
		      idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'adjustment',
		         '{"lines":[{"account_code":"trace_fees_expense","direction":"debit","amount_minor":500,"currency":"USD"},{"account_code":"trace_suspense","direction":"credit","amount_minor":500,"currency":"USD"}]}'::jsonb,
		         'trace fixture rationale', '[]'::jsonb, 0.9000,
		         $3::jsonb, $4, $5::jsonb, 'applied', 0, $6, $7, $8)
		 RETURNING id::text`,
		caseID, runID, verification, riskTier, decision,
		fmt.Sprintf("trace-state-%d", n),
		fmt.Sprintf("trace-idempotency-%d", n), f.now()).Scan(&id); err != nil {
		f.t.Fatalf("insert proposal: %v", err)
	}
	return id
}

func (f *fixture) insertApproval(proposalID string, decidedAt time.Time) string {
	f.t.Helper()
	var approverID string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO approvers (display_name, role, active)
		 VALUES ($1, 'approver', true) RETURNING id::text`,
		"Alice Approver "+fmt.Sprintf("%d", traceFixtureSeq.Load())).Scan(&approverID); err != nil {
		f.t.Fatalf("insert approver: %v", err)
	}
	var id string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO approvals
		     (proposal_id, channel, external_ref, approver_id, decision,
		      decided_at, signature_valid)
		 VALUES ($1::uuid, 'slack', $2, $3::uuid, 'approved', $4, true)
		 RETURNING id::text`,
		proposalID, "slack-ref-"+fmt.Sprintf("%d", traceFixtureSeq.Load()),
		approverID, decidedAt).Scan(&id); err != nil {
		f.t.Fatalf("insert approval: %v", err)
	}
	return id
}

func (f *fixture) insertApplied(proposalID, ledgerTxnID string, appliedAt time.Time) {
	f.t.Helper()
	var ledgerArg any
	if ledgerTxnID != "" {
		ledgerArg = ledgerTxnID
	}
	if _, err := f.d.Pool.Exec(context.Background(),
		`INSERT INTO applied_proposals
		     (proposal_id, ledger_txn_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, $2::uuid, $3, 'executor-run-fixture')`,
		proposalID, ledgerArg, appliedAt); err != nil {
		f.t.Fatalf("insert applied proposal: %v", err)
	}
}

func (f *fixture) insertLedgerTxn(n int64) string {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.d.Pool.Begin(ctx)
	if err != nil {
		f.t.Fatalf("begin ledger fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (created_at, booked_at, value_date, kind, idempotency_key,
		      request_hash, description, period_id)
		 VALUES ($1, $1, '2026-11-19', 'adjustment', $2, 'trace-request-hash',
		         'trace fixture ledger txn', '11111111-1111-4111-8111-111111111111')
		 RETURNING id::text`,
		f.now().Add(time.Duration(n)*time.Second),
		fmt.Sprintf("trace-ledger-%d", n)).Scan(&txnID); err != nil {
		f.t.Fatalf("insert ledger txn: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries
		     (txn_id, account_id, direction, amount_minor, currency, posted_at, external_ref)
		 VALUES ($1::uuid, $2::uuid, 'debit', 500, 'USD', $3, 'trace-ref'),
		        ($1::uuid, $4::uuid, 'credit', 500, 'USD', $5, 'trace-ref')`,
		txnID, f.debit, f.now().Add(time.Duration(n)*time.Second),
		f.credit, f.now().Add(time.Duration(n)*time.Second).Add(time.Second)); err != nil {
		f.t.Fatalf("insert ledger entries: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatalf("commit ledger fixture: %v", err)
	}
	return txnID
}

func (f *fixture) appendExecutorAudit(proposalID, ledgerTxnID string, appliedAt time.Time) {
	f.t.Helper()
	actor, err := audit.SystemActor("executor")
	if err != nil {
		f.t.Fatalf("build executor actor: %v", err)
	}
	testutil.AppendAudit(f.t, context.Background(), f.d.Pool, audit.Entry{
		Actor:      actor,
		Action:     "executor.applied",
		EntityType: "proposal",
		EntityID:   proposalID,
		Before:     map[string]any{"status": "approved"},
		After: map[string]any{
			"status":          "applied",
			"ledger_txn_id":   ledgerTxnID,
			"applied_at":      appliedAt.UTC().Format(time.RFC3339),
			"executor_run_id": "executor-run-fixture",
		},
		Timestamp: appliedAt,
	})
}

var traceReadOnlyTxOptions = pgx.TxOptions{
	IsoLevel:   pgx.RepeatableRead,
	AccessMode: pgx.ReadOnly,
}

func (f *fixture) byLedgerTxn(id string) (traceability.Trace, error) {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.app.BeginTx(ctx, traceReadOnlyTxOptions)
	if err != nil {
		f.t.Fatalf("begin read-only trace tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return traceability.ByLedgerTxn(ctx, tx, id)
}

func (f *fixture) byCase(id string) (traceability.Trace, error) {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.app.BeginTx(ctx, traceReadOnlyTxOptions)
	if err != nil {
		f.t.Fatalf("begin read-only trace tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return traceability.ByCase(ctx, tx, id)
}

func TestTraceByLedgerTxnFullChain(t *testing.T) {
	f := newFixture(t)
	seeded := f.seedChain(seedOptions{})

	trace, err := f.byLedgerTxn(seeded.ledgerTxnID)
	if err != nil {
		t.Fatalf("ByLedgerTxn: %v", err)
	}
	if trace.LedgerTxn == nil || trace.LedgerTxn.ID != seeded.ledgerTxnID {
		t.Fatalf("root ledger txn = %+v, want %s", trace.LedgerTxn, seeded.ledgerTxnID)
	}
	if len(trace.Chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(trace.Chains))
	}
	if len(trace.Missing) != 0 {
		t.Fatalf("top-level missing = %v, want none", trace.Missing)
	}

	chain := trace.Chains[0]
	if len(chain.Missing) != 0 {
		t.Fatalf("chain missing = %v, want none", chain.Missing)
	}
	if chain.AppliedProposal == nil || chain.AppliedProposal.ProposalID != seeded.proposalID {
		t.Fatalf("applied proposal = %+v, want %s", chain.AppliedProposal, seeded.proposalID)
	}
	if chain.Proposal == nil || chain.Proposal.ID != seeded.proposalID {
		t.Fatalf("proposal = %+v, want %s", chain.Proposal, seeded.proposalID)
	}
	if chain.Case == nil || chain.Case.ID != seeded.caseID {
		t.Fatalf("case = %+v, want %s", chain.Case, seeded.caseID)
	}
	if len(chain.Breaks) != 1 || chain.Breaks[0].ID != seeded.breakID {
		t.Fatalf("breaks = %+v, want %s", chain.Breaks, seeded.breakID)
	}
	if chain.AgentRun == nil {
		t.Fatal("agent run is missing")
	}
	if chain.AgentRun.TraceID == nil || *chain.AgentRun.TraceID == "" {
		t.Fatalf("agent run trace id = %v, want a value", chain.AgentRun.TraceID)
	}
	if chain.AgentRun.ID != seeded.agentRunID {
		t.Fatalf("agent run id = %s, want %s", chain.AgentRun.ID, seeded.agentRunID)
	}
	if len(chain.Verification) == 0 {
		t.Fatal("verification is missing")
	}
	if chain.Policy == nil {
		t.Fatal("policy is missing")
	}
	if chain.Policy.RiskTier == nil || *chain.Policy.RiskTier != "T1" {
		t.Fatalf("policy risk tier = %v, want T1", chain.Policy.RiskTier)
	}
	if chain.Policy.Rule != policy.CodeDefaultSingleApproval {
		t.Fatalf("policy rule = %q, want %q", chain.Policy.Rule, policy.CodeDefaultSingleApproval)
	}
	wantYAML := seeded.configYAML
	if chain.Policy.ConfigYAML == nil || *chain.Policy.ConfigYAML != wantYAML {
		t.Fatalf("config yaml = %v, want %q", chain.Policy.ConfigYAML, wantYAML)
	}
	if chain.Policy.ConfigLoadedAt == nil {
		t.Fatal("config loaded_at is missing")
	}
	if len(chain.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(chain.Approvals))
	}
	if chain.Approvals[0].Approver.DisplayName == "" ||
		chain.Approvals[0].Approver.Role != "approver" {
		t.Fatalf("approver = %+v, want display_name and role approver", chain.Approvals[0].Approver)
	}
	if chain.Executor.Audit == nil {
		t.Fatal("executor audit is missing")
	}
	var after struct {
		LedgerTxnID   string `json:"ledger_txn_id"`
		AppliedAt     string `json:"applied_at"`
		ExecutorRunID string `json:"executor_run_id"`
	}
	if err := json.Unmarshal(chain.Executor.Audit.After, &after); err != nil {
		t.Fatalf("decode executor after: %v", err)
	}
	if after.LedgerTxnID != seeded.ledgerTxnID {
		t.Fatalf("executor after ledger_txn_id = %q, want %q", after.LedgerTxnID, seeded.ledgerTxnID)
	}
	if after.AppliedAt == "" || after.ExecutorRunID != "executor-run-fixture" {
		t.Fatalf("executor after = %+v, want applied_at and fixture executor run", after)
	}
	if chain.LedgerTxn == nil || chain.LedgerTxn.ID != seeded.ledgerTxnID {
		t.Fatalf("chain ledger txn = %+v, want %s", chain.LedgerTxn, seeded.ledgerTxnID)
	}
	if len(chain.LedgerEntries) != 2 {
		t.Fatalf("ledger entries = %d, want 2", len(chain.LedgerEntries))
	}
	if chain.LedgerEntries[0].Direction != "debit" ||
		chain.LedgerEntries[1].Direction != "credit" {
		t.Fatalf("ledger entry order = %+v, want debit then credit by posted_at",
			chain.LedgerEntries)
	}

	caseTrace, err := f.byCase(seeded.caseID)
	if err != nil {
		t.Fatalf("ByCase: %v", err)
	}
	if caseTrace.Case == nil || caseTrace.Case.ID != seeded.caseID {
		t.Fatalf("ByCase root = %+v, want %s", caseTrace.Case, seeded.caseID)
	}
	if len(caseTrace.Chains) != 1 || caseTrace.Chains[0].Case.ID != seeded.caseID {
		t.Fatalf("ByCase chains = %+v, want one chain for the case", caseTrace.Chains)
	}
	if len(caseTrace.Missing) != 0 {
		t.Fatalf("ByCase missing = %v, want none", caseTrace.Missing)
	}
}

func TestTraceByCaseOrdersChainsByAppliedAt(t *testing.T) {
	f := newFixture(t)
	base := f.now()
	first := f.seedChain(seedOptions{appliedAt: base.Add(1 * time.Minute)})
	_ = f.seedChain(seedOptions{caseID: first.caseID, appliedAt: base.Add(3 * time.Minute)})
	_ = f.seedChain(seedOptions{caseID: first.caseID, appliedAt: base.Add(2 * time.Minute)})

	trace, err := f.byCase(first.caseID)
	if err != nil {
		t.Fatalf("ByCase: %v", err)
	}
	if len(trace.Chains) != 3 {
		t.Fatalf("chains = %d, want 3", len(trace.Chains))
	}
	for i := 1; i < len(trace.Chains); i++ {
		prev := trace.Chains[i-1].AppliedProposal.AppliedAt
		next := trace.Chains[i].AppliedProposal.AppliedAt
		if next.Before(prev) {
			t.Fatalf("chains are not ordered by applied_at: %s before %s", next, prev)
		}
	}
}

func TestTraceApprovalsOrderedByDecidedAt(t *testing.T) {
	f := newFixture(t)
	seeded := f.seedChain(seedOptions{})
	ctx := context.Background()

	var secondApprover string
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO approvers (display_name, role, active)
		 VALUES ('Bob Approver', 'approver', true) RETURNING id::text`).
		Scan(&secondApprover); err != nil {
		t.Fatalf("insert second approver: %v", err)
	}
	early := f.now().Add(1 * time.Minute)
	if _, err := f.d.Pool.Exec(ctx,
		`INSERT INTO approvals
		     (proposal_id, channel, external_ref, approver_id, decision,
		      decided_at, signature_valid)
		 VALUES ($1::uuid, 'ticket', 'ticket-ref-early', $2::uuid, 'approved', $3, true)`,
		seeded.proposalID, secondApprover, early); err != nil {
		t.Fatalf("insert early approval: %v", err)
	}

	trace, err := f.byLedgerTxn(seeded.ledgerTxnID)
	if err != nil {
		t.Fatalf("ByLedgerTxn: %v", err)
	}
	approvals := trace.Chains[0].Approvals
	if len(approvals) != 2 {
		t.Fatalf("approvals = %d, want 2", len(approvals))
	}
	if !approvals[0].DecidedAt.Equal(early) {
		t.Fatalf("first approval decided_at = %s, want %s", approvals[0].DecidedAt, early)
	}
	if approvals[1].DecidedAt.Before(approvals[0].DecidedAt) {
		t.Fatalf("approvals are not ordered by decided_at: %+v", approvals)
	}
}

func TestTraceMissingMarkers(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name   string
		opts   seedOptions
		marker string
	}{
		{"agent run trace id", seedOptions{omitTraceID: true}, traceability.MissingAgentRunTraceID},
		{"verification", seedOptions{omitVerification: true}, traceability.MissingVerification},
		{"policy", seedOptions{omitPolicy: true}, traceability.MissingPolicy},
		{"policy config", seedOptions{omitPolicyConfig: true}, traceability.MissingPolicyConfig},
		{"approvals", seedOptions{omitApprovals: true}, traceability.MissingApprovals},
		{"executor audit", seedOptions{omitExecutorAudit: true}, traceability.MissingExecutorAudit},
		{"breaks", seedOptions{omitBreaks: true}, traceability.MissingBreaks},
		{"ledger txn", seedOptions{omitLedgerTxn: true}, traceability.MissingLedgerTxn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seeded := f.seedChain(tc.opts)
			trace, err := f.byCase(seeded.caseID)
			if err != nil {
				t.Fatalf("ByCase: %v", err)
			}
			if len(trace.Chains) != 1 {
				t.Fatalf("chains = %d, want 1", len(trace.Chains))
			}
			got := trace.Chains[0].Missing
			if len(got) != 1 || got[0] != tc.marker {
				t.Fatalf("chain missing = %v, want [%s]", got, tc.marker)
			}
			if len(trace.Missing) != 1 || trace.Missing[0] != tc.marker {
				t.Fatalf("top-level missing = %v, want [%s]", trace.Missing, tc.marker)
			}
		})
	}
}

func TestTraceUnknownRoots(t *testing.T) {
	f := newFixture(t)
	unknown := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := f.byLedgerTxn(unknown); !errors.Is(err, traceability.ErrNotFound) {
		t.Fatalf("ByLedgerTxn unknown = %v, want ErrNotFound", err)
	}
	if _, err := f.byCase(unknown); !errors.Is(err, traceability.ErrNotFound) {
		t.Fatalf("ByCase unknown = %v, want ErrNotFound", err)
	}
}

func TestTraceRootWithoutApplications(t *testing.T) {
	f := newFixture(t)
	// A ledger transaction with no applied proposal: found root, no chains,
	// applied_proposal marker. This is the "adjustment applied outside the
	// proposal flow" history case.
	txnID := f.insertLedgerTxn(99)
	trace, err := f.byLedgerTxn(txnID)
	if err != nil {
		t.Fatalf("ByLedgerTxn no application: %v", err)
	}
	if len(trace.Chains) != 0 {
		t.Fatalf("chains = %d, want 0", len(trace.Chains))
	}
	if len(trace.Missing) != 1 || trace.Missing[0] != traceability.MissingAppliedProposal {
		t.Fatalf("missing = %v, want [applied_proposal]", trace.Missing)
	}

	// A case with no applications has the same top-level marker.
	caseID := f.insertCase()
	caseTrace, err := f.byCase(caseID)
	if err != nil {
		t.Fatalf("ByCase no application: %v", err)
	}
	if len(caseTrace.Chains) != 0 {
		t.Fatalf("case chains = %d, want 0", len(caseTrace.Chains))
	}
	if len(caseTrace.Missing) != 1 || caseTrace.Missing[0] != traceability.MissingAppliedProposal {
		t.Fatalf("case missing = %v, want [applied_proposal]", caseTrace.Missing)
	}
}

func TestTraceUnlinkedApplicationKeepsChain(t *testing.T) {
	f := newFixture(t)
	seeded := f.seedChain(seedOptions{omitLedgerTxn: true})
	trace, err := f.byCase(seeded.caseID)
	if err != nil {
		t.Fatalf("ByCase unlinked: %v", err)
	}
	if len(trace.Chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(trace.Chains))
	}
	chain := trace.Chains[0]
	if chain.LedgerTxnID != nil {
		t.Fatalf("ledger_txn_id = %v, want null", chain.LedgerTxnID)
	}
	if chain.LedgerTxn != nil {
		t.Fatalf("ledger txn = %+v, want null", chain.LedgerTxn)
	}
	if len(chain.Missing) != 1 || chain.Missing[0] != traceability.MissingLedgerTxn {
		t.Fatalf("missing = %v, want [ledger_txn]", chain.Missing)
	}
}

func TestTraceReadOnlySnapshot(t *testing.T) {
	f := newFixture(t)
	seeded := f.seedChain(seedOptions{})
	ctx := context.Background()

	var before int64
	if err := f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&before); err != nil {
		t.Fatalf("count audit rows before: %v", err)
	}

	tx, err := f.app.BeginTx(ctx, traceReadOnlyTxOptions)
	if err != nil {
		t.Fatalf("begin read-only tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := traceability.ByLedgerTxn(ctx, tx, seeded.ledgerTxnID); err != nil {
		t.Fatalf("ByLedgerTxn in read-only tx: %v", err)
	}
	_, writeErr := tx.Exec(ctx,
		`INSERT INTO audit_log
		     (seq, ts, actor_type, actor_id, action, entity_type, entity_id,
		      prev_hash, row_hash)
		 OVERRIDING SYSTEM VALUE
		 VALUES (1, now(), 'system', 'trace', 'trace.write', 'trace_test',
		         'read-only-probe', decode(repeat('00', 32), 'hex'),
		         decode(repeat('00', 32), 'hex'))`)
	if writeErr == nil {
		t.Fatal("write inside the trace transaction succeeded, want a read-only error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(writeErr, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write error = %v, want SQLSTATE 25006", writeErr)
	}

	var after int64
	if err := f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&after); err != nil {
		t.Fatalf("count audit rows after: %v", err)
	}
	if after != before {
		t.Fatalf("audit rows changed from %d to %d", before, after)
	}
}

// countingQueryer counts every statement the reader issues. It proves ByCase
// does not run one query per chain.
type countingQueryer struct {
	tx        pgx.Tx
	query     int
	queryRows int
}

func (q *countingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.query++
	return q.tx.Query(ctx, sql, args...)
}

func (q *countingQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.queryRows++
	return q.tx.QueryRow(ctx, sql, args...)
}

func TestTraceByCaseUsesConstantStatementCount(t *testing.T) {
	f := newFixture(t)
	one := f.seedChain(seedOptions{})
	five := f.seedChain(seedOptions{})
	for i := 0; i < 4; i++ {
		f.seedChain(seedOptions{caseID: five.caseID})
	}

	ctx := context.Background()
	tx, err := f.app.BeginTx(ctx, traceReadOnlyTxOptions)
	if err != nil {
		t.Fatalf("begin read-only tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	oneCounter := &countingQueryer{tx: tx}
	if _, err := traceability.ByCase(ctx, oneCounter, one.caseID); err != nil {
		t.Fatalf("ByCase one chain: %v", err)
	}
	fiveCounter := &countingQueryer{tx: tx}
	if _, err := traceability.ByCase(ctx, fiveCounter, five.caseID); err != nil {
		t.Fatalf("ByCase five chains: %v", err)
	}
	oneStatements := oneCounter.query + oneCounter.queryRows
	fiveStatements := fiveCounter.query + fiveCounter.queryRows
	if oneStatements != fiveStatements {
		t.Fatalf("statement count 1 chain = %d, 5 chains = %d; want equal (no N+1)",
			oneStatements, fiveStatements)
	}
	if oneStatements != 6 {
		t.Fatalf("statement count = %d, want 6 (root, chains, breaks, approvals, executor audit, entries)",
			oneStatements)
	}
}
