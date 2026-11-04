//go:build integration

// covers: FIN-54

package tieout_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const testNow = "2026-06-15T12:00:00Z"

var fixtureSeq atomic.Int64

// fixture is one reconciliation scope with its clearing account, counter
// account, and one statement batch. Every test gets an isolated database, so
// the codes and hashes only need to be unique inside one test.
type fixture struct {
	db         *testdb.DB
	scopeID    string
	accountID  string
	counterID  string
	batchID    string
	source     string
	currency   string
	accountCcy string
}

func newFixture(t *testing.T, source string) *fixture {
	t.Helper()
	return newFixtureWithCurrencies(t, source, "USD", "USD")
}

// newFixtureWithCurrencies creates a scope whose declared currency and account
// currency can differ, which is the multi-currency error case.
func newFixtureWithCurrencies(t *testing.T, source, scopeCurrency, accountCurrency string) *fixture {
	t.Helper()
	d := testdb.New(t)
	accountID := insertAccount(t, d, source+"_clearing", "asset", accountCurrency)
	counterID := insertAccount(t, d, source+"_counter", "revenue", accountCurrency)
	batchID := insertBatch(t, d, source, scopeCurrency)
	scopeID := insertScope(t, d, accountID, source, scopeCurrency)
	return &fixture{
		db:         d,
		scopeID:    scopeID,
		accountID:  accountID,
		counterID:  counterID,
		batchID:    batchID,
		source:     source,
		currency:   scopeCurrency,
		accountCcy: accountCurrency,
	}
}

func insertAccount(t *testing.T, d *testdb.DB, code, typ, currency string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, $3, 'active') RETURNING id::text`,
		code, typ, currency).Scan(&id); err != nil {
		t.Fatalf("insert account %q: %v", code, err)
	}
	return id
}

func insertBatch(t *testing.T, d *testdb.DB, source, currency string) string {
	t.Helper()
	n := fixtureSeq.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", source, n)))
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ($1, 0, $2, $3, 0, 0) RETURNING id::text`,
		source, currency, hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	return id
}

func insertScope(t *testing.T, d *testdb.DB, accountID, source, currency string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, $3) RETURNING id::text`,
		accountID, source, currency).Scan(&id); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	return id
}

func (f *fixture) addStatement(t *testing.T, asOf string, openingMinor, closingMinor int64) {
	t.Helper()
	f.addStatementCurrency(t, asOf, openingMinor, closingMinor, f.currency)
}

func (f *fixture) addStatementCurrency(t *testing.T, asOf string, openingMinor, closingMinor int64, currency string) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`INSERT INTO statement_balances
		     (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
		 VALUES ($1::uuid, $2::date, $3, $4, $5, $6::uuid)`,
		f.scopeID, asOf, openingMinor, closingMinor, currency, f.batchID); err != nil {
		t.Fatalf("insert statement %s: %v", asOf, err)
	}
}

// addExternal inserts one external record with the signed amount convention
// tie-out uses: positive is a debit to the clearing account.
func (f *fixture) addExternal(t *testing.T, ref string, amountMinor int64, valueDate string) string {
	t.Helper()
	n := fixtureSeq.Add(1)
	sourceRecordID := fmt.Sprintf("%s-%d", ref, n)
	raw, err := json.Marshal(map[string]string{"reference": ref})
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
		 VALUES ($1, $2, $3, $4, $5, 0, $6, $7, $8::uuid, $9::jsonb, $10)
		 RETURNING id::text`,
		f.source, sourceRecordID, kind, amountMinor, f.currency,
		date.Add(12*time.Hour), date, f.batchID, string(raw), hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert external record %q: %v", ref, err)
	}
	return id
}

// addLedger inserts one balanced transaction whose clearing-account entry
// carries ref and the signed amount (debit positive, D-14).
func (f *fixture) addLedger(t *testing.T, ref string, amountMinor int64, valueDate string) string {
	t.Helper()
	return f.addLedgerCurrency(t, ref, amountMinor, valueDate, f.accountCcy)
}

func (f *fixture) addLedgerCurrency(t *testing.T, ref string, amountMinor int64, valueDate, currency string) string {
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
		date, kind, fmt.Sprintf("tieout-%s-%d", ref, n), ref).Scan(&txnID); err != nil {
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
			 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)`,
			txnID, entry.accountID, entry.direction, abs, currency, entry.ref); err != nil {
			t.Fatalf("insert ledger entry for %q: %v", ref, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ledger fixture %q: %v", ref, err)
	}
	return txnID
}

// runMatcher runs the real matcher over the fixture's scope and date range.
func (f *fixture) runMatcher(t *testing.T, from, to string, toleranceMinor int64) match.RunResult {
	t.Helper()
	actor, err := audit.SystemActor("matcher")
	if err != nil {
		t.Fatalf("system actor: %v", err)
	}
	engine, err := match.New(match.Options{
		DB:     f.db.Pool,
		Clock:  clock.NewFake(mustTime(t, testNow)),
		Actor:  actor,
		Config: config.MatchConfig{FeeToleranceMinor: map[string]int64{f.currency: toleranceMinor}},
	})
	if err != nil {
		t.Fatalf("new matcher: %v", err)
	}
	result, err := engine.Run(context.Background(), match.RunRequest{
		ScopeID: f.scopeID,
		Window:  match.Window{From: mustDate(t, from), To: mustDate(t, to)},
	})
	if err != nil {
		t.Fatalf("run matcher: %v", err)
	}
	return result
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
