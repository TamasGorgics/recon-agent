package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// exitError carries an explicit process exit code through run. The tie-out
// CLI uses 2 for a non-zero unexplained difference, while all errors use 1.
type exitError struct {
	err  error
	code int
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }
func (e *exitError) ExitCode() int { return e.code }

// exitCode maps a command error to the process exit status: 0 for success,
// 2 for a tie-out difference, 1 for every other error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
}

func runTieout(ctx context.Context, args []string) error {
	return runTieoutTo(ctx, args, os.Stdout)
}

// runTieoutTo implements `recon tieout --scope UUID --date YYYY-MM-DD [--json]`.
// It is parameterised on the writer so tests can capture the table and JSON.
func runTieoutTo(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("tieout", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	scope := flags.String("scope", "", "reconciliation scope UUID")
	date := flags.String("date", "", "statement date, YYYY-MM-DD")
	asJSON := flags.Bool("json", false, "print stable JSON instead of a table")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("tieout: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tieout: unexpected argument %q", flags.Arg(0))
	}
	if *scope == "" || *date == "" {
		return errors.New("usage: recon tieout --scope <uuid> --date YYYY-MM-DD [--json]")
	}
	asOf, err := time.ParseInLocation("2006-01-02", *date, time.UTC)
	if err != nil {
		return fmt.Errorf("tieout: --date %q is not YYYY-MM-DD: %w", *date, err)
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

	result, err := tieout.Compute(ctx, pool, *scope, asOf)
	if err != nil {
		return err
	}
	if *asJSON {
		encoded, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("tieout: encode JSON: %w", err)
		}
		if _, err := fmt.Fprintf(out, "%s\n", encoded); err != nil {
			return fmt.Errorf("tieout: write JSON: %w", err)
		}
	} else if err := writeTieoutTable(out, result); err != nil {
		return fmt.Errorf("tieout: write table: %w", err)
	}
	if !result.Unexplained.IsZero() {
		return &exitError{
			code: 2,
			err:  fmt.Errorf("tie-out unexplained difference %s", result.Unexplained),
		}
	}
	return nil
}

func writeTieoutTable(out io.Writer, result tieout.Result) error {
	adjustedLedger, _ := result.LedgerBalance.Sub(result.LedgerOnly)
	adjustedStatement, _ := result.StatementClosing.Sub(result.ExternalOnly)

	var table strings.Builder
	fmt.Fprintf(&table, "Tie-out scope %s as of %s (%s)\n",
		result.ScopeID, result.AsOf.Format("2006-01-02"), result.Currency)
	fmt.Fprintf(&table, "  %-30s %s\n", "Ledger balance", result.LedgerBalance)
	fmt.Fprintf(&table, "  %-30s %s\n", fmt.Sprintf("Less ledger-only (%d)", result.LedgerOnlyCount), result.LedgerOnly)
	fmt.Fprintf(&table, "  %-30s %s\n", "Adjusted ledger", adjustedLedger)
	fmt.Fprintf(&table, "  %-30s %s\n", "Statement closing", result.StatementClosing)
	fmt.Fprintf(&table, "  %-30s %s\n", fmt.Sprintf("Less external-only (%d)", result.ExternalOnlyCount), result.ExternalOnly)
	fmt.Fprintf(&table, "  %-30s %s\n", "Adjusted statement", adjustedStatement)
	fmt.Fprintf(&table, "  %-30s %s\n", "Unexplained difference", result.Unexplained)
	if result.Unexplained.IsZero() {
		table.WriteString("Result: TIED\n")
	} else {
		table.WriteString("Result: UNEXPLAINED DIFFERENCE\n")
	}
	_, err := io.WriteString(out, table.String())
	return err
}
