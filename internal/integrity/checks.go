package integrity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// queryer is the run transaction every check operates on. It is a pgx.Tx so a
// check can also create a nested savepoint when an optional API (tie-out) can
// return after a SQL error.
type queryer = pgx.Tx

// checkOptions carries the inputs that vary by run.
type checkOptions struct {
	RunDate        Date
	TieoutObserver tieout.Observer
}

// checkError is a stable, bounded cause for a check that could not complete.
// Its code is written verbatim as the check_error detail; the wrapped database
// error is never persisted or accepted into a problem detail.
type checkError struct {
	code string
	err  error
}

func (e *checkError) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.code + ": " + e.err.Error()
}

func (e *checkError) Unwrap() error { return e.err }

// errorCause extracts the bounded classification for a check_error detail.
func errorCause(err error) string {
	var checkErr *checkError
	if errors.As(err, &checkErr) {
		return checkErr.code
	}
	return "internal"
}

// executeCheck runs one check by name.
func executeCheck(ctx context.Context, q queryer, name CheckName, opts checkOptions) (CheckResult, error) {
	switch name {
	case CheckLedgerBalance:
		return checkLedgerBalance(ctx, q)
	case CheckValueConservation:
		return checkValueConservation(ctx, q)
	case CheckAppliedProposalTrail:
		return checkAppliedProposalTrail(ctx, q)
	case CheckAuditChain:
		return checkAuditChain(ctx, q)
	case CheckTieout:
		return checkTieout(ctx, q, opts)
	default:
		return CheckResult{}, fmt.Errorf("integrity: unknown check %q", name)
	}
}

// parseInt64 parses a PostgreSQL numeric/bigint text value without truncation.
// A value outside int64 is a check error, never a wrapped or rounded amount.
func parseInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, &checkError{code: "numeric_overflow", err: err}
	}
	return parsed, nil
}

// addInt64 returns a+b and false on int64 overflow.
func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// subInt64 returns a-b and false on int64 overflow.
func subInt64(a, b int64) (int64, bool) {
	if b == math.MinInt64 {
		return 0, false
	}
	return addInt64(a, -b)
}
