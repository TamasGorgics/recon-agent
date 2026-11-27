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

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/integrity"
)

const integrityUsage = `usage: recon integrity <command> [arguments]

Commands:
  run   run the five FIN-52 integrity checks once and persist one result row
        usage: recon integrity run [--date YYYY-MM-DD] [--json]
        --date sets run_date and the tie-out as-of upper bound only. Other
        checks are current-state checks; a backdated date is not a historical
        replay of matcher state.
        exit: 0 passed, 1 completed failed run, 2 usage/config/lock/DB/cancel error
`

// runIntegrity dispatches the `recon integrity` group.
func runIntegrity(ctx context.Context, args []string) error {
	return runIntegrityTo(ctx, args, os.Stdout)
}

// runIntegrityTo is runIntegrity with a testable writer and the production
// wall clock. Tests that need a deterministic default date call
// runIntegrityToWithClock directly.
func runIntegrityTo(ctx context.Context, args []string, out io.Writer) error {
	return runIntegrityToWithClock(ctx, args, out, clock.Real{})
}

func runIntegrityToWithClock(ctx context.Context, args []string, out io.Writer, clk clock.Clock) error {
	if len(args) == 0 {
		return &exitError{code: 2, err: errors.New(
			"integrity: missing command (try: recon integrity run)")}
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := fmt.Fprint(out, integrityUsage)
		return err
	case "run":
		return runIntegrityRunTo(ctx, args[1:], out, clk)
	default:
		return &exitError{code: 2, err: fmt.Errorf(
			"unknown integrity command %q (try: recon integrity run)", args[0])}
	}
}

// runIntegrityRunTo implements `recon integrity run [--date] [--json]`.
//
// The command opens the recon_app role deliberately: it is the same role the
// worker uses, so a successful run proves the production grants. It never
// falls back to the admin/owner DSN. A completed failed run exits 1; every
// usage/config/connection/lock/cancel error is exit 2 with no report claimed.
func runIntegrityRunTo(ctx context.Context, args []string, out io.Writer, clk clock.Clock) error {
	flags := flag.NewFlagSet("integrity run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	date := flags.String("date", "", "run date and tie-out as-of, YYYY-MM-DD")
	asJSON := flags.Bool("json", false, "print the stable report JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(out, integrityUsage)
			return writeErr
		}
		return integrityUsageError("%v", err)
	}
	if flags.NArg() != 0 {
		return integrityUsageError("unexpected argument %q", flags.Arg(0))
	}

	var runDate integrity.Date
	if *date != "" {
		parsed, err := integrity.ParseDate(*date)
		if err != nil {
			return integrityUsageError("--date %q is not YYYY-MM-DD", *date)
		}
		runDate = parsed
	}

	cfg, err := config.Load()
	if err != nil {
		return &exitError{code: 2, err: redactIntegrityError(
			fmt.Errorf("integrity run: %w", err), "postgres://", "postgresql://")}
	}
	if strings.TrimSpace(cfg.Database.AppURL) == "" {
		return &exitError{code: 2, err: errors.New(
			"config: database.app_url is required for recon integrity run (set DATABASE_APP_URL or database.app_url in CONFIG_FILE); the admin DATABASE_URL is not used")}
	}
	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		return &exitError{code: 2, err: redactIntegrityError(
			fmt.Errorf("integrity run: open application role: %w", err),
			cfg.Database.AppURL)}
	}
	defer app.Close()

	result, err := integrity.Run(ctx, app.Pool, integrity.Options{
		RunDate:     runDate,
		TriggeredBy: integrity.TriggerManual,
		Clock:       clk,
	})
	if err != nil {
		if errors.Is(err, integrity.ErrAlreadyRunning) {
			return &exitError{code: 2, err: errors.New(
				"integrity run: another integrity run is in progress")}
		}
		return &exitError{code: 2, err: redactIntegrityError(
			fmt.Errorf("integrity run: %w", err), cfg.Database.AppURL)}
	}

	if *asJSON {
		encoded, err := json.Marshal(result.Report)
		if err != nil {
			return &exitError{code: 2, err: fmt.Errorf("integrity run: encode JSON: %w", err)}
		}
		if _, err := fmt.Fprintf(out, "%s\n", encoded); err != nil {
			return &exitError{code: 2, err: fmt.Errorf("integrity run: write JSON: %w", err)}
		}
	} else if err := writeIntegrityHuman(out, result.Report); err != nil {
		return &exitError{code: 2, err: err}
	}

	if result.Report.Status == integrity.StatusFailed {
		return &exitError{code: 1, err: fmt.Errorf(
			"integrity run: %d of %d checks failed", result.Report.FailureCount, result.Report.CheckCount)}
	}
	return nil
}

// writeIntegrityHuman prints the run envelope, one line per check, and at most
// a few distinct problem codes. It deliberately does not dump every detail;
// --json and the daily report are the machine/operator deep-dive paths.
func writeIntegrityHuman(out io.Writer, report integrity.Report) error {
	label := "PASSED"
	if report.Status == integrity.StatusFailed {
		label = "FAILED"
	}
	if _, err := fmt.Fprintf(out,
		"integrity run: %s run_id=%s date=%s status=%s checks=%d failures=%d\n",
		label, report.RunID, report.RunDate.String(), string(report.Status),
		report.CheckCount, report.FailureCount); err != nil {
		return fmt.Errorf("integrity run: write summary: %w", err)
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out,
			"  %s %s checked=%d skipped=%d problems=%d",
			check.Name, string(check.Status), check.Checked, check.Skipped,
			check.ProblemCount); err != nil {
			return fmt.Errorf("integrity run: write check: %w", err)
		}
		if codes := boundedProblemCodes(check.Problems, 5); len(codes) > 0 {
			if _, err := fmt.Fprintf(out, " codes=%s", strings.Join(codes, ",")); err != nil {
				return fmt.Errorf("integrity run: write codes: %w", err)
			}
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return fmt.Errorf("integrity run: write check: %w", err)
		}
	}
	if report.Status == integrity.StatusFailed {
		if _, err := fmt.Fprintln(out,
			"integrity run: failed; use --json or the daily report for the full problem detail"); err != nil {
			return fmt.Errorf("integrity run: write pointer: %w", err)
		}
	}
	return nil
}

// boundedProblemCodes returns at most max distinct codes in their stable
// problem order. It never echoes problem details.
func boundedProblemCodes(problems []integrity.Problem, max int) []string {
	codes := make([]string, 0, len(problems))
	seen := make(map[string]struct{}, len(problems))
	for _, problem := range problems {
		if _, ok := seen[problem.Code]; ok {
			continue
		}
		seen[problem.Code] = struct{}{}
		codes = append(codes, problem.Code)
		if len(codes) >= max {
			break
		}
	}
	return codes
}

// integrityUsageError marks a flag/argument/date problem as exit 2.
func integrityUsageError(format string, args ...any) error {
	return &exitError{code: 2, err: fmt.Errorf("integrity run: "+format, args...)}
}

// redactIntegrityError strips known secrets from an operator-facing error. The
// CLI never prints a DSN or password; config errors from a malformed DSN can
// otherwise echo the raw URL.
func redactIntegrityError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "<redacted>")
	}
	return errors.New(message)
}
