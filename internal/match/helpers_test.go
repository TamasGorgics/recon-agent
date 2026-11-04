//go:build integration

// covers: FIN-42

package match_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const (
	testNow      = "2026-06-15T12:00:00Z"
	usdTolerance = int64(100)
)

var fixtureSeq atomic.Int64

func newDB(t *testing.T) *testdb.DB {
	t.Helper()
	return testdb.New(t)
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

func mustDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		t.Fatalf("parse date %q: %v", value, err)
	}
	return parsed
}

func testClock(t *testing.T) *clock.Fake {
	t.Helper()
	return clock.NewFake(mustTime(t, testNow))
}

func matcherActor(t *testing.T) audit.Actor {
	t.Helper()
	actor, err := audit.ParseActor("system:matcher")
	if err != nil {
		t.Fatalf("parse matcher actor: %v", err)
	}
	return actor
}

// fixture is one reconciliation scope (ledger account + source + currency) with
// its counter account and one ingestion batch for external records.
type fixture struct {
	db        *testdb.DB
	scopeID   string
	accountID string
	counterID string
	batchID   string
	source    string
}

func newFixture(t *testing.T, source string) *fixture {
	t.Helper()
	d := newDB(t)
	accountID := insertAccount(t, d, source+"_clearing", "asset")
	counterID := insertAccount(t, d, source+"_counter", "revenue")
	batchID := insertBatch(t, d, source)
	var scopeID string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, 'USD') RETURNING id::text`,
		accountID, source).Scan(&scopeID); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	return &fixture{db: d, scopeID: scopeID, accountID: accountID, counterID: counterID, batchID: batchID, source: source}
}

func insertAccount(t *testing.T, d *testdb.DB, code, typ string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, 'USD', 'active') RETURNING id::text`,
		code, typ).Scan(&id); err != nil {
		t.Fatalf("insert account %q: %v", code, err)
	}
	return id
}

func insertBatch(t *testing.T, d *testdb.DB, source string) string {
	t.Helper()
	n := fixtureSeq.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", source, n)))
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ($1, 0, 'USD', $2, 0, 0) RETURNING id::text`,
		source, hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	return id
}

// addExternal inserts one external record. The reference lives in the raw
// payload, exactly where the psp_a and bank_x parsers put it.
func (f *fixture) addExternal(t *testing.T, ref string, amountMinor int64, valueDate string) string {
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
		f.source, sourceRecordID, kind, amountMinor, date.Add(12*time.Hour),
		date, f.batchID, string(raw), hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert external record %q: %v", ref, err)
	}
	return id
}

// addLedger inserts a balanced ledger transaction whose scope-account entry
// carries ref and the signed amount. The counter entry keeps the transaction
// balanced so the M1-03 deferred constraint accepts it.
func (f *fixture) addLedger(t *testing.T, ref string, amountMinor int64, valueDate string) string {
	t.Helper()
	ctx := context.Background()
	n := fixtureSeq.Add(1)
	kind := "charge"
	if amountMinor < 0 {
		kind = "payout"
	}
	date := mustDate(t, valueDate)
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin ledger fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ($1, $2, $3, 'fixture', $4, '11111111-1111-4111-8111-111111111111')
		 RETURNING id::text`,
		date, kind, fmt.Sprintf("fixture-%s-%d", ref, n), ref).Scan(&txnID); err != nil {
		t.Fatalf("insert ledger txn %q: %v", ref, err)
	}
	scopeDirection := "debit"
	counterDirection := "credit"
	if amountMinor < 0 {
		scopeDirection, counterDirection = "credit", "debit"
	}
	abs := amountMinor
	if abs < 0 {
		abs = -abs
	}
	for _, entry := range []struct {
		accountID string
		direction string
		ref       any
	}{
		{f.accountID, scopeDirection, ref},
		{f.counterID, counterDirection, nil},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries
			     (txn_id, account_id, direction, amount_minor, currency, external_ref)
			 VALUES ($1::uuid, $2::uuid, $3, $4, 'USD', $5)`,
			txnID, entry.accountID, entry.direction, abs, entry.ref); err != nil {
			t.Fatalf("insert ledger entry for %q: %v", ref, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ledger fixture %q: %v", ref, err)
	}
	return txnID
}

func newEngine(t *testing.T, d *testdb.DB, tolerance map[string]int64) *match.Engine {
	t.Helper()
	if tolerance == nil {
		tolerance = map[string]int64{"USD": usdTolerance}
	}
	engine, err := match.New(match.Options{
		DB:     d.Pool,
		Clock:  testClock(t),
		Actor:  matcherActor(t),
		Config: config.MatchConfig{FeeToleranceMinor: tolerance},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine
}

func runEngine(t *testing.T, engine *match.Engine, scopeID, from, to string) match.RunResult {
	t.Helper()
	result, err := engine.Run(context.Background(), match.RunRequest{
		ScopeID: scopeID,
		Window:  match.Window{From: mustDate(t, from), To: mustDate(t, to)},
	})
	if err != nil {
		t.Fatalf("run matcher: %v", err)
	}
	return result
}

func countRows(t *testing.T, d *testdb.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := d.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v\nquery: %s", err, query)
	}
	return n
}

type matchRow struct {
	MatchID    string
	Rule       string
	Status     string
	Confidence string
	MatchRunID string
	Legs       []legRow
}

type legRow struct {
	LedgerTxnID string
	AmountMinor int64
	Currency    string
	Status      string
}

func loadMatch(t *testing.T, d *testdb.DB, externalID string) matchRow {
	t.Helper()
	ctx := context.Background()
	var row matchRow
	err := d.Pool.QueryRow(ctx,
		`SELECT id::text, rule, status, confidence::text, match_run_id::text
		   FROM matches WHERE external_record_id = $1::uuid
		  ORDER BY (status = 'active') DESC, created_at DESC
		  LIMIT 1`,
		externalID).Scan(&row.MatchID, &row.Rule, &row.Status, &row.Confidence, &row.MatchRunID)
	if err != nil {
		t.Fatalf("load match for external %s: %v", externalID, err)
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT ledger_txn_id::text, amount_minor, currency, status
		   FROM match_legs WHERE match_id = $1::uuid ORDER BY ledger_txn_id`,
		row.MatchID)
	if err != nil {
		t.Fatalf("load legs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var leg legRow
		if err := rows.Scan(&leg.LedgerTxnID, &leg.AmountMinor, &leg.Currency, &leg.Status); err != nil {
			t.Fatalf("scan leg: %v", err)
		}
		row.Legs = append(row.Legs, leg)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate legs: %v", err)
	}
	return row
}

