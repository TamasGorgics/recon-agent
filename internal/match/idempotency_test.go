//go:build integration

// covers: FIN-42

package match_test

import (
	"context"
	"testing"
)

func TestRerunCreatesNoNewMatches(t *testing.T) {
	f := newFixture(t, "psp_a")
	externalID := f.addExternal(t, "IDEM-1", 12_500, "2026-06-10")
	f.addLedger(t, "IDEM-1", 12_500, "2026-06-10")
	engine := newEngine(t, f.db, nil)

	first := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")
	if got, want := first.Stats.Matched, 1; got != want {
		t.Fatalf("first run matched = %d, want %d", got, want)
	}

	second := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")
	if got, want := second.Stats.Matched, 0; got != want {
		t.Fatalf("second run matched = %d, want %d", got, want)
	}
	if got, want := second.Stats.ExternalCandidates, 0; got != want {
		t.Errorf("second run external candidates = %d, want %d", got, want)
	}
	if got, want := second.Stats.LedgerCandidates, 0; got != want {
		t.Errorf("second run ledger candidates = %d, want %d", got, want)
	}

	if got, want := countRows(t, f.db, `SELECT count(*) FROM matches`), int64(1); got != want {
		t.Errorf("matches = %d, want %d", got, want)
	}
	if got, want := countRows(t, f.db, `SELECT count(*) FROM match_legs`), int64(1); got != want {
		t.Errorf("match legs = %d, want %d", got, want)
	}
	if got, want := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE status = 'active'`), int64(1); got != want {
		t.Errorf("active matches = %d, want %d", got, want)
	}
	if got, want := countRows(t, f.db, `SELECT count(*) FROM match_runs`), int64(2); got != want {
		t.Errorf("match runs = %d, want 2 (a rerun may be recorded)", got)
	}
	if row := loadMatch(t, f.db, externalID); row.MatchRunID != first.RunID {
		t.Errorf("match run id = %s, want original %s", row.MatchRunID, first.RunID)
	}
}

func TestIncrementalRunMatchesOnlyNewData(t *testing.T) {
	f := newFixture(t, "psp_a")
	f.addExternal(t, "INC-1", 7_000, "2026-06-10")
	f.addLedger(t, "INC-1", 7_000, "2026-06-10")
	engine := newEngine(t, f.db, nil)
	first := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")
	if got, want := first.Stats.Matched, 1; got != want {
		t.Fatalf("first run matched = %d, want %d", got, want)
	}

	newExternal := f.addExternal(t, "INC-2", 8_000, "2026-06-20")
	f.addLedger(t, "INC-2", 8_000, "2026-06-20")
	second := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")
	if got, want := second.Stats.Matched, 1; got != want {
		t.Fatalf("second run matched = %d, want %d (only the new pair)", got, want)
	}
	if got, want := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE status = 'active'`), int64(2); got != want {
		t.Errorf("active matches = %d, want %d", got, want)
	}
	if row := loadMatch(t, f.db, newExternal); row.MatchRunID != second.RunID {
		t.Errorf("new match run id = %s, want %s", row.MatchRunID, second.RunID)
	}
}

func TestUnmatchFreesRecordsForLaterRun(t *testing.T) {
	f := newFixture(t, "psp_a")
	externalID := f.addExternal(t, "FREE-1", 15_000, "2026-06-10")
	f.addLedger(t, "FREE-1", 15_000, "2026-06-10")
	engine := newEngine(t, f.db, nil)
	first := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

	original := loadMatch(t, f.db, externalID)
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE matches
		    SET status = 'unmatched', unmatched_at = now(),
		        unmatched_by = 'human:tester', unmatch_reason = 'test unmatch'
		  WHERE id = $1::uuid`, original.MatchID); err != nil {
		t.Fatalf("unmatch: %v", err)
	}
	if n := countRows(t, f.db,
		`SELECT count(*) FROM match_legs WHERE status = 'active'`); n != 0 {
		t.Errorf("active legs after unmatch = %d, want 0 (status sync trigger)", n)
	}

	second := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")
	if got, want := second.Stats.Matched, 1; got != want {
		t.Fatalf("run after unmatch matched = %d, want %d", got, want)
	}
	rematched := loadMatch(t, f.db, externalID)
	if rematched.MatchID == original.MatchID {
		t.Errorf("rematched header id = %s, want a new header", rematched.MatchID)
	}
	if rematched.MatchRunID != second.RunID {
		t.Errorf("rematched run id = %s, want %s", rematched.MatchRunID, second.RunID)
	}
	if len(rematched.Legs) != 1 || rematched.Legs[0].Status != "active" {
		t.Errorf("rematched legs = %+v, want one active leg", rematched.Legs)
	}
	if got, want := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE status = 'active'`), int64(1); got != want {
		t.Errorf("active matches = %d, want %d", got, want)
	}
	if got, want := countRows(t, f.db,
		`SELECT count(*) FROM match_runs`), int64(2); got != want {
		t.Errorf("match runs = %d, want %d", got, want)
	}
	if first.RunID == second.RunID {
		t.Error("run ids are equal; every run must have a distinct id")
	}
}
