package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// Deps are the process-boundary dependencies. All fields have production
// defaults; tests inject DSNs, a clock, a dataset builder, and a per-case LLM
// client seam.
type Deps struct {
	Logger            *slog.Logger
	Clock             clock.Clock
	ResultsDSN        string
	AdminDSN          string
	ModuleRoot        string
	RuntimeConfig     *config.Config
	ClientForScenario func(ref sandbox.CaseRef, truth dataset.ScenarioTruth) (llm.Client, error)
	BuildConfig       func(manifest dataset.Manifest) (dataset.Config, error)
	CassetteDir       string
}

// Summary is the command result across every run/repeat.
type Summary struct {
	Runs []RunSummary
}

// RunSummary is one eval run's operator summary.
type RunSummary struct {
	RunID       string
	RepeatIndex *int
	Total       int
	Completed   int
	Skipped     int
	Aborted     bool
	SpentUSD    decimal.Decimal
	CapUSD      decimal.Decimal
	CassetteDir string
	Note        string
}

// Run executes the eval runner. Usage errors are *UsageError (exit 2); every
// other error is an infrastructure/validation failure (exit 1). A cost-cap stop
// returns a nil error and a summary with Aborted=true (exit 0).
func Run(ctx context.Context, opts Options, deps Deps) (Summary, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runClock := deps.Clock
	if runClock == nil {
		runClock = clock.Real{}
	}

	ApplyDefaults(&opts)
	if err := Validate(opts); err != nil {
		return Summary{}, err
	}

	var (
		stored  *runRow
		results *pgxpool.Pool
		lock    *runLock
	)
	if opts.Resume != "" {
		var err error
		results, err = openResultsPool(ctx, resultsDSN(deps))
		if err != nil {
			return Summary{}, err
		}
		defer results.Close()
		row, err := loadRun(ctx, results, strings.ToLower(opts.Resume))
		if err != nil {
			return Summary{}, err
		}
		if err := applyResumeOptions(&opts, row); err != nil {
			return Summary{}, err
		}
		lock, err = acquireRunLock(ctx, results, row.ID)
		if err != nil {
			return Summary{}, err
		}
		defer func() {
			if err := lock.release(ctx); err != nil {
				logger.Warn("eval run lock release failed", "run_id", row.ID, "error", err)
			}
		}()
		stored = &row
	}

	rd, err := loadDataset(opts, deps)
	if err != nil {
		return Summary{}, err
	}
	if opts.Seed == nil {
		seed := rd.Manifest.Seed
		opts.Seed = &seed
	}
	if stored != nil {
		if err := validateResumeFingerprint(opts, *stored, rd); err != nil {
			return Summary{}, err
		}
	}

	if results == nil {
		results, err = openResultsPool(ctx, resultsDSN(deps))
		if err != nil {
			return Summary{}, err
		}
		defer results.Close()
	}

	runtimeCfg, err := resolveRuntimeConfig(deps, opts)
	if err != nil {
		return Summary{}, err
	}
	root, err := moduleRoot(deps)
	if err != nil {
		return Summary{}, err
	}
	manager, err := newSandboxManager(opts, rd, deps, logger)
	if err != nil {
		return Summary{}, err
	}

	summary := Summary{}
	if stored != nil {
		if opts.Resume != "" {
			completed, err := loadCompletedScenarios(ctx, results, stored.ID)
			if err != nil {
				return summary, err
			}
			expected := expectedScoreableCount(rd.Dataset.Truth)
			if expected > 0 && len(completed) >= expected {
				spent, count, err := loadSpent(ctx, results, stored.ID)
				if err != nil {
					return summary, err
				}
				aborted, err := finalizeRun(ctx, results, *stored, opts, runClock, spent,
					count, false, logger)
				if err != nil {
					return summary, err
				}
				summary.Runs = append(summary.Runs, RunSummary{
					RunID:       stored.ID,
					RepeatIndex: stored.RepeatIndex,
					Total:       expected,
					Completed:   count,
					Skipped:     count,
					Aborted:     aborted,
					SpentUSD:    spent,
					CapUSD:      opts.MaxCostUSD,
					Note:        "already complete",
				})
				return summary, nil
			}
		}
		runSummary, runErr := executeRun(ctx, *stored, opts, rd, results, manager, deps,
			runtimeCfg, logger, runClock)
		summary.Runs = append(summary.Runs, runSummary)
		return summary, runErr
	}

	repeatGroup := ""
	if opts.Repeat > 1 {
		repeatGroup, err = newUUID()
		if err != nil {
			return summary, err
		}
	}
	gitSHA := bestEffortGitSHA(root)
	for index := 0; index < opts.Repeat; index++ {
		var repeatIndex *int
		if opts.Repeat > 1 {
			value := index
			repeatIndex = &value
		}
		row, err := insertRun(ctx, results, opts, rd, runClock.Now().UTC(), gitSHA,
			repeatGroup, repeatIndex)
		if err != nil {
			return summary, err
		}
		runSummary, runErr := executeRun(ctx, row, opts, rd, results, manager, deps,
			runtimeCfg, logger, runClock)
		summary.Runs = append(summary.Runs, runSummary)
		if runErr != nil {
			return summary, runErr
		}
	}
	return summary, nil
}

