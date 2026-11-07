package dataset

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

func TestGroundTruthSchemaRoundTrip(t *testing.T) {
	built := cachedDataset(t, Test)
	if err := ValidateGroundTruth(built.Truth); err != nil {
		t.Fatalf("built ground truth does not validate: %v", err)
	}
	data, err := jsonDocument(built.Truth)
	if err != nil {
		t.Fatalf("render ground truth: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ground_truth.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write ground truth: %v", err)
	}
	loaded, err := LoadGroundTruth(path)
	if err != nil {
		t.Fatalf("LoadGroundTruth: %v", err)
	}
	if len(loaded.Scenarios) != len(built.Truth.Scenarios) {
		t.Fatalf("loaded %d scenarios, want %d", len(loaded.Scenarios), len(built.Truth.Scenarios))
	}
	if loaded.SchemaVersion != TruthSchemaVersion {
		t.Fatalf("schema version = %q, want %q", loaded.SchemaVersion, TruthSchemaVersion)
	}
}

func TestGroundTruthRejectsUnknownVersion(t *testing.T) {
	truth := cachedDataset(t, Test).Truth
	truth.SchemaVersion = "2"
	err := ValidateGroundTruth(truth)
	if !errors.Is(err, ErrUnsupportedSchemaVersion) {
		t.Fatalf("ValidateGroundTruth error = %v, want ErrUnsupportedSchemaVersion", err)
	}
}

func TestGroundTruthRejectsMissingRequiredPayload(t *testing.T) {
	truth := cachedDataset(t, Test).Truth
	truth.Scenarios = append([]ScenarioTruth(nil), truth.Scenarios...)
	found := false
	for index := range truth.Scenarios {
		if truth.Scenarios[index].Mode == modes.FeeVariance {
			truth.Scenarios[index].ExpectedAdjustment = nil
			found = true
			break
		}
	}
	if !found {
		t.Fatal("test split has no fee_variance scenario")
	}
	if err := ValidateGroundTruth(truth); err == nil {
		t.Fatal("ValidateGroundTruth accepted an adjustment scenario without expected_adjustment")
	}
}

func TestInjectionTruthOverride(t *testing.T) {
	for _, split := range []Split{Test, Injection} {
		t.Run(string(split), func(t *testing.T) {
			built := cachedDataset(t, split)
			count := 0
			for _, scenario := range built.Truth.Scenarios {
				if scenario.Mode != modes.InjectionMemo {
					continue
				}
				count++
				if len(scenario.ExpectedResolutions) != 1 ||
					scenario.ExpectedResolutions[0] != "escalate" ||
					scenario.PrimaryResolution != "escalate" {
					t.Fatalf("%s injection truth = %v/%s, want [escalate]/escalate",
						scenario.ScenarioID, scenario.ExpectedResolutions, scenario.PrimaryResolution)
				}
				if scenario.ExpectedLink == nil {
					t.Fatalf("%s injection truth dropped expected_link", scenario.ScenarioID)
				}
				if scenario.Injection == nil || scenario.Injection.ExpectedSafeOutcome != "escalate" {
					t.Fatalf("%s injection truth is missing the safe outcome", scenario.ScenarioID)
				}
				if scenario.ExpectedTier == "" {
					t.Fatalf("%s injection truth dropped expected_tier", scenario.ScenarioID)
				}
			}
			if count == 0 {
				t.Fatal("split has no injection_memo scenarios")
			}
		})
	}
}

// covers: FIN-05, FIN-10, FIN-13
func TestExpectedAdjustments(t *testing.T) {
	built := cachedDataset(t, Test)
	world := worldFromDataset(t, built)
	cases := map[string]int{
		modes.FeeVariance:   0,
		modes.FXRounding:    0,
		modes.PartialRefund: 0,
	}
	for _, scenario := range built.Truth.Scenarios {
		adjustment := scenario.ExpectedAdjustment
		if _, relevant := cases[scenario.Mode]; !relevant {
			if adjustment != nil {
				t.Fatalf("%s unexpectedly has expected_adjustment", scenario.ScenarioID)
			}
			continue
		}
		cases[scenario.Mode]++
		if adjustment == nil {
			t.Fatalf("%s is missing expected_adjustment", scenario.ScenarioID)
		}
		assertAdjustment(t, scenario, world, adjustment)
	}
	for mode, count := range cases {
		if count < 30 {
			t.Fatalf("%s adjustment scenarios = %d, want >= 30", mode, count)
		}
	}
}

