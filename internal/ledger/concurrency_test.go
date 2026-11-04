//go:build integration

// covers: FIN-10, FIN-12, FIN-13, FIN-30, FIN-56
package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

// TestConcurrentDoubleReversal races two different reversal keys against the same
// transaction. The row lock in Reverse must serialize them so exactly one
// reversal exists and the loser gets ErrAlreadyReversed.
func TestConcurrentDoubleReversal(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	original := postCommitted(t, d, app, svc, chargeRequest(t, clearing, revenue, 2500, uniqueKey(t)))

	requester := func(reason, key string) error {
		return db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := svc.Reverse(ctx, tx, ledger.ReverseRequest{
				TxnID:          original.ID,
				Reason:         reason,
				IdempotencyKey: key,
				Actor:          mustActor(t, "system:executor"),
			})
			return err
		})
	}

	keys := []string{uniqueKey(t), uniqueKey(t)}
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		reason := "concurrent reversal a"
		if i == 1 {
			reason = "concurrent reversal b"
		}
		wg.Add(1)
		go func(i int, reason, key string) {
			defer wg.Done()
			<-start
			errs[i] = requester(reason, key)
		}(i, reason, keys[i])
	}
	close(start)
	wg.Wait()

	successes := 0
	alreadyReversed := 0
	for i, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ledger.ErrAlreadyReversed):
			alreadyReversed++
		default:
			t.Fatalf("goroutine %d: unexpected error %v", i, err)
		}
	}
	if successes != 1 || alreadyReversed != 1 {
		t.Fatalf("successes/already-reversed = %d/%d, want 1/1", successes, alreadyReversed)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 2 {
		t.Fatalf("ledger_txns count = %d, want original + one reversal", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns WHERE reversal_of = $1::uuid`, original.ID); got != 1 {
		t.Fatalf("reversal rows = %d, want 1", got)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != 2 {
		t.Fatalf("audit rows = %d, want 2", got)
	}
	var reversalID string
	if err := d.Pool.QueryRow(ctx,
		`SELECT id::text FROM ledger_txns WHERE reversal_of = $1::uuid`, original.ID).Scan(&reversalID); err != nil {
		t.Fatalf("read reversal id: %v", err)
	}
	if got := testutil.AssertAudited(t, d.Pool, reversalID); got != 1 {
		t.Fatalf("reversal audit rows = %d, want 1", got)
	}
}
