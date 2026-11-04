// covers: FIN-04, FIN-42

package match_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestPass2DeltaOverflowNoMatch reproduces the adversary finding: an external
// +1 against a ledger -MaxInt64 (or any pair whose true delta is 2^63 or more)
// must never satisfy the fee tolerance.
func TestPass2DeltaOverflowNoMatch(t *testing.T) {
	tests := []struct {
		name     string
		external int64
		ledger   int64
	}{
		{"external one ledger minus max", 1, -math.MaxInt64},
		{"external one ledger min", 1, math.MinInt64},
		{"external max ledger minus one", math.MaxInt64, -1},
		{"external min ledger zero", math.MinInt64, 0},
		{"external zero ledger min", 0, math.MinInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			external := []match.ExternalRecord{{
				ID: "e-ovf", Source: "psp_a", SourceRecordID: "OVF2-R1", Kind: "charge",
				AmountMinor: tc.external, Currency: "USD", ValueDate: uDate("2026-06-10"),
				Reference: "OVF2-R1",
			}}
			ledger := []match.LedgerTxn{{
				ID: "l-ovf", AmountMinor: tc.ledger, Currency: "USD",
				ValueDate: uDate("2026-06-10"), Reference: "OVF2-R1",
			}}

			candidates, _, counts := evaluatePasses(t, unitConfig(), external, ledger)
			if _, matched := findCandidate(candidates, "e-ovf"); matched {
				t.Fatalf("overflowing delta %d - %d matched: %+v", tc.external, tc.ledger, candidates)
			}
			if got := counts["reference_tolerance"]; got != 0 {
				t.Errorf("reference_tolerance count = %d, want 0", got)
			}
		})
	}
}

// TestPass2DeltaBoundaryStillMatches proves checked arithmetic did not change
// in-range behavior at the edge of the tolerance.
func TestPass2DeltaBoundaryStillMatches(t *testing.T) {
	tests := []struct {
		name     string
		external int64
		ledger   int64
	}{
		{"max at tolerance", math.MaxInt64, math.MaxInt64 - 100},
		{"min plus one", math.MinInt64 + 1, math.MinInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			external := []match.ExternalRecord{{
				ID: "e-edge", Source: "psp_a", SourceRecordID: "EDGE-R1", Kind: "charge",
				AmountMinor: tc.external, Currency: "USD", ValueDate: uDate("2026-06-10"),
				Reference: "EDGE-R1",
			}}
			ledger := []match.LedgerTxn{{
				ID: "l-edge", AmountMinor: tc.ledger, Currency: "USD",
				ValueDate: uDate("2026-06-10"), Reference: "EDGE-R1",
			}}

			candidates, _, _ := evaluatePasses(t, unitConfig(), external, ledger)
			candidate, matched := findCandidate(candidates, "e-edge")
			if !matched {
				t.Fatalf("in-range delta %d - %d did not match", tc.external, tc.ledger)
			}
			if candidate.Rule != match.RuleReferenceTolerance {
				t.Errorf("rule = %q, want %q", candidate.Rule, match.RuleReferenceTolerance)
			}
		})
	}
}

// TestCompositeDeclaredSumOverflowNoMatch reproduces the adversary's composite
// case: payout -1 against legs -MaxInt64, -MaxInt64, -3 wraps to -1 unless the
// declared sum is checked. The payout must become an external_only break.
func TestCompositeDeclaredSumOverflowNoMatch(t *testing.T) {
	cfg := unitConfig()
	cfg.FuzzyThresholdBPS = 10_000 // isolate pass 3 from fuzzy tolerance matches.

	external := []match.ExternalRecord{
		{
			ID: "e-payout", Source: "psp_a", SourceRecordID: "OVF3-P", Kind: "payout",
			AmountMinor: -1, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Reference: "OVF3-P", Batch: "batch-ovf",
		},
		{
			ID: "e-member-1", Source: "psp_a", SourceRecordID: "OVF3-R1", Kind: "payout",
			AmountMinor: -1_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Reference: "OVF3-R1", Batch: "batch-ovf",
		},
		{
			ID: "e-member-2", Source: "psp_a", SourceRecordID: "OVF3-R2", Kind: "payout",
			AmountMinor: -1_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Reference: "OVF3-R2", Batch: "batch-ovf",
		},
		{
			ID: "e-member-3", Source: "psp_a", SourceRecordID: "OVF3-R3", Kind: "payout",
			AmountMinor: -1_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Reference: "OVF3-R3", Batch: "batch-ovf",
		},
	}
	ledger := []match.LedgerTxn{
		{ID: "l-r1", AmountMinor: -math.MaxInt64, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "OVF3-R1"},
		{ID: "l-r2", AmountMinor: -math.MaxInt64, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "OVF3-R2"},
		{ID: "l-r3", AmountMinor: -3, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "OVF3-R3"},
	}

	candidates, breaks, counts := evaluatePasses(t, cfg, external, ledger)
	if _, matched := findCandidate(candidates, "e-payout"); matched {
		t.Fatalf("overflowing declared composite sum matched: %+v", candidates)
	}
	if got := counts["composite"]; got != 0 {
		t.Errorf("composite count = %d, want 0", got)
	}
	if len(findBreaks(breaks, match.SideExternalOnly)) == 0 {
		t.Fatalf("overflowing payout produced no external_only break; breaks = %+v", breaks)
	}
}

