package integrity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const (
	// DefaultRunAtUTC is the fixed daily schedule: 02:00 UTC. M5-06 does not
	// add a schedule config key; operations can revisit this constant only with
	// a measured need.
	DefaultRunAtUTC = 2 * time.Hour
	// schedulerLockRetry is the short wait after ErrAlreadyRunning. The winner
	// normally commits within one run; the loser re-queries and stops once the
	// scheduled row exists.
	schedulerLockRetry = 30 * time.Second
	// schedulerBaseBackoff is the first wait after an operational failure that
	// did not persist a row.
	schedulerBaseBackoff = 30 * time.Second
	// schedulerMaxBackoff caps the operational retry wait.
	schedulerMaxBackoff = 15 * time.Minute
)

// Runner is the single-run surface the scheduler calls. Production wires
// integrity.Run; tests use a fake. A completed failed run is (Result, nil);
// only a run that could not persist a row returns an error.
type Runner interface {
	Run(ctx context.Context) (Result, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context) (Result, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context) (Result, error) { return f(ctx) }

// Sleeper is the context-aware wait the scheduler uses between due checks.
// Production uses TimerSleeper; tests drive a fake clock instead of sleeping.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// TimerSleeper is the production interruptible wait.
type TimerSleeper struct{}

// Sleep waits for d or until ctx is canceled, whichever happens first.
func (TimerSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ScheduleQueryer is the read surface used for the persisted schedule
// idempotency state. *pgxpool.Pool, *pgx.Conn, and pgx.Tx satisfy it.
type ScheduleQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LatestScheduledRunDate returns the persisted max(run_date) across rows with
// triggered_by='schedule'. The row is authoritative across worker restarts and
// replicas; manual runs are deliberately excluded so they cannot satisfy the
// scheduled attempt for a day.
func LatestScheduledRunDate(ctx context.Context, q ScheduleQueryer) (Date, bool, error) {
	var latest *time.Time
	if err := q.QueryRow(ctx,
		`SELECT max(run_date) FROM integrity_runs WHERE triggered_by = 'schedule'`).
		Scan(&latest); err != nil {
		return Date{}, false, fmt.Errorf("integrity: read latest scheduled run date: %w", err)
	}
	if latest == nil {
		return Date{}, false, nil
	}
	return NewDate(*latest), true, nil
}

// SchedulerOptions is the scheduler dependency set. Runner and
// LatestScheduledRunDate are required; the others default.
type SchedulerOptions struct {
	// Runner executes one scheduled integrity run. Required.
	Runner Runner
	// LatestScheduledRunDate reads the persisted scheduled row. Required;
	// production uses LatestScheduledRunDate with the recon_app pool.
	LatestScheduledRunDate func(ctx context.Context) (Date, bool, error)
	// Clock supplies UTC due logic and the run date. Nil defaults to
	// clock.Real; domain code never reads the wall clock directly.
	Clock clock.Clock
	// Sleeper waits between due checks. Nil defaults to TimerSleeper.
	Sleeper Sleeper
	// Logger receives bounded scheduler diagnostics. Nil defaults to
	// slog.Default.
	Logger *slog.Logger
}

// Scheduler runs the daily 02:00 UTC integrity job with catch-up and
// single-flight (the run's advisory lock), not leader election. Every worker
// may start one; the transaction-scoped lock makes exactly one run execute.
type Scheduler struct {
	runner  Runner
	latest  func(ctx context.Context) (Date, bool, error)
	clock   clock.Clock
	sleeper Sleeper
	logger  *slog.Logger
}

// NewScheduler validates the dependency set and applies defaults.
func NewScheduler(opts SchedulerOptions) (*Scheduler, error) {
	if opts.Runner == nil {
		return nil, errors.New("integrity: scheduler runner is required")
	}
	if opts.LatestScheduledRunDate == nil {
		return nil, errors.New("integrity: scheduler latest-run reader is required")
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	sleeper := opts.Sleeper
	if sleeper == nil {
		sleeper = TimerSleeper{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		runner:  opts.Runner,
		latest:  opts.LatestScheduledRunDate,
		clock:   clk,
		sleeper: sleeper,
		logger:  logger,
	}, nil
}

// Run evaluates the due rules until ctx is canceled. Shutdown is a clean nil
// return: context.Canceled is not an integrity failure. Any other sleeper
// error is returned so cmd/worker can cancel the poll loop.
func (s *Scheduler) Run(ctx context.Context) error {
	var attempted Date
	var backoff time.Duration

	for {
		if ctx.Err() != nil {
			return nil
		}
		now := s.clock.Now().UTC()
		today := NewDate(now)
		if !attempted.IsZero() && attempted.String() != today.String() {
			// A new UTC date resets the in-memory guard; the persisted row
			// remains authoritative across restarts.
			attempted = Date{}
			backoff = 0
		}

		latest, has, err := s.latest(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.logger.Warn("integrity scheduler: read latest scheduled run failed", "error", err)
			backoff = nextSchedulerBackoff(backoff)
			if err := s.wait(ctx, backoff); err != nil {
				return schedulerWaitError(ctx, err)
			}
			continue
		}

		dueAt := today.Add(DefaultRunAtUTC)
		switch {
		case has && !latest.Before(today.Time):
			// Today already has a scheduled attempt (passed or failed), or a
			// future-dated row exists (clock skew). Never run backward; wait
			// for tomorrow's boundary and re-evaluate.
			if err := s.wait(ctx, dueAt.Add(24*time.Hour).Sub(now)); err != nil {
				return schedulerWaitError(ctx, err)
			}
			continue
		case now.Before(dueAt):
			// No scheduled row for today yet and the boundary has not passed:
			// wait for 02:00 and then let the catch-up branch run.
			if err := s.wait(ctx, dueAt.Sub(now)); err != nil {
				return schedulerWaitError(ctx, err)
			}
			continue
		case !attempted.IsZero() && attempted.String() == today.String():
			// This process already completed today's run but no row is visible
			// yet. Back off instead of hot-looping; the row should appear.
			backoff = nextSchedulerBackoff(backoff)
			if err := s.wait(ctx, backoff); err != nil {
				return schedulerWaitError(ctx, err)
			}
			continue
		}

		result, runErr := s.runner.Run(ctx)
		if runErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(runErr, ErrAlreadyRunning) {
				// Another worker holds the transaction-scoped lock. Do not
				// record an attempt or change any gauge; re-query shortly.
				s.logger.Info("integrity scheduler: another run holds the lock; retrying")
				if err := s.wait(ctx, schedulerLockRetry); err != nil {
					return schedulerWaitError(ctx, err)
				}
				continue
			}
			// The run could not persist a row. Do not record an attempt: the
			// next loop must retry the runner after the backoff until a row
			// exists for today, the UTC date changes, or shutdown.
			s.logger.Warn("integrity scheduler: run did not complete", "error", runErr)
			backoff = nextSchedulerBackoff(backoff)
			if err := s.wait(ctx, backoff); err != nil {
				return schedulerWaitError(ctx, err)
			}
			continue
		}

		// A completed pass or fail is the day's attempt. Failed rows count too;
		// the next loop re-queries the persisted row before sleeping.
		attempted = today
		backoff = 0
		s.logger.Info("integrity scheduler: scheduled run completed",
			"run_id", result.ID,
			"run_date", result.Report.RunDate.String(),
			"status", string(result.Report.Status),
			"failure_count", result.Report.FailureCount)
	}
}

// wait sleeps for d unless it is non-positive. Context cancellation is
// returned to the caller and normalized by schedulerWaitError.
func (s *Scheduler) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	return s.sleeper.Sleep(ctx, d)
}

// schedulerWaitError treats shutdown cancellation as a clean stop and returns
// any other sleeper error to the process owner.
func schedulerWaitError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// nextSchedulerBackoff grows 30s -> 1m -> 5m -> 15m. A completed run resets it
// to zero in Run.
func nextSchedulerBackoff(current time.Duration) time.Duration {
	switch {
	case current < schedulerBaseBackoff:
		return schedulerBaseBackoff
	case current < time.Minute:
		return time.Minute
	case current < 5*time.Minute:
		return 5 * time.Minute
	default:
		return schedulerMaxBackoff
	}
}
