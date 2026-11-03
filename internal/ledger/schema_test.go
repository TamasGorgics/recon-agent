//go:build integration

// covers: FIN-10, FIN-11, FIN-13, FIN-14, FIN-56
package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const (
	sqlStateCheckViolation    = "23514"
	sqlStateUniqueViolation   = "23505"
	sqlStateRestrictViolation = "23001"
)

var schemaTxnCounter atomic.Int64

// schemaAccount is one row of the chart of accounts created by a test.
type schemaAccount struct {
	ID string
}

// schemaEntry is one ledger entry to insert with a transaction.
type schemaEntry struct {
	AccountID   string
	Direction   string
	AmountMinor int64
	Currency    string
}

// schemaTxn describes a ledger transaction and its entries.
type schemaTxn struct {
	ValueDate       string
	Kind            string
	IdempotencyKey  string
	PeriodID        string
	AdjustsPeriodID string
	ReversalOf      string
	Description     string
	Entries         []schemaEntry
}

func newSchemaDB(t *testing.T) *testdb.DB {
	t.Helper()
	return testdb.New(t)
}

func createAccount(t *testing.T, d *testdb.DB, code, currency, typ, status string) schemaAccount {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id::text`,
		code, "Account "+code, typ, currency, status).Scan(&id); err != nil {
		t.Fatalf("create account %q: %v", code, err)
	}
	return schemaAccount{ID: id}
}

// openPeriodID returns the seeded broad open period. Tests that need a narrow
// or closed period create one explicitly and never rely on the seed boundaries.
func openPeriodID(t *testing.T, d *testdb.DB) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT id::text FROM periods WHERE status = 'open' ORDER BY start_date LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("read seeded open period: %v", err)
	}
	return id
}

func createPeriod(t *testing.T, d *testdb.DB, startDate, endDate, status string) string {
	t.Helper()
	ctx := context.Background()
	var closedAt any
	if status == "closed" {
		closedAt = time.Now().UTC()
	}
	var id string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO periods (start_date, end_date, status, closed_at)
		 VALUES ($1::date, $2::date, $3, $4)
		 RETURNING id::text`,
		startDate, endDate, status, closedAt).Scan(&id); err != nil {
		t.Fatalf("create %s period %s..%s: %v", status, startDate, endDate, err)
	}
	return id
}

// insertTxn inserts the ledger_txns row and its ledger_entries rows inside tx.
// Deferred balance checks do not run until the caller commits.
func insertTxn(ctx context.Context, tx pgx.Tx, spec schemaTxn) (string, error) {
	key := spec.IdempotencyKey
	if key == "" {
		key = fmt.Sprintf("schema-test-%d", schemaTxnCounter.Add(1))
	}
	kind := spec.Kind
	if kind == "" {
		kind = "charge"
	}
	periodID := spec.PeriodID
	if periodID == "" {
		if err := tx.QueryRow(ctx,
			`SELECT id::text FROM periods WHERE status = 'open' ORDER BY start_date LIMIT 1`).Scan(&periodID); err != nil {
			return "", fmt.Errorf("resolve open period: %w", err)
		}
	}

	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns (
		     value_date, kind, idempotency_key, request_hash, description,
		     period_id, adjusts_period_id, reversal_of
		 ) VALUES (
		     $1::date, $2, $3, $4, $5,
		     $6::uuid, NULLIF($7, '')::uuid, NULLIF($8, '')::uuid
		 ) RETURNING id::text`,
		spec.ValueDate, kind, key, "hash:"+key, spec.Description,
		periodID, spec.AdjustsPeriodID, spec.ReversalOf).Scan(&id); err != nil {
		return "", err
	}
	for _, e := range spec.Entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
			 VALUES ($1::uuid, $2::uuid, $3, $4, $5)`,
			id, e.AccountID, e.Direction, e.AmountMinor, e.Currency); err != nil {
			return "", err
		}
	}
	return id, nil
}

// postTxn inserts a transaction through db.WithTx so a commit failure is
// reported the same way the ledger service will see it.
func postTxn(ctx context.Context, d *testdb.DB, spec schemaTxn) error {
	return db.WithTx(ctx, d.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertTxn(ctx, tx, spec)
		return err
	})
}

