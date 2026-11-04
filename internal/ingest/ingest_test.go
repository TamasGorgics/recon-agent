//go:build integration

package ingest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
)

// covers: FIN-40, FIN-42, FIN-50
func TestIngestAcceptedFile(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	records := []ingest.Record{
		charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z"), // 11:00 EDT Monday
		charge(t, "ch_002", 2000, "2026-06-15T22:00:00Z"), // 18:00 EDT Monday
	}
	parser := stubParser{result: parseResult(records, 3000, 2, statement(t, "2026-06-16", 5000, 8000))}

	result := mustIngest(t, f, "psp_a-2026-06-16.csv", mustData("file", "one"), parser)
	if result.Status != ingest.StatusIngested {
		t.Fatalf("status = %q, want ingested (reason: %s)", result.Status, result.QuarantineReason)
	}
	if result.NoOp {
		t.Error("first ingest reported NoOp")
	}
	if result.BatchID == "" {
		t.Fatal("result has no batch id")
	}
	if result.Parsed != 2 || result.Inserted != 2 || result.Skipped != 0 {
		t.Errorf("counts parsed/inserted/skipped = %d/%d/%d, want 2/2/0",
			result.Parsed, result.Inserted, result.Skipped)
	}

	status, reason, rowCount, declaredRows := f.batch(t, result.BatchID)
	if status != string(ingest.StatusIngested) {
		t.Errorf("stored status = %q, want ingested", status)
	}
	if reason != "" {
		t.Errorf("stored quarantine_reason = %q, want empty", reason)
	}
	if rowCount != 2 || declaredRows != 2 {
		t.Errorf("stored counts = %d/%d, want 2/2", rowCount, declaredRows)
	}

	if got := f.count(t, `SELECT count(*) FROM external_records WHERE batch_id = $1::uuid`, result.BatchID); got != 2 {
		t.Errorf("external_records = %d, want 2", got)
	}
	// FIN-40/FIN-42: occurred_at stays a UTC TIMESTAMPTZ while value_date is a
	// DATE derived from the source calendar (before/after the 17:00 cutoff).
	var occurredAt, valueDate string
	err := f.db.Pool.QueryRow(context.Background(),
		`SELECT to_char(occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		        to_char(value_date, 'YYYY-MM-DD')
		   FROM external_records WHERE source = 'psp_a' AND source_record_id = 'ch_001'`).
		Scan(&occurredAt, &valueDate)
	if err != nil {
		t.Fatalf("read first record: %v", err)
	}
	if occurredAt != "2026-06-15T15:00:00Z" {
		t.Errorf("occurred_at = %s, want 2026-06-15T15:00:00Z", occurredAt)
	}
	if valueDate != "2026-06-15" {
		t.Errorf("value_date = %s, want 2026-06-15", valueDate)
	}
	err = f.db.Pool.QueryRow(context.Background(),
		`SELECT to_char(value_date, 'YYYY-MM-DD') FROM external_records
		  WHERE source = 'psp_a' AND source_record_id = 'ch_002'`).Scan(&valueDate)
	if err != nil {
		t.Fatalf("read second record: %v", err)
	}
	if valueDate != "2026-06-16" {
		t.Errorf("after-cutoff value_date = %s, want 2026-06-16", valueDate)
	}

	if got := f.count(t, `SELECT count(*) FROM statement_balances WHERE batch_id = $1::uuid`, result.BatchID); got != 1 {
		t.Errorf("statement_balances = %d, want 1", got)
	}
	audits, action, _, _ := f.auditRows(t, result.BatchID)
	if audits != 1 {
		t.Errorf("audit rows = %d, want exactly 1", audits)
	}
	if action != ingest.ActionIngest {
		t.Errorf("audit action = %q, want %q", action, ingest.ActionIngest)
	}
}

