//go:build integration

// covers: FIN-10, FIN-12, FIN-13, FIN-30, FIN-56
package ledger_test

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestPostWritesTxnEntriesPeriodAndAudit is the happy path: a balanced request
// posted as recon_app commits one transaction, its entries, the seeded open
// period, and exactly one audit row in the same transaction (FIN-10, FIN-30).
func TestPostWritesTxnEntriesPeriodAndAudit(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})

	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	wantPeriod := seededOpenPeriodID(t, d)

	res := postCommitted(t, d, app, svc, chargeRequest(t, clearing, revenue, 10000, uniqueKey(t)))

	if res.ID == "" || res.Seq <= 0 {
		t.Fatalf("result ID=%q seq=%d, want a persisted transaction", res.ID, res.Seq)
	}
	if res.Replayed {
		t.Error("first post reported Replayed = true")
	}
	if res.Kind != ledger.KindCharge {
		t.Errorf("kind = %q, want %q", res.Kind, ledger.KindCharge)
	}
	if got := res.ValueDate.Format("2006-01-02"); got != "2026-06-01" {
		t.Errorf("value date = %s, want 2026-06-01", got)
	}
	if res.PeriodID != wantPeriod {
		t.Errorf("period = %q, want seeded open period %q", res.PeriodID, wantPeriod)
	}
	if res.AdjustsPeriodID != nil {
		t.Errorf("adjusts period = %q, want NULL for an open-period post", *res.AdjustsPeriodID)
	}

	var (
		kind       string
		periodID   string
		adjusts    *string
		createdAt  time.Time
		bookedAt   time.Time
		valueDate  time.Time
		entryCount int64
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT kind, period_id::text, adjusts_period_id::text, created_at, booked_at, value_date
		   FROM ledger_txns WHERE id = $1::uuid`, res.ID).
		Scan(&kind, &periodID, &adjusts, &createdAt, &bookedAt, &valueDate); err != nil {
		t.Fatalf("read stored transaction: %v", err)
	}
	if kind != "charge" || periodID != wantPeriod || adjusts != nil {
		t.Errorf("stored txn = {kind:%s period:%s adjusts:%v}, want charge/%s/nil", kind, periodID, adjusts, wantPeriod)
	}
	if got := valueDate.Format("2006-01-02"); got != "2026-06-01" {
		t.Errorf("stored value_date = %s, want 2026-06-01", got)
	}
	if !createdAt.Equal(mustTime(t, testNow)) || !bookedAt.Equal(mustTime(t, testNow)) {
		t.Errorf("stored timestamps = %s/%s, want the injected clock %s", createdAt, bookedAt, testNow)
	}
	entryCount = countRows(t, d, `SELECT count(*) FROM ledger_entries WHERE txn_id = $1::uuid`, res.ID)
	if entryCount != 2 {
		t.Fatalf("entry count = %d, want 2", entryCount)
	}
	if got := testutil.AssertAudited(t, d.Pool, res.ID); got != 1 {
		t.Errorf("audit rows for txn = %d, want 1", got)
	}

	var action, entityType string
	if err := d.Pool.QueryRow(ctx,
		`SELECT action, entity_type FROM audit_log WHERE entity_id = $1`, res.ID).
		Scan(&action, &entityType); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if action != ledger.ActionPost || entityType != "ledger_txn" {
		t.Errorf("audit row = {%s %s}, want {%s ledger_txn}", action, entityType, ledger.ActionPost)
	}
}

// TestPostUsesInjectedClockAndUUIDv4 proves the defaults: no caller ID means a
// UUIDv4, and timestamps come from the injected clock, never time.Now (D-7).
func TestPostUsesInjectedClockAndUUIDv4(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})

	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	res := postCommitted(t, d, app, svc, chargeRequest(t, clearing, revenue, 2500, uniqueKey(t)))
	if !uuidV4Pattern.MatchString(res.ID) {
		t.Fatalf("generated ID %q is not a UUIDv4", res.ID)
	}
	if !res.CreatedAt.Equal(mustTime(t, testNow)) || !res.BookedAt.Equal(mustTime(t, testNow)) {
		t.Errorf("timestamps = %s/%s, want %s", res.CreatedAt, res.BookedAt, testNow)
	}

	var created, booked time.Time
	var auditTS time.Time
	if err := d.Pool.QueryRow(ctx,
		`SELECT created_at, booked_at FROM ledger_txns WHERE id = $1::uuid`, res.ID).
		Scan(&created, &booked); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}
	if !created.Equal(mustTime(t, testNow)) || !booked.Equal(mustTime(t, testNow)) {
		t.Errorf("stored timestamps = %s/%s, want %s", created, booked, testNow)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT ts FROM audit_log WHERE entity_id = $1`, res.ID).Scan(&auditTS); err != nil {
		t.Fatalf("read audit timestamp: %v", err)
	}
	if !auditTS.Equal(mustTime(t, testNow)) {
		t.Errorf("audit ts = %s, want injected clock %s", auditTS, testNow)
	}
}