func requirePgError(t *testing.T, err error, code, messageSubstring string) *pgconn.PgError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a Postgres error with SQLSTATE %s, got nil", code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a *pgconn.PgError", err)
	}
	if pgErr.Code != code {
		t.Fatalf("SQLSTATE = %s, want %s (error: %v)", pgErr.Code, code, err)
	}
	if messageSubstring != "" && !strings.Contains(strings.ToLower(pgErr.Message), strings.ToLower(messageSubstring)) {
		t.Fatalf("error message %q does not contain %q", pgErr.Message, messageSubstring)
	}
	return pgErr
}

// TestCurrenciesMatchMoneyTable is the drift guard between the embedded Go
// currency table and the seeded Postgres table (FIN-03).
func TestCurrenciesMatchMoneyTable(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	rows, err := d.Pool.Query(ctx, `SELECT code, exponent FROM currencies ORDER BY code`)
	if err != nil {
		t.Fatalf("query currencies: %v", err)
	}
	defer rows.Close()

	type currencyRow struct {
		code     string
		exponent int16
	}
	var got []currencyRow
	for rows.Next() {
		var row currencyRow
		if err := rows.Scan(&row.code, &row.exponent); err != nil {
			t.Fatalf("scan currency: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate currencies: %v", err)
	}

	want := money.Currencies()
	if len(want) == 0 {
		t.Fatal("money.Currencies() returned no currencies")
	}
	if len(got) != len(want) {
		t.Fatalf("currencies row count = %d, want %d", len(got), len(want))
	}
	for i, currency := range want {
		if got[i].code != currency.Code() || int(got[i].exponent) != currency.Exponent() {
			t.Fatalf("currencies[%d] = {%s,%d}, want {%s,%d}",
				i, got[i].code, got[i].exponent, currency.Code(), currency.Exponent())
		}
	}

	// Explicit spot checks for the exponent extremes required by the roadmap.
	for _, required := range []struct {
		code     string
		exponent int
	}{
		{"JPY", 0},
		{"KWD", 3},
		{"USD", 2},
		{"EUR", 2},
		{"SEK", 2},
		{"GBP", 2},
	} {
		found := false
		for _, row := range got {
			if row.code == required.code {
				found = true
				if int(row.exponent) != required.exponent {
					t.Errorf("currency %s exponent = %d, want %d", required.code, row.exponent, required.exponent)
				}
			}
		}
		if !found {
			t.Errorf("currency %s missing from currencies table", required.code)
		}
	}
}

func TestSeedOpenPeriod(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM periods WHERE status = 'open'`).Scan(&count); err != nil {
		t.Fatalf("count open periods: %v", err)
	}
	if count != 1 {
		t.Fatalf("open period count = %d, want exactly 1 seed period", count)
	}

	var start, end time.Time
	if err := d.Pool.QueryRow(ctx,
		`SELECT start_date, end_date FROM periods WHERE status = 'open'`).Scan(&start, &end); err != nil {
		t.Fatalf("read seeded open period: %v", err)
	}
	if got := start.Format("2006-01-02"); got != "2020-01-01" {
		t.Errorf("seed period start = %s, want 2020-01-01", got)
	}
	if got := end.Format("2006-01-02"); got != "2030-12-31" {
		t.Errorf("seed period end = %s, want 2030-12-31", got)
	}
}

func TestBalancedTransactionCommits(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	clearing := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	if err := postTxn(ctx, d, schemaTxn{
		ValueDate: "2026-01-15",
		Kind:      "charge",
		Entries: []schemaEntry{
			{AccountID: clearing.ID, Direction: "debit", AmountMinor: 10000, Currency: "USD"},
			{AccountID: revenue.ID, Direction: "credit", AmountMinor: 10000, Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("balanced transaction did not commit: %v", err)
	}

	var entries int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&entries); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if entries != 2 {
		t.Fatalf("ledger_entries count = %d, want 2", entries)
	}
}

// TestUnbalancedTransactionFailsAtCommit proves the balance check is deferred:
// both rows insert successfully, and COMMIT is what fails (FIN-10).
func TestUnbalancedTransactionFailsAtCommit(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	clearing := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := insertTxn(ctx, tx, schemaTxn{
		ValueDate: "2026-01-15",
		Kind:      "charge",
		Entries: []schemaEntry{
			{AccountID: clearing.ID, Direction: "debit", AmountMinor: 1000, Currency: "USD"},
			{AccountID: revenue.ID, Direction: "credit", AmountMinor: 999, Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("inserting an unbalanced transaction should succeed before commit: %v", err)
	}

	err = tx.Commit(ctx)
	requirePgError(t, err, sqlStateCheckViolation, "unbalanced")

	// The failed commit must leave no transaction row behind.
	var exists bool
	if err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_txns WHERE id = $1::uuid)`, id).Scan(&exists); err != nil {
		t.Fatalf("check rollback: %v", err)
	}
	if exists {
		t.Fatal("unbalanced transaction persisted after a failed commit")
	}
}

