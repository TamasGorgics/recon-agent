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
