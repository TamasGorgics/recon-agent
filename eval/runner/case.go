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
	"github.com/TamasGorgics/recon-agent/eval/e2e"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/eval/score"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

type runExecution struct {
	run          runRow
	opts         Options
	rd           *resolvedDataset
	sandbox      *sandbox.Sandbox
	results      *pgxpool.Pool
	deps         Deps
	runtimeCfg   config.Config
	logger       *slog.Logger
	truth        map[string]dataset.ScenarioTruth
	costs        *costState
	resumed      bool
	cassetteDir  string
	recordClient llm.Client
	policyPath   string
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
	} else if e.opts.LLM == LLMRecord {
		client = e.recordClient
	}

	mode := agentrun.ModeScripted
	if e.opts.LLM == LLMLive || e.opts.LLM == LLMRecord {
		mode = agentrun.ModeOpencode
	}
	cassetteDir := e.cassetteDir
	if cassetteDir == "" {
		cassetteDir = resolveCassetteDir(e.opts, e.deps)
	}
	report, runErr := agentrun.Run(ctx, agentrun.Options{
		CaseID:        ref.CaseID,
		Mode:          mode,
		PromptVersion: e.opts.Prompt,
		Model:         e.opts.Model,
		CassetteDir:   cassetteDir,
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

	tierActual := facts.ProposalRiskTier
	var (
		e2eResult *e2e.Result
		e2eErr    error
	)
	if e.opts.E2E {
		result, runErr := e.runE2E(ctx, ref, facts)
		e2eResult = &result
		e2eErr = runErr
		tierActual = result.PolicyTier
	}

	actual := score.Actual{
		Outcome:        facts.Outcome,
		RetrievedRefs:  facts.RetrievedRefs,
		CostUSD:        facts.CostUSD,
		Steps:          facts.ToolCalls,
		InjectionFlags: facts.InjectionFlags,
		TierActual:     tierActual,
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

	if e2eResult != nil {
		// The verifier projection cannot read truth, so W3 stamps the
		// truth-equivalence false pass once ScoreCase has decided FalseFix.
		if e2eResult.Verifier != nil {
			e2eResult.Verifier.FalsePass = e2eResult.Verifier.Passed && result.FalseFix
		}
		if err := e2eResult.Validate(); err != nil {
			fallback := failedE2EResult(facts.ProposalID, facts.ProposalRiskTier, err)
			e2eResult = &fallback
			if e2eErr == nil {
				e2eErr = fmt.Errorf("%w: invalid result: %v", e2e.ErrE2EFailed, err)
			}
		}
		for key, value := range e2eResult.Detail() {
			detail[key] = value
		}
	}

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
	if e2eErr != nil {
		return fmt.Errorf("eval/runner: scenario %s: %w", ref.ScenarioID, e2eErr)
	}
	return nil
}

var errNoAgentRun = errors.New("agent run row not found")

// runE2E drives one case through the real approval/executor/tie-out harness.
// The harness is case-scoped because every service uses the case's fake clock.
// A missing statement-balance date skips tie-out (the pipeline only computes it
// when both scope and as-of are supplied); a harness or pipeline error returns
// a persisted partial result and an error wrapping e2e.ErrE2EFailed.
func (e *runExecution) runE2E(ctx context.Context, ref sandbox.CaseRef,
	facts caseFacts) (e2e.Result, error) {
	harness, err := e2e.NewHarness(ctx, e2e.HarnessOptions{
		Admin:         e.sandbox.Admin,
		App:           e.sandbox.App,
		Exec:          e.sandbox.Exec,
		Clock:         clock.NewFake(ref.ScenarioTime),
		PolicyPath:    e.policyPath,
		ExecutorRunID: "executor:eval:" + e.run.ID,
		LedgerConfig:  e.rd.Config.Ledger,
	})
	if err != nil {
		return failedE2EResult(facts.ProposalID, facts.ProposalRiskTier, err),
			fmt.Errorf("%w: build harness: %v", e2e.ErrE2EFailed, err)
	}

	input := e2e.Input{
		ProposalID:       facts.ProposalID,
		CaseID:           ref.CaseID,
		AgentClaimedTier: facts.ProposalRiskTier,
	}
	if caseFacts, err := harness.Loader.LoadCaseFacts(ctx, ref.CaseID); err == nil {
		input.ScopeID = caseFacts.ScopeID
		input.AsOf = statementAsOf(ctx, e.sandbox.App, caseFacts.ScopeID, ref.ScenarioTime)
	}
	return harness.RunCase(ctx, input)
}

// statementAsOf returns the latest statement-balance date at or before the
// scenario instant for the case scope. A scope without a statement (for
// example a PSP-only scope) yields the zero time, which makes the pipeline skip
// tie-out instead of failing on a missing statement row. A non-zero unexplained
// amount remains a diagnostic, not a pipeline error.
func statementAsOf(ctx context.Context, pool *pgxpool.Pool, scopeID string,
	scenario time.Time) time.Time {
	if pool == nil || scopeID == "" || scenario.IsZero() {
		return time.Time{}
	}
	var asOf time.Time
	if err := pool.QueryRow(ctx,
		`SELECT as_of_date
		   FROM statement_balances
		  WHERE scope_id = $1::uuid
		    AND as_of_date <= $2::date
		  ORDER BY as_of_date DESC
		  LIMIT 1`, scopeID, scenario.UTC()).Scan(&asOf); err != nil {
		return time.Time{}
	}
	return asOf.UTC()
}

// failedE2EResult builds a valid eval-e2e/v1 document for a failure that
// happened before RunCase could return one (for example harness construction).
func failedE2EResult(proposalID string, claimedTier *string, cause error) e2e.Result {
	result := e2e.Result{
		Schema:         e2e.SchemaVersion,
		Proposal:       e2e.ProposalNone,
		RequestStatus:  e2e.RequestStatusNone,
		ApproverAction: e2e.ActionNone,
		Failed:         true,
		Error:          truncateError(cause),
	}
	if proposalID != "" && tools.IsUUID(proposalID) {
		result.Proposal = e2e.ProposalPresent
		result.ProposalID = strings.ToLower(proposalID)
		result.AgentClaimedTier = claimedTier
	}
	if err := result.Validate(); err != nil {
		// Never persist an invalid closed-schema document.
		return e2e.Result{
			Schema:         e2e.SchemaVersion,
			Proposal:       e2e.ProposalNone,
			RequestStatus:  e2e.RequestStatusNone,
			ApproverAction: e2e.ActionNone,
			Failed:         true,
			Error:          truncateError(err),
		}
	}
	return result
}

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
