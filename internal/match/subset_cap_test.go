// covers: FIN-42

package match_test

import (
	"fmt"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestCompositeSubsetDefaultCapAmbiguousNoMatch reproduces the adversary's
// Finding 2 input exactly: payout -100 against -2, -98, twenty-two -7 decoys,
// and two -50 legs. The default composite_max_nodes budget is exhausted after
// the -2/-98 subset is found, before the -50/-50 subset is discovered. A
// truncated search cannot prove the subset is unique, so under the documented
// default configuration the payout must not match and must become an
// external_only break instead of an arbitrary composite link.
func TestCompositeSubsetDefaultCapAmbiguousNoMatch(t *testing.T) {
	external := []match.ExternalRecord{{
		ID: "e-default-cap", Source: "psp_a", SourceRecordID: "CAP-1", Kind: "payout",
		AmountMinor: -100, Currency: "USD", ValueDate: uDate("2026-06-10"),
	}}
	ledger := make([]match.LedgerTxn, 0, 26)
	add := func(id string, amount int64) {
		ledger = append(ledger, match.LedgerTxn{
			ID: id, AmountMinor: amount, Currency: "USD", ValueDate: uDate("2026-06-10"),
		})
	}
	add("l-A0", -2)
	add("l-A1", -98)
	for i := 2; i <= 9; i++ {
		add(fmt.Sprintf("l-A%d", i), -7)
	}
	for i := 0; i <= 9; i++ {
		add(fmt.Sprintf("l-B%d", i), -7)
	}
	for i := 0; i <= 3; i++ {
		add(fmt.Sprintf("l-C%d", i), -7)
	}
	add("l-C4", -50)
	add("l-C5", -50)

	// unitConfig leaves the composite caps at zero, so the pass chain applies
	// the documented defaults (including composite_max_nodes = 100000).
	candidates, breaks, counts := evaluatePasses(t, unitConfig(), external, ledger)
	if candidate, ok := findCandidate(candidates, "e-default-cap"); ok {
		t.Fatalf("default-cap ambiguous payout matched a truncated subset: %+v", candidate)
	}
	if got := counts["composite"]; got != 0 {
		t.Errorf("composite count = %d, want 0", got)
	}
	if len(findBreaks(breaks, match.SideExternalOnly)) == 0 {
		t.Fatalf("ambiguous payout produced no external_only break; breaks = %+v", breaks)
	}
}

// TestCompositeSubsetNodeCapNoPartialMatch constructs the cap failure directly:
// the first solution (60+40) is found, then the node budget runs out before the
// search can prove whether another solution (50+50) exists. The pass must not
// accept the solution it happened to discover first.
func TestCompositeSubsetNodeCapNoPartialMatch(t *testing.T) {
	cfg := unitConfig()
	cfg.CompositeMaxNodes = 4 // enough for the first solution, not for the proof

	external := []match.ExternalRecord{{
		ID: "e-node-cap", Source: "psp_a", SourceRecordID: "CAP-2", Kind: "payout",
		AmountMinor: -100, Currency: "USD", ValueDate: uDate("2026-06-10"),
	}}
	ledger := []match.LedgerTxn{
		{ID: "l-1", AmountMinor: -60, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-2", AmountMinor: -40, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-3", AmountMinor: -50, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-4", AmountMinor: -50, Currency: "USD", ValueDate: uDate("2026-06-10")},
	}

	candidates, breaks, counts := evaluatePasses(t, cfg, external, ledger)
	if candidate, ok := findCandidate(candidates, "e-node-cap"); ok {
		t.Fatalf("node-cap payout matched a partial subset: %+v", candidate)
	}
	if got := counts["composite"]; got != 0 {
		t.Errorf("composite count = %d, want 0", got)
	}
	if len(findBreaks(breaks, match.SideExternalOnly)) == 0 {
		t.Fatalf("cap-exceeded payout produced no external_only break; breaks = %+v", breaks)
	}
}

// TestCompositeSubsetSingleSolutionStillMatches guards against over-rejection:
// a payout with exactly one valid subset under the default caps must still
// match with the exact legs.
func TestCompositeSubsetSingleSolutionStillMatches(t *testing.T) {
	external := []match.ExternalRecord{{
		ID: "e-single-solution", Source: "psp_a", SourceRecordID: "CAP-3", Kind: "payout",
		AmountMinor: -100, Currency: "USD", ValueDate: uDate("2026-06-10"),
	}}
	ledger := []match.LedgerTxn{
		{ID: "l-1", AmountMinor: -60, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-2", AmountMinor: -40, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-3", AmountMinor: -7, Currency: "USD", ValueDate: uDate("2026-06-10")},
		{ID: "l-4", AmountMinor: -7, Currency: "USD", ValueDate: uDate("2026-06-10")},
	}

	candidates, _, _ := evaluatePasses(t, unitConfig(), external, ledger)
	candidate, ok := findCandidate(candidates, "e-single-solution")
	if !ok {
		t.Fatal("single exact subset did not match under the default caps")
	}
	if candidate.Rule != match.RuleCompositeSubset {
		t.Errorf("rule = %q, want %q", candidate.Rule, match.RuleCompositeSubset)
	}
	got := legIDs(candidate)
	want := []string{"l-1", "l-2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("legs = %v, want %v", got, want)
	}
	var sum int64
	for _, leg := range candidate.Legs {
		sum += leg.AmountMinor
	}
	if sum != candidate.External.AmountMinor {
		t.Errorf("leg sum = %d, want exact external total %d", sum, candidate.External.AmountMinor)
	}
}
