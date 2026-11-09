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
  report           render a Markdown + JSON eval report from durable results
                   usage: recon-eval report --run <eval_run_id>
                          [--dataset-dir <path>] [--json <path>|--out-dir <dir>]
                          [--appendix-limit <n>]
  matcher          build the standalone matcher component report
                   usage: recon-eval matcher --split dev|test|injection
                          [--dataset-dir <path>] [--json <path>|--out-dir <dir>]
  help             print this help

Environment:
  DATABASE_URL       durable results database (eval_runs, eval_case_results)
  TEST_DATABASE_URL  optional sandbox maintenance DSN; falls back to DATABASE_URL
  OPENCODE_API_KEY   required only for --llm live; never read from .env
  CONFIG_FILE        optional YAML config path (defaults < environment)

Exit codes: 0 completed or cost-cap stop, 1 failure, 2 usage error.
`

// commandDeps are process-boundary seams for commands that need an injectable
// loader. The run command keeps its own runner.Deps seam.
type commandDeps struct {
	Report  reportDeps
	Matcher matcherDeps
}

func defaultCommandDeps() commandDeps {
	return commandDeps{
		Report:  defaultReportDeps(),
		Matcher: defaultMatcherDeps(),
	}
}

func main() {
	os.Exit(Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// Main is the testable command entry point.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return mainWithDeps(ctx, args, stdout, stderr, defaultCommandDeps())
}

func mainWithDeps(ctx context.Context, args []string, stdout, stderr io.Writer,
	deps commandDeps) int {
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
	case "report":
		return reportCommand(ctx, args[1:], stdout, stderr, deps.Report)
	case "matcher":
		return matcherCommand(ctx, args[1:], stdout, stderr, deps.Matcher)
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
