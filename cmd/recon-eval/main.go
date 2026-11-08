package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TamasGorgics/recon-agent/eval/runner"
)

const usage = `usage: recon-eval <command> [arguments]

Commands:
  run              run the M3 agent eval on one committed dataset split
                   usage: recon-eval run --suite agent --split dev|test|injection
                          --model <id> --prompt <version> --seed <n> --repeat <k>
                          --concurrency <n> --llm live|cassette
                          --max-cost-usd <decimal> [--resume <eval_run_id>]
                          [--dataset-dir <path>]
  help             print this help

Environment:
  DATABASE_URL       durable results database (eval_runs, eval_case_results)
  TEST_DATABASE_URL  optional sandbox maintenance DSN; falls back to DATABASE_URL
  OPENCODE_API_KEY   required only for --llm live; never read from .env
  CONFIG_FILE        optional YAML config path (defaults < environment)

Exit codes: 0 completed or cost-cap stop, 1 failure, 2 usage error.
`

func main() {
	os.Exit(Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// Main is the testable command entry point.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	case "run":
		return runCommand(ctx, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "recon-eval: unknown command %q (try: recon-eval help)\n", args[0])
		return 2
	}
}

func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := runner.ParseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	summary, err := runner.Run(ctx, opts, runner.Deps{Logger: logger})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		if runner.IsUsage(err) {
			return 2
		}
		return 1
	}
	_, _ = fmt.Fprint(stdout, summary.String())
	return 0
}
