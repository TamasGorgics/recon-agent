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

	"github.com/TamasGorgics/recon-agent/eval/gate"
	"github.com/TamasGorgics/recon-agent/eval/report"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

const GateUsage = `usage: recon-eval gate --run <candidate_run_id> --baseline <baseline_run_id>
                     [--thresholds <path>]
                     [--dataset-dir <path>]
                     [--json <path> | --out-dir <dir>]
                     [--report-only]

Evaluate the committed M3-09 mission thresholds against one scored candidate
run and one baseline run on the held-out test split. Both runs must be
standalone (not repeat-group members), on thresholds.baseline_split ("test"),
and share suite "agent", dataset_version, seed, model and prompt version. A
dev or injection run is rejected with exit 2 before evaluation.
Cross-model/prompt comparison is recon-eval compare, not the gate.

The threshold file defaults to <module root>/eval/thresholds.yaml. T0 false-fix
is not applicable until M4-14 and is never counted as a pass. Injection
resistance is gated on the injection split only; on dev/test it is reported
not applicable (dedicated split required, M6-02/M6-05). Calibration is reported
but never gated.

Markdown goes to stdout by default. --json writes JSON to exactly that path and
still prints Markdown. --out-dir writes gate.md and gate.json and suppresses
stdout. Parent directories must already exist; files are written 0644.

Exit codes: 0 all applicable thresholds pass on complete runs, 1 threshold
failure or threshold/report load failure, 2 usage, metadata mismatch, or
incomplete run data. Incomplete dominates threshold failure. --report-only
suppresses the gating exit 1 and the incomplete exit 2, marks the footer as
informational, and never suppresses usage, metadata, load or write failures.
`

type gateDeps struct {
	OpenSource     func(ctx context.Context) (report.Source, func(), error)
	LoadThresholds func(path string) (gate.Thresholds, error)
	Run            func(ctx context.Context, src report.Source, candidateID, baselineID string,
		thresholds gate.Thresholds, opts gate.Options) (gate.Document, error)
	ModuleRoot string
	Clock      clock.Clock
}

func defaultGateDeps() gateDeps {
	return gateDeps{
		OpenSource:     openGateSource,
		LoadThresholds: gate.LoadThresholds,
		Run:            gate.Run,
		Clock:          clock.Real{},
	}
}

func openGateSource(ctx context.Context) (report.Source, func(), error) {
	dsn := strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
	if dsn == "" {
		return nil, nil, errors.New("DATABASE_URL is required for gate")
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

type gateArgs struct {
	run        string
	baseline   string
	thresholds string
	datasetDir string
	jsonPath   string
	outDir     string
	reportOnly bool
}

func parseGateArgs(args []string, moduleRoot string) (gateArgs, error) {
	fs := flag.NewFlagSet("recon-eval gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var (
		parsed     gateArgs
		thresholds string
	)
	fs.StringVar(&parsed.run, "run", "", "candidate eval run ID (UUID)")
	fs.StringVar(&parsed.baseline, "baseline", "", "baseline eval run ID (UUID)")
	fs.StringVar(&thresholds, "thresholds", "", "threshold YAML path")
	fs.StringVar(&parsed.datasetDir, "dataset-dir", "", "committed dataset directory")
	fs.StringVar(&parsed.jsonPath, "json", "", "write JSON to this exact path")
	fs.StringVar(&parsed.outDir, "out-dir", "", "write gate.md and gate.json in this directory")
	fs.BoolVar(&parsed.reportOnly, "report-only", false, "report without gating exit codes")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return gateArgs{}, flag.ErrHelp
		}
		return gateArgs{}, err
	}
	if fs.NArg() != 0 {
		return gateArgs{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	parsed.run = strings.ToLower(strings.TrimSpace(parsed.run))
	parsed.baseline = strings.ToLower(strings.TrimSpace(parsed.baseline))
	if parsed.run == "" {
		return gateArgs{}, errors.New("--run is required")
	}
	if parsed.baseline == "" {
		return gateArgs{}, errors.New("--baseline is required")
	}
	if !reportUUIDPattern.MatchString(parsed.run) {
		return gateArgs{}, fmt.Errorf("--run %q is not a UUID", parsed.run)
	}
	if !reportUUIDPattern.MatchString(parsed.baseline) {
		return gateArgs{}, fmt.Errorf("--baseline %q is not a UUID", parsed.baseline)
	}
	if parsed.jsonPath != "" && parsed.outDir != "" {
		return gateArgs{}, errors.New("--json and --out-dir are mutually exclusive")
	}
	if thresholds == "" {
		path, err := gate.DefaultThresholdsPath(moduleRoot)
		if err != nil {
			return gateArgs{}, err
		}
		thresholds = path
	}
	parsed.thresholds = thresholds
	return parsed, nil
}

func gateCommand(ctx context.Context, args []string, stdout, stderr io.Writer,
	deps gateDeps) int {
	parsed, err := parseGateArgs(args, deps.ModuleRoot)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stderr, GateUsage)
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 2
	}
	if code := checkGateOutputPaths(parsed, stderr); code != 0 {
		return code
	}

	thresholds, err := deps.LoadThresholds(parsed.thresholds)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 1
	}
	source, closeSource, err := deps.OpenSource(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 1
	}
	defer closeSource()

	document, err := deps.Run(ctx, source, parsed.run, parsed.baseline, thresholds,
		gate.Options{
			DatasetDir:     parsed.datasetDir,
			ModuleRoot:     deps.ModuleRoot,
			Clock:          deps.Clock,
			ThresholdsPath: parsed.thresholds,
			ReportOnly:     parsed.reportOnly,
		})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		if errors.Is(err, gate.ErrMetadataMismatch) ||
			errors.Is(err, report.ErrCompareMismatch) {
			return 2
		}
		return 1
	}

	markdown, err := gate.RenderMarkdown(document)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: render gate Markdown: %v\n", err)
		return 1
	}
	jsonBytes, err := gate.RenderJSON(document)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: render gate JSON: %v\n", err)
		return 1
	}

	switch {
	case parsed.outDir != "":
		markdownPath := filepath.Join(parsed.outDir, "gate.md")
		jsonOut := filepath.Join(parsed.outDir, "gate.json")
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
	return document.Exit.Code
}

func checkGateOutputPaths(parsed gateArgs, stderr io.Writer) int {
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
