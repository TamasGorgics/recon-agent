package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

const (
	testCaseID    = "11111111-1111-4111-8111-111111111111"
	testRunID     = "22222222-2222-4222-8222-222222222222"
	testLeaseOwn  = "worker-1"
	testModel     = "test-model"
	testActual    = "test-model-2026"
	emptySchema   = `{"type":"object","additionalProperties":false}`
	caseIDSchama  = `{"type":"object","additionalProperties":false,"properties":{"case_id":{"type":"string"}}}`
	repairNoTool  = "Continue the investigation with a tool call, or call escalate."
	repairBadArgs = "Your tool call arguments were not valid JSON. Reply with a single tool call whose arguments are a JSON object matching the tool schema."
)

func fixedClock() *clock.Fake {
	return clock.NewFake(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
}

func newTestConfig() config.AgentConfig {
	cfg := config.DefaultAgentConfig()
	cfg.Model = testModel
	return cfg
}

// ---------------------------------------------------------------- fake store

type fakeStore struct {
	mu        sync.Mutex
	runs      map[string]agent.RunRecord
	calls     []agent.LLMCall
	finishes  []agent.RunFinish
	proposals map[string]agent.ProposalRef
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		runs:      make(map[string]agent.RunRecord),
		proposals: make(map[string]agent.ProposalRef),
	}
}

func (s *fakeStore) CreateRun(_ context.Context, run agent.RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[run.ID]; exists {
		return fmt.Errorf("duplicate run %s", run.ID)
	}
	s.runs[run.ID] = run
	return nil
}

func (s *fakeStore) RecordLLMCall(_ context.Context, call agent.LLMCall) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	return nil
}

func (s *fakeStore) FinishRun(_ context.Context, finish agent.RunFinish) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishes = append(s.finishes, finish)
	return nil
}

func (s *fakeStore) ProposalForRun(_ context.Context, runID string) (agent.ProposalRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	proposal, ok := s.proposals[runID]
	if !ok {
		return agent.ProposalRef{}, fmt.Errorf("%w: %s", agent.ErrProposalNotFound, runID)
	}
	return proposal, nil
}

func (s *fakeStore) addProposal(runID, id, typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proposals[runID] = agent.ProposalRef{ID: id, Type: typ, Status: "proposed"}
}

func (s *fakeStore) run(id string) agent.RunRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

func (s *fakeStore) llmCalls() []agent.LLMCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.LLMCall(nil), s.calls...)
}

func (s *fakeStore) finalizes() []agent.RunFinish {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.RunFinish(nil), s.finishes...)
}

// ---------------------------------------------------------------- fake cases

type fakeCases struct {
	mu          sync.Mutex
	current     cases.Case
	gets        int
	transitions []cases.TransitionRequest
	getErr      error
	transition  func(call int, req cases.TransitionRequest, current cases.Case) (cases.Case, error)
}

func newFakeCases() *fakeCases {
	owner := testLeaseOwn
	return &fakeCases{current: cases.Case{
		ID:        testCaseID,
		Status:    cases.StatusInAgent,
		Version:   3,
		LeasedBy:  &owner,
		Currency:  "USD",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
}

func (f *fakeCases) Get(ctx context.Context, _ string) (cases.Case, error) {
	if err := ctx.Err(); err != nil {
		return cases.Case{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return cases.Case{}, f.getErr
	}
	f.gets++
	return f.current, nil
}

func (f *fakeCases) Transition(_ context.Context, req cases.TransitionRequest) (cases.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, req)
	if f.transition != nil {
		updated, err := f.transition(len(f.transitions), req, f.current)
		if updated.ID != "" {
			f.current = updated
		}
		return updated, err
	}
	if req.ExpectedVersion != f.current.Version {
		return cases.Case{}, cases.ErrStale
	}
	f.current.Status = req.Status
	f.current.Version = req.ExpectedVersion + 1
	if req.EscalationReason != "" {
		reason := req.EscalationReason
		f.current.EscalationReason = &reason
	}
	return f.current, nil
}

func (f *fakeCases) transitionRequests() []cases.TransitionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cases.TransitionRequest(nil), f.transitions...)
}