// TestCompositeDeclaredSumMinInt64OverflowNoMatch covers the other direction:
// MaxInt64 + 1 wraps to MinInt64 and would falsely equal a MinInt64 payout.
func TestCompositeDeclaredSumMinInt64OverflowNoMatch(t *testing.T) {
	cfg := unitConfig()
	cfg.FuzzyThresholdBPS = 10_000

	external := []match.ExternalRecord{
		{
			ID: "e-min-payout", Source: "psp_a", SourceRecordID: "OVF4-P", Kind: "payout",
			AmountMinor: math.MinInt64, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Reference: "OVF4-P", Batch: "batch-min",
		},
		{
			ID: "e-min-member-1", Source: "psp_a", SourceRecordID: "OVF4-R1", Kind: "charge",
			AmountMinor: 1, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Reference: "OVF4-R1", Batch: "batch-min",
		},
	}
	ledger := []match.LedgerTxn{
		{ID: "l-max", AmountMinor: math.MaxInt64, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "OVF4-R1"},
		{ID: "l-one", AmountMinor: 1, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "OVF4-R2"},
	}

	candidates, breaks, _ := evaluatePasses(t, cfg, external, ledger)
	if _, matched := findCandidate(candidates, "e-min-payout"); matched {
		t.Fatalf("wrapping MinInt64 composite sum matched: %+v", candidates)
	}
	if len(findBreaks(breaks, match.SideExternalOnly)) == 0 {
		t.Fatalf("overflowing payout produced no external_only break; breaks = %+v", breaks)
	}
}

// TestCompositeSubsetOverflowNoMatch covers the subset fallback: the payout
// magnitude must be representable and each pool leg must have a representable
// magnitude, otherwise no exact subset can be claimed.
func TestCompositeSubsetOverflowNoMatch(t *testing.T) {
	tests := []struct {
		name     string
		external int64
		ledger   []int64
	}{
		{"target overflow", math.MinInt64, []int64{-math.MaxInt64, -1}},
		{"leg magnitude overflow", -1, []int64{math.MinInt64}},
		{"legs exceed target", -1, []int64{-math.MaxInt64, -math.MaxInt64, -3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := unitConfig()
			cfg.FuzzyThresholdBPS = 10_000

			external := []match.ExternalRecord{{
				ID: "e-subset-ovf", Source: "psp_a", SourceRecordID: "OVF5-P", Kind: "payout",
				AmountMinor: tc.external, Currency: "USD", ValueDate: uDate("2026-06-10"),
			}}
			ledger := make([]match.LedgerTxn, 0, len(tc.ledger))
			for i, amount := range tc.ledger {
				ledger = append(ledger, match.LedgerTxn{
					ID:          fmt.Sprintf("l-subset-%d", i),
					AmountMinor: amount,
					Currency:    "USD",
					ValueDate:   uDate("2026-06-10"),
				})
			}

			candidates, breaks, _ := evaluatePasses(t, cfg, external, ledger)
			if _, matched := findCandidate(candidates, "e-subset-ovf"); matched {
				t.Fatalf("overflowing subset matched: %+v", candidates)
			}
			if len(findBreaks(breaks, match.SideExternalOnly)) == 0 {
				t.Fatalf("overflowing subset produced no external_only break; breaks = %+v", breaks)
			}
		})
	}
}

// TestFuzzyDeltaOverflowNoMatch covers pass 4's unchecked subtraction: a pair
// whose true delta does not fit int64 must not produce a fuzzy candidate.
func TestFuzzyDeltaOverflowNoMatch(t *testing.T) {
	tests := []struct {
		name     string
		external int64
		ledger   int64
	}{
		{"max vs minus one", math.MaxInt64, -1},
		{"min vs zero", math.MinInt64, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			external := []match.ExternalRecord{{
				ID: "e-fuzzy-ovf", Source: "psp_a", SourceRecordID: "OVF6-E", Kind: "charge",
				AmountMinor: tc.external, Currency: "USD", ValueDate: uDate("2026-06-10"),
				Reference: "EXT-REF",
			}}
			ledger := []match.LedgerTxn{{
				ID: "l-fuzzy-ovf", AmountMinor: tc.ledger, Currency: "USD",
				ValueDate: uDate("2026-06-10"), Reference: "LEDGER-REF",
			}}

			candidates, _, counts := evaluatePasses(t, unitConfig(), external, ledger)
			if _, matched := findCandidate(candidates, "e-fuzzy-ovf"); matched {
				t.Fatalf("overflowing fuzzy delta matched: %+v", candidates)
			}
			if got := counts["fuzzy"]; got != 0 {
				t.Errorf("fuzzy count = %d, want 0", got)
			}
		})
	}
}
