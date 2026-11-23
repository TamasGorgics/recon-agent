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

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/prompts"
)

const replayUsage = `usage: recon replay --run <uuid> (--exact | --rerun) [flags]

Reconstructs one recorded agent run from a single read-only load and never
executes a tool or touches live data. Exit 0 means identical, 1 means the
report diverged (the full report is still printed to stdout), and 2 means a
usage/config/load/DB/cassette-miss/live-key error (stderr only, even with
--json).

Modes:
  --exact          recompute every canonical request hash and the proposal
                   digest from the recording; supports outcome=proposal only
  --rerun          feed the recorded tool results to a model and compare the
                   resulting proposal digest; cassette replay by default

Rerun flags:
  --model <id>     model override (default: recorded model)
  --prompt <v>     prompt version override (default: recorded prompt)
  --live           call the provider; requires OPENCODE_API_KEY in the process
                   environment (never .env, never a file)
  --cassette-dir   cassette directory (default eval/cassettes)

Other flags:
  --run <uuid>     agent_runs.id to replay (required)
  --json           print one JSON report on stdout

Exit status:
  0  identical with no divergences
  1  divergence (the full report is still printed to stdout); a key-only
     proposal_field_diff exits 1 even though the report keeps identical=true
  2  usage/config/load/DB/cassette-miss/live-key error (stderr only, even
     with --json)
`

// replayDeps are test seams. The zero value uses the production role pool and
// the cassette/live client factories.
type replayDeps struct {
	loadRecording func(ctx context.Context, cfg config.Config, runID string) (replay.Recording, error)
	newClient     func(cfg config.Config, opts replayClientOptions) (llm.Client, error)
}

// replayClientOptions is the rerun client selection. Live is false for the
// cassette default.
type replayClientOptions struct {
	Model       string
	Prompt      string
	CassetteDir string
	Live        bool
}

func runReplay(ctx context.Context, args []string) error {
	return runReplayTo(ctx, args, os.Stdout, replayDeps{})
}