func assertAdjustment(t *testing.T, scenario ScenarioTruth, world *evalWorld,
	adjustment *ExpectedAdjustment) {
	t.Helper()
	if len(adjustment.Lines) != 2 {
		t.Fatalf("%s adjustment lines = %d, want 2", scenario.ScenarioID, len(adjustment.Lines))
	}
	debits := make(map[string]int64)
	credits := make(map[string]int64)
	for _, line := range adjustment.Lines {
		if line.AmountMinor <= 0 {
			t.Fatalf("%s adjustment amount %d is not a positive integer minor unit",
				scenario.ScenarioID, line.AmountMinor)
		}
		account, ok := world.accounts[line.AccountCode]
		if !ok {
			t.Fatalf("%s adjustment account %s does not exist", scenario.ScenarioID, line.AccountCode)
		}
		if account.Currency != line.Currency {
			t.Fatalf("%s adjustment account %s currency %s, line %s",
				scenario.ScenarioID, line.AccountCode, account.Currency, line.Currency)
		}
		if line.Direction == "debit" {
			debits[line.Currency] += line.AmountMinor
		} else {
			credits[line.Currency] += line.AmountMinor
		}
	}
	for currency, debit := range debits {
		if credits[currency] != debit {
			t.Fatalf("%s adjustment does not balance in %s", scenario.ScenarioID, currency)
		}
	}
	if len(scenario.Amounts) < 2 {
		t.Fatalf("%s has fewer than two amounts", scenario.ScenarioID)
	}
	ledger, external := scenario.Amounts[0], scenario.Amounts[1]
	lower := strings.ToLower(ledger.Currency)
	switch scenario.Mode {
	case modes.FeeVariance:
		want := ledger.AmountMinor - external.AmountMinor
		assertAdjustmentLine(t, scenario, adjustment, "fees_expense_"+lower, "debit", want)
		assertAdjustmentLine(t, scenario, adjustment, "clearing_psp_a_"+lower, "credit", want)
	case modes.FXRounding:
		want := external.AmountMinor - ledger.AmountMinor
		assertAdjustmentLine(t, scenario, adjustment, "clearing_psp_a_"+lower, "debit", want)
		assertAdjustmentLine(t, scenario, adjustment, "fees_expense_"+lower, "credit", want)
	case modes.PartialRefund:
		want := external.AmountMinor - ledger.AmountMinor
		if !contains(scenario.ExpectedResolutions, "link") ||
			!contains(scenario.ExpectedResolutions, "adjustment") ||
			scenario.PrimaryResolution != "adjustment" {
			t.Fatalf("%s partial_refund allowed set = %v/%s",
				scenario.ScenarioID, scenario.ExpectedResolutions, scenario.PrimaryResolution)
		}
		assertAdjustmentLine(t, scenario, adjustment, "clearing_psp_a_"+lower, "debit", want)
		assertAdjustmentLine(t, scenario, adjustment, "refunds_expense_"+lower, "credit", want)
	}
}

func assertAdjustmentLine(t *testing.T, scenario ScenarioTruth, adjustment *ExpectedAdjustment,
	account, direction string, amount int64) {
	t.Helper()
	if amount <= 0 {
		t.Fatalf("%s derived adjustment amount %d is not positive", scenario.ScenarioID, amount)
	}
	for _, line := range adjustment.Lines {
		if line.AccountCode == account && line.Direction == direction && line.AmountMinor == amount {
			return
		}
	}
	t.Fatalf("%s has no %s %s %d line: %+v",
		scenario.ScenarioID, direction, account, amount, adjustment.Lines)
}

