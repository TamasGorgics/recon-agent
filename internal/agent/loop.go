package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

const repeatedCallReason = "repeated_identical_tool_call"

const injectionFlagReason = "injection_flagged"

// Result is the terminal report for one run. A nil error is returned for all
// five persisted outcomes.
type Result struct {
	RunID        string
	Outcome      Outcome
	ProposalID   string
	ProposalType string
	Escalation   *tools.Escalation
	CaseVersion  int
	TokensIn     int64
	TokensOut    int64
	CostUSD      decimal.Decimal
	Steps        int
	ToolCalls    int
	RepairUsed   bool
	LeaseLost    bool
}

// CaseService is the cases.Service surface owned by the loop.
type CaseService interface {
	Get(ctx context.Context, caseID string) (cases.Case, error)
	Transition(ctx context.Context, req cases.TransitionRequest) (cases.Case, error)
}

// Deps is the injected runner dependency set. Unit tests provide fakes; setup
// wires the production app/agent pools, read and proposal tools, and switch.
type Deps struct {
	Store      RunStore
	Cases      CaseService
	Agent      tools.Queryer
	Client     llm.Client
	Registry   *tools.Registry
	Recorder   tools.Recorder
	ToolRunner *tools.Runner
	Clock      clock.Clock
	Config     config.AgentConfig
	Switch     SwitchReader
	FlagsExec  tools.Execer
	NewRunID   func(caseID string) (string, error)
	BeforeStep func(ctx context.Context) error
	Logger     *slog.Logger
	Provider   string
	BaseURL    string
}

// Runner owns the bounded LLM tool-use loop for one case at a time.
type Runner struct {
	store         RunStore
	cases         CaseService
	agent         tools.Queryer
	client        llm.Client
	registry      *tools.Registry
	toolRunner    *tools.Runner
	clock         clock.Clock
	config        config.AgentConfig
	switchReader  SwitchReader
	flagsExec     tools.Execer
	newRunID      func(caseID string) (string, error)
	beforeStep    func(ctx context.Context) error
	logger        *slog.Logger
	provider      string
	baseURL       string
	model         string
	promptVersion string
}

