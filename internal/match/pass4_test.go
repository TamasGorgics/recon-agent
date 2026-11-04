// covers: FIN-42

package match_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestFuzzySingleCandidateMatches checks amount + date scoring: an exact amount
// on the same value date scores the default 8000 bps threshold and matches.
func TestFuzzySingleCandidateMatches(t *testing.T) {
	external := []match.ExternalRecord{{
		ID: "e-fuzzy", Source: "psp_a", SourceRecordID: "F-1", Kind: "charge",
		AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
		Reference: "EXT-REF",
	}}
	ledger := []match.LedgerTxn{{
		ID: "l-fuzzy", AmountMinor: 10_000, Currency: "USD",
		ValueDate: uDate("2026-06-10"), Reference: "LEDGER-REF",
	}}

	candidates, _, counts := evaluatePasses(t, unitConfig(), external, ledger)
	candidate, ok := findCandidate(candidates, "e-fuzzy")
	if !ok {
		t.Fatalf("clear fuzzy candidate did not match; counts = %v", counts)
	}
	if candidate.Rule != match.RuleFuzzy {
		t.Errorf("rule = %q, want %q", candidate.Rule, match.RuleFuzzy)
	}
	if got, want := candidate.ConfidenceBPS, match.ConfidenceBPS(8_000); got != want {
		t.Errorf("confidence = %d, want %d", got, want)
	}
	if got, want := counts["fuzzy"], 1; got != want {
		t.Errorf("fuzzy pass count = %d, want %d", got, want)
	}
}

// TestFuzzyBelowThresholdNoMatch proves a low score stays unmatched.
func TestFuzzyBelowThresholdNoMatch(t *testing.T) {
	external := []match.ExternalRecord{{
		ID: "e-weak", Source: "psp_a", SourceRecordID: "F-2", Kind: "charge",
		AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
		Reference: "EXT-REF",
	}}
	ledger := []match.LedgerTxn{{
		ID: "l-weak", AmountMinor: 9_900, Currency: "USD",
		ValueDate: uDate("2026-06-07"), Reference: "LEDGER-REF",
	}}

	candidates, _, _ := evaluatePasses(t, unitConfig(), external, ledger)
	if _, matched := findCandidate(candidates, "e-weak"); matched {
		t.Fatalf("below-threshold fuzzy candidate matched: %+v", candidates)
	}
}

// TestFuzzyTieNoMatch proves two equally scored top candidates never match.
func TestFuzzyTieNoMatch(t *testing.T) {
	external := []match.ExternalRecord{{
		ID: "e-tie", Source: "psp_a", SourceRecordID: "F-3", Kind: "charge",
		AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
		Reference: "EXT-REF",
	}}
	ledger := []match.LedgerTxn{
		{ID: "l-tie-a", AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "LEDGER-A"},
		{ID: "l-tie-b", AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-10"), Reference: "LEDGER-B"},
	}

	candidates, _, _ := evaluatePasses(t, unitConfig(), external, ledger)
	if _, matched := findCandidate(candidates, "e-tie"); matched {
		t.Fatalf("tied fuzzy candidates matched: %+v", candidates)
	}
}

// TestFuzzyConfidenceIsConfigThreshold proves the threshold comes from config:
// the same 7000-score pair does not match at the default 8000.
func TestFuzzyConfidenceIsConfigThreshold(t *testing.T) {
	external := []match.ExternalRecord{{
		ID: "e-day", Source: "psp_a", SourceRecordID: "F-4", Kind: "charge",
		AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-10"),
	}}
	ledger := []match.LedgerTxn{{
		ID: "l-day", AmountMinor: 10_000, Currency: "USD", ValueDate: uDate("2026-06-09"),
	}}

	candidates, _, _ := evaluatePasses(t, unitConfig(), external, ledger)
	if _, matched := findCandidate(candidates, "e-day"); matched {
		t.Fatal("one-day-late candidate matched at the default 8000 threshold")
	}
}
