//go:build integration

// covers: FIN-04, FIN-42

package match_test

import (
	"math"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestOverflowNeverCreatesActiveMatch reproduces the adversary's two database
// cases: extreme but valid int64 amounts must stay unmatched, and the refused
// composite payout must surface as an external_only break candidate.
func TestOverflowNeverCreatesActiveMatch(t *testing.T) {
	t.Run("pass2 tolerance delta", func(t *testing.T) {
		f := newFixture(t, "psp_a")
		f.addExternal(t, "OVF2-R1", 1, "2026-06-10")
		f.addLedger(t, "OVF2-R1", -math.MaxInt64, "2026-06-10")

		engine := newEngine(t, f.db, nil)
		result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

		if got, want := result.Stats.Matched, 0; got != want {
			t.Fatalf("matched = %d, want %d (true delta is 2^63, tolerance is 100)", got, want)
		}
		if got := result.Stats.ByPass["reference_tolerance"]; got != 0 {
			t.Errorf("reference_tolerance count = %d, want 0", got)
		}
		if n := countRows(t, f.db,
			`SELECT count(*) FROM matches WHERE status = 'active'`); n != 0 {
			t.Errorf("active matches = %d, want 0", n)
		}
		if result.Stats.UnmatchedExternal != 1 || result.Stats.UnmatchedLedger != 1 {
			t.Errorf("unmatched external/ledger = %d/%d, want 1/1",
				result.Stats.UnmatchedExternal, result.Stats.UnmatchedLedger)
		}
	})

	t.Run("pass3 declared composite sum", func(t *testing.T) {
		f := newFixture(t, "psp_a")
		batchID := insertBatch(t, f.db, "psp_a")
		date := mustDate(t, "2026-06-10")
		payoutID := f.addExternalAt(t, batchID, "OVF3-P", -1, "2026-06-10", date.Add(12*time.Hour))
		f.addExternalAt(t, batchID, "OVF3-R1", -1_000, "2026-06-10", date.Add(13*time.Hour))
		f.addExternalAt(t, batchID, "OVF3-R2", -1_000, "2026-06-10", date.Add(14*time.Hour))
		f.addExternalAt(t, batchID, "OVF3-R3", -1_000, "2026-06-10", date.Add(15*time.Hour))
		f.addLedger(t, "OVF3-R1", -math.MaxInt64, "2026-06-10")
		f.addLedger(t, "OVF3-R2", -math.MaxInt64, "2026-06-10")
		f.addLedger(t, "OVF3-R3", -3, "2026-06-10")

		engine := newEngine(t, f.db, nil)
		result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

		if got, want := result.Stats.Matched, 0; got != want {
			t.Fatalf("matched = %d, want %d (declared leg sum wraps to -1)", got, want)
		}
		if got := result.Stats.ByPass["composite"]; got != 0 {
			t.Errorf("composite count = %d, want 0", got)
		}
		if n := countRows(t, f.db,
			`SELECT count(*) FROM matches WHERE status = 'active'`); n != 0 {
			t.Errorf("active matches = %d, want 0", n)
		}
		if n := countRows(t, f.db,
			`SELECT count(*) FROM match_legs WHERE status = 'active'`); n != 0 {
			t.Errorf("active match legs = %d, want 0", n)
		}
		if n := countRows(t, f.db,
			`SELECT count(*) FROM matches WHERE external_record_id = $1::uuid`, payoutID); n != 0 {
			t.Errorf("payout matches = %d, want 0", n)
		}
		if len(findBreaks(result.Breaks, match.SideExternalOnly)) == 0 {
			t.Fatalf("overflowing payout produced no external_only break; breaks = %+v", result.Breaks)
		}
	})
}
