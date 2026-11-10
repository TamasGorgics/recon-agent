package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/gate"
	"github.com/TamasGorgics/recon-agent/eval/report"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const (
	cliGateCandidate = "11111111-1111-4111-8111-111111111111"
	cliGateBaseline  = "22222222-2222-4222-8222-222222222222"
)

func gateFixtureDocument(code int, reportOnly bool) gate.Document {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	failed := []string{}
	if code == 1 {
		failed = []string{gate.ThresholdAccuracy}
	}
	incompleteRuns := []string{}
	if code == 2 {
		incompleteRuns = []string{cliGateCandidate}
	}
	return gate.Document{
		Schema:      gate.DocumentSchema,
		GeneratedAt: now,
		Baseline: gate.RunMetadata{
			RunID: cliGateBaseline, Suite: "agent", Split: "test",
			DatasetVersion: "dataset", Seed: 2, Model: "model", PromptVersion: "v2",
			Cases: 100, Complete: true,
		},
		Candidate: gate.RunMetadata{
			RunID: cliGateCandidate, Suite: "agent", Split: "test",
			DatasetVersion: "dataset", Seed: 2, Model: "model", PromptVersion: "v2",
			Cases: 100, Complete: true,
		},
		Thresholds: gate.ThresholdSource{
			Path: "eval/thresholds.yaml", SchemaVersion: 1,
			BaselineSplit: "test", Confidence: 0.95, MinSample: 30,
		},
		Results: []gate.Result{},
		Exit: gate.ExitInfo{
			Code:             code,
			FailedIDs:        failed,
			PassedIDs:        []string{},
			NotApplicableIDs: []string{},
			ErrorIDs:         []string{},
			Incomplete:       code == 2,
			IncompleteRuns:   incompleteRuns,
			ReportOnly:       reportOnly,
			Reasons:          []string{"fixture reason"},
		},
		Warnings: []string{},
	}
}

type gateRunCapture struct {
	opts gate.Options
}

func fixtureGateDeps(t *testing.T, document gate.Document, runErr error,
	captured *gateRunCapture) gateDeps {
	t.Helper()
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	return gateDeps{
		OpenSource: func(context.Context) (report.Source, func(), error) {
			return nil, func() {}, nil
		},
		LoadThresholds: func(string) (gate.Thresholds, error) {
			return gate.Thresholds{}, nil
		},
		Run: func(_ context.Context, _ report.Source, candidateID, baselineID string,
			_ gate.Thresholds, opts gate.Options) (gate.Document, error) {
			if captured != nil {
				captured.opts = opts
			}
			if runErr != nil {
				return gate.Document{}, runErr
			}
			if candidateID != cliGateCandidate || baselineID != cliGateBaseline {
				return gate.Document{}, fmt.Errorf("unexpected run IDs %s %s",
					candidateID, baselineID)
			}
			return document, nil
		},
		Clock: clock.NewFake(now),
	}
}

func runGateCLI(t *testing.T, deps gateDeps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), append([]string{"gate"}, args...),
		&stdout, &stderr, commandDeps{Gate: deps})
	return code, stdout.String(), stderr.String()
}

func validGateArgs(extra ...string) []string {
	args := []string{"--run", cliGateCandidate, "--baseline", cliGateBaseline}
	return append(args, extra...)
}

func TestGateHelpAndTopLevelHelp(t *testing.T) {
	code, _, stderr := runGateCLI(t, fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil),
		"--help")
	if code != 0 {
		t.Fatalf("gate --help code = %d", code)
	}
	for _, want := range []string{
		"usage: recon-eval gate", "Exit codes: 0", "--report-only",
		"thresholds.baseline_split", "held-out test split",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("gate --help missing %q:\n%s", want, stderr)
		}
	}

	var stdout, errOut bytes.Buffer
	code = Main(context.Background(), []string{"help"}, &stdout, &errOut)
	if code != 0 {
		t.Fatalf("help code = %d", code)
	}
	if !strings.Contains(stdout.String(), "gate") {
		t.Fatalf("top-level help does not list gate:\n%s", stdout.String())
	}
}

