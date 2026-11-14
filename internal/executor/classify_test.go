package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsInfrastructureError pins the apply-time failure boundary: only
// infrastructure/transient errors leave the proposal approved.
func TestIsInfrastructureError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain_validation", errors.New("invalid payload"), false},
		{"pgx_no_rows", pgx.ErrNoRows, false},
		{"wrapped_pgx_no_rows", fmt.Errorf("load external record: %w", pgx.ErrNoRows), false},
		{"context_canceled", context.Canceled, true},
		{"wrapped_context_canceled", fmt.Errorf("resolve proposal state: %w", context.Canceled), true},
		{"context_deadline", context.DeadlineExceeded, true},
		{"wrapped_context_deadline", fmt.Errorf("apply: %w", context.DeadlineExceeded), true},
		{"tx_closed", pgx.ErrTxClosed, true},
		{"permission_denied", &pgconn.PgError{Code: "42501", Message: "permission denied"}, true},
		{"wrapped_permission_denied", fmt.Errorf("resolve case: %w",
			&pgconn.PgError{Code: "42501", Message: "permission denied"}), true},
		{"statement_timeout", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, true},
		{"serialization_failure", &pgconn.PgError{Code: "40001"}, true},
		{"wrapped_unique_violation", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"}), true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := isInfrastructureError(tc.err); got != tc.want {
				t.Errorf("isInfrastructureError(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// TestRevalidationFailureFor pins the classification result: nil stays nil,
// infrastructure errors pass through unchanged, and deterministic errors become
// a revalidationFailure with the requested reason.
func TestRevalidationFailureFor(t *testing.T) {
	if got := revalidationFailureFor(nil, FailureStateChanged); got != nil {
		t.Fatalf("revalidationFailureFor(nil) = %v, want nil", got)
	}

	infra := fmt.Errorf("resolve proposal state: %w", &pgconn.PgError{Code: "42501"})
	got := revalidationFailureFor(infra, FailureStateChanged)
	if got != infra {
		t.Fatalf("infrastructure error = %v, want the original error unchanged", got)
	}
	var failure *revalidationFailure
	if errors.As(got, &failure) {
		t.Fatal("infrastructure error was wrapped as revalidationFailure")
	}

	deterministic := fmt.Errorf("external record %s does not exist", "ext:psp_a:rec-1")
	got = revalidationFailureFor(deterministic, FailureStateChanged)
	if !errors.As(got, &failure) {
		t.Fatalf("deterministic error = %v, want *revalidationFailure", got)
	}
	if failure.reason != FailureStateChanged {
		t.Errorf("failure reason = %q, want %q", failure.reason, FailureStateChanged)
	}
	if !errors.Is(got, deterministic) {
		t.Errorf("failure does not wrap the deterministic cause: %v", got)
	}
}

// TestCitedLedgerTxnIDs pins the cited-evidence parser used by the value-date
// fallback: only canonical ledger_txn refs are used, duplicates collapse, and
// malformed refs are deterministic errors.
func TestCitedLedgerTxnIDs(t *testing.T) {
	first := "11111111-1111-4111-8111-111111111111"
	second := "22222222-2222-4222-8222-222222222222"

	got, err := citedLedgerTxnIDs([]string{
		"case:33333333-3333-4333-8333-333333333333",
		"ledger_txn:" + second,
		"ext:psp_a:rec-1",
		"ledger_txn:" + first,
		"ledger_txn:" + second,
	})
	if err != nil {
		t.Fatalf("citedLedgerTxnIDs: %v", err)
	}
	if len(got) != 2 || got[0] != second || got[1] != first {
		t.Errorf("cited ledger ids = %v, want [%s %s]", got, second, first)
	}

	if _, err := citedLedgerTxnIDs([]string{"ledger_txn:not-a-uuid"}); err == nil {
		t.Fatal("malformed ledger ref was accepted")
	}
	if _, err := citedLedgerTxnIDs([]string{"ledger_txn:AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}); err == nil {
		t.Fatal("uppercase ledger ref was accepted")
	}
}