// applyResumeOptions makes the stored run authoritative for fields the caller
// did not explicitly set, then rejects any conflicting explicit flag.
func applyResumeOptions(opts *Options, stored runRow) error {
	if opts.SuiteSet && opts.Suite != stored.Suite {
		return usagef("--suite %q conflicts with stored run suite %q", opts.Suite, stored.Suite)
	}
	opts.Suite = stored.Suite
	if opts.SplitSet && opts.Split != stored.Split {
		return usagef("--split %q conflicts with stored run split %q", opts.Split, stored.Split)
	}
	if !opts.SplitSet {
		opts.Split = stored.Split
	}
	if opts.ModelSet && config.NormalizeLLMModel(opts.Model) != config.NormalizeLLMModel(stored.Model) {
		return usagef("--model %q conflicts with stored run model %q", opts.Model, stored.Model)
	}
	if !opts.ModelSet {
		opts.Model = stored.Model
	}
	if opts.PromptSet && opts.Prompt != stored.PromptVersion {
		return usagef("--prompt %q conflicts with stored run prompt %q", opts.Prompt, stored.PromptVersion)
	}
	if !opts.PromptSet {
		opts.Prompt = stored.PromptVersion
	}
	if stored.CassetteKnown {
		switch {
		case opts.CassetteDirSet && filepath.Clean(opts.CassetteDir) != filepath.Clean(stored.CassetteDir):
			return usagef("--cassette-dir %q conflicts with stored run cassette dir %q",
				opts.CassetteDir, stored.CassetteDir)
		case !opts.CassetteDirSet:
			opts.CassetteDir = stored.CassetteDir
		}
	}
	if opts.Seed == nil {
		seed := stored.Seed
		opts.Seed = &seed
	} else if *opts.Seed != stored.Seed {
		return usagef("--seed %d conflicts with stored run seed %d", *opts.Seed, stored.Seed)
	}

	// LLM mode and cost cap are execution provenance, not free-running
	// defaults. Restore the stored values unless the caller explicitly
	// overrode the cap; an explicit conflicting mode is a usage error rather
	// than a silent switch from live to cassette.
	switch {
	case stored.LLMKnown && opts.LLMSet && opts.LLM != stored.LLM:
		return usagef("--llm %q conflicts with stored run llm %q", opts.LLM, stored.LLM)
	case stored.LLMKnown:
		opts.LLM = stored.LLM
	case !opts.LLMSet:
		return usagef("stored run has no llm mode; pass --llm explicitly")
	}

	// A non-zero programmatic cap is treated as explicit for backwards
	// compatibility with callers that do not populate MaxCostUSDSet; the CLI
	// always sets it. Zero needs the Set flag because it is both the default
	// and an explicit unlimited cap.
	capExplicit := opts.MaxCostUSDSet || !opts.MaxCostUSD.IsZero()
	if !capExplicit {
		if !stored.CapKnown {
			return usagef("stored run has no max_cost_usd; pass --max-cost-usd explicitly (0 = unlimited)")
		}
		opts.MaxCostUSD = stored.MaxCostUSD
	}

	ApplyDefaults(opts)
	return Validate(*opts)
}

func validateResumeFingerprint(opts Options, stored runRow, rd *resolvedDataset) error {
	switch {
	case opts.Split != stored.Split:
		return usagef("--split %q conflicts with stored run split %q", opts.Split, stored.Split)
	case opts.Model != stored.Model:
		return usagef("--model %q conflicts with stored run model %q", opts.Model, stored.Model)
	case opts.Prompt != stored.PromptVersion:
		return usagef("--prompt %q conflicts with stored run prompt %q", opts.Prompt, stored.PromptVersion)
	case opts.Seed == nil || *opts.Seed != stored.Seed:
		return usagef("--seed conflicts with stored run seed %d", stored.Seed)
	case rd.Manifest.EvalDatasetVersion != stored.DatasetVersion:
		return usagef("dataset version %q conflicts with stored run dataset version %q",
			rd.Manifest.EvalDatasetVersion, stored.DatasetVersion)
	}
	return nil
}

// expectedScoreableCount mirrors the sandbox mapping rule without loading a
// world: every truth row with a break signature is scoreable, and every
// injection_memo truth row gets a synthetic case in every split (the generator
// includes injection_memo applications in dev/test too). Clean and other
// empty-signature non-injection scenarios are not scoreable.
func expectedScoreableCount(truth dataset.GroundTruth) int {
	count := 0
	for _, scenario := range truth.Scenarios {
		if scenario.BreakSignature != "" || scenario.Mode == modes.InjectionMemo {
			count++
		}
	}
	return count
}

