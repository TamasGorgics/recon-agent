package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/report"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const (
	cliCompareRunA = "11111111-1111-4111-8111-111111111111"
	cliCompareRunB = "22222222-2222-4222-8222-222222222222"
)

func fixtureComparison(failed, reportOnly bool) report.Comparison {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	regressions := []report.CompareRegression{}
	if failed {
		a := 0.9
		b := 0.8
		delta := -10.0
		regressions = append(regressions, report.CompareRegression{
			Scope: "overall", Metric: "accuracy", A: &a, B: &b, DeltaPP: &delta,
		})
	}
	return report.Comparison{
		Schema:      report.CompareSchema,
		GeneratedAt: now,
		A: report.CompareRun{
			RunID: cliCompareRunA, Suite: "agent", Split: dataset.Dev,
			DatasetVersion: "dataset", Model: "model-a", PromptVersion: "v1", Cases: 12,
			Complete: true, MissingModes: []string{},
		},
		B: report.CompareRun{
			RunID: cliCompareRunB, Suite: "agent", Split: dataset.Dev,
			DatasetVersion: "dataset", Model: "model-b", PromptVersion: "v2", Cases: 12,
			Complete: true, MissingModes: []string{},
		},
		Overall: report.CompareBlock{Cases: report.CompareCounts{A: 12, B: 12}},
		ByMode:  map[string]report.CompareBlock{},
		ExitCriteria: report.CompareExitCriteria{
			Failed:         failed,
			ReportOnly:     reportOnly,
			IncompleteRuns: []string{},
			Regressions:    regressions,
		},
		Warnings: []string{"fixture warning"},
	}
}

func fixtureIncompleteComparison(reportOnly bool) report.Comparison {
	comparison := fixtureComparison(false, reportOnly)
	comparison.B.Complete = false
	comparison.B.MissingScoreable = 11
	comparison.ExitCriteria.Incomplete = true
	comparison.ExitCriteria.IncompleteRuns = []string{cliCompareRunB}
	comparison.Warnings = append(comparison.Warnings,
		"run "+cliCompareRunB+" is missing 11 scoreable scenarios")
	return comparison
}

func fixtureCompareDeps(t *testing.T, failed bool) compareDeps {
	t.Helper()
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	return compareDeps{
		OpenSource: func(context.Context) (report.Source, func(), error) {
			return nil, func() {}, nil
		},
		Compare: func(_ context.Context, _ report.Source, _, _ string,
			opts report.CompareOptions) (report.Comparison, error) {
			return fixtureComparison(failed, opts.ReportOnly), nil
		},
		Clock: clock.NewFake(now),
	}
}

func runCompareCLI(t *testing.T, deps compareDeps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), append([]string{"compare"}, args...),
		&stdout, &stderr, commandDeps{Compare: deps})
	return code, stdout.String(), stderr.String()
}

func TestCompareHelp(t *testing.T) {
	code, _, stderr := runCompareCLI(t, fixtureCompareDeps(t, false), "--help")
	if code != 0 {
		t.Fatalf("compare --help code = %d", code)
	}
	if !strings.Contains(stderr, "usage: recon-eval compare") {
		t.Fatalf("compare --help output = %q", stderr)
	}
}

func TestCompareUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no arguments":     {},
		"one argument":     {cliCompareRunA},
		"three arguments":  {cliCompareRunA, cliCompareRunB, cliCompareRunA},
		"bad runA":         {"not-a-uuid", cliCompareRunB},
		"bad runB":         {cliCompareRunA, "not-a-uuid"},
		"unknown flag":     {cliCompareRunA, cliCompareRunB, "--bogus"},
		"json and out-dir": {cliCompareRunA, cliCompareRunB, "--json", "a.json", "--out-dir", "dir"},
		"missing json":     {cliCompareRunA, cliCompareRunB, "--json"},
		"missing out-dir":  {cliCompareRunA, cliCompareRunB, "--out-dir"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runCompareCLI(t, fixtureCompareDeps(t, false), args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%q", code, stderr)
			}
		})
	}
}

