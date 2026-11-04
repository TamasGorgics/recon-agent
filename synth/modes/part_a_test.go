package modes_test

import (
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

const modeTestDays = 14
const modeTestCount = 25

// TestModeFeeVariance proves the fee-schedule delta is physically present: the
// psp_a fee is the schedule fee plus delta and the external net is ledger minus
// the same delta.
//
// covers: FIN-06
func TestModeFeeVariance(t *testing.T) {
	result := generate(t, 101, modeTestDays, synth.DifficultyEasy,
		singleMode(modes.FeeVariance, modeTestCount))
	scenarios := scenariosOf(result, modes.FeeVariance)
	if len(scenarios) != modeTestCount {
		t.Fatalf("fee_variance scenarios = %d, want %d", len(scenarios), modeTestCount)
	}
	records := parsePSPFiles(t, result)
	transactions := txnIndex(result)
	for _, scenario := range scenarios {
		assertFailureScenarioSchema(t, scenario)
		if len(scenario.LedgerTxnIDs) != 1 || len(scenario.ExternalRecords) != 1 {
			t.Fatalf("scenario %s has %d ledger / %d external refs",
				scenario.ScenarioID, len(scenario.LedgerTxnIDs), len(scenario.ExternalRecords))
		}
		record, ok := records[scenario.ExternalRecords[0].SourceRecordID]
		if !ok {
			t.Fatalf("scenario %s external record %s is not in the files",
				scenario.ScenarioID, scenario.ExternalRecords[0].SourceRecordID)
		}
		txn := mustTxn(t, transactions, scenario.LedgerTxnIDs[0])
		currency := txn.Lines[0].Currency
		schedule := scheduleFor(t, result, synth.SourcePSP, "charge", currency)
		gross := record.Record.AmountMinor + record.Record.FeeMinor
		scheduleFee := feeMinor(gross, schedule.PctBps, schedule.FixedMinor)
		delta := record.Record.FeeMinor - scheduleFee
		if delta <= 0 {
			t.Errorf("scenario %s fee %d is not above schedule fee %d",
				scenario.ScenarioID, record.Record.FeeMinor, scheduleFee)
			continue
		}
		if ledgerFee := debitOn(t, txn, "fees_expense_"+lower(currency)); ledgerFee != scheduleFee {
			t.Errorf("scenario %s ledger fee = %d, want schedule fee %d",
				scenario.ScenarioID, ledgerFee, scheduleFee)
		}
		ledgerNet := clearingNet(t, txn)
		if ledgerNet-record.Record.AmountMinor != delta {
			t.Errorf("scenario %s delta = %d, want fee-schedule delta %d",
				scenario.ScenarioID, ledgerNet-record.Record.AmountMinor, delta)
		}
		if !sameStrings(scenario.ExpectedResolutions, []string{"adjustment"}) ||
			scenario.PrimaryResolution != "adjustment" || scenario.ExpectedTier != "T0" {
			t.Errorf("scenario %s resolution/tier = %v/%s/%s",
				scenario.ScenarioID, scenario.ExpectedResolutions,
				scenario.PrimaryResolution, scenario.ExpectedTier)
		}
	}
}

// TestModeFXRounding proves the external-minus-ledger delta is exactly the
// conversion residual computed with integer arithmetic.
func TestModeFXRounding(t *testing.T) {
	result := generate(t, 202, modeTestDays, synth.DifficultyEasy,
		singleMode(modes.FXRounding, modeTestCount))
	scenarios := scenariosOf(result, modes.FXRounding)
	if len(scenarios) != modeTestCount {
		t.Fatalf("fx_rounding scenarios = %d, want %d", len(scenarios), modeTestCount)
	}
	records := parsePSPFiles(t, result)
	transactions := txnIndex(result)
	for _, scenario := range scenarios {
		assertFailureScenarioSchema(t, scenario)
		if len(scenario.Amounts) != 3 {
			t.Fatalf("scenario %s has %d amounts, want 3",
				scenario.ScenarioID, len(scenario.Amounts))
		}
		if len(scenario.ExternalRecords) != 1 || len(scenario.LedgerTxnIDs) != 1 {
			t.Fatalf("scenario %s has wrong external/ledger refs", scenario.ScenarioID)
		}
		txn := mustTxn(t, transactions, scenario.LedgerTxnIDs[0])
		targetCurrency := txn.Lines[0].Currency
		var source, truncated, rounded int64
		for _, value := range scenario.Amounts {
			switch value.Currency {
			case targetCurrency:
				if truncated == 0 {
					truncated = value.AmountMinor
				} else {
					rounded = value.AmountMinor
				}
			default:
				source = value.AmountMinor
			}
		}
		if source <= 0 || truncated <= 0 || rounded <= 0 {
			t.Fatalf("scenario %s amounts = %+v", scenario.ScenarioID, scenario.Amounts)
		}
		wantTruncated, wantRounded := modes.ConversionMinor(source)
		if truncated != wantTruncated || rounded != wantRounded {
			t.Errorf("scenario %s conversion = (%d,%d), want (%d,%d) for source %d",
				scenario.ScenarioID, truncated, rounded, wantTruncated, wantRounded, source)
		}
		if rounded == truncated {
			t.Errorf("scenario %s has no conversion residual", scenario.ScenarioID)
		}
		record := records[scenario.ExternalRecords[0].SourceRecordID]
		if record.Record.AmountMinor != rounded {
			t.Errorf("scenario %s external amount = %d, want rounded %d",
				scenario.ScenarioID, record.Record.AmountMinor, rounded)
		}
		if ledgerNet := clearingNet(t, txn); ledgerNet != truncated {
			t.Errorf("scenario %s ledger amount = %d, want truncated %d",
				scenario.ScenarioID, ledgerNet, truncated)
		}
		if got := record.Record.AmountMinor - clearingNet(t, txn); got != rounded-truncated {
			t.Errorf("scenario %s delta = %d, want exact conversion residual %d",
				scenario.ScenarioID, got, rounded-truncated)
		}
		if record.Record.Currency != targetCurrency {
			t.Errorf("scenario %s external currency = %s, want %s",
				scenario.ScenarioID, record.Record.Currency, targetCurrency)
		}
	}
}

// TestModeMissingSettlement proves there is no external record for the chosen
// ledger charge.
func TestModeMissingSettlement(t *testing.T) {
	result := generate(t, 303, modeTestDays, synth.DifficultyEasy,
		singleMode(modes.MissingSettlement, modeTestCount))
	scenarios := scenariosOf(result, modes.MissingSettlement)
	if len(scenarios) != modeTestCount {
		t.Fatalf("missing_settlement scenarios = %d, want %d", len(scenarios), modeTestCount)
	}
	records := parsePSPFiles(t, result)
	references := make(map[string]struct{}, len(records))
	for _, record := range records {
		references[record.Reference] = struct{}{}
	}
	transactions := txnIndex(result)
	for _, scenario := range scenarios {
		assertFailureScenarioSchema(t, scenario)
		if len(scenario.ExternalRecords) != 0 {
			t.Errorf("scenario %s has %d external records, want 0",
				scenario.ScenarioID, len(scenario.ExternalRecords))
		}
		if len(scenario.LedgerTxnIDs) != 1 {
			t.Fatalf("scenario %s has %d ledger txns", scenario.ScenarioID, len(scenario.LedgerTxnIDs))
		}
		txn := mustTxn(t, transactions, scenario.LedgerTxnIDs[0])
		if txn.Kind != "charge" {
			t.Errorf("scenario %s ledger kind = %q, want charge", scenario.ScenarioID, txn.Kind)
		}
		reference := clearingLine(t, txn).ExternalRef
		if _, present := references[reference]; present {
			t.Errorf("scenario %s has an external record with reference %s", scenario.ScenarioID, reference)
		}
		if net := clearingNet(t, txn); net != scenario.Amounts[0].AmountMinor {
			t.Errorf("scenario %s amount = %d, ledger net = %d",
				scenario.ScenarioID, scenario.Amounts[0].AmountMinor, net)
		}
		if !sameStrings(scenario.ExpectedResolutions, []string{"request_info", "escalate"}) {
			t.Errorf("scenario %s resolutions = %v", scenario.ScenarioID, scenario.ExpectedResolutions)
		}
	}
}

// TestModeLateSettlement proves the psp_a record settles after the configured
// cutoff.
func TestModeLateSettlement(t *testing.T) {
	result := generate(t, 404, modeTestDays, synth.DifficultyEasy,
		singleMode(modes.LateSettlement, modeTestCount))
	scenarios := scenariosOf(result, modes.LateSettlement)
	if len(scenarios) != modeTestCount {
		t.Fatalf("late_settlement scenarios = %d, want %d", len(scenarios), modeTestCount)
	}
	records := parsePSPFiles(t, result)
	transactions := txnIndex(result)
	for _, scenario := range scenarios {
		assertFailureScenarioSchema(t, scenario)
		if len(scenario.ExternalRecords) != 1 || len(scenario.LedgerTxnIDs) != 1 {
			t.Fatalf("scenario %s has wrong external/ledger refs", scenario.ScenarioID)
		}
		record, ok := records[scenario.ExternalRecords[0].SourceRecordID]
		if !ok {
			t.Fatalf("scenario %s external record is missing", scenario.ScenarioID)
		}
		if record.Record.SettledAt == nil {
			t.Errorf("scenario %s record has no settled_at", scenario.ScenarioID)
			continue
		}
		cutoff := cutoffFor(t, synth.SourcePSP, record.Record.OccurredAt)
		if !record.Record.SettledAt.After(cutoff) {
			t.Errorf("scenario %s settled_at %s is not after cutoff %s",
				scenario.ScenarioID, record.Record.SettledAt, cutoff)
		}
		if !record.Record.OccurredAt.After(cutoff) {
			t.Errorf("scenario %s occurred_at %s is not after cutoff %s",
				scenario.ScenarioID, record.Record.OccurredAt, cutoff)
		}
		txn := mustTxn(t, transactions, scenario.LedgerTxnIDs[0])
		if net := clearingNet(t, txn); net != record.Record.AmountMinor {
			t.Errorf("scenario %s ledger net %d != external %d",
				scenario.ScenarioID, net, record.Record.AmountMinor)
		}
		if !sameStrings(scenario.ExpectedResolutions, []string{"link"}) {
			t.Errorf("scenario %s resolutions = %v", scenario.ScenarioID, scenario.ExpectedResolutions)
		}
	}
}

// TestModeDuplicateCharge proves two external records share one reference and
// amount while the ledger has one transaction.
func TestModeDuplicateCharge(t *testing.T) {
	result := generate(t, 505, modeTestDays, synth.DifficultyEasy,
		singleMode(modes.DuplicateCharge, modeTestCount))
	scenarios := scenariosOf(result, modes.DuplicateCharge)
	if len(scenarios) != modeTestCount {
		t.Fatalf("duplicate_charge scenarios = %d, want %d", len(scenarios), modeTestCount)
	}
	records := parsePSPFiles(t, result)
	transactions := txnIndex(result)
	for _, scenario := range scenarios {
		assertFailureScenarioSchema(t, scenario)
		if len(scenario.ExternalRecords) != 2 || len(scenario.LedgerTxnIDs) != 1 {
			t.Fatalf("scenario %s has %d external / %d ledger refs",
				scenario.ScenarioID, len(scenario.ExternalRecords), len(scenario.LedgerTxnIDs))
		}
		first, ok := records[scenario.ExternalRecords[0].SourceRecordID]
		if !ok {
			t.Fatalf("scenario %s first record missing", scenario.ScenarioID)
		}
		second, ok := records[scenario.ExternalRecords[1].SourceRecordID]
		if !ok {
			t.Fatalf("scenario %s duplicate record missing", scenario.ScenarioID)
		}
		if first.Record.SourceRecordID == second.Record.SourceRecordID {
			t.Errorf("scenario %s duplicate reuses the record ID", scenario.ScenarioID)
		}
		if first.Reference == "" || first.Reference != second.Reference {
			t.Errorf("scenario %s references = %q and %q",
				scenario.ScenarioID, first.Reference, second.Reference)
		}
		if first.Record.AmountMinor != second.Record.AmountMinor {
			t.Errorf("scenario %s amounts = %d and %d",
				scenario.ScenarioID, first.Record.AmountMinor, second.Record.AmountMinor)
		}
		if first.Path != second.Path {
			t.Errorf("scenario %s duplicate is in a different batch", scenario.ScenarioID)
		}
		txn := mustTxn(t, transactions, scenario.LedgerTxnIDs[0])
		if reference := clearingLine(t, txn).ExternalRef; reference != first.Reference {
			t.Errorf("scenario %s ledger reference = %q, external = %q",
				scenario.ScenarioID, reference, first.Reference)
		}
		if !sameStrings(scenario.ExpectedResolutions, []string{"duplicate"}) {
			t.Errorf("scenario %s resolutions = %v", scenario.ScenarioID, scenario.ExpectedResolutions)
		}
	}
}

// TestModePartialRefund proves the external refund amount differs from the
// ledger refund and that HD-3's exact allowed set is present.
func TestModePartialRefund(t *testing.T) {
	result := generate(t, 606, modeTestDays, synth.DifficultyEasy,
		singleMode(modes.PartialRefund, modeTestCount))
	scenarios := scenariosOf(result, modes.PartialRefund)
	if len(scenarios) != modeTestCount {
		t.Fatalf("partial_refund scenarios = %d, want %d", len(scenarios), modeTestCount)
	}
	records := parsePSPFiles(t, result)
	transactions := txnIndex(result)
	for _, scenario := range scenarios {
		assertFailureScenarioSchema(t, scenario)
		if len(scenario.ExternalRecords) != 1 || len(scenario.LedgerTxnIDs) != 1 {
			t.Fatalf("scenario %s has wrong external/ledger refs", scenario.ScenarioID)
		}
		txn := mustTxn(t, transactions, scenario.LedgerTxnIDs[0])
		if txn.Kind != "refund" {
			t.Errorf("scenario %s ledger kind = %q, want refund", scenario.ScenarioID, txn.Kind)
		}
		record := records[scenario.ExternalRecords[0].SourceRecordID]
		ledgerNet := clearingNet(t, txn)
		if record.Record.AmountMinor == ledgerNet {
			t.Errorf("scenario %s refund amount did not change", scenario.ScenarioID)
		}
		delta := record.Record.AmountMinor - ledgerNet
		if delta <= 0 {
			t.Errorf("scenario %s refund delta = %d, want positive", scenario.ScenarioID, delta)
		}
		if record.Record.AmountMinor >= 0 || ledgerNet >= 0 {
			t.Errorf("scenario %s refund signs = external %d ledger %d",
				scenario.ScenarioID, record.Record.AmountMinor, ledgerNet)
		}
		if !sameStrings(scenario.ExpectedResolutions, []string{"link", "adjustment"}) {
			t.Errorf("scenario %s expected_resolutions = %v, want [link adjustment]",
				scenario.ScenarioID, scenario.ExpectedResolutions)
		}
		if scenario.PrimaryResolution != "link" && scenario.PrimaryResolution != "adjustment" {
			t.Errorf("scenario %s primary_resolution = %q",
				scenario.ScenarioID, scenario.PrimaryResolution)
		}
		if scenario.ExpectedTier != "T0" {
			t.Errorf("scenario %s expected_tier = %q", scenario.ScenarioID, scenario.ExpectedTier)
		}
	}
}

func lower(currency string) string {
	return strings.ToLower(currency)
}