// TestMultiCurrencyUnbalancedFailsAtCommit balances USD but not EUR in the same
// transaction; per-currency balancing must reject it (FIN-10).
func TestMultiCurrencyUnbalancedFailsAtCommit(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	usdClearing := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
	usdRevenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	eurClearing := createAccount(t, d, "clearing_eur", "EUR", "asset", "active")
	eurRevenue := createAccount(t, d, "revenue_eur", "EUR", "revenue", "active")

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := insertTxn(ctx, tx, schemaTxn{
		ValueDate: "2026-01-15",
		Kind:      "charge",
		Entries: []schemaEntry{
			{AccountID: usdClearing.ID, Direction: "debit", AmountMinor: 500, Currency: "USD"},
			{AccountID: usdRevenue.ID, Direction: "credit", AmountMinor: 500, Currency: "USD"},
			{AccountID: eurClearing.ID, Direction: "debit", AmountMinor: 700, Currency: "EUR"},
			{AccountID: eurRevenue.ID, Direction: "credit", AmountMinor: 699, Currency: "EUR"},
		},
	}); err != nil {
		t.Fatalf("inserting rows should succeed before commit: %v", err)
	}

	err = tx.Commit(ctx)
	pgErr := requirePgError(t, err, sqlStateCheckViolation, "unbalanced")
	if !strings.Contains(pgErr.Message, "EUR") {
		t.Fatalf("unbalanced currency diagnostics %q do not name EUR", pgErr.Message)
	}
}

func TestMultiCurrencyBalancedCommits(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	usdClearing := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
	usdRevenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	eurClearing := createAccount(t, d, "clearing_eur", "EUR", "asset", "active")
	eurRevenue := createAccount(t, d, "revenue_eur", "EUR", "revenue", "active")

	if err := postTxn(ctx, d, schemaTxn{
		ValueDate: "2026-01-15",
		Kind:      "charge",
		Entries: []schemaEntry{
			{AccountID: usdClearing.ID, Direction: "debit", AmountMinor: 500, Currency: "USD"},
			{AccountID: usdRevenue.ID, Direction: "credit", AmountMinor: 500, Currency: "USD"},
			{AccountID: eurClearing.ID, Direction: "debit", AmountMinor: 700, Currency: "EUR"},
			{AccountID: eurRevenue.ID, Direction: "credit", AmountMinor: 700, Currency: "EUR"},
		},
	}); err != nil {
		t.Fatalf("multi-currency balanced transaction did not commit: %v", err)
	}
}

