// Command worker runs the M2 case worker: poll, lease, run, and finalize cases
// with the configured budgets, attempt cap, kill switch, and graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/integrity"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/metrics"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
	"github.com/TamasGorgics/recon-agent/internal/worker"
)

const usage = `usage: worker [--once]

Runs the M2 case worker until SIGINT/SIGTERM. The worker polls the case queue,
leases with FOR UPDATE SKIP LOCKED, runs the agent loop under the configured
concurrency, daily USD budget, and attempt cap, and releases leases on
shutdown.

In long-running mode it also runs the M5-06 daily integrity job at 02:00 UTC
with catch-up and advisory-lock single-flight. --once is case-only and starts
no integrity scheduler.

Flags:
  --once   process at most one available case and exit

Configuration: CONFIG_FILE YAML plus environment overlays. Requires
DATABASE_APP_URL (recon_app), DATABASE_AGENT_URL (recon_agent_ro), and
OPENCODE_API_KEY for live model calls.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	once := flags.Bool("once", false, "process at most one available case and exit")
	flags.Usage = func() { _, _ = fmt.Fprint(os.Stderr, usage) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.ValidateWorkerRuntime(); err != nil {
		return err
	}

	return runWithTelemetry(ctx, logger, func(ctx context.Context, provider *telemetry.Provider) error {
		return runWorker(ctx, cfg, logger, provider, *once)
	})
}

// runWorker owns the database/worker lifecycle. Telemetry shutdown is handled
// by runWithTelemetry after this function returns, so the poll loop stops first.
func runWorker(ctx context.Context, cfg config.Config, logger *slog.Logger, telemetryProvider *telemetry.Provider, once bool) error {
	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		return err
	}
	defer app.Close()
	agentRO, err := db.OpenRole(ctx, db.RoleAgentRO, cfg.Database.AgentURL)
	if err != nil {
		return err
	}
	defer agentRO.Close()

	// One process registry is created before the agent runner and case service
	// so their post-commit observers and the /metrics endpoint share counters.
	processMetrics, err := metrics.New(metrics.Options{
		Component:        "worker",
		GoCollector:      true,
		ProcessCollector: true,
		Queryer:          app.Pool,
		GatherTimeout:    cfg.Metrics.GatherTimeout,
		Logger:           logger,
	})
	if err != nil {
		return fmt.Errorf("build metrics registry: %w", err)
	}
	runObserver := metrics.NewAgentObserver(processMetrics)
	caseObserver := metrics.NewCasesObserver(processMetrics, "worker")
	workerObserver := metrics.NewWorkerObserver(processMetrics)
	integrityObserver := metrics.NewIntegrityObserver(processMetrics)
	tieoutObserver := metrics.NewTieoutObserver(processMetrics)

	timeSource := clock.Real{}
	client, err := llm.NewClient(cfg.LLM, llm.Options{Clock: timeSource, Logger: logger})
	if err != nil {
		return err
	}

	switchReader := agent.DBSwitch{Queryer: app.Pool}
	runner, err := agent.NewProductionRunner(agent.SetupDeps{
		App:             app.Pool,
		Agent:           agentRO.Pool,
		AgentExec:       agentRO.Pool,
		Client:          client,
		Clock:           timeSource,
		Config:          cfg,
		UntrustedFields: cfg.ConfiguredUntrustedFields(),
		BeforeStep: func(ctx context.Context) error {
			enabled, err := switchReader.AgentEnabled(ctx)
			if err != nil {
				return err
			}
			if !enabled {
				return agent.ErrAgentDisabled
			}
			return nil
		},
		Logger:       logger,
		Telemetry:    telemetryProvider,
		RunObserver:  runObserver,
		CaseObserver: caseObserver,
	})
	if err != nil {
		return fmt.Errorf("build agent runner: %w", err)
	}

	systemActor, err := audit.SystemActor("worker")
	if err != nil {
		return err
	}
	caseService, err := cases.NewService(app.Pool, timeSource, systemActor,
		cases.WithMaxAttempts(cfg.Worker.MaxAttempts),
		cases.WithObserver(caseObserver))
	if err != nil {
		return fmt.Errorf("build case service: %w", err)
	}

	workerID, err := worker.NewID()
	if err != nil {
		return err
	}
	pollWorker, err := worker.New(worker.Options{
		ID:       workerID,
		Config:   cfg.Worker,
		Cases:    caseService,
		Runner:   runner,
		Switch:   switchReader,
		Spend:    worker.DBSpend{Queryer: app.Pool},
		Clock:    timeSource,
		Sleeper:  worker.TimerSleeper{},
		Logger:   logger,
		Observer: workerObserver,
	})
	if err != nil {
		return err
	}
	logger.Info("worker starting",
		"worker_id", pollWorker.ID(),
		"concurrency", cfg.Worker.Concurrency,
		"poll_interval", cfg.Worker.PollInterval.String(),
		"lease_ttl", cfg.Worker.LeaseTTL.String(),
		"daily_budget_usd", cfg.Worker.DailyBudgetUSD.String(),
		"max_attempts", cfg.Worker.MaxAttempts,
		"once", once)

	metricsServer, err := startWorkerMetrics(ctx, cfg, logger, processMetrics, once)
	if err != nil {
		return err
	}
	if metricsServer != nil {
		defer func() {
			if closeErr := metricsServer.Close(); closeErr != nil {
				logger.Error("worker metrics listener shutdown failed", "error", closeErr)
			}
		}()
	}

	if once {
		return runWorkerMode(ctx, true, pollWorker, pollWorker, nil, logger)
	}

	scheduler, err := integrity.NewScheduler(integrity.SchedulerOptions{
		Runner: integrity.RunnerFunc(func(ctx context.Context) (integrity.Result, error) {
			return integrity.Run(ctx, app.Pool, integrity.Options{
				TriggeredBy:    integrity.TriggerSchedule,
				Clock:          timeSource,
				Observer:       integrityObserver,
				TieoutObserver: tieoutObserver,
			})
		}),
		LatestScheduledRunDate: func(ctx context.Context) (integrity.Date, bool, error) {
			return integrity.LatestScheduledRunDate(ctx, app.Pool)
		},
		Clock:   timeSource,
		Sleeper: integrity.TimerSleeper{},
		Logger:  logger,
	})
	if err != nil {
		return fmt.Errorf("build integrity scheduler: %w", err)
	}

	if err := runWorkerMode(ctx, false, pollWorker, pollWorker, scheduler, logger); err != nil {
		return err
	}
	logger.Info("worker stopped", "worker_id", pollWorker.ID())
	return nil
}

// pollLoop is the long-running case poll surface used by runJoined.
type pollLoop interface {
	Run(ctx context.Context) error
}

// onceProcessor is the single-case surface used by --once.
type onceProcessor interface {
	ProcessOnce(ctx context.Context) error
}

// schedulerLoop is the integrity scheduler surface used by runJoined.
type schedulerLoop interface {
	Run(ctx context.Context) error
}

// runWorkerMode is the single mode switch. --once processes at most one case
// and never starts the integrity scheduler; long-running mode joins the poll
// loop and the scheduler for the whole process lifetime.
func runWorkerMode(
	ctx context.Context,
	once bool,
	poll pollLoop,
	onceJob onceProcessor,
	scheduler schedulerLoop,
	logger *slog.Logger,
) error {
	if once {
		return runCaseOnce(ctx, onceJob, logger)
	}
	if scheduler == nil {
		return errors.New("worker: integrity scheduler is required in long-running mode")
	}
	return runJoined(ctx, poll, scheduler, logger)
}

// runCaseOnce preserves the existing --once outcome mapping: a missing case,
// disabled agent, or exhausted daily budget is a clean exit, not an error.
func runCaseOnce(ctx context.Context, job onceProcessor, logger *slog.Logger) error {
	err := job.ProcessOnce(ctx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, cases.ErrNoCase):
		logger.Info("worker: no case available")
		return nil
	case errors.Is(err, worker.ErrAgentDisabled):
		logger.Info("worker: agent_enabled is off; not leasing")
		return nil
	case errors.Is(err, worker.ErrDailyBudgetReached):
		logger.Info("worker: daily budget reached; not leasing")
		return nil
	default:
		return err
	}
}

// runJoined starts the poll loop and the integrity scheduler, cancels the
// other when either returns, and waits for both before returning. The caller's
// deferred app.Close therefore runs only after the scheduler has stopped. A
// non-cancellation scheduler error wins because it cancels the poll loop and
// must surface from runWorker; shutdown cancellation is not an error.
func runJoined(ctx context.Context, poll pollLoop, scheduler schedulerLoop, logger *slog.Logger) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pollDone := make(chan error, 1)
	schedulerDone := make(chan error, 1)
	go func() { pollDone <- poll.Run(runCtx) }()
	go func() { schedulerDone <- scheduler.Run(runCtx) }()

	var pollErr, schedulerErr error
	select {
	case pollErr = <-pollDone:
		cancel()
		schedulerErr = <-schedulerDone
	case schedulerErr = <-schedulerDone:
		cancel()
		pollErr = <-pollDone
	}

	pollErr = normalizeLoopShutdown(ctx, pollErr)
	schedulerErr = normalizeLoopShutdown(ctx, schedulerErr)
	if schedulerErr != nil {
		if pollErr != nil && logger != nil {
			logger.Warn("worker: poll loop stopped after scheduler error", "error", pollErr)
		}
		return schedulerErr
	}
	if pollErr != nil {
		return pollErr
	}
	return nil
}

// normalizeLoopShutdown treats cancellation from either loop as a clean stop.
func normalizeLoopShutdown(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// startWorkerMetrics starts the worker's loopback /metrics listener for the
// long-running process. --once is short-lived and must not bind the port, so it
// returns nil without touching the configured address.
func startWorkerMetrics(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	m *metrics.Metrics,
	once bool,
) (*metrics.Server, error) {
	if once {
		return nil, nil
	}
	return metrics.StartForProcess(ctx, m, cfg.Metrics.WorkerAddr, "metrics.worker_addr", logger,
		metrics.ServerOptions{
			ReadHeaderTimeout: cfg.Metrics.ReadHeaderTimeout,
			ShutdownTimeout:   cfg.Metrics.ShutdownTimeout,
		})
}
