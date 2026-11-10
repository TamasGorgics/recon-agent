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
                          --concurrency <n> --llm live|record|cassette
                          --max-cost-usd <decimal>
                          [--cassette-dir <path>] [--update-cassettes]
                          [--resume <eval_run_id>] [--dataset-dir <path>]
                   record = live execution plus cassette recording under
                          <cassette-dir>/<provider>/<model>/<prompt>/<sha256>.json
  report           render a Markdown + JSON eval report from durable results
                   usage: recon-eval report --run <eval_run_id>
                          [--dataset-dir <path>] [--json <path>|--out-dir <dir>]
                          [--appendix-limit <n>]
  matcher          build the standalone matcher component report
                   usage: recon-eval matcher --split dev|test|injection
                          [--dataset-dir <path>] [--json <path>|--out-dir <dir>]
  compare          compare two scored runs and flag gating regressions
                   usage: recon-eval compare <runA> <runB> [--report-only]
                          [--dataset-dir <path>] [--json <path>|--out-dir <dir>]
  gate             evaluate the M3-09 mission thresholds for two held-out test runs
                   usage: recon-eval gate --run <candidate_run_id>
                          --baseline <baseline_run_id> [--thresholds <path>]
                          [--dataset-dir <path>] [--json <path>|--out-dir <dir>]
                          [--report-only]
  help             print this help

Environment:
  DATABASE_URL       durable results database (eval_runs, eval_case_results)
  TEST_DATABASE_URL  optional sandbox maintenance DSN; falls back to DATABASE_URL
  OPENCODE_API_KEY   required only for --llm live or --llm record; never read from .env
  CONFIG_FILE        optional YAML config path (defaults < environment)

Exit codes: 0 completed or cost-cap stop, 1 failure, 2 usage error.
compare: 0 no gating regression, 1 gating regression or failure, 2 usage,
metadata mismatch, or incomplete run data; --report-only suppresses the gating
exit 1 and the incomplete exit 2, exiting 0 with warnings.
gate: 0 all applicable thresholds pass on complete runs, 1 threshold failure
or threshold/report load failure, 2 usage, metadata mismatch, or incomplete run
data; --report-only suppresses the gating exit 1 and the incomplete exit 2.
`

// commandDeps are process-boundary seams for commands that need an injectable
// loader. The run command keeps its own runner.Deps seam.
type commandDeps struct {
	Report  reportDeps
	Matcher matcherDeps
	Compare compareDeps
	Gate    gateDeps
}

func defaultCommandDeps() commandDeps {
	return commandDeps{
		Report:  defaultReportDeps(),
		Matcher: defaultMatcherDeps(),
		Compare: defaultCompareDeps(),
		Gate:    defaultGateDeps(),
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
	case "compare":
		return compareCommand(ctx, args[1:], stdout, stderr, deps.Compare)
	case "gate":
		return gateCommand(ctx, args[1:], stdout, stderr, deps.Gate)
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
