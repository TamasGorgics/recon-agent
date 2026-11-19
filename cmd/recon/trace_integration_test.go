//go:build integration

// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
	"github.com/TamasGorgics/recon-agent/internal/traceability"
)

const (
	traceCLINow      = "2026-11-19T12:00:00Z"
	traceCLIDebit    = "trace_cli_fees_expense"
	traceCLICredit   = "trace_cli_suspense"
	traceCLITxnDescr = "trace fixture ledger txn"
)

type traceCLIChain struct {
	caseID      string
	proposalID  string
	ledgerTxnID string
	configYAML  string
}

type traceCLIOptions struct {
	omitLedgerTxn    bool
	omitTraceID      bool
	omitVerification bool
	omitPolicy       bool
	omitPolicyConfig bool
	omitApprovals    bool
	omitAudit        bool
	omitBreaks       bool
	omitApplied      bool
}

// newTraceCLIFixture clones a database, points the CLI at its app-role login,
// and seeds one realistic FIN-32 chain with SQL. The reader is read-only, so a
// SQL fixture is sufficient here; internal/executor owns the apply-path shape.
func newTraceCLIFixture(t *testing.T, opts traceCLIOptions) (*testdb.DB, traceCLIChain) {
	t.Helper()
	d := testdb.New(t)
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", d.DSN)
	t.Setenv("DATABASE_APP_URL", d.DSNFor(db.RoleApp))

	ctx := context.Background()
	now := mustTraceTime(t, traceCLINow)
	chain := traceCLIChain{}

	var debit, credit, scope, matchRun string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, 'expense', 'USD', 'active') RETURNING id::text`,
		traceCLIDebit).Scan(&debit); err != nil {
		t.Fatalf("insert debit account: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, 'liability', 'USD', 'active') RETURNING id::text`,
		traceCLICredit).Scan(&credit); err != nil {
		t.Fatalf("insert credit account: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'trace_cli_psp', 'USD') RETURNING id::text`,
		debit).Scan(&scope); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs
		     (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'trace-cli-fixture')
		 RETURNING id::text`, scope, now).Scan(&matchRun); err != nil {
		t.Fatalf("insert match run: %v", err)
	}

	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO cases
		     (priority, value_minor, currency, status, version, attempts,
		      created_at, resolved_at, resolution_type)
		 VALUES (1, 500, 'USD', 'resolved', 0, 0, $1, $1, 'adjustment_applied')
		 RETURNING id::text`, now).Scan(&chain.caseID); err != nil {
		t.Fatalf("insert case: %v", err)
	}
	if !opts.omitBreaks {
		sum := sha256.Sum256([]byte("trace-cli-break"))
		var breakID string
		if err := d.Pool.QueryRow(ctx,
			`INSERT INTO breaks
			     (signature, scope_id, match_run_id, side, ledger_txn_ids,
			      external_refs, delta_minor, currency, status, detected_at)
			 VALUES ($1, $2::uuid, $3::uuid, 'external_only', ARRAY[]::uuid[],
			         '[{"source":"trace_cli_psp","source_record_id":"ext-cli"}]'::jsonb,
			         500, 'USD', 'in_case', $4)
			 RETURNING id::text`,
			hex.EncodeToString(sum[:]), scope, matchRun, now).Scan(&breakID); err != nil {
			t.Fatalf("insert break: %v", err)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO case_breaks (case_id, break_id, created_at)
			 VALUES ($1::uuid, $2::uuid, $3)`, chain.caseID, breakID, now); err != nil {
			t.Fatalf("insert case break: %v", err)
		}
	}

	var traceID any
	if !opts.omitTraceID {
		traceID = "trace-cli-1"
	}
	var agentRunID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs
		     (case_id, model, prompt_version, started_at, ended_at, outcome, trace_id)
		 VALUES ($1::uuid, 'deepseek-v4.1-flash', 'v1', $2, $2, 'proposal', $3)
		 RETURNING id::text`, chain.caseID, now, traceID).Scan(&agentRunID); err != nil {
		t.Fatalf("insert agent run: %v", err)
	}

	chain.configYAML = "schema_version: 1\n# trace cli fixture\nauto_apply:\n  max_value_minor:\n    USD: 500\n"
	configHash := policy.HashYAML([]byte(chain.configYAML))
	if !opts.omitPolicyConfig {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO policy_config_versions (hash, yaml, loaded_at)
			 VALUES ($1, $2, $3)`, configHash, chain.configYAML, now); err != nil {
			t.Fatalf("insert policy config version: %v", err)
		}
	}

	var riskTier, decision, verification any
	if !opts.omitPolicy {
		doc := policy.Decision{
			SchemaVersion: 1,
			Tier:          policy.TierT1,
			Rule:          policy.CodeDefaultSingleApproval,
			Reasons:       []string{policy.CodeDefaultSingleApproval},
			ConfigHash:    configHash,
		}
		encoded, err := doc.JSON()
		if err != nil {
			t.Fatalf("marshal decision: %v", err)
		}
		riskTier = string(policy.TierT1)
		decision = string(encoded)
	}
	if !opts.omitVerification {
		verification = `{"schema_version":1,"pattern":"fee_variance","passed":true}`
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, evidence, confidence,
		      verification, risk_tier, policy_decision, status, version, state_hash,
		      idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'adjustment',
		         '{"lines":[{"account_code":"trace_cli_fees_expense","direction":"debit","amount_minor":500,"currency":"USD"},{"account_code":"trace_cli_suspense","direction":"credit","amount_minor":500,"currency":"USD"}]}'::jsonb,
		         'trace cli fixture rationale', '[]'::jsonb, 0.9000,
		         $3::jsonb, $4, $5::jsonb, 'applied', 0, 'trace-cli-state-hash',
		         'trace-cli-idempotency', $6)
		 RETURNING id::text`,
		chain.caseID, agentRunID, verification, riskTier, decision,
		now).Scan(&chain.proposalID); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	var approverID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO approvers (display_name, role, active)
		 VALUES ('Alice Approver', 'approver', true) RETURNING id::text`).
		Scan(&approverID); err != nil {
		t.Fatalf("insert approver: %v", err)
	}
	if !opts.omitApprovals && !opts.omitPolicy {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvals
			     (proposal_id, channel, external_ref, approver_id, decision,
			      decided_at, signature_valid)
			 VALUES ($1::uuid, 'slack', 'slack-ref-cli', $2::uuid, 'approved',
			         $3, true)`,
			chain.proposalID, approverID, now.Add(5*time.Minute)); err != nil {
			t.Fatalf("insert approval: %v", err)
		}
	}

	if !opts.omitLedgerTxn {
		chain.ledgerTxnID = insertTraceCLILedgerTxn(t, d, debit, credit, now)
	}
	if !opts.omitApplied {
		var ledgerArg any
		if chain.ledgerTxnID != "" {
			ledgerArg = chain.ledgerTxnID
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals
			     (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, $2::uuid, $3, 'executor-run-cli')`,
			chain.proposalID, ledgerArg, now); err != nil {
			t.Fatalf("insert applied proposal: %v", err)
		}
	}
	if !opts.omitAudit {
		actor, err := audit.SystemActor("executor")
		if err != nil {
			t.Fatalf("build executor actor: %v", err)
		}
		testutil.AppendAudit(t, ctx, d.Pool, audit.Entry{
			Actor:      actor,
			Action:     "executor.applied",
			EntityType: "proposal",
			EntityID:   chain.proposalID,
			Before:     map[string]any{"status": "approved"},
			After: map[string]any{
				"status":          "applied",
				"ledger_txn_id":   chain.ledgerTxnID,
				"applied_at":      now.Format(time.RFC3339),
				"executor_run_id": "executor-run-cli",
			},
			Timestamp: now,
		})
	}
	return d, chain
}

func insertTraceCLILedgerTxn(t *testing.T, d *testdb.DB, debit, credit string,
	now time.Time) string {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin ledger fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (created_at, booked_at, value_date, kind, idempotency_key,
		      request_hash, description, period_id)
		 VALUES ($1, $1, '2026-11-19', 'adjustment', 'trace-cli-ledger',
		         'trace-cli-request-hash', $2,
		         '11111111-1111-4111-8111-111111111111')
		 RETURNING id::text`, now, traceCLITxnDescr).Scan(&txnID); err != nil {
		t.Fatalf("insert ledger txn: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries
		     (txn_id, account_id, direction, amount_minor, currency, posted_at, external_ref)
		 VALUES ($1::uuid, $2::uuid, 'debit', 500, 'USD', $3, 'trace-cli-ref'),
		        ($1::uuid, $4::uuid, 'credit', 500, 'USD', $5, 'trace-cli-ref')`,
		txnID, debit, now, credit, now.Add(time.Second)); err != nil {
		t.Fatalf("insert ledger entries: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ledger fixture: %v", err)
	}
	return txnID
}

func mustTraceTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

func TestTraceCLIFullChainJSON(t *testing.T) {
	_, chain := newTraceCLIFixture(t, traceCLIOptions{})

	out, err := runTraceForTest(t, "--txn", chain.ledgerTxnID, "--json")
	if err != nil {
		t.Fatalf("trace --json: %v", err)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("trace JSON does not end with a newline: %q", out)
	}

	var decoded struct {
		SchemaVersion int                     `json:"schema_version"`
		LedgerTxn     *traceability.LedgerTxn `json:"ledger_txn"`
		Chains        []traceability.Chain    `json:"chains"`
		Missing       []string                `json:"missing"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode trace JSON: %v", err)
	}
	if decoded.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", decoded.SchemaVersion)
	}
	if decoded.LedgerTxn == nil || decoded.LedgerTxn.ID != chain.ledgerTxnID {
		t.Fatalf("ledger_txn = %+v, want %s", decoded.LedgerTxn, chain.ledgerTxnID)
	}
	if len(decoded.Chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(decoded.Chains))
	}
	if len(decoded.Missing) != 0 {
		t.Fatalf("missing = %v, want none", decoded.Missing)
	}
	got := decoded.Chains[0]
	if got.AppliedProposal == nil || got.AppliedProposal.ProposalID != chain.proposalID {
		t.Fatalf("applied_proposal = %+v, want %s", got.AppliedProposal, chain.proposalID)
	}
	if got.Policy == nil || got.Policy.ConfigYAML == nil ||
		*got.Policy.ConfigYAML != chain.configYAML {
		t.Fatalf("policy config yaml = %+v, want %q", got.Policy, chain.configYAML)
	}
	if len(got.Approvals) != 1 ||
		got.Approvals[0].Approver.DisplayName != "Alice Approver" ||
		got.Approvals[0].Approver.Role != "approver" {
		t.Fatalf("approvals = %+v, want the approver display/role join", got.Approvals)
	}
	if got.Executor.Audit == nil {
		t.Fatal("executor audit is missing")
	}
	if got.LedgerTxn == nil || got.LedgerTxn.ID != chain.ledgerTxnID {
		t.Fatalf("chain ledger_txn = %+v, want %s", got.LedgerTxn, chain.ledgerTxnID)
	}
	if len(got.LedgerEntries) != 2 {
		t.Fatalf("ledger entries = %d, want 2", len(got.LedgerEntries))
	}

	// The whole document is deterministic run to run.
	outAgain, err := runTraceForTest(t, "--txn", chain.ledgerTxnID, "--json")
	if err != nil {
		t.Fatalf("trace --json second run: %v", err)
	}
	if outAgain != out {
		t.Fatalf("trace JSON is not stable:\nfirst  %s\nsecond %s", out, outAgain)
	}

	// Exactly four top-level keys.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("decode raw JSON: %v", err)
	}
	if len(raw) != 4 {
		t.Fatalf("top-level keys = %v, want exactly four", raw)
	}
	for _, key := range []string{"schema_version", "ledger_txn", "chains", "missing"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("top-level JSON is missing %q: %s", key, out)
		}
	}
}

