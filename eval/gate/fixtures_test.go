package gate

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/report"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

const (
	gateRunA = "11111111-1111-4111-8111-111111111111"
	gateRunB = "22222222-2222-4222-8222-222222222222"

	gateModel  = "deepseek-v4.1-flash"
	gatePrompt = "v2"
)

var (
	gateTruthOnce sync.Once
	gateTruth     dataset.GroundTruth
	gateManifest  dataset.Manifest
	gateTruthErr  error
)

func testSplitDataset(t *testing.T) (dataset.GroundTruth, dataset.Manifest) {
	t.Helper()
	gateTruthOnce.Do(func() {
		cfg, err := dataset.DefaultConfig(dataset.Test)
		if err != nil {
			gateTruthErr = err
			return
		}
		built, err := dataset.Build(cfg)
		if err != nil {
			gateTruthErr = err
			return
		}
		gateTruth = built.Truth
		gateManifest = built.Manifest
	})
	if gateTruthErr != nil {
		t.Fatalf("build test dataset: %v", gateTruthErr)
	}
	return gateTruth, gateManifest
}

func testSplitDir(t *testing.T) string {
	t.Helper()
	root, err := report.FindModuleRoot()
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	return filepath.Join(root, "eval", "datasets", string(dataset.Test))
}

func gateClock(t *testing.T) clock.Clock {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2026-11-10T00:00:00Z")
	if err != nil {
		t.Fatalf("parse fixture clock: %v", err)
	}
	return clock.NewFake(now)
}

// gateFixtureSource is the narrow report.Source fixture used by the Run tests.
type gateFixtureSource struct {
	runs []report.RunRow
	rows []report.CaseRow
}

func (s *gateFixtureSource) LoadRun(_ context.Context, runID string) (report.RunRow, error) {
	for _, run := range s.runs {
		if run.ID == runID {
			return run, nil
		}
	}
	return report.RunRow{}, fmt.Errorf("eval run %s not found", runID)
}

