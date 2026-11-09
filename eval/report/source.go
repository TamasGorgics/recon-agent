package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
)

// PostgresSource reads durable eval rows. It is read-only; reports never write
// eval_runs or eval_case_results.
type PostgresSource struct {
	pool *pgxpool.Pool
}

// NewPostgresSource wraps an open results pool. The caller owns the pool.
func NewPostgresSource(pool *pgxpool.Pool) *PostgresSource {
	return &PostgresSource{pool: pool}
}

// LoadRun loads one eval_runs row by UUID.
func (s *PostgresSource) LoadRun(ctx context.Context, runID string) (RunRow, error) {
	row := s.pool.QueryRow(ctx, runSelect+` WHERE id = $1::uuid`, runID)
	run, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRow{}, fmt.Errorf("eval run %s not found", runID)
	}
	if err != nil {
		return RunRow{}, err
	}
	return run, nil
}

// LoadRepeatGroup loads every run in a repeat group ordered by repeat_index.
func (s *PostgresSource) LoadRepeatGroup(ctx context.Context, groupID string) ([]RunRow, error) {
	rows, err := s.pool.Query(ctx,
		runSelect+` WHERE repeat_group = $1::uuid ORDER BY repeat_index ASC, id ASC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("query repeat group %s: %w", groupID, err)
	}
	defer rows.Close()
	runs := make([]RunRow, 0, 3)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repeat group %s: %w", groupID, err)
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("repeat group %s not found", groupID)
	}
	return runs, nil
}

// LoadCaseResults loads every case result for the run IDs in deterministic
// order. detail is decoded into a map so score.Aggregate can see JSON booleans
// exactly as the scorer wrote them.
func (s *PostgresSource) LoadCaseResults(ctx context.Context, runIDs []string) ([]CaseRow, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT eval_run_id::text, scenario_id, failure_mode, correct, false_fix, escalated,
		        tier_expected, tier_actual, confidence::text, cost_usd::text,
		        latency_ms, steps, detail::text
		   FROM eval_case_results
		  WHERE eval_run_id = ANY($1::uuid[])
		  ORDER BY eval_run_id, scenario_id`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("query case results: %w", err)
	}
	defer rows.Close()
	out := make([]CaseRow, 0, len(runIDs)*30)
	for rows.Next() {
		var (
			row        CaseRow
			confidence *string
			detailText string
		)
		if err := rows.Scan(&row.RunID, &row.ScenarioID, &row.FailureMode, &row.Correct,
			&row.FalseFix, &row.Escalated, &row.TierExpected, &row.TierActual,
			&confidence, &row.CostUSD, &row.LatencyMS, &row.Steps, &detailText); err != nil {
			return nil, fmt.Errorf("scan case result: %w", err)
		}
		if confidence != nil {
			parsed, err := decimal.NewFromString(*confidence)
			if err != nil {
				return nil, fmt.Errorf("run %s scenario %s confidence %q: %w",
					row.RunID, row.ScenarioID, *confidence, err)
			}
			row.Confidence = &parsed
		}
		if err := json.Unmarshal([]byte(detailText), &row.Detail); err != nil {
			return nil, fmt.Errorf("run %s scenario %s detail is not a JSON object: %w",
				row.RunID, row.ScenarioID, err)
		}
		if row.Detail == nil {
			row.Detail = map[string]any{}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate case results: %w", err)
	}
	return out, nil
}

const runSelect = `SELECT id::text, suite, split, dataset_version, model, prompt_version, seed,
	       COALESCE(git_sha, ''), config::text, started_at, ended_at,
	       COALESCE(repeat_group::text, ''), repeat_index
	  FROM eval_runs`

type scanRow interface {
	Scan(dest ...any) error
}

func scanRun(row scanRow) (RunRow, error) {
	var (
		run        RunRow
		split      string
		configText string
	)
	if err := row.Scan(&run.ID, &run.Suite, &split, &run.DatasetVersion, &run.Model,
		&run.PromptVersion, &run.Seed, &run.GitSHA, &configText, &run.StartedAt,
		&run.EndedAt, &run.RepeatGroup, &run.RepeatIndex); err != nil {
		return RunRow{}, err
	}
	run.Split = dataset.Split(strings.TrimSpace(split))
	if !run.Split.Valid() {
		return RunRow{}, fmt.Errorf("eval run %s has unknown split %q", run.ID, split)
	}
	config, err := parseRunConfig(configText)
	if err != nil {
		return RunRow{}, fmt.Errorf("eval run %s config: %w", run.ID, err)
	}
	run.Aborted = config.aborted
	run.AbortReason = config.abortReason
	run.CostCapUSD = config.costCap
	return run, nil
}

type runConfigView struct {
	aborted     bool
	abortReason string
	costCap     *string
}

// parseRunConfig reads only the whitelisted run-status keys. Arbitrary config
// content (pricing snapshots, storage paths, DSN-like values) is never copied
// into a report model.
func parseRunConfig(text string) (runConfigView, error) {
	if strings.TrimSpace(text) == "" {
		return runConfigView{}, nil
	}
	var document struct {
		Aborted     bool            `json:"aborted"`
		AbortReason json.RawMessage `json:"abort_reason"`
		CostCap     json.RawMessage `json:"cost_cap"`
	}
	if err := json.Unmarshal([]byte(text), &document); err != nil {
		return runConfigView{}, fmt.Errorf("decode JSON: %w", err)
	}
	view := runConfigView{aborted: document.Aborted}
	if reason, ok := rawString(document.AbortReason); ok {
		view.abortReason = reason
	}
	if cap, ok := rawString(document.CostCap); ok {
		parsed, err := decimal.NewFromString(cap)
		if err != nil {
			return runConfigView{}, fmt.Errorf("cost_cap %q is not a decimal", cap)
		}
		if parsed.IsNegative() {
			return runConfigView{}, fmt.Errorf("cost_cap %q is negative", cap)
		}
		formatted := parsed.StringFixed(6)
		view.costCap = &formatted
	}
	return view, nil
}

func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, true
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String(), true
	}
	return "", false
}
