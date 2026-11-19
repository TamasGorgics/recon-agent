// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34
package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// recordingBeginner returns a fresh recordingTx per BeginTx call and keeps the
// transactions in call order so retry tests can assert rollback-before-retry.
type recordingBeginner struct {
	txs []*recordingTx
}

func (b *recordingBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.BeginTx(ctx, pgx.TxOptions{})
}

func (b *recordingBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	tx := &recordingTx{}
	b.txs = append(b.txs, tx)
	return tx, nil
}

// recordingTx embeds pgx.Tx so only the lifecycle methods are implemented.
type recordingTx struct {
	pgx.Tx
	commits   int
	rollbacks int
}

func (tx *recordingTx) Commit(context.Context) error {
	tx.commits++
	return nil
}

func (tx *recordingTx) Rollback(context.Context) error {
	tx.rollbacks++
	return nil
}

func noSleepPolicy() db.RetryPolicy {
	return db.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}
}

func pgError(code, constraint string) *pgconn.PgError {
	return &pgconn.PgError{
		Code:           code,
		ConstraintName: constraint,
		Message:        "injected SQLSTATE " + code,
	}
}

// TestWithTxPolicyRetriesAuditSeqUniqueViolation is the retry-classification
// table: existing 40001/40P01 retries stay intact, a 23505 named
// audit_log_seq_key retries, and every other 23505 (including the empty
// constraint name) is terminal after exactly one attempt.
//
// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34
func TestWithTxPolicyRetriesAuditSeqUniqueViolation(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantAttempts int
		wantRetried  bool
	}{
		{name: "serialization failure", err: pgError("40001", ""), wantAttempts: 2, wantRetried: true},
		{name: "deadlock detected", err: pgError("40P01", ""), wantAttempts: 2, wantRetried: true},
		{name: "audit chain seq key", err: pgError("23505", "audit_log_seq_key"), wantAttempts: 2, wantRetried: true},
		{name: "unrelated unique constraint", err: pgError("23505", "some_other_key"), wantAttempts: 1},
		{name: "empty constraint name", err: pgError("23505", ""), wantAttempts: 1},
		{name: "non postgres error", err: errors.New("boom"), wantAttempts: 1},
		{name: "success on first attempt", err: nil, wantAttempts: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int
			b := &recordingBeginner{}
			cause := tc.err

			err := db.WithTxPolicy(context.Background(), b, pgx.TxOptions{}, noSleepPolicy(),
				func(context.Context, pgx.Tx) error {
					attempts++
					if attempts == 1 {
						return cause
					}
					return nil
				})

			if attempts != tc.wantAttempts {
				t.Fatalf("callback attempts = %d, want %d", attempts, tc.wantAttempts)
			}
			if len(b.txs) != tc.wantAttempts {
				t.Fatalf("BeginTx calls = %d, want %d", len(b.txs), tc.wantAttempts)
			}

			if !tc.wantRetried {
				if cause != nil {
					if !errors.Is(err, cause) {
						t.Fatalf("error = %v, want the original error %v", err, cause)
					}
					if errors.Is(err, db.ErrRetryExhausted) {
						t.Fatalf("error = %v, must not wrap ErrRetryExhausted", err)
					}
					if b.txs[0].rollbacks != 1 || b.txs[0].commits != 0 {
						t.Fatalf("failed attempt commits=%d rollbacks=%d, want 0/1",
							b.txs[0].commits, b.txs[0].rollbacks)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("WithTxPolicy() error = %v, want nil after retry", err)
			}
			if b.txs[0].commits != 0 || b.txs[0].rollbacks != 1 {
				t.Fatalf("first attempt commits=%d rollbacks=%d, want 0/1 (rollback before retry)",
					b.txs[0].commits, b.txs[0].rollbacks)
			}
			last := b.txs[len(b.txs)-1]
			if last.commits != 1 || last.rollbacks != 0 {
				t.Fatalf("final attempt commits=%d rollbacks=%d, want 1/0",
					last.commits, last.rollbacks)
			}
		})
	}
}
