//go:build integration

package ledger_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// testNow is the fixed wall-clock reading shared by the ledger tests. No test
// reads time.Now: the clock is always injected (FIN-43).
const testNow = "2026-06-15T12:00:00Z"

var ledgerTxnCounter atomic.Int64

func newLedgerDB(t *testing.T) *testdb.DB {
	t.Helper()
	return testdb.New(t)
}

// openRolePool opens a pool through the named application role. Normal posts
// use recon_app; adjustments use recon_executor because the M1-04 trigger
// requires executor membership.
func openRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	rp, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(rp.Close)
	return rp.Pool
}

func mustActor(t *testing.T, canonical string) audit.Actor {
	t.Helper()
	actor, err := audit.ParseActor(canonical)
	if err != nil {
		t.Fatalf("parse actor %q: %v", canonical, err)
	}
	return actor
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

func fakeClock(t *testing.T) *clock.Fake {
	t.Helper()
	return clock.NewFake(mustTime(t, testNow))
}

// timeZero returns the zero time, used to assert the as-of guard.
func timeZero() time.Time { return time.Time{} }

// auditZeroActor is the invalid zero Actor; Post must reject it.
func auditZeroActor() audit.Actor { return audit.Actor{} }

func testService(t *testing.T, cfg config.LedgerConfig) *ledger.Service {
	t.Helper()
	return ledger.NewService(fakeClock(t), cfg)
}

func newAccount(t *testing.T, d *testdb.DB, code, currency, typ, status string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id::text`,
		code, "Account "+code, typ, currency, status).Scan(&id); err != nil {
		t.Fatalf("create account %q: %v", code, err)
	}
	return id
}

func newPeriod(t *testing.T, d *testdb.DB, startDate, endDate, status string) string {
	t.Helper()
	var closedAt any
	if status == "closed" {
		closedAt = mustTime(t, testNow)
	}
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO periods (start_date, end_date, status, closed_at)
		 VALUES ($1::date, $2::date, $3, $4)
		 RETURNING id::text`,
		startDate, endDate, status, closedAt).Scan(&id); err != nil {
		t.Fatalf("create %s period %s..%s: %v", status, startDate, endDate, err)
	}
	return id
}

// seededOpenPeriodID returns the broad open period created by the M1-03 seed.
func seededOpenPeriodID(t *testing.T, d *testdb.DB) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT id::text FROM periods WHERE status = 'open' ORDER BY start_date LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("read seeded open period: %v", err)
	}
	return id
}

func uniqueKey(t *testing.T) string {
	t.Helper()
	return "ledger-test-" + t.Name() + "-" + itoa(ledgerTxnCounter.Add(1))
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// chargeRequest is the canonical balanced two-line charge used by most tests:
// debit the clearing asset, credit revenue.
func chargeRequest(t *testing.T, clearing, revenue string, amount int64, key string) ledger.PostRequest {
	t.Helper()
	return ledger.PostRequest{
		IdempotencyKey: key,
		Kind:           ledger.KindCharge,
		ValueDate:      mustDate(t, "2026-06-01"),
		Description:    "test charge",
		Actor:          mustActor(t, "system:executor"),
		Lines: []ledger.Line{
			{AccountID: clearing, Direction: ledger.Debit, AmountMinor: amount, Currency: "USD"},
			{AccountID: revenue, Direction: ledger.Credit, AmountMinor: amount, Currency: "USD"},
		},
	}
}

// postCommitted posts req through app and commits, returning the result.
func postCommitted(t *testing.T, d *testdb.DB, app *pgxpool.Pool, svc *ledger.Service, req ledger.PostRequest) ledger.Result {
	t.Helper()
	var result ledger.Result
	if err := db.WithTx(context.Background(), app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = svc.Post(ctx, tx, req)
		return err
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	return result
}

// postError returns the error from posting req through app. It never fails the
// test: callers assert the typed error.
func postError(t *testing.T, d *testdb.DB, app *pgxpool.Pool, svc *ledger.Service, req ledger.PostRequest) error {
	t.Helper()
	return db.WithTx(context.Background(), app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.Post(ctx, tx, req)
		return err
	})
}

func countRows(t *testing.T, d *testdb.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := d.Pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}
