package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/eval/score"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

type runExecution struct {
	run        runRow
	opts       Options
	rd         *resolvedDataset
	sandbox    *sandbox.Sandbox
	results    *pgxpool.Pool
	deps       Deps
	runtimeCfg config.Config
	logger     *slog.Logger
	truth      map[string]dataset.ScenarioTruth
	costs      *costState
	resumed    bool
}

// caseResultRow is one eval_case_results insert.
type caseResultRow struct {
	ScenarioID   string
	FailureMode  string
	Correct      bool
	FalseFix     bool
	Escalated    bool
	TierExpected *string
	TierActual   *string
	Confidence   *decimal.Decimal
	CostUSD      decimal.Decimal
	LatencyMS    int
	Steps        int
	Detail       map[string]any
}

type caseFacts struct {
	RunID          string
	Outcome        string
	Model          string
	PromptVersion  string
	CostUSD        decimal.Decimal
	TokensIn       int64
	TokensOut      int64
	StartedAt      time.Time
	EndedAt        *time.Time
	InjectionFlags []string

	ProposalID         string
	ProposalType       string
	ProposalPayload    json.RawMessage
	ProposalEvidence   []string
	ProposalConfidence *decimal.Decimal
	ProposalRiskTier   *string

	ToolCalls     int
	ToolCallMS    int64
	LLMCalls      int
	LLMCallMS     int64
	RetrievedRefs []string
}

