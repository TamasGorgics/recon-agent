//go:build integration

package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

func TestMain(m *testing.M) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "report: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(context.Background(),
		testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "report: ensure test template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestPostgresReportEndToEnd(t *testing.T) {
	ctx := context.Background()
	truth, manifest := devDataset(t)
	scenarios := map[string]dataset.ScenarioTruth{
		"duplicate_charge": scenarioByMode(t, truth, "duplicate_charge"),
		"fee_variance":     scenarioByMode(t, truth, "fee_variance"),
		"partial_refund":   scenarioByMode(t, truth, "partial_refund"),
	}

	database := testdb.New(t)
	app, err := db.OpenRole(ctx, db.RoleApp, database.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open app role: %v", err)
	}
	t.Cleanup(app.Close)
	pool := app.Pool

	started := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ended := started.Add(5 * time.Minute)
	var runID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO eval_runs
		     (suite, split, dataset_version, model, prompt_version, seed,
		      git_sha, config, started_at, ended_at)
		 VALUES ('agent', 'dev', $1, 'test-model', 'v1', $2, NULL, '{}'::jsonb, $3, $4)
		 RETURNING id::text`,
		manifest.EvalDatasetVersion, manifest.Seed, started, ended).Scan(&runID); err != nil {
		t.Fatalf("insert eval run: %v", err)
	}

	transcript := map[string]any{
		"version":   1,
		"outcome":   "proposal",
		"proposal":  map[string]any{"id": "proposal-1", "type": "link"},
		"assistant": "Checked the duplicate charge and proposed the original link.",
		"tool_steps": []any{
			map[string]any{"seq": 1, "tool": "get_case", "refs": []any{"case:1"},
				"error_code": "", "truncated": false},
		},
		"truncated": false,
	}
	insertCase := func(scenario dataset.ScenarioTruth, correct, falseFix, escalated bool,
		actual string, confidence *string, detail map[string]any) {
		t.Helper()
		detail["actual_resolution"] = actual
		detail["expected_resolutions"] = scenario.ExpectedResolutions
		detail["allows_escalate"] = containsString(scenario.ExpectedResolutions, "escalate")
		detail["resolvable_truth"] = resolvableTruth(scenario.ExpectedResolutions)
		detail["has_proposal"] = actual != "escalate"
		detail["outcome"] = "proposal"
		if actual == "escalate" {
			detail["outcome"] = "escalated"
		}
		detail["agent_run_id"] = "agent-run-" + scenario.Mode
		if has, _ := detail["has_proposal"].(bool); has {
			detail["proposal_id"] = "proposal-" + scenario.Mode
			detail["evidence_total"] = 1
			detail["evidence_real"] = 1
			detail["evidence_relevant"] = 1
			detail["evidence_valid"] = true
		}
		encoded, err := json.Marshal(detail)
		if err != nil {
			t.Fatalf("marshal detail: %v", err)
		}
		var confidenceValue any
		if confidence != nil {
			confidenceValue = *confidence
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO eval_case_results
			     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated,
			      confidence, cost_usd, latency_ms, steps, detail)
			 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9, $10, $11::jsonb)`,
			runID, scenario.ScenarioID, scenario.Mode, correct, falseFix, escalated,
			confidenceValue, "0.010000", 120, 2, encoded); err != nil {
			t.Fatalf("insert case result: %v", err)
		}
	}

	duplicate := scenarios["duplicate_charge"]
	confidence := "0.8000"
	insertCase(duplicate, false, true, false, "adjustment", &confidence,
		map[string]any{"transcript": transcript})
	fee := scenarios["fee_variance"]
	insertCase(fee, false, false, false, "write_off", nil, map[string]any{})
	refund := scenarios["partial_refund"]
	insertCase(refund, false, false, true, "escalate", nil, map[string]any{})

	before := countCaseRows(t, pool)
	built, err := Build(ctx, NewPostgresSource(pool), runID, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	after := countCaseRows(t, pool)
	if before != after {
		t.Fatalf("report wrote case rows: %d -> %d", before, after)
	}
	if built.Schema != Schema || built.Run.ID != runID {
		t.Fatalf("report = %+v", built.Run)
	}
	if built.Headline.Cases != 3 {
		t.Fatalf("cases = %d, want 3", built.Headline.Cases)
	}
	if built.Dataset.EvalDatasetVersion != manifest.EvalDatasetVersion {
		t.Fatalf("dataset version = %q", built.Dataset.EvalDatasetVersion)
	}
	if len(built.FailureAppendix) != 3 {
		t.Fatalf("appendix = %+v", built.FailureAppendix)
	}
	if built.FailureAppendix[0].ScenarioID != duplicate.ScenarioID ||
		built.FailureAppendix[0].TranscriptStatus != transcriptStatusAvailable {
		t.Fatalf("first appendix = %+v", built.FailureAppendix[0])
	}
	if built.FailureAppendix[1].ScenarioID != fee.ScenarioID ||
		built.FailureAppendix[1].TranscriptStatus != transcriptStatusUnavailable {
		t.Fatalf("second appendix = %+v", built.FailureAppendix[1])
	}
	if built.FailureAppendix[2].ScenarioID != refund.ScenarioID {
		t.Fatalf("third appendix = %+v", built.FailureAppendix[2])
	}
	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !strings.Contains(string(markdown), "## Headline") {
		t.Fatalf("markdown = %s", markdown)
	}
	jsonBytes, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	for _, forbidden := range []string{`"args"`, `"result"`, `"memo"`, `"api_key"`} {
		if strings.Contains(string(jsonBytes), forbidden) {
			t.Fatalf("JSON contains forbidden key %s", forbidden)
		}
	}

	if _, err := Build(ctx, NewPostgresSource(pool),
		"99999999-9999-4999-8999-999999999999", Options{
			DatasetDir: devDatasetDir(t),
			Clock:      fixtureClock(t),
		}); err == nil {
		t.Fatal("missing run did not fail")
	}
}

func countCaseRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM eval_case_results`).Scan(&count); err != nil {
		t.Fatalf("count case rows: %v", err)
	}
	return count
}

func TestPostgresE2EReportEndToEnd(t *testing.T) {
	ctx := context.Background()
	truth, manifest := devDataset(t)
	scenarios := map[string]dataset.ScenarioTruth{
		"duplicate_charge": scenarioByMode(t, truth, "duplicate_charge"),
		"fee_variance":     scenarioByMode(t, truth, "fee_variance"),
		"partial_refund":   scenarioByMode(t, truth, "partial_refund"),
	}

	database := testdb.New(t)
	app, err := db.OpenRole(ctx, db.RoleApp, database.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open app role: %v", err)
	}
	t.Cleanup(app.Close)
	pool := app.Pool

	started := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ended := started.Add(5 * time.Minute)
	var runID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO eval_runs
		     (suite, split, dataset_version, model, prompt_version, seed,
		      git_sha, config, started_at, ended_at)
		 VALUES ('e2e', 'dev', $1, 'test-model', 'v1', $2, NULL,
		         '{"e2e":true}'::jsonb, $3, $4)
		 RETURNING id::text`,
		manifest.EvalDatasetVersion, manifest.Seed, started, ended).Scan(&runID); err != nil {
		t.Fatalf("insert e2e run: %v", err)
	}

	t0, t1 := "T0", "T1"
	insertCase := func(scenario dataset.ScenarioTruth, correct, falseFix bool,
		tierActual *string, detail map[string]any) {
		t.Helper()
		e2e := map[string]any{
			"schema":             "eval-e2e/v1",
			"proposal":           "present",
			"proposal_id":        "00000000-0000-4000-8000-000000000001",
			"request_status":     "applied",
			"policy_tier":        tierActual,
			"agent_claimed_tier": t1,
			"approver_action":    "approve",
			"applied":            true,
			"failed":             false,
			"error":              "",
		}
		if tierActual != nil && *tierActual == "T0" {
			e2e["approver_action"] = "skip_t0"
		}
		for key, value := range detail {
			e2e[key] = value
		}
		detail["e2e"] = e2e
		detail["actual_resolution"] = "adjustment"
		detail["expected_resolutions"] = scenario.ExpectedResolutions
		detail["allows_escalate"] = containsString(scenario.ExpectedResolutions, "escalate")
		detail["resolvable_truth"] = resolvableTruth(scenario.ExpectedResolutions)
		detail["has_proposal"] = true
		detail["outcome"] = "proposal"
		detail["agent_run_id"] = "agent-run-" + scenario.Mode
		detail["evidence_total"] = 1
		detail["evidence_real"] = 1
		detail["evidence_relevant"] = 1
		detail["evidence_valid"] = true
		encoded, err := json.Marshal(detail)
		if err != nil {
			t.Fatalf("marshal detail: %v", err)
		}
		tierExpected := scenario.ExpectedTier
		if _, err := pool.Exec(ctx,
			`INSERT INTO eval_case_results
			     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated,
			      tier_expected, tier_actual, confidence, cost_usd, latency_ms, steps, detail)
			 VALUES ($1::uuid, $2, $3, $4, $5, false, $6, $7, NULL,
			         '0.010000'::numeric, 120, 2, $8::jsonb)`,
			runID, scenario.ScenarioID, scenario.Mode, correct, falseFix,
			tierExpected, tierActual, encoded); err != nil {
			t.Fatalf("insert e2e case result: %v", err)
		}
	}

	insertCase(scenarios["fee_variance"], true, false, &t0, map[string]any{
		"verifier": map[string]any{
			"evaluated": true, "passed": true, "pattern": "fee_variance",
		},
	})
	insertCase(scenarios["duplicate_charge"], false, true, &t0, map[string]any{
		"verifier": map[string]any{
			"evaluated": true, "passed": true, "pattern": "other",
		},
	})
	insertCase(scenarios["partial_refund"], true, false, &t1, map[string]any{
		"verifier": map[string]any{
			"evaluated": true, "passed": false, "pattern": "",
		},
	})

	before := countCaseRows(t, pool)
	built, err := Build(ctx, NewPostgresSource(pool), runID, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	after := countCaseRows(t, pool)
	if before != after {
		t.Fatalf("e2e report wrote case rows: %d -> %d", before, after)
	}
	if err != nil {
		t.Fatalf("Build e2e report: %v", err)
	}
	if built.E2E == nil {
		t.Fatal("e2e block is nil")
	}
	if built.E2E.Cases != 3 || built.E2E.T0Applied != 2 ||
		built.E2E.Verifier.Evaluated != 3 || built.E2E.Verifier.Passed != 2 {
		t.Fatalf("e2e block = %+v", built.E2E)
	}
	if len(built.Scorecards) != 1 || !built.Scorecards[0].Applicable {
		t.Fatalf("verifier scorecard = %+v", built.Scorecards)
	}
	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !strings.Contains(string(markdown), "## E2E pipeline") {
		t.Fatalf("markdown missing e2e section:\n%s", markdown)
	}
	jsonBytes, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(string(jsonBytes), `"e2e": {`) {
		t.Fatalf("JSON missing e2e block:\n%s", jsonBytes)
	}

	// An unknown detail schema on an e2e run must fail closed.
	if _, err := database.Pool.Exec(ctx,
		`UPDATE eval_case_results
		    SET detail = jsonb_set(detail, '{e2e,schema}', '"eval-e2e/v2"')
		  WHERE eval_run_id = $1::uuid`, runID); err != nil {
		t.Fatalf("corrupt e2e schema: %v", err)
	}
	if _, err := Build(ctx, NewPostgresSource(pool), runID, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	}); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("unknown e2e schema error = %v", err)
	}
}