// runReplayTo implements `recon replay`. The recording is loaded once through
// the app role in a REPEATABLE READ READ ONLY transaction, the pool is closed,
// and then ExactReplay or Rerun runs with no database handle. This path never
// calls runWithTelemetry: replay must not export a trace or contact an OTLP
// endpoint.
func runReplayTo(ctx context.Context, args []string, out io.Writer, deps replayDeps) error {
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	runID := flags.String("run", "", "agent run UUID")
	exact := flags.Bool("exact", false, "exact replay")
	rerun := flags.Bool("rerun", false, "rerun with a model")
	model := flags.String("model", "", "rerun model override")
	promptVersion := flags.String("prompt", "", "rerun prompt version override")
	live := flags.Bool("live", false, "call the provider (requires OPENCODE_API_KEY)")
	cassetteDir := flags.String("cassette-dir", llm.DefaultCassetteDir, "cassette directory for --rerun")
	asJSON := flags.Bool("json", false, "print one JSON report")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(out, replayUsage)
			return writeErr
		}
		return replayUsageError("%v", err)
	}
	if flags.NArg() != 0 {
		return replayUsageError("unexpected argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(*runID) == "" {
		return replayUsageError("--run <uuid> is required")
	}
	if !tools.IsUUID(strings.ToLower(strings.TrimSpace(*runID))) {
		return replayUsageError("--run %q is not a UUID", *runID)
	}
	switch {
	case *exact && *rerun:
		return replayUsageError("--exact and --rerun are mutually exclusive")
	case !*exact && !*rerun:
		return replayUsageError("exactly one of --exact or --rerun is required")
	}
	if *exact && (*live || strings.TrimSpace(*model) != "" || strings.TrimSpace(*promptVersion) != "") {
		return replayUsageError("--model, --prompt and --live apply only to --rerun")
	}
	// The key is inspected only when --live is passed, and only in the process
	// environment. A missing key is a usage/config error, never a divergence.
	if *live && strings.TrimSpace(os.Getenv(config.EnvOpenCodeAPIKey)) == "" {
		return &exitError{code: 2, err: errors.New(
			"replay: live_key_missing: --live requires OPENCODE_API_KEY in the process environment")}
	}
	if *rerun && strings.TrimSpace(*promptVersion) != "" {
		if _, err := prompts.Load(strings.TrimSpace(*promptVersion)); err != nil {
			return replayCodedError("config", err)
		}
	}

	// Replay needs only the app role DSN and the agent/LLM settings. It does not
	// require the migration DSN, policy file, or unrelated runtime settings.
	cfg, err := config.Load()
	if err != nil {
		return replayCodedError("config", err)
	}
	if err := cfg.LLM.Validate(); err != nil {
		return replayCodedError("config", err)
	}
	if err := cfg.Agent.Validate(); err != nil {
		return replayCodedError("config", err)
	}

	load := deps.loadRecording
	if load == nil {
		if strings.TrimSpace(cfg.Database.AppURL) == "" {
			return &exitError{code: 2, err: errors.New(
				"replay: config: database.app_url is required for recon replay (set DATABASE_APP_URL or database.app_url in CONFIG_FILE)")}
		}
		load = loadRecordingReadOnly
	}
	recording, err := load(ctx, cfg, *runID)
	if err != nil {
		return replayLoadError(err)
	}

	replayConfig := replay.Config{
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
		Agent:    cfg.Agent,
	}
	var report replay.Report
	if *exact {
		report, err = replay.ExactReplay(recording, replayConfig)
	} else {
		newClient := deps.newClient
		if newClient == nil {
			newClient = newReplayClient
		}
		client, clientErr := newClient(cfg, replayClientOptions{
			Model:       strings.TrimSpace(*model),
			Prompt:      strings.TrimSpace(*promptVersion),
			CassetteDir: *cassetteDir,
			Live:        *live,
		})
		if clientErr != nil {
			return replayCodedError("config", clientErr)
		}
		report, err = replay.Rerun(ctx, recording, replay.RerunOptions{
			Client: client,
			Model:  strings.TrimSpace(*model),
			Prompt: strings.TrimSpace(*promptVersion),
			Config: replayConfig,
		})
	}
	if err != nil {
		return replayRunError(err)
	}

	if writeErr := writeReplayReport(out, report, *asJSON); writeErr != nil {
		return replayCodedError("output", writeErr)
	}
	// Exit 1 follows the divergence list, per spec §5. A key-only
	// proposal_field_diff is deliberately non-identity-breaking (identical
	// stays true), but it is still a divergence the operator must see.
	if !report.Identical || len(report.Divergences) > 0 {
		return &exitError{code: 1, err: fmt.Errorf(
			"replay: divergence: run %s reported %d divergence(s)",
			report.RunID, len(report.Divergences))}
	}
	return nil
}

// loadRecordingReadOnly is the only database phase. It opens the app role,
// loads the run in one REPEATABLE READ READ ONLY transaction, commits, and
// closes the pool before returning. The returned Recording is the complete
// in-memory snapshot exact/rerun replay uses; neither receives a pool.
func loadRecordingReadOnly(ctx context.Context, cfg config.Config, runID string) (replay.Recording, error) {
	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		return replay.Recording{}, fmt.Errorf("replay: open application role: %w", err)
	}
	var recording replay.Recording
	loadErr := db.WithTx(ctx, app.Pool, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	}, func(ctx context.Context, tx pgx.Tx) error {
		loaded, err := replay.LoadRecording(ctx, tx, runID)
		if err != nil {
			return err
		}
		recording = loaded
		return nil
	})
	// Close regardless of the load result: after this point replay must run
	// without any reachable database connection.
	app.Close()
	return recording, loadErr
}

// newReplayClient builds the rerun client. Cassettes are the default and never
// touch the network. Live is the only provider path and requires the API key
// from the process environment; LLMConfig.APIKey is yaml:"-" so a config file
// cannot supply it.
func newReplayClient(cfg config.Config, opts replayClientOptions) (llm.Client, error) {
	if !opts.Live {
		return llm.NewCassette(nil, llm.CassetteOptions{
			Mode:     llm.CassetteReplay,
			Dir:      opts.CassetteDir,
			Provider: cfg.LLM.Provider,
			BaseURL:  cfg.LLM.BaseURL,
		})
	}
	apiKey := strings.TrimSpace(os.Getenv(config.EnvOpenCodeAPIKey))
	if apiKey == "" {
		return nil, errors.New("live_key_missing: --live requires OPENCODE_API_KEY in the process environment")
	}
	liveConfig := cfg.LLM
	liveConfig.APIKey = apiKey
	return llm.NewClient(liveConfig, llm.Options{})
}

