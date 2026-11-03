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

// runDemo implements `recon demo seed`: the M1 end-to-end pipeline.
func runDemo(ctx context.Context, args []string) error {
	return runDemoTo(ctx, args, os.Stdout)
}

// runDemoTo is runDemo with an explicit writer, so the help text and summary
// can be captured by tests.
func runDemoTo(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "seed" {
		return errors.New("usage: recon demo seed [--seed N] [--days N] [--difficulty easy|realistic|adversarial] [--reset]")
	}
	flags := flag.NewFlagSet("demo seed", flag.ContinueOnError)
	flags.SetOutput(out)
	seed := flags.Int64("seed", demo.DefaultSeed, "deterministic dataset seed")
	days := flags.Int("days", demo.DefaultDays, "number of calendar days to generate")
	difficulty := flags.String("difficulty", demo.DefaultDifficulty, "easy, realistic, or adversarial")
	reset := flags.Bool("reset", false, "drop and recreate the public schema, then migrate (dev only)")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(out, "usage: recon demo seed [flags]")
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Generate a deterministic dataset, ingest it, run the matcher, open breaks")
		_, _ = fmt.Fprintln(out, "and cases, and print the M1 summary table.")
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
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("demo seed: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("demo seed: unexpected argument %q", flags.Arg(0))
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *reset {
		if err := demo.Reset(ctx, cfg.Database.URL); err != nil {
			return err
		}
	}
	summary, err := demo.Run(ctx, demo.Options{
		DBURL:      cfg.Database.URL,
		Seed:       *seed,
		Days:       *days,
		Difficulty: *difficulty,
		Synth:      cfg.Synth,
		Match:      cfg.Match,
		Ledger:     cfg.Ledger,
		Sources:    cfg.Ingest.Sources,
	})
	if err != nil {
		return err
	}
	return summary.Write(out)
}