func (e *runExecution) executeCase(ctx context.Context, ref sandbox.CaseRef) error {
	truth, ok := e.truth[ref.ScenarioID]
	if !ok {
		return fmt.Errorf("eval/runner: no ground truth for scenario %s", ref.ScenarioID)
	}

	var client llm.Client
	if e.deps.ClientForScenario != nil {
		var err error
		client, err = e.deps.ClientForScenario(ref, truth)
		if err != nil {
			return fmt.Errorf("eval/runner: build client for scenario %s: %w", ref.ScenarioID, err)
		}
	}

	mode := agentrun.ModeScripted
	if e.opts.LLM == LLMLive {
		mode = agentrun.ModeOpencode
	}
	report, runErr := agentrun.Run(ctx, agentrun.Options{
		CaseID:        ref.CaseID,
		Mode:          mode,
		PromptVersion: e.opts.Prompt,
		Model:         e.opts.Model,
		CassetteDir:   e.deps.CassetteDir,
	}, agentrun.Deps{
		Config:    e.runtimeCfg,
		App:       e.sandbox.App,
		Agent:     e.sandbox.Agent,
		AgentExec: e.sandbox.Agent,
		Clock:     clock.NewFake(ref.ScenarioTime),
		Logger:    e.logger,
		Client:    client,
		WorkerID:  "eval:" + e.run.ID + ":" + ref.ScenarioID,
	})

	if runErr != nil {
		if errors.Is(runErr, llm.ErrCassetteMiss) {
			return fmt.Errorf("cassette_miss for scenario %s: %w", ref.ScenarioID, runErr)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	facts, err := e.loadFacts(ctx, ref, report)
	if err != nil {
		if runErr != nil && errors.Is(err, errNoAgentRun) {
			return runErr
		}
		return err
	}
	if facts.Outcome == "" {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("eval/runner: scenario %s agent run %s has no terminal outcome",
			ref.ScenarioID, facts.RunID)
	}

	actual := score.Actual{
		Outcome:        facts.Outcome,
		RetrievedRefs:  facts.RetrievedRefs,
		CostUSD:        facts.CostUSD,
		Steps:          facts.ToolCalls,
		InjectionFlags: facts.InjectionFlags,
		TierActual:     facts.ProposalRiskTier,
		Confidence:     facts.ProposalConfidence,
		CostDetail: &score.CostDetail{
			Allowance:  decimal.Zero,
			TokensIn:   facts.TokensIn,
			TokensOut:  facts.TokensOut,
			CostSource: "agent_runs",
		},
		LatencyDetail: &score.LatencyDetail{
			LLMCallMS:  facts.LLMCallMS,
			ToolCallMS: facts.ToolCallMS,
		},
		LoopSteps: &facts.LLMCalls,
	}
	if facts.EndedAt != nil {
		actual.Latency = facts.EndedAt.Sub(facts.StartedAt)
	}
	if facts.ProposalID != "" {
		actual.Proposal = &score.Proposal{
			Type:     facts.ProposalType,
			Payload:  facts.ProposalPayload,
			Evidence: facts.ProposalEvidence,
		}
	}

	result, err := score.ScoreCase(truth, actual, e.opts.Split)
	if err != nil {
		return fmt.Errorf("eval/runner: score scenario %s: %w", ref.ScenarioID, err)
	}

	detail := make(map[string]any, len(result.Detail)+6)
	for key, value := range result.Detail {
		detail[key] = value
	}
	detail["agent_run_id"] = facts.RunID
	detail["outcome"] = facts.Outcome
	detail["break_signature"] = truth.BreakSignature
	detail["synthetic_case"] = ref.Synthetic
	if facts.ProposalID != "" {
		detail["proposal_id"] = facts.ProposalID
	}
	if runErr != nil {
		detail["error"] = truncateError(runErr)
	}
	if facts.EndedAt == nil {
		detail[score.DetailLatency] = map[string]any{
			"note": "incomplete run; latency is supplied only for completed agent runs",
		}
	}
	// Persist the sanitized, bounded transcript before the sandbox is
	// destroyed; reports render it after the clone is gone.
	detail["transcript"] = transcriptSnapshot(report, facts.Outcome, facts.ProposalID, facts.ProposalType)

	if err := insertCaseResult(ctx, e.results, e.run.ID, caseResultRow{
		ScenarioID:   ref.ScenarioID,
		FailureMode:  result.Mode,
		Correct:      result.Correct,
		FalseFix:     result.FalseFix,
		Escalated:    result.Escalated,
		TierExpected: result.TierExpected,
		TierActual:   result.TierActual,
		Confidence:   result.Confidence,
		CostUSD:      result.CostUSD,
		LatencyMS:    int(result.Latency.Milliseconds()),
		Steps:        result.Steps,
		Detail:       detail,
	}); err != nil {
		return err
	}

	spent, _, err := loadSpent(ctx, e.results, e.run.ID)
	if err != nil {
		return err
	}
	e.costs.raise(spent)
	return nil
}

var errNoAgentRun = errors.New("agent run row not found")

func (e *runExecution) loadFacts(ctx context.Context, ref sandbox.CaseRef,
	report agentrun.Report) (caseFacts, error) {
	query := `SELECT id::text, COALESCE(outcome, ''), model, prompt_version,
	                 cost_usd::text, tokens_in, tokens_out, started_at, ended_at,
	                 injection_flags::text
	            FROM agent_runs
	           WHERE case_id = $1::uuid`
	args := []any{ref.CaseID}
	if report.RunID != "" {
		query += ` AND id = $2::uuid`
		args = append(args, report.RunID)
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT 1`

	var facts caseFacts
	var (
		costText  string
		flagsJSON string
	)
	err := e.sandbox.App.QueryRow(ctx, query, args...).Scan(
		&facts.RunID, &facts.Outcome, &facts.Model, &facts.PromptVersion,
		&costText, &facts.TokensIn, &facts.TokensOut, &facts.StartedAt,
		&facts.EndedAt, &flagsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return caseFacts{}, errNoAgentRun
	}
	if err != nil {
		return caseFacts{}, fmt.Errorf("eval/runner: read agent run for case %s: %w", ref.CaseID, err)
	}
	parsedCost, err := decimal.NewFromString(costText)
	if err != nil {
		return caseFacts{}, fmt.Errorf("eval/runner: parse run cost %q: %w", costText, err)
	}
	facts.CostUSD = parsedCost
	if strings.TrimSpace(flagsJSON) != "" {
		if err := json.Unmarshal([]byte(flagsJSON), &facts.InjectionFlags); err != nil {
			return caseFacts{}, fmt.Errorf("eval/runner: decode injection flags for run %s: %w", facts.RunID, err)
		}
	}
	if facts.StartedAt.IsZero() {
		return caseFacts{}, fmt.Errorf("eval/runner: agent run %s has no started_at", facts.RunID)
	}
	facts.StartedAt = facts.StartedAt.UTC()
	if facts.EndedAt != nil {
		ended := facts.EndedAt.UTC()
		facts.EndedAt = &ended
	}

	if err := e.loadProposal(ctx, &facts); err != nil {
		return caseFacts{}, err
	}
	if err := e.loadToolFacts(ctx, &facts); err != nil {
		return caseFacts{}, err
	}
	if err := e.loadLLMFacts(ctx, &facts); err != nil {
		return caseFacts{}, err
	}
	return facts, nil
}

func (e *runExecution) loadProposal(ctx context.Context, facts *caseFacts) error {
	var (
		payloadJSON  []byte
		evidenceJSON []byte
		confidence   *string
	)
	err := e.sandbox.App.QueryRow(ctx,
		`SELECT id::text, type, payload, evidence, confidence::text, risk_tier
		   FROM proposals
		  WHERE agent_run_id = $1::uuid
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`, facts.RunID).
		Scan(&facts.ProposalID, &facts.ProposalType, &payloadJSON,
			&evidenceJSON, &confidence, &facts.ProposalRiskTier)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("eval/runner: read proposal for run %s: %w", facts.RunID, err)
	}
	facts.ProposalPayload = append(json.RawMessage(nil), payloadJSON...)
	if len(evidenceJSON) > 0 {
		if err := json.Unmarshal(evidenceJSON, &facts.ProposalEvidence); err != nil {
			return fmt.Errorf("eval/runner: decode proposal evidence for run %s: %w", facts.RunID, err)
		}
	}
	if confidence != nil {
		parsed, err := decimal.NewFromString(*confidence)
		if err != nil {
			return fmt.Errorf("eval/runner: parse proposal confidence %q: %w", *confidence, err)
		}
		facts.ProposalConfidence = &parsed
	}
	return nil
}

func (e *runExecution) loadToolFacts(ctx context.Context, facts *caseFacts) error {
	if err := e.sandbox.App.QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(duration_ms), 0)
		   FROM tool_calls WHERE agent_run_id = $1::uuid`, facts.RunID).
		Scan(&facts.ToolCalls, &facts.ToolCallMS); err != nil {
		return fmt.Errorf("eval/runner: read tool calls for run %s: %w", facts.RunID, err)
	}

	rows, err := e.sandbox.App.Query(ctx,
		`SELECT result FROM tool_calls
		  WHERE agent_run_id = $1::uuid AND error IS NULL
		  ORDER BY seq`, facts.RunID)
	if err != nil {
		return fmt.Errorf("eval/runner: read tool results for run %s: %w", facts.RunID, err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	refs := make([]string, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("eval/runner: scan tool result for run %s: %w", facts.RunID, err)
		}
		extracted, err := tools.ExtractRefs(raw)
		if err != nil {
			return fmt.Errorf("eval/runner: extract refs from run %s tool result: %w", facts.RunID, err)
		}
		for _, ref := range extracted {
			if _, duplicate := seen[ref]; duplicate {
				continue
			}
			seen[ref] = struct{}{}
			refs = append(refs, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("eval/runner: iterate tool results for run %s: %w", facts.RunID, err)
	}
	sort.Strings(refs)
	facts.RetrievedRefs = refs
	return nil
}

func (e *runExecution) loadLLMFacts(ctx context.Context, facts *caseFacts) error {
	if err := e.sandbox.App.QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(duration_ms), 0)
		   FROM llm_calls WHERE agent_run_id = $1::uuid`, facts.RunID).
		Scan(&facts.LLMCalls, &facts.LLMCallMS); err != nil {
		return fmt.Errorf("eval/runner: read llm calls for run %s: %w", facts.RunID, err)
	}
	return nil
}
