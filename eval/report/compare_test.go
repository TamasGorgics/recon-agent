package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
)

type comparePlan struct {
	Mode           string
	Cases          int
	Correct        int
	FalseFix       int
	CostUSD        string
	LatencyMS      int
	Steps          int
	Escalated      bool
	AllowsEscalate bool
	Resolvable     bool
	HasProposal    bool
	EvidenceValid  bool
}

func compareRunRow(t *testing.T, id, model, prompt string) RunRow {
	t.Helper()
	_, manifest := devDataset(t)
	started, err := time.Parse(time.RFC3339, fixtureStartedAt)
	if err != nil {
		t.Fatalf("parse fixture time: %v", err)
	}
	ended := started.Add(10 * time.Minute)
	return RunRow{
		ID:             id,
		Suite:          "agent",
		Split:          dataset.Dev,
		DatasetVersion: manifest.EvalDatasetVersion,
		Model:          model,
		PromptVersion:  prompt,
		Seed:           manifest.Seed,
		GitSHA:         fixtureGitSHA,
		StartedAt:      started,
		EndedAt:        &ended,
	}
}

func compareRows(t *testing.T, runID string, plans []comparePlan) []CaseRow {
	t.Helper()
	truth, _ := devDataset(t)
	byMode := make(map[string][]dataset.ScenarioTruth)
	for _, scenario := range truth.Scenarios {
		if scenario.BreakSignature == "" && scenario.Mode != "injection_memo" {
			continue
		}
		byMode[scenario.Mode] = append(byMode[scenario.Mode], scenario)
	}
	for mode := range byMode {
		sort.Slice(byMode[mode], func(i, j int) bool {
			return byMode[mode][i].ScenarioID < byMode[mode][j].ScenarioID
		})
	}
	rows := make([]CaseRow, 0)
	for _, plan := range plans {
		scenarios := byMode[plan.Mode]
		if plan.Cases > len(scenarios) {
			t.Fatalf("mode %s has %d scoreable scenarios, plan wants %d",
				plan.Mode, len(scenarios), plan.Cases)
		}
		for index := 0; index < plan.Cases; index++ {
			falseFix := index < plan.FalseFix
			correct := !falseFix && index < plan.FalseFix+plan.Correct
			cost := plan.CostUSD
			if cost == "" {
				cost = "0.010000"
			}
			detail := map[string]any{}
			if plan.AllowsEscalate {
				detail["allows_escalate"] = true
			}
			if plan.Resolvable {
				detail["resolvable_truth"] = true
			}
			if plan.HasProposal {
				detail["has_proposal"] = true
				detail["evidence_valid"] = plan.EvidenceValid
			}
			rows = append(rows, CaseRow{
				RunID:       runID,
				ScenarioID:  scenarios[index].ScenarioID,
				FailureMode: plan.Mode,
				Correct:     correct,
				FalseFix:    falseFix,
				Escalated:   plan.Escalated,
				CostUSD:     cost,
				LatencyMS:   plan.LatencyMS,
				Steps:       plan.Steps,
				Detail:      detail,
			})
		}
	}
	return rows
}

func compareFixture(t *testing.T, plansA, plansB []comparePlan) (*fixtureSource, RunRow, RunRow) {
	t.Helper()
	runA := compareRunRow(t, fixtureRun0, "deepseek-v4.1-flash", "v1")
	runB := compareRunRow(t, fixtureRun1, "glm-5.3-flash", "v2")
	rows := append(compareRows(t, runA.ID, plansA), compareRows(t, runB.ID, plansB)...)
	return &fixtureSource{runs: []RunRow{runA, runB}, rows: rows}, runA, runB
}

// completeComparePlans returns one plan per scoreable failure mode covering
// every scenario in the committed dev truth, so Compare sees a complete run.
func completeComparePlans(t *testing.T) []comparePlan {
	t.Helper()
	truth, _ := devDataset(t)
	counts := make(map[string]int)
	for _, scenario := range truth.Scenarios {
		if scenario.BreakSignature == "" && scenario.Mode != "injection_memo" {
			continue
		}
		counts[scenario.Mode]++
	}
	modeNames := make([]string, 0, len(counts))
	for mode := range counts {
		modeNames = append(modeNames, mode)
	}
	sort.Strings(modeNames)
	plans := make([]comparePlan, 0, len(modeNames))
	for _, mode := range modeNames {
		plans = append(plans, comparePlan{
			Mode: mode, Cases: counts[mode], Correct: counts[mode],
			CostUSD: "0.010000", LatencyMS: 100, Steps: 2,
		})
	}
	return plans
}

