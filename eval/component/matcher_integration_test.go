//go:build integration

package component

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/matchscore"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

func TestMain(m *testing.M) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "component: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(context.Background(),
		testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "component: ensure test template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func integrationOptions(t *testing.T, dir string, cfg dataset.Config) Options {
	t.Helper()
	return Options{
		Split:      dataset.Dev,
		DatasetDir: dir,
		AdminDSN:   os.Getenv("TEST_DATABASE_URL"),
		Clock:      clock.NewFake(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		BuildConfig: func(dataset.Manifest) (dataset.Config, error) {
			return cfg, nil
		},
	}
}

func TestMatcherBuildMiniDatasetDeterministic(t *testing.T) {
	ds, cfg := miniatureDataset(t)
	dir := writeMiniatureManifest(t, ds)
	opts := integrationOptions(t, dir, cfg)

	first, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	second, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("matcher reports differ across clones:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if first.Schema != MatcherSchema || first.Split != dataset.Dev {
		t.Fatalf("report metadata = %+v", first)
	}
	if first.DatasetVersion != ds.Manifest.EvalDatasetVersion {
		t.Fatalf("dataset version = %q", first.DatasetVersion)
	}
	if first.Scenarios != len(ds.Truth.Scenarios) {
		t.Fatalf("scenarios = %d, want %d", first.Scenarios, len(ds.Truth.Scenarios))
	}
	if first.ScoreableScenarios != ScoreableScenarios(ds.Truth) {
		t.Fatalf("scoreable scenarios = %d, want %d",
			first.ScoreableScenarios, ScoreableScenarios(ds.Truth))
	}
	if first.ExpectedLinks == 0 {
		t.Fatal("mini dataset produced no required links")
	}
	if first.Recall.Denominator != first.ExpectedLinks {
		t.Fatalf("recall denominator = %d, expected links = %d",
			first.Recall.Denominator, first.ExpectedLinks)
	}
	for _, pass := range first.PerPass {
		if pass.Recall.Denominator != first.ExpectedLinks {
			t.Fatalf("pass %s recall denominator = %d, want %d",
				pass.Pass, pass.Recall.Denominator, first.ExpectedLinks)
		}
	}
}

func countEvalRows(t *testing.T, box *sandbox.Sandbox) (int, int) {
	t.Helper()
	var runs, cases int
	if err := box.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM eval_runs`).Scan(&runs); err != nil {
		t.Fatalf("count eval_runs: %v", err)
	}
	if err := box.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM eval_case_results`).Scan(&cases); err != nil {
		t.Fatalf("count eval_case_results: %v", err)
	}
	return runs, cases
}

func TestMatcherReadsAppAndWritesNoEvalRows(t *testing.T) {
	ds, cfg := miniatureDataset(t)
	dir := writeMiniatureManifest(t, ds)
	resolved, err := loadDataset(Options{
		Split:      dataset.Dev,
		DatasetDir: dir,
		BuildConfig: func(dataset.Manifest) (dataset.Config, error) {
			return cfg, nil
		},
	})
	if err != nil {
		t.Fatalf("loadDataset: %v", err)
	}
	manager, err := sandbox.NewManager(sandbox.Options{
		AdminDSN:  os.Getenv("TEST_DATABASE_URL"),
		BaseClock: cfg.GeneratedAt,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	box, err := manager.Clone(context.Background(), resolved.Dataset,
		sandbox.LoadOptions{Config: resolved.Config})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(func() { _ = box.Close(context.Background()) })

	runsBefore, casesBefore := countEvalRows(t, box)
	report, err := reportFromSandbox(context.Background(), box, resolved, Options{
		Clock: clock.NewFake(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("reportFromSandbox: %v", err)
	}
	runsAfter, casesAfter := countEvalRows(t, box)
	if runsBefore != runsAfter || casesBefore != casesAfter {
		t.Fatalf("eval row counts changed: runs %d→%d cases %d→%d",
			runsBefore, runsAfter, casesBefore, casesAfter)
	}
	groups, err := matchscore.LoadActiveMatches(context.Background(), box.App, "")
	if err != nil {
		t.Fatalf("LoadActiveMatches: %v", err)
	}
	if report.Matches != len(groups) {
		t.Fatalf("report matches = %d, loaded groups = %d", report.Matches, len(groups))
	}
}
