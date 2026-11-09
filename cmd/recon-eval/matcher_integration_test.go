//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/component"
	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

func TestMain(m *testing.M) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "cmd/recon-eval: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(context.Background(),
		testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "cmd/recon-eval: ensure test template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func cliMiniatureDataset(t *testing.T) (*dataset.Dataset, dataset.Config) {
	t.Helper()
	cfg, err := dataset.DefaultConfig(dataset.Dev)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Seed = 7307
	cfg.Days = 7
	cfg.Difficulty = synth.DifficultyEasy
	cfg.Synth.ChargesPerDay = 1
	counts := make(map[string]int, len(modes.AllModes))
	total := 0
	for _, mode := range modes.AllModes {
		counts[mode] = 1
		total++
	}
	cfg.Modes = modes.Config{Total: total, Counts: counts}
	ds, err := dataset.Build(cfg)
	if err != nil {
		t.Fatalf("Build miniature dataset: %v", err)
	}
	return ds, cfg
}

func TestMatcherCommandMiniDatasetEndToEnd(t *testing.T) {
	ds, cfg := cliMiniatureDataset(t)
	dir := t.TempDir()
	if err := ds.WriteManifest(dir); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	outDir := t.TempDir()

	results := testdb.New(t)
	countRows := func() (int, int) {
		t.Helper()
		var runs, cases int
		if err := results.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM eval_runs`).Scan(&runs); err != nil {
			t.Fatalf("count eval_runs: %v", err)
		}
		if err := results.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM eval_case_results`).Scan(&cases); err != nil {
			t.Fatalf("count eval_case_results: %v", err)
		}
		return runs, cases
	}
	runsBefore, casesBefore := countRows()

	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	deps := matcherDeps{
		Clock: clock.NewFake(now),
		Build: func(ctx context.Context, opts component.Options) (component.MatcherReport, error) {
			opts.BuildConfig = func(dataset.Manifest) (dataset.Config, error) {
				return cfg, nil
			}
			opts.AdminDSN = os.Getenv("TEST_DATABASE_URL")
			opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return component.Build(ctx, opts)
		},
	}
	code, stdout, stderr := runMatcherCLI(t, deps, "--split", "dev",
		"--dataset-dir", dir, "--out-dir", outDir)
	if code != 0 {
		t.Fatalf("matcher code = %d stderr=%q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("matcher --out-dir stdout = %q", stdout)
	}
	for _, name := range []string{"matcher.md", "matcher.json"} {
		if info, err := os.Stat(filepath.Join(outDir, name)); err != nil || info.Size() == 0 {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(outDir, "matcher.json"))
	if err != nil {
		t.Fatalf("read matcher.json: %v", err)
	}
	var document component.MatcherReport
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode matcher.json: %v", err)
	}
	if document.Schema != component.MatcherSchema || document.Split != dataset.Dev {
		t.Fatalf("report = %+v", document)
	}
	if document.DatasetVersion != ds.Manifest.EvalDatasetVersion {
		t.Fatalf("dataset version = %q", document.DatasetVersion)
	}
	if document.Scenarios != len(ds.Truth.Scenarios) {
		t.Fatalf("scenarios = %d, want %d", document.Scenarios, len(ds.Truth.Scenarios))
	}
	if document.ExpectedLinks == 0 {
		t.Fatal("mini dataset produced no required links")
	}

	runsAfter, casesAfter := countRows()
	if runsBefore != runsAfter || casesBefore != casesAfter {
		t.Fatalf("eval row counts changed: runs %d→%d cases %d→%d",
			runsBefore, runsAfter, casesBefore, casesAfter)
	}
}
