//go:build integration

package ingest_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
)

// covers: FIN-55
func TestContinuityBaselineAndChain(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	// The first statement for a scope is the baseline even when its opening is
	// non-zero; it has no previous closing balance to compare against.
	baseline := mustIngest(t, f, "day-one.csv", mustData("baseline"),
		stubParser{result: parseResult(nil, 0, 0, statement(t, "2026-06-15", 100, 200))})
	if baseline.Status != ingest.StatusIngested {
		t.Fatalf("baseline status = %q, want ingested (%s)", baseline.Status, baseline.QuarantineReason)
	}

	chained := mustIngest(t, f, "day-two.csv", mustData("chained"),
		stubParser{result: parseResult(nil, 0, 0, statement(t, "2026-06-16", 200, 300))})
	if chained.Status != ingest.StatusIngested {
		t.Fatalf("chained status = %q, want ingested (%s)", chained.Status, chained.QuarantineReason)
	}

	if got := f.count(t, `SELECT count(*) FROM statement_balances`); got != 2 {
		t.Errorf("statement_balances = %d, want 2", got)
	}
}

// covers: FIN-55
func TestContinuityGapQuarantines(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	first := mustIngest(t, f, "day-one.csv", mustData("day", "one"),
		stubParser{result: parseResult(nil, 0, 0, statement(t, "2026-06-15", 100, 200))})
	if first.Status != ingest.StatusIngested {
		t.Fatalf("first status = %q, want ingested (%s)", first.Status, first.QuarantineReason)
	}

	// A missing statement day shows up as a discontinuity: this opening does
	// not match day one's closing.
	gap := mustIngest(t, f, "day-three-gap.csv", mustData("day", "three"),
		stubParser{result: parseResult([]ingest.Record{
			charge(t, "ch_001", 500, "2026-06-17T15:00:00Z"),
		}, 500, 1, statement(t, "2026-06-17", 450, 950))})
	f.assertQuarantined(t, gap, "continuity")
	if got := f.count(t, `SELECT count(*) FROM external_records WHERE batch_id = $1::uuid`, gap.BatchID); got != 0 {
		t.Errorf("external_records for gapped batch = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM statement_balances`); got != 1 {
		t.Errorf("statement_balances = %d, want 1 after the gap quarantine", got)
	}

	// The corrected day-two statement chains on day one and is accepted.
	fixed := mustIngest(t, f, "day-two-fixed.csv", mustData("day", "two", "fixed"),
		stubParser{result: parseResult(nil, 0, 0, statement(t, "2026-06-16", 200, 300))})
	if fixed.Status != ingest.StatusIngested {
		t.Fatalf("fixed status = %q, want ingested (%s)", fixed.Status, fixed.QuarantineReason)
	}
	if got := f.count(t, `SELECT count(*) FROM statement_balances`); got != 2 {
		t.Errorf("statement_balances = %d, want 2", got)
	}
}

// covers: FIN-55
func TestContinuityBackfillThatBreaksTheNextStatementQuarantines(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	first := mustIngest(t, f, "day-two.csv", mustData("day", "two"),
		stubParser{result: parseResult(nil, 0, 0, statement(t, "2026-06-16", 200, 300))})
	if first.Status != ingest.StatusIngested {
		t.Fatalf("first status = %q, want ingested (%s)", first.Status, first.QuarantineReason)
	}

	// Backfilling day one is allowed only if it chains into day two. This
	// closing (250) cannot: day two opens at 200.
	backfill := mustIngest(t, f, "day-one-backfill.csv", mustData("day", "one"),
		stubParser{result: parseResult(nil, 0, 0, statement(t, "2026-06-15", 100, 250))})
	f.assertQuarantined(t, backfill, "continuity")

	if got := f.count(t, `SELECT count(*) FROM statement_balances`); got != 1 {
		t.Errorf("statement_balances = %d, want 1 (broken backfill must not insert)", got)
	}
}
