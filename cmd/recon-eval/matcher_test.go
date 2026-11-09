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

	"github.com/TamasGorgics/recon-agent/eval/component"
	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

func fixtureMatcherReport(split dataset.Split) component.MatcherReport {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	value := 0.5
	return component.MatcherReport{
		Schema:             component.MatcherSchema,
		GeneratedAt:        now,
		Split:              split,
		DatasetVersion:     "dataset",
		Scenarios:          8,
		ScoreableScenarios: 6,
		Matches:            2,
		CorrectMatches:     1,
		Precision: component.RateView{
			Defined: true, Numerator: 1, Denominator: 2, Value: &value,
		},
		ExpectedLinks: 2,
		RecalledLinks: 1,
		Recall: component.RateView{
			Defined: true, Numerator: 1, Denominator: 2, Value: &value,
		},
		PerPass:  []component.PassScore{},
		Excluded: map[string]int{},
		Warnings: []string{"fixture warning"},
	}
}

func fixtureMatcherDeps(t *testing.T) matcherDeps {
	t.Helper()
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	return matcherDeps{
		Build: func(_ context.Context, opts component.Options) (component.MatcherReport, error) {
			return fixtureMatcherReport(opts.Split), nil
		},
		Clock: clock.NewFake(now),
	}
}

func runMatcherCLI(t *testing.T, deps matcherDeps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), append([]string{"matcher"}, args...),
		&stdout, &stderr, commandDeps{Matcher: deps})
	return code, stdout.String(), stderr.String()
}

func TestMatcherHelp(t *testing.T) {
	code, _, stderr := runMatcherCLI(t, fixtureMatcherDeps(t), "--help")
	if code != 0 {
		t.Fatalf("matcher --help code = %d", code)
	}
	if !strings.Contains(stderr, "usage: recon-eval matcher") {
		t.Fatalf("matcher --help output = %q", stderr)
	}
}

func TestMatcherUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"missing split":    {},
		"empty split":      {"--split", ""},
		"unknown split":    {"--split", "prod"},
		"unknown flag":     {"--split", "dev", "--bogus"},
		"extra argument":   {"--split", "dev", "extra"},
		"json and out-dir": {"--split", "dev", "--json", "a.json", "--out-dir", "dir"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runMatcherCLI(t, fixtureMatcherDeps(t), args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%q", code, stderr)
			}
		})
	}
}

func TestMatcherBuildError(t *testing.T) {
	deps := fixtureMatcherDeps(t)
	deps.Build = func(context.Context, component.Options) (component.MatcherReport, error) {
		return component.MatcherReport{}, errors.New("dataset drift")
	}
	code, _, stderr := runMatcherCLI(t, deps, "--split", "dev")
	if code != 1 || !strings.Contains(stderr, "dataset drift") {
		t.Fatalf("build error = code %d stderr %q", code, stderr)
	}
}

func TestMatcherSuccessStdoutAndOptions(t *testing.T) {
	var gotOpts component.Options
	deps := fixtureMatcherDeps(t)
	deps.Build = func(_ context.Context, opts component.Options) (component.MatcherReport, error) {
		gotOpts = opts
		return fixtureMatcherReport(opts.Split), nil
	}
	code, stdout, stderr := runMatcherCLI(t, deps, "--split", "test",
		"--dataset-dir", "/tmp/dataset")
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "# Matcher report — split test") {
		t.Fatalf("stdout = %q", stdout)
	}
	if gotOpts.Split != dataset.Test || gotOpts.DatasetDir != "/tmp/dataset" {
		t.Fatalf("build options = %+v", gotOpts)
	}
	if gotOpts.Clock == nil {
		t.Fatal("build clock is nil")
	}
}

func TestMatcherJSONPathWritesJSONAndStdoutMarkdown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json")
	code, stdout, stderr := runMatcherCLI(t, fixtureMatcherDeps(t),
		"--split", "dev", "--json", path)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "# Matcher report") {
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
	if document["schema"] != component.MatcherSchema {
		t.Fatalf("schema = %v", document["schema"])
	}
}

func TestMatcherOutDirWritesBothAndSuppressesStdout(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runMatcherCLI(t, fixtureMatcherDeps(t),
		"--split", "dev", "--out-dir", dir)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty for --out-dir", stdout)
	}
	for _, name := range []string{"matcher.md", "matcher.json"} {
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

func TestMatcherMissingOutputParentsFail(t *testing.T) {
	base := t.TempDir()
	code, _, stderr := runMatcherCLI(t, fixtureMatcherDeps(t),
		"--split", "dev", "--out-dir", filepath.Join(base, "missing"))
	if code != 1 || !strings.Contains(stderr, "out-dir") {
		t.Fatalf("missing out-dir = code %d stderr %q", code, stderr)
	}
	code, _, stderr = runMatcherCLI(t, fixtureMatcherDeps(t),
		"--split", "dev", "--json", filepath.Join(base, "missing", "matcher.json"))
	if code != 1 || !strings.Contains(stderr, "json parent") {
		t.Fatalf("missing json parent = code %d stderr %q", code, stderr)
	}
}

func TestMainHelpIncludesMatcherAndDispatchUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"help"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "recon-eval matcher") {
		t.Fatalf("help = code %d out %q", code, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Main(context.Background(), []string{"matcher", "--split", "prod"},
		&stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "not dev, test, or injection") {
		t.Fatalf("dispatch usage = code %d stderr %q", code, stderr.String())
	}
}
