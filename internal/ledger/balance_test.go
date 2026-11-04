//go:build integration

// covers: FIN-10, FIN-12, FIN-13, FIN-30, FIN-56
package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
)

// TestBalanceSignsAndInclusiveAsOf checks D-14 normal-balance signs on a
// hand-computed two-currency ledger and proves the as-of filter is inclusive of
// value_date = asOf (D-2).
func TestBalanceSignsAndInclusiveAsOf(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})

	clearingUSD := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenueUSD := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	expenseUSD := newAccount(t, d, "expense_usd", "USD", "expense", "active")
	payableUSD := newAccount(t, d, "payable_usd", "USD", "liability", "active")
	clearingEUR := newAccount(t, d, "clearing_eur", "EUR", "asset", "active")
	revenueEUR := newAccount(t, d, "revenue_eur", "EUR", "revenue", "active")

	post := func(valueDate string, lines []ledger.Line) {
		t.Helper()
		req := ledger.PostRequest{
			IdempotencyKey: uniqueKey(t),
			Kind:           ledger.KindCharge,
			ValueDate:      mustDate(t, valueDate),
			Description:    "balance fixture " + valueDate,
			Actor:          mustActor(t, "system:executor"),
			Lines:          lines,
		}
		postCommitted(t, d, app, svc, req)
	}

	// 2026-01-01: charge, clearing asset +1000, revenue +1000.
	post("2026-01-01", []ledger.Line{
		{AccountID: clearingUSD, Direction: ledger.Debit, AmountMinor: 1000, Currency: "USD"},
		{AccountID: revenueUSD, Direction: ledger.Credit, AmountMinor: 1000, Currency: "USD"},
	})
	// 2026-01-05: expense incurred, payable liability +1000.
	post("2026-01-05", []ledger.Line{
		{AccountID: expenseUSD, Direction: ledger.Debit, AmountMinor: 1000, Currency: "USD"},
		{AccountID: payableUSD, Direction: ledger.Credit, AmountMinor: 1000, Currency: "USD"},
	})
	// 2026-01-10: charge, clearing asset +500.
	post("2026-01-10", []ledger.Line{
		{AccountID: clearingUSD, Direction: ledger.Debit, AmountMinor: 500, Currency: "USD"},
		{AccountID: revenueUSD, Direction: ledger.Credit, AmountMinor: 500, Currency: "USD"},
	})
	// 2026-01-20: settle part of the payable, clearing asset -300.
	post("2026-01-20", []ledger.Line{
		{AccountID: payableUSD, Direction: ledger.Debit, AmountMinor: 300, Currency: "USD"},
		{AccountID: clearingUSD, Direction: ledger.Credit, AmountMinor: 300, Currency: "USD"},
	})
	// 2026-02-01: EUR charge, clearing EUR +2000.
	post("2026-02-01", []ledger.Line{
		{AccountID: clearingEUR, Direction: ledger.Debit, AmountMinor: 2000, Currency: "EUR"},
		{AccountID: revenueEUR, Direction: ledger.Credit, AmountMinor: 2000, Currency: "EUR"},
	})
	// 2026-03-01: USD refund, clearing asset -200, revenue -200.
	post("2026-03-01", []ledger.Line{
		{AccountID: revenueUSD, Direction: ledger.Debit, AmountMinor: 200, Currency: "USD"},
		{AccountID: clearingUSD, Direction: ledger.Credit, AmountMinor: 200, Currency: "USD"},
	})

	cases := []struct {
		name    string
		account string
		asOf    string
		want    int64
		ccy     string
	}{
		{name: "before any activity", account: clearingUSD, asOf: "2025-12-31", want: 0, ccy: "USD"},
		{name: "before second charge", account: clearingUSD, asOf: "2026-01-09", want: 1000, ccy: "USD"},
		{name: "inclusive of first charge", account: clearingUSD, asOf: "2026-01-01", want: 1000, ccy: "USD"},
		{name: "inclusive of second charge", account: clearingUSD, asOf: "2026-01-10", want: 1500, ccy: "USD"},
		{name: "after settlement", account: clearingUSD, asOf: "2026-01-20", want: 1200, ccy: "USD"},
		{name: "after refund", account: clearingUSD, asOf: "2026-03-01", want: 1000, ccy: "USD"},
		{name: "liability credits less debits", account: payableUSD, asOf: "2026-01-05", want: 1000, ccy: "USD"},
		{name: "liability after settlement", account: payableUSD, asOf: "2026-01-20", want: 700, ccy: "USD"},
		{name: "expense debits less credits", account: expenseUSD, asOf: "2026-06-30", want: 1000, ccy: "USD"},
		{name: "eur asset", account: clearingEUR, asOf: "2026-02-01", want: 2000, ccy: "EUR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			balance, err := ledger.Balance(ctx, d.Pool, tc.account, mustDate(t, tc.asOf))
			if err != nil {
				t.Fatalf("Balance(%s, %s): %v", tc.account, tc.asOf, err)
			}
			if got := balance.AmountMinor(); got != tc.want {
				t.Errorf("balance = %d, want %d", got, tc.want)
			}
			if got := balance.Currency().Code(); got != tc.ccy {
				t.Errorf("balance currency = %s, want %s", got, tc.ccy)
			}
		})
	}
}

func TestBalanceRejectsUnknownAccountAndZeroDate(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)

	if _, err := ledger.Balance(ctx, d.Pool, "66666666-6666-4666-8666-666666666666", mustDate(t, "2026-01-01")); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatalf("unknown account error = %v, want ErrAccountNotFound", err)
	}
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	if _, err := ledger.Balance(ctx, d.Pool, clearing, timeZero()); !errors.Is(err, ledger.ErrInvalidRequest) {
		t.Fatalf("zero as-of error = %v, want ErrInvalidRequest", err)
	}
}