// writeReplayReport renders the full report. The bytes are built before the
// single write so an exit-2 path can never leak a partial JSON object.
func writeReplayReport(out io.Writer, report replay.Report, asJSON bool) error {
	if asJSON {
		encoded, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("encode JSON: %w", err)
		}
		_, err = fmt.Fprintf(out, "%s\n", encoded)
		return err
	}
	_, err := io.WriteString(out, renderReplayHuman(report))
	return err
}

func renderReplayHuman(report replay.Report) string {
	verdict := "IDENTICAL"
	if !report.Identical {
		verdict = "DIVERGED"
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "replay %s run=%s mode=%s\n", verdict, report.RunID, report.Mode)
	fmt.Fprintf(&summary, "  proposal_hash %s\n", report.ProposalHash)
	fmt.Fprintf(&summary, "  stored_hash   %s\n", report.StoredHash)
	if report.StateHash != "" {
		fmt.Fprintf(&summary, "  state_hash    %s\n", report.StateHash)
	}
	if report.Outcome != "" {
		fmt.Fprintf(&summary, "  outcome       %s\n", report.Outcome)
	}
	if report.StepsTotal > 0 {
		fmt.Fprintf(&summary, "  steps         %d\n", report.StepsTotal)
	}
	if report.Model != "" {
		fmt.Fprintf(&summary, "  model         %s\n", report.Model)
	}
	if report.Prompt != "" {
		fmt.Fprintf(&summary, "  prompt        %s\n", report.Prompt)
	}
	if len(report.Divergences) > 0 {
		summary.WriteString("divergences:\n")
		for _, divergence := range report.Divergences {
			fmt.Fprintf(&summary, "  step=%d code=%s field=%s detail=%s\n",
				divergence.Step, divergence.Code,
				optionalText(divergence.Field), optionalText(divergence.Detail))
		}
	}
	if len(report.Diagnostics) > 0 {
		summary.WriteString("diagnostics:\n")
		for _, diagnostic := range report.Diagnostics {
			fmt.Fprintf(&summary, "  step=%d code=%s detail=%s\n",
				diagnostic.Step, diagnostic.Code, diagnostic.Detail)
		}
	}
	return summary.String()
}

func optionalText(value *string) string {
	if value == nil || *value == "" {
		return "-"
	}
	return *value
}

// replayUsageError marks a flag/argument problem as exit 2.
func replayUsageError(format string, args ...any) error {
	return &exitError{code: 2, err: fmt.Errorf("replay: usage: "+format, args...)}
}

// replayCodedError marks an operational failure as exit 2 with its stable code.
func replayCodedError(code string, err error) error {
	return &exitError{code: 2, err: fmt.Errorf("replay: %s: %w", code, err)}
}

// replayLoadError maps the load phase. ErrRunNotFound and ErrUnsupportedRun get
// their spec codes; anything else is a load/config/DB failure.
func replayLoadError(err error) error {
	switch {
	case errors.Is(err, replay.ErrRunNotFound):
		return replayCodedError("run_not_found", err)
	case errors.Is(err, replay.ErrUnsupportedRun):
		return replayCodedError("unsupported_run", err)
	case errors.Is(err, replay.ErrInvalidRunID):
		return replayCodedError("usage", err)
	default:
		return replayCodedError("load", err)
	}
}

// replayRunError maps an engine failure. A cassette miss is explicitly an
// operational error, never a divergence report.
func replayRunError(err error) error {
	switch {
	case errors.Is(err, replay.ErrCassetteMiss), errors.Is(err, llm.ErrCassetteMiss):
		return replayCodedError("cassette_miss", err)
	case errors.Is(err, replay.ErrRunNotFound):
		return replayCodedError("run_not_found", err)
	case errors.Is(err, replay.ErrUnsupportedRun):
		return replayCodedError("unsupported_run", err)
	case errors.Is(err, replay.ErrInvalidRecording), errors.Is(err, replay.ErrRebuild):
		return replayCodedError("load", err)
	default:
		return replayCodedError("replay", err)
	}
}
