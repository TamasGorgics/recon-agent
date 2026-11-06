package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/prompts"
)

const agentUsage = `usage: recon agent run --case <uuid> --llm scripted|opencode --prompt v1
                       [--model <id>] [--cassette-dir eval/cassettes] [--json]

Leases one open case, runs the bounded agent loop, and prints the transcript,
tool calls, and proposal. --llm scripted replays committed cassettes with no
network or OPENCODE_API_KEY; --llm opencode makes live calls and requires
OPENCODE_API_KEY in the process environment.
`

func runAgent(ctx context.Context, args []string) error {
	return runAgentTo(ctx, args, os.Stdout)
}

// runAgentTo parameterises stdout so CLI tests can capture the transcript.
func runAgentTo(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: recon agent run [flags]")
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, err := fmt.Fprint(out, agentUsage)
		return err
	}
	if args[0] != "run" {
		return fmt.Errorf("unknown agent command %q (try: recon agent run)", args[0])
	}

	flags := flag.NewFlagSet("agent run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	caseID := flags.String("case", "", "case UUID")
	llmMode := flags.String("llm", "", "scripted (cassette replay) or opencode (live)")
	promptVersion := flags.String("prompt", "v1", "prompt version")
	model := flags.String("model", "", "model API id (defaults to agent.model, then llm.model)")
	cassetteDir := flags.String("cassette-dir", "eval/cassettes", "cassette directory")
	asJSON := flags.Bool("json", false, "print one JSON object")
	flags.Usage = func() { _, _ = fmt.Fprint(out, agentUsage) }
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(out, agentUsage)
			return writeErr
		}
		return fmt.Errorf("agent run: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("agent run: unexpected argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(*caseID) == "" {
		return errors.New("usage: recon agent run --case <uuid> --llm scripted|opencode --prompt v1")
	}

	// Flag and prompt validation happens before config loading and any
	// database or model work.
	if err := agentrun.ValidateCaseID(*caseID); err != nil {
		return err
	}
	if err := agentrun.ValidateMode(*llmMode); err != nil {
		return err
	}
	if _, err := prompts.Load(strings.TrimSpace(*promptVersion)); err != nil {
		return err
	}
	if agentrun.Mode(strings.TrimSpace(*llmMode)) == agentrun.ModeOpencode &&
		strings.TrimSpace(os.Getenv(config.EnvOpenCodeAPIKey)) == "" {
		return errors.New("agent run: --llm opencode requires OPENCODE_API_KEY in the process environment")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Database.AppURL) == "" {
		return errors.New("config: database.app_url is required for recon agent (set DATABASE_APP_URL or database.app_url in CONFIG_FILE)")
	}
	if err := cfg.ValidateAgentRuntime(); err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		return err
	}
	defer app.Close()
	agentRO, err := db.OpenRole(ctx, db.RoleAgentRO, cfg.Database.AgentURL)
	if err != nil {
		return err
	}
	defer agentRO.Close()

	report, err := agentrun.Run(ctx, agentrun.Options{
		CaseID:        *caseID,
		Mode:          agentrun.Mode(strings.TrimSpace(*llmMode)),
		PromptVersion: *promptVersion,
		Model:         *model,
		CassetteDir:   *cassetteDir,
	}, agentrun.Deps{
		Config:    cfg,
		App:       app.Pool,
		Agent:     agentRO.Pool,
		AgentExec: agentRO.Pool,
		Logger:    logger,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return report.JSON(out)
	}
	return report.Human(out)
}