func executeRun(ctx context.Context, row runRow, opts Options, rd *resolvedDataset,
	results *pgxpool.Pool, manager *sandbox.Manager, deps Deps, runtimeCfg config.Config,
	logger *slog.Logger, runClock clock.Clock) (RunSummary, error) {
	summary := RunSummary{
		RunID:       row.ID,
		RepeatIndex: row.RepeatIndex,
		CapUSD:      opts.MaxCostUSD,
		CassetteDir: resolveCassetteDir(opts, deps),
	}

	completed, err := loadCompletedScenarios(ctx, results, row.ID)
	if err != nil {
		return summary, err
	}
	spent, _, err := loadSpent(ctx, results, row.ID)
	if err != nil {
		return summary, err
	}

	var recordClient llm.Client
	if opts.LLM == LLMRecord {
		client, err := newRecordClient(runtimeCfg.LLM, summary.CassetteDir, opts.UpdateCassettes,
			llm.Options{Clock: runClock, Logger: logger})
		if err != nil {
			return summary, markRunError(ctx, results, row, runClock, spent, opts.MaxCostUSD,
				fmt.Errorf("eval/runner: build recording client: %w", err), logger)
		}
		recordClient = client
	}

	actor, err := loadLoadActor()
	if err != nil {
		return summary, markRunError(ctx, results, row, runClock, spent, opts.MaxCostUSD, err, logger)
	}
	sb, err := manager.Clone(ctx, rd.Dataset, sandbox.LoadOptions{
		Config: rd.Config,
		Actor:  actor,
	})
	if err != nil {
		return summary, markRunError(ctx, results, row, runClock, spent, opts.MaxCostUSD, err, logger)
	}

	refs, err := sb.ScoreableScenarios(ctx)
	if err != nil {
		_ = sb.Close(context.WithoutCancel(ctx))
		return summary, markRunError(ctx, results, row, runClock, spent, opts.MaxCostUSD, err, logger)
	}
	summary.Total = len(refs)

	pending := make([]sandbox.CaseRef, 0, len(refs))
	truthByScenario := make(map[string]dataset.ScenarioTruth, len(rd.Dataset.Truth.Scenarios))
	for _, scenario := range rd.Dataset.Truth.Scenarios {
		truthByScenario[scenario.ScenarioID] = scenario
	}
	for _, ref := range refs {
		if _, done := completed[ref.ScenarioID]; done {
			summary.Skipped++
			continue
		}
		if _, ok := truthByScenario[ref.ScenarioID]; !ok {
			_ = sb.Close(context.WithoutCancel(ctx))
			err := fmt.Errorf("eval/runner: scoreable scenario %s has no ground truth", ref.ScenarioID)
			return summary, markRunError(ctx, results, row, runClock, spent, opts.MaxCostUSD, err, logger)
		}
		pending = append(pending, ref)
	}

	if len(pending) == 0 {
		closeErr := sb.Close(context.WithoutCancel(ctx))
		completedCount := len(completed)
		summary.Completed = completedCount
		summary.SpentUSD = spent
		summary.Note = "already complete"
		if closeErr != nil {
			return summary, markRunError(ctx, results, row, runClock, spent, opts.MaxCostUSD, closeErr, logger)
		}
		aborted, err := finalizeRun(ctx, results, row, opts, runClock, spent,
			completedCount, false, logger)
		if err != nil {
			return summary, err
		}
		summary.Aborted = aborted
		return summary, nil
	}

	execution := &runExecution{
		run:          row,
		opts:         opts,
		rd:           rd,
		sandbox:      sb,
		results:      results,
		deps:         deps,
		runtimeCfg:   runtimeCfg,
		logger:       logger,
		truth:        truthByScenario,
		costs:        newCostState(spent),
		resumed:      opts.Resume != "",
		cassetteDir:  summary.CassetteDir,
		recordClient: recordClient,
	}
	executed, aborted, poolErr := executePool(ctx, execution, pending)
	closeErr := sb.Close(context.WithoutCancel(ctx))

	finalSpent, finalCount, loadErr := loadSpent(context.WithoutCancel(ctx), results, row.ID)
	if loadErr == nil {
		summary.SpentUSD = finalSpent
		summary.Completed = finalCount
	} else {
		if execution.costs != nil {
			summary.SpentUSD = execution.costs.value()
		} else {
			summary.SpentUSD = spent
		}
		summary.Completed = len(completed) + executed
	}

	if poolErr == nil && closeErr != nil {
		poolErr = closeErr
	}
	if poolErr != nil {
		return summary, markRunError(ctx, results, row, runClock, summary.SpentUSD, opts.MaxCostUSD, poolErr, logger)
	}
	finalAborted, err := finalizeRun(ctx, results, row, opts, runClock, summary.SpentUSD,
		summary.Completed, aborted, logger)
	if err != nil {
		return summary, err
	}
	summary.Aborted = finalAborted
	return summary, nil
}

