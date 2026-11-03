// covers: FIN-42

package match_test

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestCompositeDeclaredBatchPreferred proves the declared settlement batch is
// tried before the bounded subset-sum, even when a second exact subset exists.
func TestCompositeDeclaredBatchPreferred(t *testing.T) {
	external := []match.ExternalRecord{
		{
			ID: "e-payout", Source: "psp_a", SourceRecordID: "P-1", Kind: "payout",
			AmountMinor: -10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Batch: "batch-1",
		},
		{
			ID: "e-member-1", Source: "psp_a", SourceRecordID: "M-1", Kind: "charge",
			AmountMinor: 1, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Reference: "R1", Batch: "batch-1",
		},
		{
			ID: "e-member-2", Source: "psp_a", SourceRecordID: "M-2", Kind: "charge",
			AmountMinor: 2, Currency: "USD", ValueDate: uDate("2026-06-10"),
			Reference: "R2", Batch: "batch-1",
		},
	}
	ledger := []match.LedgerTxn{
		{ID: "l-r1", AmountMinor: -6_000, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "R1"},
		{ID: "l-r2", AmountMinor: -4_000, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "R2"},
		{ID: "l-alt-1", AmountMinor: -5_000, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "ALT-1"},
		{ID: "l-alt-2", AmountMinor: -5_000, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "ALT-2"},
	}

	candidates, _, counts := evaluatePasses(t, unitConfig(), external, ledger)
	candidate, ok := findCandidate(candidates, "e-payout")
	if !ok {
		t.Fatalf("declared batch payout did not match; candidates = %+v, counts = %v", candidates, counts)
	}
	if candidate.Rule != match.RuleCompositeBatch {
		t.Errorf("rule = %q, want %q", candidate.Rule, match.RuleCompositeBatch)
	}
	if got, want := legIDs(candidate), []string{"l-r1", "l-r2"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("declared legs = %v, want %v (declared membership must win over the alternative subset)", got, want)
	}
	if got, want := counts["composite"], 1; got != want {
		t.Errorf("composite pass count = %d, want %d", got, want)
	}
}

