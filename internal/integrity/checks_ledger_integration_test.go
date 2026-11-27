//go:build integration

// covers: FIN-52
package integrity_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/integrity"
)

// TestLedgerBalance proves the independent per-transaction balance check: a
// clean pair passes, an unbalanced currency is reported with its signed net, a
// zero-entry transaction is reported, and the deferred ledger trigger still
// rejects a normal-path unbalanced transaction.
func TestLedgerBalance(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		f := newFixture(t)
		result := f.mustRun(t, integrity.Options{})
		check := checkByName(t, result.Report, integrity.CheckLedgerBalance)
		if check.Status != integrity.StatusPassed || check.Checked != 2 {
			t.Fatalf("ledger_balance status/checked = %q/%d, want passed/2", check.Status, check.Checked)
		}
	})

	t.Run("unbalanced", func(t *testing.T) {
		f := newFixture(t)
		f.tamper(t, func(ctx context.Context, tx pgx.Tx) {
			tag, err := tx.Exec(ctx,
				`UPDATE ledger_entries SET amount_minor = 9990
				  WHERE txn_id = $1::uuid AND direction = 'debit'`, f.chargeTxnID)
			if err != nil {
				t.Fatalf("corrupt ledger entry: %v", err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("corrupted %d entries, want 1", tag.RowsAffected())
			}
		})
		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckLedgerBalance,
			integrity.CodeLedgerTxnUnbalanced)
		if problem.ID != f.chargeTxnID {
			t.Fatalf("unbalanced problem id = %q, want %q", problem.ID, f.chargeTxnID)
		}
		if !strings.Contains(problem.Detail, "currency=USD net_minor=-10") {
			t.Fatalf("unbalanced detail = %q, want currency=USD net_minor=-10", problem.Detail)
		}
	})

	t.Run("zero entries", func(t *testing.T) {
		f := newFixture(t)
		var txnID string
		f.tamper(t, func(ctx context.Context, tx pgx.Tx) {
			if err := tx.QueryRow(ctx,
				`INSERT INTO ledger_txns
				     (value_date, kind, idempotency_key, request_hash, description, period_id)
				 VALUES ($1::date, 'charge', 'zero-entry-1', 'fixture-hash', '', $2::uuid)
				 RETURNING id::text`, fixtureValueDay, f.periodID).Scan(&txnID); err != nil {
				t.Fatalf("insert zero-entry txn: %v", err)
			}
		})
		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckLedgerBalance,
			integrity.CodeLedgerTxnNoEntries)
		if problem.ID != txnID || problem.Detail != "entries=0" {
			t.Fatalf("zero-entry problem = %+v, want id %s detail entries=0", problem, txnID)
		}
	})

	t.Run("deferred trigger intact", func(t *testing.T) {
		f := newFixture(t)
		var triggers int
		if err := f.db.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_trigger
			  WHERE tgname IN ('ledger_txns_balanced', 'ledger_entries_balanced')`).
			Scan(&triggers); err != nil {
			t.Fatalf("count ledger triggers: %v", err)
		}
		if triggers != 2 {
			t.Fatalf("ledger balance triggers = %d, want 2", triggers)
		}

		ctx := context.Background()
		tx, err := f.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin normal-path tx: %v", err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		var txnID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO ledger_txns
			     (value_date, kind, idempotency_key, request_hash, description, period_id)
			 VALUES ($1::date, 'charge', 'trigger-still-on', 'fixture-hash', '', $2::uuid)
			 RETURNING id::text`, fixtureValueDay, f.periodID).Scan(&txnID); err != nil {
			t.Fatalf("insert normal-path txn: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
			 VALUES ($1::uuid, $2::uuid, 'debit', 100, 'USD')`,
			txnID, f.accounts["psp_clearing_usd"]); err != nil {
			t.Fatalf("insert one-sided entry: %v", err)
		}
		if err := tx.Commit(ctx); err == nil {
			t.Fatal("normal-path unbalanced transaction committed; the deferred trigger is weakened")
		} else if !strings.Contains(fmt.Sprint(err), "balanced") && !strings.Contains(fmt.Sprint(err), "check") {
			t.Fatalf("unexpected trigger error: %v", err)
		}
	})
}
