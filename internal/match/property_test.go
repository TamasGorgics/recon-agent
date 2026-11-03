// covers: FIN-42

package match_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// mixedFixture exercises every pass at once: exact, tolerance, declared
// composite, subset composite, fuzzy, duplicates, and an unmatched remainder.
func mixedFixture() ([]match.ExternalRecord, []match.LedgerTxn) {
	external := []match.ExternalRecord{
		{
			ID: "e-exact", Source: "psp_a", SourceRecordID: "X-1", Kind: "charge",
			AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "EX",
		},
		{
			ID: "e-tolerance", Source: "psp_a", SourceRecordID: "T-1", Kind: "charge",
			AmountMinor: 20_000, Currency: "USD", ValueDate: uDate("2026-06-11"), Reference: "TOL",
		},
		{
			ID: "e-payout", Source: "psp_a", SourceRecordID: "P-1", Kind: "payout",
			AmountMinor: -10_000, Currency: "USD", ValueDate: uDate("2026-06-12"), Batch: "B-1",
		},
		{
			ID: "e-member-1", Source: "psp_a", SourceRecordID: "M-1", Kind: "charge",
			AmountMinor: 1, Currency: "USD", ValueDate: uDate("2026-06-12"), Reference: "MR-1", Batch: "B-1",
		},
		{
			ID: "e-member-2", Source: "psp_a", SourceRecordID: "M-2", Kind: "charge",
			AmountMinor: 2, Currency: "USD", ValueDate: uDate("2026-06-12"), Reference: "MR-2", Batch: "B-1",
		},
		{
			ID: "e-subset", Source: "psp_a", SourceRecordID: "S-1", Kind: "payout",
			AmountMinor: -9_500, Currency: "USD", ValueDate: uDate("2026-06-13"),
		},
		{
			ID: "e-fuzzy", Source: "psp_a", SourceRecordID: "F-1", Kind: "charge",
			AmountMinor: 8_000, Currency: "USD", ValueDate: uDate("2026-06-14"), Reference: "F-EXT",
		},
		{
			ID: "e-dup-1", Source: "psp_a", SourceRecordID: "D-1", Kind: "charge",
			AmountMinor: 6_000, Currency: "USD", ValueDate: uDate("2026-06-15"),
			OccurredAt: uTime("2026-06-15T09:00:00Z"),
		},
		{
			ID: "e-dup-2", Source: "psp_a", SourceRecordID: "D-2", Kind: "charge",
			AmountMinor: 6_000, Currency: "USD", ValueDate: uDate("2026-06-15"),
			OccurredAt: uTime("2026-06-15T09:00:00Z"),
		},
	}
	ledger := []match.LedgerTxn{
		{ID: "l-exact", AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "EX"},
		{ID: "l-tolerance", AmountMinor: 19_950, Currency: "USD", ValueDate: uDate("2026-06-11"), Reference: "TOL"},
		{ID: "l-member-1", AmountMinor: -6_000, Currency: "USD", ValueDate: uDate("2026-06-12"), Reference: "MR-1"},
		{ID: "l-member-2", AmountMinor: -4_000, Currency: "USD", ValueDate: uDate("2026-06-12"), Reference: "MR-2"},
		{ID: "l-subset-a", AmountMinor: -5_000, Currency: "USD", ValueDate: uDate("2026-06-13")},
		{ID: "l-subset-b", AmountMinor: -4_500, Currency: "USD", ValueDate: uDate("2026-06-13")},
		{ID: "l-fuzzy", AmountMinor: 8_000, Currency: "USD", ValueDate: uDate("2026-06-14"), Reference: "F-LEDGER"},
	}
	return external, ledger
}

// TestFullEngineOrderIndependent shuffles the whole snapshot and compares the
// canonical match and break sets across every pass (M1-13 acceptance).
func TestFullEngineOrderIndependent(t *testing.T) {
	external, ledger := mixedFixture()

	forward, forwardBreaks, forwardCounts := evaluatePasses(t, unitConfig(), external, ledger)
	// Reverse both slices; RunContext sorts by id, and every pass must be
	// insensitive to the original allocation order.
	reversedExternal := append([]match.ExternalRecord(nil), external...)
	reversedLedger := append([]match.LedgerTxn(nil), ledger...)
	sort.SliceStable(reversedExternal, func(i, j int) bool { return reversedExternal[i].ID > reversedExternal[j].ID })
	sort.SliceStable(reversedLedger, func(i, j int) bool { return reversedLedger[i].ID > reversedLedger[j].ID })
	backward, backwardBreaks, backwardCounts := evaluatePasses(t, unitConfig(), reversedExternal, reversedLedger)

	wantForward := canonicalRun(forward, forwardBreaks)
	wantBackward := canonicalRun(backward, backwardBreaks)
	if strings.Join(wantForward, "\n") != strings.Join(wantBackward, "\n") {
		t.Fatalf("match/break sets differ under shuffled input:\nforward:\n%s\nbackward:\n%s",
			strings.Join(wantForward, "\n"), strings.Join(wantBackward, "\n"))
	}
	for name, count := range forwardCounts {
		if backwardCounts[name] != count {
			t.Errorf("pass %q count = %d, want %d", name, backwardCounts[name], count)
		}
	}

	// The mixed fixture must actually exercise every pass.
	wantCounts := map[string]int{
		"exact":               1,
		"reference_tolerance": 1,
		"composite":           2,
		"fuzzy":               1,
		"duplicate":           0,
	}
	for name, want := range wantCounts {
		if got := forwardCounts[name]; got != want {
			t.Errorf("pass %q matched %d, want %d (counts = %v)", name, got, want, forwardCounts)
		}
	}
	if got, want := len(findBreaks(forwardBreaks, match.SideDuplicate)), 1; got != want {
		t.Errorf("duplicate breaks = %d, want %d", got, want)
	}
}
