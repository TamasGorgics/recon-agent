package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// runRow is one durable eval_runs row. LLM and MaxCostUSD are the effective
// execution settings captured in config.eval at run creation; resume restores
// them instead of silently falling back to the CLI defaults.
type runRow struct {
	ID             string
	Suite          string
	Split          dataset.Split
	DatasetVersion string
	Model          string
	PromptVersion  string
	Seed           int64
	GitSHA         string
	Config         []byte
	LLM            string
	MaxCostUSD     decimal.Decimal
	CassetteDir    string
	LLMKnown       bool
	CapKnown       bool
	CassetteKnown  bool
	StartedAt      time.Time
	EndedAt        *time.Time
	RepeatGroup    string
	RepeatIndex    *int
}

type runConfigDocument struct {
	Eval    evalSettings `json:"eval"`
	Pricing llm.Snapshot `json:"pricing"`
}

type evalSettings struct {
	Suite       string `json:"suite"`
	Split       string `json:"split"`
	Repeat      int    `json:"repeat"`
	RepeatIndex int    `json:"repeat_index"`
	Concurrency int    `json:"concurrency"`
	LLM         string `json:"llm"`
	MaxCostUSD  string `json:"max_cost_usd"`
	CassetteDir string `json:"cassette_dir,omitempty"`
}

func buildRunConfig(opts Options, repeatIndex int) ([]byte, error) {
	document := runConfigDocument{
		Eval: evalSettings{
			Suite:       opts.Suite,
			Split:       string(opts.Split),
			Repeat:      opts.Repeat,
			RepeatIndex: repeatIndex,
			Concurrency: opts.Concurrency,
			LLM:         opts.LLM,
			MaxCostUSD:  opts.MaxCostUSD.String(),
			CassetteDir: opts.CassetteDir,
		},
		Pricing: llm.PricingSnapshot(),
	}
	data, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("eval/runner: marshal run config: %w", err)
	}
	return data, nil
}

func insertRun(ctx context.Context, pool *pgxpool.Pool, opts Options, rd *resolvedDataset,
	startedAt time.Time, gitSHA, repeatGroup string, repeatIndex *int) (runRow, error) {
	configJSON, err := buildRunConfig(opts, repeatIndexValue(repeatIndex))
	if err != nil {
		return runRow{}, err
	}
	var gitValue any
	if gitSHA != "" {
		gitValue = gitSHA
	}
	var groupValue any
	if repeatGroup != "" {
		groupValue = repeatGroup
	}
	var indexValue any
	if repeatIndex != nil {
		indexValue = *repeatIndex
	}

	row := runRow{
		Suite:          opts.Suite,
		Split:          opts.Split,
		DatasetVersion: rd.Manifest.EvalDatasetVersion,
		Model:          opts.Model,
		PromptVersion:  opts.Prompt,
		Seed:           rd.Manifest.Seed,
		GitSHA:         gitSHA,
		Config:         configJSON,
		StartedAt:      startedAt,
		RepeatGroup:    repeatGroup,
		RepeatIndex:    repeatIndex,
	}
	err = pool.QueryRow(ctx,
		`INSERT INTO eval_runs
		     (suite, split, dataset_version, model, prompt_version, seed, git_sha,
		      config, started_at, repeat_group, repeat_index)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10::uuid, $11)
		 RETURNING id::text, started_at`,
		row.Suite, string(row.Split), row.DatasetVersion, row.Model, row.PromptVersion,
		row.Seed, gitValue, configJSON, startedAt, groupValue, indexValue).
		Scan(&row.ID, &row.StartedAt)
	if err != nil {
		return runRow{}, fmt.Errorf("eval/runner: insert eval_runs row: %w", err)
	}
	return row, nil
}

func repeatIndexValue(index *int) int {
	if index == nil {
		return 0
	}
	return *index
}

