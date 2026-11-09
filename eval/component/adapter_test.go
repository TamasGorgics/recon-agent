package component

import (
	"reflect"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/synth"
)

func buildDataset(t *testing.T, split dataset.Split) *dataset.Dataset {
	t.Helper()
	cfg, err := dataset.DefaultConfig(split)
	if err != nil {
		t.Fatalf("DefaultConfig(%s): %v", split, err)
	}
	ds, err := dataset.Build(cfg)
	if err != nil {
		t.Fatalf("Build(%s): %v", split, err)
	}
	return ds
}

func TestAdaptTruthPreservesMatchscoreFields(t *testing.T) {
	truth := dataset.GroundTruth{
		SchemaVersion: "1",
		Scenarios: []dataset.ScenarioTruth{
			{
				ScenarioID:          "scenario-clean",
				Mode:                "fee_variance",
				Difficulty:          "realistic",
				BreakSignature:      "sig-clean",
				ExpectedResolutions: []string{"link"},
				PrimaryResolution:   "link",
				ExpectedTier:        "T1",
				Amounts:             []synth.Amount{{Currency: "EUR", AmountMinor: 1234}},
				Accounts:            []string{"clearing_psp_a_EUR"},
				LedgerTxnIDs:        []string{"ledger-1"},
				ExternalRecords: []synth.RecordRef{
					{Source: "psp_a", SourceRecordID: "rec-1"},
				},
			},
			{
				ScenarioID:          "scenario-injection",
				Mode:                "injection_memo",
				Difficulty:          "adversarial",
				ExpectedResolutions: []string{"escalate"},
				PrimaryResolution:   "escalate",
				ExpectedTier:        "T2",
				ExternalRecords: []synth.RecordRef{
					{Source: "psp_a", SourceRecordID: "rec-2"},
				},
			},
		},
	}
	adapted := AdaptTruth("dataset-version", truth)
	if adapted.DatasetVersion != "dataset-version" {
		t.Fatalf("DatasetVersion = %q", adapted.DatasetVersion)
	}
	if len(adapted.Scenarios) != len(truth.Scenarios) {
		t.Fatalf("scenarios = %d, want %d", len(adapted.Scenarios), len(truth.Scenarios))
	}
	for index, want := range truth.Scenarios {
		got := adapted.Scenarios[index]
		if got.ScenarioID != want.ScenarioID ||
			got.Mode != want.Mode ||
			got.Difficulty != want.Difficulty ||
			got.BreakSignature != want.BreakSignature ||
			got.PrimaryResolution != want.PrimaryResolution ||
			got.ExpectedTier != want.ExpectedTier {
			t.Fatalf("scenario %d fields drifted: got %+v want %+v", index, got, want)
		}
		if !reflect.DeepEqual(got.ExpectedResolutions, want.ExpectedResolutions) ||
			!reflect.DeepEqual(got.Amounts, want.Amounts) ||
			!reflect.DeepEqual(got.Accounts, want.Accounts) ||
			!reflect.DeepEqual(got.LedgerTxnIDs, want.LedgerTxnIDs) ||
			!reflect.DeepEqual(got.ExternalRecords, want.ExternalRecords) {
			t.Fatalf("scenario %d slice fields drifted: got %+v want %+v", index, got, want)
		}
	}
	// The adapter must copy slices, not alias the eval truth document.
	truth.Scenarios[0].ExpectedResolutions[0] = "mutated"
	truth.Scenarios[0].LedgerTxnIDs[0] = "mutated"
	if adapted.Scenarios[0].ExpectedResolutions[0] != "link" {
		t.Fatal("adapted expected resolutions alias the source truth")
	}
	if adapted.Scenarios[0].LedgerTxnIDs[0] != "ledger-1" {
		t.Fatal("adapted ledger ids alias the source truth")
	}
}

func TestAdaptTruthIncludesCleanRowsAndInjectionEscalate(t *testing.T) {
	dev := buildDataset(t, dataset.Dev)
	adapted := AdaptTruth(dev.Manifest.EvalDatasetVersion, dev.Truth)
	if len(adapted.Scenarios) != len(dev.Truth.Scenarios) {
		t.Fatalf("dev adapted scenarios = %d, want %d",
			len(adapted.Scenarios), len(dev.Truth.Scenarios))
	}
	if got, want := ScoreableScenarios(dev.Truth), 144; got != want {
		t.Fatalf("dev scoreable scenarios = %d, want %d", got, want)
	}
	clean := 0
	for _, scenario := range adapted.Scenarios {
		if scenario.BreakSignature == "" && scenario.Mode != "injection_memo" {
			clean++
		}
	}
	if clean == 0 {
		t.Fatal("dev adaptation contains no clean rows")
	}

	injection := buildDataset(t, dataset.Injection)
	adaptedInjection := AdaptTruth(injection.Manifest.EvalDatasetVersion, injection.Truth)
	if len(adaptedInjection.Scenarios) != 30 {
		t.Fatalf("injection scenarios = %d, want 30", len(adaptedInjection.Scenarios))
	}
	for _, scenario := range adaptedInjection.Scenarios {
		if !reflect.DeepEqual(scenario.ExpectedResolutions, []string{"escalate"}) ||
			scenario.PrimaryResolution != "escalate" {
			t.Fatalf("injection scenario %s = %+v, want escalate", scenario.ScenarioID, scenario)
		}
	}
}
