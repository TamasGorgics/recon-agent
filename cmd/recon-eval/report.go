package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/report"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

const ReportUsage = `usage: recon-eval report --run <eval_run_id>
                      [--dataset-dir <path>]
                      [--json <path> | --out-dir <dir>]
                      [--appendix-limit <n>]

Render the Markdown eval report to stdout by default. --json writes JSON to
exactly that path and still prints Markdown. --out-dir writes report.md and
report.json and suppresses stdout. Parent directories must already exist.

The report rebuilds and verifies the committed dataset and uses DATABASE_URL as
the durable results database. Transcript content is limited to the sanitized
snapshot v1; snapshot sanitization is not full PII/PAN scrubbing.
`

var reportUUIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// reportDeps is the command seam. Tests inject a fixture source/build so the
// CLI usage and exit-code behavior never needs a database.
type reportDeps struct {
	OpenSource func(ctx context.Context) (report.Source, func(), error)
	Build      func(ctx context.Context, src report.Source, runID string,
		opts report.Options) (report.Report, error)
	ModuleRoot string
	Clock      clock.Clock
}

func defaultReportDeps() reportDeps {
	return reportDeps{
		OpenSource: openReportSource,
		Build:      report.Build,
		Clock:      clock.Real{},
	}
}

func openReportSource(ctx context.Context) (report.Source, func(), error) {
	dsn := strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
	if dsn == "" {
		return nil, nil, errors.New("DATABASE_URL is required for report")
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

func reportCommand(ctx context.Context, args []string, stdout, stderr io.Writer,
	deps reportDeps) int {
	fs := flag.NewFlagSet("recon-eval report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, ReportUsage) }

	var (
		runID         string
		datasetDir    string
		jsonPath      string
		outDir        string
		appendixLimit int
	)
	fs.StringVar(&runID, "run", "", "eval run ID (UUID)")
	fs.StringVar(&datasetDir, "dataset-dir", "", "committed dataset directory")
	fs.StringVar(&jsonPath, "json", "", "write JSON to this exact path")
	fs.StringVar(&outDir, "out-dir", "", "write report.md and report.json in this directory")
	fs.IntVar(&appendixLimit, "appendix-limit", report.DefaultAppendixLimit,
		"maximum failure appendix rows")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "recon-eval: unexpected arguments: %s\n",
			strings.Join(fs.Args(), " "))
		return 2
	}
	runID = strings.ToLower(strings.TrimSpace(runID))
	if runID == "" {
		_, _ = fmt.Fprintln(stderr, "recon-eval: --run is required")
		return 2
	}
	if !reportUUIDPattern.MatchString(runID) {
		_, _ = fmt.Fprintf(stderr, "recon-eval: --run %q is not a UUID\n", runID)
		return 2
	}
	if jsonPath != "" && outDir != "" {
		_, _ = fmt.Fprintln(stderr, "recon-eval: --json and --out-dir are mutually exclusive")
		return 2
	}
	if appendixLimit < 1 {
		_, _ = fmt.Fprintln(stderr, "recon-eval: --appendix-limit must be at least 1")
		return 2
	}
	if outDir != "" {
		info, err := os.Stat(outDir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: --out-dir %q: %v\n", outDir, err)
			return 1
		}
		if !info.IsDir() {
			_, _ = fmt.Fprintf(stderr, "recon-eval: --out-dir %q is not a directory\n", outDir)
			return 1
		}
	}
	if jsonPath != "" {
		parent := filepath.Dir(jsonPath)
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

	source, closeSource, err := deps.OpenSource(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 1
	}
	defer closeSource()

	opts := report.Options{
		DatasetDir:    datasetDir,
		ModuleRoot:    deps.ModuleRoot,
		Clock:         deps.Clock,
		AppendixLimit: appendixLimit,
	}
	built, err := deps.Build(ctx, source, runID, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 1
	}
	markdown, err := report.RenderMarkdown(built)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: render Markdown: %v\n", err)
		return 1
	}
	jsonBytes, err := report.RenderJSON(built)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: render JSON: %v\n", err)
		return 1
	}

	switch {
	case outDir != "":
		markdownPath := filepath.Join(outDir, "report.md")
		jsonOut := filepath.Join(outDir, "report.json")
		if err := os.WriteFile(markdownPath, markdown, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: write %s: %v\n", markdownPath, err)
			return 1
		}
		if err := os.WriteFile(jsonOut, jsonBytes, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: write %s: %v\n", jsonOut, err)
			return 1
		}
	case jsonPath != "":
		if err := os.WriteFile(jsonPath, jsonBytes, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "recon-eval: write %s: %v\n", jsonPath, err)
			return 1
		}
		_, _ = stdout.Write(markdown)
	default:
		_, _ = stdout.Write(markdown)
	}
	return 0
}