func loadRun(ctx context.Context, pool *pgxpool.Pool, id string) (runRow, error) {
	var row runRow
	var split string
	var configJSON []byte
	err := pool.QueryRow(ctx,
		`SELECT id::text, suite, split, dataset_version, model, prompt_version, seed,
		        COALESCE(git_sha, ''), config::text, started_at, ended_at,
		        COALESCE(repeat_group::text, ''), repeat_index
		   FROM eval_runs
		  WHERE id = $1::uuid`, id).
		Scan(&row.ID, &row.Suite, &split, &row.DatasetVersion, &row.Model,
			&row.PromptVersion, &row.Seed, &row.GitSHA, &configJSON,
			&row.StartedAt, &row.EndedAt, &row.RepeatGroup, &row.RepeatIndex)
	if err == pgx.ErrNoRows {
		return runRow{}, usagef("--resume %s: eval run not found", id)
	}
	if err != nil {
		return runRow{}, fmt.Errorf("eval/runner: load eval run %s: %w", id, err)
	}
	row.Split = dataset.Split(split)
	row.Config = configJSON
	llmMode, maxCost, llmKnown, capKnown, err := parseStoredRunSettings(configJSON)
	if err != nil {
		return runRow{}, fmt.Errorf("eval/runner: parse config for run %s: %w", id, err)
	}
	row.LLM = llmMode
	row.MaxCostUSD = maxCost
	row.LLMKnown = llmKnown
	row.CapKnown = capKnown
	cassetteDir, cassetteKnown, err := parseStoredCassetteDir(configJSON)
	if err != nil {
		return runRow{}, fmt.Errorf("eval/runner: parse config for run %s: %w", id, err)
	}
	row.CassetteDir = cassetteDir
	row.CassetteKnown = cassetteKnown
	return row, nil
}

// storedRunSettings is the subset of eval_runs.config.eval that resume must
// treat as authoritative. Pointer fields distinguish an absent value from an
// explicit zero.
type storedRunSettings struct {
	Eval struct {
		LLM         string  `json:"llm"`
		MaxCostUSD  *string `json:"max_cost_usd"`
		CassetteDir *string `json:"cassette_dir"`
	} `json:"eval"`
}

// parseStoredRunSettings reads the stored LLM mode and cost cap without
// rewriting the pricing snapshot or any other config key.
func parseStoredRunSettings(data []byte) (llmMode string, maxCost decimal.Decimal,
	llmKnown, capKnown bool, err error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return "", decimal.Zero, false, false, nil
	}
	var settings storedRunSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", decimal.Zero, false, false, fmt.Errorf("decode stored eval settings: %w", err)
	}
	llmMode = strings.ToLower(strings.TrimSpace(settings.Eval.LLM))
	llmKnown = llmMode != ""
	if settings.Eval.MaxCostUSD == nil {
		return llmMode, decimal.Zero, llmKnown, false, nil
	}
	raw := strings.TrimSpace(*settings.Eval.MaxCostUSD)
	if raw == "" {
		return "", decimal.Zero, false, false, fmt.Errorf("stored max_cost_usd is empty")
	}
	parsed, err := decimal.NewFromString(raw)
	if err != nil {
		return "", decimal.Zero, false, false, fmt.Errorf("parse stored max_cost_usd %q: %w", raw, err)
	}
	if parsed.IsNegative() {
		return "", decimal.Zero, false, false, fmt.Errorf("stored max_cost_usd %q is negative", raw)
	}
	return llmMode, parsed, llmKnown, true, nil
}

// parseStoredCassetteDir reads the recorded cassette directory without
// rewriting any other config key. It is empty and unknown for runs created
// before record mode or by a caller that did not set a root.
func parseStoredCassetteDir(data []byte) (string, bool, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return "", false, nil
	}
	var settings storedRunSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", false, fmt.Errorf("decode stored eval settings: %w", err)
	}
	if settings.Eval.CassetteDir == nil {
		return "", false, nil
	}
	dir := strings.TrimSpace(*settings.Eval.CassetteDir)
	if dir == "" {
		return "", false, nil
	}
	return dir, true, nil
}