func replaceComparePlan(plans []comparePlan, replacement comparePlan) []comparePlan {
	out := append([]comparePlan(nil), plans...)
	for index := range out {
		if out[index].Mode == replacement.Mode {
			out[index] = replacement
			return out
		}
	}
	return append(out, replacement)
}

func transformComparePlans(plans []comparePlan,
	transform func(comparePlan) comparePlan) []comparePlan {
	out := make([]comparePlan, len(plans))
	for index, plan := range plans {
		out[index] = transform(plan)
	}
	return out
}

func buildComparison(t *testing.T, source *fixtureSource, runA, runB string,
	opts CompareOptions) Comparison {
	t.Helper()
	if opts.DatasetDir == "" {
		opts.DatasetDir = devDatasetDir(t)
	}
	if opts.Clock == nil {
		opts.Clock = fixtureClock(t)
	}
	comparison, err := Compare(context.Background(), source, runA, runB, opts)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return comparison
}

func TestCompareRegression(t *testing.T) {
	plansA := completeComparePlans(t)
	plansB := transformComparePlans(plansA, func(plan comparePlan) comparePlan {
		correct := plan.Cases / 2
		return comparePlan{
			Mode: plan.Mode, Cases: plan.Cases, Correct: correct,
			FalseFix: plan.Cases - correct,
			CostUSD:  plan.CostUSD, LatencyMS: plan.LatencyMS, Steps: plan.Steps,
		}
	})
	source, runA, runB := compareFixture(t, plansA, plansB)
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	if comparison.Schema != CompareSchema {
		t.Fatalf("schema = %q", comparison.Schema)
	}
	if comparison.Overall.Accuracy.Classification != ClassificationRegression {
		t.Fatalf("accuracy classification = %q",
			comparison.Overall.Accuracy.Classification)
	}
	if comparison.Overall.FalseFix.Classification != ClassificationRegression {
		t.Fatalf("false-fix classification = %q",
			comparison.Overall.FalseFix.Classification)
	}
	if comparison.Overall.Accuracy.DeltaPP == nil ||
		*comparison.Overall.Accuracy.DeltaPP != -50 {
		t.Fatalf("accuracy delta_pp = %v, want -50", comparison.Overall.Accuracy.DeltaPP)
	}
	if comparison.Overall.Accuracy.ACI == nil || comparison.Overall.Accuracy.BCI == nil ||
		comparison.Overall.Accuracy.Overlap == nil || *comparison.Overall.Accuracy.Overlap {
		t.Fatalf("accuracy intervals = %+v", comparison.Overall.Accuracy)
	}
	if !comparison.ExitCriteria.Failed {
		t.Fatal("exit criteria failed = false for a gating regression")
	}
	if comparison.ExitCriteria.ReportOnly {
		t.Fatal("exit criteria report_only = true without the flag")
	}
	if len(comparison.ExitCriteria.Regressions) == 0 {
		t.Fatal("no regressions recorded")
	}
	assertRegression(t, comparison, "overall", "", "false_fix")
	assertRegression(t, comparison, "overall", "", "accuracy")
	assertRegression(t, comparison, "mode", "fee_variance", "false_fix")
	assertRegression(t, comparison, "mode", "fee_variance", "accuracy")

	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("Gating regression detected")) ||
		!bytes.Contains(markdown, []byte("Exit code 1.")) {
		t.Fatalf("markdown decision missing:\n%s", markdown)
	}
}

func TestCompareImprovement(t *testing.T) {
	plansA := transformComparePlans(completeComparePlans(t), func(plan comparePlan) comparePlan {
		return comparePlan{
			Mode: plan.Mode, Cases: plan.Cases, Correct: plan.Cases / 2,
			CostUSD: plan.CostUSD, LatencyMS: plan.LatencyMS, Steps: plan.Steps,
		}
	})
	plansB := completeComparePlans(t)
	source, runA, runB := compareFixture(t, plansA, plansB)
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	if comparison.Overall.Accuracy.Classification != ClassificationImprovement {
		t.Fatalf("accuracy classification = %q",
			comparison.Overall.Accuracy.Classification)
	}
	if comparison.Overall.Accuracy.DeltaPP == nil ||
		*comparison.Overall.Accuracy.DeltaPP != 50 {
		t.Fatalf("accuracy delta_pp = %v, want 50", comparison.Overall.Accuracy.DeltaPP)
	}
	if comparison.Overall.FalseFix.Classification != ClassificationNoChange {
		t.Fatalf("false-fix classification = %q",
			comparison.Overall.FalseFix.Classification)
	}
	if comparison.ExitCriteria.Failed || len(comparison.ExitCriteria.Regressions) != 0 {
		t.Fatalf("exit criteria = %+v, want no gating regression", comparison.ExitCriteria)
	}
	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("No gating regression.")) {
		t.Fatalf("markdown decision missing:\n%s", markdown)
	}
}