// covers: FIN-51
func TestIngestSameFileIsNoOp(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")
	data := mustData("same", "file")
	parser := stubParser{result: parseResult([]ingest.Record{
		charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z"),
		charge(t, "ch_002", 2000, "2026-06-15T15:00:00Z"),
	}, 3000, 2, statement(t, "2026-06-15", 0, 3000))}

	first := mustIngest(t, f, "first.csv", data, parser)
	if first.Status != ingest.StatusIngested {
		t.Fatalf("first status = %q, want ingested (%s)", first.Status, first.QuarantineReason)
	}

	second := mustIngest(t, f, "renamed-copy.csv", data, parser)
	if !second.NoOp {
		t.Fatal("second ingest of identical bytes was not a no-op")
	}
	if second.BatchID != first.BatchID {
		t.Errorf("no-op batch id = %s, want original %s", second.BatchID, first.BatchID)
	}
	if second.Status != ingest.StatusIngested {
		t.Errorf("no-op status = %q, want ingested", second.Status)
	}

	if got := f.count(t, `SELECT count(*) FROM settlement_batches`); got != 1 {
		t.Errorf("settlement_batches = %d, want 1", got)
	}
	if got := f.count(t, `SELECT count(*) FROM external_records`); got != 2 {
		t.Errorf("external_records = %d, want 2", got)
	}
	if got := f.count(t, `SELECT count(*) FROM statement_balances`); got != 1 {
		t.Errorf("statement_balances = %d, want 1", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 1 {
		t.Errorf("audit_log rows = %d, want 1 (no duplicate audit mutation)", got)
	}
}

// covers: FIN-51
func TestIngestDuplicateRecordAcrossFilesSkipped(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	first := mustIngest(t, f, "first.csv", mustData("file", "one"),
		stubParser{result: parseResult([]ingest.Record{
			charge(t, "ch_001", 1000, "2026-06-15T15:00:00Z"),
			charge(t, "ch_002", 2000, "2026-06-15T15:00:00Z"),
		}, 3000, 2)})
	if first.Status != ingest.StatusIngested {
		t.Fatalf("first status = %q, want ingested (%s)", first.Status, first.QuarantineReason)
	}

	second := mustIngest(t, f, "second.csv", mustData("file", "two"),
		stubParser{result: parseResult([]ingest.Record{
			charge(t, "ch_002", 2000, "2026-06-15T15:00:00Z"), // re-sent
			charge(t, "ch_003", 3000, "2026-06-15T15:00:00Z"), // new
		}, 5000, 2)})
	if second.Status != ingest.StatusIngested {
		t.Fatalf("second status = %q, want ingested (%s)", second.Status, second.QuarantineReason)
	}
	if second.Parsed != 2 || second.Inserted != 1 || second.Skipped != 1 {
		t.Errorf("counts parsed/inserted/skipped = %d/%d/%d, want 2/1/1",
			second.Parsed, second.Inserted, second.Skipped)
	}
	_, _, rowCount, declaredRows := f.batch(t, second.BatchID)
	if rowCount != 1 || declaredRows != 2 {
		t.Errorf("stored counts = %d/%d, want inserted/declared = 1/2", rowCount, declaredRows)
	}
	if got := f.count(t, `SELECT count(*) FROM external_records`); got != 3 {
		t.Errorf("external_records = %d, want 3 (the re-sent record must not duplicate)", got)
	}
	if got := f.count(t, `SELECT count(*) FROM external_records WHERE source_record_id = 'ch_002'`); got != 1 {
		t.Errorf("ch_002 rows = %d, want 1", got)
	}

	audits, _, skipReason, skippedCount := f.auditRows(t, second.BatchID)
	if audits != 1 {
		t.Errorf("audit rows = %d, want 1", audits)
	}
	if skippedCount != "1" {
		t.Errorf("audit skipped_row_count = %q, want 1", skippedCount)
	}
	if !strings.Contains(strings.ToLower(skipReason), "duplicate") {
		t.Errorf("audit skip_reason = %q, want it to mention the duplicate reason", skipReason)
	}
}

// covers: FIN-50
func TestIngestEmptyFile(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	result := mustIngest(t, f, "empty.csv", []byte{}, stubParser{result: parseResult(nil, 0, 0)})
	if result.Status != ingest.StatusIngested {
		t.Fatalf("status = %q, want ingested (%s)", result.Status, result.QuarantineReason)
	}
	if result.Parsed != 0 || result.Inserted != 0 || result.Skipped != 0 {
		t.Errorf("counts = %d/%d/%d, want 0/0/0", result.Parsed, result.Inserted, result.Skipped)
	}
	_, _, rowCount, declaredRows := f.batch(t, result.BatchID)
	if rowCount != 0 || declaredRows != 0 {
		t.Errorf("stored counts = %d/%d, want 0/0", rowCount, declaredRows)
	}
}

func TestIngestParseErrorWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.seedScope(t, testSource, "USD")

	_, err := f.ing.Ingest(context.Background(), ingest.Request{
		Source:   testSource,
		FileName: "broken.csv",
		Data:     mustData("broken"),
		Parser:   stubParser{err: errParser},
	})
	if !errors.Is(err, ingest.ErrParse) {
		t.Fatalf("Ingest error = %v, want ErrParse", err)
	}
	if got := f.count(t, `SELECT count(*) FROM settlement_batches`); got != 0 {
		t.Errorf("settlement_batches = %d, want 0 after a parse error", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Errorf("audit rows = %d, want 0 after a parse error", got)
	}
}

func TestIngestUnknownSourceWritesNothing(t *testing.T) {
	f := newFixture(t)
	_, err := f.ing.Ingest(context.Background(), ingest.Request{
		Source:   "not_configured",
		FileName: "x.csv",
		Data:     mustData("x"),
		Parser:   stubParser{result: parseResult(nil, 0, 0)},
	})
	if !errors.Is(err, ingest.ErrUnknownSource) {
		t.Fatalf("Ingest error = %v, want ErrUnknownSource", err)
	}
	if got := f.count(t, `SELECT count(*) FROM settlement_batches`); got != 0 {
		t.Errorf("settlement_batches = %d, want 0", got)
	}
}
