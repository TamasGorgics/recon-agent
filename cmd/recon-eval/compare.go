package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/report"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

const CompareUsage = `usage: recon-eval compare <runA> <runB>
                      [--report-only]
                      [--dataset-dir <path>]
                      [--json <path> | --out-dir <dir>]

Compare two scored eval runs side by side. A is the baseline and B is the
candidate; every delta is B − A. Both runs must share suite, split and
dataset_version; different models and prompt versions are allowed. A repeat-group
member is compared as that named run only, with a warning.

Markdown goes to stdout by default. --json writes JSON to exactly that path and
still prints Markdown. --out-dir writes compare.md and compare.json and
suppresses stdout. Parent directories must already exist.

Gating regressions use disjoint 95% Wilson intervals: false-fix increase,
accuracy decrease, evidence-validity decrease, and correct-escalation decrease,
overall and per mode. Injection resistance and over-escalation are classified
but non-gating; cost, latency and steps are descriptive only.

A run that is partial, aborted, has no case results, or omits scoreable
scenarios makes the comparison incomplete. The report carries Build's
completeness warnings and the per-run complete/missing metadata; the command
fails closed instead of reporting a clean no-regression result.

Exit codes: 0 no gating regression on complete runs, 1 gating regression or
load/validation failure, 2 usage, metadata mismatch, or incomplete run data.
--report-only suppresses the gating exit 1 and the incomplete exit 2 and marks
the footer; it never suppresses usage, metadata, load or write failures.
`

type compareDeps struct {
	OpenSource func(ctx context.Context) (report.Source, func(), error)
	Compare    func(ctx context.Context, src report.Source, runA, runB string,
		opts report.CompareOptions) (report.Comparison, error)
	ModuleRoot string
	Clock      clock.Clock
}

func defaultCompareDeps() compareDeps {
	return compareDeps{
		OpenSource: openCompareSource,
		Compare:    report.Compare,
		Clock:      clock.Real{},
	}
}

