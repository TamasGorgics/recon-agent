//go:build integration

// covers: FIN-20, FIN-21, FIN-23

package executor_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/executor"
)

// serviceWithSleep builds an executor service sharing this world's dependencies
// with an injected poll wait.
func (w *world) serviceWithSleep(t *testing.T, sleep func(context.Context, time.Duration) error) *executor.Service {
	t.Helper()
	opts := w.options()
	opts.Sleep = sleep
	svc, err := executor.NewService(opts)
	if err != nil {
		t.Fatalf("new executor service with sleep: %v", err)
	}
	return svc
}

// TestRunDrainsApprovedQueue applies every queued proposal, then waits on the
// now-empty queue; the injected sleep cancels the loop.
func TestRunDrainsApprovedQueue(t *testing.T) {
	w := newWorld(t)
	fixtures := make([]*caseFixture, 0, 3)
	for i := 0; i < 3; i++ {
		fixtures = append(fixtures, w.newCase(t, caseSpec{}))
	}
	var sleeps atomic.Int64
	svc := w.serviceWithSleep(t, func(context.Context, time.Duration) error {
		sleeps.Add(1)
		return context.Canceled
	})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sleeps.Load() == 0 {
		t.Error("Run never waited on the empty queue")
	}
	for i, fixture := range fixtures {
		if got := w.oneString(t,
			`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "applied" {
			t.Errorf("proposal %d status = %s, want applied", i, got)
		}
	}
}

// TestRunEmptyQueue waits and exits without attempting an apply.
func TestRunEmptyQueue(t *testing.T) {
	w := newWorld(t)
	var sleeps atomic.Int64
	svc := w.serviceWithSleep(t, func(context.Context, time.Duration) error {
		sleeps.Add(1)
		return context.Canceled
	})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sleeps.Load(); got != 1 {
		t.Errorf("sleep calls = %d, want 1", got)
	}
}

// TestRunContextCancel returns nil for an already-cancelled context and for a
// context cancelled inside the poll wait.
func TestRunContextCancel(t *testing.T) {
	w := newWorld(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	svc := w.serviceWithSleep(t, func(context.Context, time.Duration) error {
		t.Fatal("sleep called for an already-cancelled context")
		return nil
	})
	if err := svc.Run(cancelled); err != nil {
		t.Fatalf("Run(cancelled): %v", err)
	}

	ctx, cancelDuringSleep := context.WithCancel(context.Background())
	svc = w.serviceWithSleep(t, func(sleepCtx context.Context, _ time.Duration) error {
		cancelDuringSleep()
		<-sleepCtx.Done()
		return sleepCtx.Err()
	})
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run(cancel during sleep): %v", err)
	}
}

// TestRunContinuesAfterBadProposal proves one deterministic apply failure does
// not stop the loop: the bad proposal is recorded apply_failed and the next
// approved proposal is applied.
func TestRunContinuesAfterBadProposal(t *testing.T) {
	w := newWorld(t)
	bad := w.newCase(t, caseSpec{})
	w.setPayload(t, bad.proposalID, map[string]any{"lines": []any{}})
	good := w.newCase(t, caseSpec{})

	svc := w.serviceWithSleep(t, func(context.Context, time.Duration) error {
		return context.Canceled
	})
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, bad.proposalID); got != "apply_failed" {
		t.Errorf("bad proposal status = %s, want apply_failed", got)
	}
	if got := w.oneString(t,
		`SELECT after->>'reason' FROM audit_log
		  WHERE action = 'proposal.transition' AND entity_id = $1
		    AND after->>'reason' = $2`, bad.proposalID, executor.FailureStateChanged); got != executor.FailureStateChanged {
		t.Errorf("bad proposal failure reason = %s, want %s", got, executor.FailureStateChanged)
	}
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, good.proposalID); got != "applied" {
		t.Errorf("good proposal status = %s, want applied", got)
	}
}

// TestRunDrainsExpiredApprovedQueue proves the loop also polls approved rows
// whose expires_at has passed: Apply owns the expiry decision, records
// apply_failed(expired) with no apply side effects, and the loop continues to
// the next approved proposal and then waits on the empty queue.
func TestRunDrainsExpiredApprovedQueue(t *testing.T) {
	w := newWorld(t)
	expiredAt := mustTime(t, testNow).Add(-time.Hour)
	expired := w.newCase(t, caseSpec{expiresAt: &expiredAt})
	good := w.newCase(t, caseSpec{})

	var sleeps atomic.Int64
	svc := w.serviceWithSleep(t, func(context.Context, time.Duration) error {
		sleeps.Add(1)
		return context.Canceled
	})
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, expired.proposalID); got != "apply_failed" {
		t.Errorf("expired proposal status = %s, want apply_failed", got)
	}
	if got := w.oneString(t,
		`SELECT after->>'reason' FROM audit_log
		  WHERE action = 'proposal.transition' AND entity_id = $1
		    AND after->>'reason' = $2`, expired.proposalID, executor.FailureExpired); got != executor.FailureExpired {
		t.Errorf("expired proposal failure reason = %s, want %s", got, executor.FailureExpired)
	}
	w.assertNoApplySideEffects(t, expired)
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, good.proposalID); got != "applied" {
		t.Errorf("good proposal status = %s, want applied (loop did not continue)", got)
	}
	if sleeps.Load() == 0 {
		t.Error("Run never waited after draining the expired and good proposals")
	}
}
