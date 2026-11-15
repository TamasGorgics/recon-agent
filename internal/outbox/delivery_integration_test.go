//go:build integration

// covers: FIN-22

package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

// TestDeliverySuccess proves a nil handler result marks the row delivered with
// the worker clock, records the Delivered metric once, and leaves attempts at
// zero.
func TestDeliverySuccess(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "delivery-success"
	id := w.insert(t, insertSpec{key: key, payload: `{"proposal_id":"p1"}`})

	metrics := newMetricsRecorder()
	var (
		mu       sync.Mutex
		received []outbox.Message
	)
	handler := outbox.HandlerFunc(func(_ context.Context, msg outbox.Message) error {
		mu.Lock()
		received = append(received, msg)
		mu.Unlock()
		return nil
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.Metrics = metrics
	})

	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	if len(received) != 1 {
		t.Fatalf("handler calls = %d, want 1", len(received))
	}
	msg := received[0]
	if msg.ID != id || msg.Topic != testTopic || msg.IdempotencyKey != key || msg.Attempts != 0 {
		t.Fatalf("handler message = %+v, want id/topic/key/attempts %s/%s/%s/0", msg, id, testTopic, key)
	}
	requireJSONEqual(t, "handler payload", msg.Payload, `{"proposal_id":"p1"}`)

	row := w.load(t, id)
	if row.DeliveredAt == nil {
		t.Fatal("delivered_at = NULL, want set")
	}
	requireTimeEqual(t, "delivered_at", *row.DeliveredAt, w.clock.Now())
	if row.Attempts != 0 || row.Dead {
		t.Fatalf("attempts/dead = %d/%t, want 0/false", row.Attempts, row.Dead)
	}
	if got := metrics.deliveredCount(testTopic); got != 1 {
		t.Fatalf("Delivered metric = %d, want 1", got)
	}
	if got := metrics.failedCount(testTopic); got != 0 {
		t.Fatalf("Failed metric = %d, want 0", got)
	}
	if got := metrics.deadAttempts(testTopic); len(got) != 0 {
		t.Fatalf("DeadLettered metric = %v, want none", got)
	}

	// A delivered row is never claimed again.
	w.clock.Advance(time.Hour)
	claimed, err = worker.Process(ctx)
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("second claimed = %d, want 0", claimed)
	}
	if got := metrics.deliveredCount(testTopic); got != 1 {
		t.Fatalf("Delivered metric after second poll = %d, want 1", got)
	}
}

// TestDeliveryFailureBackoffAndDeadLetter covers exact exponential backoff, the
// attempt count, the dead-letter threshold, and one DeadLettered metric.
func TestDeliveryFailureBackoffAndDeadLetter(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "delivery-failure"
	id := w.insert(t, insertSpec{key: key})

	metrics := newMetricsRecorder()
	failure := errors.New("channel unavailable")
	handler := outbox.HandlerFunc(func(context.Context, outbox.Message) error {
		return failure
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.Metrics = metrics
		opts.MaxAttempts = 3
		opts.BaseDelay = 2 * time.Second
		opts.MaxDelay = 3 * time.Second
	})

	start := w.clock.Now()
	steps := []struct {
		now         time.Time
		wantAttempt int
		wantNext    time.Time
		wantDead    bool
	}{
		{now: start, wantAttempt: 1, wantNext: start.Add(2 * time.Second), wantDead: false},
		{now: start.Add(2 * time.Second), wantAttempt: 2, wantNext: start.Add(5 * time.Second), wantDead: false},
		{now: start.Add(5 * time.Second), wantAttempt: 3, wantNext: start.Add(8 * time.Second), wantDead: true},
	}
	for i, step := range steps {
		w.clock.Set(step.now)
		claimed, err := worker.Process(ctx)
		if err != nil {
			t.Fatalf("Process step %d: %v", i+1, err)
		}
		if claimed != 1 {
			t.Fatalf("step %d claimed = %d, want 1", i+1, claimed)
		}
		row := w.load(t, id)
		if row.Attempts != step.wantAttempt {
			t.Fatalf("step %d attempts = %d, want %d", i+1, row.Attempts, step.wantAttempt)
		}
		requireTimeEqual(t, fmt.Sprintf("step %d next_attempt_at", i+1), row.NextAttemptAt, step.wantNext)
		if row.Dead != step.wantDead {
			t.Fatalf("step %d dead = %t, want %t", i+1, row.Dead, step.wantDead)
		}
		if row.DeliveredAt != nil {
			t.Fatalf("step %d delivered_at set, want NULL", i+1)
		}
		if got := metrics.failedCount(testTopic); got != step.wantAttempt {
			t.Fatalf("step %d Failed metric = %d, want %d", i+1, got, step.wantAttempt)
		}
	}

	if got := metrics.deliveredCount(testTopic); got != 0 {
		t.Fatalf("Delivered metric = %d, want 0", got)
	}
	if got := metrics.deadAttempts(testTopic); len(got) != 1 || got[0] != 3 {
		t.Fatalf("DeadLettered attempts = %v, want [3]", got)
	}

	// A dead row is never claimed again, even long after its backoff.
	w.clock.Advance(24 * time.Hour)
	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process after dead-letter: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("claimed after dead-letter = %d, want 0", claimed)
	}
	if got := metrics.deadAttempts(testTopic); len(got) != 1 {
		t.Fatalf("DeadLettered metric after extra poll = %v, want one call", got)
	}
}

