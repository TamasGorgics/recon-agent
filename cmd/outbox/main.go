// Command outbox runs the Slack approval outbox worker. It opens only the
// recon_app pool, registers the three approval topics plus integrity.failed,
// and delivers due messages through the Slack Web API until SIGINT/SIGTERM. It
// deliberately has no agent or LLM dependency so outbox deployments do not need
// model credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/TamasGorgics/recon-agent/internal/approval/slack"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/metrics"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

const usage = `usage: outbox [--once]

Runs the Slack approval outbox worker until SIGINT/SIGTERM. The worker claims
due outbox rows for approval.request, approval.update, approval.cancel, and
integrity.failed and delivers them through the Slack Web API.

Deploy this binary with the integrity.failed handler before the worker
scheduler starts producing daily integrity runs.

Flags:
  --once   process at most one due batch and exit

Configuration: CONFIG_FILE YAML plus environment overlays. Requires
DATABASE_APP_URL (recon_app), SLACK_BOT_TOKEN, SLACK_CHANNEL_ID, and
SLACK_CASE_FILE_BASE_URL. No agent DSN or LLM key is required.
`

// appPool is the dependency surface shared by the outbox worker (short
// transactions) and the Slack handlers (single-statement reads/writes). The
// production value is the recon_app *pgxpool.Pool.
type appPool interface {
	db.Beginner
	slack.Queryer
}

// dependencies collects process boundaries so tests can construct and run the
// command without a live database or Slack. The zero value is not usable;
// productionDependencies supplies the real implementations.
type dependencies struct {
	// openApp opens the recon_app pool and returns its cleanup function.
	openApp func(ctx context.Context, dsn string) (appPool, func(), error)
	// httpClient is the Slack HTTP transport. Nil uses the client's default
	// 10-second *http.Client.
	httpClient *http.Client
	// slackBaseURL overrides the Slack Web API base URL in tests. Empty uses
	// slack.DefaultBaseURL.
	slackBaseURL string
	// clock supplies handler case age and worker lease/backoff time.
	clock clock.Clock
}

func productionDependencies() dependencies {
	return dependencies{
		openApp: func(ctx context.Context, dsn string) (appPool, func(), error) {
			role, err := db.OpenRole(ctx, db.RoleApp, dsn)
			if err != nil {
				return nil, nil, err
			}
			return role.Pool, role.Close, nil
		},
		clock: clock.Real{},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "outbox: %v\n", err)
		os.Exit(1)
	}
}

// run is main's testable entry point. It parses flags, loads and validates the
// outbox runtime configuration, opens the recon_app pool, builds the worker,
// and delivers until the context is cancelled or one batch completes.
func run(ctx context.Context, args []string, out io.Writer) error {
	return runWith(ctx, args, out, productionDependencies())
}