func TestCompareNoChange(t *testing.T) {
	plan := []comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 9, LatencyMS: 100, Steps: 2}}
	source, runA, runB := compareFixture(t, plan, plan)
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	if comparison.Overall.Accuracy.Classification != ClassificationNoChange {
		t.Fatalf("accuracy classification = %q",
			comparison.Overall.Accuracy.Classification)
	}
	if comparison.Overall.Accuracy.DeltaPP == nil || *comparison.Overall.Accuracy.DeltaPP != 0 {
		t.Fatalf("accuracy delta_pp = %v, want 0", comparison.Overall.Accuracy.DeltaPP)
	}
	if comparison.Overall.Accuracy.Overlap == nil || !*comparison.Overall.Accuracy.Overlap {
		t.Fatalf("accuracy overlap = %v, want true", comparison.Overall.Accuracy.Overlap)
	}
	if comparison.ExitCriteria.Failed {
		t.Fatal("identical runs flagged as a gating regression")
	}
}

func TestCompareLargePointDeltaWithOverlap(t *testing.T) {
	source, runA, runB := compareFixture(t,
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 4, LatencyMS: 100, Steps: 2}},
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 8, LatencyMS: 100, Steps: 2}})
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	accuracy := comparison.Overall.Accuracy
	if accuracy.DeltaPP == nil || *accuracy.DeltaPP < 33 || *accuracy.DeltaPP > 34 {
		t.Fatalf("accuracy delta_pp = %v, want about +33.33", accuracy.DeltaPP)
	}
	if accuracy.Overlap == nil || !*accuracy.Overlap {
		t.Fatalf("accuracy overlap = %v, want true", accuracy.Overlap)
	}
	if accuracy.Classification != ClassificationNoChange {
		t.Fatalf("accuracy classification = %q, want no_change despite a large delta",
			accuracy.Classification)
	}
	if comparison.ExitCriteria.Failed {
		t.Fatal("overlapping intervals caused a gating regression")
	}
}

func TestCompareUndefinedRatesAndAbsentMode(t *testing.T) {
	source, runA, runB := compareFixture(t,
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12, LatencyMS: 100, Steps: 2}},
		[]comparePlan{{Mode: "duplicate_charge", Cases: 12, Correct: 12, LatencyMS: 100, Steps: 2}})
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	fee := comparison.ByMode["fee_variance"]
	if fee.Accuracy.Classification != ClassificationNotApplicable {
		t.Fatalf("absent-side accuracy classification = %q",
			fee.Accuracy.Classification)
	}
	if fee.Accuracy.B != nil || fee.Accuracy.BN != nil || fee.Accuracy.DeltaPP != nil ||
		fee.Accuracy.Overlap != nil {
		t.Fatalf("absent-side accuracy is not n/a: %+v", fee.Accuracy)
	}
	if fee.Cases.A != 12 || fee.Cases.B != 0 {
		t.Fatalf("fee cases = %+v, want 12/0", fee.Cases)
	}
	duplicate := comparison.ByMode["duplicate_charge"]
	if duplicate.Accuracy.Classification != ClassificationNotApplicable ||
		duplicate.Cases.A != 0 || duplicate.Cases.B != 12 {
		t.Fatalf("duplicate block = %+v", duplicate)
	}
	if comparison.ExitCriteria.Failed {
		t.Fatal("undefined per-mode rates caused a gating regression")
	}
}

func TestCompareZeroCases(t *testing.T) {
	plansA := completeComparePlans(t)
	runA := compareRunRow(t, fixtureRun0, "model-a", "v1")
	runB := compareRunRow(t, fixtureRun1, "model-b", "v2")
	source := &fixtureSource{
		runs: []RunRow{runA, runB},
		rows: compareRows(t, runA.ID, plansA),
	}
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	if comparison.B.Cases != 0 {
		t.Fatalf("B cases = %d, want 0", comparison.B.Cases)
	}
	if comparison.Overall.Accuracy.B != nil ||
		comparison.Overall.Accuracy.Classification != ClassificationNotApplicable {
		t.Fatalf("B accuracy = %+v, want n/a", comparison.Overall.Accuracy)
	}
	if !containsWarning(comparison.Warnings, "has no case results") {
		t.Fatalf("warnings = %v, want zero-case warning", comparison.Warnings)
	}
	if comparison.B.Complete {
		t.Fatalf("zero-case B marked complete: %+v", comparison.B)
	}
	if comparison.B.MissingScoreable == 0 {
		t.Fatalf("zero-case B missing_scoreable = 0, want all scoreable scenarios")
	}
	if !comparison.ExitCriteria.Incomplete ||
		len(comparison.ExitCriteria.IncompleteRuns) != 1 ||
		comparison.ExitCriteria.IncompleteRuns[0] != runB.ID {
		t.Fatalf("incomplete = %+v, want run B only", comparison.ExitCriteria)
	}
	if comparison.ExitCriteria.Failed {
		t.Fatal("zero-case side caused a gating regression")
	}
}