// TestDeliveryStaleAttemptsUsesAuthoritativeDeadState simulates a lease-expiry
// redelivery that advances the stored row to MaxAttempts-1 after this worker's
// claim but before its failure mark. The claimed Message.Attempts is stale
// (zero), so the dead decision and DeadLettered metric must come from the row
// the UPDATE returns, not from the message.
func TestDeliveryStaleAttemptsUsesAuthoritativeDeadState(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "delivery-stale-attempts"
	id := w.insert(t, insertSpec{key: key, attempts: 0})

	metrics := newMetricsRecorder()
	failure := errors.New("channel unavailable")
	handler := outbox.HandlerFunc(func(context.Context, outbox.Message) error {
		// A concurrent redelivery has already recorded four failures on the
		// stored row; this worker still holds the stale claim with attempts 0.
		if _, err := w.db.Pool.Exec(ctx,
			`UPDATE outbox
			    SET attempts = 4
			  WHERE id = $1::uuid
			    AND delivered_at IS NULL
			    AND dead = false`, id); err != nil {
			t.Errorf("advance stored attempts: %v", err)
		}
		return failure
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.Metrics = metrics
		opts.MaxAttempts = 5
	})

	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}

	row := w.load(t, id)
	if !row.Dead {
		t.Fatal("dead = false, want true from the authoritative stored attempts")
	}
	if row.Attempts != 5 {
		t.Fatalf("attempts = %d, want 5", row.Attempts)
	}
	if row.DeliveredAt != nil {
		t.Fatal("delivered_at set, want NULL")
	}
	if got := metrics.failedCount(testTopic); got != 1 {
		t.Fatalf("Failed metric = %d, want 1", got)
	}
	if got := metrics.deadAttempts(testTopic); len(got) != 1 || got[0] != 5 {
		t.Fatalf("DeadLettered attempts = %v, want [5] from the stored row", got)
	}
}

// TestDeliveryStaleAttemptsBackoffUsesAuthoritativeAttempts simulates the same
// lease-expiry race for a row that is not dead yet: the claimed Message.Attempts
// is stale, so next_attempt_at must be corrected in the mark transaction from
// the attempts the failure UPDATE returned.
func TestDeliveryStaleAttemptsBackoffUsesAuthoritativeAttempts(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "delivery-stale-attempts-backoff"
	id := w.insert(t, insertSpec{key: key, attempts: 0})

	metrics := newMetricsRecorder()
	failure := errors.New("channel unavailable")
	handler := outbox.HandlerFunc(func(context.Context, outbox.Message) error {
		// MaxAttempts is 5: a concurrent redelivery advanced the stored row to
		// 3 (MaxAttempts-2), so this stale claim records attempt 4, not 1.
		if _, err := w.db.Pool.Exec(ctx,
			`UPDATE outbox
			    SET attempts = 3
			  WHERE id = $1::uuid
			    AND delivered_at IS NULL
			    AND dead = false`, id); err != nil {
			t.Errorf("advance stored attempts: %v", err)
		}
		return failure
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.Metrics = metrics
		opts.MaxAttempts = 5
		opts.BaseDelay = time.Second
		opts.MaxDelay = time.Minute
	})

	start := w.clock.Now()
	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}

	row := w.load(t, id)
	if row.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4 from the authoritative stored row", row.Attempts)
	}
	if row.Dead {
		t.Fatal("dead = true, want false below MaxAttempts")
	}
	// backoff(4) = 1s * 2^3 = 8s, not the stale backoff(1) = 1s.
	requireTimeEqual(t, "next_attempt_at", row.NextAttemptAt, start.Add(8*time.Second))
	if row.DeliveredAt != nil {
		t.Fatal("delivered_at set, want NULL")
	}
	if got := metrics.failedCount(testTopic); got != 1 {
		t.Fatalf("Failed metric = %d, want 1", got)
	}
	if got := metrics.deadAttempts(testTopic); len(got) != 0 {
		t.Fatalf("DeadLettered = %v, want none", got)
	}
}

