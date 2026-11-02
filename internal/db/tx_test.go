package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeBeginner counts BeginTx calls and returns a fresh fakeTx for each one.
type fakeBeginner struct {
	calls   int
	beginTx func(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

func (f *fakeBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return f.BeginTx(ctx, pgx.TxOptions{})
}

func (f *fakeBeginner) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	f.calls++
	if f.beginTx != nil {
		return f.beginTx(ctx, opts)
	}
	return &fakeTx{}, nil
}

// fakeTx embeds pgx.Tx so only the methods the helper uses are implemented.
type fakeTx struct {
	pgx.Tx
	commits   int
	rollbacks int
}

func (tx *fakeTx) Commit(context.Context) error {
	tx.commits++
	return nil
}

func (tx *fakeTx) Rollback(context.Context) error {
	tx.rollbacks++
	return nil
}

func noSleepPolicy() RetryPolicy {
	return RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}
}

func serializationFailure() error {
	return &pgconn.PgError{Code: sqlStateSerializationFailure, Message: "could not serialize access due to concurrent update"}
}

func deadlockDetected() error {
	return &pgconn.PgError{Code: sqlStateDeadlockDetected, Message: "deadlock detected"}
}

func TestWithTxRetriesSerializationFailure(t *testing.T) {
	var attempts int
	b := &fakeBeginner{}
	err := WithTxPolicy(context.Background(), b, pgx.TxOptions{IsoLevel: pgx.Serializable}, noSleepPolicy(),
		func(context.Context, pgx.Tx) error {
			attempts++
			if attempts <= 2 {
				return serializationFailure()
			}
			return nil
		})
	if err != nil {
		t.Fatalf("WithTxPolicy() error = %v, want nil", err)
	}
	if attempts != 3 {
		t.Fatalf("callback attempts = %d, want 3", attempts)
	}
	if b.calls != 3 {
		t.Fatalf("BeginTx calls = %d, want 3", b.calls)
	}
}

func TestWithTxRetriesDeadlock(t *testing.T) {
	var attempts int
	b := &fakeBeginner{}
	err := WithTxPolicy(context.Background(), b, pgx.TxOptions{}, noSleepPolicy(),
		func(context.Context, pgx.Tx) error {
			attempts++
			if attempts == 1 {
				return deadlockDetected()
			}
			return nil
		})
	if err != nil {
		t.Fatalf("WithTxPolicy() error = %v, want nil", err)
	}
	if attempts != 2 {
		t.Fatalf("callback attempts = %d, want 2", attempts)
	}
	if b.calls != 2 {
		t.Fatalf("BeginTx calls = %d, want 2", b.calls)
	}
}

func TestWithTxGivesUpAfterBound(t *testing.T) {
	var attempts int
	b := &fakeBeginner{}
	err := WithTxPolicy(context.Background(), b, pgx.TxOptions{}, noSleepPolicy(),
		func(context.Context, pgx.Tx) error {
			attempts++
			return serializationFailure()
		})
	if attempts != DefaultMaxAttempts {
		t.Fatalf("callback attempts = %d, want %d", attempts, DefaultMaxAttempts)
	}
	if b.calls != DefaultMaxAttempts {
		t.Fatalf("BeginTx calls = %d, want %d", b.calls, DefaultMaxAttempts)
	}
	if !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("error = %v, want errors.Is(ErrRetryExhausted)", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlStateSerializationFailure {
		t.Fatalf("error = %v, want wrapped SQLSTATE %s", err, sqlStateSerializationFailure)
	}
}

func TestWithTxDoesNotRetryNonRetryable(t *testing.T) {
	unique := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	var attempts int
	b := &fakeBeginner{}
	err := WithTxPolicy(context.Background(), b, pgx.TxOptions{}, noSleepPolicy(),
		func(context.Context, pgx.Tx) error {
			attempts++
			return unique
		})
	if attempts != 1 {
		t.Fatalf("callback attempts = %d, want 1", attempts)
	}
	if b.calls != 1 {
		t.Fatalf("BeginTx calls = %d, want 1", b.calls)
	}
	if !errors.Is(err, unique) {
		t.Fatalf("error = %v, want the original 23505 error", err)
	}
	if errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("error = %v, must not wrap ErrRetryExhausted", err)
	}
}

func TestWithTxStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts, sleeps int
	b := &fakeBeginner{}
	err := WithTxPolicy(ctx, b, pgx.TxOptions{},
		RetryPolicy{Sleep: func(context.Context, time.Duration) error {
			sleeps++
			return nil
		}},
		func(ctx context.Context, _ pgx.Tx) error {
			attempts++
			cancel()
			return ctx.Err()
		})
	if attempts != 1 {
		t.Fatalf("callback attempts = %d, want 1", attempts)
	}
	if b.calls != 1 {
		t.Fatalf("BeginTx calls = %d, want 1", b.calls)
	}
	if sleeps != 0 {
		t.Fatalf("sleep calls = %d, want 0", sleeps)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("error = %v, must not wrap ErrRetryExhausted", err)
	}
}

func TestWithTxUsesPolicySleep(t *testing.T) {
	const (
		base = 10 * time.Millisecond
		max  = 40 * time.Millisecond
	)
	var delays []time.Duration
	var attempts int
	b := &fakeBeginner{}
	err := WithTxPolicy(context.Background(), b, pgx.TxOptions{}, RetryPolicy{
		MaxAttempts: 5,
		BaseDelay:   base,
		MaxDelay:    max,
		Sleep: func(_ context.Context, d time.Duration) error {
			delays = append(delays, d)
			return nil
		},
	}, func(context.Context, pgx.Tx) error {
		attempts++
		return serializationFailure()
	})

	if attempts != 5 {
		t.Fatalf("callback attempts = %d, want 5", attempts)
	}
	if len(delays) != 4 {
		t.Fatalf("sleep calls = %d, want 4", len(delays))
	}
	for i, d := range delays {
		upper := base << i
		if upper > max {
			upper = max
		}
		if d < 0 || d >= upper {
			t.Fatalf("sleep %d = %v, want [0, %v)", i, d, upper)
		}
	}
	if !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("error = %v, want errors.Is(ErrRetryExhausted)", err)
	}
}
