package verify

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// testTime is midday UTC so date-only truncation is unambiguous.
var testTime = time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)

func balancedLines(total int64, currency string) []AdjustmentLine {
	return []AdjustmentLine{
		{AccountCode: "clearing", Direction: "debit", AmountMinor: total, Currency: currency},
		{AccountCode: "fees_expense_psp_a", Direction: "credit", AmountMinor: total, Currency: currency},
	}
}

func validFeeInput() FeeVarianceInput {
	return FeeVarianceInput{
		Claimed: PatternFeeVariance,
		External: ExternalRecord{
			Source:         "psp_a",
			SourceRecordID: "ext-1",
			Kind:           "charge",
			Currency:       "USD",
			AmountMinor:    100_000,
			FeeMinor:       20_000,
			OccurredAt:     testTime,
		},
		Ledger: LedgerTxn{
			ID:        "ledger-1",
			ValueDate: testTime,
			Currency:  "USD",
			FeeMinor:  42,
			NetMinor:  119_958,
		},
		Schedules: []FeeSchedule{{
			ID:         "schedule-1",
			Source:     "psp_a",
			Kind:       "charge",
			Currency:   "USD",
			PctBPS:     1,
			FixedMinor: 30,
			ValidFrom:  time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
			ValidTo:    time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC),
		}},
		Lines: balancedLines(19_958, "USD"),
	}
}

func validFXInput() FXRoundingInput {
	return FXRoundingInput{
		Claimed: PatternFXRounding,
		External: ExternalRecord{
			Source:         "psp_a",
			SourceRecordID: "ext-fx-1",
			Kind:           "fx",
			Currency:       "EUR",
			AmountMinor:    91,
			OccurredAt:     testTime,
		},
		Ledger: LedgerTxn{
			ID:        "ledger-fx-1",
			ValueDate: testTime,
			Currency:  "EUR",
			NetMinor:  90,
		},
		Rates: []FXRate{{
			Base:   "USD",
			Quote:  "EUR",
			Source: "psp_a",
			Rate:   "0.9",
			AsOf:   testTime.Add(-24 * time.Hour),
		}},
		Lines: balancedLines(1, "EUR"),
	}
}

func validRoundingInput() RoundingInput {
	return RoundingInput{
		Claimed: PatternRounding,
		External: ExternalRecord{
			Source:         "psp_a",
			SourceRecordID: "ext-round-1",
			Kind:           "charge",
			Currency:       "USD",
			AmountMinor:    1_000,
			OccurredAt:     testTime,
		},
		Ledger: LedgerTxn{
			ID:        "ledger-round-1",
			ValueDate: testTime,
			Currency:  "USD",
			NetMinor:  999,
		},
		Lines: balancedLines(1, "USD"),
	}
}

func validReferenceInput() ReferenceTypoInput {
	return ReferenceTypoInput{
		Claimed: PatternReferenceTypo,
		External: ExternalRecord{
			Source:         "psp_a",
			SourceRecordID: "ext-ref-1",
			Kind:           "charge",
			Currency:       "USD",
			AmountMinor:    1_000,
			OccurredAt:     testTime,
		},
		Candidates: []Candidate{{
			LedgerTxnID: "ledger-ref-1",
			AmountMinor: 1_000,
			Currency:    "USD",
			ValueDate:   testTime.Add(48 * time.Hour),
		}},
		ProposedLedgerIDs: []string{"ledger-ref-1"},
	}
}

func assertPass(t *testing.T, got Result, pattern, claimed string) {
	t.Helper()
	if !got.Passed {
		t.Fatalf("Passed = false, want true; details=%+v", got.Details)
	}
	if got.Pattern != pattern {
		t.Fatalf("Pattern = %q, want %q", got.Pattern, pattern)
	}
	if got.Details.Claimed != claimed {
		t.Fatalf("Claimed = %q, want %q", got.Details.Claimed, claimed)
	}
	if got.Details.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.Details.SchemaVersion, SchemaVersion)
	}
	if got.Details.Reason != "" || len(got.Details.Reasons) != 0 {
		t.Fatalf("pass carries reasons: %+v", got.Details)
	}
}

func assertFailure(t *testing.T, got Result, pattern, claimed string, wantReasons ...string) {
	t.Helper()
	if got.Passed {
		t.Fatalf("Passed = true, want false; details=%+v", got.Details)
	}
	if got.Pattern != pattern {
		t.Fatalf("Pattern = %q, want %q", got.Pattern, pattern)
	}
	if got.Details.Claimed != claimed {
		t.Fatalf("Claimed = %q, want %q", got.Details.Claimed, claimed)
	}
	if got.Details.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.Details.SchemaVersion, SchemaVersion)
	}
	if wantReasons == nil {
		wantReasons = []string{}
	}
	if !reflect.DeepEqual(got.Details.Reasons, wantReasons) {
		t.Fatalf("Reasons = %v, want %v", got.Details.Reasons, wantReasons)
	}
	if len(wantReasons) > 0 && got.Details.Reason != wantReasons[0] {
		t.Fatalf("Reason = %q, want primary %q", got.Details.Reason, wantReasons[0])
	}
	for _, reason := range got.Details.Reasons {
		if !IsReasonCode(reason) {
			t.Fatalf("reason %q is not in the closed vocabulary", reason)
		}
	}
}

func resultJSON(t *testing.T, result Result) string {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return string(encoded)
}
