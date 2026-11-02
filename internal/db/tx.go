package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRetryExhausted is returned when a transaction keeps failing with a
// retryable SQLSTATE after the retry policy's attempt bound. The returned
// error wraps both ErrRetryExhausted and the last transaction error.
var ErrRetryExhausted = errors.New("db: retry attempts exhausted")

// Retryable SQLSTATE codes. Postgres has already rolled the transaction back
// when it reports either of these, so running the callback again is safe.
const (
	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"
)

// Retry policy defaults.
const (
	// DefaultMaxAttempts is the total number of attempts, including the first.
	DefaultMaxAttempts = 5
	// DefaultBaseDelay is the first backoff ceiling.
	DefaultBaseDelay = 10 * time.Millisecond
	// DefaultMaxDelay caps the backoff ceiling.
	DefaultMaxDelay = 500 * time.Millisecond
)

// Beginner starts transactions. *pgxpool.Pool and *pgx.Conn satisfy it.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// RetryPolicy bounds how WithTxPolicy retries a transaction. The zero value is
// valid: it means DefaultMaxAttempts, DefaultBaseDelay, DefaultMaxDelay, and a
// real context-aware sleep.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first.
	MaxAttempts int
	// BaseDelay is the first full-jitter ceiling.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff ceiling.
	MaxDelay time.Duration
	// Sleep waits for d or until ctx is done. Tests inject it to record delays.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = DefaultBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = DefaultMaxDelay
	}
	if p.Sleep == nil {
		p.Sleep = sleepContext
	}
	return p
}

// WithTx runs fn inside a transaction started with opts, retrying only
// serialization failures (SQLSTATE 40001) and deadlocks (SQLSTATE 40P01) with
// bounded, full-jitter exponential backoff.
//
// fn may run up to five times, so it must be re-runnable: it must not depend on
// side effects from an earlier attempt and must not perform network calls while
// the transaction is open (FIN-22).
//
// Unique violations (23505), context cancellation or deadline expiry, and all
// other errors are returned immediately without a retry. After the attempt
// bound WithTx returns an error that wraps both ErrRetryExhausted and the last
// transaction error.
func WithTx(ctx context.Context, b Beginner, opts pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	return WithTxPolicy(ctx, b, opts, RetryPolicy{}, fn)
}

// WithTxPolicy is WithTx with an explicit RetryPolicy.
func WithTxPolicy(ctx context.Context, b Beginner, opts pgx.TxOptions, p RetryPolicy, fn func(context.Context, pgx.Tx) error) error {
	p = p.withDefaults()

	var lastErr error
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := runTx(ctx, b, opts, fn)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if !isRetryable(err) {
			return err
		}

		lastErr = err
		if attempt == p.MaxAttempts-1 {
			break
		}
		if err := p.Sleep(ctx, p.backoff(attempt)); err != nil {
			return err
		}
	}

	return fmt.Errorf("%w: %w", ErrRetryExhausted, lastErr)
}

// runTx starts one transaction, runs fn, and commits or rolls back. The
// rollback uses a context that is not canceled so cleanup still happens when
// the caller's context expires; pgx does not roll back automatically.
func runTx(ctx context.Context, b Beginner, opts pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	tx, err := b.BeginTx(ctx, opts)
	if err != nil {
		return err
	}

	if err := fn(ctx, tx); err != nil {
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("db: rollback after callback error: %w (callback error: %v)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("db: commit failed: %w (rollback error: %v)", err, rbErr)
		}
		return err
	}
	return nil
}

// isRetryable reports whether err is a serialization failure or deadlock.
func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case sqlStateSerializationFailure, sqlStateDeadlockDetected:
		return true
	default:
		return false
	}
}

// backoff returns a full-jitter delay in [0, min(MaxDelay, BaseDelay*2^attempt)).
func (p RetryPolicy) backoff(attempt int) time.Duration {
	upper := p.BaseDelay
	for i := 0; i < attempt; i++ {
		if upper >= p.MaxDelay {
			upper = p.MaxDelay
			break
		}
		upper *= 2
	}
	if upper > p.MaxDelay {
		upper = p.MaxDelay
	}
	if upper <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(upper)))
}

// sleepContext waits for d, returning early if ctx is canceled.
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