func runWith(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	flags := flag.NewFlagSet("outbox", flag.ContinueOnError)
	flags.SetOutput(out)
	once := flags.Bool("once", false, "process at most one due batch and exit")
	flags.Usage = func() {
		_, _ = fmt.Fprint(out, usage)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateOutboxRuntime(); err != nil {
		return err
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(cfg.Log.Level)); err != nil {
		return fmt.Errorf("invalid log level %q", cfg.Log.Level)
	}
	logger := telemetry.NewLogger(out, level)

	pool, closePool, err := deps.openApp(ctx, cfg.Database.AppURL)
	if err != nil {
		return fmt.Errorf("open recon_app pool: %w", err)
	}
	if closePool != nil {
		defer closePool()
	}

	processMetrics, err := metrics.New(metrics.Options{
		Component:        "outbox",
		GoCollector:      true,
		ProcessCollector: true,
		Queryer:          pool,
		GatherTimeout:    cfg.Metrics.GatherTimeout,
		Logger:           logger,
	})
	if err != nil {
		return fmt.Errorf("build metrics registry: %w", err)
	}

	runtime, err := buildWorker(buildOptions{
		Config:       cfg,
		Pool:         pool,
		HTTPClient:   deps.httpClient,
		SlackBaseURL: deps.slackBaseURL,
		Clock:        deps.clock,
		Logger:       logger,
		Metrics:      processMetrics,
	})
	if err != nil {
		return err
	}

	logger.Info("outbox worker starting",
		"once", *once,
		"topics", runtime.topics(),
		"batch_size", runtime.options.BatchSize,
		"lease", runtime.options.Lease.String(),
		"handler_timeout", runtime.options.HandlerTimeout.String())

	metricsServer, err := startOutboxMetrics(ctx, cfg, logger, processMetrics, *once)
	if err != nil {
		return err
	}
	if metricsServer != nil {
		defer func() {
			if closeErr := metricsServer.Close(); closeErr != nil {
				logger.Error("outbox metrics listener shutdown failed", "error", closeErr)
			}
		}()
	}

	if *once {
		processed, err := runtime.worker.Process(ctx)
		if err != nil {
			logger.Error("outbox worker: process failed", "error", err)
			return err
		}
		logger.Info("outbox worker stopped", "once", true, "processed", processed)
		return nil
	}
	if err := runtime.worker.Run(ctx); err != nil {
		return err
	}
	logger.Info("outbox worker stopped", "once", false)
	return nil
}

// buildOptions is the dependency set for the worker construction. It is
// separate from dependencies so tests can build the worker without parsing
// flags or opening a pool.
type buildOptions struct {
	Config       config.Config
	Pool         appPool
	HTTPClient   *http.Client
	SlackBaseURL string
	Clock        clock.Clock
	Logger       *slog.Logger
	// Metrics is the process registry. Nil leaves the worker's no-op metrics
	// recorder in place; the delivered/failed/dead-letter adapter is built
	// here so the outbox package keeps no metrics import.
	Metrics *metrics.Metrics
}

// workerRuntime is the constructed worker plus the fields tests and logging
// need. The handler map and options are kept as the exact values passed to
// outbox.NewWorker; the worker copies them internally.
type workerRuntime struct {
	worker   *outbox.Worker
	client   *slack.Client
	handlers map[string]outbox.Handler
	options  outbox.WorkerOptions
}

// buildWorker constructs the Slack client, the four outbox handlers, and the
// worker. It validates the outbox runtime configuration again so a caller
// cannot construct a worker with an incomplete configuration. It performs no
// network or database call.
func buildWorker(opts buildOptions) (*workerRuntime, error) {
	if err := opts.Config.ValidateOutboxRuntime(); err != nil {
		return nil, err
	}
	if opts.Pool == nil {
		return nil, errors.New("outbox: recon_app pool is required")
	}

	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	baseURL := strings.TrimSpace(opts.SlackBaseURL)
	if baseURL == "" {
		baseURL = slack.DefaultBaseURL
	}

	client, err := slack.NewClient(slack.ClientOptions{
		BaseURL:    baseURL,
		BotToken:   opts.Config.Slack.BotToken,
		HTTPClient: opts.HTTPClient,
		Timeout:    slack.DefaultHTTPTimeout,
		Clock:      clk,
	})
	if err != nil {
		return nil, fmt.Errorf("build slack client: %w", err)
	}

	handlers, err := slack.NewHandlerMap(slack.HandlersOptions{
		Queryer:         opts.Pool,
		Client:          client,
		ChannelID:       opts.Config.Slack.ChannelID,
		CaseFileBaseURL: opts.Config.Slack.CaseFileBaseURL,
		Clock:           clk,
		Logger:          logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build slack handlers: %w", err)
	}

	// Reuse the outbox defaults explicitly: bounded batch and lease plus the
	// 30-second handler timeout documented as sufficient for the Slack budget.
	workerOptions := outbox.WorkerOptions{
		Pool:           opts.Pool,
		Handlers:       handlers,
		Clock:          clk,
		BatchSize:      outbox.DefaultBatchSize,
		Lease:          outbox.DefaultLease,
		MaxAttempts:    outbox.DefaultMaxAttempts,
		BaseDelay:      outbox.DefaultBaseDelay,
		MaxDelay:       outbox.DefaultMaxDelay,
		PollInterval:   outbox.DefaultPollInterval,
		HandlerTimeout: outbox.DefaultHandlerTimeout,
		Logger:         logger,
	}
	if opts.Metrics != nil {
		workerOptions.Metrics = metrics.NewOutboxMetrics(opts.Metrics)
	}
	worker, err := outbox.NewWorker(workerOptions)
	if err != nil {
		return nil, fmt.Errorf("build outbox worker: %w", err)
	}

	return &workerRuntime{
		worker:   worker,
		client:   client,
		handlers: handlers,
		options:  workerOptions,
	}, nil
}

// topics returns the registered topic names in stable order for startup logs
// and tests. It never includes payloads or secrets.
func (r *workerRuntime) topics() []string {
	names := make([]string, 0, len(r.handlers))
	for topic := range r.handlers {
		names = append(names, topic)
	}
	sort.Strings(names)
	return names
}

// startOutboxMetrics starts the outbox's loopback /metrics listener for the
// long-running process. --once is short-lived and must not bind the port, so it
// returns nil without touching the configured address.
func startOutboxMetrics(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	m *metrics.Metrics,
	once bool,
) (*metrics.Server, error) {
	if once {
		return nil, nil
	}
	return metrics.StartForProcess(ctx, m, cfg.Metrics.OutboxAddr, "metrics.outbox_addr", logger,
		metrics.ServerOptions{
			ReadHeaderTimeout: cfg.Metrics.ReadHeaderTimeout,
			ShutdownTimeout:   cfg.Metrics.ShutdownTimeout,
		})
}