// TestPostAcceptsCallerSuppliedIDAndTimestamps covers the M1-09 requirement that
// the generator can supply stable IDs and explicit timestamps (D-7).
func TestPostAcceptsCallerSuppliedIDAndTimestamps(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})

	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	const fixedID = "22222222-2222-4222-8222-222222222222"
	created := mustTime(t, "2026-05-01T08:00:00Z")
	booked := mustTime(t, "2026-05-02T09:30:00Z")
	req := chargeRequest(t, clearing, revenue, 750, uniqueKey(t))
	req.ID = fixedID
	req.CreatedAt = created
	req.BookedAt = booked
	req.ValueDate = mustDate(t, "2026-05-02")

	res := postCommitted(t, d, app, svc, req)
	if res.ID != fixedID {
		t.Fatalf("result ID = %q, want caller-supplied %q", res.ID, fixedID)
	}

	var (
		storedCreated time.Time
		storedBooked  time.Time
		auditTS       time.Time
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT created_at, booked_at FROM ledger_txns WHERE id = $1::uuid`, fixedID).
		Scan(&storedCreated, &storedBooked); err != nil {
		t.Fatalf("read caller-supplied timestamps: %v", err)
	}
	if !storedCreated.Equal(created) || !storedBooked.Equal(booked) {
		t.Errorf("stored timestamps = %s/%s, want %s/%s", storedCreated, storedBooked, created, booked)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT ts FROM audit_log WHERE entity_id = $1`, fixedID).Scan(&auditTS); err != nil {
		t.Fatalf("read audit ts: %v", err)
	}
	if !auditTS.Equal(booked) {
		t.Errorf("audit ts = %s, want booked_at %s", auditTS, booked)
	}
}

// TestPostRejectsUnbalanced proves the in-code balance validation (FIN-10). The
// DB trigger is the backstop; here no row may even be inserted.
func TestPostRejectsUnbalanced(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})

	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	eurClearing := newAccount(t, d, "clearing_eur", "EUR", "asset", "active")
	eurRevenue := newAccount(t, d, "revenue_eur", "EUR", "revenue", "active")

	req := chargeRequest(t, clearing, revenue, 1000, uniqueKey(t))
	req.Lines[1].AmountMinor = 999
	if err := postError(t, d, app, svc, req); !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("unbalanced post error = %v, want ErrUnbalanced", err)
	}

	// USD balances but EUR does not: per-currency validation must reject it.
	req = chargeRequest(t, clearing, revenue, 500, uniqueKey(t))
	req.Lines = append(req.Lines,
		ledger.Line{AccountID: eurClearing, Direction: ledger.Debit, AmountMinor: 700, Currency: "EUR"},
		ledger.Line{AccountID: eurRevenue, Direction: ledger.Credit, AmountMinor: 699, Currency: "EUR"},
	)
	err := postError(t, d, app, svc, req)
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("multi-currency unbalanced post error = %v, want ErrUnbalanced", err)
	}

	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 0 {
		t.Fatalf("ledger_txns count = %d after rejected posts, want 0", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Fatalf("audit_log count = %d after rejected posts, want 0", got)
	}

	// A multi-currency transaction that balances in every currency is allowed
	// (FIN-10 balances per currency, not across currencies).
	req = chargeRequest(t, clearing, revenue, 500, uniqueKey(t))
	req.Lines = append(req.Lines,
		ledger.Line{AccountID: eurClearing, Direction: ledger.Debit, AmountMinor: 700, Currency: "EUR"},
		ledger.Line{AccountID: eurRevenue, Direction: ledger.Credit, AmountMinor: 700, Currency: "EUR"},
	)
	res := postCommitted(t, d, app, svc, req)
	if got := countRows(t, d, `SELECT count(*) FROM ledger_entries WHERE txn_id = $1::uuid`, res.ID); got != 4 {
		t.Fatalf("multi-currency entry count = %d, want 4", got)
	}
}

