//go:build integration

// covers: FIN-42

package match_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// addExternalAt inserts one external record into an explicit settlement batch
// and with an explicit event time, so pass 3-5 tests can model declared batch
// membership and same-time duplicates.
func (f *fixture) addExternalAt(t *testing.T, batchID, ref string, amountMinor int64, valueDate string, occurredAt time.Time) string {
	t.Helper()
	n := fixtureSeq.Add(1)
	sourceRecordID := fmt.Sprintf("%s-%d", ref, n)
	rawKey := "reference"
	if f.source == "bank_x" {
		rawKey = "bank_reference"
	}
	raw, err := json.Marshal(map[string]string{rawKey: ref})
	if err != nil {
		t.Fatalf("marshal raw payload: %v", err)
	}
	sum := sha256.Sum256(raw)
	kind := "charge"
	if amountMinor < 0 {
		kind = "payout"
	}
	date := mustDate(t, valueDate)
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ($1, $2, $3, $4, 'USD', 0, $5, $6, $7::uuid, $8::jsonb, $9)
		 RETURNING id::text`,
		f.source, sourceRecordID, kind, amountMinor, occurredAt, date,
		batchID, string(raw), hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert external record %q: %v", ref, err)
	}
	return id
}

func itoa(n int64) string {
	return strconvFormat(n)
}

// strconvFormat avoids importing strconv in every helper call site.
func strconvFormat(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

// TestCompositeRunUsesHeaderAndLegs proves pass 3 matches through the header/leg
// model: one header, N legs, exact sum, and the declared batch winning.
func TestCompositeDeclaredBatchRun(t *testing.T) {
	f := newFixture(t, "psp_a")
	batchID := insertBatch(t, f.db, "psp_a")
	payoutID := f.addExternalAt(t, batchID, "COMP-P", -10_000, "2026-06-10",
		mustDate(t, "2026-06-10").Add(12*time.Hour))
	f.addExternalAt(t, batchID, "COMP-R1", 1, "2026-06-10",
		mustDate(t, "2026-06-10").Add(13*time.Hour))
	f.addExternalAt(t, batchID, "COMP-R2", 2, "2026-06-10",
		mustDate(t, "2026-06-10").Add(14*time.Hour))
	legOne := f.addLedger(t, "COMP-R1", -6_000, "2026-06-10")
	legTwo := f.addLedger(t, "COMP-R2", -4_000, "2026-06-10")
	// An alternative two-leg subset exists; declared membership must win.
	f.addLedger(t, "COMP-ALT1", -5_000, "2026-06-10")
	f.addLedger(t, "COMP-ALT2", -5_000, "2026-06-10")

	engine := newEngine(t, f.db, nil)
	result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

	if got, want := result.Stats.Matched, 1; got != want {
		t.Fatalf("matched = %d, want %d (only the payout; member records stay unmatched)", got, want)
	}
	if got, want := result.Stats.ByPass["composite"], 1; got != want {
		t.Errorf("composite pass count = %d, want %d", got, want)
	}
	row := loadMatch(t, f.db, payoutID)
	if row.Rule != match.RuleCompositeBatch {
		t.Errorf("rule = %q, want %q", row.Rule, match.RuleCompositeBatch)
	}
	if row.Status != "active" || row.MatchRunID != result.RunID {
		t.Errorf("header = %+v, want active with run %s", row, result.RunID)
	}
	if len(row.Legs) != 2 {
		t.Fatalf("legs = %+v, want 2", row.Legs)
	}
	gotLegs := map[string]int64{row.Legs[0].LedgerTxnID: row.Legs[0].AmountMinor, row.Legs[1].LedgerTxnID: row.Legs[1].AmountMinor}
	if gotLegs[legOne] != -6_000 || gotLegs[legTwo] != -4_000 {
		t.Errorf("composite legs = %+v, want %s=-6000 and %s=-4000", gotLegs, legOne, legTwo)
	}
	if got := countRows(t, f.db,
		`SELECT count(*) FROM matches m
		   JOIN external_records e ON e.id = m.external_record_id
		  WHERE m.status = 'active'
		    AND e.amount_minor = (SELECT sum(l.amount_minor) FROM match_legs l
		                           WHERE l.match_id = m.id AND l.status = 'active')`); got != 1 {
		t.Errorf("exact-sum headers = %d, want 1", got)
	}
	if got := countRows(t, f.db,
		`SELECT count(*) FROM matches WHERE external_record_id = $1::uuid AND status = 'active'`,
		payoutID); got != 1 {
		t.Errorf("active headers for the payout = %d, want 1", got)
	}
}

// TestFuzzyRunStoresConfidence proves pass 4 matches and persists its score in
// matches.confidence.
func TestFuzzyRunStoresConfidence(t *testing.T) {
	f := newFixture(t, "psp_a")
	externalID := f.addExternalAt(t, f.batchID, "FUZZ-EXT", 10_000, "2026-06-10",
		mustDate(t, "2026-06-10").Add(12*time.Hour))
	f.addLedger(t, "FUZZ-LEDGER", 10_000, "2026-06-10")

	engine := newEngine(t, f.db, nil)
	result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

	if got, want := result.Stats.ByPass["fuzzy"], 1; got != want {
		t.Fatalf("fuzzy pass count = %d, want %d (stats = %+v)", got, want, result.Stats)
	}
	row := loadMatch(t, f.db, externalID)
	if row.Rule != match.RuleFuzzy {
		t.Errorf("rule = %q, want %q", row.Rule, match.RuleFuzzy)
	}
	if row.Confidence != "0.8000" {
		t.Errorf("confidence = %q, want 0.8000", row.Confidence)
	}
	if len(row.Legs) != 1 {
		t.Errorf("fuzzy legs = %+v, want 1", row.Legs)
	}
}

// TestDuplicateRunProducesBreaks proves pass 5 never writes matches and returns
// duplicate break candidates whose signatures are stable across a rerun.
func TestDuplicateRunProducesBreaks(t *testing.T) {
	f := newFixture(t, "psp_a")
	sameTime := mustDate(t, "2026-06-10").Add(9 * time.Hour)
	f.addExternalAt(t, f.batchID, "DUP-A", 5_000, "2026-06-10", sameTime)
	f.addExternalAt(t, f.batchID, "DUP-B", 5_000, "2026-06-10", sameTime)

	engine := newEngine(t, f.db, nil)
	first := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

	if got, want := first.Stats.Matched, 0; got != want {
		t.Fatalf("duplicates matched = %d, want %d", got, want)
	}
	if got, want := first.Stats.Breaks, 1; got != want {
		t.Fatalf("break candidates = %d, want %d (%+v)", got, want, first.Breaks)
	}
	if got, want := first.Stats.BreaksBySide["duplicate"], 1; got != want {
		t.Errorf("duplicate breaks = %d, want %d", got, want)
	}
	duplicates := findBreaks(first.Breaks, match.SideDuplicate)
	if len(duplicates) != 1 || len(duplicates[0].ExternalRecords) != 2 {
		t.Fatalf("duplicate break = %+v, want two external refs", duplicates)
	}
	if got := countRows(t, f.db, `SELECT count(*) FROM matches`); got != 0 {
		t.Errorf("matches rows = %d, want 0 (duplicates never link)", got)
	}

	second := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")
	if len(second.Breaks) != len(first.Breaks) {
		t.Fatalf("rerun break count = %d, want %d", len(second.Breaks), len(first.Breaks))
	}
	for i := range first.Breaks {
		if first.Breaks[i].Signature() != second.Breaks[i].Signature() {
			t.Errorf("break signature changed on rerun: %s vs %s",
				first.Breaks[i].Signature(), second.Breaks[i].Signature())
		}
	}
}
