//go:build integration

package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/e2e"
	"github.com/TamasGorgics/recon-agent/eval/runner"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// e2eFixture builds a tiny sandbox world with unknown-deposit cases. The
// unknown-deposit break belongs to a bank_x scope, so the tie-out harness has a
// statement-balance row to use.
func e2eFixture(t *testing.T, count int) (*dataset.Dataset, dataset.Config, []dataset.ScenarioTruth) {
	t.Helper()
	cfg, err := dataset.DefaultConfig(dataset.Dev)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Days = 1
	cfg.Modes = modes.Config{
		Total:  count,
		Counts: map[string]int{modes.UnknownDeposit: count},
	}
	ds, err := dataset.Build(cfg)
	if err != nil {
		t.Fatalf("build e2e fixture: %v", err)
	}
	scoreable := make([]dataset.ScenarioTruth, 0, count)
	for _, truth := range ds.Truth.Scenarios {
		if truth.BreakSignature != "" || truth.Mode == modes.InjectionMemo {
			scoreable = append(scoreable, truth)
		}
	}
	sort.Slice(scoreable, func(i, j int) bool {
		return scoreable[i].ScenarioID < scoreable[j].ScenarioID
	})
	if len(scoreable) != count {
		t.Fatalf("scoreable scenarios = %d, want %d", len(scoreable), count)
	}
	for _, truth := range scoreable {
		if truth.Mode != modes.UnknownDeposit {
			t.Fatalf("scenario %s mode = %q, want %q", truth.ScenarioID, truth.Mode, modes.UnknownDeposit)
		}
	}
	return ds, cfg, scoreable
}

// e2eAdjustmentLines builds a balanced adjustment using the sandbox dataset's
// own currency-suffixed account allowlist. The amount is the observable case
// delta, so the exact-match approver approves it.
func e2eAdjustmentLines(cfg dataset.Config, truth dataset.ScenarioTruth) ([]map[string]any, error) {
	if len(truth.Amounts) == 0 {
		return nil, fmt.Errorf("scenario %s has no truth amount", truth.ScenarioID)
	}
	amount := truth.Amounts[0].AmountMinor
	if amount < 0 {
		amount = -amount
	}
	if amount <= 0 {
		return nil, fmt.Errorf("scenario %s amount is not positive", truth.ScenarioID)
	}
	currency := truth.Amounts[0].Currency
	lower := strings.ToLower(currency)
	debit := "fees_expense_" + lower
	credit := "refunds_expense_" + lower
	if !cfg.Ledger.AllowsAdjustment(debit) || !cfg.Ledger.AllowsAdjustment(credit) {
		return nil, fmt.Errorf("dataset ledger config does not allow %s/%s", debit, credit)
	}
	return []map[string]any{
		{"account_code": debit, "direction": "debit", "amount_minor": amount, "currency": currency},
		{"account_code": credit, "direction": "credit", "amount_minor": amount, "currency": currency},
	}, nil
}

// e2eClient scripts one adjustment proposal (pattern "other" -> T1) and
// escalations for every other scenario. It never uses the network.
func e2eClient(cfg dataset.Config,
	adjustScenario string) func(sandbox.CaseRef, dataset.ScenarioTruth) (llm.Client, error) {
	return func(ref sandbox.CaseRef, truth dataset.ScenarioTruth) (llm.Client, error) {
		if ref.ScenarioID != adjustScenario {
			args := fmt.Sprintf(`{"case_id":%q,"reason":"eval_scripted","summary":"scripted escalation"}`, ref.CaseID)
			return llm.NewScripted(llm.ScriptedStep{Response: toolCallResponse("escalate", args)}), nil
		}
		lines, err := e2eAdjustmentLines(cfg, truth)
		if err != nil {
			return nil, err
		}
		evidence := []string{"case:" + strings.ToLower(ref.CaseID)}
		for _, record := range truth.ExternalRecords {
			evidence = append(evidence, "ext:"+record.Source+":"+record.SourceRecordID)
		}
		proposalArgs, err := json.Marshal(map[string]any{
			"case_id":    ref.CaseID,
			"rationale":  "e2e synthetic adjustment",
			"evidence":   evidence,
			"confidence": 0.9,
			"pattern":    "other",
			"lines":      lines,
		})
		if err != nil {
			return nil, err
		}
		getArgs := fmt.Sprintf(`{"case_id":%q}`, ref.CaseID)
		return llm.NewScripted(
			llm.ScriptedStep{Response: toolCallResponse("get_case", getArgs)},
			llm.ScriptedStep{Response: toolCallResponse("propose_adjustment", string(proposalArgs))},
		), nil
	}
}

