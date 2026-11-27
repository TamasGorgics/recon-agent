// covers: FIN-52
package integrity_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/integrity"
)

func quietSchedulerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeLatestStore is the persisted max(run_date) reader. Tests set the value
// explicitly; the runner may update it to simulate a committed row.
type fakeLatestStore struct {
	mu     sync.Mutex
	latest *time.Time
	err    error
	hook   func(context.Context) (integrity.Date, bool, error)
}

func (s *fakeLatestStore) read(ctx context.Context) (integrity.Date, bool, error) {
	s.mu.Lock()
	hook := s.hook
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return s.value()
}

func (s *fakeLatestStore) value() (integrity.Date, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return integrity.Date{}, false, s.err
	}
	if s.latest == nil {
		return integrity.Date{}, false, nil
	}
	return integrity.NewDate(*s.latest), true, nil
}

func (s *fakeLatestStore) setFrom(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := t.UTC()
	s.latest = &value
}

// fakeSchedulerRunner records each scheduled attempt and can simulate a
// committed row by updating the fake store.
type fakeSchedulerRunner struct {
	mu    sync.Mutex
	clock *clock.Fake
	calls int
	at    []time.Time
	run   func(call int, now time.Time) (integrity.Result, error)
}

func (r *fakeSchedulerRunner) Run(context.Context) (integrity.Result, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	now := r.clock.Now().UTC()
	r.at = append(r.at, now)
	fn := r.run
	r.mu.Unlock()
	if fn != nil {
		return fn(call, now)
	}
	return integrity.Result{}, nil
}

func (r *fakeSchedulerRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// advancingSleeper advances the fake clock by the requested wait and cancels
// the scheduler after max sleeps, so Run always terminates deterministically.
type advancingSleeper struct {
	clock  *clock.Fake
	cancel context.CancelFunc
	max    int
	mu     sync.Mutex
	sleeps []time.Duration
}

func (s *advancingSleeper) Sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.sleeps = append(s.sleeps, d)
	n := len(s.sleeps)
	s.mu.Unlock()
	s.clock.Advance(d)
	if n >= s.max {
		s.cancel()
		return context.Canceled
	}
	return nil
}

func (s *advancingSleeper) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.sleeps...)
}

type schedulerHarness struct {
	clock   *clock.Fake
	store   *fakeLatestStore
	runner  *fakeSchedulerRunner
	sleeper *advancingSleeper
	run     func() error
}

// newSchedulerHarness builds a scheduler at start with at most maxSleeps
// deterministic waits. The scheduler is started by run.
func newSchedulerHarness(t *testing.T, start time.Time, maxSleeps int) *schedulerHarness {
	t.Helper()
	clk := clock.NewFake(start)
	h := &schedulerHarness{
		clock:  clk,
		store:  &fakeLatestStore{},
		runner: &fakeSchedulerRunner{clock: clk},
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.sleeper = &advancingSleeper{clock: clk, cancel: cancel, max: maxSleeps}
	scheduler, err := integrity.NewScheduler(integrity.SchedulerOptions{
		Runner:                 h.runner,
		LatestScheduledRunDate: h.store.read,
		Clock:                  clk,
		Sleeper:                h.sleeper,
		Logger:                 quietSchedulerLogger(),
	})
	if err != nil {
		cancel()
		t.Fatalf("NewScheduler: %v", err)
	}
	t.Cleanup(cancel)
	h.run = func() error { return scheduler.Run(ctx) }
	return h
}

func TestSchedulerWaitsFor0200ThenCatchesUp(t *testing.T) {
	start := time.Date(2026, 11, 26, 1, 59, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 2)
	h.runner.run = func(int, time.Time) (integrity.Result, error) {
		h.store.setFrom(h.clock.Now())
		return integrity.Result{ID: "run-1", Report: passedSchedulerReport()}, nil
	}

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 1 {
		t.Fatalf("runner calls = %d, want 1 catch-up run", got)
	}
	if got := h.runner.at[0]; !got.Equal(time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("first run at %s, want 02:00 UTC", got)
	}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != 2 {
		t.Fatalf("sleeps = %v, want 2", sleeps)
	}
	if sleeps[0] != time.Minute {
		t.Fatalf("first sleep = %s, want 1m to 02:00", sleeps[0])
	}
	if sleeps[1] != 24*time.Hour {
		t.Fatalf("second sleep = %s, want 24h from 02:00 to tomorrow", sleeps[1])
	}
}

func TestSchedulerCatchUpAfter0200RunsCurrentDate(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 1)
	h.runner.run = func(int, time.Time) (integrity.Result, error) {
		h.store.setFrom(h.clock.Now())
		return integrity.Result{ID: "run-1", Report: passedSchedulerReport()}, nil
	}

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 1 {
		t.Fatalf("runner calls = %d, want 1 catch-up run", got)
	}
	if got := h.runner.at[0]; !got.Equal(start) {
		t.Fatalf("catch-up ran at %s, want %s", got, start)
	}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != 1 || sleeps[0] != 23*time.Hour {
		t.Fatalf("sleeps = %v, want [23h]", sleeps)
	}
}

