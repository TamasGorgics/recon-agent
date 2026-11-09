package component

import (
	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/synth"
)

// AdaptTruth converts eval ground truth (eval/dataset) into the synth ground
// truth type consumed by internal/matchscore. It is a pure field mapping: no
// resolution normalization, threshold, or matcher semantic is added here.
//
// Every truth scenario is included, including clean link rows, because the
// matcher scores clean pairs as well as break scenarios. Injection scenarios
// keep their ["escalate"] expected resolutions; matchscore excludes them from
// the recall denominator itself.
func AdaptTruth(datasetVersion string, truth dataset.GroundTruth) synth.GroundTruth {
	adapted := synth.GroundTruth{
		DatasetVersion: datasetVersion,
		Scenarios:      make([]synth.Scenario, 0, len(truth.Scenarios)),
	}
	for _, scenario := range truth.Scenarios {
		adapted.Scenarios = append(adapted.Scenarios, synth.Scenario{
			ScenarioID:          scenario.ScenarioID,
			Mode:                scenario.Mode,
			Difficulty:          scenario.Difficulty,
			BreakSignature:      scenario.BreakSignature,
			ExpectedResolutions: append([]string(nil), scenario.ExpectedResolutions...),
			PrimaryResolution:   scenario.PrimaryResolution,
			ExpectedTier:        scenario.ExpectedTier,
			Amounts:             append([]synth.Amount(nil), scenario.Amounts...),
			Accounts:            append([]string(nil), scenario.Accounts...),
			LedgerTxnIDs:        append([]string(nil), scenario.LedgerTxnIDs...),
			ExternalRecords:     append([]synth.RecordRef(nil), scenario.ExternalRecords...),
		})
	}
	return adapted
}

// ScoreableScenarios counts the eval-runner scoreable rows for a truth
// document: a non-empty break signature or an injection_memo scenario (Q-54).
// The matcher report reports this alongside the total truth count so the
// matcher denominator is not confused with the agent-eval denominator.
func ScoreableScenarios(truth dataset.GroundTruth) int {
	count := 0
	for _, scenario := range truth.Scenarios {
		if scenario.BreakSignature != "" || scenario.Mode == injectionMode {
			count++
		}
	}
	return count
}

const injectionMode = "injection_memo"
