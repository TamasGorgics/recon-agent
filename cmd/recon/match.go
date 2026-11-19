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

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

const matchUsage = `usage: recon match <command> [arguments]

Commands:
  unmatch   mark one active match unmatched, reopen its linked breaks and
            cases, cancel active proposals, and write the audit trail
            usage: recon match unmatch --id <match-uuid> --reason <text>
                   --actor human:<id> [--json]
            exit: 0 unmatched, 2 usage/validation, 1 not found/already
            unmatched/applied adjustment/transaction error
`

// runMatch dispatches the minimal `recon match` group.
func runMatch(ctx context.Context, args []string) error {
	return runMatchTo(ctx, args, os.Stdout)
}

// runMatchTo is runMatch with a testable writer and dispatch.
func runMatchTo(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return &exitError{code: 2, err: errors.New(
			"match: missing command (try: recon match unmatch)")}
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := fmt.Fprint(out, matchUsage)
		return err
	case "unmatch":
		return runMatchUnmatchTo(ctx, args[1:], out)
	default:
		return &exitError{code: 2, err: fmt.Errorf(
			"unknown match command %q (try: recon match unmatch)", args[0])}
	}
}

// runMatchUnmatchTo implements
// `recon match unmatch --id UUID --reason TEXT --actor human:<id> [--json]`.
// Flag and actor/reason validation run before config loading or any database
// work, so usage errors exit 2 without opening a pool.
func runMatchUnmatchTo(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("match unmatch", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	matchID := flags.String("id", "", "match UUID")
	reason := flags.String("reason", "", "unmatch reason (trimmed 1..500 bytes)")
	actor := flags.String("actor", "", "human:<id> actor recorded in the audit")
	asJSON := flags.Bool("json", false, "print one JSON object")
	flags.Usage = func() { _, _ = fmt.Fprint(out, matchUsage) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(out, matchUsage)
			return writeErr
		}
		return matchUnmatchUsageError("%v", err)
	}
	if flags.NArg() != 0 {
		return matchUnmatchUsageError("unexpected argument %q", flags.Arg(0))
	}
	if *matchID == "" || *reason == "" || *actor == "" {
		return matchUnmatchUsageError(
			"--id, --reason and --actor are required")
	}
	if err := lifecycle.ValidateUnmatchInput(*matchID, *reason, *actor); err != nil {
		return matchUnmatchUsageError("%v", err)
	}

	// Admin unmatch needs only the application-role DSN. Do not require the
	// migration database.url or unrelated process settings on an admin host.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Database.AppURL) == "" {
		return errors.New("config: database.app_url is required for recon match unmatch (set DATABASE_APP_URL or database.app_url in CONFIG_FILE)")
	}
	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		return fmt.Errorf("match unmatch: open application role: %w", err)
	}
	defer app.Close()

	service, err := lifecycle.NewService(lifecycle.Options{
		DB:                               app.Pool,
		Clock:                            clock.Real{},
		MissingSettlementSLABusinessDays: cfg.Cases.MissingSettlementSLABusinessDays,
	})
	if err != nil {
		return fmt.Errorf("match unmatch: build lifecycle service: %w", err)
	}

	var result lifecycle.UnmatchResult
	err = db.WithTx(ctx, app.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = service.Unmatch(ctx, tx, *matchID, *reason, *actor)
		return err
	})
	if err != nil {
		if isUnmatchUsageError(err) {
			return matchUnmatchUsageError("%v", err)
		}
		return fmt.Errorf("match unmatch: %w", err)
	}

	if *asJSON {
		return writeUnmatchJSON(out, result)
	}
	return writeUnmatchSummary(out, result)
}

// unmatchJSON is the stable `--json` shape from spec 7.1.
type unmatchJSON struct {
	MatchID            string    `json:"match_id"`
	BreaksReopened     int       `json:"breaks_reopened"`
	CasesReopened      int       `json:"cases_reopened"`
	ProposalsCancelled int       `json:"proposals_cancelled"`
	UnmatchedAt        time.Time `json:"unmatched_at"`
}

func writeUnmatchJSON(out io.Writer, result lifecycle.UnmatchResult) error {
	encoded, err := json.Marshal(unmatchJSON{
		MatchID:            result.MatchID,
		BreaksReopened:     result.BreaksReopened,
		CasesReopened:      result.CasesReopened,
		ProposalsCancelled: result.ProposalsCancelled,
		UnmatchedAt:        result.UnmatchedAt,
	})
	if err != nil {
		return fmt.Errorf("match unmatch: encode JSON: %w", err)
	}
	if _, err := fmt.Fprintf(out, "%s\n", encoded); err != nil {
		return fmt.Errorf("match unmatch: write JSON: %w", err)
	}
	return nil
}

func writeUnmatchSummary(out io.Writer, result lifecycle.UnmatchResult) error {
	if _, err := fmt.Fprintf(out, "match %s unmatched at %s\n",
		result.MatchID, result.UnmatchedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("match unmatch: write summary: %w", err)
	}
	for _, line := range []struct {
		label string
		count int
	}{
		{"breaks reopened", result.BreaksReopened},
		{"cases reopened", result.CasesReopened},
		{"proposals cancelled", result.ProposalsCancelled},
	} {
		if _, err := fmt.Fprintf(out, "  %s: %d\n", line.label, line.count); err != nil {
			return fmt.Errorf("match unmatch: write summary: %w", err)
		}
	}
	return nil
}

// matchUnmatchUsageError marks a flag/actor/reason problem as exit 2. The
// service repeats validation, so a late validation escape is mapped the same
// way instead of becoming a transaction error (exit 1).
func matchUnmatchUsageError(format string, args ...any) error {
	return &exitError{code: 2, err: fmt.Errorf("match unmatch: "+format, args...)}
}

func isUnmatchUsageError(err error) bool {
	return errors.Is(err, lifecycle.ErrInvalidRequest) ||
		errors.Is(err, lifecycle.ErrInvalidActor) ||
		errors.Is(err, lifecycle.ErrActorNotAllowed) ||
		errors.Is(err, lifecycle.ErrInvalidReason)
}
