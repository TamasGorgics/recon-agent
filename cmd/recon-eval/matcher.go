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

	"github.com/TamasGorgics/recon-agent/eval/component"
	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const MatcherUsage = `usage: recon-eval matcher --split dev|test|injection
                         [--dataset-dir <path>]
                         [--json <path> | --out-dir <dir>]

Build the standalone matcher component report (matcher-report/v1). The command
creates a disposable eval sandbox, runs the production matcher, and reads active
matches through the recon_app role. It writes no eval_runs/eval_case_results
rows. Recall below 1.0 is a finding, not a command failure.

Markdown goes to stdout by default. --json writes JSON to exactly that path and
still prints Markdown. --out-dir writes matcher.md and matcher.json and
suppresses stdout. Parent directories must already exist.

The sandbox admin DSN is TEST_DATABASE_URL, falling back to DATABASE_URL.
Synthetic eval data only.
`

// matcherDeps is the command seam. Tests inject a fixture build so CLI usage
// and exit codes never need a database.
type matcherDeps struct {
	Build      func(ctx context.Context, opts component.Options) (component.MatcherReport, error)
	ModuleRoot string
	Clock      clock.Clock
}

func defaultMatcherDeps() matcherDeps {
	return matcherDeps{Build: component.Build, Clock: clock.Real{}}
}

func matcherCommand(ctx context.Context, args []string, stdout, stderr io.Writer,
	deps matcherDeps) int {
	fs := flag.NewFlagSet("recon-eval matcher", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, MatcherUsage) }

	var (
		split      string
		datasetDir string
		jsonPath   string
		outDir     string
	)
	fs.StringVar(&split, "split", "", "dataset split (dev, test, injection)")
	fs.StringVar(&datasetDir, "dataset-dir", "", "committed dataset directory")
	fs.StringVar(&jsonPath, "json", "", "write JSON to this exact path")
	fs.StringVar(&outDir, "out-dir", "", "write matcher.md and matcher.json in this directory")
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
	split = strings.ToLower(strings.TrimSpace(split))
	if split == "" {
		_, _ = fmt.Fprintln(stderr, "recon-eval: --split is required")
		return 2
	}
	parsedSplit := dataset.Split(split)
	if !parsedSplit.Valid() {
		_, _ = fmt.Fprintf(stderr, "recon-eval: --split %q is not dev, test, or injection\n", split)
		return 2
	}
	if jsonPath != "" && outDir != "" {
		_, _ = fmt.Fprintln(stderr, "recon-eval: --json and --out-dir are mutually exclusive")
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

	built, err := deps.Build(ctx, component.Options{
		Split:      parsedSplit,
		DatasetDir: datasetDir,
		ModuleRoot: deps.ModuleRoot,
		Clock:      deps.Clock,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: %v\n", err)
		return 1
	}
	markdown, err := component.RenderMarkdown(built)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: render matcher Markdown: %v\n", err)
		return 1
	}
	jsonBytes, err := component.RenderJSON(built)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recon-eval: render matcher JSON: %v\n", err)
		return 1
	}

	switch {
	case outDir != "":
		markdownPath := filepath.Join(outDir, "matcher.md")
		jsonOut := filepath.Join(outDir, "matcher.json")
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