// TestDeliveryUnknownTopicDeadLetters proves an unregistered topic is removed
// immediately without running a handler or incrementing attempts.
func TestDeliveryUnknownTopicDeadLetters(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	id := w.insert(t, insertSpec{key: "unknown-topic", topic: "nobody.handles.this"})

	metrics := newMetricsRecorder()
	var logs logBuffer
	registeredCalled := false
	registered := outbox.HandlerFunc(func(context.Context, outbox.Message) error {
		registeredCalled = true
		return nil
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: registered}, func(opts *outbox.WorkerOptions) {
		opts.Metrics = metrics
		opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})

	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	if registeredCalled {
		t.Fatal("registered handler ran for an unknown topic")
	}
	row := w.load(t, id)
	if !row.Dead {
		t.Fatal("dead = false, want true")
	}
	if row.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 (no handler ran)", row.Attempts)
	}
	if row.DeliveredAt != nil {
		t.Fatal("delivered_at set, want NULL")
	}
	if got := metrics.deadAttempts("nobody.handles.this"); len(got) != 1 || got[0] != 0 {
		t.Fatalf("DeadLettered = %v, want [0]", got)
	}
	if got := metrics.failedCount("nobody.handles.this"); got != 0 {
		t.Fatalf("Failed metric = %d, want 0", got)
	}
	if !strings.Contains(logs.String(), "unknown topic") {
		t.Fatalf("logs = %q, want unknown-topic diagnostic", logs.String())
	}

	w.clock.Advance(time.Hour)
	claimed, err = worker.Process(ctx)
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("claimed after dead-letter = %d, want 0", claimed)
	}
	if got := metrics.deadAttempts("nobody.handles.this"); len(got) != 1 {
		t.Fatalf("DeadLettered after second poll = %v, want one call", got)
	}
}

// TestDeliveryPanicIsFailure proves a panicking handler is recovered, counted
// as one attempt, and does not stop Process.
func TestDeliveryPanicIsFailure(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	id := w.insert(t, insertSpec{key: "delivery-panic"})

	metrics := newMetricsRecorder()
	handler := outbox.HandlerFunc(func(context.Context, outbox.Message) error {
		panic("boom")
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.Metrics = metrics
	})

	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process after panic: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	row := w.load(t, id)
	if row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", row.Attempts)
	}
	if row.Dead {
		t.Fatal("dead = true, want false below MaxAttempts")
	}
	if got := metrics.failedCount(testTopic); got != 1 {
		t.Fatalf("Failed metric = %d, want 1", got)
	}
}

// TestDeliveryHandlerTimeoutIsFailure proves a handler that outlives
// HandlerTimeout is canceled and recorded as a failed attempt.
func TestDeliveryHandlerTimeoutIsFailure(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	id := w.insert(t, insertSpec{key: "delivery-timeout"})

	metrics := newMetricsRecorder()
	handler := outbox.HandlerFunc(func(ctx context.Context, _ outbox.Message) error {
		<-ctx.Done()
		return ctx.Err()
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.Metrics = metrics
		opts.HandlerTimeout = 25 * time.Millisecond
	})

	started := time.Now()
	claimed, err := worker.Process(ctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("handler timeout took %s, want about 25ms", elapsed)
	}
	row := w.load(t, id)
	if row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", row.Attempts)
	}
	if got := metrics.failedCount(testTopic); got != 1 {
		t.Fatalf("Failed metric = %d, want 1", got)
	}
}

// TestCrashAfterSendRedeliversAndDeduplicates proves a handler that records a
// side effect and then fails is redelivered after the lease/backoff and the
// idempotent channel keeps one unique side effect.
func TestCrashAfterSendRedeliversAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const key = "crash-after-send"
	id := w.insert(t, insertSpec{key: key})

	channel := outbox.NewFakeChannel()
	channel.SetFailAfterSend(true)
	metrics := newMetricsRecorder()
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: channel}, func(opts *outbox.WorkerOptions) {
		opts.Metrics = metrics
		opts.Lease = 30 * time.Second
		opts.BaseDelay = time.Second
		opts.MaxDelay = time.Minute
	})

	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("first Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("first claimed = %d, want 1", claimed)
	}
	if got := channel.Delivered(key); got != 1 {
		t.Fatalf("side effects after failed send = %d, want 1", got)
	}
	row := w.load(t, id)
	if row.DeliveredAt != nil {
		t.Fatal("delivered_at set after failed send, want NULL")
	}
	if row.Attempts != 1 || row.Dead {
		t.Fatalf("attempts/dead after failed send = %d/%t, want 1/false", row.Attempts, row.Dead)
	}
	if got := metrics.failedCount(testTopic); got != 1 {
		t.Fatalf("Failed metric = %d, want 1", got)
	}

	// Move past both the lease and the first backoff.
	w.clock.Advance(31 * time.Second)
	claimed, err = worker.Process(ctx)
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("second claimed = %d, want 1", claimed)
	}
	if got := channel.Delivered(key); got != 1 {
		t.Fatalf("unique side effects = %d, want 1", got)
	}
	if got := len(channel.Messages()); got != 1 {
		t.Fatalf("recorded messages = %d, want 1", got)
	}
	row = w.load(t, id)
	if row.DeliveredAt == nil {
		t.Fatal("delivered_at = NULL after redelivery, want set")
	}
	if row.Attempts != 1 || row.Dead {
		t.Fatalf("attempts/dead = %d/%t, want 1/false", row.Attempts, row.Dead)
	}
	if got := metrics.deliveredCount(testTopic); got != 1 {
		t.Fatalf("Delivered metric = %d, want 1", got)
	}
	if got := metrics.deadAttempts(testTopic); len(got) != 0 {
		t.Fatalf("DeadLettered = %v, want none", got)
	}
}