// NewRunner validates and returns a runner. The caller must supply a complete
// agent config; production uses config.Load defaults.
func NewRunner(deps Deps) (*Runner, error) {
	if deps.Store == nil || deps.Cases == nil || deps.Agent == nil || deps.Client == nil ||
		deps.Registry == nil || deps.Clock == nil || deps.Switch == nil {
		return nil, fmt.Errorf("%w: store, cases, agent queryer, client, registry, clock and switch are required", ErrInvalidConfig)
	}
	if err := deps.Config.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if strings.TrimSpace(deps.Config.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidConfig)
	}
	toolRunner := deps.ToolRunner
	if toolRunner == nil {
		if deps.Recorder == nil {
			return nil, fmt.Errorf("%w: a tool recorder or tool runner is required", ErrInvalidConfig)
		}
		toolRunner = tools.NewRunner(deps.Registry, deps.Recorder, deps.Clock)
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	provider := strings.TrimSpace(deps.Provider)
	if provider == "" {
		provider = llm.ProviderOpenCodeGo
	}
	baseURL := strings.TrimSpace(deps.BaseURL)
	if baseURL == "" {
		baseURL = llm.DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	newRunID := deps.NewRunID
	if newRunID == nil {
		newRunID = func(string) (string, error) { return newUUIDv4() }
	}
	return &Runner{
		store:         deps.Store,
		cases:         deps.Cases,
		agent:         deps.Agent,
		client:        deps.Client,
		registry:      deps.Registry,
		toolRunner:    toolRunner,
		clock:         deps.Clock,
		config:        deps.Config,
		switchReader:  deps.Switch,
		flagsExec:     deps.FlagsExec,
		newRunID:      newRunID,
		beforeStep:    deps.BeforeStep,
		logger:        logger,
		provider:      provider,
		baseURL:       baseURL,
		model:         deps.Config.Model,
		promptVersion: deps.Config.PromptVersion,
	}, nil
}

// Run investigates one leased case. It returns a Result with a nil error for
// the five terminal outcomes. A cassette miss is finalized provider_error and
// returned as an error; ErrAgentDisabled is finalized escalated with no case
// transition. Unexpected errors leave the run row open for the worker to
// release.
func (r *Runner) Run(ctx context.Context, caseID string) (Result, error) {
	caseID = strings.ToLower(strings.TrimSpace(caseID))
	if !tools.IsUUID(caseID) {
		return Result{}, fmt.Errorf("agent: case id %q is not a UUID", caseID)
	}
	prompt, err := loadPrompt(r.promptVersion)
	if err != nil {
		return Result{}, err
	}
	toolDefs, err := toolDefinitions(r.registry)
	if err != nil {
		return Result{}, err
	}
	if len(toolDefs) == 0 {
		return Result{}, fmt.Errorf("%w: no tools registered", ErrInvalidConfig)
	}

	c, err := r.cases.Get(ctx, caseID)
	if err != nil {
		return Result{}, fmt.Errorf("agent: load case %s: %w", caseID, err)
	}
	if c.Version < 0 {
		return Result{}, fmt.Errorf("agent: case %s has an invalid version", caseID)
	}
	if c.Status != cases.StatusInAgent {
		return Result{}, fmt.Errorf("%w: case is %s", ErrNotLeased, c.Status)
	}
	if c.LeasedBy == nil || strings.TrimSpace(*c.LeasedBy) == "" {
		return Result{}, fmt.Errorf("%w: case has no lease owner", ErrNotLeased)
	}
	owner := *c.LeasedBy
	initialVersion := c.Version

	startedAt := r.clock.Now().UTC().Truncate(time.Microsecond)
	runID, err := r.newRunID(caseID)
	if err != nil {
		return Result{}, fmt.Errorf("agent: generate run id: %w", err)
	}
	if !tools.IsUUID(runID) {
		return Result{}, fmt.Errorf("agent: generated run id %q is not a UUID", runID)
	}
	runModel := r.model
	if err := r.store.CreateRun(ctx, RunRecord{
		ID:            runID,
		CaseID:        caseID,
		Model:         runModel,
		PromptVersion: r.promptVersion,
		StartedAt:     startedAt,
	}); err != nil {
		return Result{RunID: runID}, err
	}

	result := Result{RunID: runID, CaseVersion: initialVersion}
	usage := &budgetUsage{Cost: decimal.Zero}
	limits := budgetLimits{
		MaxSteps:       r.config.MaxSteps,
		MaxToolCalls:   r.config.MaxToolCalls,
		MaxTotalTokens: r.config.MaxTotalTokens,
		MaxCost:        r.config.MaxCost(),
		MaxWallClock:   r.config.MaxWallClock,
		StartedAt:      startedAt,
	}
	baseFinish := func() finishRequest {
		return finishRequest{
			result:    result,
			usage:     usage,
			startedAt: startedAt,
			model:     runModel,
			caseID:    caseID,
			owner:     owner,
		}
	}
	budgetExhausted := func() (Result, error) {
		req := baseFinish()
		req.outcome = OutcomeBudgetExhausted
		return r.finishNoTransition(ctx, req)
	}
	invalidOutput := func(repairUsed bool) (Result, error) {
		req := baseFinish()
		req.outcome = OutcomeInvalidOutput
		req.repairUsed = repairUsed
		return r.finishNoTransition(ctx, req)
	}

	// The kill switch is checked before the seed call and before every model
	// completion. A disabled run is finalized escalated without a transition.
	if err := r.checkSwitch(ctx); err != nil {
		if errors.Is(err, ErrAgentDisabled) {
			req := baseFinish()
			req.outcome = OutcomeEscalated
			req.err = ErrAgentDisabled
			return r.finishNoTransition(ctx, req)
		}
		return result, err
	}

	wrapper, err := untrusted.NewRunWrapper(runID)
	if err != nil {
		return result, err
	}
	runCtx := tools.NewRunContext(runID, caseID, r.agent, r.clock)
	seedArgs := json.RawMessage(fmt.Sprintf(`{"case_id":%q}`, caseID))
	seedEnvelope, err := r.toolRunner.Call(ctx, runCtx, 1, "get_case", seedArgs)
	if err != nil {
		return result, fmt.Errorf("agent: seed get_case: %w", err)
	}
	usage.ToolCalls = 1
	seedVisible, err := modelVisibleResult(seedEnvelope, wrapper)
	if err != nil {
		return result, err
	}
	messages := initialTranscript(caseID, seedVisible)
	if runCtx.State().Flagged() {
		return r.finishEscalated(ctx, finishRequest{
			result: result, usage: usage, startedAt: startedAt, model: runModel,
			caseID: caseID, owner: owner,
			flags: runCtx.State().Flags(), reason: injectionReason(runCtx),
		})
	}
	if limits.toolCallsExhausted(*usage) || limits.wallClockExhausted(r.clock.Now()) {
		return budgetExhausted()
	}

	temperature := r.config.TemperatureValue()
	toolChoice := &llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	sessionID := "ses_" + strings.ReplaceAll(runID, "-", "")
	lastKeys := make([]string, 0, 2)
	repairUsed := false
	llmSeq := 0
	nextToolCallID := 2 // the seed assistant message already owns call id 1 (seed_get_case)

	for {
		if limits.stepsExhausted(*usage) || limits.tokensExhausted(*usage) ||
			limits.costExhausted(*usage) || limits.wallClockExhausted(r.clock.Now()) {
			return budgetExhausted()
		}
		if err := r.beforeCompletion(ctx); err != nil {
			if errors.Is(err, ErrAgentDisabled) {
				req := baseFinish()
				req.outcome = OutcomeEscalated
				req.err = ErrAgentDisabled
				return r.finishNoTransition(ctx, req)
			}
			return result, err
		}

		request := llm.Request{
			Model:         runModel,
			System:        prompt.Composed,
			Messages:      append([]llm.Message(nil), messages...),
			Tools:         toolDefs,
			ToolChoice:    toolChoice,
			Temperature:   &temperature, //floatlint:allow provider temperature, not money
			MaxTokens:     r.config.MaxTokensPerResponse,
			SessionID:     sessionID,
			PromptVersion: r.promptVersion,
		}
		requestHash, err := llm.CanonicalRequestHash(r.provider, r.baseURL, request)
		if err != nil {
			return result, fmt.Errorf("agent: hash request: %w", err)
		}
		callStarted := r.clock.Now()
		response, err := r.client.Complete(ctx, request)
		if err != nil {
			if errors.Is(err, llm.ErrCassetteMiss) {
				req := baseFinish()
				req.outcome = OutcomeProviderError
				res, finishErr := r.finishNoTransition(ctx, req)
				if finishErr != nil {
					return res, errors.Join(finishErr, err)
				}
				return res, fmt.Errorf("agent: run %s: %w", runID, err)
			}
			if _, ok := llm.AsProviderError(err); ok {
				r.logger.Warn("agent completion failed", "run_id", runID, "error", err)
				req := baseFinish()
				req.outcome = OutcomeProviderError
				return r.finishNoTransition(ctx, req)
			}
			return result, fmt.Errorf("agent: completion: %w", err)
		}
		duration := clampDuration(r.clock.Since(callStarted))
		actualModel := strings.TrimSpace(response.ActualModel)
		if actualModel == "" {
			actualModel = request.Model
		}
		clampedUsage, clampedFields := sanitizeUsage(response.Usage)
		response.Usage = clampedUsage
		if len(clampedFields) > 0 {
			r.logger.Debug("agent provider usage clamped to zero",
				"run_id", runID, "fields", strings.Join(clampedFields, ","))
		}
		nextToolCallID = ensureToolCallIDs(response.Message.ToolCalls, nextToolCallID)
		responseBody, err := responseJSON(response)
		if err != nil {
			return result, err
		}
		usageBody, err := responseUsageJSON(response)
		if err != nil {
			return result, err
		}
		llmSeq++
		if err := r.store.RecordLLMCall(ctx, LLMCall{
			RunID:       runID,
			Seq:         llmSeq,
			Model:       actualModel,
			RequestHash: requestHash,
			Response:    responseBody,
			Usage:       usageBody,
			StartedAt:   callStarted,
			DurationMS:  duration,
		}); err != nil {
			return result, err
		}
		runModel = actualModel
		usage.Steps++
		usage.addUsage(response.Usage)
		// A completion that pushes the run over the token or cost cap stops
		// before any of its tool calls are executed.
		if limits.tokensExhausted(*usage) || limits.costExhausted(*usage) {
			return budgetExhausted()
		}
		messages = append(messages, response.Message)

		if len(response.Message.ToolCalls) == 0 {
			if !repairUsed {
				repairUsed = true
				lastKeys = lastKeys[:0]
				messages = append(messages, repairMessage(repairNoToolCall))
				continue
			}
			return invalidOutput(true)
		}

		protocolFailure := false
		for _, call := range response.Message.ToolCalls {
			if limits.toolCallsExhausted(*usage) || limits.wallClockExhausted(r.clock.Now()) {
				return budgetExhausted()
			}
			seq := usage.ToolCalls + 1
			if !isJSONObject(call.Arguments) {
				protocolFailure = true
				lastKeys = lastKeys[:0]
				envelope, callErr := r.toolRunner.Call(ctx, runCtx, seq, call.Name, call.Arguments)
				if callErr != nil {
					return result, fmt.Errorf("agent: tool call: %w", callErr)
				}
				usage.ToolCalls++
				wrapped, wrapErr := modelVisibleResult(envelope, wrapper)
				if wrapErr != nil {
					return result, wrapErr
				}
				messages = append(messages, toolMessage(call.ID, wrapped))
				if limits.toolCallsExhausted(*usage) || limits.wallClockExhausted(r.clock.Now()) {
					return budgetExhausted()
				}
				continue
			}

			key, keyErr := canonicalCallKey(call.Name, call.Arguments)
			if keyErr != nil {
				return result, keyErr
			}
			if len(lastKeys) == 2 && lastKeys[0] == key && lastKeys[1] == key {
				req := baseFinish()
				req.reason = repeatedCallReason
				req.repairUsed = repairUsed
				return r.finishEscalated(ctx, req)
			}
			lastKeys = append(lastKeys, key)
			if len(lastKeys) > 2 {
				lastKeys = lastKeys[len(lastKeys)-2:]
			}

			envelope, callErr := r.toolRunner.Call(ctx, runCtx, seq, call.Name, call.Arguments)
			if callErr != nil {
				return result, fmt.Errorf("agent: tool call: %w", callErr)
			}
			usage.ToolCalls++
			wrapped, wrapErr := modelVisibleResult(envelope, wrapper)
			if wrapErr != nil {
				return result, wrapErr
			}
			messages = append(messages, toolMessage(call.ID, wrapped))

			if escalation := runCtx.State().Escalation(); escalation != nil {
				req := baseFinish()
				req.reason = escalation.Reason
				req.repairUsed = repairUsed
				req.escalation = escalation
				return r.finishEscalated(ctx, req)
			}
			if runCtx.State().ProposalKey() != "" {
				req := baseFinish()
				req.repairUsed = repairUsed
				return r.finishProposal(ctx, req)
			}
			if runCtx.State().Flagged() {
				req := baseFinish()
				req.flags = runCtx.State().Flags()
				req.reason = injectionReason(runCtx)
				req.repairUsed = repairUsed
				return r.finishEscalated(ctx, req)
			}
			if limits.toolCallsExhausted(*usage) || limits.wallClockExhausted(r.clock.Now()) {
				return budgetExhausted()
			}
		}
		if protocolFailure {
			if !repairUsed {
				repairUsed = true
				lastKeys = lastKeys[:0]
				messages = append(messages, repairMessage(repairMalformedArgs))
				continue
			}
			return invalidOutput(true)
		}
	}
}

func injectionReason(runCtx *tools.RunContext) string {
	if escalation := runCtx.State().Escalation(); escalation != nil {
		return escalation.Reason
	}
	return injectionFlagReason
}

func (r *Runner) checkSwitch(ctx context.Context) error {
	enabled, err := r.switchReader.AgentEnabled(ctx)
	if err != nil {
		return fmt.Errorf("agent: read kill switch: %w", err)
	}
	if !enabled {
		return ErrAgentDisabled
	}
	return nil
}

func (r *Runner) beforeCompletion(ctx context.Context) error {
	if r.beforeStep != nil {
		if err := r.beforeStep(ctx); err != nil {
			if errors.Is(err, ErrAgentDisabled) {
				return ErrAgentDisabled
			}
			return fmt.Errorf("agent: before step: %w", err)
		}
	}
	return r.checkSwitch(ctx)
}

// finishRequest is the shared terminal path input.
type finishRequest struct {
	result     Result
	usage      *budgetUsage
	startedAt  time.Time
	model      string
	caseID     string
	owner      string
	outcome    Outcome
	reason     string
	flags      []string
	repairUsed bool
	err        error
	escalation *tools.Escalation
	transition *cases.Status
}

func (r *Runner) finishProposal(ctx context.Context, req finishRequest) (Result, error) {
	proposal, err := r.store.ProposalForRun(ctx, req.result.RunID)
	if err != nil {
		return req.result, err
	}
	req.result.ProposalID = proposal.ID
	req.result.ProposalType = proposal.Type
	req.outcome = OutcomeProposal
	target := cases.StatusProposalPending
	if proposal.Type == "request_info" {
		target = cases.StatusWaitingInfo
	}
	req.transition = &target
	return r.finish(ctx, req)
}

func (r *Runner) finishEscalated(ctx context.Context, req finishRequest) (Result, error) {
	req.outcome = OutcomeEscalated
	if req.escalation == nil {
		req.escalation = &tools.Escalation{Reason: req.reason}
	}
	req.result.Escalation = req.escalation
	target := cases.StatusEscalated
	req.transition = &target
	return r.finish(ctx, req)
}

func (r *Runner) finishNoTransition(ctx context.Context, req finishRequest) (Result, error) {
	req.transition = nil
	return r.finish(ctx, req)
}

func (r *Runner) finish(ctx context.Context, req finishRequest) (Result, error) {
	if !req.outcome.Valid() {
		return req.result, fmt.Errorf("agent: invalid terminal outcome %q", req.outcome)
	}
	endedAt := r.clock.Now().UTC().Truncate(time.Microsecond)
	if endedAt.Before(req.startedAt) {
		endedAt = req.startedAt
	}
	flags := normalizedFlags(req.flags)
	usage := req.usage.nonNegative()

	if req.transition != nil {
		version, leaseLost, err := r.transitionCase(ctx, req.caseID, req.owner, *req.transition, req.reason, req.result.CaseVersion)
		if err != nil {
			return req.result, err
		}
		req.result.LeaseLost = leaseLost
		if !leaseLost {
			req.result.CaseVersion = version
		}
	}
	if len(flags) > 0 {
		if r.flagsExec == nil {
			return req.result, fmt.Errorf("%w: flagged run needs a flags writer", ErrInvalidConfig)
		}
		if err := tools.SaveRunFlags(ctx, r.flagsExec, req.result.RunID, flags); err != nil {
			r.logger.Warn("agent save run flags failed", "run_id", req.result.RunID, "error", err)
		}
	}
	if err := r.store.FinishRun(ctx, RunFinish{
		RunID:     req.result.RunID,
		EndedAt:   endedAt,
		TokensIn:  usage.TokensIn,
		TokensOut: usage.TokensOut,
		CostUSD:   usage.Cost,
		Outcome:   req.outcome,
		Flags:     flags,
		Model:     req.model,
	}); err != nil {
		return req.result, err
	}
	req.result.Outcome = req.outcome
	req.result.TokensIn = usage.TokensIn
	req.result.TokensOut = usage.TokensOut
	req.result.CostUSD = usage.Cost
	req.result.Steps = usage.Steps
	req.result.ToolCalls = usage.ToolCalls
	req.result.RepairUsed = req.repairUsed
	r.logger.Info("agent run finished",
		"run_id", req.result.RunID,
		"outcome", string(req.outcome),
		"steps", usage.Steps,
		"tool_calls", usage.ToolCalls,
		"tokens_in", usage.TokensIn,
		"tokens_out", usage.TokensOut,
		"cost_usd", usage.Cost.String())
	if req.err != nil {
		return req.result, req.err
	}
	return req.result, nil
}

// transitionCase applies the M2-09 owner guard. A stale write is retried once
// only when a fresh read still shows in_agent with the captured leased_by. A
// lost lease finalizes without overwriting the case.
func (r *Runner) transitionCase(
	ctx context.Context,
	caseID, owner string,
	status cases.Status,
	reason string,
	expectedVersion int,
) (int, bool, error) {
	if strings.TrimSpace(owner) == "" {
		return expectedVersion, true, nil
	}
	_, err := r.cases.Transition(ctx, cases.TransitionRequest{
		CaseID:           caseID,
		ExpectedVersion:  expectedVersion,
		Status:           status,
		EscalationReason: reason,
	})
	if err == nil {
		return expectedVersion + 1, false, nil
	}
	if !errors.Is(err, cases.ErrStale) {
		return expectedVersion, false, fmt.Errorf("agent: transition case: %w", err)
	}
	fresh, err := r.cases.Get(ctx, caseID)
	if err != nil {
		return expectedVersion, false, fmt.Errorf("agent: re-read case after stale transition: %w", err)
	}
	if fresh.Status != cases.StatusInAgent || fresh.LeasedBy == nil ||
		strings.TrimSpace(*fresh.LeasedBy) != owner {
		return expectedVersion, true, nil
	}
	updated, err := r.cases.Transition(ctx, cases.TransitionRequest{
		CaseID:           caseID,
		ExpectedVersion:  fresh.Version,
		Status:           status,
		EscalationReason: reason,
	})
	if err == nil {
		return updated.Version, false, nil
	}
	if errors.Is(err, cases.ErrStale) {
		return expectedVersion, true, nil
	}
	return expectedVersion, false, fmt.Errorf("agent: retry transition case: %w", err)
}
