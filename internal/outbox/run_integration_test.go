//go:build integration

// covers: FIN-22

package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

// TestRunDrainsWithoutSleepingBetweenBatches proves Run keeps calling Process
// while batches are non-empty, sleeps once on empty, and returns nil after
// cancellation.
func TestRunDrainsWithoutSleepingBetweenBatches(t *testing.T) {
	w := newWorld(t)
	const rows = 5
	for i := 0; i < rows; i++ {
		w.insert(t, insertSpec{key: fmt.Sprintf("run-drain-%d", i)})
	}

	channel := outbox.NewFakeChannel()
	var (
		sleepMu    sync.Mutex
		sleepCalls int
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: channel}, func(opts *outbox.WorkerOptions) {
		opts.BatchSize = 2
		opts.Sleep = func(context.Context, time.Duration) error {
			sleepMu.Lock()
			sleepCalls++
			sleepMu.Unlock()
			cancel()
			return nil
		}
	})

	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}

	if got := len(channel.Messages()); got != rows {
		t.Fatalf("delivered messages = %d, want %d (all batches drained before sleep)", got, rows)
	}
	sleepMu.Lock()
	gotSleeps := sleepCalls
	sleepMu.Unlock()
	if gotSleeps != 1 {
		t.Fatalf("Sleep calls = %d, want exactly 1 empty-poll wait", gotSleeps)
	}
}

// TestRunEmptyQueueSleepsAndCancels proves an empty queue waits PollInterval
// through the injected Sleep and returns nil when that wait is canceled.
func TestRunEmptyQueueSleepsAndCancels(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sleepCalls int
	worker := w.newWorker(t, nil, func(opts *outbox.WorkerOptions) {
		opts.PollInterval = time.Hour
		opts.Sleep = func(context.Context, time.Duration) error {
			sleepCalls++
			cancel()
			return context.Canceled
		}
	})

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil after cancellation", err)
	}
	if sleepCalls != 1 {
		t.Fatalf("Sleep calls = %d, want 1", sleepCalls)
	}
}

// TestRunCanceledBeforeStart proves a pre-canceled context returns nil without
// polling or sleeping.
func TestRunCanceledBeforeStart(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	slept := false
	worker := w.newWorker(t, nil, func(opts *outbox.WorkerOptions) {
		opts.Sleep = func(context.Context, time.Duration) error {
			slept = true
			return nil
		}
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if slept {
		t.Fatal("Run slept with a pre-canceled context")
	}
}

// TestRunContinuesAfterHandlerFailure proves a failed delivery does not stop
// the loop and the row is retried after its backoff.
func TestRunContinuesAfterHandlerFailure(t *testing.T) {
	w := newWorld(t)
	first := w.insert(t, insertSpec{key: "run-failure-1", dueAt: w.clock.Now().Add(-2 * time.Second)})
	second := w.insert(t, insertSpec{key: "run-failure-2", dueAt: w.clock.Now().Add(-time.Second)})

	var (
		mu     sync.Mutex
		seen   []string
		failed = errors.New("transient")
	)
	handler := outbox.HandlerFunc(func(_ context.Context, msg outbox.Message) error {
		mu.Lock()
		seen = append(seen, msg.ID)
		attempts := len(seen)
		mu.Unlock()
		if attempts == 1 {
			return failed
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sleepCalls int
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.BatchSize = 1
		opts.BaseDelay = time.Second
		opts.MaxAttempts = 3
		opts.Sleep = func(context.Context, time.Duration) error {
			sleepCalls++
			if sleepCalls > 1 {
				cancel()
				return context.Canceled
			}
			// Retry the failed first row once, then let Run wait.
			w.clock.Advance(time.Second)
			return nil
		}
	})

	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish")
	}

	if got := len(seen); got != 3 {
		t.Fatalf("handler calls = %d, want 3 (retry first, deliver second, then empty)", got)
	}
	if row := w.load(t, first); row.DeliveredAt == nil || row.Attempts != 1 {
		t.Fatalf("first row delivered/attempts = %t/%d, want true/1", row.DeliveredAt != nil, row.Attempts)
	}
	if row := w.load(t, second); row.DeliveredAt == nil {
		t.Fatal("second row delivered_at = NULL, want set")
	}
}
