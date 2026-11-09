package report

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/score"
)

func calibrationConfidence(t *testing.T, value string) decimal.Decimal {
	t.Helper()
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("parse confidence %q: %v", value, err)
	}
	return parsed
}

func assertFloat(t *testing.T, name string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("%s = %v, want %v (±%v)", name, got, want, tolerance)
	}
}

func TestCalibrateEmpty(t *testing.T) {
	calibration, err := Calibrate(nil)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if calibration.Applicable {
		t.Fatal("empty calibration is applicable")
	}
	if calibration.Target != CalibrationTarget || calibration.Bins != CalibrationBins {
		t.Fatalf("contract = %+v", calibration)
	}
	if calibration.N != 0 || calibration.ECE != nil {
		t.Fatalf("N=%d ECE=%v, want 0/nil", calibration.N, calibration.ECE)
	}
	if len(calibration.Reliability) != 0 {
		t.Fatalf("reliability = %v, want empty", calibration.Reliability)
	}
	if calibration.PerRunECE == nil {
		t.Fatal("per_run_ece is nil, want an empty array")
	}
}

func TestCalibratePerfect(t *testing.T) {
	observations := make([]CalibrationObservation, 0, 10)
	for i := 0; i < 10; i++ {
		observations = append(observations, CalibrationObservation{
			Confidence: calibrationConfidence(t, "0.5"),
			Correct:    i < 5,
		})
	}
	calibration, err := Calibrate(observations)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if !calibration.Applicable || calibration.N != 10 {
		t.Fatalf("calibration = %+v", calibration)
	}
	bin := calibration.Reliability[5]
	if bin.Count != 10 || bin.MeanConfidence == nil || bin.Accuracy == nil || bin.Gap == nil {
		t.Fatalf("bin = %+v", bin)
	}
	assertFloat(t, "mean confidence", *bin.MeanConfidence, 0.5, 1e-12)
	assertFloat(t, "accuracy", *bin.Accuracy, 0.5, 1e-12)
	assertFloat(t, "gap", *bin.Gap, 0, 1e-12)
	assertFloat(t, "ECE", *calibration.ECE, 0, 1e-12)
	for index, expected := range []string{
		"[0.0,0.1)", "[0.1,0.2)", "[0.2,0.3)", "[0.3,0.4)", "[0.4,0.5)",
		"[0.5,0.6)", "[0.6,0.7)", "[0.7,0.8)", "[0.8,0.9)", "[0.9,1.0]",
	} {
		if calibration.Reliability[index].Bin != expected {
			t.Fatalf("bin[%d] = %q, want %q", index,
				calibration.Reliability[index].Bin, expected)
		}
	}
	if calibration.Reliability[0].MeanConfidence != nil {
		t.Fatal("empty bin has a mean confidence")
	}
}

func TestCalibrateWorst(t *testing.T) {
	observations := []CalibrationObservation{
		{Confidence: calibrationConfidence(t, "1.0"), Correct: false},
		{Confidence: calibrationConfidence(t, "1.0"), Correct: false},
		{Confidence: calibrationConfidence(t, "1.0"), Correct: false},
	}
	calibration, err := Calibrate(observations)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	bin := calibration.Reliability[9]
	if bin.Count != 3 {
		t.Fatalf("last bin count = %d", bin.Count)
	}
	assertFloat(t, "last bin accuracy", *bin.Accuracy, 0, 1e-12)
	assertFloat(t, "last bin gap", *bin.Gap, 1, 1e-12)
	assertFloat(t, "ECE", *calibration.ECE, 1, 1e-12)
}

func TestCalibrateWeightedBins(t *testing.T) {
	observations := make([]CalibrationObservation, 0, 10)
	for i := 0; i < 9; i++ {
		observations = append(observations, CalibrationObservation{
			Confidence: calibrationConfidence(t, "0.05"),
			Correct:    false,
		})
	}
	observations = append(observations, CalibrationObservation{
		Confidence: calibrationConfidence(t, "1.0"),
		Correct:    false,
	})
	calibration, err := Calibrate(observations)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if calibration.N != 10 || calibration.Reliability[0].Count != 9 ||
		calibration.Reliability[9].Count != 1 {
		t.Fatalf("calibration = %+v", calibration)
	}
	// Weighted: 9/10 × 0.05 + 1/10 × 1.0. An unweighted gap mean would be
	// 0.525; ECE must be bin-count weighted.
	assertFloat(t, "ECE", *calibration.ECE, 0.145, 1e-12)
}

func TestCalibrateBoundaries(t *testing.T) {
	confidences := []string{"0.0", "0.09999", "0.1", "0.3", "0.9", "1.0"}
	observations := make([]CalibrationObservation, 0, len(confidences))
	for _, confidence := range confidences {
		observations = append(observations, CalibrationObservation{
			Confidence: calibrationConfidence(t, confidence),
			Correct:    false,
		})
	}
	calibration, err := Calibrate(observations)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	wantCounts := map[int]int{0: 2, 1: 1, 3: 1, 9: 2}
	for index, bin := range calibration.Reliability {
		if bin.Count != wantCounts[index] {
			t.Fatalf("bin %d (%s) count = %d, want %d", index, bin.Bin,
				bin.Count, wantCounts[index])
		}
	}
	if calibration.N != len(confidences) {
		t.Fatalf("N = %d", calibration.N)
	}
}