func TestPostgresCompareEndToEnd(t *testing.T) {
	ctx := context.Background()
	truth, manifest := devDataset(t)
	feeScenarios := make([]dataset.ScenarioTruth, 0, 12)
	for _, scenario := range truth.Scenarios {
		if scenario.Mode == "fee_variance" && scenario.BreakSignature != "" {
			feeScenarios = append(feeScenarios, scenario)
		}
	}
	if len(feeScenarios) != 12 {
		t.Fatalf("fee_variance scoreable scenarios = %d, want 12", len(feeScenarios))
	}

	database := testdb.New(t)
	app, err := db.OpenRole(ctx, db.RoleApp, database.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open app role: %v", err)
	}
	t.Cleanup(app.Close)
	pool := app.Pool

	started := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ended := started.Add(5 * time.Minute)
	insertRun := func(model, prompt string) string {
		t.Helper()
		var runID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO eval_runs
			     (suite, split, dataset_version, model, prompt_version, seed,
			      git_sha, config, started_at, ended_at)
			 VALUES ('agent', 'dev', $1, $2, $3, $4, NULL, '{}'::jsonb, $5, $6)
			 RETURNING id::text`,
			manifest.EvalDatasetVersion, model, prompt, manifest.Seed,
			started, ended).Scan(&runID); err != nil {
			t.Fatalf("insert eval run: %v", err)
		}
		return runID
	}
	insertCase := func(runID string, scenario dataset.ScenarioTruth, correct, falseFix bool) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO eval_case_results
			     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated,
			      confidence, cost_usd, latency_ms, steps, detail)
			 VALUES ($1::uuid, $2, $3, $4, $5, false, NULL, '0.010000'::numeric,
			         120, 2, '{}'::jsonb)`,
			runID, scenario.ScenarioID, scenario.Mode, correct, falseFix); err != nil {
			t.Fatalf("insert case result: %v", err)
		}
	}

	runA := insertRun("model-a", "v1")
	runB := insertRun("model-b", "v2")
	for index, scenario := range feeScenarios {
		insertCase(runA, scenario, true, false)
		insertCase(runB, scenario, index < 6, index >= 6)
	}

	before := countCaseRows(t, pool)
	comparison, err := Compare(ctx, NewPostgresSource(pool), runA, runB, CompareOptions{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	after := countCaseRows(t, pool)
	if before != after {
		t.Fatalf("compare wrote case rows: %d -> %d", before, after)
	}
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comparison.Schema != CompareSchema || comparison.A.RunID != runA ||
		comparison.B.RunID != runB {
		t.Fatalf("comparison metadata = %+v", comparison)
	}
	if comparison.Overall.Cases.A != 12 || comparison.Overall.Cases.B != 12 {
		t.Fatalf("cases = %+v", comparison.Overall.Cases)
	}
	if comparison.Overall.FalseFix.Classification != ClassificationRegression ||
		!comparison.ExitCriteria.Failed {
		t.Fatalf("comparison = %+v", comparison.Overall.FalseFix)
	}
	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	if !strings.Contains(string(markdown), "Gating regression detected") {
		t.Fatalf("markdown = %s", markdown)
	}
	if _, err := RenderCompareJSON(comparison); err != nil {
		t.Fatalf("RenderCompareJSON: %v", err)
	}
}