func TestGateUsageErrors(t *testing.T) {
	deps := fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil)
	cases := map[string][]string{
		"missing run":        {"--baseline", cliGateBaseline},
		"missing baseline":   {"--run", cliGateCandidate},
		"invalid run":        {"--run", "not-a-uuid", "--baseline", cliGateBaseline},
		"invalid baseline":   {"--run", cliGateCandidate, "--baseline", "nope"},
		"mutually exclusive": validGateArgs("--json", "out.json", "--out-dir", "."),
		"unexpected arg":     validGateArgs("extra"),
		"unknown flag":       validGateArgs("--bogus"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runGateCLI(t, deps, args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%q", code, stderr)
			}
		})
	}
}

func TestGateLoadAndSourceErrors(t *testing.T) {
	t.Run("threshold load", func(t *testing.T) {
		deps := fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil)
		deps.LoadThresholds = func(string) (gate.Thresholds, error) {
			return gate.Thresholds{}, errors.New("bad thresholds")
		}
		code, _, stderr := runGateCLI(t, deps, validGateArgs()...)
		if code != 1 || !strings.Contains(stderr, "bad thresholds") {
			t.Fatalf("code = %d stderr = %q", code, stderr)
		}
	})
	t.Run("open source", func(t *testing.T) {
		deps := fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil)
		deps.OpenSource = func(context.Context) (report.Source, func(), error) {
			return nil, nil, errors.New("no database")
		}
		code, _, stderr := runGateCLI(t, deps, validGateArgs()...)
		if code != 1 || !strings.Contains(stderr, "no database") {
			t.Fatalf("code = %d stderr = %q", code, stderr)
		}
	})
}

func TestGateRunErrors(t *testing.T) {
	t.Run("metadata mismatch", func(t *testing.T) {
		deps := fixtureGateDeps(t, gate.Document{}, fmt.Errorf(
			"%w: model \"a\" != \"b\"", gate.ErrMetadataMismatch), nil)
		code, _, stderr := runGateCLI(t, deps, validGateArgs()...)
		if code != 2 || !strings.Contains(stderr, "model") {
			t.Fatalf("code = %d stderr = %q", code, stderr)
		}
	})
	t.Run("generic run error", func(t *testing.T) {
		deps := fixtureGateDeps(t, gate.Document{}, errors.New("report build failed"), nil)
		code, _, stderr := runGateCLI(t, deps, validGateArgs()...)
		if code != 1 || !strings.Contains(stderr, "report build failed") {
			t.Fatalf("code = %d stderr = %q", code, stderr)
		}
	})
}

func TestGateExitCodes(t *testing.T) {
	for _, want := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("exit-%d", want), func(t *testing.T) {
			deps := fixtureGateDeps(t, gateFixtureDocument(want, false), nil, nil)
			code, stdout, stderr := runGateCLI(t, deps, validGateArgs()...)
			if code != want {
				t.Fatalf("code = %d, want %d; stderr=%q", code, want, stderr)
			}
			if !strings.Contains(stdout, fmt.Sprintf("Exit code: `%d`", want)) {
				t.Fatalf("stdout missing exit footer:\n%s", stdout)
			}
		})
	}
}

func TestGateReportOnly(t *testing.T) {
	var captured gateRunCapture
	deps := fixtureGateDeps(t, gateFixtureDocument(0, true), nil, &captured)
	code, stdout, stderr := runGateCLI(t, deps, validGateArgs("--report-only")...)
	if code != 0 {
		t.Fatalf("code = %d stderr = %q", code, stderr)
	}
	if !captured.opts.ReportOnly {
		t.Fatal("--report-only was not passed to gate.Run")
	}
	if !strings.Contains(stdout, "informational") {
		t.Fatalf("stdout missing report-only marker:\n%s", stdout)
	}
}

