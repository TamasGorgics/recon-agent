package report

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/score"
)

// Calibration contract from spec §6.4.
const (
	// CalibrationTarget is the correctness target; false-fix is a separate
	// headline metric and never the calibration target.
	CalibrationTarget = "correct"
	// CalibrationBins is the number of equal-width confidence bins.
	CalibrationBins = 10
	// CalibrationScopeRun is a single-run calibration.
	CalibrationScopeRun = "run"
	// CalibrationScopeRepeatGroup pools observations across a repeat group.
	CalibrationScopeRepeatGroup = "repeat_group"
)

// Calibration is the eval-report/v1 calibration section. It covers only
// proposal rows with a non-null confidence; escalation and infrastructure rows
// have no confidence and are excluded. Calibration is descriptive and never
// gates.
type Calibration struct {
	Applicable  bool                `json:"applicable"`
	Target      string              `json:"target"`
	Bins        int                 `json:"bins"`
	N           int                 `json:"n"`
	ECE         *float64            `json:"ece"`
	Reliability []CalibrationBin    `json:"reliability"`
	Scope       string              `json:"scope"`
	PerRunECE   []PerRunCalibration `json:"per_run_ece"`
}

// CalibrationBin is one equal-width confidence bin. Empty bins carry a null
// mean confidence, accuracy and gap, never a misleading zero.
type CalibrationBin struct {
	Bin            string   `json:"bin"`
	Count          int      `json:"count"`
	MeanConfidence *float64 `json:"mean_confidence"`
	Accuracy       *float64 `json:"accuracy"`
	Gap            *float64 `json:"gap"`
}

// PerRunCalibration is one repeat member's ECE. N may be zero, in which case
// ECE is null.
type PerRunCalibration struct {
	RunID       string   `json:"run_id"`
	RepeatIndex int      `json:"repeat_index"`
	N           int      `json:"n"`
	ECE         *float64 `json:"ece"`
}

// CalibrationObservation is one proposal confidence observation. Correct is
// resolution correctness (eval_case_results.correct), never false-fix.
type CalibrationObservation struct {
	Confidence decimal.Decimal
	Correct    bool
}

// Calibrate builds the 10-bin reliability table and ECE. Bin assignment uses
// decimal arithmetic (floor(confidence × 10), clamped to the last bin); float64
// is used only for display values and ECE. An empty observation set is not
// applicable, with a null ECE and an empty reliability table.
func Calibrate(observations []CalibrationObservation) (Calibration, error) {
	calibration := Calibration{
		Target:      CalibrationTarget,
		Bins:        CalibrationBins,
		Reliability: []CalibrationBin{},
		PerRunECE:   []PerRunCalibration{},
	}
	type binAccumulator struct {
		count      int
		correct    int
		confidence decimal.Decimal
	}
	var bins [CalibrationBins]binAccumulator
	for _, observation := range observations {
		if observation.Confidence.IsNegative() ||
			observation.Confidence.GreaterThan(decimal.NewFromInt(1)) {
			return Calibration{}, fmt.Errorf(
				"eval/report: calibration confidence %s is outside [0, 1]",
				observation.Confidence)
		}
		index := calibrationBin(observation.Confidence)
		bins[index].count++
		bins[index].confidence = bins[index].confidence.Add(observation.Confidence)
		if observation.Correct {
			bins[index].correct++
		}
		calibration.N++
	}
	if calibration.N == 0 {
		return calibration, nil
	}
	calibration.Applicable = true
	ece := 0.0
	total := float64(calibration.N)
	for index := range bins {
		bin := CalibrationBin{Bin: calibrationBinLabel(index), Count: bins[index].count}
		if bins[index].count == 0 {
			calibration.Reliability = append(calibration.Reliability, bin)
			continue
		}
		count := float64(bins[index].count)
		meanConfidence := roundCalibration(bins[index].confidence.
			Div(decimal.NewFromInt(int64(bins[index].count))).InexactFloat64())
		accuracy := roundCalibration(float64(bins[index].correct) / count)
		gap := roundCalibration(math.Abs(accuracy - meanConfidence))
		bin.MeanConfidence = &meanConfidence
		bin.Accuracy = &accuracy
		bin.Gap = &gap
		ece += (count / total) * gap
		calibration.Reliability = append(calibration.Reliability, bin)
	}
	calibration.ECE = ptrRoundCalibration(ece)
	return calibration, nil
}

