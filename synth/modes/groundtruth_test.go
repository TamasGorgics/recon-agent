package modes_test

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// TestGroundTruthSchemaAndManifestCounts validates every scenario shape, the
// one-scenario-per-record invariant, and that manifest counts equal the
// generated scenarios.
func TestGroundTruthSchemaAndManifestCounts(t *testing.T) {
	cfg := modes.DefaultConfig(24)
	result := generate(t, 707, modeTestDays, synth.DifficultyEasy, cfg)
	records := parsePSPFiles(t, result)
	transactions := txnIndex(result)

	seenScenarios := make(map[string]struct{}, len(result.Truth.Scenarios))
	seenExternal := make(map[string]string)
	modeActual := make(map[string]int)
	totalLinks := 0
	for _, scenario := range result.Truth.Scenarios {
		if scenario.Mode == modes.CleanMode {
			if scenario.BreakSignature != "" {
				t.Errorf("clean scenario %s has signature %q", scenario.ScenarioID, scenario.BreakSignature)
			}
			if !sameStrings(scenario.ExpectedResolutions, []string{"link"}) {
				t.Errorf("clean scenario %s resolutions = %v", scenario.ScenarioID, scenario.ExpectedResolutions)
			}
		} else {
			assertFailureScenarioSchema(t, scenario)
		}
		modeActual[scenario.Mode]++
		totalLinks += len(scenario.ExternalRecords)
		if _, duplicate := seenScenarios[scenario.ScenarioID]; duplicate {
			t.Errorf("duplicate scenario_id %s", scenario.ScenarioID)
		}
		seenScenarios[scenario.ScenarioID] = struct{}{}
		for _, ref := range scenario.ExternalRecords {
			if ref.Source != synth.SourcePSP {
				continue
			}
			if _, ok := records[ref.SourceRecordID]; !ok {
				t.Errorf("scenario %s references missing record %s",
					scenario.ScenarioID, ref.SourceRecordID)
			}
			key := ref.Source + "|" + ref.SourceRecordID
			if previous, ok := seenExternal[key]; ok {
				t.Errorf("record %s is in scenarios %s and %s", key, previous, scenario.ScenarioID)
			}
			seenExternal[key] = scenario.ScenarioID
		}
		for _, id := range scenario.LedgerTxnIDs {
			if _, ok := transactions[id]; !ok {
				t.Errorf("scenario %s references missing ledger txn %s", scenario.ScenarioID, id)
			}
		}
	}

	for _, name := range modes.PartAModes {
		if modeActual[name] != 4 {
			t.Errorf("mode %s has %d scenarios, want 4", name, modeActual[name])
		}
		if result.Manifest.ModeCounts[name] != modeActual[name] {
			t.Errorf("manifest mode_counts[%s] = %d, scenarios = %d",
				name, result.Manifest.ModeCounts[name], modeActual[name])
		}
	}
	if result.Manifest.ModeCounts[modes.CleanMode] != modeActual[modes.CleanMode] {
		t.Errorf("manifest clean count = %d, scenarios = %d",
			result.Manifest.ModeCounts[modes.CleanMode], modeActual[modes.CleanMode])
	}
	modeSum := 0
	for _, count := range result.Manifest.ModeCounts {
		modeSum += count
	}
	if modeSum != len(result.Truth.Scenarios) {
		t.Errorf("mode count sum = %d, scenarios = %d", modeSum, len(result.Truth.Scenarios))
	}
	difficultySum := 0
	for _, count := range result.Manifest.DifficultyCounts {
		difficultySum += count
	}
	if difficultySum != len(result.Truth.Scenarios) {
		t.Errorf("difficulty count sum = %d, scenarios = %d",
			difficultySum, len(result.Truth.Scenarios))
	}
	if result.Manifest.DifficultyCounts[synth.DifficultyEasy] != len(result.Truth.Scenarios) {
		t.Errorf("difficulty_counts = %v", result.Manifest.DifficultyCounts)
	}
	if result.Manifest.Counts.GroundTruthLinks != totalLinks {
		t.Errorf("manifest ground_truth_links = %d, scenarios link %d",
			result.Manifest.Counts.GroundTruthLinks, totalLinks)
	}
	if result.Manifest.Counts.LedgerTxns != len(result.Ledger.Transactions) {
		t.Errorf("manifest ledger_txns = %d, ledger has %d",
			result.Manifest.Counts.LedgerTxns, len(result.Ledger.Transactions))
	}
	if result.Manifest.DatasetVersion == "" ||
		result.Manifest.DatasetVersion != result.Truth.DatasetVersion ||
		result.Manifest.DatasetVersion != result.Ledger.DatasetVersion {
		t.Errorf("dataset_version mismatch: manifest %q truth %q ledger %q",
			result.Manifest.DatasetVersion, result.Truth.DatasetVersion, result.Ledger.DatasetVersion)
	}
	if _, err := hex.DecodeString(result.Manifest.DatasetVersion); err != nil {
		t.Errorf("dataset_version is not hex: %v", err)
	}

	// The rendered manifest must carry the same counts as the in-memory result,
	// including the flattened M1-09 fields.
	var document struct {
		Seed             int64          `json:"seed"`
		DatasetVersion   string         `json:"dataset_version"`
		ModeCounts       map[string]int `json:"mode_counts"`
		DifficultyCounts map[string]int `json:"difficulty_counts"`
		Noise            modes.Noise    `json:"noise"`
		Counts           struct {
			GroundTruthLinks int `json:"ground_truth_links"`
			LedgerTxns       int `json:"ledger_txns"`
			ExternalRecords  int `json:"external_records"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(fileData(t, result, "manifest.json"), &document); err != nil {
		t.Fatalf("decode manifest.json: %v", err)
	}
	if document.Seed != result.Manifest.Seed ||
		document.DatasetVersion != result.Manifest.DatasetVersion {
		t.Errorf("manifest.json identity = %+v", document)
	}
	for name, count := range modeActual {
		if document.ModeCounts[name] != count {
			t.Errorf("manifest.json mode_counts[%s] = %d, want %d",
				name, document.ModeCounts[name], count)
		}
	}
	if document.DifficultyCounts[synth.DifficultyEasy] != len(result.Truth.Scenarios) {
		t.Errorf("manifest.json difficulty_counts = %v", document.DifficultyCounts)
	}
	if document.Counts.GroundTruthLinks != totalLinks ||
		document.Counts.LedgerTxns != len(result.Ledger.Transactions) {
		t.Errorf("manifest.json counts = %+v", document.Counts)
	}
	if document.Noise != result.Manifest.Noise {
		t.Errorf("manifest.json noise = %+v, want %+v", document.Noise, result.Manifest.Noise)
	}
}

// TestGroundTruthHD3PartialRefund pins the human decision: partial_refund has
// exactly ["link","adjustment"] plus a primary value, and no code assumes one
// canonical answer.
func TestGroundTruthHD3PartialRefund(t *testing.T) {
	result := generate(t, 708, modeTestDays, synth.DifficultyEasy,
		singleMode(modes.PartialRefund, 4))
	partial := scenariosOf(result, modes.PartialRefund)
	if len(partial) != 4 {
		t.Fatalf("partial_refund scenarios = %d, want 4", len(partial))
	}
	for _, scenario := range partial {
		if !sameStrings(scenario.ExpectedResolutions, []string{"link", "adjustment"}) {
			t.Errorf("scenario %s expected_resolutions = %v, want [link adjustment]",
				scenario.ScenarioID, scenario.ExpectedResolutions)
		}
		if scenario.PrimaryResolution != "adjustment" {
			t.Errorf("scenario %s primary_resolution = %q, want adjustment",
				scenario.ScenarioID, scenario.PrimaryResolution)
		}
		others := 0
		for _, resolution := range scenario.ExpectedResolutions {
			if resolution != scenario.PrimaryResolution {
				others++
			}
		}
		if others == 0 {
			t.Errorf("scenario %s has no alternative allowed resolution", scenario.ScenarioID)
		}
	}
	for _, scenario := range result.Truth.Scenarios {
		found := false
		for _, resolution := range scenario.ExpectedResolutions {
			if resolution == scenario.PrimaryResolution {
				found = true
			}
		}
		if !found {
			t.Errorf("scenario %s primary %q not in %v",
				scenario.ScenarioID, scenario.PrimaryResolution, scenario.ExpectedResolutions)
		}
	}
	if strings.TrimSpace(modes.GateDifficulty) != synth.DifficultyRealistic {
		t.Errorf("GateDifficulty = %q, want realistic", modes.GateDifficulty)
	}
}
