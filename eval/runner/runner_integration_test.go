//go:build integration

package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/runner"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

var (
	miniOnce sync.Once
	miniDS   *dataset.Dataset
	miniCfg  dataset.Config
	miniErr  error
)

// miniAcceptanceDataset is the deterministic 20-application world used by the
// M3-04 acceptance tests: five modes x four applications, each emitting one
// scoreable scenario.
func miniAcceptanceDataset(t *testing.T) (*dataset.Dataset, dataset.Config) {
	t.Helper()
	miniOnce.Do(func() {
		cfg, err := dataset.DefaultConfig(dataset.Dev)
		if err != nil {
			miniErr = err
			return
		}
		cfg.Days = 7
		cfg.Modes = modes.Config{
			Total: 20,
			Counts: map[string]int{
				modes.DuplicateCharge: 4,
				modes.FeeVariance:     4,
				modes.FXRounding:      4,
				modes.PartialRefund:   4,
				modes.ReferenceTypo:   4,
			},
		}
		miniDS, miniErr = dataset.Build(cfg)
		miniCfg = cfg
	})
	if miniErr != nil {
		t.Fatalf("build acceptance dataset: %v", miniErr)
	}
	scoreable := 0
	for _, scenario := range miniDS.Truth.Scenarios {
		if scenario.BreakSignature != "" {
			scoreable++
		}
	}
	if scoreable != 20 {
		t.Fatalf("acceptance dataset scoreable scenarios = %d, want 20", scoreable)
	}
	return miniDS, miniCfg
}

func writeManifest(t *testing.T, ds *dataset.Dataset) string {
	t.Helper()
	dir := t.TempDir()
	if err := ds.WriteManifest(dir); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func newResultsPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	d := testdb.New(t)
	rolePool, err := db.OpenRole(context.Background(), db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open app role pool: %v", err)
	}
	t.Cleanup(rolePool.Close)
	return rolePool.Pool, d.DSNFor(db.RoleApp)
}

func baseOptions(dir string) runner.Options {
	return runner.Options{
		Split:       dataset.Dev,
		Repeat:      1,
		Concurrency: 4,
		LLM:         runner.LLMCassette,
		DatasetDir:  dir,
	}
}

func baseDeps(t *testing.T, cfg dataset.Config, resultsDSN string,
	client func(sandbox.CaseRef, dataset.ScenarioTruth) (llm.Client, error)) runner.Deps {
	t.Helper()
	root, err := runner.FindModuleRoot()
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	return runner.Deps{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		ResultsDSN: resultsDSN,
		AdminDSN:   os.Getenv("TEST_DATABASE_URL"),
		ModuleRoot: root,
		BuildConfig: func(dataset.Manifest) (dataset.Config, error) {
			return cfg, nil
		},
		ClientForScenario: client,
	}
}

// escalationClient returns one valid escalate terminal call per case.
func escalationClient() func(sandbox.CaseRef, dataset.ScenarioTruth) (llm.Client, error) {
	return func(ref sandbox.CaseRef, _ dataset.ScenarioTruth) (llm.Client, error) {
		args := fmt.Sprintf(`{"case_id":%q,"reason":"eval_scripted","summary":"scripted escalation"}`, ref.CaseID)
		return llm.NewScripted(llm.ScriptedStep{Response: toolCallResponse("escalate", args)}), nil
	}
}

func toolCallResponse(name, args string) llm.Response {
	return llm.Response{
		Provider:    llm.ProviderOpenCodeGo,
		ActualModel: "test-model-2026",
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID:        "call_" + name,
			Name:      name,
			Arguments: json.RawMessage(args),
		}}},
		StopReason: llm.StopToolCalls,
		Usage: llm.Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
			CostUSD:          decimal.RequireFromString("0.001"),
		},
	}
}