// finalizeRun records the terminal run state. A positive cap is a hard signal
// even when the last in-flight case completed the final pending scenario: an
// overshoot is allowed but the run must be marked aborted. Spending exactly the
// cap after every case completed may remain completed.
func finalizeRun(ctx context.Context, results *pgxpool.Pool, row runRow, opts Options,
	runClock clock.Clock, spent decimal.Decimal, completed int, aborted bool,
	logger *slog.Logger) (bool, error) {
	if !aborted && opts.MaxCostUSD.IsPositive() && spent.GreaterThan(opts.MaxCostUSD) {
		aborted = true
	}
	if aborted {
		return true, finishAbortedRun(ctx, results, row, opts, runClock, spent, logger)
	}
	return false, finishCompletedRun(ctx, results, row, opts, runClock, spent, completed, logger)
}

func finishCompletedRun(ctx context.Context, results *pgxpool.Pool, row runRow, opts Options,
	runClock clock.Clock, spent decimal.Decimal, completed int, logger *slog.Logger) error {
	now := runClock.Now().UTC()
	patch := map[string]any{
		"aborted":      false,
		"abort_reason": nil,
		"cost_spent":   spent.String(),
		"cost_cap":     opts.MaxCostUSD.String(),
	}
	if opts.Resume != "" {
		patch["resumed_at"] = now.Format(time.RFC3339Nano)
		patch["last_resume_by"] = runnerIdentity()
	}
	if err := finishRun(ctx, results, row.ID, now, patch, opts.MaxCostUSD.String()); err != nil {
		return err
	}
	logger.Info("eval run complete", "run_id", row.ID, "completed", completed,
		"cost_spent", spent.String(), "cost_cap", opts.MaxCostUSD.String())
	return nil
}

func finishAbortedRun(ctx context.Context, results *pgxpool.Pool, row runRow, opts Options,
	runClock clock.Clock, spent decimal.Decimal, logger *slog.Logger) error {
	now := runClock.Now().UTC()
	patch := map[string]any{
		"aborted":      true,
		"abort_reason": "cost_cap",
		"cost_spent":   spent.String(),
		"cost_cap":     opts.MaxCostUSD.String(),
	}
	if opts.Resume != "" {
		patch["resumed_at"] = now.Format(time.RFC3339Nano)
		patch["last_resume_by"] = runnerIdentity()
	}
	if err := finishRun(ctx, results, row.ID, now, patch, opts.MaxCostUSD.String()); err != nil {
		return err
	}
	logger.Info("eval run stopped at cost cap", "run_id", row.ID,
		"cost_spent", spent.String(), "cost_cap", opts.MaxCostUSD.String())
	return nil
}

func markRunError(ctx context.Context, results *pgxpool.Pool, row runRow, runClock clock.Clock,
	spent, cap decimal.Decimal, cause error, logger *slog.Logger) error {
	if cause == nil {
		return nil
	}
	patch := map[string]any{
		"aborted":      true,
		"abort_reason": "error",
		"error":        truncateError(cause),
		"cost_spent":   spent.String(),
		"cost_cap":     cap.String(),
	}
	if err := finishRun(context.WithoutCancel(ctx), results, row.ID, runClock.Now().UTC(), patch,
		cap.String()); err != nil {
		logger.Error("eval run error finalize failed", "run_id", row.ID, "error", err)
		return errors.Join(cause, err)
	}
	return cause
}

func truncateError(err error) string {
	const max = 500
	text := err.Error()
	if len(text) <= max {
		return text
	}
	return text[:max]
}

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", fmt.Errorf("eval/runner: generate uuid: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// String renders a human-readable summary for the CLI.
func (s Summary) String() string {
	var builder strings.Builder
	for _, run := range s.Runs {
		fmt.Fprintf(&builder,
			"run %s: completed %d/%d scenarios (skipped %d), spent %s of cap %s",
			run.RunID, run.Completed, run.Total, run.Skipped,
			run.SpentUSD.String(), capLabel(run.CapUSD))
		if run.Aborted {
			builder.WriteString(" — aborted at cost cap")
		}
		if run.CassetteDir != "" {
			builder.WriteString(" — cassettes " + run.CassetteDir)
		}
		if run.Note != "" {
			builder.WriteString(" — " + run.Note)
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

func capLabel(cap decimal.Decimal) string {
	if cap.IsZero() {
		return "unlimited"
	}
	return cap.String()
}
