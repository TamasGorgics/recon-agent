//go:build integration

// covers: FIN-22

package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

// TestEnqueueInsertsInCallerTransaction covers a committed insert, the
// aggregate defaults, the database clock, and caller-owned atomicity.
func TestEnqueueInsertsInCallerTransaction(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "enqueue-insert"

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin app transaction: %v", err)
	}
	inserted, err := outbox.Enqueue(ctx, tx, testTopic, json.RawMessage(`{"proposal_id":"p1"}`), key)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !inserted {
		t.Fatal("Enqueue inserted = false, want true")
	}
	// The row is invisible outside the caller's transaction until commit.
	if got := w.countByKey(t, key); got != 0 {
		t.Fatalf("visible rows before commit = %d, want 0", got)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit app transaction: %v", err)
	}

	row := w.loadByKey(t, key)
	if row.AggregateType != "outbox" {
		t.Errorf("aggregate_type = %q, want outbox", row.AggregateType)
	}
	if row.AggregateID != key {
		t.Errorf("aggregate_id = %q, want %q", row.AggregateID, key)
	}
	if row.Topic != testTopic {
		t.Errorf("topic = %q, want %q", row.Topic, testTopic)
	}
	if row.IdempotencyKey != key {
		t.Errorf("idempotency_key = %q, want %q", row.IdempotencyKey, key)
	}
	requireJSONEqual(t, "payload", row.Payload, `{"proposal_id":"p1"}`)
	requireTimeEqual(t, "created_at vs next_attempt_at", row.CreatedAt, row.NextAttemptAt)
	if row.DeliveredAt != nil {
		t.Errorf("delivered_at = %v, want NULL", row.DeliveredAt)
	}
	if row.Attempts != 0 || row.Dead {
		t.Errorf("attempts/dead = %d/%t, want 0/false", row.Attempts, row.Dead)
	}
	// Enqueue must use the database clock, not the Go wall clock. The worker
	// clock is deliberately one minute ahead of the database.
	if delta := row.CreatedAt.Sub(w.baseTime); delta < -5*time.Second || delta > 5*time.Second {
		t.Errorf("created_at differs from database clock by %s, want within 5s", delta)
	}
	if delta := row.CreatedAt.Sub(w.clock.Now()); delta > -30*time.Second {
		t.Errorf("created_at used the worker clock: delta %s, want about -1m", delta)
	}
}

// TestEnqueueRollbackRemovesRow proves Enqueue never commits on its own.
func TestEnqueueRollbackRemovesRow(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "enqueue-rollback"

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin app transaction: %v", err)
	}
	inserted, err := outbox.Enqueue(ctx, tx, testTopic, json.RawMessage(`{"rollback":true}`), key)
	if err != nil || !inserted {
		t.Fatalf("Enqueue = %t, %v; want true, nil", inserted, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := w.countByKey(t, key); got != 0 {
		t.Fatalf("rows after rollback = %d, want 0", got)
	}
}

// TestEnqueueDuplicateIsNoOp covers duplicate keys both inside one transaction
// and across committed transactions; the first payload wins.
func TestEnqueueDuplicateIsNoOp(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "enqueue-duplicate"

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin app transaction: %v", err)
	}
	first, err := outbox.Enqueue(ctx, tx, testTopic, json.RawMessage(`{"version":1}`), key)
	if err != nil || !first {
		t.Fatalf("first Enqueue = %t, %v; want true, nil", first, err)
	}
	second, err := outbox.Enqueue(ctx, tx, testTopic, json.RawMessage(`{"version":2}`), key)
	if err != nil {
		t.Fatalf("duplicate Enqueue in same transaction: %v", err)
	}
	if second {
		t.Fatal("duplicate Enqueue in same transaction inserted = true, want false")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx, err = w.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second app transaction: %v", err)
	}
	third, err := outbox.Enqueue(ctx, tx, testTopic, json.RawMessage(`{"version":3}`), key)
	if err != nil {
		t.Fatalf("duplicate Enqueue across transactions: %v", err)
	}
	if third {
		t.Fatal("duplicate Enqueue across transactions inserted = true, want false")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit second: %v", err)
	}

	if got := w.countByKey(t, key); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
	row := w.loadByKey(t, key)
	requireJSONEqual(t, "payload", row.Payload, `{"version":1}`)
}

// TestEnqueueAggregateCarriesTraceability proves the aggregate variant writes
// the caller's aggregate fields with the same conflict semantics.
func TestEnqueueAggregateCarriesTraceability(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "proposal.applied:aggregate-test"

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin app transaction: %v", err)
	}
	inserted, err := outbox.EnqueueAggregate(ctx, tx, "proposal", "aggregate-test", "proposal.applied", json.RawMessage(`{"proposal_id":"aggregate-test"}`), key)
	if err != nil || !inserted {
		t.Fatalf("EnqueueAggregate = %t, %v; want true, nil", inserted, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	row := w.loadByKey(t, key)
	if row.AggregateType != "proposal" {
		t.Errorf("aggregate_type = %q, want proposal", row.AggregateType)
	}
	if row.AggregateID != "aggregate-test" {
		t.Errorf("aggregate_id = %q, want aggregate-test", row.AggregateID)
	}
	if row.Topic != "proposal.applied" {
		t.Errorf("topic = %q, want proposal.applied", row.Topic)
	}
}

// TestEnqueueExecutorRole proves the real recon_executor login can enqueue with
// its INSERT and column-level conflict-target SELECT, including the duplicate
// no-op, and still cannot read the row back.
func TestEnqueueExecutorRole(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	executor := openExecutorPool(t, w)
	const key = "executor-enqueue"

	tx, err := executor.Begin(ctx)
	if err != nil {
		t.Fatalf("begin executor transaction: %v", err)
	}
	inserted, err := outbox.Enqueue(ctx, tx, "proposal.applied", json.RawMessage(`{"proposal_id":"executor"}`), key)
	if err != nil || !inserted {
		t.Fatalf("executor Enqueue = %t, %v; want true, nil", inserted, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit executor transaction: %v", err)
	}

	tx, err = executor.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second executor transaction: %v", err)
	}
	duplicate, err := outbox.Enqueue(ctx, tx, "proposal.applied", json.RawMessage(`{"proposal_id":"executor"}`), key)
	if err != nil {
		t.Fatalf("executor duplicate Enqueue: %v", err)
	}
	if duplicate {
		t.Fatal("executor duplicate Enqueue inserted = true, want false")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit second executor transaction: %v", err)
	}

	row := w.loadByKey(t, key)
	requireJSONEqual(t, "payload", row.Payload, `{"proposal_id":"executor"}`)

	// The executor still has no table-level SELECT on outbox.
	if _, err := executor.Exec(ctx, `SELECT * FROM outbox`); err == nil {
		t.Fatal("executor SELECT * FROM outbox succeeded, want permission denied")
	} else {
		var pgErr interface{ SQLState() string }
		if !errors.As(err, &pgErr) || pgErr.SQLState() != "42501" {
			t.Fatalf("executor SELECT error = %v, want SQLSTATE 42501", err)
		}
	}
}
