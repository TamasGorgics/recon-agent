//go:build integration

// covers: FIN-54

package tieout_test

import (
	"context"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// TestSeededToleranceBugSurfaces seeds the matcher bug from the roadmap: pass 2
// matches a ledger booking of 20,000 against an external record of 19,950
// because the 50 minor-unit delta is inside the fee tolerance, but the delta is
// never booked. Both rows are therefore matched (zero open items), and the
// unexplained difference must equal the unbooked delta exactly.
func TestSeededToleranceBugSurfaces(t *testing.T) {
	f := newFixture(t, "psp_a")
	f.addExternal(t, "BUG-TOL", 19_950, "2026-06-10")
	f.addLedger(t, "BUG-TOL", 20_000, "2026-06-10")
	f.addStatement(t, "2026-06-10", 0, 19_950)

	result := f.runMatcher(t, "2026-06-01", "2026-06-30", 100)
	if got, want := result.Stats.ByPass["reference_tolerance"], 1; got != want {
		t.Fatalf("tolerance matches = %d, want %d (the bug must be seeded by the real matcher)", got, want)
	}

	got, err := tieout.Compute(context.Background(), f.db.Pool, f.scopeID, mustDate(t, "2026-06-10"))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got.LedgerOnlyCount != 0 || got.ExternalOnlyCount != 0 {
		t.Fatalf("open items = %d ledger-only / %d external-only, want 0/0 (both rows are matched)",
			got.LedgerOnlyCount, got.ExternalOnlyCount)
	}
	if got.Unexplained.AmountMinor() != 50 {
		t.Fatalf("unexplained = %d, want 50 (the unbooked tolerance delta)",
			got.Unexplained.AmountMinor())
	}
}

// TestDroppedMatchStillTies pins the roadmap rule: dropping a match only
// creates breaks and leaves both balances queued as open items, so the tie-out
// is still zero. The matcher component evals catch a dropped match; FIN-54
// catches a balance that does not tie.
func TestDroppedMatchStillTies(t *testing.T) {
	f := newFixture(t, "psp_a")
	f.addExternal(t, "DROPPED", 10_000, "2026-06-10")
	f.addLedger(t, "DROPPED", 10_000, "2026-06-10")
	f.addStatement(t, "2026-06-10", 0, 10_000)

	// No matcher run: the match is deliberately dropped. The ledger-only and
	// external-only open items offset, so the unexplained difference stays
	// zero. This is expected: only the matcher evals can detect the missing
	// link; FIN-54 detects balance discrepancies, not missing matches.
	result, err := tieout.Compute(context.Background(), f.db.Pool, f.scopeID, mustDate(t, "2026-06-10"))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got, want := result.LedgerOnlyCount, 1; got != want {
		t.Errorf("ledger-only count = %d, want %d", got, want)
	}
	if got, want := result.ExternalOnlyCount, 1; got != want {
		t.Errorf("external-only count = %d, want %d", got, want)
	}
	if !result.Unexplained.IsZero() {
		t.Errorf("unexplained = %d, want exactly 0", result.Unexplained.AmountMinor())
	}
}
