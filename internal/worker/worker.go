package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

const (
	// releaseTimeout bounds every shutdown-safe release/attempt context.
	releaseTimeout = 5 * time.Second
	// agentFailedReason is the dead-letter escalation reason for a poison case.
	agentFailedReason = "agent_failed"
)

// Sentinels returned by the worker. They classify the reason leasing stopped;
// caller command wrappers may treat them as a clean exit.
var (
	// ErrAgentDisabled means agent_enabled is off (or unreadable); the worker
	// leases nothing.
	ErrAgentDisabled = errors.New("worker: agent_enabled is off")
	// ErrDailyBudgetReached means the UTC-day spend is at or above the cap; the
	// worker leases nothing until the next day.
	ErrDailyBudgetReached = errors.New("worker: daily budget reached")
	// ErrInvalidOptions means the worker dependency set or config is incomplete.
	ErrInvalidOptions = errors.New("worker: invalid options")
)

// CaseService is the cases.Service surface the worker owns.
type CaseService interface {
	Lease(ctx context.Context, workerID string, ttl time.Duration) (cases.Case, error)
	Release(ctx context.Context, caseID, workerID string) (cases.Case, error)
	RecordFailedAttempt(ctx context.Context, caseID, workerID string, maxAttempts int) (cases.Case, error)
	ReclaimExpired(ctx context.Context) (int, error)
	Transition(ctx context.Context, req cases.TransitionRequest) (cases.Case, error)
}

// Runner is the agent loop surface. ProcessOnce and the poll loop call the same
// method, so the gate exercises the production worker policy.
type Runner interface {
	Run(ctx context.Context, caseID string) (agent.Result, error)
}

// SwitchReader reads the operator kill switch.
type SwitchReader interface {
	AgentEnabled(ctx context.Context) (bool, error)
}

// SpendReader sums agent_run cost for a UTC day.
type SpendReader interface {
	SpendSince(ctx context.Context, since time.Time) (decimal.Decimal, error)
}

// Sleeper is the interruptible poll wait. Production uses a timer; tests use a
// controllable fake.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// Options is the worker dependency set.
type Options struct {
	ID      string
	Config  config.WorkerConfig
	Cases   CaseService
	Runner  Runner
	Switch  SwitchReader
	Spend   SpendReader
	Clock   clock.Clock
	Sleeper Sleeper
	Logger  *slog.Logger
}

// Worker polls and processes cases until its context is canceled.
type Worker struct {
	id      string
	config  config.WorkerConfig
	budget  decimal.Decimal
	cases   CaseService
	runner  Runner
	switchR SwitchReader
	spend   SpendReader
	clock   clock.Clock
	sleeper Sleeper
	logger  *slog.Logger

	spendMu    sync.Mutex
	spendDay   time.Time
	spendAt    time.Time
	spendTotal decimal.Decimal
	spendLocal decimal.Decimal
	spendValid bool
}

// New validates and returns a worker.
func New(opts Options) (*Worker, error) {
	if opts.Cases == nil || opts.Runner == nil || opts.Switch == nil || opts.Spend == nil {
		return nil, fmt.Errorf("%w: cases, runner, switch and spend readers are required", ErrInvalidOptions)
	}
	if opts.Clock == nil {
		return nil, fmt.Errorf("%w: clock is required", ErrInvalidOptions)
	}
	if opts.ID == "" {
		return nil, fmt.Errorf("%w: worker id is required", ErrInvalidOptions)
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOptions, err)
	}
	sleeper := opts.Sleeper
	if sleeper == nil {
		sleeper = TimerSleeper{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		id:      opts.ID,
		config:  opts.Config,
		budget:  opts.Config.DailyBudget(),
		cases:   opts.Cases,
		runner:  opts.Runner,
		switchR: opts.Switch,
		spend:   opts.Spend,
		clock:   opts.Clock,
		sleeper: sleeper,
		logger:  logger,
	}, nil
}

// ID returns the stable worker id.
func (w *Worker) ID() string { return w.id }

// NewID returns worker:<hostname>:<pid>:<8 random hex>. It is stable for the
// process and unique across processes.
func NewID() (string, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("worker: generate id: %w", err)
	}
	return fmt.Sprintf("worker:%s:%d:%s", host, os.Getpid(), hex.EncodeToString(raw[:])), nil
}

