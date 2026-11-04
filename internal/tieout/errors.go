package tieout

import "errors"

// Errors returned by Compute. They are sentinels so callers can distinguish a
// caller mistake from a missing statement or a data problem.
var (
	// ErrInvalidRequest means Compute was called without a queryer, scope id,
	// or as-of date, or the scope references a non-asset account.
	ErrInvalidRequest = errors.New("tieout: invalid request")
	// ErrScopeNotFound means no reconciliation scope has the requested id.
	ErrScopeNotFound = errors.New("tieout: scope not found")
	// ErrNoStatement means the scope has no statement_balances row for the
	// requested as-of date. A missing statement is never a zero balance (D-2).
	ErrNoStatement = errors.New("tieout: statement balance not found")
	// ErrCurrencyMismatch means the scope, ledger account, or statement row
	// disagree about currency. The tie-out never silently mixes currencies.
	ErrCurrencyMismatch = errors.New("tieout: currency mismatch")
)