func TestEmptyTransactionRejected(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := insertTxn(ctx, tx, schemaTxn{ValueDate: "2026-01-15", Kind: "charge"})
	if err != nil {
		t.Fatalf("inserting an empty transaction row should succeed before commit: %v", err)
	}

	err = tx.Commit(ctx)
	requirePgError(t, err, sqlStateCheckViolation, "no entries")

	var exists bool
	if err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_txns WHERE id = $1::uuid)`, id).Scan(&exists); err != nil {
		t.Fatalf("check rollback: %v", err)
	}
	if exists {
		t.Fatal("empty transaction persisted after a failed commit")
	}
}

// TestLedgerRowsAreImmutable runs as the maintenance/superuser role on purpose:
// grants alone must not be the reason UPDATE/DELETE/TRUNCATE fail (FIN-11).
func TestLedgerRowsAreImmutable(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	var role, superuser string
	if err := d.Pool.QueryRow(ctx, `SELECT current_user, current_setting('is_superuser')`).Scan(&role, &superuser); err != nil {
		t.Fatalf("read current role: %v", err)
	}
	t.Logf("mutations attempted as role %q (superuser=%s)", role, superuser)

	clearing := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	if err := postTxn(ctx, d, schemaTxn{
		ValueDate: "2026-01-15",
		Kind:      "charge",
		Entries: []schemaEntry{
			{AccountID: clearing.ID, Direction: "debit", AmountMinor: 1234, Currency: "USD"},
			{AccountID: revenue.ID, Direction: "credit", AmountMinor: 1234, Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("prepare transaction: %v", err)
	}

	var txnID string
	if err := d.Pool.QueryRow(ctx, `SELECT id::text FROM ledger_txns LIMIT 1`).Scan(&txnID); err != nil {
		t.Fatalf("read transaction id: %v", err)
	}

	mutations := []struct {
		name  string
		sql   string
		args  []any
		code  string
		match string
	}{
		{
			name:  "update ledger entry",
			sql:   `UPDATE ledger_entries SET amount_minor = amount_minor + 1 WHERE txn_id = $1::uuid`,
			args:  []any{txnID},
			code:  sqlStateRestrictViolation,
			match: "immutable",
		},
		{
			name:  "delete ledger entry",
			sql:   `DELETE FROM ledger_entries WHERE txn_id = $1::uuid`,
			args:  []any{txnID},
			code:  sqlStateRestrictViolation,
			match: "immutable",
		},
		{
			name:  "update ledger txn",
			sql:   `UPDATE ledger_txns SET description = 'tampered' WHERE id = $1::uuid`,
			args:  []any{txnID},
			code:  sqlStateRestrictViolation,
			match: "immutable",
		},
		{
			name:  "delete ledger txn",
			sql:   `DELETE FROM ledger_txns WHERE id = $1::uuid`,
			args:  []any{txnID},
			code:  sqlStateRestrictViolation,
			match: "immutable",
		},
		{
			name:  "truncate ledger entries",
			sql:   `TRUNCATE ledger_entries`,
			code:  sqlStateRestrictViolation,
			match: "immutable",
		},
		{
			// The entries -> txns RESTRICT foreign key may reject this before
			// the trigger runs; either way truncation is refused.
			name: "truncate ledger txns",
			sql:  `TRUNCATE ledger_txns`,
		},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			_, err := d.Pool.Exec(ctx, mutation.sql, mutation.args...)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded", mutation.name)
			}
			if mutation.code != "" {
				requirePgError(t, err, mutation.code, mutation.match)
			}
		})
	}

	// All six immutability triggers must exist even if a foreign-key check
	// masks one of the behavioral cases above.
	var triggers int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM pg_trigger tg
		   JOIN pg_class c ON c.oid = tg.tgrelid
		  WHERE c.relname IN ('ledger_txns', 'ledger_entries')
		    AND tg.tgname IN (
		        'ledger_txns_reject_update', 'ledger_txns_reject_delete', 'ledger_txns_reject_truncate',
		        'ledger_entries_reject_update', 'ledger_entries_reject_delete', 'ledger_entries_reject_truncate'
		    )`).Scan(&triggers); err != nil {
		t.Fatalf("count immutability triggers: %v", err)
	}
	if triggers != 6 {
		t.Fatalf("immutability triggers = %d, want 6", triggers)
	}
}

