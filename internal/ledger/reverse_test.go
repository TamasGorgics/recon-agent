//go:build integration

// covers: FIN-10, FIN-12, FIN-13, FIN-30, FIN-56
package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

func reverseCommitted(t *testing.T, app db.Beginner, svc *ledger.Service, req ledger.ReverseRequest) ledger.Result {
	t.Helper()
	var result ledger.Result
	if err := db.WithTx(context.Background(), app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = svc.Reverse(ctx, tx, req)
		return err
	}); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	return result
}

func reverseError(t *testing.T, app db.Beginner, svc *ledger.Service, req ledger.ReverseRequest) error {
	t.Helper()
	return db.WithTx(context.Background(), app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.Reverse(ctx, tx, req)
		return err
	})
}

// TestReverseCreatesOppositeEntries posts a charge and reverses it: the new
// transaction has kind reversal, reversal_of set to the original, opposite
// directions on every entry, its own audit row, and nets the account to zero
// (FIN-11, FIN-30).
func TestReverseCreatesOppositeEntries(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	original := postCommitted(t, d, app, svc, chargeRequest(t, clearing, revenue, 5000, uniqueKey(t)))
	reversal := reverseCommitted(t, app, svc, ledger.ReverseRequest{
		TxnID:          original.ID,
		Reason:         "customer disputed the charge",
		IdempotencyKey: uniqueKey(t),
		Actor:          mustActor(t, "system:executor"),
	})

	if reversal.Kind != ledger.KindReversal {
		t.Errorf("reversal kind = %q, want reversal", reversal.Kind)
	}
	if reversal.ReversalOf == nil || *reversal.ReversalOf != original.ID {
		t.Fatalf("reversal_of = %v, want %s", reversal.ReversalOf, original.ID)
	}
	if got := reversal.ValueDate.Format("2006-01-02"); got != "2026-06-01" {
		t.Errorf("reversal value date = %s, want the original value date", got)
	}

	var debit, credit, amount int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT
		    count(*) FILTER (WHERE direction = 'debit'),
		    count(*) FILTER (WHERE direction = 'credit'),
		    COALESCE(sum(amount_minor), 0)
		 FROM ledger_entries WHERE txn_id = $1::uuid`, reversal.ID).
		Scan(&debit, &credit, &amount); err != nil {
		t.Fatalf("read reversal entries: %v", err)
	}
	if debit != 1 || credit != 1 || amount != 10000 {
		t.Fatalf("reversal entries = %d debit/%d credit, total %d, want 1/1 and 10000", debit, credit, amount)
	}
	if got := testutil.AssertAudited(t, d.Pool, reversal.ID); got != 1 {
		t.Fatalf("reversal audit rows = %d, want 1", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 2 {
		t.Fatalf("total audit rows = %d, want 2 (post + reversal)", got)
	}

	balance, err := ledger.Balance(ctx, d.Pool, clearing, mustDate(t, "2026-06-30"))
	if err != nil {
		t.Fatalf("balance after reversal: %v", err)
	}
	if !balance.IsZero() || balance.Currency().Code() != "USD" {
		t.Fatalf("clearing balance after reversal = %s, want 0 USD", balance)
	}
}

// TestSecondReversalRejected proves a transaction can be reversed at most once.
func TestSecondReversalRejected(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	original := postCommitted(t, d, app, svc, chargeRequest(t, clearing, revenue, 1500, uniqueKey(t)))
	reverseCommitted(t, app, svc, ledger.ReverseRequest{
		TxnID:          original.ID,
		Reason:         "first reversal",
		IdempotencyKey: uniqueKey(t),
		Actor:          mustActor(t, "system:executor"),
	})

	err := reverseError(t, app, svc, ledger.ReverseRequest{
		TxnID:          original.ID,
		Reason:         "second reversal",
		IdempotencyKey: uniqueKey(t),
		Actor:          mustActor(t, "system:executor"),
	})
	if !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("second reversal error = %v, want ErrAlreadyReversed", err)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 2 {
		t.Fatalf("ledger_txns count = %d, want original + one reversal", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 2 {
		t.Fatalf("audit rows = %d, want 2", got)
	}
}

// TestReverseReplayIsIdempotent covers FIN-12 for the reversal write path.
func TestReverseReplayIsIdempotent(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	original := postCommitted(t, d, app, svc, chargeRequest(t, clearing, revenue, 800, uniqueKey(t)))
	req := ledger.ReverseRequest{
		TxnID:          original.ID,
		Reason:         "reversal",
		IdempotencyKey: uniqueKey(t),
		Actor:          mustActor(t, "system:executor"),
	}
	first := reverseCommitted(t, app, svc, req)
	second := reverseCommitted(t, app, svc, req)
	if second.ID != first.ID || second.Seq != first.Seq || !second.Replayed {
		t.Fatalf("reversal replay = {id:%s seq:%d replayed:%t}, want original reversal",
			second.ID, second.Seq, second.Replayed)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 2 {
		t.Fatalf("ledger_txns count = %d, want 2", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 2 {
		t.Fatalf("audit rows = %d, want 2", got)
	}
}

// TestReverseOfReversalAllowed proves the chain original -> reversal ->
// reversal-of-reversal is valid and each link carries reversal_of.
func TestReverseOfReversalAllowed(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	original := postCommitted(t, d, app, svc, chargeRequest(t, clearing, revenue, 900, uniqueKey(t)))
	first := reverseCommitted(t, app, svc, ledger.ReverseRequest{
		TxnID:          original.ID,
		Reason:         "reverse charge",
		IdempotencyKey: uniqueKey(t),
		Actor:          mustActor(t, "system:executor"),
	})
	second := reverseCommitted(t, app, svc, ledger.ReverseRequest{
		TxnID:          first.ID,
		Reason:         "reverse the reversal",
		IdempotencyKey: uniqueKey(t),
		Actor:          mustActor(t, "system:executor"),
	})
	if second.ReversalOf == nil || *second.ReversalOf != first.ID {
		t.Fatalf("second reversal_of = %v, want %s", second.ReversalOf, first.ID)
	}

	// original + first reversal netted zero; reversing the reversal restores the
	// original debit balance.
	balance, err := ledger.Balance(context.Background(), d.Pool, clearing, mustDate(t, "2026-06-30"))
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if got, want := balance.AmountMinor(), int64(900); got != want {
		t.Fatalf("clearing balance = %d, want %d", got, want)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 3 {
		t.Fatalf("audit rows = %d, want one per transaction", got)
	}
}

func TestReverseUnknownTransaction(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	err := reverseError(t, app, svc, ledger.ReverseRequest{
		TxnID:          "55555555-5555-4555-8555-555555555555",
		Reason:         "missing original",
		IdempotencyKey: uniqueKey(t),
		Actor:          mustActor(t, "system:executor"),
	})
	if !errors.Is(err, ledger.ErrTxnNotFound) {
		t.Fatalf("reverse unknown txn error = %v, want ErrTxnNotFound", err)
	}
}

func TestReverseRequiresReason(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	original := postCommitted(t, d, app, svc, chargeRequest(t, clearing, revenue, 100, uniqueKey(t)))

	err := reverseError(t, app, svc, ledger.ReverseRequest{
		TxnID:          original.ID,
		IdempotencyKey: uniqueKey(t),
		Actor:          mustActor(t, "system:executor"),
	})
	if !errors.Is(err, ledger.ErrInvalidRequest) {
		t.Fatalf("empty reason error = %v, want ErrInvalidRequest", err)
	}
}