func TestCompareIncompleteCandidateFailsClosed(t *testing.T) {
	plansA := completeComparePlans(t)
	plansB := replaceComparePlan(plansA, comparePlan{
		Mode: "fee_variance", Cases: 11, Correct: 11,
		CostUSD: "0.010000", LatencyMS: 100, Steps: 2,
	})
	source, runA, runB := compareFixture(t, plansA, plansB)
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	if !comparison.A.Complete || comparison.A.MissingScoreable != 0 {
		t.Fatalf("baseline completeness = %+v, want complete", comparison.A)
	}
	if comparison.B.Complete || comparison.B.Partial || comparison.B.Aborted {
		t.Fatalf("candidate completeness = %+v, want incomplete", comparison.B)
	}
	if comparison.B.MissingScoreable != 1 {
		t.Fatalf("candidate missing_scoreable = %d, want 1", comparison.B.MissingScoreable)
	}
	if comparison.ExitCriteria.Failed {
		t.Fatalf("truncated candidate caused a gating regression: %+v",
			comparison.ExitCriteria.Regressions)
	}
	if !comparison.ExitCriteria.Incomplete ||
		len(comparison.ExitCriteria.IncompleteRuns) != 1 ||
		comparison.ExitCriteria.IncompleteRuns[0] != runB.ID {
		t.Fatalf("exit criteria = %+v, want incomplete candidate %s",
			comparison.ExitCriteria, runB.ID)
	}
	if !containsWarning(comparison.Warnings, "is missing 1 scoreable scenarios") {
		t.Fatalf("warnings = %v, want missing-scoreable warning", comparison.Warnings)
	}

	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	for _, want := range []string{
		"Incomplete comparison",
		runB.ID,
		"missing 1 scoreable scenarios",
		"| Complete | yes | no |",
		"| Missing scoreable | 0 | 1 |",
		"Exit code 2 (incomplete run data).",
	} {
		if !bytes.Contains(markdown, []byte(want)) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
	if bytes.Contains(markdown, []byte("No gating regression.")) {
		t.Fatalf("incomplete comparison claimed no regression:\n%s", markdown)
	}

	jsonBytes, err := RenderCompareJSON(comparison)
	if err != nil {
		t.Fatalf("RenderCompareJSON: %v", err)
	}
	var decoded Comparison
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("decode compare JSON: %v", err)
	}
	if !decoded.A.Complete || decoded.B.Complete || decoded.B.MissingScoreable != 1 {
		t.Fatalf("JSON completeness = A %+v B %+v", decoded.A, decoded.B)
	}
	if !decoded.ExitCriteria.Incomplete ||
		len(decoded.ExitCriteria.IncompleteRuns) != 1 ||
		decoded.ExitCriteria.IncompleteRuns[0] != runB.ID {
		t.Fatalf("JSON exit criteria = %+v", decoded.ExitCriteria)
	}
	if !containsWarning(decoded.Warnings, "is missing 1 scoreable scenarios") {
		t.Fatalf("JSON warnings = %v, want missing-scoreable warning", decoded.Warnings)
	}
}

func TestCompareIncompleteReportOnly(t *testing.T) {
	plansA := completeComparePlans(t)
	plansB := replaceComparePlan(plansA, comparePlan{
		Mode: "fee_variance", Cases: 1, Correct: 1,
		CostUSD: "0.010000", LatencyMS: 100, Steps: 2,
	})
	source, runA, runB := compareFixture(t, plansA, plansB)
	comparison := buildComparison(t, source, runA.ID, runB.ID,
		CompareOptions{ReportOnly: true})

	if !comparison.ExitCriteria.Incomplete || !comparison.ExitCriteria.ReportOnly {
		t.Fatalf("exit criteria = %+v, want incomplete + report_only", comparison.ExitCriteria)
	}
	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("--report-only: exit suppressed (would exit 2 for incomplete data).")) {
		t.Fatalf("markdown missing report-only incomplete footer:\n%s", markdown)
	}
	if bytes.Contains(markdown, []byte("Exit code 2")) {
		t.Fatalf("report-only rendered an unsuppressed exit code:\n%s", markdown)
	}
}

