package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/TamasGorgics/recon-agent/internal/demo"
)

// runDemo implements `recon demo seed` and `recon demo ops`.
func runDemo(ctx context.Context, args []string) error {
	return runDemoTo(ctx, args, os.Stdout)
}

// runDemoTo is runDemo with an explicit writer, so the help text and summary
// can be captured by tests.
func runDemoTo(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: recon demo <seed|ops> [--seed N] [--days N] [--difficulty easy|realistic|adversarial] [--reset]")
	}
	switch args[0] {
	case "seed":
		return runDemoCommand(ctx, "seed", args[1:], out)
	case "ops":
		return runDemoCommand(ctx, "ops", args[1:], out)
	default:
		return fmt.Errorf("unknown demo command %q (usage: recon demo <seed|ops>)", args[0])
	}
}

type demoFlags struct {
	seed       int64
	days       int
	difficulty string
	reset      bool
}

func runDemoCommand(ctx context.Context, mode string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("demo "+mode, flag.ContinueOnError)
	flags.SetOutput(out)
	fs := demoFlags{}
	flags.Int64Var(&fs.seed, "seed", demo.DefaultSeed, "deterministic dataset seed")
	flags.IntVar(&fs.days, "days", demo.DefaultDays, "number of calendar days to generate")
	flags.StringVar(&fs.difficulty, "difficulty", demo.DefaultDifficulty, "easy, realistic, or adversarial")
	flags.BoolVar(&fs.reset, "reset", false, "drop and recreate the public schema, then migrate (dev only)")
	flags.Usage = func() {
		_, _ = fmt.Fprintf(out, "usage: recon demo %s [flags]\n", mode)
		_, _ = fmt.Fprintln(out)
		switch mode {
		case "ops":
			_, _ = fmt.Fprintln(out, "Run the deterministic M1 dataset, then add the synthetic M5-04 ops")
			_, _ = fmt.Fprintln(out, "rows used by the operations dashboard: agent runs, proposals,")
			_, _ = fmt.Fprintln(out, "approvals, applied proposals, classified cases, and break-aging fixtures.")
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintln(out, "This is synthetic dashboard demo evidence, not a production pipeline run.")
			_, _ = fmt.Fprintln(out, "It uses no network and no LLM calls.")
		default:
			_, _ = fmt.Fprintln(out, "Generate a deterministic dataset, ingest it, run the matcher, open breaks")
			_, _ = fmt.Fprintln(out, "and cases, and print the M1 summary table.")
		}
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Flags:")
		flags.VisitAll(func(f *flag.Flag) {
			_, _ = fmt.Fprintf(out, "  --%-10s %s", f.Name, f.Usage)
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				_, _ = fmt.Fprintf(out, " (default %s)", f.DefValue)
			}
			_, _ = fmt.Fprintln(out)
		})
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("demo %s: %w", mode, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("demo %s: unexpected argument %q", mode, flags.Arg(0))
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if fs.reset {
		if err := demo.Reset(ctx, cfg.Database.URL); err != nil {
			return err
		}
	}

	opts := demo.Options{
		DBURL:      cfg.Database.URL,
		Seed:       fs.seed,
		Days:       fs.days,
		Difficulty: fs.difficulty,
		Synth:      cfg.Synth,
		Match:      cfg.Match,
		Ledger:     cfg.Ledger,
		Sources:    cfg.Ingest.Sources,
	}
	if mode == "ops" {
		summary, err := demo.RunOps(ctx, opts)
		if err != nil {
			return err
		}
		return summary.Write(out)
	}
	summary, err := demo.Run(ctx, opts)
	if err != nil {
		return err
	}
	return summary.Write(out)
}
