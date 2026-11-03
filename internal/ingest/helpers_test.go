//go:build integration

package ingest_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const testSource = "psp_a"

var ingestTestNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

type fixture struct {
	db  *testdb.DB
	app *pgxpool.Pool
	ing *ingest.Ingester
}

// newFixture opens a fresh test database and an ingester that runs through the
// recon_app role, so the M1-07 grant matrix is exercised by every pipeline test.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testdb.New(t)
	rolePool, err := db.OpenRole(context.Background(), db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(rolePool.Close)

	actor, err := audit.SystemActor("ingest")
	if err != nil {
		t.Fatalf("system actor: %v", err)
	}
	ing, err := ingest.New(ingest.Options{
		DB:      rolePool.Pool,
		Clock:   clock.NewFake(ingestTestNow),
		Actor:   actor,
		Sources: testSourceConfig(),
	})
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}
	return &fixture{db: d, app: rolePool.Pool, ing: ing}
}

// seedScope inserts the clearing account and reconciliation scope a source and
// currency need. M1-09's load does this in production; tests seed directly.
func (f *fixture) seedScope(t *testing.T, source, currency string) string {
	t.Helper()
	ctx := context.Background()
	code := "clearing_" + source + "_" + strings.ToLower(currency)
	var accountID string
	err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency)
		 VALUES ($1, $1, 'asset', $2)
		 RETURNING id::text`, code, currency).Scan(&accountID)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	var scopeID string
	err = f.db.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, $3)
		 RETURNING id::text`, accountID, source, currency).Scan(&scopeID)
	if err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	return scopeID
}

type stubParser struct {
	result ingest.ParseResult
	err    error
}

func (p stubParser) Parse(io.Reader) (ingest.ParseResult, error) {
	return p.result, p.err
}

func mustIngest(t *testing.T, f *fixture, fileName string, data []byte, parser ingest.Parser) ingest.Result {
	t.Helper()
	result, err := f.ing.Ingest(context.Background(), ingest.Request{
		Source:   testSource,
		FileName: fileName,
		Data:     data,
		Parser:   parser,
	})
	if err != nil {
		t.Fatalf("Ingest(%s): %v", fileName, err)
	}
	return result
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	if err := f.db.Pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return count
}

func (f *fixture) batch(t *testing.T, batchID string) (status, reason string, rowCount, declaredRowCount int) {
	t.Helper()
	var maybeReason *string
	err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status, quarantine_reason, row_count, declared_row_count
		   FROM settlement_batches WHERE id = $1::uuid`, batchID).
		Scan(&status, &maybeReason, &rowCount, &declaredRowCount)
	if err != nil {
		t.Fatalf("read batch %s: %v", batchID, err)
	}
	if maybeReason != nil {
		reason = *maybeReason
	}
	return status, reason, rowCount, declaredRowCount
}

func (f *fixture) auditRows(t *testing.T, batchID string) (count int, action, skipReason, skippedCount string) {
	t.Helper()
	var maybeSkip, maybeCount *string
	err := f.db.Pool.QueryRow(context.Background(),
		`SELECT count(*), min(action), min(after->>'skip_reason'), min(after->>'skipped_row_count')
		   FROM audit_log WHERE entity_id = $1`, batchID).
		Scan(&count, &action, &maybeSkip, &maybeCount)
	if err != nil {
		t.Fatalf("read audit rows for batch %s: %v", batchID, err)
	}
	if maybeSkip != nil {
		skipReason = *maybeSkip
	}
	if maybeCount != nil {
		skippedCount = *maybeCount
	}
	return count, action, skipReason, skippedCount
}

// assertQuarantined checks the FIN-50/FIN-55 invariant: a rejected batch
// persists exactly once with a reason, inserts zero records and zero
// statements, and appends exactly one audit entry.
func (f *fixture) assertQuarantined(t *testing.T, result ingest.Result, wantReason string) {
	t.Helper()
	if result.Status != ingest.StatusQuarantined {
		t.Fatalf("status = %q, want quarantined (result: %+v)", result.Status, result)
	}
	if result.NoOp {
		t.Fatal("result unexpectedly reports NoOp")
	}
	if result.BatchID == "" {
		t.Fatal("quarantined result has no batch id")
	}
	if !strings.Contains(strings.ToLower(result.QuarantineReason), strings.ToLower(wantReason)) {
		t.Fatalf("quarantine reason = %q, want it to mention %q", result.QuarantineReason, wantReason)
	}
	status, reason, rowCount, _ := f.batch(t, result.BatchID)
	if status != string(ingest.StatusQuarantined) {
		t.Errorf("stored status = %q, want quarantined", status)
	}
	if strings.TrimSpace(reason) == "" {
		t.Error("stored quarantine_reason is empty")
	}
	if rowCount != 0 {
		t.Errorf("stored row_count = %d, want 0", rowCount)
	}
	if got := f.count(t, `SELECT count(*) FROM external_records WHERE batch_id = $1::uuid`, result.BatchID); got != 0 {
		t.Errorf("external_records for quarantined batch = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM statement_balances WHERE batch_id = $1::uuid`, result.BatchID); got != 0 {
		t.Errorf("statement_balances for quarantined batch = %d, want 0", got)
	}
	audits, action, _, _ := f.auditRows(t, result.BatchID)
	if audits != 1 {
		t.Errorf("audit rows for quarantined batch = %d, want exactly 1", audits)
	}
	if action != ingest.ActionQuarantine {
		t.Errorf("audit action = %q, want %q", action, ingest.ActionQuarantine)
	}
}

func charge(t *testing.T, id string, amountMinor int64, occurredAt string) ingest.Record {
	t.Helper()
	return ingest.Record{
		SourceRecordID: id,
		Kind:           ingest.KindCharge,
		AmountMinor:    amountMinor,
		Currency:       "USD",
		OccurredAt:     utc(t, occurredAt),
		Raw:            []byte(`{"memo":"test"}`),
	}
}

func parseResult(records []ingest.Record, declaredTotal int64, declaredRows int, statements ...ingest.Statement) ingest.ParseResult {
	return ingest.ParseResult{
		Records:    records,
		Declared:   ingest.Declared{TotalMinor: declaredTotal, RowCount: declaredRows, Currency: "USD"},
		Statements: statements,
	}
}

func statement(t *testing.T, asOf string, opening, closing int64) ingest.Statement {
	t.Helper()
	return ingest.Statement{AsOfDate: date(t, asOf), OpeningMinor: opening, ClosingMinor: closing, Currency: "USD"}
}

func mustData(parts ...string) []byte { return []byte(strings.Join(parts, "|")) }

var errParser = errors.New("synthetic parser failure")