func TestCompareCompleteRunsUnchanged(t *testing.T) {
	plans := completeComparePlans(t)
	source, runA, runB := compareFixture(t, plans, plans)
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	if !comparison.A.Complete || !comparison.B.Complete {
		t.Fatalf("complete runs marked incomplete: A %+v B %+v",
			comparison.A, comparison.B)
	}
	if comparison.ExitCriteria.Incomplete ||
		len(comparison.ExitCriteria.IncompleteRuns) != 0 {
		t.Fatalf("exit criteria = %+v, want complete", comparison.ExitCriteria)
	}
	if comparison.ExitCriteria.Failed {
		t.Fatalf("identical complete runs failed: %+v", comparison.ExitCriteria.Regressions)
	}
	if len(comparison.Warnings) != 0 {
		t.Fatalf("complete runs produced warnings: %v", comparison.Warnings)
	}
	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("No gating regression.")) ||
		!bytes.Contains(markdown, []byte("Exit code 0.")) {
		t.Fatalf("complete decision missing:\n%s", markdown)
	}
}

func TestCompareAbortedAndPartialRuns(t *testing.T) {
	t.Run("aborted", func(t *testing.T) {
		plans := completeComparePlans(t)
		source, runA, runB := compareFixture(t, plans, plans)
		source.runs[1].Aborted = true
		source.runs[1].AbortReason = "cost_cap"
		comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

		if comparison.B.Complete || !comparison.B.Aborted ||
			comparison.B.AbortReason != "cost_cap" {
			t.Fatalf("candidate metadata = %+v, want aborted cost_cap", comparison.B)
		}
		if !comparison.ExitCriteria.Incomplete {
			t.Fatalf("exit criteria = %+v, want incomplete", comparison.ExitCriteria)
		}
		if !containsWarning(comparison.Warnings, "aborted (cost_cap)") {
			t.Fatalf("warnings = %v, want aborted warning", comparison.Warnings)
		}
		markdown, err := RenderCompareMarkdown(comparison)
		if err != nil {
			t.Fatalf("RenderCompareMarkdown: %v", err)
		}
		if !bytes.Contains(markdown, []byte("| Aborted | no | yes (cost_cap) |")) {
			t.Fatalf("markdown missing aborted metadata:\n%s", markdown)
		}
	})

	t.Run("partial", func(t *testing.T) {
		plans := completeComparePlans(t)
		source, runA, runB := compareFixture(t, plans, plans)
		source.runs[1].EndedAt = nil
		comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

		if comparison.B.Complete || !comparison.B.Partial {
			t.Fatalf("candidate metadata = %+v, want partial", comparison.B)
		}
		if !comparison.ExitCriteria.Incomplete {
			t.Fatalf("exit criteria = %+v, want incomplete", comparison.ExitCriteria)
		}
		if !containsWarning(comparison.Warnings, "is partial: ended_at is null") {
			t.Fatalf("warnings = %v, want partial warning", comparison.Warnings)
		}
	})
}

func TestCompareSameRun(t *testing.T) {
	run := compareRunRow(t, fixtureRun0, "model-a", "v1")
	source := &fixtureSource{
		runs: []RunRow{run},
		rows: compareRows(t, run.ID,
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 9,
				CostUSD: "0.010000", LatencyMS: 100, Steps: 2}}),
	}
	comparison := buildComparison(t, source, run.ID, run.ID, CompareOptions{})

	if !containsWarning(comparison.Warnings, "comparing a run to itself") {
		t.Fatalf("warnings = %v", comparison.Warnings)
	}
	if comparison.A.Cases != 12 || comparison.B.Cases != 12 {
		t.Fatalf("cases = %+v", comparison.Overall.Cases)
	}
	if comparison.Overall.Accuracy.DeltaPP == nil || *comparison.Overall.Accuracy.DeltaPP != 0 {
		t.Fatalf("accuracy delta = %v", comparison.Overall.Accuracy.DeltaPP)
	}
	if comparison.Overall.Accuracy.Overlap == nil || !*comparison.Overall.Accuracy.Overlap {
		t.Fatalf("accuracy overlap = %v", comparison.Overall.Accuracy.Overlap)
	}
	if comparison.Overall.Accuracy.Classification != ClassificationNoChange {
		t.Fatalf("accuracy classification = %q",
			comparison.Overall.Accuracy.Classification)
	}
	if comparison.Cost.DeltaUSD == nil || *comparison.Cost.DeltaUSD != "0.000000" {
		t.Fatalf("cost delta = %v", comparison.Cost.DeltaUSD)
	}
	if comparison.ExitCriteria.Failed {
		t.Fatal("same run flagged as a gating regression")
	}
}

