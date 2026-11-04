// covers: FIN-42

package match_test

import (
	"encoding/hex"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

func baseBreakCandidate() match.BreakCandidate {
	return match.BreakCandidate{
		ScopeID: "scope-1",
		Side:    match.SideAmountMismatch,
		LedgerTxnIDs: []string{
			"33333333-3333-4333-8333-333333333333",
			"11111111-1111-4111-8111-111111111111",
			"22222222-2222-4222-8222-222222222222",
		},
		ExternalRecords: []match.RecordRef{
			{Source: "psp_a", SourceRecordID: "rec-b"},
			{Source: "bank_x", SourceRecordID: "rec-c"},
			{Source: "psp_a", SourceRecordID: "rec-a"},
		},
		Currency:   "USD",
		DeltaMinor: -125,
	}
}

func TestBreakCandidateSignatureIsStableAcrossOrder(t *testing.T) {
	first := baseBreakCandidate()
	second := baseBreakCandidate()
	second.LedgerTxnIDs[0], second.LedgerTxnIDs[2] = second.LedgerTxnIDs[2], second.LedgerTxnIDs[0]
	second.ExternalRecords[0], second.ExternalRecords[2] = second.ExternalRecords[2], second.ExternalRecords[0]

	if got, want := second.Signature(), first.Signature(); got != want {
		t.Fatalf("Signature() = %s, want stable %s", got, want)
	}
	if _, err := hex.DecodeString(first.Signature()); err != nil {
		t.Fatalf("Signature() is not hex: %v", err)
	}
	if got, want := len(first.Signature()), 64; got != want {
		t.Fatalf("Signature() length = %d, want %d", got, want)
	}

	// Signature must not reorder or mutate the caller's slices.
	if got, want := first.LedgerTxnIDs[0], "33333333-3333-4333-8333-333333333333"; got != want {
		t.Errorf("LedgerTxnIDs mutated: first = %s, want %s", got, want)
	}
	if got, want := first.ExternalRecords[0].SourceRecordID, "rec-b"; got != want {
		t.Errorf("ExternalRecords mutated: first = %s, want %s", got, want)
	}
}

func TestBreakCandidateSignatureSensitiveToCanonicalFields(t *testing.T) {
	base := baseBreakCandidate().Signature()
	cases := []struct {
		name   string
		mutate func(*match.BreakCandidate)
	}{
		{"scope", func(c *match.BreakCandidate) { c.ScopeID = "scope-2" }},
		{"side", func(c *match.BreakCandidate) { c.Side = match.SideExternalOnly }},
		{"ledger ids", func(c *match.BreakCandidate) {
			c.LedgerTxnIDs = append(c.LedgerTxnIDs, "44444444-4444-4444-8444-444444444444")
		}},
		{"external refs", func(c *match.BreakCandidate) {
			c.ExternalRecords = append(c.ExternalRecords, match.RecordRef{Source: "psp_a", SourceRecordID: "rec-d"})
		}},
		{"currency", func(c *match.BreakCandidate) { c.Currency = "EUR" }},
		{"delta", func(c *match.BreakCandidate) { c.DeltaMinor = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := baseBreakCandidate()
			tc.mutate(&candidate)
			if got := candidate.Signature(); got == base {
				t.Fatalf("Signature() did not change when %s changed", tc.name)
			}
		})
	}
}
