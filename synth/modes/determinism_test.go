package modes_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// TestDeterministicSameSeedAndMix pins D-7 for failure-mode datasets: the same
// seed, difficulty, and mix produce byte-identical files across two runs.
func TestDeterministicSameSeedAndMix(t *testing.T) {
	cfg := modes.DefaultConfig(18)
	first := generate(t, 808, 10, synth.DifficultyRealistic, cfg)
	second := generate(t, 808, 10, synth.DifficultyRealistic, cfg)
	firstHashes := hashFiles(first.Files)
	secondHashes := hashFiles(second.Files)
	if len(firstHashes) != len(secondHashes) {
		t.Fatalf("file counts differ: %d vs %d", len(firstHashes), len(secondHashes))
	}
	for _, path := range sortedPaths(first.Files) {
		if secondHashes[path] != firstHashes[path] {
			t.Errorf("%s is not byte-identical", path)
		}
	}
	if !reflect.DeepEqual(first.Manifest, second.Manifest) {
		t.Error("manifest differs between identical runs")
	}
	if !reflect.DeepEqual(first.Truth, second.Truth) {
		t.Error("ground truth differs between identical runs")
	}
}

// TestDatasetVersionIncludesMix proves a different mix or seed is a different
// dataset version.
func TestDatasetVersionIncludesMix(t *testing.T) {
	base := generate(t, 909, 8, synth.DifficultyRealistic, modes.DefaultConfig(12))
	otherMix := modes.Config{Total: 12, Counts: map[string]int{modes.FeeVariance: 12}}
	otherSeed := generate(t, 910, 8, synth.DifficultyRealistic, modes.DefaultConfig(12))
	mixed := generate(t, 909, 8, synth.DifficultyRealistic, otherMix)
	if base.Manifest.DatasetVersion == mixed.Manifest.DatasetVersion {
		t.Error("different mix produced the same dataset_version")
	}
	if base.Manifest.DatasetVersion == otherSeed.Manifest.DatasetVersion {
		t.Error("different seed produced the same dataset_version")
	}
}

// TestDifficultyChangesOnlyNoise proves difficulty changes only the documented
// noise knobs: scenarios, ledger transactions, and bank files stay identical.
func TestDifficultyChangesOnlyNoise(t *testing.T) {
	cfg := modes.DefaultConfig(12)
	easy := generate(t, 111, 10, synth.DifficultyEasy, cfg)
	realistic := generate(t, 111, 10, synth.DifficultyRealistic, cfg)
	adversarial := generate(t, 111, 10, synth.DifficultyAdversarial, cfg)

	if !reflect.DeepEqual(stripDifficulty(easy), stripDifficulty(realistic)) {
		t.Error("realistic changed scenario correctness")
	}
	if !reflect.DeepEqual(stripDifficulty(easy), stripDifficulty(adversarial)) {
		t.Error("adversarial changed scenario correctness")
	}
	if !reflect.DeepEqual(easy.Ledger.Transactions, realistic.Ledger.Transactions) {
		t.Error("realistic changed ledger transactions")
	}
	if !reflect.DeepEqual(easy.Ledger.Transactions, adversarial.Ledger.Transactions) {
		t.Error("adversarial changed ledger transactions")
	}
	if !reflect.DeepEqual(bankFiles(easy), bankFiles(realistic)) {
		t.Error("realistic changed bank_x files")
	}
	if !reflect.DeepEqual(bankFiles(easy), bankFiles(adversarial)) {
		t.Error("adversarial changed bank_x files")
	}

	if easy.Manifest.Noise != (modes.Noise{}) {
		t.Errorf("easy noise = %+v, want zero", easy.Manifest.Noise)
	}
	if realistic.Manifest.Noise != (modes.Noise{UnrelatedBreaks: 2, NearDuplicateAmounts: 2}) {
		t.Errorf("realistic noise = %+v", realistic.Manifest.Noise)
	}
	if adversarial.Manifest.Noise != (modes.Noise{UnrelatedBreaks: 6, NearDuplicateAmounts: 6}) {
		t.Errorf("adversarial noise = %+v", adversarial.Manifest.Noise)
	}
	if got := realistic.Manifest.Counts.ExternalRecords - easy.Manifest.Counts.ExternalRecords; got != 4 {
		t.Errorf("realistic external record delta = %d, want 4", got)
	}
	if got := adversarial.Manifest.Counts.ExternalRecords - easy.Manifest.Counts.ExternalRecords; got != 12 {
		t.Errorf("adversarial external record delta = %d, want 12", got)
	}

	easyRecords := parsePSPFiles(t, easy)
	realisticRecords := parsePSPFiles(t, realistic)
	adversarialRecords := parsePSPFiles(t, adversarial)
	if countNoise(easyRecords) != 0 {
		t.Errorf("easy has %d noise records", countNoise(easyRecords))
	}
	if countNoise(realisticRecords) != 4 {
		t.Errorf("realistic has %d noise records, want 4", countNoise(realisticRecords))
	}
	if countNoise(adversarialRecords) != 12 {
		t.Errorf("adversarial has %d noise records, want 12", countNoise(adversarialRecords))
	}
	assertNearDuplicates(t, realistic, realisticRecords)
	assertNearDuplicates(t, adversarial, adversarialRecords)
}

func stripDifficulty(result *modes.Result) []synth.Scenario {
	scenarios := make([]synth.Scenario, len(result.Truth.Scenarios))
	for i, scenario := range result.Truth.Scenarios {
		scenario.Difficulty = ""
		scenarios[i] = scenario
	}
	return scenarios
}

func bankFiles(result *modes.Result) map[string]string {
	files := make(map[string]string)
	for path, hash := range hashFiles(result.Files) {
		if strings.HasPrefix(path, synth.SourceBank+"/") {
			files[path] = hash
		}
	}
	return files
}

func countNoise(records map[string]pspRecord) int {
	count := 0
	for id := range records {
		if strings.HasPrefix(id, "noise_") {
			count++
		}
	}
	return count
}

func assertNearDuplicates(t *testing.T, result *modes.Result, records map[string]pspRecord) {
	t.Helper()
	near := 0
	for id, record := range records {
		if !strings.HasPrefix(id, "noise_") || !strings.Contains(id, "_near_") {
			continue
		}
		near++
		found := false
		for _, scenario := range result.Truth.Scenarios {
			for _, value := range scenario.Amounts {
				if value.Currency != record.Record.Currency {
					continue
				}
				delta := value.AmountMinor - record.Record.AmountMinor
				if delta < 0 {
					delta = -delta
				}
				if delta <= 2 {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("noise record %s amount %d is not near a scenario amount",
				id, record.Record.AmountMinor)
		}
	}
	want := result.Manifest.Noise.NearDuplicateAmounts
	if near != want {
		t.Errorf("near-duplicate noise rows = %d, want %d", near, want)
	}
}