func TestSchedulerSameDayScheduledRowSuppressesRun(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 1)
	h.store.setFrom(start)

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 0 {
		t.Fatalf("runner calls = %d, want 0 when today already has a scheduled row", got)
	}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != 1 || sleeps[0] != 23*time.Hour {
		t.Fatalf("sleeps = %v, want [23h] to tomorrow 02:00", sleeps)
	}
}

func TestSchedulerFailedRowCountsAsTodaysAttempt(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 1)
	h.runner.run = func(int, time.Time) (integrity.Result, error) {
		// The run completed (row exists) but failed; it still counts.
		h.store.setFrom(h.clock.Now())
		return integrity.Result{ID: "run-failed", Report: failedSchedulerReport()}, nil
	}

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 1 {
		t.Fatalf("runner calls = %d, want exactly 1 failed attempt for the day", got)
	}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != 1 || sleeps[0] != 23*time.Hour {
		t.Fatalf("sleeps = %v, want [23h] after the failed row", sleeps)
	}
}

// An operational failure before a row is written must not burn the day's
// attempt: the next loop retries the runner after the backoff.
func TestSchedulerRetriesOperationalFailureThenSucceeds(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 2)
	runErr := errors.New("database unavailable")
	h.runner.run = func(call int, now time.Time) (integrity.Result, error) {
		if call == 1 {
			return integrity.Result{}, runErr
		}
		h.store.setFrom(now)
		return integrity.Result{ID: "run-retry", Report: passedSchedulerReport()}, nil
	}

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 2 {
		t.Fatalf("runner calls = %d, want failure then retry success", got)
	}
	wantSecond := start.Add(30 * time.Second)
	if got := h.runner.at[1]; !got.Equal(wantSecond) {
		t.Fatalf("retry at %s, want %s", got, wantSecond)
	}
	sleeps := h.sleeper.recorded()
	wantSleeps := []time.Duration{30 * time.Second, 22*time.Hour + 59*time.Minute + 30*time.Second}
	if len(sleeps) != len(wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", sleeps, wantSleeps)
	}
	for i := range wantSleeps {
		if sleeps[i] != wantSleeps[i] {
			t.Fatalf("sleeps = %v, want %v", sleeps, wantSleeps)
		}
	}
}

// A permanently failing runner is retried after every bounded backoff; the
// attempt is never marked done, so there is no hot loop and no stuck day.
func TestSchedulerOperationalFailureBackoffProgression(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 5)
	runErr := errors.New("database unavailable")
	h.runner.run = func(int, time.Time) (integrity.Result, error) {
		return integrity.Result{}, runErr
	}

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 5 {
		t.Fatalf("runner calls = %d, want one retry per backoff wait", got)
	}
	wantSleeps := []time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != len(wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", sleeps, wantSleeps)
	}
	for i := range wantSleeps {
		if sleeps[i] != wantSleeps[i] {
			t.Fatalf("sleeps = %v, want %v", sleeps, wantSleeps)
		}
	}
	wantCalls := []time.Duration{0, 30 * time.Second, 90 * time.Second, 390 * time.Second, 1290 * time.Second}
	for i, offset := range wantCalls {
		if want := start.Add(offset); !h.runner.at[i].Equal(want) {
			t.Fatalf("attempt %d at %s, want %s", i+1, h.runner.at[i], want)
		}
	}
}

// A completed run that persisted status=failed is still the day's scheduled
// attempt. Even before the row is visible to the next query, the in-memory
// guard backs off instead of calling the runner again.
func TestSchedulerCompletedFailedRunCountsAsAttempt(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 2)
	h.runner.run = func(int, time.Time) (integrity.Result, error) {
		return integrity.Result{ID: "run-failed", Report: failedSchedulerReport()}, nil
	}

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 1 {
		t.Fatalf("runner calls = %d, want no retry after a completed failed run", got)
	}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != 2 || sleeps[0] != 30*time.Second || sleeps[1] != time.Minute {
		t.Fatalf("sleeps = %v, want guarded 30s then 1m backoff", sleeps)
	}
}

// Cancellation during the operational backoff is shutdown, not a failure.
func TestSchedulerCancelDuringOperationalBackoffReturnsCleanly(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 1)
	runErr := errors.New("database unavailable")
	h.runner.run = func(int, time.Time) (integrity.Result, error) {
		return integrity.Result{}, runErr
	}

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run on cancel = %v, want nil", err)
	}
	if got := h.runner.count(); got != 1 {
		t.Fatalf("runner calls = %d, want 1 before cancellation", got)
	}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != 1 || sleeps[0] != 30*time.Second {
		t.Fatalf("sleeps = %v, want the 30s operational backoff canceled", sleeps)
	}
}

