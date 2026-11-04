//go:build integration

// covers: FIN-54

package tieout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// TestComputeCleanDayTies pins the FIN-54 happy path: a clean synthetic day
// with matched transactions plus one open item on each side returns an
// unexplained difference of exactly zero. The open items are deliberately
// included so the equation is exercised, not just the trivial no-activity case.
func TestComputeCleanDayTies(t *testing.T) {
	f := newFixture(t, "psp_a")
	f.addExternal(t, "C-EXACT-1", 10_000, "2026-06-10")
	f.addLedger(t, "C-EXACT-1", 10_000, "2026-06-10")
	f.addExternal(t, "C-EXACT-2", 5_000, "2026-06-10")
	f.addLedger(t, "C-EXACT-2", 5_000, "2026-06-10")

	// Timing differences: the bank statement has a record the ledger has not
	// booked yet, and the ledger has a booking the statement has not seen.
	f.addExternal(t, "C-EXT-ONLY", 2_000, "2026-06-10")
	f.addLedger(t, "C-LEDGER-ONLY", 1_000, "2026-06-10")

	f.runMatcher(t, "2026-06-01", "2026-06-30", 100)

	// Ledger balance = 10,000 + 5,000 + 1,000 = 16,000.
	// Statement closing includes the external-only item = 17,000.
	f.addStatement(t, "2026-06-10", 0, 17_000)

	result, err := tieout.Compute(context.Background(), f.db.Pool, f.scopeID, mustDate(t, "2026-06-10"))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got, want := result.Currency, "USD"; got != want {
		t.Errorf("currency = %q, want %q", got, want)
	}
	if got, want := result.LedgerBalance.AmountMinor(), int64(16_000); got != want {
		t.Errorf("ledger balance = %d, want %d", got, want)
	}
	if got, want := result.StatementClosing.AmountMinor(), int64(17_000); got != want {
		t.Errorf("statement closing = %d, want %d", got, want)
	}
	if got, want := result.LedgerOnly.AmountMinor(), int64(1_000); got != want {
		t.Errorf("ledger-only amount = %d, want %d", got, want)
	}
	if got, want := result.LedgerOnlyCount, 1; got != want {
		t.Errorf("ledger-only count = %d, want %d", got, want)
	}
	if got, want := result.ExternalOnly.AmountMinor(), int64(2_000); got != want {
		t.Errorf("external-only amount = %d, want %d", got, want)
	}
	if got, want := result.ExternalOnlyCount, 1; got != want {
		t.Errorf("external-only count = %d, want %d", got, want)
	}
	if !result.Unexplained.IsZero() {
		t.Errorf("unexplained = %d, want exactly 0", result.Unexplained.AmountMinor())
	}
}

// TestComputeAsOfIsInclusive pins D-2: value_date <= asOf is included and the
// next day is not. The result fields, not just the zero difference, prove the
// boundary because equal open items on both sides would still net to zero.
func TestComputeAsOfIsInclusive(t *testing.T) {
	f := newFixture(t, "psp_a")
	f.addExternal(t, "ASOF-EXT", 100, "2026-06-10")
	f.addLedger(t, "ASOF-LEDGER", 100, "2026-06-10")
	f.addExternal(t, "AFTER-EXT", 500, "2026-06-11")
	f.addLedger(t, "AFTER-LEDGER", 500, "2026-06-11")
	f.addStatement(t, "2026-06-10", 0, 100)

	result, err := tieout.Compute(context.Background(), f.db.Pool, f.scopeID, mustDate(t, "2026-06-10"))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got, want := result.LedgerBalance.AmountMinor(), int64(100); got != want {
		t.Errorf("ledger balance = %d, want %d (asOf is inclusive)", got, want)
	}
	if got, want := result.LedgerOnly.AmountMinor(), int64(100); got != want {
		t.Errorf("ledger-only amount = %d, want %d", got, want)
	}
	if got, want := result.LedgerOnlyCount, 1; got != want {
		t.Errorf("ledger-only count = %d, want %d (asOf+1 must be excluded)", got, want)
	}
	if got, want := result.ExternalOnly.AmountMinor(), int64(100); got != want {
		t.Errorf("external-only amount = %d, want %d", got, want)
	}
	if got, want := result.ExternalOnlyCount, 1; got != want {
		t.Errorf("external-only count = %d, want %d (asOf+1 must be excluded)", got, want)
	}
	if !result.Unexplained.IsZero() {
		t.Errorf("unexplained = %d, want exactly 0", result.Unexplained.AmountMinor())
	}
}

// TestComputeMissingStatement pins D-2: a missing statement row is an error,
// never a zero closing balance.
func TestComputeMissingStatement(t *testing.T) {
	f := newFixture(t, "psp_a")
	f.addLedger(t, "NO-STMT", 100, "2026-06-10")

	_, err := tieout.Compute(context.Background(), f.db.Pool, f.scopeID, mustDate(t, "2026-06-10"))
	if !errors.Is(err, tieout.ErrNoStatement) {
		t.Fatalf("Compute error = %v, want ErrNoStatement", err)
	}
}

// TestComputeMissingScope pins the caller error path.
func TestComputeMissingScope(t *testing.T) {
	f := newFixture(t, "psp_a")
	_, err := tieout.Compute(context.Background(), f.db.Pool,
		"99999999-9999-4999-8999-999999999999", mustDate(t, "2026-06-10"))
	if !errors.Is(err, tieout.ErrScopeNotFound) {
		t.Fatalf("Compute error = %v, want ErrScopeNotFound", err)
	}
}

// TestComputeMultiCurrencyRejectsMismatch pins that only the scope's currency is
// summed and a mismatched-currency row in the scope is an error, never silently
// included.
func TestComputeMultiCurrencyRejectsMismatch(t *testing.T) {
	t.Run("statement currency differs from scope", func(t *testing.T) {
		f := newFixture(t, "psp_a")
		f.addLedger(t, "CCY-STMT", 100, "2026-06-10")
		f.addStatementCurrency(t, "2026-06-10", 0, 100, "EUR")

		_, err := tieout.Compute(context.Background(), f.db.Pool, f.scopeID, mustDate(t, "2026-06-10"))
		if !errors.Is(err, tieout.ErrCurrencyMismatch) {
			t.Fatalf("Compute error = %v, want ErrCurrencyMismatch", err)
		}
	})

	t.Run("account currency differs from scope", func(t *testing.T) {
		f := newFixtureWithCurrencies(t, "psp_a", "USD", "EUR")
		f.addLedger(t, "CCY-ACCOUNT", 100, "2026-06-10")
		f.addStatement(t, "2026-06-10", 0, 100)

		_, err := tieout.Compute(context.Background(), f.db.Pool, f.scopeID, mustDate(t, "2026-06-10"))
		if !errors.Is(err, tieout.ErrCurrencyMismatch) {
			t.Fatalf("Compute error = %v, want ErrCurrencyMismatch", err)
		}
	})
}