// Run polls and processes cases until ctx is canceled. Startup reclaims expired
// leases once. Poll and database errors are logged and retried after one poll
// interval; they do not crash the worker.
func (w *Worker) Run(ctx context.Context) error {
	if n, err := w.cases.ReclaimExpired(ctx); err != nil {
		w.logger.Warn("worker: startup reclaim failed", "error", err)
	} else if n > 0 {
		w.logger.Info("worker: reclaimed expired leases", "count", n)
	}

	runCtx, cancelRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRuns()

	var wg sync.WaitGroup
	sem := make(chan struct{}, w.config.Concurrency)

loop:
	for ctx.Err() == nil {
		if err := w.leaseAllowed(ctx); err != nil {
			w.logLeasePaused(err)
			if !w.wait(ctx) {
				break
			}
			continue
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}

		// Re-check after the semaphore wait. A run may finish while this poll is
		// blocked, but the switch or daily budget can also change during the
		// wait. Leasing without this check would start a run after an operator
		// turned the agent off or after the daily budget was exhausted.
		if err := w.leaseAllowed(ctx); err != nil {
			<-sem
			w.logLeasePaused(err)
			if !w.wait(ctx) {
				break
			}
			continue
		}

		leased, err := w.cases.Lease(ctx, w.id, w.config.LeaseTTL)
		if err != nil {
			<-sem
			if ctx.Err() != nil {
				break
			}
			if errors.Is(err, cases.ErrNoCase) {
				if !w.wait(ctx) {
					break
				}
				continue
			}
			w.logger.Warn("worker: lease failed", "error", err)
			if !w.wait(ctx) {
				break
			}
			continue
		}

		if leased.Attempts >= w.config.MaxAttempts {
			_ = w.escalateExhausted(ctx, leased)
			<-sem
			continue
		}

		wg.Add(1)
		go func(c cases.Case) {
			defer wg.Done()
			defer func() { <-sem }()
			w.process(runCtx, c)
		}(leased)
	}

	w.shutdown(&wg, cancelRuns)
	return nil
}

// ProcessOnce leases and synchronously runs at most one case. It shares the
// poll loop's switch, budget, attempt-cap, and finalization policy.
func (w *Worker) ProcessOnce(ctx context.Context) error {
	if err := w.leaseAllowed(ctx); err != nil {
		return err
	}
	leased, err := w.cases.Lease(ctx, w.id, w.config.LeaseTTL)
	if err != nil {
		return err
	}
	if leased.Attempts >= w.config.MaxAttempts {
		return w.escalateExhausted(ctx, leased)
	}
	w.process(ctx, leased)
	return nil
}

// logLeasePaused records a failed lease precheck at the severity the reason
// deserves: normal operator pauses are debug, unreadable state is a warning.
func (w *Worker) logLeasePaused(err error) {
	switch {
	case errors.Is(err, ErrAgentDisabled), errors.Is(err, ErrDailyBudgetReached):
		w.logger.Debug("worker: leasing paused", "reason", err.Error())
	default:
		w.logger.Warn("worker: lease precheck failed", "error", err)
	}
}

// leaseAllowed applies the fail-closed switch and daily budget checks.
func (w *Worker) leaseAllowed(ctx context.Context) error {
	enabled, err := w.switchR.AgentEnabled(ctx)
	if err != nil {
		return fmt.Errorf("worker: read agent_enabled switch: %w", err)
	}
	if !enabled {
		return ErrAgentDisabled
	}
	spent, err := w.spentToday(ctx)
	if err != nil {
		return err
	}
	if spent.GreaterThanOrEqual(w.budget) {
		return ErrDailyBudgetReached
	}
	return nil
}

// spentToday returns the UTC-day spend. It queries the store at most once per
// poll interval and adds locally finished run costs between queries.
func (w *Worker) spentToday(ctx context.Context) (decimal.Decimal, error) {
	now := w.clock.Now().UTC()
	day := startOfUTCDay(now)

	w.spendMu.Lock()
	if w.spendValid && w.spendDay.Equal(day) && now.Sub(w.spendAt) < w.config.PollInterval {
		total := w.spendTotal.Add(w.spendLocal)
		w.spendMu.Unlock()
		return total, nil
	}
	w.spendMu.Unlock()

	total, err := w.spend.SpendSince(ctx, day)
	if err != nil {
		return decimal.Zero, fmt.Errorf("worker: read daily spend: %w", err)
	}
	w.spendMu.Lock()
	w.spendTotal = total
	w.spendLocal = decimal.Zero
	w.spendDay = day
	w.spendAt = now
	w.spendValid = true
	w.spendMu.Unlock()
	return total, nil
}

// recordSpend adds a finished run's cost to the local cache.
func (w *Worker) recordSpend(cost decimal.Decimal) {
	if !cost.IsPositive() {
		return
	}
	w.spendMu.Lock()
	w.spendLocal = w.spendLocal.Add(cost)
	w.spendMu.Unlock()
}

func startOfUTCDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// wait sleeps for one poll interval. It reports false when the context ended.
func (w *Worker) wait(ctx context.Context) bool {
	if err := w.sleeper.Sleep(ctx, w.config.PollInterval); err != nil {
		return false
	}
	return ctx.Err() == nil
}

// shutdown stops leasing, waits up to shutdown_grace for in-flight runs, then
// cancels them and waits for their release paths.
func (w *Worker) shutdown(wg *sync.WaitGroup, cancel context.CancelFunc) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(w.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		cancel()
		return
	case <-timer.C:
		w.logger.Warn("worker: shutdown grace elapsed; canceling in-flight runs",
			"grace", w.config.ShutdownGrace)
		cancel()
	}

	timer.Reset(releaseTimeout + time.Second)
	select {
	case <-done:
	case <-timer.C:
		w.logger.Warn("worker: in-flight runs did not stop after cancel")
	}
}

// TimerSleeper is the production interruptible poll wait.
type TimerSleeper struct{}

// Sleep waits for d or until ctx is canceled.
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