func addRun(t *testing.T, d *testdb.DB, scopeID string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, now(), now(), 'succeeded', '{}'::jsonb, 'fixture')
		 RETURNING id::text`, scopeID).Scan(&id); err != nil {
		t.Fatalf("insert match run: %v", err)
	}
	return id
}

func insertMatch(t *testing.T, tx pgx.Tx, scopeID, externalID, runID, rule, status, confidence string) string {
	t.Helper()
	id, err := insertMatchDB(tx, scopeID, externalID, runID, rule, status, confidence)
	if err != nil {
		t.Fatalf("insert match: %v", err)
	}
	return id
}

func insertMatchDB(tx pgx.Tx, scopeID, externalID, runID, rule, status, confidence string) (string, error) {
	var id string
	err := tx.QueryRow(context.Background(),
		`INSERT INTO matches
		     (scope_id, external_record_id, rule, confidence, match_run_id, status, unmatched_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4::numeric, $5::uuid, $6,
		         CASE WHEN $6 = 'unmatched' THEN now() ELSE NULL END)
		 RETURNING id::text`,
		scopeID, externalID, rule, confidence, runID, status).Scan(&id)
	return id, err
}

func insertLeg(t *testing.T, tx pgx.Tx, matchID, ledgerTxnID string, amountMinor int64, status string) {
	t.Helper()
	if err := insertLegDB(tx, matchID, ledgerTxnID, amountMinor, status); err != nil {
		t.Fatalf("insert leg: %v", err)
	}
}

func insertLegDB(tx pgx.Tx, matchID, ledgerTxnID string, amountMinor int64, status string) error {
	_, err := tx.Exec(context.Background(),
		`INSERT INTO match_legs (match_id, ledger_txn_id, amount_minor, currency, status)
		 VALUES ($1::uuid, $2::uuid, $3, 'USD', $4)`,
		matchID, ledgerTxnID, amountMinor, status)
	return err
}

func execInTx(t *testing.T, d *testdb.DB, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	fn(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func txError(t *testing.T, d *testdb.DB, fn func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	err = fn(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}