// TestCompositeSubsetExactSum proves the fallback subset-sum matches only when
// the signed leg sum equals the external total exactly.
func TestCompositeSubsetExactSum(t *testing.T) {
	external := []match.ExternalRecord{
		{
			ID: "e-subset", Source: "psp_a", SourceRecordID: "S-1", Kind: "payout",
			AmountMinor: -10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
		},
		{
			ID: "e-near-miss", Source: "psp_a", SourceRecordID: "S-2", Kind: "payout",
			AmountMinor: -10_001, Currency: "USD", ValueDate: uDate("2026-06-10"),
		},
	}
	ledger := []match.LedgerTxn{
		{ID: "l-a", AmountMinor: -6_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-b", AmountMinor: -4_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
	}

	candidates, _, _ := evaluatePasses(t, unitConfig(), external, ledger)
	candidate, ok := findCandidate(candidates, "e-subset")
	if !ok {
		t.Fatal("exact subset did not match")
	}
	if candidate.Rule != match.RuleCompositeSubset {
		t.Errorf("rule = %q, want %q", candidate.Rule, match.RuleCompositeSubset)
	}
	var sum int64
	for _, leg := range candidate.Legs {
		sum += leg.AmountMinor
	}
	if sum != candidate.External.AmountMinor {
		t.Errorf("leg sum = %d, want exact external total %d", sum, candidate.External.AmountMinor)
	}
	if _, matched := findCandidate(candidates, "e-near-miss"); matched {
		t.Error("near-miss payout matched a subset whose sum differs by one minor unit")
	}
}

// TestCompositeSizeCapYieldsBreak proves a subset larger than the leg cap is
// never partially matched; the payout becomes a break candidate.
func TestCompositeSizeCapYieldsBreak(t *testing.T) {
	cfg := unitConfig()
	cfg.CompositeMaxLegs = 2
	external := []match.ExternalRecord{{
		ID: "e-cap", Source: "psp_a", SourceRecordID: "C-1", Kind: "payout",
		AmountMinor: -10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
	}}
	ledger := []match.LedgerTxn{
		{ID: "l-1", AmountMinor: -4_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-2", AmountMinor: -3_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-3", AmountMinor: -3_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
	}

	candidates, breaks, _ := evaluatePasses(t, cfg, external, ledger)
	if _, matched := findCandidate(candidates, "e-cap"); matched {
		t.Fatal("payout matched a subset that exceeds the configured leg cap")
	}
	if len(findBreaks(breaks, match.SideExternalOnly)) == 0 {
		t.Fatalf("cap-exceeded payout produced no break candidate; breaks = %+v", breaks)
	}
}

// TestCompositeAmbiguityNoMatch proves two equally valid subsets produce no
// match and a break candidate instead of an arbitrary choice.
func TestCompositeAmbiguityNoMatch(t *testing.T) {
	external := []match.ExternalRecord{{
		ID: "e-ambiguous", Source: "psp_a", SourceRecordID: "A-1", Kind: "payout",
		AmountMinor: -10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
	}}
	ledger := []match.LedgerTxn{
		{ID: "l-a", AmountMinor: -6_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-b", AmountMinor: -4_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-c", AmountMinor: -5_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-d", AmountMinor: -5_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
	}

	candidates, breaks, _ := evaluatePasses(t, unitConfig(), external, ledger)
	if _, matched := findCandidate(candidates, "e-ambiguous"); matched {
		t.Fatalf("ambiguous payout matched arbitrarily: %+v", candidates)
	}
	if len(findBreaks(breaks, match.SideExternalOnly)) == 0 {
		t.Fatalf("ambiguous payout produced no break candidate; breaks = %+v", breaks)
	}
}

// TestCompositeExactSumProperty checks the invariant across generated leg sets:
// every claimed composite leg sum equals the external total exactly.
func TestCompositeExactSumProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(2, 5).Draw(rt, "legs")
		ledger := make([]match.LedgerTxn, 0, n)
		var total int64
		for i := 0; i < n; i++ {
			amount := rapid.Int64Range(1, 10_000).Draw(rt, "amount")
			total += amount
			ledger = append(ledger, match.LedgerTxn{
				ID:          fmt.Sprintf("l-%d", i),
				AmountMinor: -amount,
				Currency:    "USD",
				ValueDate:   uDate("2026-06-10"),
			})
		}
		external := []match.ExternalRecord{{
			ID: "e-prop", Source: "psp_a", SourceRecordID: "PROP", Kind: "payout",
			AmountMinor: -total, Currency: "USD", ValueDate: uDate("2026-06-10"),
		}}

		candidates, _, _ := evaluatePasses(t, unitConfig(), external, ledger)
		for _, candidate := range candidates {
			var sum int64
			seen := make(map[string]struct{}, len(candidate.Legs))
			for _, leg := range candidate.Legs {
				sum += leg.AmountMinor
				if leg.Currency != "USD" {
					rt.Fatalf("leg %s currency = %q, want USD", leg.LedgerTxnID, leg.Currency)
				}
				if _, duplicate := seen[leg.LedgerTxnID]; duplicate {
					rt.Fatalf("leg %s used twice", leg.LedgerTxnID)
				}
				seen[leg.LedgerTxnID] = struct{}{}
			}
			if sum != candidate.External.AmountMinor {
				rt.Fatalf("leg sum = %d, want exact total %d", sum, candidate.External.AmountMinor)
			}
		}
	})
}

func TestCompositeSkipsSingleLeg(t *testing.T) {
	external := []match.ExternalRecord{{
		ID: "e-single", Source: "psp_a", SourceRecordID: "S-1", Kind: "payout",
		AmountMinor: -10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
	}}
	ledger := []match.LedgerTxn{
		{ID: "l-only", AmountMinor: -10_000, Currency: "USD", ValueDate: uDate("2026-06-10")},
	}
	// The single ledger candidate would be fuzzy-eligible, not composite: pass
	// 3 must require at least two legs.
	cfg := unitConfig()
	cfg.FuzzyThresholdBPS = 10_000
	cfg.FuzzyDateWindowDays = 0
	candidates, _, _ := evaluatePasses(t, cfg, external, ledger)
	for _, candidate := range candidates {
		if candidate.Rule == match.RuleCompositeSubset && len(candidate.Legs) < 2 {
			t.Fatalf("composite matched a single leg: %+v", candidate)
		}
	}
}
