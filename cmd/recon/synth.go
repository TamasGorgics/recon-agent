package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/synth"
)

// runSynth implements `recon synth generate` and `recon synth load`.
//
// generate is deliberately DB-free: it loads config but does not require
// database.url and never opens a pool. load is the only path that connects.
func runSynth(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: recon synth <generate|load> [flags]")
	}
	switch args[0] {
	case "generate":
		return runSynthGenerate(args[1:])
	case "load":
		return runSynthLoad(ctx, args[1:])
	default:
		return fmt.Errorf("unknown synth command %q (usage: recon synth <generate|load>)", args[0])
	}
}

func runSynthGenerate(args []string) error {
	flags := flag.NewFlagSet("synth generate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	seed := flags.Int64("seed", 1, "deterministic dataset seed")
	days := flags.Int("days", 7, "number of calendar days to generate")
	out := flags.String("out", "", "output directory")
	difficulty := flags.String("difficulty", synth.DifficultyEasy, "easy, realistic, or adversarial")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("synth generate: %w", err)
	}
	if *out == "" {
		return errors.New("usage: recon synth generate --seed N --days N --out DIR [--difficulty easy|realistic|adversarial]")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("synth generate: unexpected argument %q", flags.Arg(0))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	generator := synth.New(clock.Real{})
	dataset, err := generator.Generate(synth.Options{
		Seed:       *seed,
		Days:       *days,
		Difficulty: *difficulty,
		Config:     cfg.Synth,
	})
	if err != nil {
		return err
	}
	if err := dataset.Write(*out); err != nil {
		return err
	}
	fmt.Printf("synth generate: seed=%d days=%d difficulty=%s dataset_version=%s files=%d out=%s\n",
		dataset.Manifest.Seed, dataset.Manifest.Days, dataset.Manifest.Difficulty,
		dataset.Manifest.DatasetVersion, len(dataset.Files), *out)
	return nil
}

func runSynthLoad(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("synth load", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", "", "generated dataset directory")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("synth load: %w", err)
	}
	if *dir == "" {
		return errors.New("usage: recon synth load --dir DIR")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("synth load: unexpected argument %q", flags.Arg(0))
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()

	actor, err := audit.SystemActor("synth")
	if err != nil {
		return err
	}
	result, err := synth.Load(ctx, synth.LoadOptions{
		Dir:     *dir,
		DB:      pool,
		Clock:   clock.Real{},
		Actor:   actor,
		Sources: cfg.Ingest,
		Ledger:  cfg.Ledger,
	})
	if err != nil {
		return err
	}
	fmt.Printf("synth load: accounts=%d fee_schedules=%d scopes=%d ledger_posted=%d ledger_replayed=%d files=%d no_op_files=%d records_inserted=%d quarantined=%d\n",
		result.Accounts, result.FeeSchedules, result.Scopes, result.LedgerPosted,
		result.LedgerReplayed, result.Files, result.NoOpFiles, result.RecordsInserted,
		result.Quarantined)
	if result.Quarantined != 0 {
		for _, reason := range result.QuarantineReasons {
			fmt.Printf("synth load: quarantined: %s\n", reason)
		}
		return fmt.Errorf("synth load: %d quarantined batch(es)", result.Quarantined)
	}
	return nil
}