func (f *fakeCases) snapshot() cases.Case {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

// ---------------------------------------------------------------- fake switch

type fakeSwitch struct {
	mu       sync.Mutex
	values   []bool
	fallback bool
}

func (s *fakeSwitch) AgentEnabled(context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.values) > 0 {
		value := s.values[0]
		s.values = s.values[1:]
		return value, nil
	}
	return s.fallback, nil
}

// ---------------------------------------------------------------- recorder

type fakeRecorder struct {
	mu    sync.Mutex
	calls []tools.ToolCall
	err   error
}

func (r *fakeRecorder) RecordToolCall(_ context.Context, call tools.ToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, call)
	return nil
}

func (r *fakeRecorder) recorded() []tools.ToolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tools.ToolCall(nil), r.calls...)
}

// ---------------------------------------------------------------- fake execer

type fakeExecer struct {
	mu    sync.Mutex
	args  [][]any
	calls int
}

func (e *fakeExecer) Exec(_ context.Context, _ string, arguments ...any) (pgconn.CommandTag, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.args = append(e.args, append([]any(nil), arguments...))
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (e *fakeExecer) lastArgs() []any {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.args) == 0 {
		return nil
	}
	return e.args[len(e.args)-1]
}

// ---------------------------------------------------------------- test tools

type toolHooks struct {
	getCaseFlags []string
	getCaseErr   error
	readFlags    []string
	readErr      error
}

type testTool struct {
	name   string
	schema json.RawMessage
	call   func(ctx context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error)
}

func (t *testTool) Name() string                 { return t.name }
func (t *testTool) Description() string          { return "test tool " + t.name }
func (t *testTool) InputSchema() json.RawMessage { return t.schema }
func (t *testTool) Call(ctx context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error) {
	return t.call(ctx, run, args)
}

func newTestRegistry(t *testing.T, store *fakeStore, hooks toolHooks) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	add := func(tool *testTool) {
		t.Helper()
		if err := registry.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.name, err)
		}
	}

	add(&testTool{
		name:   "get_case",
		schema: json.RawMessage(caseIDSchama),
		call: func(_ context.Context, run *tools.RunContext, _ json.RawMessage) (tools.Result, error) {
			if hooks.getCaseErr != nil {
				return tools.Result{}, hooks.getCaseErr
			}
			run.State().AddFlags(hooks.getCaseFlags...)
			return tools.Result{Data: map[string]any{
				"case_id":          run.CaseID,
				"summary":          "benign memo text",
				"untrusted_fields": []string{"summary"},
			}}, nil
		},
	})
	add(&testTool{
		name:   "read_thing",
		schema: json.RawMessage(emptySchema),
		call: func(_ context.Context, run *tools.RunContext, _ json.RawMessage) (tools.Result, error) {
			if hooks.readErr != nil {
				return tools.Result{}, hooks.readErr
			}
			run.State().AddFlags(hooks.readFlags...)
			return tools.Result{Data: map[string]any{"value": "read ok"}}, nil
		},
	})
	add(&testTool{
		name:   "propose_adjustment",
		schema: json.RawMessage(emptySchema),
		call: func(_ context.Context, run *tools.RunContext, _ json.RawMessage) (tools.Result, error) {
			if !run.State().MarkProposalUsed("proposal:" + run.RunID) {
				return tools.Result{}, errors.New("proposal slot already used")
			}
			store.addProposal(run.RunID, "33333333-3333-4333-8333-333333333333", "adjustment")
			return tools.Result{Data: map[string]any{"status": "recorded"}}, nil
		},
	})
	add(&testTool{
		name:   "request_info",
		schema: json.RawMessage(emptySchema),
		call: func(_ context.Context, run *tools.RunContext, _ json.RawMessage) (tools.Result, error) {
			if !run.State().MarkProposalUsed("proposal:" + run.RunID) {
				return tools.Result{}, errors.New("proposal slot already used")
			}
			store.addProposal(run.RunID, "44444444-4444-4444-8444-444444444444", "request_info")
			return tools.Result{Data: map[string]any{"status": "recorded"}}, nil
		},
	})
	add(&testTool{
		name:   "escalate",
		schema: json.RawMessage(emptySchema),
		call: func(_ context.Context, run *tools.RunContext, _ json.RawMessage) (tools.Result, error) {
			run.State().SetEscalation("ambiguous_evidence", "needs an operator")
			return tools.Result{Data: map[string]any{"status": "escalated"}}, nil
		},
	})
	return registry
}