func (s *gateFixtureSource) LoadRepeatGroup(_ context.Context, groupID string) ([]report.RunRow, error) {
	out := make([]report.RunRow, 0, len(s.runs))
	for _, run := range s.runs {
		if run.RepeatGroup == groupID {
			out = append(out, run)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("repeat group %s not found", groupID)
	}
	return out, nil
}

func (s *gateFixtureSource) LoadCaseResults(_ context.Context, runIDs []string) ([]report.CaseRow, error) {
	known := make(map[string]struct{}, len(runIDs))
	for _, id := range runIDs {
		known[id] = struct{}{}
	}
	out := make([]report.CaseRow, 0, len(s.rows))
	for _, row := range s.rows {
		if _, ok := known[row.RunID]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

func gateRunRow(t *testing.T, id string, seed int64) report.RunRow {
	t.Helper()
	_, manifest := testSplitDataset(t)
	started, err := time.Parse(time.RFC3339, "2026-11-10T00:00:00Z")
	if err != nil {
		t.Fatalf("parse started at: %v", err)
	}
	ended := started.Add(10 * time.Minute)
	return report.RunRow{
		ID:             id,
		Suite:          "agent",
		Split:          dataset.Test,
		DatasetVersion: manifest.EvalDatasetVersion,
		Model:          gateModel,
		PromptVersion:  gatePrompt,
		Seed:           seed,
		GitSHA:         "0123456789abcdef0123456789abcdef01234567",
		StartedAt:      started,
		EndedAt:        &ended,
	}
}

// completeGateRows returns one complete, all-correct row per scoreable test
// scenario. The fixture exercises thresholds, never completeness.
func completeGateRows(t *testing.T, runID string) []report.CaseRow {
	t.Helper()
	truth, _ := testSplitDataset(t)
	rows := make([]report.CaseRow, 0, len(truth.Scenarios))
	for index, scenario := range truth.Scenarios {
		if scenario.BreakSignature == "" && scenario.Mode != modes.InjectionMemo {
			continue
		}
		confidence := decimal.RequireFromString("0.9000")
		rows = append(rows, report.CaseRow{
			RunID:       runID,
			ScenarioID:  scenario.ScenarioID,
			FailureMode: scenario.Mode,
			Correct:     true,
			FalseFix:    false,
			Escalated:   false,
			Confidence:  &confidence,
			CostUSD:     "0.010000",
			LatencyMS:   100 + index%7,
			Steps:       2,
			Detail: map[string]any{
				"actual_resolution":    "adjustment",
				"expected_resolutions": append([]string(nil), scenario.ExpectedResolutions...),
				"allows_escalate":      false,
				"resolvable_truth":     true,
				"has_proposal":         true,
				"evidence_total":       2,
				"evidence_real":        2,
				"evidence_relevant":    2,
				"evidence_valid":       true,
				"injection_safe":       true,
				"outcome":              "proposal",
			},
		})
	}
	return rows
}

func completeGateSource(t *testing.T) (*gateFixtureSource, report.RunRow, report.RunRow) {
	t.Helper()
	baseline := gateRunRow(t, gateRunA, 2)
	candidate := gateRunRow(t, gateRunB, 2)
	rows := append(completeGateRows(t, baseline.ID), completeGateRows(t, candidate.ID)...)
	return &gateFixtureSource{
		runs: []report.RunRow{baseline, candidate},
		rows: rows,
	}, baseline, candidate
}

func baseComparison() report.Comparison {
	return report.Comparison{
		Schema:      report.CompareSchema,
		GeneratedAt: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		A: report.CompareRun{
			RunID: gateRunA, Suite: "agent", Split: dataset.Test,
			DatasetVersion: "test-version", Model: gateModel, PromptVersion: gatePrompt,
			Seed: 2, GitSHA: "sha-a", Cases: 100, Complete: true,
			MissingModes: []string{},
		},
		B: report.CompareRun{
			RunID: gateRunB, Suite: "agent", Split: dataset.Test,
			DatasetVersion: "test-version", Model: gateModel, PromptVersion: gatePrompt,
			Seed: 2, GitSHA: "sha-b", Cases: 100, Complete: true,
			MissingModes: []string{},
		},
		Overall: report.CompareBlock{
			Cases: report.CompareCounts{A: 100, B: 100},
		},
		ByMode: map[string]report.CompareBlock{},
		ExitCriteria: report.CompareExitCriteria{
			IncompleteRuns: []string{},
			Regressions:    []report.CompareRegression{},
		},
		Warnings: []string{},
	}
}

// gateRate builds a CompareRate exactly the way report.Compare does: Wilson
// intervals from the counts, proportion delta in percentage points, and the
// disjoint-interval classification. Tests use it to cover threshold math
// without duplicating report's comparison implementation.
func gateRate(metric string, higherIsBetter bool, aNum, aDen, bNum, bDen int) report.CompareRate {
	rate := report.CompareRate{
		Direction:      directionLabel(higherIsBetter),
		Gating:         metric == "false_fix" || metric == "accuracy",
		Classification: report.ClassificationNotApplicable,
	}
	out := rate
	an, bn := aDen, bDen
	out.AN = &an
	out.BN = &bn
	if aDen > 0 {
		value := float64(aNum) / float64(aDen)
		out.A = &value
	}
	if bDen > 0 {
		value := float64(bNum) / float64(bDen)
		out.B = &value
	}
	intervalA, okA := report.Wilson(aNum, aDen)
	intervalB, okB := report.Wilson(bNum, bDen)
	if okA {
		out.ACI = &[2]float64{intervalA.Low, intervalA.High}
	}
	if okB {
		out.BCI = &[2]float64{intervalB.Low, intervalB.High}
	}
	if out.A == nil || out.B == nil {
		return out
	}
	deltaPP := math.Round((*out.B-*out.A)*100*1e6) / 1e6
	out.DeltaPP = &deltaPP
	if out.ACI == nil || out.BCI == nil {
		return out
	}
	overlap := !(out.BCI[1] < out.ACI[0] || out.BCI[0] > out.ACI[1])
	out.Overlap = &overlap
	if overlap {
		out.Classification = report.ClassificationNoChange
		return out
	}
	if higherIsBetter {
		if out.BCI[1] < out.ACI[0] {
			out.Classification = report.ClassificationRegression
		} else {
			out.Classification = report.ClassificationImprovement
		}
		return out
	}
	if out.BCI[0] > out.ACI[1] {
		out.Classification = report.ClassificationRegression
	} else {
		out.Classification = report.ClassificationImprovement
	}
	return out
}

func directionLabel(higherIsBetter bool) string {
	if higherIsBetter {
		return "higher_is_better"
	}
	return "lower_is_better"
}

// passingComparison has every applicable metric defined and non-regressing;
// injection resistance is undefined on the test split exactly like the real
// aggregate.
func passingComparison() report.Comparison {
	comparison := baseComparison()
	comparison.Overall.Accuracy = gateRate("accuracy", true, 80, 100, 80, 100)
	comparison.Overall.FalseFix = gateRate("false_fix", false, 0, 100, 0, 100)
	comparison.Overall.InjectionResistance = gateRate("injection_resistance", true, 0, 0, 0, 0)
	return comparison
}

func failingFalseFixComparison() report.Comparison {
	comparison := passingComparison()
	// 0/100 vs 10/100 is a genuine disjoint 95% Wilson regression. The spec
	// example's 5/100 is not disjoint under the committed Wilson math (Q-69).
	comparison.Overall.FalseFix = gateRate("false_fix", false, 0, 100, 10, 100)
	return comparison
}