func TestCompareRepeatMemberUsesNamedRunOnly(t *testing.T) {
	_, manifest := devDataset(t)
	started, err := time.Parse(time.RFC3339, fixtureStartedAt)
	if err != nil {
		t.Fatalf("parse fixture time: %v", err)
	}
	ended := started.Add(10 * time.Minute)
	runs := make([]RunRow, 0, 3)
	rows := make([]CaseRow, 0)
	for index, id := range []string{fixtureRun0, fixtureRun1, fixtureRun2} {
		repeatIndex := index
		run := RunRow{
			ID:             id,
			Suite:          "agent",
			Split:          dataset.Dev,
			DatasetVersion: manifest.EvalDatasetVersion,
			Model:          "deepseek-v4.1-flash",
			PromptVersion:  "v1",
			Seed:           manifest.Seed,
			GitSHA:         fixtureGitSHA,
			StartedAt:      started,
			EndedAt:        &ended,
			RepeatGroup:    fixtureGroup,
			RepeatIndex:    &repeatIndex,
		}
		runs = append(runs, run)
		correct := 12 - index*3
		rows = append(rows, compareRows(t, run.ID, []comparePlan{{
			Mode: "fee_variance", Cases: 12, Correct: correct, LatencyMS: 100, Steps: 2,
		}})...)
	}
	source := &fixtureSource{runs: runs, rows: rows}
	comparison := buildComparison(t, source, fixtureRun0, fixtureRun1, CompareOptions{})

	if comparison.A.Cases != 12 || comparison.B.Cases != 12 {
		t.Fatalf("repeat member cases = %+v, want named runs only (12 each)", comparison.Overall.Cases)
	}
	if !containsWarning(comparison.Warnings, "repeat member") ||
		!containsWarning(comparison.Warnings, "comparing that run only") {
		t.Fatalf("warnings = %v", comparison.Warnings)
	}
	if comparison.A.RepeatGroup != fixtureGroup {
		t.Fatalf("A repeat group = %q", comparison.A.RepeatGroup)
	}
}

func TestCompareDescriptiveDeltas(t *testing.T) {
	source, runA, runB := compareFixture(t,
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 9,
			CostUSD: "0.010000", LatencyMS: 100, Steps: 2}},
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 9,
			CostUSD: "0.015000", LatencyMS: 140, Steps: 3}})
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	if comparison.Cost.AMeanUSD == nil || *comparison.Cost.AMeanUSD != "0.010000" {
		t.Fatalf("A cost mean = %v", comparison.Cost.AMeanUSD)
	}
	if comparison.Cost.BMeanUSD == nil || *comparison.Cost.BMeanUSD != "0.015000" {
		t.Fatalf("B cost mean = %v", comparison.Cost.BMeanUSD)
	}
	if comparison.Cost.DeltaUSD == nil || *comparison.Cost.DeltaUSD != "0.005000" {
		t.Fatalf("cost delta = %v", comparison.Cost.DeltaUSD)
	}
	if comparison.Cost.DeltaPct == nil || *comparison.Cost.DeltaPct != 50 {
		t.Fatalf("cost delta pct = %v, want 50", comparison.Cost.DeltaPct)
	}
	if comparison.Latency.P50DeltaMS == nil || *comparison.Latency.P50DeltaMS != 40 {
		t.Fatalf("p50 delta = %v, want 40", comparison.Latency.P50DeltaMS)
	}
	if comparison.Latency.P95DeltaMS == nil || *comparison.Latency.P95DeltaMS != 40 {
		t.Fatalf("p95 delta = %v, want 40", comparison.Latency.P95DeltaMS)
	}
	if comparison.Latency.P50DeltaPct == nil || *comparison.Latency.P50DeltaPct != 40 {
		t.Fatalf("p50 delta pct = %v, want 40", comparison.Latency.P50DeltaPct)
	}
	if comparison.Steps.DeltaSteps == nil || *comparison.Steps.DeltaSteps != "1.00" {
		t.Fatalf("steps delta = %v, want 1.00", comparison.Steps.DeltaSteps)
	}
}

func TestCompareCostZeroBaseline(t *testing.T) {
	source, runA, runB := compareFixture(t,
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 9,
			CostUSD: "0.000000", LatencyMS: 100, Steps: 2}},
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 9,
			CostUSD: "0.010000", LatencyMS: 100, Steps: 2}})
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	if comparison.Cost.DeltaUSD == nil || *comparison.Cost.DeltaUSD != "0.010000" {
		t.Fatalf("cost delta = %v", comparison.Cost.DeltaUSD)
	}
	if comparison.Cost.DeltaPct != nil {
		t.Fatalf("cost delta pct = %v, want null for a zero baseline", *comparison.Cost.DeltaPct)
	}
}

