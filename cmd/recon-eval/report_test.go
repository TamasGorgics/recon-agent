package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/report"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const cliRunID = "22222222-2222-4222-8222-222222222222"

func fixtureReport(runID string) report.Report {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	return report.Report{
		Schema:      report.Schema,
		GeneratedAt: now,
		Run: report.RunMetadata{
			ID: runID, Suite: "agent", Split: dataset.Dev,
			DatasetVersion: "dataset", Model: "test-model", PromptVersion: "v1",
		},
		Runs: []report.RunMetadata{{
			ID: runID, Suite: "agent", Split: dataset.Dev,
			DatasetVersion: "dataset", Model: "test-model", PromptVersion: "v1",
		}},
		Dataset: report.DatasetInfo{
			Split: dataset.Dev, EvalDatasetVersion: "dataset", Matched: true,
		},
		Headline:        report.MetricBlock{},
		ByMode:          map[string]report.MetricBlock{},
		ByDifficulty:    map[string]report.MetricBlock{},
		Scorecards:      []report.Scorecard{},
		FailureAppendix: []report.AppendixCase{},
		Warnings:        []string{"fixture warning"},
	}
}

func fixtureDeps(t *testing.T) reportDeps {
	t.Helper()
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	return reportDeps{
		OpenSource: func(context.Context) (report.Source, func(), error) {
			return nil, func() {}, nil
		},
		Build: func(_ context.Context, _ report.Source, runID string,
			_ report.Options) (report.Report, error) {
			return fixtureReport(runID), nil
		},
		Clock: clock.NewFake(now),
	}
}

func runReportCLI(t *testing.T, deps reportDeps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), append([]string{"report"}, args...),
		&stdout, &stderr, commandDeps{Report: deps})
	return code, stdout.String(), stderr.String()
}

func TestReportHelp(t *testing.T) {
	code, _, stderr := runReportCLI(t, fixtureDeps(t), "--help")
	if code != 0 {
		t.Fatalf("report --help code = %d", code)
	}
	if !strings.Contains(stderr, "usage: recon-eval report") {
		t.Fatalf("report --help output = %q", stderr)
	}
}

func TestReportUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"missing run":      {},
		"empty run":        {"--run", ""},
		"malformed run":    {"--run", "not-a-uuid"},
		"unknown flag":     {"--run", cliRunID, "--bogus"},
		"extra argument":   {"--run", cliRunID, "extra"},
		"json and out-dir": {"--run", cliRunID, "--json", "a.json", "--out-dir", "dir"},
		"zero appendix":    {"--run", cliRunID, "--appendix-limit", "0"},
		"negative app":     {"--run", cliRunID, "--appendix-limit", "-2"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runReportCLI(t, fixtureDeps(t), args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%q", code, stderr)
			}
		})
	}
}

func TestReportOpenAndBuildErrors(t *testing.T) {
	deps := fixtureDeps(t)
	deps.OpenSource = func(context.Context) (report.Source, func(), error) {
		return nil, nil, errors.New("database unavailable")
	}
	code, _, stderr := runReportCLI(t, deps, "--run", cliRunID)
	if code != 1 || !strings.Contains(stderr, "database unavailable") {
		t.Fatalf("open error = code %d stderr %q", code, stderr)
	}

	deps = fixtureDeps(t)
	deps.Build = func(context.Context, report.Source, string,
		report.Options) (report.Report, error) {
		return report.Report{}, errors.New("dataset drift")
	}
	code, _, stderr = runReportCLI(t, deps, "--run", cliRunID)
	if code != 1 || !strings.Contains(stderr, "dataset drift") {
		t.Fatalf("build error = code %d stderr %q", code, stderr)
	}
}

func TestReportSuccessStdoutAndOptions(t *testing.T) {
	var gotOpts report.Options
	deps := fixtureDeps(t)
	deps.Build = func(_ context.Context, _ report.Source, runID string,
		opts report.Options) (report.Report, error) {
		gotOpts = opts
		return fixtureReport(runID), nil
	}
	code, stdout, stderr := runReportCLI(t, deps, "--run", cliRunID,
		"--dataset-dir", "/tmp/dataset", "--appendix-limit", "7")
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "# Eval report — run "+cliRunID) {
		t.Fatalf("stdout = %q", stdout)
	}
	if gotOpts.DatasetDir != "/tmp/dataset" || gotOpts.AppendixLimit != 7 {
		t.Fatalf("build options = %+v", gotOpts)
	}
	if gotOpts.Clock == nil {
		t.Fatal("build clock is nil")
	}
}

func TestReportJSONPathWritesJSONAndStdoutMarkdown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json")
	code, stdout, stderr := runReportCLI(t, fixtureDeps(t),
		"--run", cliRunID, "--json", path)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "# Eval report") {
		t.Fatalf("stdout = %q", stdout)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read JSON: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if document["schema"] != report.Schema {
		t.Fatalf("schema = %v", document["schema"])
	}
}

func TestReportOutDirWritesBothAndSuppressesStdout(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runReportCLI(t, fixtureDeps(t),
		"--run", cliRunID, "--out-dir", dir)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty for --out-dir", stdout)
	}
	for _, name := range []string{"report.md", "report.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", name)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("%s mode = %v, want 0644", name, info.Mode().Perm())
		}
	}
}

func TestReportMissingOutputParentsFail(t *testing.T) {
	base := t.TempDir()
	code, _, stderr := runReportCLI(t, fixtureDeps(t),
		"--run", cliRunID, "--out-dir", filepath.Join(base, "missing"))
	if code != 1 || !strings.Contains(stderr, "out-dir") {
		t.Fatalf("missing out-dir = code %d stderr %q", code, stderr)
	}
	code, _, stderr = runReportCLI(t, fixtureDeps(t),
		"--run", cliRunID, "--json", filepath.Join(base, "missing", "report.json"))
	if code != 1 || !strings.Contains(stderr, "json parent") {
		t.Fatalf("missing json parent = code %d stderr %q", code, stderr)
	}
}

func TestMainHelpIncludesReportAndDispatchUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"help"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "recon-eval report") {
		t.Fatalf("help = code %d out %q", code, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Main(context.Background(), []string{"report", "--run", "bad"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "not a UUID") {
		t.Fatalf("dispatch usage = code %d stderr %q", code, stderr.String())
	}
}