// finishRun sets ended_at, merges a top-level patch into eval_runs.config, and
// records the effective cost cap in config.eval.max_cost_usd so a resumed cap
// override is visible next to the top-level config.cost_cap. It never rewrites
// the pricing snapshot or the other original eval settings.
func finishRun(ctx context.Context, pool *pgxpool.Pool, runID string, endedAt time.Time,
	patch map[string]any, effectiveCap string) error {
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("eval/runner: marshal run config patch: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE eval_runs
		    SET ended_at = $2,
		        config = jsonb_set(config || $3::jsonb, '{eval,max_cost_usd}',
		                           to_jsonb($4::text), true)
		  WHERE id = $1::uuid`, runID, endedAt, patchJSON, effectiveCap); err != nil {
		return fmt.Errorf("eval/runner: finish eval run %s: %w", runID, err)
	}
	return nil
}

func loadCompletedScenarios(ctx context.Context, pool *pgxpool.Pool, runID string) (map[string]struct{}, error) {
	rows, err := pool.Query(ctx,
		`SELECT scenario_id FROM eval_case_results WHERE eval_run_id = $1::uuid`, runID)
	if err != nil {
		return nil, fmt.Errorf("eval/runner: load completed scenarios: %w", err)
	}
	defer rows.Close()
	completed := make(map[string]struct{})
	for rows.Next() {
		var scenarioID string
		if err := rows.Scan(&scenarioID); err != nil {
			return nil, fmt.Errorf("eval/runner: scan completed scenario: %w", err)
		}
		completed[scenarioID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval/runner: iterate completed scenarios: %w", err)
	}
	return completed, nil
}

func loadSpent(ctx context.Context, pool *pgxpool.Pool, runID string) (decimal.Decimal, int, error) {
	var (
		costText string
		count    int
	)
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(sum(cost_usd), 0)::text, count(*)
		   FROM eval_case_results WHERE eval_run_id = $1::uuid`, runID).
		Scan(&costText, &count); err != nil {
		return decimal.Zero, 0, fmt.Errorf("eval/runner: load spent cost: %w", err)
	}
	spent, err := decimal.NewFromString(costText)
	if err != nil {
		return decimal.Zero, 0, fmt.Errorf("eval/runner: parse spent cost %q: %w", costText, err)
	}
	return spent, count, nil
}

func insertCaseResult(ctx context.Context, pool *pgxpool.Pool, runID string, result caseResultRow) error {
	detailJSON, err := json.Marshal(result.Detail)
	if err != nil {
		return fmt.Errorf("eval/runner: marshal case detail for %s: %w", result.ScenarioID, err)
	}
	var confidence any
	if result.Confidence != nil {
		confidence = result.Confidence.String()
	}
	var tierActual any
	if result.TierActual != nil {
		tierActual = *result.TierActual
	}
	var tierExpected any
	if result.TierExpected != nil {
		tierExpected = *result.TierExpected
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated,
		      tier_expected, tier_actual, confidence, cost_usd, latency_ms, steps, detail)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9::numeric,
		         $10::numeric, $11, $12, $13::jsonb)
		 ON CONFLICT (eval_run_id, scenario_id) DO NOTHING`,
		runID, result.ScenarioID, result.FailureMode, result.Correct, result.FalseFix,
		result.Escalated, tierExpected, tierActual, confidence, result.CostUSD.String(),
		result.LatencyMS, result.Steps, detailJSON); err != nil {
		return fmt.Errorf("eval/runner: insert case result %s: %w", result.ScenarioID, err)
	}
	return nil
}

var gitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// bestEffortGitSHA returns the module HEAD when git is available and the
// output is a full lower-case SHA. Any failure is non-fatal and returns "".
func bestEffortGitSHA(moduleRoot string) string {
	if moduleRoot == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", moduleRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	sha := strings.ToLower(strings.TrimSpace(string(out)))
	if !gitSHAPattern.MatchString(sha) {
		return ""
	}
	return sha
}

// runnerIdentity is a non-secret resume marker.
func runnerIdentity() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}