type e2eCaseRow struct {
	ScenarioID string
	TierActual *string
	Result     e2e.Result
}

func loadE2ECaseRows(t *testing.T, pool *pgxpool.Pool, runID string) []e2eCaseRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT scenario_id, tier_actual, detail->'e2e'
		   FROM eval_case_results
		  WHERE eval_run_id = $1::uuid
		  ORDER BY scenario_id`, runID)
	if err != nil {
		t.Fatalf("load e2e case rows: %v", err)
	}
	defer rows.Close()
	var results []e2eCaseRow
	for rows.Next() {
		var (
			row     e2eCaseRow
			rawJSON []byte
		)
		if err := rows.Scan(&row.ScenarioID, &row.TierActual, &rawJSON); err != nil {
			t.Fatalf("scan e2e case row: %v", err)
		}
		if len(rawJSON) == 0 {
			t.Fatalf("scenario %s has no detail e2e object", row.ScenarioID)
		}
		if err := json.Unmarshal(rawJSON, &row.Result); err != nil {
			t.Fatalf("decode e2e result for %s: %v", row.ScenarioID, err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate e2e case rows: %v", err)
	}
	return results
}

type e2eResults struct {
	app   *pgxpool.Pool
	dsn   string
	admin *pgxpool.Pool
}

func newE2EResults(t *testing.T) e2eResults {
	t.Helper()
	d := testdb.New(t)
	appRole, err := db.OpenRole(context.Background(), db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open app results pool: %v", err)
	}
	t.Cleanup(appRole.Close)
	return e2eResults{app: appRole.Pool, dsn: d.DSNFor(db.RoleApp), admin: d.Pool}
}

func TestE2ERunUnknownDepositT1AndResume(t *testing.T) {
	ds, cfg, scenarios := e2eFixture(t, 2)
	adjustScenario := scenarios[0].ScenarioID
	escalateScenario := scenarios[1].ScenarioID
	dir := writeManifest(t, ds)
	results := newE2EResults(t)
	pool, resultsDSN := results.app, results.dsn
	ctx := context.Background()

	opts := baseOptions(dir)
	opts.Suite = runner.SuiteE2E
	opts.E2E = true
	opts.Concurrency = 2
	deps := baseDeps(t, cfg, resultsDSN, e2eClient(cfg, adjustScenario))

	summary, err := runner.Run(ctx, opts, deps)
	if err != nil {
		t.Fatalf("e2e Run: %v", err)
	}
	if len(summary.Runs) != 1 || summary.Runs[0].Total != 2 || summary.Runs[0].Completed != 2 {
		t.Fatalf("summary = %+v, want 2 completed", summary.Runs)
	}
	runID := summary.Runs[0].RunID

	var suite, e2eFlag string
	var ended bool
	if err := pool.QueryRow(ctx,
		`SELECT suite, COALESCE(config->>'e2e', ''), ended_at IS NOT NULL
		   FROM eval_runs WHERE id = $1::uuid`, runID).
		Scan(&suite, &e2eFlag, &ended); err != nil {
		t.Fatalf("read e2e run: %v", err)
	}
	if suite != runner.SuiteE2E || e2eFlag != "true" || !ended {
		t.Fatalf("run metadata = suite %q e2e %q ended %t", suite, e2eFlag, ended)
	}

	cases := loadE2ECaseRows(t, pool, runID)
	if len(cases) != 2 {
		t.Fatalf("case rows = %d, want 2", len(cases))
	}
	var applied, abstained *e2eCaseRow
	for index := range cases {
		switch cases[index].ScenarioID {
		case adjustScenario:
			applied = &cases[index]
		case escalateScenario:
			abstained = &cases[index]
		}
	}
	if applied == nil || abstained == nil {
		t.Fatalf("missing expected case rows: %+v", cases)
	}

	result := applied.Result
	if result.Schema != e2e.SchemaVersion {
		t.Fatalf("schema = %q, want %q", result.Schema, e2e.SchemaVersion)
	}
	if result.Proposal != e2e.ProposalPresent || result.ProposalID == "" {
		t.Fatalf("proposal = %q/%q, want present with id", result.Proposal, result.ProposalID)
	}
	if result.RequestStatus != e2e.RequestStatusApplied || !result.Applied {
		t.Fatalf("request/applied = %q/%t, want applied/true", result.RequestStatus, result.Applied)
	}
	if result.PolicyTier == nil || *result.PolicyTier != "T1" {
		t.Fatalf("policy tier = %v, want T1", result.PolicyTier)
	}
	if result.ApproverAction != e2e.ActionApprove || result.ApproverRule != "exact_match" {
		t.Fatalf("approver = %q/%q, want approve/exact_match", result.ApproverAction, result.ApproverRule)
	}
	if result.LedgerTxnID == "" {
		t.Fatal("applied e2e result has no ledger txn id")
	}
	if result.Tieout == nil {
		t.Fatal("e2e result has no tie-out")
	}
	if result.Tieout.AsOf == "" || result.Tieout.Currency == "" {
		t.Fatalf("tie-out = %+v, want as-of and currency", result.Tieout)
	}
	if result.Failed || result.Error != "" {
		t.Fatalf("successful e2e result failed: %+v", result)
	}
	if applied.TierActual == nil || *applied.TierActual != "T1" {
		t.Fatalf("tier_actual = %v, want T1", applied.TierActual)
	}
	if result.Verifier == nil || !result.Verifier.Evaluated || result.Verifier.Passed {
		t.Fatalf("verifier = %+v, want evaluated and not passed", result.Verifier)
	}

	abstain := abstained.Result
	if abstain.Proposal != e2e.ProposalNone || abstain.RequestStatus != e2e.RequestStatusNone ||
		abstain.ApproverAction != e2e.ActionAbstain || abstain.Failed {
		t.Fatalf("no-proposal result = %+v", abstain)
	}
	if abstained.TierActual != nil {
		t.Fatalf("no-proposal tier_actual = %q, want NULL", *abstained.TierActual)
	}

	// Resume: delete the escalation row and resume without --e2e. The stored
	// e2e mode must be restored, the completed adjustment row skipped, and no
	// duplicate result row created.
	var preservedJSON string
	if err := pool.QueryRow(ctx,
		`SELECT detail->'e2e'::text FROM eval_case_results
		  WHERE eval_run_id = $1::uuid AND scenario_id = $2`,
		runID, adjustScenario).Scan(&preservedJSON); err != nil {
		t.Fatalf("read preserved e2e detail: %v", err)
	}
	if _, err := results.admin.Exec(ctx,
		`DELETE FROM eval_case_results WHERE eval_run_id = $1::uuid AND scenario_id = $2`,
		runID, escalateScenario); err != nil {
		t.Fatalf("delete escalation row: %v", err)
	}

	resumeOpts := baseOptions(dir)
	resumeOpts.Resume = runID
	resumeSummary, err := runner.Run(ctx, resumeOpts, deps)
	if err != nil {
		t.Fatalf("resume e2e run: %v", err)
	}
	if len(resumeSummary.Runs) != 1 || resumeSummary.Runs[0].Completed != 2 {
		t.Fatalf("resume summary = %+v, want 2 completed", resumeSummary.Runs)
	}
	resumedCases := loadE2ECaseRows(t, pool, runID)
	if len(resumedCases) != 2 {
		t.Fatalf("resumed case rows = %d, want 2", len(resumedCases))
	}
	var preservedAfter string
	if err := pool.QueryRow(ctx,
		`SELECT detail->'e2e'::text FROM eval_case_results
		  WHERE eval_run_id = $1::uuid AND scenario_id = $2`,
		runID, adjustScenario).Scan(&preservedAfter); err != nil {
		t.Fatalf("read preserved e2e detail after resume: %v", err)
	}
	if preservedAfter != preservedJSON {
		t.Fatalf("resume rewrote a completed e2e detail:\nbefore=%s\nafter=%s",
			preservedJSON, preservedAfter)
	}
	if err := pool.QueryRow(ctx,
		`SELECT suite, COALESCE(config->>'e2e', '')
		   FROM eval_runs WHERE id = $1::uuid`, runID).Scan(&suite, &e2eFlag); err != nil {
		t.Fatalf("read resumed run metadata: %v", err)
	}
	if suite != runner.SuiteE2E || e2eFlag != "true" {
		t.Fatalf("resumed metadata = suite %q e2e %q", suite, e2eFlag)
	}
	for _, row := range resumedCases {
		if row.Result.Schema != e2e.SchemaVersion {
			t.Fatalf("resumed scenario %s schema = %q", row.ScenarioID, row.Result.Schema)
		}
	}
}

func TestE2ERunFailClosedPersistsAndResumeStaysFailed(t *testing.T) {
	ds, cfg, scenarios := e2eFixture(t, 1)
	dir := writeManifest(t, ds)
	results := newE2EResults(t)
	pool, resultsDSN := results.app, results.dsn
	ctx := context.Background()

	opts := baseOptions(dir)
	opts.Suite = runner.SuiteE2E
	opts.E2E = true
	deps := baseDeps(t, cfg, resultsDSN, e2eClient(cfg, scenarios[0].ScenarioID))
	// A module root without policy.yaml makes harness construction fail after
	// the agent run and before RunCase, which must persist a failed result.
	deps.ModuleRoot = t.TempDir()

	summary, err := runner.Run(ctx, opts, deps)
	if err == nil || !errors.Is(err, e2e.ErrE2EFailed) {
		t.Fatalf("fail-closed Run error = %v, want ErrE2EFailed", err)
	}
	if len(summary.Runs) != 1 {
		t.Fatalf("summary runs = %d, want 1", len(summary.Runs))
	}
	runID := summary.Runs[0].RunID

	var aborted bool
	var abortReason, runError string
	if err := pool.QueryRow(ctx,
		`SELECT (config->>'aborted')::boolean, COALESCE(config->>'abort_reason', ''),
		        COALESCE(config->>'error', '')
		   FROM eval_runs WHERE id = $1::uuid`, runID).
		Scan(&aborted, &abortReason, &runError); err != nil {
		t.Fatalf("read failed run: %v", err)
	}
	if !aborted || abortReason != "error" || runError == "" {
		t.Fatalf("failed run = aborted %t reason %q error %q", aborted, abortReason, runError)
	}

	cases := loadE2ECaseRows(t, pool, runID)
	if len(cases) != 1 {
		t.Fatalf("failed case rows = %d, want 1", len(cases))
	}
	failed := cases[0].Result
	if !failed.Failed || failed.Error == "" {
		t.Fatalf("persisted e2e result = %+v, want failed with error", failed)
	}
	if !strings.Contains(failed.Error, "policy") {
		t.Fatalf("persisted e2e error = %q, want policy load failure", failed.Error)
	}

	// Resuming the same run must stay fail-closed: the persisted failed case is
	// already complete, so the runner must not finalize it as a success.
	resumeOpts := baseOptions(dir)
	resumeOpts.Resume = runID
	_, err = runner.Run(ctx, resumeOpts, deps)
	if err == nil || !errors.Is(err, e2e.ErrE2EFailed) {
		t.Fatalf("resume failed-run error = %v, want ErrE2EFailed", err)
	}
}
