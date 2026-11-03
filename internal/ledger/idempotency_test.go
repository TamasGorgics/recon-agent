//go:build integration

// covers: FIN-10, FIN-12, FIN-13, FIN-30, FIN-56
package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

// TestIdempotentReplayReturnsOriginal replays the same key and payload: the
// original row comes back, and no second transaction, entry, or audit row is
// written (FIN-12).
func TestIdempotentReplayReturnsOriginal(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	clk := fakeClock(t)
	svc := ledger.NewService(clk, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	key := uniqueKey(t)
	req := chargeRequest(t, clearing, revenue, 1234, key)
	first := postCommitted(t, d, app, svc, req)

	// Advancing the clock must not matter: timestamps are not part of the
	// canonical request hash, so a retry is still a replay.
	clk.Advance(90 * time.Minute)
	second := postCommitted(t, d, app, svc, req)
	if second.ID != first.ID || second.Seq != first.Seq {
		t.Fatalf("replay = {id:%s seq:%d}, want original {id:%s seq:%d}", second.ID, second.Seq, first.ID, first.Seq)
	}
	if !second.Replayed {
		t.Error("replay did not report Replayed = true")
	}

	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`, key); got != 1 {
		t.Fatalf("txn rows for key = %d, want 1", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_entries WHERE txn_id = $1::uuid`, first.ID); got != 2 {
		t.Fatalf("entry rows = %d, want 2", got)
	}
	if got := testutil.AssertAudited(t, d.Pool, first.ID); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}

// TestIdempotentReplayConflictsOnChangedPayload covers a changed description
// and a changed amount under the same key (FIN-12).
func TestIdempotentReplayConflictsOnChangedPayload(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	key := uniqueKey(t)
	original := chargeRequest(t, clearing, revenue, 500, key)
	res := postCommitted(t, d, app, svc, original)

	changedDescription := original
	changedDescription.Description = "different description"
	if err := postError(t, d, app, svc, changedDescription); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("changed-description error = %v, want ErrIdempotencyConflict", err)
	}

	changedAmount := original
	changedAmount.Lines = []ledger.Line{
		{AccountID: clearing, Direction: ledger.Debit, AmountMinor: 501, Currency: "USD"},
		{AccountID: revenue, Direction: ledger.Credit, AmountMinor: 501, Currency: "USD"},
	}
	if err := postError(t, d, app, svc, changedAmount); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("changed-lines error = %v, want ErrIdempotencyConflict", err)
	}

	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 1 {
		t.Fatalf("ledger_txns count = %d, want 1", got)
	}
	if got := testutil.AssertAudited(t, d.Pool, res.ID); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}

// TestConcurrentPostSameKey runs 50 goroutines with the same key and payload.
// Exactly one transaction (and one audit row) may exist; every caller gets the
// original result (FIN-12).
func TestConcurrentPostSameKey(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	key := uniqueKey(t)
	req := chargeRequest(t, clearing, revenue, 4321, key)

	const goroutines = 50
	results := make([]ledger.Result, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				result, err := svc.Post(ctx, tx, req)
				if err != nil {
					return err
				}
				results[i] = result
				return nil
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var winner string
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v (want the original transaction or a retryable error)", i, errs[i])
		}
		if winner == "" {
			winner = results[i].ID
		}
		if results[i].ID != winner {
			t.Fatalf("goroutine %d returned id %s, want %s", i, results[i].ID, winner)
		}
	}
	if winner == "" {
		t.Fatal("no winner returned")
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`, key); got != 1 {
		t.Fatalf("txn rows = %d, want 1", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_entries WHERE txn_id = $1::uuid`, winner); got != 2 {
		t.Fatalf("entry rows = %d, want 2", got)
	}
	if got := testutil.AssertAudited(t, d.Pool, winner); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 1 {
		t.Fatalf("total audit rows = %d, want 1", got)
	}
}

// TestConcurrentPostSameKeyConflictingPayload races two payloads under one key:
// exactly one transaction wins and every loser gets ErrIdempotencyConflict.
func TestConcurrentPostSameKeyConflictingPayload(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	key := uniqueKey(t)
	small := chargeRequest(t, clearing, revenue, 100, key)
	large := chargeRequest(t, clearing, revenue, 200, key)

	const goroutines = 20
	errs := make([]error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		req := small
		if i%2 == 1 {
			req = large
		}
		wg.Add(1)
		go func(i int, req ledger.PostRequest) {
			defer wg.Done()
			<-start
			errs[i] = db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				_, err := svc.Post(ctx, tx, req)
				return err
			})
		}(i, req)
	}
	close(start)
	wg.Wait()

	wins := 0
	conflicts := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ledger.ErrIdempotencyConflict):
			conflicts++
		default:
			t.Fatalf("goroutine %d: unexpected error %v", i, err)
		}
	}
	if wins != goroutines/2 || conflicts != goroutines/2 {
		t.Fatalf("wins/conflicts = %d/%d, want %d/%d", wins, conflicts, goroutines/2, goroutines/2)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`, key); got != 1 {
		t.Fatalf("txn rows = %d, want 1", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}