func TestCompareGatingMetricSet(t *testing.T) {
	t.Run("over-escalation is non-gating", func(t *testing.T) {
		source, runA, runB := compareFixture(t,
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12,
				Resolvable: true, LatencyMS: 100, Steps: 2}},
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12,
				Resolvable: true, Escalated: true, LatencyMS: 100, Steps: 2}})
		comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})
		overEscalation := comparison.Overall.OverEscalation
		if overEscalation.Classification != ClassificationRegression {
			t.Fatalf("over-escalation classification = %q",
				overEscalation.Classification)
		}
		if overEscalation.Gating {
			t.Fatal("over-escalation marked gating")
		}
		if comparison.ExitCriteria.Failed {
			t.Fatalf("over-escalation alone failed the gate: %+v",
				comparison.ExitCriteria.Regressions)
		}
	})

	t.Run("evidence validity gates", func(t *testing.T) {
		source, runA, runB := compareFixture(t,
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12,
				HasProposal: true, EvidenceValid: true, LatencyMS: 100, Steps: 2}},
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12,
				HasProposal: true, EvidenceValid: false, LatencyMS: 100, Steps: 2}})
		comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})
		if comparison.Overall.EvidenceValidity.Classification != ClassificationRegression {
			t.Fatalf("evidence classification = %q",
				comparison.Overall.EvidenceValidity.Classification)
		}
		assertRegression(t, comparison, "overall", "", "evidence_validity")
	})

	t.Run("correct escalation gates", func(t *testing.T) {
		source, runA, runB := compareFixture(t,
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12,
				AllowsEscalate: true, Escalated: true, LatencyMS: 100, Steps: 2}},
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12,
				AllowsEscalate: true, LatencyMS: 100, Steps: 2}})
		comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})
		if comparison.Overall.CorrectEscalation.Classification != ClassificationRegression {
			t.Fatalf("correct-escalation classification = %q",
				comparison.Overall.CorrectEscalation.Classification)
		}
		assertRegression(t, comparison, "overall", "", "correct_escalation")
	})

	t.Run("injection resistance is classified but non-gating", func(t *testing.T) {
		source, runA, runB := compareFixture(t,
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12, LatencyMS: 100, Steps: 2}},
			[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12, LatencyMS: 100, Steps: 2}})
		comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})
		if comparison.Overall.InjectionResistance.Gating {
			t.Fatal("injection resistance marked gating in M3-07")
		}
	})
}

func TestCompareMetadataMismatch(t *testing.T) {
	cases := map[string]struct {
		mutate func(*RunRow)
		want   string
	}{
		"split": {mutate: func(run *RunRow) { run.Split = dataset.Test }, want: "split"},
		"dataset_version": {
			mutate: func(run *RunRow) { run.DatasetVersion = "different" },
			want:   "dataset_version",
		},
		"suite": {mutate: func(run *RunRow) { run.Suite = "matcher" }, want: "suite"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			source, runA, runB := compareFixture(t,
				[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12, LatencyMS: 100, Steps: 2}},
				[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12, LatencyMS: 100, Steps: 2}})
			tc.mutate(&source.runs[1])
			_, err := Compare(context.Background(), source, runA.ID, runB.ID, CompareOptions{
				DatasetDir: devDatasetDir(t),
				Clock:      fixtureClock(t),
			})
			if !errors.Is(err, ErrCompareMismatch) {
				t.Fatalf("error = %v, want ErrCompareMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCompareReportOnlyMarksFooter(t *testing.T) {
	source, runA, runB := compareFixture(t,
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 12, LatencyMS: 100, Steps: 2}},
		[]comparePlan{{Mode: "fee_variance", Cases: 12, Correct: 6, FalseFix: 6,
			LatencyMS: 100, Steps: 2}})
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{ReportOnly: true})

	if !comparison.ExitCriteria.Failed {
		t.Fatal("report-only cleared the regression flag")
	}
	if !comparison.ExitCriteria.ReportOnly {
		t.Fatal("report_only = false in the model")
	}
	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("--report-only: exit suppressed")) {
		t.Fatalf("markdown missing report-only footer:\n%s", markdown)
	}
}

