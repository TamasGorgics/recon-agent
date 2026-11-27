package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/worker"
)

func quietWorkerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeLoop models either the poll loop or the integrity scheduler.
type fakeLoop struct {
	mu        sync.Mutex
	started   bool
	done      bool
	err       error
	block     bool
	startedCh chan struct{}
}

func newFakeLoop(err error, block bool) *fakeLoop {
	return &fakeLoop{err: err, block: block, startedCh: make(chan struct{})}
}

func (f *fakeLoop) Run(ctx context.Context) error {
	f.mu.Lock()
	f.started = true
	close(f.startedCh)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.done = true
		f.mu.Unlock()
	}()
	if f.err != nil {
		return f.err
	}
	if !f.block {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeLoop) state() (started, done bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.done
}

type fakeOnce struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeOnce) ProcessOnce(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakeOnce) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestRunWorkerModeOnceStartsNoScheduler(t *testing.T) {
	poll := newFakeLoop(nil, true)
	scheduler := newFakeLoop(nil, true)
	once := &fakeOnce{}

	if err := runWorkerMode(context.Background(), true, poll, once, scheduler, quietWorkerLogger()); err != nil {
		t.Fatalf("runWorkerMode once: %v", err)
	}
	if once.count() != 1 {
		t.Fatalf("ProcessOnce calls = %d, want 1", once.count())
	}
	if started, _ := poll.state(); started {
		t.Fatal("--once started the long-running poll loop")
	}
	if started, _ := scheduler.state(); started {
		t.Fatal("--once started the integrity scheduler")
	}
}

func TestRunWorkerModeOnceKeepsCleanExitSentinels(t *testing.T) {
	for _, err := range []error{nil, cases.ErrNoCase, worker.ErrAgentDisabled, worker.ErrDailyBudgetReached} {
		job := &fakeOnce{err: err}
		if got := runWorkerMode(context.Background(), true, newFakeLoop(nil, true), job, nil, quietWorkerLogger()); got != nil {
			t.Fatalf("runWorkerMode once with %v = %v, want clean exit", err, got)
		}
	}
}

func TestRunWorkerModeLongRunningRequiresScheduler(t *testing.T) {
	err := runWorkerMode(context.Background(), false, newFakeLoop(nil, true), &fakeOnce{}, nil, quietWorkerLogger())
	if err == nil {
		t.Fatal("long-running mode accepted a nil scheduler")
	}
}

func TestRunJoinedWaitsForSchedulerAfterPollReturns(t *testing.T) {
	poll := newFakeLoop(nil, false) // returns immediately
	scheduler := newFakeLoop(nil, true)

	if err := runJoined(context.Background(), poll, scheduler, quietWorkerLogger()); err != nil {
		t.Fatalf("runJoined: %v", err)
	}
	if _, done := scheduler.state(); !done {
		t.Fatal("runJoined returned before the scheduler exited")
	}
}

func TestRunJoinedPropagatesPollError(t *testing.T) {
	pollErr := errors.New("poll failed")
	poll := newFakeLoop(pollErr, false)
	scheduler := newFakeLoop(nil, true)

	err := runJoined(context.Background(), poll, scheduler, quietWorkerLogger())
	if !errors.Is(err, pollErr) {
		t.Fatalf("runJoined error = %v, want poll error", err)
	}
	if _, done := scheduler.state(); !done {
		t.Fatal("runJoined returned before the scheduler exited after a poll error")
	}
}

func TestRunJoinedPropagatesSchedulerError(t *testing.T) {
	schedulerErr := errors.New("scheduler failed")
	poll := newFakeLoop(nil, true)
	scheduler := newFakeLoop(schedulerErr, false)

	err := runJoined(context.Background(), poll, scheduler, quietWorkerLogger())
	if !errors.Is(err, schedulerErr) {
		t.Fatalf("runJoined error = %v, want scheduler error", err)
	}
	if _, done := poll.state(); !done {
		t.Fatal("runJoined returned before the poll loop exited after a scheduler error")
	}
}

func TestRunJoinedExternalCancellationIsClean(t *testing.T) {
	poll := newFakeLoop(nil, true)
	scheduler := newFakeLoop(nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runJoined(ctx, poll, scheduler, quietWorkerLogger()) }()

	select {
	case <-poll.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("poll loop did not start")
	}
	select {
	case <-scheduler.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not start")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runJoined after cancellation = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runJoined did not return after cancellation")
	}
	if _, done := poll.state(); !done {
		t.Fatal("poll loop outlived runJoined")
	}
	if _, done := scheduler.state(); !done {
		t.Fatal("scheduler outlived runJoined")
	}
}
