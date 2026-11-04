//go:build integration

// covers: FIN-42

package match_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCompositeHeaderAndLegsReadBack(t *testing.T) {
	f := newFixture(t, "psp_a")
	externalID := f.addExternal(t, "COMP-1", 10_000, "2026-06-10")
	firstLedger := f.addLedger(t, "COMP-1", 6_000, "2026-06-10")
	secondLedger := f.addLedger(t, "COMP-1", 4_000, "2026-06-10")
	runID := addRun(t, f.db, f.scopeID)

	var matchID string
	execInTx(t, f.db, func(tx pgx.Tx) {
		matchID = insertMatch(t, tx, f.scopeID, externalID, runID, "composite", "active", "1.0000")
		insertLeg(t, tx, matchID, firstLedger, 6_000, "active")
		insertLeg(t, tx, matchID, secondLedger, 4_000, "active")
	})

	row := loadMatch(t, f.db, externalID)
	if row.MatchID != matchID || row.Rule != "composite" || row.Status != "active" {
		t.Fatalf("header read back = %+v, want composite active %s", row, matchID)
	}
	if len(row.Legs) != 2 {
		t.Fatalf("legs = %+v, want 2", row.Legs)
	}
	var sum int64
	for _, leg := range row.Legs {
		sum += leg.AmountMinor
		if leg.Currency != "USD" || leg.Status != "active" {
			t.Errorf("leg = %+v, want active USD", leg)
		}
	}
	if sum != 10_000 {
		t.Errorf("leg sum = %d, want 10000", sum)
	}

	// The scoring/tie-out shape: header joined to its legs, compared with the
	// external total. This is the query M1-15 will rely on for membership.
	scored := countRows(t, f.db,
		`SELECT count(*)
		   FROM matches m
		   JOIN external_records e ON e.id = m.external_record_id
		  WHERE m.status = 'active'
		    AND e.amount_minor = (
		        SELECT sum(l.amount_minor)
		          FROM match_legs l
		         WHERE l.match_id = m.id AND l.status = 'active'
		    )`)
	if scored != 1 {
		t.Errorf("scoring query matched %d headers, want 1", scored)
	}
}

