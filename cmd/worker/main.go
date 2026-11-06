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
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/worker"
)

const usage = `usage: worker [--once]

Runs the M2 case worker until SIGINT/SIGTERM. The worker polls the case queue,
leases with FOR UPDATE SKIP LOCKED, runs the agent loop under the configured
concurrency, daily USD budget, and attempt cap, and releases leases on
shutdown.

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
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("build agent runner: %w", err)
	}

	systemActor, err := audit.SystemActor("worker")
	if err != nil {
		return err
	}
	caseService, err := cases.NewService(app.Pool, timeSource, systemActor,
		cases.WithMaxAttempts(cfg.Worker.MaxAttempts))
	if err != nil {
		return fmt.Errorf("build case service: %w", err)
	}

	workerID, err := worker.NewID()
	if err != nil {
		return err
	}
	pollWorker, err := worker.New(worker.Options{
		ID:      workerID,
		Config:  cfg.Worker,
		Cases:   caseService,
		Runner:  runner,
		Switch:  switchReader,
		Spend:   worker.DBSpend{Queryer: app.Pool},
		Clock:   timeSource,
		Sleeper: worker.TimerSleeper{},
		Logger:  logger,
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
		"once", *once)

	if *once {
		err := pollWorker.ProcessOnce(ctx)
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
	if err := pollWorker.Run(ctx); err != nil {
		return err
	}
	logger.Info("worker stopped", "worker_id", pollWorker.ID())
	return nil
}