func TestCompareOpenAndBuildErrors(t *testing.T) {
	deps := fixtureCompareDeps(t, false)
	deps.OpenSource = func(context.Context) (report.Source, func(), error) {
		return nil, nil, errors.New("database unavailable")
	}
	code, _, stderr := runCompareCLI(t, deps, cliCompareRunA, cliCompareRunB)
	if code != 1 || !strings.Contains(stderr, "database unavailable") {
		t.Fatalf("open error = code %d stderr %q", code, stderr)
	}

	deps = fixtureCompareDeps(t, false)
	deps.Compare = func(context.Context, report.Source, string, string,
		report.CompareOptions) (report.Comparison, error) {
		return report.Comparison{}, errors.New("eval run missing")
	}
	code, _, stderr = runCompareCLI(t, deps, cliCompareRunA, cliCompareRunB, "--report-only")
	if code != 1 || !strings.Contains(stderr, "eval run missing") {
		t.Fatalf("load error = code %d stderr %q", code, stderr)
	}

	deps = fixtureCompareDeps(t, false)
	deps.Compare = func(context.Context, report.Source, string, string,
		report.CompareOptions) (report.Comparison, error) {
		return report.Comparison{}, fmt.Errorf("%w: split \"dev\" != \"test\"",
			report.ErrCompareMismatch)
	}
	code, _, stderr = runCompareCLI(t, deps, cliCompareRunA, cliCompareRunB, "--report-only")
	if code != 2 {
		t.Fatalf("metadata mismatch code = %d, want 2 even with --report-only; stderr=%q",
			code, stderr)
	}
}

func TestCompareExitCodes(t *testing.T) {
	cases := map[string]struct {
		failed     bool
		args       []string
		wantCode   int
		wantFooter string
	}{
		"regression": {
			failed: true, args: nil, wantCode: 1,
			wantFooter: "Exit code 1.",
		},
		"regression report-only": {
			failed: true, args: []string{"--report-only"}, wantCode: 0,
			wantFooter: "--report-only: exit suppressed",
		},
		"improvement or no-change": {
			failed: false, args: nil, wantCode: 0,
			wantFooter: "No gating regression.",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			args := append([]string{cliCompareRunA, cliCompareRunB}, tc.args...)
			code, stdout, stderr := runCompareCLI(t, fixtureCompareDeps(t, tc.failed), args...)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stderr=%q", code, tc.wantCode, stderr)
			}
			if !strings.Contains(stdout, tc.wantFooter) {
				t.Fatalf("stdout missing %q:\n%s", tc.wantFooter, stdout)
			}
		})
	}
}

func incompleteCompareDeps() compareDeps {
	return compareDeps{
		OpenSource: func(context.Context) (report.Source, func(), error) {
			return nil, func() {}, nil
		},
		Compare: func(_ context.Context, _ report.Source, _, _ string,
			opts report.CompareOptions) (report.Comparison, error) {
			return fixtureIncompleteComparison(opts.ReportOnly), nil
		},
		Clock: clock.NewFake(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)),
	}
}

