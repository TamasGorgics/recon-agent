//go:build integration

// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34
package db_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// wave1Entry returns a distinct valid audit entry for the seq-race tests.
func wave1Entry(t *testing.T, n int) audit.Entry {
	t.Helper()
	actor, err := audit.SystemActor("m4-13-wave1")
	if err != nil {
		t.Fatalf("construct system actor: %v", err)
	}
	return audit.Entry{
		Actor:      actor,
		Action:     fmt.Sprintf("m4_13.wave1.%02d", n),
		EntityType: "m4_13_test",
		EntityID:   fmt.Sprintf("entity-%02d", n),
		After:      map[string]any{"n": int64(n)},
		Timestamp:  time.Date(2026, 11, 19, 12, 0, n, 0, time.UTC),
	}
}

func appendWave1(t *testing.T, ctx context.Context, b db.Beginner, entry audit.Entry) audit.Record {
	t.Helper()
	var record audit.Record
	err := db.WithTx(ctx, b, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		record, err = audit.Append(ctx, tx, entry)
		return err
	})
	if err != nil {
		t.Fatalf("seed audit entry: %v", err)
	}
	return record
}

// TestAuditAppendSeqRaceRetries reproduces the Q-95(13) seq race deterministically:
//
//  1. a SERIALIZABLE appender transaction reads the chain head, fixing its snapshot;
//  2. a READ COMMITTED appender commits the next chain row (barrier channels, no
//     sleeps);
//  3. the SERIALIZABLE audit.Append reads the stale head from its snapshot and
//     inserts the already-taken seq, hitting audit_log_seq_key (23505);
//  4. WithTxPolicy retries within the bound, recomputes seq from a fresh
//     snapshot, and commits the next row.
//
// Red before the Wave 1 fix: WithTxPolicy treats 23505 as terminal, so the call
// returns the unique violation. Green after: the call succeeds on attempt 2.
//
// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34
func TestAuditAppendSeqRaceRetries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d := testdb.New(t)
	seed := appendWave1(t, ctx, d.Pool, wave1Entry(t, 1))
	if seed.Seq != 1 {
		t.Fatalf("seed seq = %d, want 1", seed.Seq)
	}
	concurrentEntry := wave1Entry(t, 2)
	retriedEntry := wave1Entry(t, 3)

	snapshotFixed := make(chan struct{})
	type appendResult struct {
		record audit.Record
		err    error
	}
	appended := make(chan appendResult, 1)

	go func() {
		select {
		case <-snapshotFixed:
		case <-ctx.Done():
			appended <- appendResult{err: ctx.Err()}
			return
		}

		var result appendResult
		result.err = db.WithTx(ctx, d.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result.record, err = audit.Append(ctx, tx, concurrentEntry)
			return err
		})
		appended <- result
	}()

	var (
		attempts    atomic.Int32
		conflictErr error
		committed   audit.Record
	)

	err := db.WithTxPolicy(ctx, d.Pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, noSleepPolicy(),
		func(ctx context.Context, tx pgx.Tx) error {
			attempt := attempts.Add(1)
			if attempt == 1 {
				// First statement of the SERIALIZABLE transaction fixes its
				// snapshot at the pre-race head.
				var staleSeq int64
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(MAX(seq), 0) FROM audit_chain_head`).Scan(&staleSeq); err != nil {
					return fmt.Errorf("fix serializable snapshot: %w", err)
				}
				if staleSeq != seed.Seq {
					return fmt.Errorf("snapshot head = %d, want %d", staleSeq, seed.Seq)
				}
				close(snapshotFixed)

				select {
				case result := <-appended:
					if result.err != nil {
						return fmt.Errorf("concurrent appender: %w", result.err)
					}
					if result.record.Seq != staleSeq+1 {
						return fmt.Errorf("concurrent append seq = %d, want %d", result.record.Seq, staleSeq+1)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			record, err := audit.Append(ctx, tx, retriedEntry)
			if err != nil {
				if attempt == 1 {
					conflictErr = err
				}
				return err
			}
			committed = record
			return nil
		})

	if err != nil {
		t.Fatalf("WithTxPolicy() error = %v, want nil after seq-race retry", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("callback attempts = %d, want >= 2 (the race must have forced a retry)", got)
	}
	if got := attempts.Load(); got > int32(db.DefaultMaxAttempts) {
		t.Fatalf("callback attempts = %d, want <= %d", got, db.DefaultMaxAttempts)
	}

	if conflictErr == nil {
		t.Fatal("first attempt did not observe the seq-race conflict")
	}
	var conflict *pgconn.PgError
	if !errors.As(conflictErr, &conflict) {
		t.Fatalf("first-attempt error %v is not a *pgconn.PgError", conflictErr)
	}
	if conflict.Code != "23505" || conflict.ConstraintName != "audit_log_seq_key" {
		t.Fatalf("first-attempt error = SQLSTATE %s constraint %q, want 23505/audit_log_seq_key",
			conflict.Code, conflict.ConstraintName)
	}

	if committed.Seq != seed.Seq+2 {
		t.Fatalf("retried append seq = %d, want %d (previous chain row was %d)",
			committed.Seq, seed.Seq+2, seed.Seq+1)
	}

	report, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil {
		t.Fatalf("Verify after seq-race retry: %v", err)
	}
	if !report.OK() {
		t.Fatalf("Verify reported problems after seq-race retry: %+v", report.Problems)
	}
	if report.Checked != 3 || report.HeadSeq != 3 {
		t.Fatalf("Verify checked/head = %d/%d, want 3/3", report.Checked, report.HeadSeq)
	}
}

// TestWithTxPolicyDoesNotRetryUnrelatedUniqueViolation proves the narrow
// classification against a real Postgres unique constraint that is not the
// audit-chain seq key: the callback runs exactly once and the original
// *pgconn.PgError is returned unwrapped by ErrRetryExhausted.
//
// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34
func TestWithTxPolicyDoesNotRetryUnrelatedUniqueViolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d := testdb.New(t)
	var (
		attempts atomic.Int32
		original error
	)

	err := db.WithTxPolicy(ctx, d.Pool, pgx.TxOptions{}, noSleepPolicy(),
		func(ctx context.Context, tx pgx.Tx) error {
			attempts.Add(1)
			if _, err := tx.Exec(ctx,
				`CREATE TEMP TABLE m4_13_unique_probe (id integer PRIMARY KEY)`); err != nil {
				return fmt.Errorf("create probe table: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO m4_13_unique_probe (id) VALUES (1)`); err != nil {
				return fmt.Errorf("seed probe row: %w", err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO m4_13_unique_probe (id) VALUES (1)`)
			original = err
			return err
		})

	if got := attempts.Load(); got != 1 {
		t.Fatalf("callback attempts = %d, want exactly 1 for an unrelated unique violation", got)
	}
	if err == nil {
		t.Fatal("WithTxPolicy() error = nil, want the unrelated unique violation")
	}
	if errors.Is(err, db.ErrRetryExhausted) {
		t.Fatalf("error = %v, must not wrap ErrRetryExhausted", err)
	}
	if !errors.Is(err, original) {
		t.Fatalf("error = %v, want the original unique violation %v", err, original)
	}

	originalPg, ok := original.(*pgconn.PgError)
	if !ok {
		t.Fatalf("original error %v is not a *pgconn.PgError", original)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a *pgconn.PgError", err)
	}
	if pgErr != originalPg {
		t.Fatalf("errors.As returned a different PgError than the original")
	}
	if pgErr.Code != "23505" || pgErr.ConstraintName != "m4_13_unique_probe_pkey" {
		t.Fatalf("error = SQLSTATE %s constraint %q, want 23505/m4_13_unique_probe_pkey",
			pgErr.Code, pgErr.ConstraintName)
	}
}

// TestWithTxPolicyExhaustsAuditSeqRetryWithOriginalError forces the real
// audit_log_seq_key conflict on every attempt and proves that, after the same
// five-attempt bound, the last 23505 is still reachable with errors.As rather
// than being replaced by an opaque retry-exhausted sentinel.
//
// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34
func TestWithTxPolicyExhaustsAuditSeqRetryWithOriginalError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d := testdb.New(t)
	seed := appendWave1(t, ctx, d.Pool, wave1Entry(t, 1))

	var (
		attempts    atomic.Int32
		attemptErrs []error
	)
	zeroHash := make([]byte, 32)

	err := db.WithTxPolicy(ctx, d.Pool, pgx.TxOptions{}, noSleepPolicy(),
		func(ctx context.Context, tx pgx.Tx) error {
			attempts.Add(1)
			_, err := tx.Exec(ctx,
				`INSERT INTO audit_log
				     (seq, ts, actor_type, actor_id, action, entity_type, entity_id,
				      before, after, trace_id, prev_hash, row_hash)
				 OVERRIDING SYSTEM VALUE
				 VALUES ($1, now(), 'system', 'm4-13-wave1', 'm4_13.wave1.exhaust',
				         'm4_13_test', 'entity-exhaust', NULL, NULL, NULL, $2, $3)`,
				seed.Seq, zeroHash, zeroHash)
			attemptErrs = append(attemptErrs, err)
			return err
		})

	if got := attempts.Load(); got != int32(db.DefaultMaxAttempts) {
		t.Fatalf("callback attempts = %d, want %d", got, db.DefaultMaxAttempts)
	}
	if !errors.Is(err, db.ErrRetryExhausted) {
		t.Fatalf("error = %v, want errors.Is(ErrRetryExhausted)", err)
	}
	if len(attemptErrs) != db.DefaultMaxAttempts {
		t.Fatalf("recorded attempt errors = %d, want %d", len(attemptErrs), db.DefaultMaxAttempts)
	}

	last := attemptErrs[len(attemptErrs)-1]
	if !errors.Is(err, last) {
		t.Fatalf("error = %v, must wrap the last attempt error %v", err, last)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a *pgconn.PgError", err)
	}
	lastPg, ok := last.(*pgconn.PgError)
	if !ok {
		t.Fatalf("last attempt error %v is not a *pgconn.PgError", last)
	}
	if pgErr != lastPg {
		t.Fatalf("errors.As returned a different PgError than the last attempt error")
	}
	if pgErr.Code != "23505" || pgErr.ConstraintName != "audit_log_seq_key" {
		t.Fatalf("last attempt error = SQLSTATE %s constraint %q, want 23505/audit_log_seq_key",
			pgErr.Code, pgErr.ConstraintName)
	}
}