func TestGoldenCompare(t *testing.T) {
	plansA := completeComparePlans(t)
	plansB := transformComparePlans(plansA, func(plan comparePlan) comparePlan {
		plan.CostUSD = "0.020000"
		plan.LatencyMS = 150
		plan.Steps = 3
		return plan
	})
	plansA = replaceComparePlan(plansA, comparePlan{
		Mode: "fee_variance", Cases: 12, Correct: 12,
		CostUSD: "0.010000", LatencyMS: 100, Steps: 2,
	})
	plansB = replaceComparePlan(plansB, comparePlan{
		Mode: "fee_variance", Cases: 12, Correct: 6, FalseFix: 6,
		CostUSD: "0.020000", LatencyMS: 150, Steps: 3,
	})
	plansA = replaceComparePlan(plansA, comparePlan{
		Mode: "duplicate_charge", Cases: 12, Correct: 6, FalseFix: 6,
		CostUSD: "0.010000", LatencyMS: 100, Steps: 2,
	})
	plansB = replaceComparePlan(plansB, comparePlan{
		Mode: "duplicate_charge", Cases: 12, Correct: 12,
		CostUSD: "0.020000", LatencyMS: 150, Steps: 3,
	})
	plansA = replaceComparePlan(plansA, comparePlan{
		Mode: "partial_refund", Cases: 12, Correct: 9,
		CostUSD: "0.010000", LatencyMS: 100, Steps: 2,
	})
	plansB = replaceComparePlan(plansB, comparePlan{
		Mode: "partial_refund", Cases: 12, Correct: 9,
		CostUSD: "0.020000", LatencyMS: 150, Steps: 3,
	})
	source, runA, runB := compareFixture(t, plansA, plansB)
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	jsonBytes, err := RenderCompareJSON(comparison)
	if err != nil {
		t.Fatalf("RenderCompareJSON: %v", err)
	}
	if got := comparison.GeneratedAt.Format(time.RFC3339); got != fixtureFixedNow {
		t.Fatalf("generated_at = %q", got)
	}
	checkGolden(t, filepath.Join("testdata", "compare.md"), markdown)
	checkGolden(t, filepath.Join("testdata", "compare.json"), jsonBytes)
}

func TestCompareMarkdownJSONConsistency(t *testing.T) {
	plansA := []comparePlan{
		{Mode: "fee_variance", Cases: 12, Correct: 12, CostUSD: "0.010000", LatencyMS: 100, Steps: 2},
	}
	plansB := []comparePlan{
		{Mode: "fee_variance", Cases: 12, Correct: 6, FalseFix: 6,
			CostUSD: "0.020000", LatencyMS: 140, Steps: 3},
	}
	source, runA, runB := compareFixture(t, plansA, plansB)
	comparison := buildComparison(t, source, runA.ID, runB.ID, CompareOptions{})

	jsonBytes, err := RenderCompareJSON(comparison)
	if err != nil {
		t.Fatalf("RenderCompareJSON: %v", err)
	}
	var decoded Comparison
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("decode compare JSON: %v", err)
	}
	if decoded.Overall.Accuracy.Classification != ClassificationRegression ||
		decoded.Overall.FalseFix.Classification != ClassificationRegression {
		t.Fatalf("JSON classifications = accuracy %q false-fix %q",
			decoded.Overall.Accuracy.Classification,
			decoded.Overall.FalseFix.Classification)
	}
	if decoded.Cost.DeltaUSD == nil || *decoded.Cost.DeltaUSD != "0.010000" {
		t.Fatalf("JSON cost delta = %v", decoded.Cost.DeltaUSD)
	}
	markdown, err := RenderCompareMarkdown(comparison)
	if err != nil {
		t.Fatalf("RenderCompareMarkdown: %v", err)
	}
	for _, want := range []string{
		"| Accuracy | 1.0000 | 0.5000 | -50.00 |",
		"| False-fix | 0.0000 | 0.5000 | +50.00 |",
		"| Cost mean (USD) | 0.010000 | 0.020000 | 0.010000 | +100.00 |",
		"| Latency p50 (ms) | 100 | 140 | +40 | +40.00 |",
		"| Steps mean | 2.00 | 3.00 | 1.00 | — |",
		"| no | higher better | regression |",
		"| no | lower better | regression |",
	} {
		if !bytes.Contains(markdown, []byte(want)) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func assertRegression(t *testing.T, comparison Comparison, scope, mode, metric string) {
	t.Helper()
	for _, regression := range comparison.ExitCriteria.Regressions {
		if regression.Scope == scope && regression.Mode == mode && regression.Metric == metric {
			return
		}
	}
	t.Fatalf("missing regression scope=%q mode=%q metric=%q in %+v",
		scope, mode, metric, comparison.ExitCriteria.Regressions)
}

func containsWarning(warnings []string, fragment string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, fragment) {
			return true
		}
	}
	return false
}