// TestEntryValidation covers FIN-13 and FIN-56 at the row level: an entry must
// use its account's currency, the account must be active, and the transaction's
// period must be open.
func TestEntryValidation(t *testing.T) {
	t.Run("wrong currency", func(t *testing.T) {
		ctx := context.Background()
		d := newSchemaDB(t)
		usd := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
		revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")

		err := postTxn(ctx, d, schemaTxn{
			ValueDate: "2026-01-15",
			Kind:      "charge",
			Entries: []schemaEntry{
				{AccountID: usd.ID, Direction: "debit", AmountMinor: 100, Currency: "USD"},
				{AccountID: revenue.ID, Direction: "credit", AmountMinor: 100, Currency: "EUR"},
			},
		})
		requirePgError(t, err, sqlStateCheckViolation, "currency")
	})

	t.Run("inactive account", func(t *testing.T) {
		ctx := context.Background()
		d := newSchemaDB(t)
		inactive := createAccount(t, d, "clearing_usd", "USD", "asset", "inactive")
		revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")

		err := postTxn(ctx, d, schemaTxn{
			ValueDate: "2026-01-15",
			Kind:      "charge",
			Entries: []schemaEntry{
				{AccountID: inactive.ID, Direction: "debit", AmountMinor: 100, Currency: "USD"},
				{AccountID: revenue.ID, Direction: "credit", AmountMinor: 100, Currency: "USD"},
			},
		})
		requirePgError(t, err, sqlStateCheckViolation, "cannot be posted")
	})

	t.Run("closed account", func(t *testing.T) {
		ctx := context.Background()
		d := newSchemaDB(t)
		closed := createAccount(t, d, "clearing_usd", "USD", "asset", "closed")
		revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")

		err := postTxn(ctx, d, schemaTxn{
			ValueDate: "2026-01-15",
			Kind:      "charge",
			Entries: []schemaEntry{
				{AccountID: closed.ID, Direction: "debit", AmountMinor: 100, Currency: "USD"},
				{AccountID: revenue.ID, Direction: "credit", AmountMinor: 100, Currency: "USD"},
			},
		})
		requirePgError(t, err, sqlStateCheckViolation, "cannot be posted")
	})

	t.Run("closed period", func(t *testing.T) {
		ctx := context.Background()
		d := newSchemaDB(t)
		clearing := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
		revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")
		closedPeriod := createPeriod(t, d, "2019-01-01", "2019-12-31", "closed")

		err := postTxn(ctx, d, schemaTxn{
			ValueDate: "2019-06-15",
			Kind:      "charge",
			PeriodID:  closedPeriod,
			Entries: []schemaEntry{
				{AccountID: clearing.ID, Direction: "debit", AmountMinor: 100, Currency: "USD"},
				{AccountID: revenue.ID, Direction: "credit", AmountMinor: 100, Currency: "USD"},
			},
		})
		requirePgError(t, err, sqlStateCheckViolation, "closed")
	})
}

func TestSeqStrictlyIncreasing(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	clearing := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	for i := 0; i < 3; i++ {
		if err := postTxn(ctx, d, schemaTxn{
			ValueDate: "2026-01-15",
			Kind:      "charge",
			Entries: []schemaEntry{
				{AccountID: clearing.ID, Direction: "debit", AmountMinor: int64(100 + i), Currency: "USD"},
				{AccountID: revenue.ID, Direction: "credit", AmountMinor: int64(100 + i), Currency: "USD"},
			},
		}); err != nil {
			t.Fatalf("post transaction %d: %v", i, err)
		}
	}

	rows, err := d.Pool.Query(ctx, `SELECT seq FROM ledger_txns ORDER BY seq`)
	if err != nil {
		t.Fatalf("query seq: %v", err)
	}
	defer rows.Close()

	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan seq: %v", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate seq: %v", err)
	}
	if len(seqs) != 3 {
		t.Fatalf("seq count = %d, want 3", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seq values are not strictly increasing: %v", seqs)
		}
	}
}