func TestCompareIncompleteExitCodes(t *testing.T) {
	args := []string{cliCompareRunA, cliCompareRunB}
	code, stdout, stderr := runCompareCLI(t, incompleteCompareDeps(), args...)
	if code != 2 {
		t.Fatalf("incomplete code = %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "incomplete run data") {
		t.Fatalf("stderr = %q, want incomplete warning", stderr)
	}
	for _, want := range []string{
		"Incomplete comparison",
		"Exit code 2 (incomplete run data).",
		"missing 11 scoreable scenarios",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}

	jsonPath := filepath.Join(t.TempDir(), "compare.json")
	code, stdout, stderr = runCompareCLI(t, incompleteCompareDeps(),
		append(append([]string{}, args...), "--json", jsonPath)...)
	if code != 2 {
		t.Fatalf("incomplete --json code = %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Incomplete comparison") {
		t.Fatalf("--json stdout missing markdown warning:\n%s", stdout)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read JSON output: %v", err)
	}
	var decoded report.Comparison
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode JSON output: %v", err)
	}
	if !decoded.ExitCriteria.Incomplete || decoded.B.Complete ||
		decoded.B.MissingScoreable != 11 {
		t.Fatalf("JSON completeness = %+v exit=%+v",
			decoded.B, decoded.ExitCriteria)
	}
	if !strings.Contains(strings.Join(decoded.Warnings, "\n"),
		"missing 11 scoreable scenarios") {
		t.Fatalf("JSON warnings = %v", decoded.Warnings)
	}

	code, stdout, stderr = runCompareCLI(t, incompleteCompareDeps(),
		append(append([]string{}, args...), "--report-only")...)
	if code != 0 {
		t.Fatalf("incomplete --report-only code = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Incomplete comparison") ||
		!strings.Contains(stdout,
			"--report-only: exit suppressed (would exit 2 for incomplete data).") {
		t.Fatalf("report-only stdout missing incomplete footer:\n%s", stdout)
	}
	if !strings.Contains(stderr, "incomplete run data") {
		t.Fatalf("report-only stderr = %q, want warning", stderr)
	}
}

func TestCompareSuccessOutputs(t *testing.T) {
	var gotOpts report.CompareOptions
	deps := fixtureCompareDeps(t, false)
	deps.ModuleRoot = "/module"
	deps.Compare = func(_ context.Context, _ report.Source, runA, runB string,
		opts report.CompareOptions) (report.Comparison, error) {
		gotOpts = opts
		if runA != cliCompareRunA || runB != cliCompareRunB {
			t.Fatalf("run ids = %q/%q", runA, runB)
		}
		return fixtureComparison(false, opts.ReportOnly), nil
	}

	code, stdout, stderr := runCompareCLI(t, deps,
		cliCompareRunA, cliCompareRunB, "--report-only", "--dataset-dir", "/dataset")
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "# Eval run comparison") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !gotOpts.ReportOnly || gotOpts.DatasetDir != "/dataset" || gotOpts.ModuleRoot != "/module" {
		t.Fatalf("compare options = %+v", gotOpts)
	}

	jsonDir := t.TempDir()
	jsonPath := filepath.Join(jsonDir, "explicit.json")
	code, stdout, stderr = runCompareCLI(t, deps, cliCompareRunA, cliCompareRunB,
		"--json", jsonPath)
	if code != 0 {
		t.Fatalf("json code = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "# Eval run comparison") {
		t.Fatalf("json stdout = %q", stdout)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read JSON output: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode JSON output: %v", err)
	}
	if decoded["schema"] != report.CompareSchema {
		t.Fatalf("JSON schema = %v", decoded["schema"])
	}

	outDir := t.TempDir()
	code, stdout, stderr = runCompareCLI(t, deps, cliCompareRunA, cliCompareRunB,
		"--out-dir", outDir)
	if code != 0 {
		t.Fatalf("out-dir code = %d stderr=%q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("out-dir stdout = %q, want empty", stdout)
	}
	for _, name := range []string{"compare.md", "compare.json"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Fatalf("out-dir %s: %v", name, err)
		}
	}
}

func TestCompareOutputPathErrors(t *testing.T) {
	code, _, stderr := runCompareCLI(t, fixtureCompareDeps(t, false),
		cliCompareRunA, cliCompareRunB, "--out-dir", filepath.Join(t.TempDir(), "missing"))
	if code != 1 {
		t.Fatalf("missing out-dir code = %d stderr=%q", code, stderr)
	}
	code, _, stderr = runCompareCLI(t, fixtureCompareDeps(t, false),
		cliCompareRunA, cliCompareRunB,
		"--json", filepath.Join(t.TempDir(), "missing", "compare.json"))
	if code != 1 {
		t.Fatalf("missing json parent code = %d stderr=%q", code, stderr)
	}
}