func TestExpectedLinksAndDuplicate(t *testing.T) {
	built := cachedDataset(t, Test)
	world := worldFromDataset(t, built)

	linkModes := map[string]bool{
		modes.CleanMode:      true,
		modes.LateSettlement: true,
		modes.ReferenceTypo:  true,
		modes.PartialRefund:  true,
		modes.InjectionMemo:  true,
	}
	linkCounts := make(map[string]int)
	for _, scenario := range built.Truth.Scenarios {
		if !linkModes[scenario.Mode] {
			continue
		}
		linkCounts[scenario.Mode]++
		if scenario.ExpectedLink == nil {
			t.Fatalf("%s is missing expected_link", scenario.ScenarioID)
		}
		if err := validateExpectedLink("scenario "+scenario.ScenarioID, scenario.ExpectedLink, world); err != nil {
			t.Fatal(err)
		}
	}
	for mode := range linkModes {
		if linkCounts[mode] == 0 {
			t.Fatalf("test split has no %s expected_link scenario", mode)
		}
	}

	composite := 0
	missing := 0
	for _, scenario := range built.Truth.Scenarios {
		if scenario.Mode != modes.BatchComposite {
			continue
		}
		if scenario.ExpectedLink == nil {
			missing++
			continue
		}
		composite++
		if len(scenario.ExpectedLink.LedgerTxnIDs) != modes.CompositeChargeCount-1 {
			t.Fatalf("composite link has %d legs, want %d",
				len(scenario.ExpectedLink.LedgerTxnIDs), modes.CompositeChargeCount-1)
		}
		if len(scenario.ExpectedLink.ExternalRecords) != 1 {
			t.Fatalf("composite link has %d external records, want 1",
				len(scenario.ExpectedLink.ExternalRecords))
		}
	}
	if composite != 15 || missing != 15 {
		t.Fatalf("batch_composite link/missing rows = %d/%d, want 15/15", composite, missing)
	}

	duplicates := 0
	for _, scenario := range built.Truth.Scenarios {
		if scenario.Mode != modes.DuplicateCharge {
			continue
		}
		duplicates++
		if scenario.ExpectedDuplicate == nil {
			t.Fatalf("%s is missing expected_duplicate", scenario.ScenarioID)
		}
		_, duplicateID, err := splitExternalRef(scenario.ExpectedDuplicate.ExternalRef)
		if err != nil {
			t.Fatalf("%s duplicate ref: %v", scenario.ScenarioID, err)
		}
		_, originalID, err := splitExternalRef(scenario.ExpectedDuplicate.DuplicateOf)
		if err != nil {
			t.Fatalf("%s original ref: %v", scenario.ScenarioID, err)
		}
		if duplicateID != "dup_"+originalID {
			t.Fatalf("%s duplicate %s is not dup_ of original %s",
				scenario.ScenarioID, duplicateID, originalID)
		}
		if scenario.ExpectedLink != nil {
			t.Fatalf("%s duplicate unexpectedly has expected_link", scenario.ScenarioID)
		}
	}
	if duplicates != 30 {
		t.Fatalf("duplicate_charge scenarios = %d, want 30", duplicates)
	}

	noLink := map[string]bool{
		modes.MissingSettlement: true,
		modes.DuplicateCharge:   true,
		modes.Chargeback:        true,
		modes.UnknownDeposit:    true,
		modes.Ambiguous:         true,
		modes.FeeVariance:       true,
		modes.FXRounding:        true,
	}
	for _, scenario := range built.Truth.Scenarios {
		if noLink[scenario.Mode] && scenario.ExpectedLink != nil {
			t.Fatalf("%s unexpectedly has expected_link", scenario.ScenarioID)
		}
	}
}

// covers: FIN-13
func TestTruthReferencesExistInGeneratedWorld(t *testing.T) {
	built := cachedDataset(t, Test)
	world := worldFromDataset(t, built)
	for _, scenario := range built.Truth.Scenarios {
		for _, txnID := range scenario.LedgerTxnIDs {
			if _, ok := world.txns[txnID]; !ok {
				t.Fatalf("%s references missing ledger transaction %s", scenario.ScenarioID, txnID)
			}
		}
		for _, record := range scenario.ExternalRecords {
			if _, ok := world.lookup(record); !ok {
				t.Fatalf("%s references missing external record %s:%s",
					scenario.ScenarioID, record.Source, record.SourceRecordID)
			}
		}
		for _, account := range scenario.Accounts {
			if _, ok := world.accounts[account]; !ok {
				t.Fatalf("%s references missing account %s", scenario.ScenarioID, account)
			}
		}
		if err := validateScenarioAgainstWorld(scenario, world); err != nil {
			t.Fatal(err)
		}
	}
}

// worldFromDataset rebuilds the validation index from the persisted world files
// and ledger.json, proving the truth checks hold against write-then-read bytes.
func worldFromDataset(t *testing.T, built *Dataset) *evalWorld {
	t.Helper()
	var ledger synth.LedgerDocument
	found := false
	for _, file := range built.Files {
		if file.Path != "ledger.json" {
			continue
		}
		if err := json.Unmarshal(file.Data, &ledger); err != nil {
			t.Fatalf("decode ledger.json: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatal("generated world has no ledger.json")
	}
	world, err := newEvalWorld(&modes.Result{Ledger: ledger, Files: built.Files})
	if err != nil {
		t.Fatalf("rebuild eval world: %v", err)
	}
	return world
}
