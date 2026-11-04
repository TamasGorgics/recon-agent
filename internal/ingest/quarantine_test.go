//go:build integration

package ingest_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
)

// covers: FIN-50
func TestQuarantineControlTotal(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	parser := stubParser{result: parseResult([]ingest.Record{
		charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z"),
		charge(t, "ch_002", 2000, "2026-06-15T15:00:00Z"),
	}, 9999, 2, statement(t, "2026-06-15", 0, 3000))}
	result := mustIngest(t, f, "bad-total.csv", mustData("bad", "total"), parser)

	f.assertQuarantined(t, result, "control total")
	_, _, _, declaredRows := f.batch(t, result.BatchID)
	if declaredRows != 2 {
		t.Errorf("declared_row_count = %d, want 2", declaredRows)
	}
	if got := f.count(t, `SELECT count(*) FROM external_records`); got != 0 {
		t.Errorf("external_records total = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM statement_balances`); got != 0 {
		t.Errorf("statement_balances total = %d, want 0", got)
	}
}

// covers: FIN-50
func TestQuarantineRowCount(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	parser := stubParser{result: parseResult([]ingest.Record{
		charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z"),
		charge(t, "ch_002", 2000, "2026-06-15T15:00:00Z"),
	}, 3000, 3)}
	result := mustIngest(t, f, "bad-count.csv", mustData("bad", "count"), parser)

	f.assertQuarantined(t, result, "row count")
	_, _, _, declaredRows := f.batch(t, result.BatchID)
	if declaredRows != 3 {
		t.Errorf("declared_row_count = %d, want 3", declaredRows)
	}
}

// covers: FIN-50, FIN-51
func TestDuplicateFileAfterQuarantineIsNoOp(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")
	data := mustData("quarantined", "file")
	parser := stubParser{result: parseResult([]ingest.Record{
		charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z"),
	}, 500, 1)}

	first := mustIngest(t, f, "first.csv", data, parser)
	f.assertQuarantined(t, first, "control total")

	second := mustIngest(t, f, "second.csv", data, parser)
	if !second.NoOp {
		t.Fatal("re-ingesting a quarantined file was not a no-op")
	}
	if second.BatchID != first.BatchID {
		t.Errorf("no-op batch id = %s, want original %s", second.BatchID, first.BatchID)
	}
	if second.Status != ingest.StatusQuarantined {
		t.Errorf("no-op status = %q, want quarantined", second.Status)
	}
	if got := f.count(t, `SELECT count(*) FROM settlement_batches`); got != 1 {
		t.Errorf("settlement_batches = %d, want 1", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 1 {
		t.Errorf("audit rows = %d, want 1 (no duplicate quarantine audit)", got)
	}
}

// covers: FIN-50
func TestQuarantineUnknownScope(t *testing.T) {
	f := newFixture(t)
	// Deliberately no threshold scope for psp_a/USD.

	parser := stubParser{result: parseResult([]ingest.Record{
		charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z"),
	}, 1000, 1)}
	result := mustIngest(t, f, "unknown-scope.csv", mustData("unknown", "scope"), parser)

	f.assertQuarantined(t, result, "unknown reconciliation scope")
	if got := f.count(t, `SELECT count(*) FROM external_records`); got != 0 {
		t.Errorf("external_records = %d, want 0", got)
	}
}

// covers: FIN-50
func TestQuarantineCurrencyMismatch(t *testing.T) {
	t.Run("record currency", func(t *testing.T) {
		f := newFixture(t)
		f.seedScope(t, testSource, "USD")

		record := charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z")
		record.Currency = "EUR"
		parser := stubParser{result: parseResult([]ingest.Record{record}, 1000, 1)}
		result := mustIngest(t, f, "wrong-record-currency.csv", mustData("wrong", "record"), parser)

		f.assertQuarantined(t, result, "currency")
	})

	t.Run("statement currency", func(t *testing.T) {
		f := newFixture(t)
		f.seedScope(t, testSource, "USD")

		badStatement := statement(t, "2026-06-15", 0, 1000)
		badStatement.Currency = "EUR"
		parser := stubParser{result: parseResult([]ingest.Record{
			charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z"),
		}, 1000, 1, badStatement)}
		result := mustIngest(t, f, "wrong-statement-currency.csv", mustData("wrong", "statement"), parser)

		f.assertQuarantined(t, result, "currency")
	})
}

// covers: FIN-55
func TestQuarantineRestatement(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	first := mustIngest(t, f, "day-one.csv", mustData("day", "one"),
		stubParser{result: parseResult(nil, 0, 0, statement(t, "2026-06-15", 1000, 2000))})
	if first.Status != ingest.StatusIngested {
		t.Fatalf("baseline status = %q, want ingested (%s)", first.Status, first.QuarantineReason)
	}

	restated := mustIngest(t, f, "day-one-restated.csv", mustData("day", "one", "restated"),
		stubParser{result: parseResult(nil, 0, 0, statement(t, "2026-06-15", 1000, 2500))})
	f.assertQuarantined(t, restated, "restatement")

	if got := f.count(t, `SELECT count(*) FROM statement_balances`); got != 1 {
		t.Errorf("statement_balances = %d, want 1 (restatement must not insert)", got)
	}

	var closing int64
	if err := f.db.Pool.QueryRow(t.Context(),
		`SELECT closing_minor FROM statement_balances WHERE as_of_date = '2026-06-15'`).Scan(&closing); err != nil {
		t.Fatalf("read statement: %v", err)
	}
	if closing != 2000 {
		t.Errorf("closing_minor = %d, want the original 2000", closing)
	}
}