func TestPostRejectsEmptyAndInvalidLines(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	empty := chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
	empty.Lines = nil
	if err := postError(t, d, app, svc, empty); !errors.Is(err, ledger.ErrEmptyTransaction) {
		t.Fatalf("empty request error = %v, want ErrEmptyTransaction", err)
	}

	zeroAmount := chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
	zeroAmount.Lines[0].AmountMinor = 0
	if err := postError(t, d, app, svc, zeroAmount); !errors.Is(err, ledger.ErrInvalidLine) {
		t.Fatalf("zero-amount line error = %v, want ErrInvalidLine", err)
	}

	badDirection := chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
	badDirection.Lines[0].Direction = "sideways"
	if err := postError(t, d, app, svc, badDirection); !errors.Is(err, ledger.ErrInvalidLine) {
		t.Fatalf("bad-direction line error = %v, want ErrInvalidLine", err)
	}

	badCurrency := chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
	badCurrency.Lines[0].Currency = "XXX"
	if err := postError(t, d, app, svc, badCurrency); !errors.Is(err, ledger.ErrInvalidLine) {
		t.Fatalf("unknown-currency line error = %v, want ErrInvalidLine", err)
	}

	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 0 {
		t.Fatalf("ledger_txns count = %d after invalid requests, want 0", got)
	}
}

// TestPostRejectsAccountProblems covers account existence, status, and currency
// checks in code (FIN-13).
func TestPostRejectsAccountProblems(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})

	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	inactive := newAccount(t, d, "inactive_usd", "USD", "asset", "inactive")
	closed := newAccount(t, d, "closed_usd", "USD", "asset", "closed")
	eurRevenue := newAccount(t, d, "revenue_eur", "EUR", "revenue", "active")

	req := chargeRequest(t, clearing, inactive, 100, uniqueKey(t))
	if err := postError(t, d, app, svc, req); !errors.Is(err, ledger.ErrAccountNotPostable) {
		t.Fatalf("inactive-account error = %v, want ErrAccountNotPostable", err)
	}

	req = chargeRequest(t, clearing, closed, 100, uniqueKey(t))
	if err := postError(t, d, app, svc, req); !errors.Is(err, ledger.ErrAccountNotPostable) {
		t.Fatalf("closed-account error = %v, want ErrAccountNotPostable", err)
	}

	// The EUR revenue account cannot take a USD entry.
	req = chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
	req.Lines[1].AccountID = eurRevenue
	if err := postError(t, d, app, svc, req); !errors.Is(err, ledger.ErrEntryCurrencyMismatch) {
		t.Fatalf("wrong-currency error = %v, want ErrEntryCurrencyMismatch", err)
	}

	req = chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
	req.Lines[1].AccountID = "33333333-3333-4333-8333-333333333333"
	if err := postError(t, d, app, svc, req); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatalf("unknown-account error = %v, want ErrAccountNotFound", err)
	}

	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 0 {
		t.Fatalf("ledger_txns count = %d after rejected posts, want 0", got)
	}
}