func TestGateOutputs(t *testing.T) {
	t.Run("json writes exact path and prints markdown", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "gate.json")
		deps := fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil)
		code, stdout, stderr := runGateCLI(t, deps, validGateArgs("--json", path)...)
		if code != 0 {
			t.Fatalf("code = %d stderr = %q", code, stderr)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read JSON: %v", err)
		}
		if !bytes.Contains(data, []byte(`"schema": "eval-gate/v1"`)) {
			t.Fatalf("JSON missing schema:\n%s", data)
		}
		if !strings.Contains(stdout, "Eval gate") {
			t.Fatalf("stdout missing markdown:\n%s", stdout)
		}
	})
	t.Run("out dir writes pair", func(t *testing.T) {
		dir := t.TempDir()
		deps := fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil)
		code, stdout, stderr := runGateCLI(t, deps, validGateArgs("--out-dir", dir)...)
		if code != 0 {
			t.Fatalf("code = %d stderr = %q", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want empty", stdout)
		}
		for _, name := range []string{"gate.md", "gate.json"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	t.Run("missing json parent", func(t *testing.T) {
		dir := t.TempDir()
		deps := fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil)
		code, _, _ := runGateCLI(t, deps, validGateArgs(
			"--json", filepath.Join(dir, "missing", "gate.json"))...)
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})
	t.Run("missing out dir", func(t *testing.T) {
		dir := t.TempDir()
		deps := fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil)
		code, _, _ := runGateCLI(t, deps, validGateArgs(
			"--out-dir", filepath.Join(dir, "missing"))...)
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})
}

func TestGateUnwrittenFilePath(t *testing.T) {
	deps := fixtureGateDeps(t, gateFixtureDocument(0, false), nil, nil)
	code, _, _ := runGateCLI(t, deps, validGateArgs("--thresholds", "")...)
	if code != 0 {
		t.Fatalf("empty thresholds flag should fall back to the default; code = %d", code)
	}
}

// devGateSource is the minimal report.Source for split-rejection CLI tests:
// gate.Run rejects the runs before it reads any case results.
type devGateSource map[string]report.RunRow

func (s devGateSource) LoadRun(_ context.Context, runID string) (report.RunRow, error) {
	run, ok := s[runID]
	if !ok {
		return report.RunRow{}, fmt.Errorf("eval run %s not found", runID)
	}
	return run, nil
}

func (s devGateSource) LoadRepeatGroup(context.Context, string) ([]report.RunRow, error) {
	return nil, errors.New("repeat group not used")
}

func (s devGateSource) LoadCaseResults(context.Context, []string) ([]report.CaseRow, error) {
	return nil, nil
}

func TestGateNonTestSplitExitsTwo(t *testing.T) {
	root, err := report.FindModuleRoot()
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	thresholds, err := gate.LoadThresholds(filepath.Join(root, "eval", "thresholds.yaml"))
	if err != nil {
		t.Fatalf("load thresholds: %v", err)
	}
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	src := devGateSource{
		cliGateBaseline: {
			ID: cliGateBaseline, Suite: "agent", Split: dataset.Dev,
			DatasetVersion: "dataset", Seed: 2, Model: "model", PromptVersion: "v1",
		},
		cliGateCandidate: {
			ID: cliGateCandidate, Suite: "agent", Split: dataset.Dev,
			DatasetVersion: "dataset", Seed: 2, Model: "model", PromptVersion: "v1",
		},
	}
	deps := gateDeps{
		OpenSource: func(context.Context) (report.Source, func(), error) {
			return src, func() {}, nil
		},
		LoadThresholds: func(string) (gate.Thresholds, error) { return thresholds, nil },
		Run:            gate.Run,
		ModuleRoot:     root,
		Clock:          clock.NewFake(now),
	}
	code, _, stderr := runGateCLI(t, deps, validGateArgs()...)
	if code != 2 {
		t.Fatalf("dev-vs-dev code = %d, want 2; stderr=%q", code, stderr)
	}
	for _, want := range []string{"split", "thresholds.baseline_split", `"test"`, `"dev"`} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q does not contain %q", stderr, want)
		}
	}

	// --report-only suppresses gating and incomplete exits only; a non-test
	// split is a metadata rejection and must still exit 2.
	code, _, stderr = runGateCLI(t, deps, validGateArgs("--report-only")...)
	if code != 2 {
		t.Fatalf("dev-vs-dev --report-only code = %d, want 2; stderr=%q", code, stderr)
	}
}