func TestIdempotencyKeyUnique(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	clearing := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	spec := schemaTxn{
		ValueDate:      "2026-01-15",
		Kind:           "charge",
		IdempotencyKey: "unique-key-1",
		Entries: []schemaEntry{
			{AccountID: clearing.ID, Direction: "debit", AmountMinor: 100, Currency: "USD"},
			{AccountID: revenue.ID, Direction: "credit", AmountMinor: 100, Currency: "USD"},
		},
	}
	if err := postTxn(ctx, d, spec); err != nil {
		t.Fatalf("first post: %v", err)
	}

	spec.Entries[0].AmountMinor = 200
	spec.Entries[1].AmountMinor = 200
	err := postTxn(ctx, d, spec)
	requirePgError(t, err, sqlStateUniqueViolation, "")

	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger_txns`).Scan(&count); err != nil {
		t.Fatalf("count txns: %v", err)
	}
	if count != 1 {
		t.Fatalf("ledger_txns count = %d, want 1 after duplicate idempotency key", count)
	}
}

func TestLedgerIndexesAndForeignKeys(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	indexes := map[string]string{
		"ledger_entries_txn_id_idx":     "txn_id",
		"ledger_entries_account_id_idx": "account_id",
		"ledger_txns_value_date_idx":    "value_date",
		"ledger_txns_period_id_idx":     "period_id",
	}
	for name, column := range indexes {
		var definition string
		if err := d.Pool.QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name).Scan(&definition); err != nil {
			t.Errorf("index %s is missing: %v", name, err)
			continue
		}
		if !strings.Contains(definition, "("+column+")") {
			t.Errorf("index %s definition %q does not index column %s", name, definition, column)
		}
	}

	foreignKeys := map[string]string{
		"ledger_entries_txn_id_fkey":         "r",
		"ledger_entries_account_id_fkey":     "r",
		"ledger_txns_period_id_fkey":         "r",
		"ledger_txns_adjusts_period_id_fkey": "r",
		"ledger_txns_reversal_of_fkey":       "r",
	}
	for name, wantAction := range foreignKeys {
		var action string
		if err := d.Pool.QueryRow(ctx,
			`SELECT confdeltype FROM pg_constraint WHERE conname = $1`, name).Scan(&action); err != nil {
			t.Errorf("foreign key %s is missing: %v", name, err)
			continue
		}
		if action != wantAction {
			t.Errorf("foreign key %s ON DELETE action = %q, want %q (RESTRICT)", name, action, wantAction)
		}
	}
}

func TestColumnChecks(t *testing.T) {
	ctx := context.Background()
	d := newSchemaDB(t)

	usd := createAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := createAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	if err := postTxn(ctx, d, schemaTxn{
		ValueDate: "2026-01-15",
		Kind:      "charge",
		Entries: []schemaEntry{
			{AccountID: usd.ID, Direction: "debit", AmountMinor: 50, Currency: "USD"},
			{AccountID: revenue.ID, Direction: "credit", AmountMinor: 50, Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("prepare balanced transaction: %v", err)
	}
	var txnID string
	if err := d.Pool.QueryRow(ctx, `SELECT id::text FROM ledger_txns LIMIT 1`).Scan(&txnID); err != nil {
		t.Fatalf("read txn id: %v", err)
	}

	periodID := openPeriodID(t, d)

	invalid := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "invalid account status",
			sql:  `INSERT INTO accounts (code, name, type, currency, status) VALUES ('bad_status', 'bad', 'asset', 'USD', 'frozen')`,
		},
		{
			name: "invalid account type",
			sql:  `INSERT INTO accounts (code, name, type, currency, status) VALUES ('bad_type', 'bad', 'cash', 'USD', 'active')`,
		},
		{
			name: "invalid period status",
			sql:  `INSERT INTO periods (start_date, end_date, status) VALUES ('2026-02-01', '2026-02-28', 'locked')`,
		},
		{
			name: "invalid txn kind",
			sql:  `INSERT INTO ledger_txns (value_date, kind, idempotency_key, request_hash, period_id) VALUES ('2026-01-15', 'bogus', 'bad-kind', 'hash', $1::uuid)`,
			args: []any{periodID},
		},
		{
			name: "invalid entry direction",
			sql:  `INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency) VALUES ($1::uuid, $2::uuid, 'sideways', 10, 'USD')`,
			args: []any{txnID, usd.ID},
		},
		{
			name: "non-positive entry amount",
			sql:  `INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency) VALUES ($1::uuid, $2::uuid, 'debit', 0, 'USD')`,
			args: []any{txnID, usd.ID},
		},
	}

	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.Pool.Exec(ctx, tc.sql, tc.args...)
			requirePgError(t, err, sqlStateCheckViolation, "")
		})
	}
}