// TestPostAdjustmentAllowlist runs through recon_executor because only that role
// may insert kind='adjustment' (M1-04 trigger). The config allowlist decides
// which accounts an adjustment may target (FIN-13).
func TestPostAdjustmentAllowlist(t *testing.T) {
	d := newLedgerDB(t)
	executor := openRolePool(t, d, db.RoleExecutor)
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	suspense := newAccount(t, d, "suspense_usd", "USD", "expense", "active")

	// Only the suspense account is allowlisted: the clearing leg is rejected.
	restricted := testService(t, config.LedgerConfig{AdjustmentAccounts: []string{"suspense_usd"}})
	req := chargeRequest(t, clearing, suspense, 400, uniqueKey(t))
	req.Kind = ledger.KindAdjustment
	if err := postError(t, d, executor, restricted, req); !errors.Is(err, ledger.ErrAdjustmentNotAllowed) {
		t.Fatalf("non-allowlisted adjustment error = %v, want ErrAdjustmentNotAllowed", err)
	}

	allowed := testService(t, config.LedgerConfig{AdjustmentAccounts: []string{"clearing_usd", "suspense_usd"}})
	req = chargeRequest(t, clearing, suspense, 400, uniqueKey(t))
	req.Kind = ledger.KindAdjustment
	res := postCommitted(t, d, executor, allowed, req)
	if res.Kind != ledger.KindAdjustment {
		t.Fatalf("kind = %q, want adjustment", res.Kind)
	}
	if got := testutil.AssertAudited(t, d.Pool, res.ID); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}

// TestPostCustomerFundsRequiresT2 proves the customer-funds error the policy
// engine maps to tier T2 (FIN-13).
func TestPostCustomerFundsRequiresT2(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{CustomerFundsAccounts: []string{"customer_funds_usd"}})

	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	customerFunds := newAccount(t, d, "customer_funds_usd", "USD", "liability", "active")

	req := chargeRequest(t, clearing, customerFunds, 900, uniqueKey(t))
	if err := postError(t, d, app, svc, req); !errors.Is(err, ledger.ErrCustomerFundsT2) {
		t.Fatalf("customer-funds error = %v, want ErrCustomerFundsT2", err)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 0 {
		t.Fatalf("ledger_txns count = %d, want 0", got)
	}
}

// TestPostRollbackRemovesAudit proves the audit row joins the caller's
// transaction rather than committing independently (FIN-30, FIN-25).
func TestPostRollbackRemovesAudit(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := svc.Post(ctx, tx, chargeRequest(t, clearing, revenue, 1000, uniqueKey(t)))
	if err != nil {
		t.Fatalf("post inside tx: %v", err)
	}
	// Inside the transaction the audit row is already visible.
	if got := testutil.AssertAudited(t, tx, res.ID); got != 1 {
		t.Fatalf("audit rows inside tx = %d, want 1", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 0 {
		t.Fatalf("ledger_txns count = %d after rollback, want 0", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_entries`); got != 0 {
		t.Fatalf("ledger_entries count = %d after rollback, want 0", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Fatalf("audit_log count = %d after rollback, want 0", got)
	}
}

func TestPostRequiresTransaction(t *testing.T) {
	svc := testService(t, config.LedgerConfig{})
	req := ledger.PostRequest{
		IdempotencyKey: "no-tx",
		Kind:           ledger.KindCharge,
		ValueDate:      mustDate(t, "2026-06-01"),
		Actor:          mustActor(t, "system:executor"),
		Lines:          []ledger.Line{{AccountID: "44444444-4444-4444-8444-444444444444", Direction: ledger.Debit, AmountMinor: 1, Currency: "USD"}},
	}
	if _, err := svc.Post(context.Background(), nil, req); !errors.Is(err, ledger.ErrNoTransaction) {
		t.Fatalf("Post(nil tx) error = %v, want ErrNoTransaction", err)
	}
}

func TestPostRequiresActorAndKey(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	emptyKey := chargeRequest(t, clearing, revenue, 100, "")
	if err := postError(t, d, app, svc, emptyKey); !errors.Is(err, ledger.ErrInvalidRequest) {
		t.Fatalf("empty idempotency key error = %v, want ErrInvalidRequest", err)
	}

	noActor := chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
	noActor.Actor = auditZeroActor()
	if err := postError(t, d, app, svc, noActor); !errors.Is(err, ledger.ErrInvalidRequest) {
		t.Fatalf("zero actor error = %v, want ErrInvalidRequest", err)
	}
}
