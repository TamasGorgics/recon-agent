//go:build integration

// covers: FIN-52
package integrity_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/integrity"
)

const schedulerFixtureStart = "2026-06-15T03:00:00Z"

func fixtureLatest(app *pgxpool.Pool) func(context.Context) (integrity.Date, bool, error) {
	return func(ctx context.Context) (integrity.Date, bool, error) {
		return integrity.LatestScheduledRunDate(ctx, app)
	}
}

func newIntegrationScheduler(
	t *testing.T,
	start time.Time,
	maxSleeps int,
	runner integrity.Runner,
	latest func(context.Context) (integrity.Date, bool, error),
) (*integrity.Scheduler, *clock.Fake, context.Context, *advancingSleeper) {
	t.Helper()
	clk := clock.NewFake(start)
	ctx, cancel := context.WithCancel(context.Background())
	sleeper := &advancingSleeper{clock: clk, cancel: cancel, max: maxSleeps}
	t.Cleanup(cancel)
	scheduler, err := integrity.NewScheduler(integrity.SchedulerOptions{
		Runner:                 runner,
		LatestScheduledRunDate: latest,
		Clock:                  clk,
		Sleeper:                sleeper,
		Logger:                 quietSchedulerLogger(),
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	return scheduler, clk, ctx, sleeper
}

func insertIntegrityRow(t *testing.T, f *fixture, runDate, status, triggeredBy string) {
	t.Helper()
	failureCount := 0
	if status == "failed" {
		failureCount = 1
	}
	started := mustDate(t, runDate).Add(2 * time.Hour)
	if _, err := f.db.Pool.Exec(context.Background(), `
		INSERT INTO integrity_runs
		    (run_date, started_at, finished_at, status, check_count,
		     failure_count, checks, triggered_by)
		VALUES ($1::date, $2, $2, $3, 5, $4, '[{},{},{},{},{}]'::jsonb, $5)`,
		runDate, started, status, failureCount, triggeredBy); err != nil {
		t.Fatalf("insert integrity_runs row: %v", err)
	}
}

// TestSchedulerIntegrationCatchUpRunsCurrentDate proves an empty schedule
// catches up once after 02:00, writes exactly one schedule row, and then stops
// for the day.
func TestSchedulerIntegrationCatchUpRunsCurrentDate(t *testing.T) {
	f := newFixture(t)
	app := f.appPool(t)
	clk := clock.NewFake(mustTime(t, schedulerFixtureStart))
	calls := 0
	runner := integrity.RunnerFunc(func(ctx context.Context) (integrity.Result, error) {
		calls++
		return integrity.Run(ctx, app, integrity.Options{
			TriggeredBy: integrity.TriggerSchedule,
			Clock:       clk,
		})
	})
	scheduler, _, ctx, sleeper := newIntegrationScheduler(
		t, mustTime(t, schedulerFixtureStart), 1, runner, fixtureLatest(app))

	if err := scheduler.Run(ctx); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("scheduled runner calls = %d, want 1 catch-up", calls)
	}
	if got := countRows(t, f.db.Pool,
		`SELECT count(*) FROM integrity_runs WHERE triggered_by = 'schedule'`); got != 1 {
		t.Fatalf("scheduled rows = %d, want 1", got)
	}
	var runDate, status string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT run_date::text, status FROM integrity_runs WHERE triggered_by = 'schedule'`).
		Scan(&runDate, &status); err != nil {
		t.Fatalf("read scheduled row: %v", err)
	}
	if runDate != fixtureRunDate || status != "passed" {
		t.Fatalf("scheduled row = date %s status %s, want %s/passed",
			runDate, status, fixtureRunDate)
	}
	if sleeps := sleeper.recorded(); len(sleeps) != 1 || sleeps[0] != 23*time.Hour {
		t.Fatalf("sleeps = %v, want [23h] after the 03:00 catch-up", sleeps)
	}
}

// TestSchedulerIntegrationScheduledRowSuppresses proves a pre-existing
// scheduled row for the UTC date (passed or failed) suppresses the run; the row
// is the day's attempt across worker restarts.
func TestSchedulerIntegrationScheduledRowSuppresses(t *testing.T) {
	for _, status := range []string{"passed", "failed"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t)
			app := f.appPool(t)
			insertIntegrityRow(t, f, fixtureRunDate, status, "schedule")
			runner := integrity.RunnerFunc(func(context.Context) (integrity.Result, error) {
				t.Error("scheduler ran despite today's scheduled row")
				return integrity.Result{}, nil
			})
			scheduler, _, ctx, sleeper := newIntegrationScheduler(
				t, mustTime(t, schedulerFixtureStart), 1, runner, fixtureLatest(app))

			if err := scheduler.Run(ctx); err != nil {
				t.Fatalf("scheduler Run: %v", err)
			}
			if got := countRows(t, f.db.Pool,
				`SELECT count(*) FROM integrity_runs WHERE triggered_by = 'schedule'`); got != 1 {
				t.Fatalf("scheduled rows = %d, want the seeded 1", got)
			}
			if sleeps := sleeper.recorded(); len(sleeps) != 1 || sleeps[0] != 23*time.Hour {
				t.Fatalf("sleeps = %v, want [23h]", sleeps)
			}
		})
	}
}

// TestSchedulerIntegrationManualRunDoesNotSatisfySchedule proves the scheduler
// filters triggered_by='schedule': a manual row for today does not suppress the
// scheduled attempt.
func TestSchedulerIntegrationManualRunDoesNotSatisfySchedule(t *testing.T) {
	f := newFixture(t)
	app := f.appPool(t)
	insertIntegrityRow(t, f, fixtureRunDate, "passed", "manual")
	calls := 0
	runner := integrity.RunnerFunc(func(context.Context) (integrity.Result, error) {
		calls++
		return integrity.Result{ID: "fake", Report: passedSchedulerReport()}, nil
	})
	scheduler, _, ctx, sleeper := newIntegrationScheduler(
		t, mustTime(t, schedulerFixtureStart), 1, runner, fixtureLatest(app))

	if err := scheduler.Run(ctx); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("scheduled runner calls = %d, want 1 despite the manual row", calls)
	}
	if sleeps := sleeper.recorded(); len(sleeps) != 1 || sleeps[0] != 30*time.Second {
		t.Fatalf("sleeps = %v, want a 30s guard backoff after the fake success", sleeps)
	}
	if got := countRows(t, f.db.Pool,
		`SELECT count(*) FROM integrity_runs WHERE triggered_by = 'manual'`); got != 1 {
		t.Fatalf("manual rows = %d, want 1", got)
	}
}

// TestSchedulerIntegrationLockLoserLeavesNoRowOrObservation holds the run's
// advisory lock while the scheduler attempts its catch-up. The loser gets
// ErrAlreadyRunning, retries after 30s, and is canceled without writing a row
// or notifying the integrity observer (so the gauge is unchanged).
func TestSchedulerIntegrationLockLoserLeavesNoRowOrObservation(t *testing.T) {
	f := newFixture(t)
	app := f.appPool(t)
	ctx := context.Background()
	lockTx, err := app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	var locked bool
	if err := lockTx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`,
		integrity.AdvisoryLockKey).Scan(&locked); err != nil {
		t.Fatalf("acquire fixture lock: %v", err)
	}
	if !locked {
		t.Fatal("fixture failed to acquire the integrity lock")
	}
	defer func() { _ = lockTx.Rollback(context.WithoutCancel(ctx)) }()

	clk := clock.NewFake(mustTime(t, schedulerFixtureStart))
	observer := &recordingObserver{}
	runner := integrity.RunnerFunc(func(ctx context.Context) (integrity.Result, error) {
		return integrity.Run(ctx, app, integrity.Options{
			TriggeredBy: integrity.TriggerSchedule,
			Clock:       clk,
			Observer:    observer,
		})
	})
	scheduler, _, runCtx, sleeper := newIntegrationScheduler(
		t, mustTime(t, schedulerFixtureStart), 1, runner, fixtureLatest(app))

	if err := scheduler.Run(runCtx); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if len(observer.calls) != 0 {
		t.Fatalf("lock loser observer calls = %v, want none", observer.calls)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 0 {
		t.Fatalf("lock loser wrote %d rows, want 0", got)
	}
	if sleeps := sleeper.recorded(); len(sleeps) != 1 || sleeps[0] != 30*time.Second {
		t.Fatalf("sleeps = %v, want [30s] lock retry", sleeps)
	}
}

// cancelOnRowSleeper models the worker wait: it stops as soon as the scheduled
// row is visible, advancing the shared fake clock otherwise.
type cancelOnRowSleeper struct {
	clock  *clock.Fake
	cancel context.CancelFunc
	hasRow func() bool
	max    int
	mu     sync.Mutex
	sleeps int
}

func (s *cancelOnRowSleeper) Sleep(_ context.Context, d time.Duration) error {
	if s.hasRow != nil && s.hasRow() {
		s.cancel()
		return context.Canceled
	}
	s.mu.Lock()
	s.sleeps++
	n := s.sleeps
	s.mu.Unlock()
	s.clock.Advance(d)
	if n >= s.max {
		s.cancel()
		return context.Canceled
	}
	return nil
}

// TestSchedulerIntegrationTwoWorkersOneRow starts two schedulers against the
// same real database and clock. The transaction-scoped advisory lock makes
// exactly one scheduled run execute; the other observes the persisted row and
// stops without a second row.
func TestSchedulerIntegrationTwoWorkersOneRow(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := clock.NewFake(mustTime(t, schedulerFixtureStart))
	sharedSleeper := &cancelOnRowSleeper{
		clock:  clk,
		cancel: cancel,
		max:    200,
		hasRow: func() bool {
			var n int64
			err := f.db.Pool.QueryRow(context.Background(),
				`SELECT count(*) FROM integrity_runs WHERE triggered_by = 'schedule'`).Scan(&n)
			return err == nil && n > 0
		},
	}

	newWorker := func() *integrity.Scheduler {
		app := f.appPool(t)
		runner := integrity.RunnerFunc(func(ctx context.Context) (integrity.Result, error) {
			return integrity.Run(ctx, app, integrity.Options{
				TriggeredBy: integrity.TriggerSchedule,
				Clock:       clk,
			})
		})
		scheduler, err := integrity.NewScheduler(integrity.SchedulerOptions{
			Runner:                 runner,
			LatestScheduledRunDate: fixtureLatest(app),
			Clock:                  clk,
			Sleeper:                sharedSleeper,
			Logger:                 quietSchedulerLogger(),
		})
		if err != nil {
			t.Fatalf("NewScheduler: %v", err)
		}
		return scheduler
	}

	workers := []*integrity.Scheduler{newWorker(), newWorker()}
	var wg sync.WaitGroup
	errs := make(chan error, len(workers))
	for _, worker := range workers {
		wg.Add(1)
		go func(scheduler *integrity.Scheduler) {
			defer wg.Done()
			errs <- scheduler.Run(ctx)
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("scheduler Run: %v", err)
		}
	}
	if got := countRows(t, f.db.Pool,
		`SELECT count(*) FROM integrity_runs WHERE triggered_by = 'schedule'`); got != 1 {
		t.Fatalf("scheduled rows after two schedulers = %d, want 1", got)
	}
}

// TestSchedulerIntegrationCancelDuringSleep proves cancellation stops the
// scheduler without a run or a row.
func TestSchedulerIntegrationCancelDuringSleep(t *testing.T) {
	f := newFixture(t)
	app := f.appPool(t)
	insertIntegrityRow(t, f, fixtureRunDate, "passed", "schedule")
	runner := integrity.RunnerFunc(func(context.Context) (integrity.Result, error) {
		t.Error("scheduler ran after cancellation")
		return integrity.Result{}, nil
	})
	scheduler, _, ctx, _ := newIntegrationScheduler(
		t, mustTime(t, schedulerFixtureStart), 1, runner, fixtureLatest(app))

	if err := scheduler.Run(ctx); err != nil {
		t.Fatalf("scheduler Run: %v", err)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 1 {
		t.Fatalf("rows = %d, want the seeded 1", got)
	}
}