func TestActiveUniquenessConstraints(t *testing.T) {
	f := newFixture(t, "psp_a")
	externalID := f.addExternal(t, "UNIQ-1", 5_000, "2026-06-10")
	otherExternalID := f.addExternal(t, "UNIQ-2", 6_000, "2026-06-10")
	ledgerID := f.addLedger(t, "UNIQ-1", 5_000, "2026-06-10")
	otherLedgerID := f.addLedger(t, "UNIQ-2", 6_000, "2026-06-10")
	runID := addRun(t, f.db, f.scopeID)

	var headerID string
	execInTx(t, f.db, func(tx pgx.Tx) {
		headerID = insertMatch(t, tx, f.scopeID, externalID, runID, "exact", "active", "1.0000")
		insertLeg(t, tx, headerID, ledgerID, 5_000, "active")
	})

	err := txError(t, f.db, func(tx pgx.Tx) error {
		_, err := insertMatchDB(tx, f.scopeID, externalID, runID, "exact", "active", "1.0000")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "matches_external_active_uidx") {
		t.Fatalf("duplicate active header error = %v, want partial unique violation", err)
	}

	var inactiveHeader string
	execInTx(t, f.db, func(tx pgx.Tx) {
		inactiveHeader = insertMatch(t, tx, f.scopeID, externalID, runID, "exact", "unmatched", "1.0000")
	})
	if inactiveHeader == "" {
		t.Fatal("inactive duplicate header was not inserted")
	}

	err = txError(t, f.db, func(tx pgx.Tx) error {
		return insertLegDB(tx, inactiveHeader, ledgerID, 5_000, "active")
	})
	if err == nil || !strings.Contains(err.Error(), "match_legs_ledger_active_uidx") {
		t.Fatalf("duplicate active leg error = %v, want partial unique violation", err)
	}

	err = txError(t, f.db, func(tx pgx.Tx) error {
		return insertLegDB(tx, headerID, ledgerID, 5_000, "unmatched")
	})
	if err == nil || !strings.Contains(err.Error(), "match_legs_match_ledger_key") {
		t.Fatalf("duplicate (match, ledger) error = %v, want composite unique violation", err)
	}

	var otherHeader string
	execInTx(t, f.db, func(tx pgx.Tx) {
		otherHeader = insertMatch(t, tx, f.scopeID, otherExternalID, runID, "exact", "active", "1.0000")
		insertLeg(t, tx, otherHeader, otherLedgerID, 6_000, "active")
	})
	if otherHeader == "" {
		t.Fatal("independent active header was not inserted")
	}
	if got, want := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE status = 'active'`), int64(2); got != want {
		t.Errorf("active matches = %d, want %d", got, want)
	}
}

func TestStatusSyncTriggerAndUnmatchFreesActiveKeys(t *testing.T) {
	f := newFixture(t, "psp_a")
	externalID := f.addExternal(t, "SYNC-1", 9_000, "2026-06-10")
	ledgerID := f.addLedger(t, "SYNC-1", 9_000, "2026-06-10")
	runID := addRun(t, f.db, f.scopeID)

	var originalHeader string
	execInTx(t, f.db, func(tx pgx.Tx) {
		originalHeader = insertMatch(t, tx, f.scopeID, externalID, runID, "exact", "active", "1.0000")
		insertLeg(t, tx, originalHeader, ledgerID, 9_000, "active")
	})

	execInTx(t, f.db, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(),
			`UPDATE matches
			    SET status = 'unmatched', unmatched_at = now(),
			        unmatched_by = 'human:tester', unmatch_reason = 'manual'
			  WHERE id = $1::uuid`, originalHeader); err != nil {
			t.Fatalf("unmatch header: %v", err)
		}
	})

	if got := countRows(t, f.db,
		`SELECT count(*) FROM match_legs WHERE match_id = $1::uuid AND status = 'unmatched'`,
		originalHeader); got != 1 {
		t.Errorf("legs synced to unmatched = %d, want 1", got)
	}
	if got := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE id = $1::uuid AND unmatched_at IS NOT NULL`,
		originalHeader); got != 1 {
		t.Errorf("header unmatched_at not recorded")
	}

	var replacementHeader string
	execInTx(t, f.db, func(tx pgx.Tx) {
		replacementHeader = insertMatch(t, tx, f.scopeID, externalID, runID, "exact", "active", "1.0000")
		insertLeg(t, tx, replacementHeader, ledgerID, 9_000, "active")
	})
	if replacementHeader == originalHeader {
		t.Fatal("replacement header reused the old id")
	}
	if got, want := countRows(t, f.db,
		`SELECT count(*) FROM match_legs WHERE ledger_txn_id = $1::uuid AND status = 'active'`, ledgerID), int64(1); got != want {
		t.Errorf("active legs for ledger txn = %d, want %d", got, want)
	}
}

func TestMatchRunsAndStatsShape(t *testing.T) {
	f := newFixture(t, "psp_a")
	externalID := f.addExternal(t, "RUN-1", 4_000, "2026-06-10")
	f.addLedger(t, "RUN-1", 4_000, "2026-06-10")
	engine := newEngine(t, f.db, nil)
	result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

	var status, configHash, stats string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status, config_hash, stats::text FROM match_runs WHERE id = $1::uuid`,
		result.RunID).Scan(&status, &configHash, &stats); err != nil {
		t.Fatalf("load match run: %v", err)
	}
	if status != "succeeded" {
		t.Errorf("match run status = %q, want succeeded", status)
	}
	if configHash == "" || configHash != result.ConfigHash {
		t.Errorf("config hash = %q, want %q", configHash, result.ConfigHash)
	}
	if !strings.Contains(stats, `"external_candidates": 1`) && !strings.Contains(stats, `"external_candidates":1`) {
		t.Errorf("stats do not carry candidate counts: %s", stats)
	}
	if got := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE external_record_id = $1::uuid AND match_run_id = $2::uuid`,
		externalID, result.RunID); got != 1 {
		t.Errorf("matches carrying the run id = %d, want 1", got)
	}
}
