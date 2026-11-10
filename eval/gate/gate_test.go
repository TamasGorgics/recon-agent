package gate

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/report"
)

func findResult(t *testing.T, document Document, id string) Result {
	t.Helper()
	for _, result := range document.Results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("result %q missing", id)
	return Result{}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func floatPtr(value float64) *float64 { return &value }

func TestGateSelfPass(t *testing.T) {
	thresholds := committedThresholds(t)
	src, baseline, _ := completeGateSource(t)
	document, err := Run(context.Background(), src, baseline.ID, baseline.ID, thresholds,
		Options{
			DatasetDir:     testSplitDir(t),
			Clock:          gateClock(t),
			ThresholdsPath: "eval/thresholds.yaml",
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if document.Schema != DocumentSchema {
		t.Fatalf("schema = %q", document.Schema)
	}
	if document.Exit.Code != 0 {
		t.Fatalf("exit = %d reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	if document.Exit.Incomplete {
		t.Fatal("self-gate reported incomplete")
	}
	if len(document.Results) != 5 {
		t.Fatalf("results = %d, want 5", len(document.Results))
	}
	t0 := findResult(t, document, ThresholdT0FalseFix)
	if t0.Status != StatusNotApplicable || t0.Applicable {
		t.Fatalf("t0 = %+v, want not_applicable/applicable=false", t0)
	}
	if !strings.Contains(t0.Reason, "M4-14") {
		t.Fatalf("t0 reason = %q", t0.Reason)
	}
	if contains(document.Exit.PassedIDs, ThresholdT0FalseFix) {
		t.Fatal("T0 is counted as a pass")
	}
	injection := findResult(t, document, ThresholdInjection)
	if injection.Status != StatusNotApplicable || injection.Applicable {
		t.Fatalf("injection on test = %+v, want not_applicable", injection)
	}
	if !strings.Contains(injection.Reason, "M6-02/M6-05") {
		t.Fatalf("injection reason = %q", injection.Reason)
	}
	if document.Calibration.Gated {
		t.Fatal("calibration must not be gated")
	}
	if !document.Calibration.Baseline.Applicable || document.Calibration.Baseline.N == 0 {
		t.Fatalf("baseline calibration = %+v", document.Calibration.Baseline)
	}
	if document.Calibration.Baseline.ECE == nil {
		t.Fatal("baseline ECE is nil")
	}
	if document.Baseline.Cases == 0 || !document.Baseline.Complete {
		t.Fatalf("baseline metadata = %+v", document.Baseline)
	}
}

func TestGatePassesSameCombination(t *testing.T) {
	thresholds := committedThresholds(t)
	src, baseline, candidate := completeGateSource(t)
	document, err := Run(context.Background(), src, candidate.ID, baseline.ID, thresholds,
		Options{DatasetDir: testSplitDir(t), Clock: gateClock(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if document.Exit.Code != 0 {
		t.Fatalf("exit = %d reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	if len(document.Exit.FailedIDs) != 0 {
		t.Fatalf("failed IDs = %v", document.Exit.FailedIDs)
	}
}

func TestGateRejectsNonTestSplit(t *testing.T) {
	thresholds := committedThresholds(t)
	cases := map[string]struct {
		baseline  dataset.Split
		candidate dataset.Split
		wantSide  string
	}{
		"dev vs dev":    {baseline: dataset.Dev, candidate: dataset.Dev, wantSide: "baseline run"},
		"dev baseline":  {baseline: dataset.Dev, candidate: dataset.Test, wantSide: "baseline run"},
		"dev candidate": {baseline: dataset.Test, candidate: dataset.Dev, wantSide: "candidate run"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			src, baseline, candidate := completeGateSource(t)
			src.runs[0].Split = testCase.baseline
			src.runs[1].Split = testCase.candidate
			_, err := Run(context.Background(), src, candidate.ID, baseline.ID, thresholds,
				Options{DatasetDir: testSplitDir(t), Clock: gateClock(t)})
			if !errors.Is(err, ErrMetadataMismatch) {
				t.Fatalf("err = %v, want ErrMetadataMismatch", err)
			}
			message := err.Error()
			for _, want := range []string{
				"split", testCase.wantSide, "thresholds.baseline_split", `"test"`, `"dev"`,
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("error %q does not contain %q", message, want)
				}
			}
		})
	}
}

func TestGateT0NeverPasses(t *testing.T) {
	document := EvaluateComparison(passingComparison(), committedThresholds(t), false)
	t0 := findResult(t, document, ThresholdT0FalseFix)
	if t0.Status != StatusNotApplicable {
		t.Fatalf("status = %q", t0.Status)
	}
	if t0.Applicable {
		t.Fatal("T0 marked applicable")
	}
	if !strings.Contains(t0.Reason, "M4-14") {
		t.Fatalf("reason = %q", t0.Reason)
	}
	if contains(document.Exit.PassedIDs, ThresholdT0FalseFix) {
		t.Fatal("T0 appeared in passed IDs")
	}
}

func TestGateFalseFixRegression(t *testing.T) {
	document := EvaluateComparison(failingFalseFixComparison(), committedThresholds(t), false)
	if document.Exit.Code != 1 {
		t.Fatalf("exit = %d, want 1; reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	result := findResult(t, document, ThresholdOverallFalseFix)
	if result.Status != StatusFail {
		t.Fatalf("status = %q", result.Status)
	}
	if result.Classification != report.ClassificationRegression {
		t.Fatalf("classification = %q", result.Classification)
	}
	if !contains(document.Exit.FailedIDs, ThresholdOverallFalseFix) {
		t.Fatalf("failed IDs = %v", document.Exit.FailedIDs)
	}
	if result.DeltaPP == nil || *result.DeltaPP <= 0 {
		t.Fatalf("delta_pp = %v", result.DeltaPP)
	}

	reportOnly := EvaluateComparison(failingFalseFixComparison(),
		committedThresholds(t), true)
	if reportOnly.Exit.Code != 0 {
		t.Fatalf("report-only exit = %d, want 0", reportOnly.Exit.Code)
	}
	if !contains(reportOnly.Exit.FailedIDs, ThresholdOverallFalseFix) {
		t.Fatalf("report-only failed IDs = %v", reportOnly.Exit.FailedIDs)
	}
	if !reportOnly.Exit.ReportOnly {
		t.Fatal("report-only marker missing")
	}
}

// TestGateFalseFixRegressionThroughCompare exercises the real report.Compare
// classification over complete DB-row fixtures rather than a hand-built
// CompareRate.
func TestGateFalseFixRegressionThroughCompare(t *testing.T) {
	thresholds := committedThresholds(t)
	src, baseline, candidate := completeGateSource(t)
	changed := 0
	for index := range src.rows {
		if src.rows[index].RunID != candidate.ID || changed >= 10 {
			continue
		}
		src.rows[index].FalseFix = true
		src.rows[index].Correct = false
		changed++
	}
	if changed != 10 {
		t.Fatalf("mutated %d candidate rows, want 10", changed)
	}

	document, err := Run(context.Background(), src, candidate.ID, baseline.ID, thresholds,
		Options{DatasetDir: testSplitDir(t), Clock: gateClock(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if document.Exit.Code != 1 {
		t.Fatalf("exit = %d, want 1; reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	result := findResult(t, document, ThresholdOverallFalseFix)
	if result.Status != StatusFail {
		t.Fatalf("status = %q, want fail", result.Status)
	}
	if result.Classification != report.ClassificationRegression {
		t.Fatalf("classification = %q, want regression", result.Classification)
	}
	if !contains(document.Exit.FailedIDs, ThresholdOverallFalseFix) {
		t.Fatalf("failed IDs = %v", document.Exit.FailedIDs)
	}
}

func TestGateAccuracyBoundary(t *testing.T) {
	thresholds := committedThresholds(t)

	atBoundary := passingComparison()
	atBoundary.Overall.Accuracy = gateRate("accuracy", true, 80, 100, 78, 100)
	document := EvaluateComparison(atBoundary, thresholds, false)
	result := findResult(t, document, ThresholdAccuracy)
	if document.Exit.Code != 0 {
		t.Fatalf("exactly -2.00pp exit = %d reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	if result.Status != StatusPass {
		t.Fatalf("exactly -2.00pp status = %q", result.Status)
	}
	if result.DeltaPP == nil || *result.DeltaPP != -2.0 {
		t.Fatalf("delta_pp = %v", result.DeltaPP)
	}

	worse := passingComparison()
	worse.Overall.Accuracy = gateRate("accuracy", true, 80, 100, 77, 100)
	document = EvaluateComparison(worse, thresholds, false)
	result = findResult(t, document, ThresholdAccuracy)
	if document.Exit.Code != 1 {
		t.Fatalf("-3.00pp exit = %d, want 1", document.Exit.Code)
	}
	if result.Status != StatusFail {
		t.Fatalf("-3.00pp status = %q", result.Status)
	}
	if !contains(document.Exit.FailedIDs, ThresholdAccuracy) {
		t.Fatalf("failed IDs = %v", document.Exit.FailedIDs)
	}
}

func TestGateInjectionSplit(t *testing.T) {
	thresholds := committedThresholds(t)

	injection := baseComparison()
	injection.A.Split = dataset.Injection
	injection.B.Split = dataset.Injection
	injection.Overall.Accuracy = gateRate("accuracy", true, 0, 0, 0, 0)
	injection.Overall.FalseFix = gateRate("false_fix", false, 0, 30, 0, 30)
	injection.Overall.InjectionResistance = gateRate("injection_resistance", true, 30, 30, 28, 30)
	document := EvaluateComparison(injection, thresholds, false)
	if document.Exit.Code != 1 {
		t.Fatalf("exit = %d, want 1; reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	result := findResult(t, document, ThresholdInjection)
	if result.Status != StatusFail || !result.Applicable {
		t.Fatalf("injection result = %+v", result)
	}
	if !contains(document.Exit.FailedIDs, ThresholdInjection) {
		t.Fatalf("failed IDs = %v", document.Exit.FailedIDs)
	}

	onTest := passingComparison()
	onTest.Overall.InjectionResistance = gateRate("injection_resistance", true, 30, 30, 28, 30)
	document = EvaluateComparison(onTest, thresholds, false)
	result = findResult(t, document, ThresholdInjection)
	if document.Exit.Code != 0 {
		t.Fatalf("test-split exit = %d, want 0; reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	if result.Status != StatusNotApplicable || result.Applicable {
		t.Fatalf("test-split injection = %+v", result)
	}
	if !strings.Contains(result.Reason, "dedicated injection split") {
		t.Fatalf("reason = %q", result.Reason)
	}
}

func TestGateUndefinedRequiredMetric(t *testing.T) {
	comparison := passingComparison()
	comparison.Overall.FalseFix = gateRate("false_fix", false, 0, 0, 0, 0)
	comparison.Overall.Accuracy = gateRate("accuracy", true, 0, 0, 0, 0)
	document := EvaluateComparison(comparison, committedThresholds(t), false)
	if document.Exit.Code != 1 {
		t.Fatalf("exit = %d, want 1; reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	for _, id := range []string{ThresholdOverallFalseFix, ThresholdAccuracy} {
		result := findResult(t, document, id)
		if result.Status != StatusError {
			t.Fatalf("%s status = %q, want error", id, result.Status)
		}
		if !contains(document.Exit.ErrorIDs, id) {
			t.Fatalf("%s missing from error IDs: %v", id, document.Exit.ErrorIDs)
		}
		if contains(document.Exit.PassedIDs, id) {
			t.Fatalf("%s counted as pass", id)
		}
	}
}

func TestGateCalibrationNonGating(t *testing.T) {
	document := Evaluate(Input{
		Comparison: passingComparison(),
		BaselineCalibration: &report.Calibration{
			Applicable: true, Target: report.CalibrationTarget, Bins: report.CalibrationBins,
			N: 100, ECE: floatPtr(0.01),
		},
		CandidateCalibration: &report.Calibration{
			Applicable: true, Target: report.CalibrationTarget, Bins: report.CalibrationBins,
			N: 100, ECE: floatPtr(0.90),
		},
		Thresholds: committedThresholds(t),
	})
	if document.Exit.Code != 0 {
		t.Fatalf("exit = %d, want 0; reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	if document.Calibration.Gated {
		t.Fatal("calibration gated")
	}
	if document.Calibration.Candidate.ECE == nil ||
		*document.Calibration.Candidate.ECE != 0.90 {
		t.Fatalf("candidate ECE = %v", document.Calibration.Candidate.ECE)
	}
	result := findResult(t, document, ThresholdCalibration)
	if result.Status != StatusNotApplicable || result.Applicable {
		t.Fatalf("calibration result = %+v", result)
	}
}

func TestGateIncomplete(t *testing.T) {
	thresholds := committedThresholds(t)
	src, baseline, candidate := completeGateSource(t)
	kept := make([]report.CaseRow, 0, len(src.rows))
	dropped := false
	for _, row := range src.rows {
		if row.RunID == candidate.ID {
			if !dropped {
				dropped = true
				continue
			}
		}
		kept = append(kept, row)
	}
	src.rows = kept

	document, err := Run(context.Background(), src, candidate.ID, baseline.ID, thresholds,
		Options{DatasetDir: testSplitDir(t), Clock: gateClock(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if document.Exit.Code != 2 {
		t.Fatalf("exit = %d, want 2; reasons=%v", document.Exit.Code, document.Exit.Reasons)
	}
	if !document.Exit.Incomplete {
		t.Fatal("exit.incomplete = false")
	}
	if !contains(document.Exit.IncompleteRuns, candidate.ID) {
		t.Fatalf("incomplete runs = %v", document.Exit.IncompleteRuns)
	}
	markdown, err := RenderMarkdown(document)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("cannot certify an incomplete run")) {
		t.Fatalf("markdown missing incomplete marker:\n%s", markdown)
	}

	reportOnly, err := Run(context.Background(), src, candidate.ID, baseline.ID, thresholds,
		Options{DatasetDir: testSplitDir(t), Clock: gateClock(t), ReportOnly: true})
	if err != nil {
		t.Fatalf("Run report-only: %v", err)
	}
	if reportOnly.Exit.Code != 0 {
		t.Fatalf("report-only exit = %d, want 0", reportOnly.Exit.Code)
	}
	if !reportOnly.Exit.ReportOnly || !reportOnly.Exit.Incomplete {
		t.Fatalf("report-only exit = %+v", reportOnly.Exit)
	}
}

func TestGateMetadataMismatch(t *testing.T) {
	thresholds := committedThresholds(t)
	cases := map[string]struct {
		mutate    func(*report.RunRow, *report.RunRow)
		field     string
		baseline  string
		candidate string
	}{
		"suite": {
			mutate: func(_, candidate *report.RunRow) { candidate.Suite = "matcher" },
			field:  "suite", baseline: "agent", candidate: "matcher",
		},
		"split": {
			mutate: func(_, candidate *report.RunRow) { candidate.Split = dataset.Dev },
			field:  "split", baseline: "test", candidate: "dev",
		},
		"dataset version": {
			mutate: func(_, candidate *report.RunRow) { candidate.DatasetVersion = "other" },
			field:  "dataset_version", baseline: "", candidate: "other",
		},
		"seed": {
			mutate: func(_, candidate *report.RunRow) { candidate.Seed = 3 },
			field:  "seed", baseline: "2", candidate: "3",
		},
		"model": {
			mutate: func(_, candidate *report.RunRow) { candidate.Model = "glm-5.3-flash" },
			field:  "model", baseline: gateModel, candidate: "glm-5.3-flash",
		},
		"prompt": {
			mutate: func(_, candidate *report.RunRow) { candidate.PromptVersion = "v1" },
			field:  "prompt_version", baseline: gatePrompt, candidate: "v1",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			src, baseline, candidate := completeGateSource(t)
			testCase.mutate(&src.runs[0], &src.runs[1])
			_, err := Run(context.Background(), src, candidate.ID, baseline.ID, thresholds,
				Options{DatasetDir: testSplitDir(t), Clock: gateClock(t)})
			if !errors.Is(err, ErrMetadataMismatch) {
				t.Fatalf("err = %v, want ErrMetadataMismatch", err)
			}
			message := err.Error()
			if !strings.Contains(message, testCase.field) {
				t.Fatalf("error %q does not name field %q", message, testCase.field)
			}
			if testCase.baseline != "" && !strings.Contains(message, testCase.baseline) {
				t.Fatalf("error %q does not name baseline %q", message, testCase.baseline)
			}
			if !strings.Contains(message, testCase.candidate) {
				t.Fatalf("error %q does not name candidate %q", message, testCase.candidate)
			}
		})
	}
}

func TestGateRepeatGroupRejected(t *testing.T) {
	thresholds := committedThresholds(t)
	for name, apply := range map[string]func(*gateFixtureSource){
		"baseline":  func(src *gateFixtureSource) { src.runs[0].RepeatGroup = "group-id" },
		"candidate": func(src *gateFixtureSource) { src.runs[1].RepeatGroup = "group-id" },
	} {
		t.Run(name, func(t *testing.T) {
			src, baseline, candidate := completeGateSource(t)
			apply(src)
			_, err := Run(context.Background(), src, candidate.ID, baseline.ID, thresholds,
				Options{DatasetDir: testSplitDir(t), Clock: gateClock(t)})
			if !errors.Is(err, ErrMetadataMismatch) {
				t.Fatalf("err = %v, want ErrMetadataMismatch", err)
			}
			if !strings.Contains(err.Error(), "repeat-group") {
				t.Fatalf("error = %q", err)
			}
		})
	}
}

// covers: FIN-43
func TestGateDeterministicDocument(t *testing.T) {
	input := Input{
		Comparison:     passingComparison(),
		Thresholds:     committedThresholds(t),
		ThresholdsPath: "eval/thresholds.yaml",
	}
	first := Evaluate(input)
	second := Evaluate(input)
	firstJSON, err := RenderJSON(first)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	secondJSON, err := RenderJSON(second)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("JSON is not deterministic")
	}
	firstMD, err := RenderMarkdown(first)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	secondMD, err := RenderMarkdown(second)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !bytes.Equal(firstMD, secondMD) {
		t.Fatal("Markdown is not deterministic")
	}
	if !bytes.Contains(firstJSON, []byte(`"generated_at": "2026-11-10T00:00:00Z"`)) {
		t.Fatalf("generated_at not pinned by the injected clock:\n%s", firstJSON)
	}
	for _, marker := range []string{
		"not applicable until M4-14",
		"Injection resistance: n/a on test",
		"Calibration is reported, not gated",
	} {
		if !bytes.Contains(firstMD, []byte(marker)) {
			t.Fatalf("markdown missing %q:\n%s", marker, firstMD)
		}
	}
}
