package executor

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// isInfrastructureError reports whether err means the executor could not make
// an apply decision. Infrastructure and transient failures must be returned to
// the caller unchanged so the proposal stays approved for the next poll; they
// must never be recorded as apply_failed.
//
// A plain validation error and pgx.ErrNoRows are deliberately not
// infrastructure. A *pgconn.PgError is always infrastructure, including
// permission denied (42501), statement timeout (57014), serialization failure
// (40001), and deadlock (40P01): the apply transaction was aborted, not the
// proposal invalidated. Call sites that know a specific SQLSTATE is a
// deterministic conflict (for example a unique violation on matches or
// match_legs) must handle it before calling the classifier.
func isInfrastructureError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, pgx.ErrTxClosed) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr)
}

// revalidationFailureFor classifies err for Apply. A nil error stays nil, an
// infrastructure/transient error is returned unchanged (and leaves the
// proposal approved), and every other error becomes a deterministic
// revalidationFailure with the given reason.
func revalidationFailureFor(err error, reason string) error {
	if err == nil {
		return nil
	}
	if isInfrastructureError(err) {
		return err
	}
	return &revalidationFailure{reason: reason, cause: err}
}