// roundCalibration strips binary float noise from display values and ECE while
// preserving far more precision than the report renders. It shares the report
// canonicalization so calibration values are as portable as the rate views.
func roundCalibration(value float64) float64 {
	return canonicalFloat(value)
}

func ptrRoundCalibration(value float64) *float64 {
	rounded := roundCalibration(value)
	return &rounded
}

// calibrateResults extracts proposal observations from reconstructed case
// results. Rows without a proposal/confidence are skipped; correctness is the
// row's Correct flag, not FalseFix.
func calibrateResults(results []score.CaseResult) ([]CalibrationObservation, error) {
	observations := make([]CalibrationObservation, 0, len(results))
	for _, result := range results {
		if result.Confidence == nil {
			continue
		}
		if result.Confidence.IsNegative() ||
			result.Confidence.GreaterThan(decimal.NewFromInt(1)) {
			return nil, fmt.Errorf(
				"eval/report: scenario %s confidence %s is outside [0, 1]",
				result.ScenarioID, result.Confidence)
		}
		observations = append(observations, CalibrationObservation{
			Confidence: *result.Confidence,
			Correct:    result.Correct,
		})
	}
	return observations, nil
}

// buildCalibration derives the report section from the run data. A repeat
// group pools observations for the reliability table and lists per-run ECE;
// a single run has scope "run" and no per-run entries.
func buildCalibration(runData []runReport) (*Calibration, error) {
	repeatGroup := len(runData) > 1 ||
		(len(runData) == 1 && runData[0].run.RepeatGroup != "")
	scope := CalibrationScopeRun
	if repeatGroup {
		scope = CalibrationScopeRepeatGroup
	}
	all := make([]CalibrationObservation, 0)
	perRun := make([]PerRunCalibration, 0, len(runData))
	for _, data := range runData {
		observations, err := calibrateResults(data.results)
		if err != nil {
			return nil, err
		}
		all = append(all, observations...)
		if repeatGroup {
			runCalibration, err := Calibrate(observations)
			if err != nil {
				return nil, err
			}
			perRun = append(perRun, PerRunCalibration{
				RunID:       data.run.ID,
				RepeatIndex: repeatIndexValue(data.run),
				N:           runCalibration.N,
				ECE:         runCalibration.ECE,
			})
		}
	}
	calibration, err := Calibrate(all)
	if err != nil {
		return nil, err
	}
	calibration.Scope = scope
	if repeatGroup {
		calibration.PerRunECE = perRun
	}
	return &calibration, nil
}

// calibrationBin returns the equal-width bin index for a confidence in [0, 1].
// Decimal arithmetic keeps 0.1, 0.3 and 0.9 in the intended bins; 1.0 is
// clamped into the last bin.
func calibrationBin(confidence decimal.Decimal) int {
	index := int(confidence.Mul(decimal.NewFromInt(CalibrationBins)).Floor().IntPart())
	if index < 0 {
		return 0
	}
	if index > CalibrationBins-1 {
		return CalibrationBins - 1
	}
	return index
}

func calibrationBinLabel(index int) string {
	lower := float64(index) / float64(CalibrationBins)
	if index == CalibrationBins-1 {
		return fmt.Sprintf("[%.1f,1.0]", lower)
	}
	return fmt.Sprintf("[%.1f,%.1f)", lower, lower+1.0/float64(CalibrationBins))
}
