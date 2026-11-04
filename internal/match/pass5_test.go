// covers: FIN-42

package match_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestDuplicateRepeatedExternalIDBreak proves a repeated source record id
// produces a duplicate break candidate and no match.
func TestDuplicateRepeatedExternalIDBreak(t *testing.T) {
	external := []match.ExternalRecord{
		{
			ID: "e-dup-1", Source: "psp_a", SourceRecordID: "REC-1", Kind: "charge",
			AmountMinor: 5_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
			OccurredAt: uTime("2026-06-10T12:00:00Z"),
		},
		{
			ID: "e-dup-2", Source: "psp_a", SourceRecordID: "REC-1", Kind: "charge",
			AmountMinor: 5_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
			OccurredAt: uTime("2026-06-10T12:00:00Z"),
		},
	}

	candidates, breaks, counts := evaluatePasses(t, unitConfig(), external, nil)
	if len(candidates) != 0 {
		t.Fatalf("duplicates produced matches: %+v", candidates)
	}
	duplicates := findBreaks(breaks, match.SideDuplicate)
	if len(duplicates) != 1 {
		t.Fatalf("duplicate breaks = %d (%+v), want 1", len(duplicates), breaks)
	}
	if got, want := len(duplicates[0].ExternalRecords), 2; got != want {
		t.Errorf("duplicate break refs = %d, want %d", got, want)
	}
	if duplicates[0].ScopeID != unitScope.ID || duplicates[0].Currency != "USD" {
		t.Errorf("duplicate break scope/currency = %q/%q, want %q/USD",
			duplicates[0].ScopeID, duplicates[0].Currency, unitScope.ID)
	}
	if got, want := counts["duplicate"], 0; got != want {
		t.Errorf("duplicates pass matched %d, want %d (never auto-links)", got, want)
	}
}

// TestDuplicateSameAmountAndTimeBreak covers two distinct ids with the same
// amount at the same instant.
func TestDuplicateSameAmountAndTimeBreak(t *testing.T) {
	external := []match.ExternalRecord{
		{
			ID: "e-same-1", Source: "psp_a", SourceRecordID: "REC-A", Kind: "charge",
			AmountMinor: 7_500, Currency: "USD", ValueDate: uDate("2026-06-10"),
			OccurredAt: uTime("2026-06-10T09:30:00Z"), Reference: "SAME",
		},
		{
			ID: "e-same-2", Source: "psp_a", SourceRecordID: "REC-B", Kind: "charge",
			AmountMinor: 7_500, Currency: "USD", ValueDate: uDate("2026-06-10"),
			OccurredAt: uTime("2026-06-10T09:30:00Z"), Reference: "SAME",
		},
	}
	// One ledger txn exists, so fuzzy has a candidate for both records and must
	// drop them as ambiguous; duplicates must still never link.
	ledger := []match.LedgerTxn{{
		ID: "l-same", AmountMinor: 7_500, Currency: "USD", ValueDate: uDate("2026-06-10"),
	}}

	candidates, breaks, _ := evaluatePasses(t, unitConfig(), external, ledger)
	if len(candidates) != 0 {
		t.Fatalf("same amount/time duplicates were linked: %+v", candidates)
	}
	duplicates := findBreaks(breaks, match.SideDuplicate)
	if len(duplicates) != 1 {
		t.Fatalf("duplicate breaks = %d (%+v), want 1", len(duplicates), breaks)
	}
	refs := duplicates[0].ExternalRecords
	if len(refs) != 2 || refs[0].Source != "psp_a" || refs[1].Source != "psp_a" {
		t.Errorf("duplicate refs = %+v, want two psp_a records", refs)
	}
}

// TestDuplicateBreakSignatureIsStable proves re-evaluation (a rerun) produces
// the same break signature, which is what makes M1-14's break insertion
// idempotent.
func TestDuplicateBreakSignatureIsStable(t *testing.T) {
	external := []match.ExternalRecord{
		{
			ID: "e-stable-1", Source: "psp_a", SourceRecordID: "REC-X", Kind: "charge",
			AmountMinor: 3_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
			OccurredAt: uTime("2026-06-10T08:00:00Z"),
		},
		{
			ID: "e-stable-2", Source: "psp_a", SourceRecordID: "REC-Y", Kind: "charge",
			AmountMinor: 3_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
			OccurredAt: uTime("2026-06-10T08:00:00Z"),
		},
	}

	_, first, _ := evaluatePasses(t, unitConfig(), external, nil)
	_, second, _ := evaluatePasses(t, unitConfig(), external, nil)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("break counts = %d/%d, want 1/1", len(first), len(second))
	}
	if first[0].Signature() != second[0].Signature() {
		t.Errorf("break signature changed between runs: %s vs %s", first[0].Signature(), second[0].Signature())
	}
}