func TestCalibrationTargetCorrectNotFalseFix(t *testing.T) {
	// A false-fix row can still be resolution-correct in a fixture; the
	// calibration target is the Correct flag, never FalseFix.
	confidence := calibrationConfidence(t, "0.6")
	results := []score.CaseResult{{
		ScenarioID: "scenario-1",
		Correct:    true,
		FalseFix:   true,
		Confidence: &confidence,
	}}
	observations, err := calibrateResults(results)
	if err != nil {
		t.Fatalf("calibrateResults: %v", err)
	}
	calibration, err := Calibrate(observations)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	bin := calibration.Reliability[6]
	if bin.Count != 1 || bin.Accuracy == nil {
		t.Fatalf("bin = %+v", bin)
	}
	assertFloat(t, "accuracy", *bin.Accuracy, 1, 1e-12)
}

func TestCalibrateOutOfRangeConfidence(t *testing.T) {
	for _, value := range []string{"-0.1", "1.1"} {
		_, err := Calibrate([]CalibrationObservation{{
			Confidence: calibrationConfidence(t, value),
			Correct:    true,
		}})
		if err == nil {
			t.Fatalf("confidence %s did not fail", value)
		}
	}
}

func TestBuildCalibrationRepeatGroup(t *testing.T) {
	source := goldenFixture(t)
	built, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	calibration := built.Calibration
	if calibration == nil {
		t.Fatal("calibration is nil")
	}
	if !calibration.Applicable || calibration.Scope != CalibrationScopeRepeatGroup {
		t.Fatalf("calibration = %+v", calibration)
	}
	if calibration.N != 8 {
		t.Fatalf("N = %d, want 8", calibration.N)
	}
	assertFloat(t, "ECE", *calibration.ECE, 0.225, 1e-9)
	if got := calibration.Reliability[6].Count; got != 1 {
		t.Fatalf("bin 6 count = %d", got)
	}
	if got := calibration.Reliability[7].Count; got != 2 {
		t.Fatalf("bin 7 count = %d", got)
	}
	if got := calibration.Reliability[8].Count; got != 3 {
		t.Fatalf("bin 8 count = %d", got)
	}
	if got := calibration.Reliability[9].Count; got != 2 {
		t.Fatalf("bin 9 count = %d", got)
	}
	if len(calibration.PerRunECE) != 3 {
		t.Fatalf("per_run_ece = %+v", calibration.PerRunECE)
	}
	wantN := []int{3, 2, 3}
	wantECE := []float64{0.4, 0.15, 1.1 / 3}
	for index, perRun := range calibration.PerRunECE {
		if perRun.N != wantN[index] {
			t.Fatalf("per_run_ece[%d] N = %d, want %d", index, perRun.N, wantN[index])
		}
		if perRun.ECE == nil {
			t.Fatalf("per_run_ece[%d] ECE is nil", index)
		}
		assertFloat(t, "per-run ECE", *perRun.ECE, wantECE[index], 1e-9)
	}

	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	for _, want := range []string{
		"## Calibration",
		"Expected calibration error (ECE): 0.2250",
		"Per-run ECE:",
		"[0.9,1.0]",
	} {
		if !bytes.Contains(markdown, []byte(want)) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}

	jsonBytes, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	for _, want := range []string{
		`"calibration"`, `"target": "correct"`, `"bins": 10`,
		`"ece"`, `"reliability"`, `"scope": "repeat_group"`, `"per_run_ece"`,
	} {
		if !bytes.Contains(jsonBytes, []byte(want)) {
			t.Fatalf("JSON missing %q", want)
		}
	}
}

func TestBuildCalibrationNotApplicable(t *testing.T) {
	run := fixtureBaseRun(t, 0, 0)
	run.RepeatGroup = ""
	run.RepeatIndex = nil
	source := &fixtureSource{runs: []RunRow{run}}
	built, err := Build(context.Background(), source, run.ID, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	calibration := built.Calibration
	if calibration == nil {
		t.Fatal("calibration is nil")
	}
	if calibration.Applicable {
		t.Fatalf("zero-case calibration is applicable: %+v", calibration)
	}
	if calibration.Scope != CalibrationScopeRun {
		t.Fatalf("scope = %q", calibration.Scope)
	}
	if calibration.ECE != nil || len(calibration.Reliability) != 0 {
		t.Fatalf("zero-case calibration = %+v", calibration)
	}
	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("Not applicable: no proposal confidence observations")) {
		t.Fatalf("markdown missing not-applicable calibration:\n%s", markdown)
	}
	if bytes.Contains(markdown, []byte("| Bin | Count |")) {
		t.Fatalf("zero-case calibration rendered a table:\n%s", markdown)
	}
}

func TestVerifierScorecardPlaceholder(t *testing.T) {
	source := goldenFixture(t)
	built, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(built.Scorecards) != 1 {
		t.Fatalf("scorecards = %+v", built.Scorecards)
	}
	card := built.Scorecards[0]
	if card.Name != "verifier" || card.Applicable ||
		card.DeferredTo != "M4-05" || card.Cases != 0 ||
		card.Reason != "deterministic verifiers land in M4-05" {
		t.Fatalf("verifier scorecard = %+v", card)
	}
	if card.Metrics == nil {
		t.Fatal("scorecard metrics is nil, want an empty array")
	}
	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("verifier: not applicable until M4-05")) {
		t.Fatalf("markdown missing verifier placeholder:\n%s", markdown)
	}
}