func openCompareSource(ctx context.Context) (report.Source, func(), error) {
	dsn := strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
	if dsn == "" {
		return nil, nil, errors.New("DATABASE_URL is required for compare")
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolConfig.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("open results pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("ping results database: %w", err)
	}
	return report.NewPostgresSource(pool), pool.Close, nil
}

type compareArgs struct {
	runA       string
	runB       string
	reportOnly bool
	datasetDir string
	jsonPath   string
	outDir     string
}

func parseCompareArgs(args []string) (compareArgs, error) {
	var parsed compareArgs
	positionals := make([]string, 0, 2)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help":
			return compareArgs{}, flag.ErrHelp
		case arg == "--report-only":
			parsed.reportOnly = true
		case arg == "--dataset-dir" || strings.HasPrefix(arg, "--dataset-dir="):
			value, next, err := compareFlagValue(args, index, "--dataset-dir")
			if err != nil {
				return compareArgs{}, err
			}
			parsed.datasetDir = value
			index = next
		case arg == "--json" || strings.HasPrefix(arg, "--json="):
			value, next, err := compareFlagValue(args, index, "--json")
			if err != nil {
				return compareArgs{}, err
			}
			parsed.jsonPath = value
			index = next
		case arg == "--out-dir" || strings.HasPrefix(arg, "--out-dir="):
			value, next, err := compareFlagValue(args, index, "--out-dir")
			if err != nil {
				return compareArgs{}, err
			}
			parsed.outDir = value
			index = next
		case strings.HasPrefix(arg, "-") && arg != "-":
			return compareArgs{}, fmt.Errorf("flag provided but not defined: %s", arg)
		default:
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 2 {
		return compareArgs{}, fmt.Errorf("expected <runA> <runB>, got %d argument(s)",
			len(positionals))
	}
	parsed.runA = strings.ToLower(strings.TrimSpace(positionals[0]))
	parsed.runB = strings.ToLower(strings.TrimSpace(positionals[1]))
	if !reportUUIDPattern.MatchString(parsed.runA) {
		return compareArgs{}, fmt.Errorf("runA %q is not a UUID", positionals[0])
	}
	if !reportUUIDPattern.MatchString(parsed.runB) {
		return compareArgs{}, fmt.Errorf("runB %q is not a UUID", positionals[1])
	}
	if parsed.jsonPath != "" && parsed.outDir != "" {
		return compareArgs{}, errors.New("--json and --out-dir are mutually exclusive")
	}
	return parsed, nil
}

func compareFlagValue(args []string, index int, name string) (string, int, error) {
	arg := args[index]
	if strings.HasPrefix(arg, name+"=") {
		value := strings.TrimPrefix(arg, name+"=")
		if value == "" {
			return "", index, fmt.Errorf("%s requires a value", name)
		}
		return value, index, nil
	}
	if index+1 >= len(args) {
		return "", index, fmt.Errorf("%s requires a value", name)
	}
	return args[index+1], index + 1, nil
}

func compareCommand(ctx context.Context, args []string, stdout, stderr io.Writer,
	deps compareDeps) int {
	parsed, err := parseCompareArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stderr, CompareUsage)
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 2
	}
	if code := checkCompareOutputPaths(parsed, stderr); code != 0 {
		return code
	}

	source, closeSource, err := deps.OpenSource(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 1
	}
	defer closeSource()

	built, err := deps.Compare(ctx, source, parsed.runA, parsed.runB, report.CompareOptions{
		DatasetDir: parsed.datasetDir,
		ModuleRoot: deps.ModuleRoot,
		Clock:      deps.Clock,
		ReportOnly: parsed.reportOnly,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		if errors.Is(err, report.ErrCompareMismatch) {
			return 2
		}
		return 1
	}
	markdown, err := report.RenderCompareMarkdown(built)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: render compare Markdown: %v\n", err)
		return 1
	}
	jsonBytes, err := report.RenderCompareJSON(built)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: render compare JSON: %v\n", err)
		return 1
	}

	switch {
	case parsed.outDir != "":
		markdownPath := filepath.Join(parsed.outDir, "compare.md")
		jsonOut := filepath.Join(parsed.outDir, "compare.json")
		if err := os.WriteFile(markdownPath, markdown, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: write %s: %v\n", markdownPath, err)
			return 1
		}
		if err := os.WriteFile(jsonOut, jsonBytes, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: write %s: %v\n", jsonOut, err)
			return 1
		}
	case parsed.jsonPath != "":
		if err := os.WriteFile(parsed.jsonPath, jsonBytes, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: write %s: %v\n", parsed.jsonPath, err)
			return 1
		}
		_, _ = stdout.Write(markdown)
	default:
		_, _ = stdout.Write(markdown)
	}
	if built.ExitCriteria.Incomplete {
		_, _ = fmt.Fprintf(stderr,
			"recon-eval: warning: comparison includes incomplete run data (%s); "+
				"see the report warnings\n",
			strings.Join(built.ExitCriteria.IncompleteRuns, ", "))
	}
	if built.ExitCriteria.Incomplete && !parsed.reportOnly {
		return 2
	}
	if built.ExitCriteria.Failed && !parsed.reportOnly {
		return 1
	}
	return 0
}

func checkCompareOutputPaths(parsed compareArgs, stderr io.Writer) int {
	if parsed.outDir != "" {
		info, err := os.Stat(parsed.outDir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: --out-dir %q: %v\n", parsed.outDir, err)
			return 1
		}
		if !info.IsDir() {
			_, _ = fmt.Fprintf(stderr, "recon-eval: --out-dir %q is not a directory\n", parsed.outDir)
			return 1
		}
	}
	if parsed.jsonPath != "" {
		parent := filepath.Dir(parsed.jsonPath)
		info, err := os.Stat(parent)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: --json parent %q: %v\n", parent, err)
			return 1
		}
		if !info.IsDir() {
			_, _ = fmt.Fprintf(stderr, "recon-eval: --json parent %q is not a directory\n", parent)
			return 1
		}
	}
	return 0
}
