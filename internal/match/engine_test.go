//go:build integration

// covers: FIN-42

package match_test

import (
	"context"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

func TestRunExactToleranceAndWindow(t *testing.T) {
	f := newFixture(t, "psp_a")
	exactExternal := f.addExternal(t, "P-EXACT", 10_000, "2026-06-10")
	exactLedger := f.addLedger(t, "P-EXACT", 10_000, "2026-06-10")

	toleranceExternal := f.addExternal(t, "P-TOL", 20_000, "2026-06-11")
	toleranceLedger := f.addLedger(t, "P-TOL", 19_950, "2026-06-11")

	outsideToleranceExternal := f.addExternal(t, "P-OUT", 30_000, "2026-06-12")
	f.addLedger(t, "P-OUT", 29_700, "2026-06-12")

	outsideWindowExternal := f.addExternal(t, "P-LATE", 40_000, "2026-07-05")
	f.addLedger(t, "P-LATE", 40_000, "2026-07-05")

	engine := newEngine(t, f.db, nil)
	result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

	if result.Stats.ExternalCandidates != 3 {
		t.Errorf("external candidates = %d, want 3 (window filters the July record)", result.Stats.ExternalCandidates)
	}
	if result.Stats.LedgerCandidates != 3 {
		t.Errorf("ledger candidates = %d, want 3 (window filters the July transaction)", result.Stats.LedgerCandidates)
	}
	if got, want := result.Stats.Matched, 2; got != want {
		t.Errorf("matched = %d, want %d", got, want)
	}
	if got, want := result.Stats.ByPass["exact"], 1; got != want {
		t.Errorf("exact pass count = %d, want %d", got, want)
	}
	if got, want := result.Stats.ByPass["reference_tolerance"], 1; got != want {
		t.Errorf("tolerance pass count = %d, want %d", got, want)
	}
	if got, want := result.Stats.UnmatchedExternal, 1; got != want {
		t.Errorf("unmatched external = %d, want %d", got, want)
	}

	exact := loadMatch(t, f.db, exactExternal)
	if exact.Rule != match.RuleExact || exact.Status != "active" || exact.MatchRunID != result.RunID {
		t.Errorf("exact match = %+v, want active exact with run %s", exact, result.RunID)
	}
	if len(exact.Legs) != 1 || exact.Legs[0].LedgerTxnID != exactLedger || exact.Legs[0].AmountMinor != 10_000 || exact.Legs[0].Status != "active" {
		t.Errorf("exact legs = %+v, want one active leg for %s", exact.Legs, exactLedger)
	}

	tolerance := loadMatch(t, f.db, toleranceExternal)
	if tolerance.Rule != match.RuleReferenceTolerance || tolerance.Status != "active" || tolerance.MatchRunID != result.RunID {
		t.Errorf("tolerance match = %+v, want active reference_tolerance with run %s", tolerance, result.RunID)
	}
	if len(tolerance.Legs) != 1 || tolerance.Legs[0].LedgerTxnID != toleranceLedger || tolerance.Legs[0].AmountMinor != 19_950 {
		t.Errorf("tolerance legs = %+v, want one leg for %s", tolerance.Legs, toleranceLedger)
	}

	if n := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE external_record_id = $1::uuid`, outsideToleranceExternal); n != 0 {
		t.Errorf("outside-tolerance external has %d matches, want 0", n)
	}
	if n := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE external_record_id = $1::uuid`, outsideWindowExternal); n != 0 {
		t.Errorf("outside-window external has %d matches, want 0", n)
	}

	if n := countRows(t, f.db,
		`SELECT count(*) FROM audit_log WHERE action = 'match.run' AND entity_id = $1`, result.RunID); n != 1 {
		t.Errorf("audit summaries for run = %d, want exactly 1", n)
	}
	if n := countRows(t, f.db,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'match.run' AND entity_id = $1
		    AND actor_type = 'system' AND actor_id = 'matcher'
		    AND after->'stats'->>'matched' = '2'`, result.RunID); n != 1 {
		t.Errorf("audit summary does not carry the matcher actor and matched count")
	}
}

func TestPass1PrecedenceOverTolerancePass(t *testing.T) {
	f := newFixture(t, "psp_a")
	exactExternal := f.addExternal(t, "PRE-1", 10_000, "2026-06-10")
	exactLedger := f.addLedger(t, "PRE-1", 10_000, "2026-06-10")
	// A second ledger candidate within tolerance is deliberately present. Pass 1
	// must claim the exact amount and pass 2 must not touch the external record.
	f.addLedger(t, "PRE-1", 9_950, "2026-06-10")

	toleranceExternal := f.addExternal(t, "PRE-2", 20_000, "2026-06-11")
	toleranceLedger := f.addLedger(t, "PRE-2", 19_950, "2026-06-11")

	engine := newEngine(t, f.db, nil)
	result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

	exact := loadMatch(t, f.db, exactExternal)
	if exact.Rule != match.RuleExact || len(exact.Legs) != 1 || exact.Legs[0].LedgerTxnID != exactLedger {
		t.Fatalf("pass 1 did not take precedence: %+v", exact)
	}
	tolerance := loadMatch(t, f.db, toleranceExternal)
	if tolerance.Rule != match.RuleReferenceTolerance || len(tolerance.Legs) != 1 || tolerance.Legs[0].LedgerTxnID != toleranceLedger {
		t.Fatalf("pass 2 match = %+v, want reference_tolerance for %s", tolerance, toleranceLedger)
	}
	if n := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE external_record_id = $1::uuid`, exactExternal); n != 1 {
		t.Errorf("external has %d matches, want exactly 1 (pass 2 re-matched pass 1)", n)
	}
	if got, want := result.Stats.ByPass["exact"], 1; got != want {
		t.Errorf("exact pass count = %d, want %d", got, want)
	}
	if got, want := result.Stats.ByPass["reference_tolerance"], 1; got != want {
		t.Errorf("tolerance pass count = %d, want %d", got, want)
	}
}

func TestEngineRunsWithApplicationRole(t *testing.T) {
	f := newFixture(t, "psp_a")
	externalID := f.addExternal(t, "APP-ROLE", 5_000, "2026-06-10")
	f.addLedger(t, "APP-ROLE", 5_000, "2026-06-10")

	ctx := context.Background()
	rolePool, err := db.OpenRole(ctx, db.RoleApp, f.db.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(rolePool.Close)

	engine, err := match.New(match.Options{
		DB:     rolePool.Pool,
		Clock:  testClock(t),
		Actor:  matcherActor(t),
		Config: config.MatchConfig{FeeToleranceMinor: map[string]int64{"USD": usdTolerance}},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")
	if got, want := result.Stats.Matched, 1; got != want {
		t.Fatalf("matched = %d, want %d", got, want)
	}
	if row := loadMatch(t, f.db, externalID); row.MatchRunID != result.RunID {
		t.Errorf("match run id = %s, want %s", row.MatchRunID, result.RunID)
	}
}