func TestSchedulerLockRetryDoesNotRecordAttempt(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 2)
	h.runner.run = func(call int, _ time.Time) (integrity.Result, error) {
		if call == 1 {
			return integrity.Result{}, integrity.ErrAlreadyRunning
		}
		h.store.setFrom(h.clock.Now())
		return integrity.Result{ID: "run-2", Report: passedSchedulerReport()}, nil
	}

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 2 {
		t.Fatalf("runner calls = %d, want lock retry then success", got)
	}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != 2 || sleeps[0] != 30*time.Second {
		t.Fatalf("sleeps = %v, want first 30s lock retry", sleeps)
	}
	second := h.runner.at[1]
	if want := start.Add(30 * time.Second); !second.Equal(want) {
		t.Fatalf("second attempt at %s, want %s", second, want)
	}
}

func TestSchedulerQueryErrorBacksOffWithoutRunning(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 3)
	var queryErr = errors.New("database unavailable")
	calls := 0
	h.store.hook = func(ctx context.Context) (integrity.Date, bool, error) {
		calls++
		if calls <= 2 {
			return integrity.Date{}, false, queryErr
		}
		return h.store.value()
	}
	h.store.setFrom(start)

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := h.runner.count(); got != 0 {
		t.Fatalf("runner calls = %d, want 0 while the store is unreadable", got)
	}
	sleeps := h.sleeper.recorded()
	if len(sleeps) != 3 {
		t.Fatalf("sleeps = %v, want 3", sleeps)
	}
	if sleeps[0] != 30*time.Second || sleeps[1] != time.Minute {
		t.Fatalf("backoff sleeps = %v, want 30s then 1m", sleeps[:2])
	}
}

func TestSchedulerCancelDuringSleepReturnsCleanly(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 1)
	h.store.setFrom(start)

	if err := h.run(); err != nil {
		t.Fatalf("scheduler Run on cancel = %v, want nil", err)
	}
	if got := h.runner.count(); got != 0 {
		t.Fatalf("runner calls = %d, want 0", got)
	}
}

func TestSchedulerCanceledRunReturnsCleanly(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, start, 1)
	ctx, cancel := context.WithCancel(context.Background())
	h.runner.run = func(int, time.Time) (integrity.Result, error) {
		cancel()
		return integrity.Result{}, context.Canceled
	}
	scheduler, err := integrity.NewScheduler(integrity.SchedulerOptions{
		Runner:                 h.runner,
		LatestScheduledRunDate: h.store.read,
		Clock:                  h.clock,
		Sleeper:                integrity.TimerSleeper{},
		Logger:                 quietSchedulerLogger(),
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatalf("scheduler Run after runner cancellation = %v, want nil", err)
	}
	if got := h.runner.count(); got != 1 {
		t.Fatalf("runner calls = %d, want 1", got)
	}
}

func TestSchedulerRequiresRunnerAndLatestReader(t *testing.T) {
	if _, err := integrity.NewScheduler(integrity.SchedulerOptions{}); err == nil {
		t.Fatal("NewScheduler accepted an empty dependency set")
	}
	if _, err := integrity.NewScheduler(integrity.SchedulerOptions{
		Runner: integrity.RunnerFunc(func(context.Context) (integrity.Result, error) {
			return integrity.Result{}, nil
		}),
	}); err == nil {
		t.Fatal("NewScheduler accepted a missing latest-run reader")
	}
}

func passedSchedulerReport() integrity.Report {
	checks := make([]integrity.CheckResult, 0, len(integrity.CheckOrder))
	for _, name := range integrity.CheckOrder {
		checks = append(checks, integrity.CheckResult{
			Name: name, Status: integrity.StatusPassed, Problems: []integrity.Problem{},
		})
	}
	return integrity.Report{
		RunDate:    integrity.NewDate(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)),
		Status:     integrity.StatusPassed,
		CheckCount: len(checks),
		Checks:     checks,
	}
}

func failedSchedulerReport() integrity.Report {
	report := passedSchedulerReport()
	report.Status = integrity.StatusFailed
	report.FailureCount = 1
	report.Checks[len(report.Checks)-1].Status = integrity.StatusFailed
	report.Checks[len(report.Checks)-1].ProblemCount = 1
	report.Checks[len(report.Checks)-1].Problems = []integrity.Problem{{
		Code: integrity.CodeTieoutUnexplained, Entity: "scope", ID: "scope-1",
	}}
	return report
}