func countCases(t *testing.T, pool *pgxpool.Pool, runID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM eval_case_results WHERE eval_run_id = $1::uuid`, runID).
		Scan(&count); err != nil {
		t.Fatalf("count case results: %v", err)
	}
	return count
}

func assertNoPackageSandboxes(t *testing.T) {
	t.Helper()
	names, err := listNames(context.Background(), os.Getenv("TEST_DATABASE_URL"),
		`SELECT datname FROM pg_database WHERE starts_with(datname, $1) ORDER BY datname`,
		packageDatabasePrefix())
	if err != nil {
		t.Fatalf("list eval databases: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("leftover eval databases: %v", names)
	}
}

// TestEvalRun20ScenariosScripted is the M3-04 acceptance run: 20 scoreable
// scenarios, one shared sandbox, concurrency 4, one durable result per case.
func TestEvalRun20ScenariosScripted(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)

	summary, err := runner.Run(context.Background(), baseOptions(dir),
		baseDeps(t, cfg, resultsDSN, escalationClient()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(summary.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(summary.Runs))
	}
	run := summary.Runs[0]
	if run.Total != 20 || run.Completed != 20 || run.Skipped != 0 || run.Aborted {
		t.Fatalf("summary = %+v, want 20 completed, not aborted", run)
	}

	var (
		suite, split, model, promptVersion, datasetVersion string
		pricingSchema, llmMode, maxCost                    string
		seed                                               int64
		endedSet                                           bool
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT suite, split, model, prompt_version, dataset_version,
		        config->'pricing'->>'schema', config->'eval'->>'llm',
		        config->'eval'->>'max_cost_usd', seed, ended_at IS NOT NULL
		   FROM eval_runs WHERE id = $1::uuid`, run.RunID).
		Scan(&suite, &split, &model, &promptVersion, &datasetVersion,
			&pricingSchema, &llmMode, &maxCost, &seed, &endedSet); err != nil {
		t.Fatalf("read eval run: %v", err)
	}
	if suite != "agent" || split != "dev" || model == "" || promptVersion != "v1" {
		t.Fatalf("run metadata = %s/%s/%s/%s", suite, split, model, promptVersion)
	}
	if datasetVersion != ds.Manifest.EvalDatasetVersion {
		t.Fatalf("dataset version = %q, want %q", datasetVersion, ds.Manifest.EvalDatasetVersion)
	}
	if seed != ds.Manifest.Seed {
		t.Fatalf("seed = %d, want %d", seed, ds.Manifest.Seed)
	}
	if pricingSchema != "llm-pricing/v1" || llmMode != "cassette" || maxCost != "0" {
		t.Fatalf("config = pricing %q llm %q max_cost %q", pricingSchema, llmMode, maxCost)
	}
	if !endedSet {
		t.Fatal("ended_at is NULL")
	}

	var (
		cases, distinctScenarios int
		costText                 string
		nonNegative              bool
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), count(DISTINCT scenario_id),
		        COALESCE(sum(cost_usd), 0)::text,
		        COALESCE(bool_and(cost_usd >= 0 AND latency_ms >= 0 AND steps > 0), false)
		   FROM eval_case_results WHERE eval_run_id = $1::uuid`, run.RunID).
		Scan(&cases, &distinctScenarios, &costText, &nonNegative); err != nil {
		t.Fatalf("read case results: %v", err)
	}
	if cases != 20 || distinctScenarios != 20 {
		t.Fatalf("case results = %d (distinct %d), want 20", cases, distinctScenarios)
	}
	if !nonNegative {
		t.Fatal("a case has negative cost/latency or zero steps")
	}
	if costText != "0.020000" {
		t.Fatalf("total cost = %q, want 0.020000", costText)
	}

	var (
		queryCases int
		queryCost  string
		meanLat    string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT count(c.id), COALESCE(sum(c.cost_usd), 0)::text,
		        COALESCE(avg(c.latency_ms), 0)::text
		   FROM eval_runs r
		   LEFT JOIN eval_case_results c ON c.eval_run_id = r.id
		  WHERE r.id = $1::uuid
		  GROUP BY r.id`, run.RunID).
		Scan(&queryCases, &queryCost, &meanLat); err != nil {
		t.Fatalf("SQL queryability: %v", err)
	}
	if queryCases != 20 || queryCost != "0.020000" {
		t.Fatalf("queryability = %d cases, cost %q", queryCases, queryCost)
	}
	if _, err := decimal.NewFromString(meanLat); err != nil {
		t.Fatalf("mean latency %q is not decimal", meanLat)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT failure_mode, count(*) FROM eval_case_results
		  WHERE eval_run_id = $1::uuid GROUP BY failure_mode`, run.RunID)
	if err != nil {
		t.Fatalf("per-mode breakdown: %v", err)
	}
	modesSeen := 0
	for rows.Next() {
		var mode string
		var count int
		if err := rows.Scan(&mode, &count); err != nil {
			rows.Close()
			t.Fatalf("scan mode: %v", err)
		}
		if mode == "" || count <= 0 {
			rows.Close()
			t.Fatalf("bad mode row %q=%d", mode, count)
		}
		modesSeen++
	}
	rows.Close()
	if modesSeen == 0 {
		t.Fatal("no failure-mode breakdown rows")
	}

	// Results survive sandbox close: the durable DB is a different database
	// from the dropped clone, and no clone from this process remains.
	assertNoPackageSandboxes(t)
}

func TestEvalRunCostCapAndResume(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)
	deps := baseDeps(t, cfg, resultsDSN, escalationClient())
	ctx := context.Background()

	firstOpts := baseOptions(dir)
	firstOpts.Concurrency = 1
	firstOpts.MaxCostUSD = decimal.RequireFromString("0.005")
	summary, err := runner.Run(ctx, firstOpts, deps)
	if err != nil {
		t.Fatalf("cost-cap Run: %v", err)
	}
	if len(summary.Runs) != 1 || !summary.Runs[0].Aborted {
		t.Fatalf("cost-cap summary = %+v, want aborted", summary.Runs)
	}
	runID := summary.Runs[0].RunID
	completedBeforeCap := countCases(t, pool, runID)
	// The scheduler may have accepted one in-flight case before the persisted
	// sum crossed the cap; the locked overshoot bound is at most concurrency.
	if completedBeforeCap < 5 || completedBeforeCap > 6 {
		t.Fatalf("completed before cap = %d, want 5..6 (cap 0.005, concurrency 1)", completedBeforeCap)
	}

	var (
		endedSet    bool
		aborted     bool
		abortReason *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT ended_at IS NOT NULL, (config->>'aborted')::boolean, config->>'abort_reason'
		   FROM eval_runs WHERE id = $1::uuid`, runID).
		Scan(&endedSet, &aborted, &abortReason); err != nil {
		t.Fatalf("read aborted run: %v", err)
	}
	if !endedSet || !aborted || abortReason == nil || *abortReason != "cost_cap" {
		t.Fatalf("aborted config = ended %t aborted %t reason %v", endedSet, aborted, abortReason)
	}

	// Resume with a lower cap stops immediately at the already-spent sum.
	lowerOpts := baseOptions(dir)
	lowerOpts.Concurrency = 1
	lowerOpts.MaxCostUSD = decimal.RequireFromString("0.001")
	lowerOpts.Resume = runID
	lowerSummary, err := runner.Run(ctx, lowerOpts, deps)
	if err != nil {
		t.Fatalf("lower-cap resume: %v", err)
	}
	if len(lowerSummary.Runs) != 1 || !lowerSummary.Runs[0].Aborted {
		t.Fatalf("lower-cap summary = %+v, want aborted", lowerSummary.Runs)
	}
	if got := countCases(t, pool, runID); got != completedBeforeCap {
		t.Fatalf("cases after lower-cap resume = %d, want %d", got, completedBeforeCap)
	}
	var (
		lowerCostCap *string
		lowerEvalCap *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT config->>'cost_cap', config->'eval'->>'max_cost_usd'
		   FROM eval_runs WHERE id = $1::uuid`, runID).
		Scan(&lowerCostCap, &lowerEvalCap); err != nil {
		t.Fatalf("read lower-cap provenance: %v", err)
	}
	if lowerCostCap == nil || *lowerCostCap != "0.001" ||
		lowerEvalCap == nil || *lowerEvalCap != "0.001" {
		t.Fatalf("lower-cap provenance = %v/%v, want 0.001/0.001", lowerCostCap, lowerEvalCap)
	}

	// Resume with a higher cap completes the remaining scenarios.
	higherOpts := baseOptions(dir)
	higherOpts.Concurrency = 4
	higherOpts.MaxCostUSD = decimal.RequireFromString("1.0")
	higherOpts.Resume = runID
	higherSummary, err := runner.Run(ctx, higherOpts, deps)
	if err != nil {
		t.Fatalf("higher-cap resume: %v", err)
	}
	if len(higherSummary.Runs) != 1 || higherSummary.Runs[0].Completed != 20 ||
		higherSummary.Runs[0].Aborted {
		t.Fatalf("higher-cap summary = %+v, want 20 completed", higherSummary.Runs)
	}
	if got := countCases(t, pool, runID); got != 20 {
		t.Fatalf("cases after higher-cap resume = %d, want 20", got)
	}
	var (
		finalAborted bool
		finalReason  *string
		resumedAt    *string
		finalCostCap *string
		finalEvalCap *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT (config->>'aborted')::boolean, config->>'abort_reason', config->>'resumed_at',
		        config->>'cost_cap', config->'eval'->>'max_cost_usd'
		   FROM eval_runs WHERE id = $1::uuid`, runID).
		Scan(&finalAborted, &finalReason, &resumedAt, &finalCostCap, &finalEvalCap); err != nil {
		t.Fatalf("read resumed run: %v", err)
	}
	if finalAborted || finalReason != nil || resumedAt == nil {
		t.Fatalf("resumed config = aborted %t reason %v resumed_at %v", finalAborted, finalReason, resumedAt)
	}
	if finalCostCap == nil || *finalCostCap != "1" ||
		finalEvalCap == nil || *finalEvalCap != "1" {
		t.Fatalf("higher-cap provenance = %v/%v, want 1/1", finalCostCap, finalEvalCap)
	}

	// A fully completed run is a no-op: no new rows and no error.
	noopSummary, err := runner.Run(ctx, higherOpts, deps)
	if err != nil {
		t.Fatalf("completed resume: %v", err)
	}
	if len(noopSummary.Runs) != 1 || noopSummary.Runs[0].Note != "already complete" {
		t.Fatalf("no-op summary = %+v", noopSummary.Runs)
	}
	if got := countCases(t, pool, runID); got != 20 {
		t.Fatalf("cases after no-op resume = %d, want 20", got)
	}
	var runRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eval_runs`).Scan(&runRows); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runRows != 1 {
		t.Fatalf("eval_runs rows = %d, want 1", runRows)
	}
	assertNoPackageSandboxes(t)
}

func TestEvalRunRepeats(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)

	opts := baseOptions(dir)
	opts.Repeat = 2
	summary, err := runner.Run(context.Background(), opts,
		baseDeps(t, cfg, resultsDSN, escalationClient()))
	if err != nil {
		t.Fatalf("repeats Run: %v", err)
	}
	if len(summary.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(summary.Runs))
	}
	if summary.Runs[0].RunID == summary.Runs[1].RunID {
		t.Fatal("repeat runs share an id")
	}
	for _, run := range summary.Runs {
		if run.Completed != 20 || run.Aborted {
			t.Fatalf("repeat summary = %+v", run)
		}
	}

	var (
		groups, indices, runsLinked int
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT count(DISTINCT repeat_group), count(DISTINCT repeat_index), count(*)
		   FROM eval_runs`).Scan(&groups, &indices, &runsLinked); err != nil {
		t.Fatalf("read repeat group: %v", err)
	}
	if groups != 1 || indices != 2 || runsLinked != 2 {
		t.Fatalf("repeat metadata = groups %d indices %d runs %d", groups, indices, runsLinked)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT r.repeat_index, count(c.id)
		   FROM eval_runs r
		   LEFT JOIN eval_case_results c ON c.eval_run_id = r.id
		  GROUP BY r.repeat_index ORDER BY r.repeat_index`)
	if err != nil {
		t.Fatalf("repeat link query: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var index, cases int
		if err := rows.Scan(&index, &cases); err != nil {
			t.Fatalf("scan repeat link: %v", err)
		}
		if index != seen || cases != 20 {
			t.Fatalf("repeat %d has %d cases", index, cases)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("repeat link rows: %v", err)
	}
	if seen != 2 {
		t.Fatalf("repeat link rows = %d, want 2", seen)
	}
	assertNoPackageSandboxes(t)
}

func TestEvalRunManifestDrift(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)

	drifted := cfg
	drifted.Days++
	deps := baseDeps(t, cfg, resultsDSN, escalationClient())
	deps.BuildConfig = func(dataset.Manifest) (dataset.Config, error) { return drifted, nil }

	_, err := runner.Run(context.Background(), baseOptions(dir), deps)
	if err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("drift error = %v, want drift", err)
	}
	var runs int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eval_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("eval_runs rows = %d, want 0", runs)
	}
	assertNoPackageSandboxes(t)
}

func TestEvalRunCassetteMiss(t *testing.T) {
	cfg := smallAcceptanceConfig(t)
	ds, err := dataset.Build(cfg)
	if err != nil {
		t.Fatalf("build small dataset: %v", err)
	}
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)

	deps := baseDeps(t, cfg, resultsDSN, nil)
	deps.CassetteDir = t.TempDir()
	_, err = runner.Run(context.Background(), baseOptions(dir), deps)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cassette_miss") {
		t.Fatalf("cassette miss error = %v", err)
	}

	var (
		runRows     int
		endedSet    bool
		aborted     bool
		abortReason *string
		errorText   *string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(bool_and(ended_at IS NOT NULL), false),
		        COALESCE(bool_and((config->>'aborted')::boolean), false),
		        max(config->>'abort_reason'), max(config->>'error')
		   FROM eval_runs`).Scan(&runRows, &endedSet, &aborted, &abortReason, &errorText); err != nil {
		t.Fatalf("read cassette-miss run: %v", err)
	}
	if runRows != 1 || !endedSet || !aborted || abortReason == nil || *abortReason != "error" {
		t.Fatalf("cassette-miss run = rows %d ended %t aborted %t reason %v",
			runRows, endedSet, aborted, abortReason)
	}
	if errorText == nil || !strings.Contains(strings.ToLower(*errorText), "cassette_miss") {
		t.Fatalf("cassette-miss detail = %v", errorText)
	}
	var cases int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eval_case_results`).Scan(&cases); err != nil {
		t.Fatalf("count cases: %v", err)
	}
	if cases != 0 {
		t.Fatalf("cassette miss wrote %d case rows, want 0", cases)
	}
	assertNoPackageSandboxes(t)
}

// smallAcceptanceConfig is a two-scenario world for wiring tests.
func smallAcceptanceConfig(t *testing.T) dataset.Config {
	t.Helper()
	cfg, err := dataset.DefaultConfig(dataset.Dev)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Days = 3
	cfg.Modes = modes.Config{
		Total:  2,
		Counts: map[string]int{modes.FeeVariance: 1, modes.ReferenceTypo: 1},
	}
	return cfg
}