func TestTraceCLIHumanOutput(t *testing.T) {
	_, chain := newTraceCLIFixture(t, traceCLIOptions{})

	out, err := runTraceForTest(t, "--txn", chain.ledgerTxnID)
	if err != nil {
		t.Fatalf("trace human: %v", err)
	}
	for _, line := range []string{
		"trace: ledger txn " + chain.ledgerTxnID,
		"  chains: 1",
		"  chain 1: applied proposal " + chain.proposalID,
		"trace_id=trace-cli-1",
		"approver=Alice Approver role=approver",
		"          schema_version: 1",
		"      executor.audit: seq=",
		"      ledger_txn: " + chain.ledgerTxnID,
		"        entries: 2",
		"    missing: none",
		"  missing: none",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("human output is missing %q:\n%s", line, out)
		}
	}
}

func TestTraceCLINoApplicationJSONIsExact(t *testing.T) {
	d, _ := newTraceCLIFixture(t, traceCLIOptions{omitApplied: true})

	var ledgerTxnID string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT id::text FROM ledger_txns`).Scan(&ledgerTxnID); err != nil {
		t.Fatalf("read fixture ledger txn: %v", err)
	}

	out, err := runTraceForTest(t, "--txn", ledgerTxnID, "--json")
	if err != nil {
		t.Fatalf("trace no application: %v", err)
	}
	want := `{"schema_version":1,"ledger_txn":{"id":"` + ledgerTxnID +
		`","seq":1,"kind":"adjustment","value_date":"2026-11-19",` +
		`"created_at":"2026-11-19T12:00:00Z","booked_at":"2026-11-19T12:00:00Z",` +
		`"description":"trace fixture ledger txn",` +
		`"period_id":"11111111-1111-4111-8111-111111111111",` +
		`"adjusts_period_id":null,"reversal_of":null},"chains":[],` +
		`"missing":["applied_proposal"]}` + "\n"
	if out != want {
		t.Fatalf("no-application JSON:\n got %s\nwant %s", out, want)
	}
}

func TestTraceCLIPartialChainExitsZero(t *testing.T) {
	_, chain := newTraceCLIFixture(t, traceCLIOptions{
		omitTraceID:      true,
		omitVerification: true,
		omitPolicyConfig: true,
		omitApprovals:    true,
		omitAudit:        true,
	})

	out, err := runTraceForTest(t, "--txn", chain.ledgerTxnID, "--json")
	if err != nil {
		t.Fatalf("trace partial chain = %v, want exit 0", err)
	}
	var decoded struct {
		Chains  []traceability.Chain `json:"chains"`
		Missing []string             `json:"missing"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode partial JSON: %v", err)
	}
	wantMissing := []string{
		traceability.MissingAgentRunTraceID,
		traceability.MissingVerification,
		traceability.MissingPolicyConfig,
		traceability.MissingApprovals,
		traceability.MissingExecutorAudit,
	}
	if strings.Join(decoded.Missing, ",") != strings.Join(wantMissing, ",") {
		t.Fatalf("top-level missing = %v, want %v", decoded.Missing, wantMissing)
	}
	if len(decoded.Chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(decoded.Chains))
	}
	if strings.Join(decoded.Chains[0].Missing, ",") != strings.Join(wantMissing, ",") {
		t.Fatalf("chain missing = %v, want %v", decoded.Chains[0].Missing, wantMissing)
	}

	human, err := runTraceForTest(t, "--txn", chain.ledgerTxnID)
	if err != nil {
		t.Fatalf("trace partial chain human = %v, want exit 0", err)
	}
	if !strings.Contains(human,
		"missing: agent_run.trace_id, verification, policy_config, approvals, executor.audit") {
		t.Fatalf("human output does not print every marker:\n%s", human)
	}
}

func TestTraceCLIUnknownTxnIsExitOne(t *testing.T) {
	newTraceCLIFixture(t, traceCLIOptions{})
	out, err := runTraceForTest(t,
		"--txn", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "--json")
	if err == nil {
		t.Fatal("unknown txn = nil, want exit 1")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1 (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, want a not-found message", err)
	}
	if out != "" {
		t.Fatalf("unknown txn wrote to stdout: %q", out)
	}
}

func TestTraceCLIMalformedUUIDIsExitTwo(t *testing.T) {
	newTraceCLIFixture(t, traceCLIOptions{})
	out, err := runTraceForTest(t, "--txn", "not-a-uuid", "--json")
	if err == nil {
		t.Fatal("malformed txn = nil, want exit 2")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	if out != "" {
		t.Fatalf("malformed txn wrote to stdout: %q", out)
	}
}

func TestTraceCLIQueryErrorIsExitTwo(t *testing.T) {
	d, chain := newTraceCLIFixture(t, traceCLIOptions{})
	if _, err := d.Pool.Exec(context.Background(),
		`REVOKE SELECT ON applied_proposals FROM recon_app`); err != nil {
		t.Fatalf("revoke applied_proposals SELECT: %v", err)
	}
	out, err := runTraceForTest(t, "--txn", chain.ledgerTxnID, "--json")
	if err == nil {
		t.Fatal("trace with revoked SELECT = nil, want exit 2")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("error = %v, want a 42501 permission error", err)
	}
	if out != "" {
		t.Fatalf("query error wrote to stdout: %q", out)
	}
}

func TestTraceCLITransactionIsReadOnly(t *testing.T) {
	d, chain := newTraceCLIFixture(t, traceCLIOptions{})
	ctx := context.Background()

	var before int64
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&before); err != nil {
		t.Fatalf("count audit rows before: %v", err)
	}
	if _, err := runTraceForTest(t, "--txn", chain.ledgerTxnID, "--json"); err != nil {
		t.Fatalf("trace: %v", err)
	}

	app, err := db.OpenRole(ctx, db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	defer app.Close()
	writeErr := withTraceTx(ctx, app.Pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO audit_log
			     (seq, ts, actor_type, actor_id, action, entity_type, entity_id,
			      prev_hash, row_hash)
			 OVERRIDING SYSTEM VALUE
			 VALUES (1, now(), 'system', 'trace', 'trace.write', 'trace_test',
			         'read-only-probe', decode(repeat('00', 32), 'hex'),
			         decode(repeat('00', 32), 'hex'))`)
		return err
	})
	if writeErr == nil {
		t.Fatal("write inside the trace transaction succeeded, want a read-only error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(writeErr, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write error = %v, want SQLSTATE 25006", writeErr)
	}

	var after int64
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&after); err != nil {
		t.Fatalf("count audit rows after: %v", err)
	}
	if after != before {
		t.Fatalf("audit rows changed from %d to %d", before, after)
	}
}

func TestTraceCLIDispatchUsage(t *testing.T) {
	var out strings.Builder
	if err := runTo(context.Background(), []string{"help"}, &out); err != nil {
		t.Fatalf("recon help: %v", err)
	}
	if !strings.Contains(out.String(), "trace            walk one ledger transaction") {
		t.Fatalf("top-level usage does not list trace:\n%s", out.String())
	}
}
