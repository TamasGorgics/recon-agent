package main

import (
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

func TestTieoutJSONStable(t *testing.T) {
	usd := money.MustParseCurrency("USD")
	result := tieout.Result{
		ScopeID:           "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		AsOf:              time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC),
		Currency:          "USD",
		LedgerBalance:     money.FromMinor(16_000, usd),
		StatementClosing:  money.FromMinor(17_000, usd),
		LedgerOnly:        money.FromMinor(1_000, usd),
		LedgerOnlyCount:   1,
		ExternalOnly:      money.FromMinor(2_000, usd),
		ExternalOnlyCount: 1,
		Unexplained:       money.FromMinor(0, usd),
	}

	encoded, err := result.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	want := `{"scope_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","as_of":"2026-06-10","currency":"USD",` +
		`"ledger_balance":{"amount_minor":16000,"currency":"USD"},` +
		`"statement_closing":{"amount_minor":17000,"currency":"USD"},` +
		`"ledger_only":{"amount":{"amount_minor":1000,"currency":"USD"},"count":1},` +
		`"external_only":{"amount":{"amount_minor":2000,"currency":"USD"},"count":1},` +
		`"unexplained":{"amount_minor":0,"currency":"USD"}}`
	if string(encoded) != want {
		t.Fatalf("JSON =\n%s\nwant\n%s", encoded, want)
	}
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: 0},
		{name: "error", err: errors.New("boom"), want: 1},
		{name: "difference", err: &exitError{code: 2, err: errors.New("difference")}, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