// ---------------------------------------------------------------- stub queryer

type stubQueryer struct{}

func (stubQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("stub queryer: Query must not be called")
}

func (stubQueryer) QueryRow(context.Context, string, ...any) pgx.Row {
	return stubRow{}
}

type stubRow struct{}

func (stubRow) Scan(...any) error { return errors.New("stub queryer: Scan must not be called") }

// ---------------------------------------------------------------- runner harness

type runnerOptions struct {
	config        config.AgentConfig
	clock         *clock.Fake
	store         *fakeStore
	cases         *fakeCases
	steps         []llm.ScriptedStep
	switchValues  []bool
	switchDefault bool
	flagsExec     *fakeExecer
	beforeStep    func(context.Context) error
	hooks         toolHooks
	newRunID      func(string) (string, error)
	logger        *slog.Logger
}

type runnerHarness struct {
	runner   *agent.Runner
	store    *fakeStore
	cases    *fakeCases
	scripted *llm.Scripted
	recorder *fakeRecorder
	clock    *clock.Fake
	flags    *fakeExecer
}

func newHarness(t *testing.T, opts runnerOptions) *runnerHarness {
	t.Helper()
	if opts.config.Model == "" {
		opts.config = newTestConfig()
	}
	if opts.clock == nil {
		opts.clock = fixedClock()
	}
	if opts.store == nil {
		opts.store = newFakeStore()
	}
	if opts.cases == nil {
		opts.cases = newFakeCases()
	}
	if opts.flagsExec == nil {
		opts.flagsExec = &fakeExecer{}
	}
	if opts.newRunID == nil {
		opts.newRunID = func(string) (string, error) { return testRunID, nil }
	}
	registry := newTestRegistry(t, opts.store, opts.hooks)
	recorder := &fakeRecorder{}
	scripted := llm.NewScripted(opts.steps...)

	runner, err := agent.NewRunner(agent.Deps{
		Store:      opts.store,
		Cases:      opts.cases,
		Agent:      stubQueryer{},
		Client:     scripted,
		Registry:   registry,
		Recorder:   recorder,
		Clock:      opts.clock,
		Config:     opts.config,
		Switch:     &fakeSwitch{values: opts.switchValues, fallback: true},
		FlagsExec:  opts.flagsExec,
		NewRunID:   opts.newRunID,
		BeforeStep: opts.beforeStep,
		Logger:     opts.logger,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return &runnerHarness{
		runner: runner, store: opts.store, cases: opts.cases,
		scripted: scripted, recorder: recorder, clock: opts.clock, flags: opts.flagsExec,
	}
}

// ---------------------------------------------------------------- responses

func usageWith(promptTokens, completionTokens int, cost string) llm.Usage {
	costDecimal, err := decimal.NewFromString(cost)
	if err != nil {
		panic(err)
	}
	return llm.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		CostUSD:          costDecimal,
	}
}

func toolResponse(id, name, args string) llm.Response {
	return llm.Response{
		Provider:       llm.ProviderOpenCodeGo,
		ActualModel:    testActual,
		Message:        llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}}},
		StopReason:     llm.StopToolCalls,
		Usage:          usageWith(10, 5, "0.01"),
		RequestedModel: testModel,
	}
}

func textResponse(content string) llm.Response {
	return llm.Response{
		Provider:       llm.ProviderOpenCodeGo,
		ActualModel:    testActual,
		Message:        llm.Message{Role: llm.RoleAssistant, Content: content},
		StopReason:     llm.StopStop,
		Usage:          usageWith(10, 5, "0.01"),
		RequestedModel: testModel,
	}
}
