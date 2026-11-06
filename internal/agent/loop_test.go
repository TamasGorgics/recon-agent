package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/prompts"
)

func TestRunSeedContextAndRequestShape(t *testing.T) {
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Response: toolResponse("call_1", "read_thing", `{}`)},
		{Response: toolResponse("call_2", "propose_adjustment", `{}`)},
	}})

	result, err := h.runner.Run(context.Background(), strings.ToUpper(testCaseID))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeProposal {
		t.Fatalf("outcome = %s, want proposal", result.Outcome)
	}

	requests := h.scripted.Requests()
	if len(requests) != 2 {
		t.Fatalf("completions = %d, want 2", len(requests))
	}
	first := requests[0]

	prompt, err := prompts.Load("v1")
	if err != nil {
		t.Fatalf("load v1 prompt: %v", err)
	}
	if first.System != prompt.Composed {
		t.Fatalf("system prompt is not the composed v1 prompt")
	}
	if first.Model != testModel {
		t.Fatalf("request model = %q, want %q", first.Model, testModel)
	}
	if first.PromptVersion != "v1" {
		t.Fatalf("request prompt version = %q, want v1", first.PromptVersion)
	}
	if first.MaxTokens != 2048 {
		t.Fatalf("request max tokens = %d, want 2048", first.MaxTokens)
	}
	if first.Temperature == nil || *first.Temperature != 0 {
		t.Fatalf("request temperature = %v, want pointer to 0", first.Temperature)
	}
	if first.ToolChoice == nil || first.ToolChoice.Mode != llm.ToolChoiceAuto {
		t.Fatalf("tool choice = %+v, want auto", first.ToolChoice)
	}
	wantSession := "ses_" + strings.ReplaceAll(testRunID, "-", "")
	if first.SessionID != wantSession {
		t.Fatalf("session id = %q, want %q", first.SessionID, wantSession)
	}

	if len(first.Messages) != 3 {
		t.Fatalf("seed transcript has %d messages, want 3", len(first.Messages))
	}
	if first.Messages[0].Role != llm.RoleUser ||
		first.Messages[0].Content != "Investigate reconciliation case "+testCaseID+". Start from the case data returned by get_case." {
		t.Fatalf("seed user message = %+v", first.Messages[0])
	}
	if first.Messages[1].Role != llm.RoleAssistant || len(first.Messages[1].ToolCalls) != 1 ||
		first.Messages[1].ToolCalls[0].ID != "seed_get_case" ||
		first.Messages[1].ToolCalls[0].Name != "get_case" {
		t.Fatalf("seed assistant message = %+v", first.Messages[1])
	}
	if first.Messages[2].Role != llm.RoleTool || first.Messages[2].ToolCallID != "seed_get_case" {
		t.Fatalf("seed tool message = %+v", first.Messages[2])
	}
	if !strings.Contains(first.Messages[2].Content, "UNTRUSTED:field=summary:token=") {
		t.Fatalf("seed tool result was not untrusted-wrapped: %s", first.Messages[2].Content)
	}

	names := make([]string, 0, len(first.Tools))
	for _, tool := range first.Tools {
		names = append(names, tool.Name)
		if len(tool.Parameters) == 0 {
			t.Fatalf("tool %s has no parameters schema", tool.Name)
		}
	}
	if !slices.IsSorted(names) {
		t.Fatalf("tool definitions are not sorted: %v", names)
	}
	wantNames := []string{"escalate", "get_case", "propose_adjustment", "read_thing", "request_info"}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("tool definitions = %v, want %v", names, wantNames)
	}
	if !strings.Contains(first.System, "Untrusted") || !strings.Contains(first.System, "evidence") {
		t.Fatalf("composed prompt lost the untrusted/evidence rules")
	}
}

func TestRunHappyProposalPersistence(t *testing.T) {
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Response: toolResponse("call_1", "read_thing", `{}`)},
		{Response: toolResponse("call_2", "propose_adjustment", `{}`)},
	}})

	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeProposal || result.ProposalType != "adjustment" ||
		result.ProposalID != "33333333-3333-4333-8333-333333333333" {
		t.Fatalf("result = %+v, want an adjustment proposal", result)
	}
	if result.TokensIn != 20 || result.TokensOut != 10 {
		t.Fatalf("tokens = %d/%d, want 20/10", result.TokensIn, result.TokensOut)
	}
	if result.CostUSD.String() != "0.02" {
		t.Fatalf("cost = %s, want 0.02", result.CostUSD)
	}
	if result.Steps != 2 || result.ToolCalls != 3 {
		t.Fatalf("steps/tool calls = %d/%d, want 2/3", result.Steps, result.ToolCalls)
	}
	if result.CaseVersion != 4 || result.LeaseLost || result.RepairUsed {
		t.Fatalf("result case/lease/repair = %d/%v/%v", result.CaseVersion, result.LeaseLost, result.RepairUsed)
	}

	run := h.store.run(testRunID)
	if run.CaseID != testCaseID || run.Model != testModel || run.PromptVersion != "v1" {
		t.Fatalf("created run = %+v", run)
	}

	calls := h.store.llmCalls()
	if len(calls) != 2 {
		t.Fatalf("llm_calls rows = %d, want 2", len(calls))
	}
	for i, call := range calls {
		if call.Seq != i+1 {
			t.Fatalf("llm call %d seq = %d, want %d", i, call.Seq, i+1)
		}
		if call.Model != testActual {
			t.Fatalf("llm call %d model = %q, want actual model %q", i, call.Model, testActual)
		}
		if call.RequestHash == "" || len(call.Response) == 0 || len(call.Usage) == 0 {
			t.Fatalf("llm call %d is incomplete: %+v", i, call)
		}
		if call.DurationMS < 0 {
			t.Fatalf("llm call %d negative duration", i)
		}
	}
	canonical, err := llm.CanonicalRequestHash(llm.ProviderOpenCodeGo, llm.DefaultBaseURL, h.scripted.Requests()[0])
	if err != nil {
		t.Fatalf("CanonicalRequestHash: %v", err)
	}
	if calls[0].RequestHash != canonical {
		t.Fatalf("persisted request hash != canonical hash")
	}

	recorded := h.recorder.recorded()
	if len(recorded) != 3 {
		t.Fatalf("tool_calls rows = %d, want 3", len(recorded))
	}
	wantTools := []string{"get_case", "read_thing", "propose_adjustment"}
	for i, call := range recorded {
		if call.Seq != i+1 || call.ToolName != wantTools[i] {
			t.Fatalf("tool call %d = %s/%d, want %s/%d", i, call.ToolName, call.Seq, wantTools[i], i+1)
		}
		if call.ResultHash == "" || call.ErrorCode != "" {
			t.Fatalf("tool call %d envelope incomplete: %+v", i, call)
		}
	}

	transitions := h.cases.transitionRequests()
	if len(transitions) != 1 {
		t.Fatalf("case transitions = %d, want 1", len(transitions))
	}
	if transitions[0].Status != cases.StatusProposalPending ||
		transitions[0].ExpectedVersion != 3 || transitions[0].EscalationReason != "" {
		t.Fatalf("transition request = %+v", transitions[0])
	}

	finishes := h.store.finalizes()
	if len(finishes) != 1 {
		t.Fatalf("finalize rows = %d, want 1", len(finishes))
	}
	finish := finishes[0]
	if finish.Outcome != agent.OutcomeProposal || finish.Model != testActual ||
		finish.TokensIn != 20 || finish.TokensOut != 10 || finish.CostUSD.String() != "0.02" {
		t.Fatalf("finish row = %+v", finish)
	}
	if finish.EndedAt.Before(h.store.run(testRunID).StartedAt) {
		t.Fatalf("ended_at before started_at")
	}
	if len(finish.Flags) != 0 {
		t.Fatalf("unexpected flags on a clean run: %v", finish.Flags)
	}
}

func TestRunClampsNegativeUsageAndFinalizes(t *testing.T) {
	first := toolResponse("call_1", "read_thing", `{}`)
	first.Usage = llm.Usage{
		PromptTokens:     -10,
		CompletionTokens: -5,
		TotalTokens:      -15,
		CostUSD:          decimal.RequireFromString("-0.25"),
	}
	second := toolResponse("call_2", "escalate", `{}`)
	second.Usage = llm.Usage{
		PromptTokens:     -1,
		CompletionTokens: -2,
		CostUSD:          decimal.RequireFromString("-0.01"),
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := newHarness(t, runnerOptions{
		logger: logger,
		steps:  []llm.ScriptedStep{{Response: first}, {Response: second}},
	})

	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v, want a finalized run", err)
	}
	if result.Outcome != agent.OutcomeEscalated {
		t.Fatalf("outcome = %s, want escalated", result.Outcome)
	}
	if result.TokensIn != 0 || result.TokensOut != 0 || !result.CostUSD.IsZero() {
		t.Fatalf("result usage = %d/%d/%s, want zero", result.TokensIn, result.TokensOut, result.CostUSD)
	}

	finishes := h.store.finalizes()
	if len(finishes) != 1 {
		t.Fatalf("finalize rows = %d, want 1", len(finishes))
	}
	finish := finishes[0]
	if finish.TokensIn < 0 || finish.TokensOut < 0 || finish.CostUSD.IsNegative() {
		t.Fatalf("negative usage reached FinishRun: %+v", finish)
	}
	if finish.TokensIn != 0 || finish.TokensOut != 0 || !finish.CostUSD.IsZero() {
		t.Fatalf("finish usage = %d/%d/%s, want zero", finish.TokensIn, finish.TokensOut, finish.CostUSD)
	}

	calls := h.store.llmCalls()
	if len(calls) != 2 {
		t.Fatalf("llm_calls rows = %d, want 2", len(calls))
	}
	for i, call := range calls {
		var usage llm.Usage
		if err := json.Unmarshal(call.Usage, &usage); err != nil {
			t.Fatalf("llm call %d usage: %v", i, err)
		}
		if usage.PromptTokens < 0 || usage.CompletionTokens < 0 ||
			usage.TotalTokens < 0 || usage.CostUSD.IsNegative() {
			t.Fatalf("llm call %d persisted negative usage: %+v", i, usage)
		}
	}

	logged := logs.String()
	if !strings.Contains(logged, "agent provider usage clamped to zero") {
		t.Fatalf("clamp log missing: %s", logged)
	}
	if !strings.Contains(logged, "prompt_tokens") {
		t.Fatalf("clamp log lacks clamped field names: %s", logged)
	}
	if strings.Contains(logged, "-0.25") || strings.Contains(logged, "prompt_tokens=-10") {
		t.Fatalf("clamp log leaked usage values: %s", logged)
	}
}

func TestRunSynthesizesMissingToolCallIDs(t *testing.T) {
	multi := llm.Response{
		Provider:       llm.ProviderOpenCodeGo,
		ActualModel:    testActual,
		RequestedModel: testModel,
		StopReason:     llm.StopToolCalls,
		Usage:          usageWith(10, 5, "0.01"),
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "read_thing", Arguments: json.RawMessage(`{}`)},
			{Name: "read_thing", Arguments: json.RawMessage(`{}`)},
		}},
	}
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Response: multi},
		{Response: toolResponse("", "get_case", `{}`)},
		{Response: toolResponse("   ", "escalate", `{}`)},
	}})

	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeEscalated {
		t.Fatalf("outcome = %s, want escalated", result.Outcome)
	}

	requests := h.scripted.Requests()
	if len(requests) != 3 {
		t.Fatalf("completions = %d, want 3", len(requests))
	}
	first := requests[1]
	if len(first.Messages) != 6 {
		t.Fatalf("second request transcript = %d messages, want 6", len(first.Messages))
	}
	assistant := first.Messages[3]
	if assistant.Role != llm.RoleAssistant || len(assistant.ToolCalls) != 2 {
		t.Fatalf("multi-call assistant message = %+v", assistant)
	}
	ids := []string{assistant.ToolCalls[0].ID, assistant.ToolCalls[1].ID}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("synthesized ids = %v, want two distinct non-empty ids", ids)
	}
	if first.Messages[4].Role != llm.RoleTool || first.Messages[4].ToolCallID != ids[0] {
		t.Fatalf("first tool message = %+v, want id %q", first.Messages[4], ids[0])
	}
	if first.Messages[5].Role != llm.RoleTool || first.Messages[5].ToolCallID != ids[1] {
		t.Fatalf("second tool message = %+v, want id %q", first.Messages[5], ids[1])
	}

	second := requests[2]
	if len(second.Messages) != 8 {
		t.Fatalf("third request transcript = %d messages, want 8", len(second.Messages))
	}
	if second.Messages[3].ToolCalls[0].ID != ids[0] || second.Messages[3].ToolCalls[1].ID != ids[1] {
		t.Fatalf("synthesized ids changed between requests: %+v", second.Messages[3].ToolCalls)
	}
	single := second.Messages[6]
	if single.Role != llm.RoleAssistant || len(single.ToolCalls) != 1 || single.ToolCalls[0].ID == "" {
		t.Fatalf("single-call assistant message = %+v", single)
	}
	if second.Messages[7].Role != llm.RoleTool || second.Messages[7].ToolCallID != single.ToolCalls[0].ID {
		t.Fatalf("single-call tool message = %+v, want id %q", second.Messages[7], single.ToolCalls[0].ID)
	}

	for reqIndex, request := range requests {
		for msgIndex, message := range request.Messages {
			if len(message.ToolCalls) == 0 {
				continue
			}
			for _, call := range message.ToolCalls {
				if strings.TrimSpace(call.ID) == "" {
					t.Fatalf("request %d message %d has an empty tool-call id", reqIndex, msgIndex)
				}
				found := false
				for _, next := range request.Messages[msgIndex+1:] {
					if next.Role == llm.RoleAssistant {
						break
					}
					if next.Role == llm.RoleTool && next.ToolCallID == call.ID {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("request %d: tool call %q has no matching tool message", reqIndex, call.ID)
				}
			}
		}
	}

	for i, call := range h.store.llmCalls() {
		var persisted llm.Response
		if err := json.Unmarshal(call.Response, &persisted); err != nil {
			t.Fatalf("llm call %d response: %v", i, err)
		}
		for _, toolCall := range persisted.Message.ToolCalls {
			if strings.TrimSpace(toolCall.ID) == "" {
				t.Fatalf("llm call %d persisted an empty tool-call id", i)
			}
		}
	}
}

func TestRunRequestInfoOutcome(t *testing.T) {
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Response: toolResponse("call_1", "request_info", `{}`)},
	}})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeProposal || result.ProposalType != "request_info" {
		t.Fatalf("result = %+v, want request_info proposal", result)
	}
	transitions := h.cases.transitionRequests()
	if len(transitions) != 1 || transitions[0].Status != cases.StatusWaitingInfo {
		t.Fatalf("transition = %+v, want waiting_info", transitions)
	}
}

func TestRunEscalateOutcome(t *testing.T) {
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Response: toolResponse("call_1", "escalate", `{}`)},
	}})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeEscalated {
		t.Fatalf("outcome = %s, want escalated", result.Outcome)
	}
	if result.Escalation == nil || result.Escalation.Reason != "ambiguous_evidence" {
		t.Fatalf("escalation = %+v, want reason ambiguous_evidence", result.Escalation)
	}
	transitions := h.cases.transitionRequests()
	if len(transitions) != 1 || transitions[0].Status != cases.StatusEscalated ||
		transitions[0].EscalationReason != "ambiguous_evidence" {
		t.Fatalf("transition = %+v", transitions)
	}
}

func TestRunBudgetExhaustion(t *testing.T) {
	advancingClock := fixedClock()
	type budgetCase struct {
		name            string
		config          config.AgentConfig
		clock           *clock.Fake
		beforeStep      func(context.Context) error
		steps           []llm.ScriptedStep
		wantCompletions int
		wantToolCalls   int
	}
	steps := func() []llm.ScriptedStep {
		return []llm.ScriptedStep{{Response: toolResponse("call_1", "read_thing", `{}`)}}
	}
	cases := []budgetCase{
		{
			name: "steps",
			config: func() config.AgentConfig {
				cfg := newTestConfig()
				cfg.MaxSteps = 1
				return cfg
			}(),
			steps:           steps(),
			wantCompletions: 1,
			wantToolCalls:   2,
		},
		{
			name: "tool calls",
			config: func() config.AgentConfig {
				cfg := newTestConfig()
				cfg.MaxToolCalls = 1
				return cfg
			}(),
			wantCompletions: 0,
			wantToolCalls:   1,
		},
		{
			name: "tokens",
			config: func() config.AgentConfig {
				cfg := newTestConfig()
				cfg.MaxTotalTokens = 10
				return cfg
			}(),
			steps:           steps(),
			wantCompletions: 1,
			wantToolCalls:   1,
		},
		{
			name: "cost",
			config: func() config.AgentConfig {
				cfg := newTestConfig()
				cfg.MaxCostUSD = "0.01"
				return cfg
			}(),
			steps:           steps(),
			wantCompletions: 1,
			wantToolCalls:   1,
		},
		{
			name: "zero cost cap",
			config: func() config.AgentConfig {
				cfg := newTestConfig()
				cfg.MaxCostUSD = "0"
				return cfg
			}(),
			steps:           steps(),
			wantCompletions: 1,
			wantToolCalls:   1,
		},
		{
			name: "wall clock",
			config: func() config.AgentConfig {
				cfg := newTestConfig()
				cfg.MaxWallClock = 30 * time.Second
				return cfg
			}(),
			clock: advancingClock,
			beforeStep: func(context.Context) error {
				advancingClock.Advance(31 * time.Second)
				return nil
			},
			steps:           steps(),
			wantCompletions: 1,
			wantToolCalls:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, runnerOptions{
				config:        tc.config,
				clock:         tc.clock,
				beforeStep:    tc.beforeStep,
				steps:         tc.steps,
				switchDefault: true,
			})
			result, err := h.runner.Run(context.Background(), testCaseID)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Outcome != agent.OutcomeBudgetExhausted {
				t.Fatalf("outcome = %s, want budget_exhausted", result.Outcome)
			}
			if got := h.scripted.Calls(); got != tc.wantCompletions {
				t.Fatalf("completions = %d, want %d", got, tc.wantCompletions)
			}
			if got := len(h.recorder.recorded()); got != tc.wantToolCalls {
				t.Fatalf("recorded tool calls = %d, want %d", got, tc.wantToolCalls)
			}
			if got := len(h.cases.transitionRequests()); got != 0 {
				t.Fatalf("budget run transitioned the case %d times, want 0", got)
			}
			finishes := h.store.finalizes()
			if len(finishes) != 1 || finishes[0].Outcome != agent.OutcomeBudgetExhausted {
				t.Fatalf("finalize = %+v, want budget_exhausted", finishes)
			}
		})
	}
}

func TestRunLoopDetectionSuppressesThirdCall(t *testing.T) {
	third := toolResponse("call_3", "read_thing", `{}`)
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Response: toolResponse("call_1", "read_thing", `{}`)},
		{Response: toolResponse("call_2", "read_thing", `{}`)},
		{Response: third},
	}})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeEscalated || result.Escalation == nil ||
		result.Escalation.Reason != "repeated_identical_tool_call" {
		t.Fatalf("result = %+v, want repeated_identical_tool_call escalation", result)
	}
	if got := h.scripted.Calls(); got != 3 {
		t.Fatalf("completions = %d, want 3", got)
	}
	recorded := h.recorder.recorded()
	if len(recorded) != 3 {
		t.Fatalf("recorded tool calls = %d, want seed + two executed calls", len(recorded))
	}
	if got := len(h.store.llmCalls()); got != 3 {
		t.Fatalf("llm_calls rows = %d, want 3 (the suppressed call still recorded)", got)
	}
	transitions := h.cases.transitionRequests()
	if len(transitions) != 1 || transitions[0].Status != cases.StatusEscalated ||
		transitions[0].EscalationReason != "repeated_identical_tool_call" {
		t.Fatalf("transition = %+v", transitions)
	}
}

func TestRunRepairNoToolCall(t *testing.T) {
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Response: textResponse("")},
		{Response: textResponse("")},
	}})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeInvalidOutput || !result.RepairUsed {
		t.Fatalf("result = %+v, want invalid_output with repair used", result)
	}
	if got := h.scripted.Calls(); got != 2 {
		t.Fatalf("completions = %d, want 2", got)
	}
	requests := h.scripted.Requests()
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != llm.RoleUser || last.Content != repairNoTool {
		t.Fatalf("repair message = %+v, want exact %q", last, repairNoTool)
	}
	if got := len(h.cases.transitionRequests()); got != 0 {
		t.Fatalf("invalid_output transitioned the case %d times, want 0", got)
	}
}

func TestRunRepairMalformedArguments(t *testing.T) {
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Response: toolResponse("call_1", "read_thing", `[]`)},
		{Response: toolResponse("call_2", "read_thing", `[]`)},
	}})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeInvalidOutput || !result.RepairUsed {
		t.Fatalf("result = %+v, want invalid_output with repair used", result)
	}
	requests := h.scripted.Requests()
	if len(requests) != 2 {
		t.Fatalf("completions = %d, want 2", len(requests))
	}
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != llm.RoleUser || last.Content != repairBadArgs {
		t.Fatalf("repair message = %+v, want exact %q", last, repairBadArgs)
	}
	recorded := h.recorder.recorded()
	if len(recorded) != 3 {
		t.Fatalf("recorded tool calls = %d, want seed + two invalid attempts", len(recorded))
	}
	for _, call := range recorded[1:] {
		if call.ErrorCode != "invalid_arguments" {
			t.Fatalf("malformed attempt error = %q, want invalid_arguments", call.ErrorCode)
		}
	}
	if got := len(h.cases.transitionRequests()); got != 0 {
		t.Fatalf("invalid_output transitioned the case %d times, want 0", got)
	}
}

func TestRunProviderErrorFinalizesWithoutLLMCall(t *testing.T) {
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
		{Err: &llm.ProviderError{Provider: llm.ProviderOpenCodeGo, Kind: llm.KindUnavailable, Retryable: true}},
	}})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v, want nil error for a persisted provider_error", err)
	}
	if result.Outcome != agent.OutcomeProviderError {
		t.Fatalf("outcome = %s, want provider_error", result.Outcome)
	}
	if got := len(h.store.llmCalls()); got != 0 {
		t.Fatalf("llm_calls rows = %d, want 0 for a provider error", got)
	}
	if got := len(h.cases.transitionRequests()); got != 0 {
		t.Fatalf("provider_error transitioned the case %d times, want 0", got)
	}
	finishes := h.store.finalizes()
	if len(finishes) != 1 || finishes[0].Outcome != agent.OutcomeProviderError {
		t.Fatalf("finalize = %+v", finishes)
	}
}

func TestRunCassetteMissFinalizesAndReturnsError(t *testing.T) {
	h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{{Err: llm.ErrCassetteMiss}}})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if !errors.Is(err, llm.ErrCassetteMiss) {
		t.Fatalf("Run error = %v, want ErrCassetteMiss", err)
	}
	if result.Outcome != agent.OutcomeProviderError {
		t.Fatalf("outcome = %s, want provider_error", result.Outcome)
	}
	if got := len(h.store.llmCalls()); got != 0 {
		t.Fatalf("llm_calls rows = %d, want 0", got)
	}
}

func TestRunSeedInjectionFlagStopsBeforeCompletion(t *testing.T) {
	h := newHarness(t, runnerOptions{
		hooks: toolHooks{getCaseFlags: []string{"injection_suspected"}},
	})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeEscalated || result.Escalation == nil ||
		result.Escalation.Reason != "injection_flagged" {
		t.Fatalf("result = %+v, want injection_flagged escalation", result)
	}
	if got := h.scripted.Calls(); got != 0 {
		t.Fatalf("completions = %d, want 0 after a flagged seed", got)
	}
	assertFlagsSaved(t, h, "injection_suspected")
	transitions := h.cases.transitionRequests()
	if len(transitions) != 1 || transitions[0].Status != cases.StatusEscalated ||
		transitions[0].EscalationReason != "injection_flagged" {
		t.Fatalf("transition = %+v", transitions)
	}
}

func TestRunMidRunInjectionFlagStopsBeforeProposal(t *testing.T) {
	h := newHarness(t, runnerOptions{
		hooks: toolHooks{readFlags: []string{"injection_suspected"}},
		steps: []llm.ScriptedStep{
			{Response: toolResponse("call_1", "read_thing", `{}`)},
		},
	})
	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeEscalated || result.Escalation == nil ||
		result.Escalation.Reason != "injection_flagged" {
		t.Fatalf("result = %+v, want injection_flagged escalation", result)
	}
	if got := h.scripted.Calls(); got != 1 {
		t.Fatalf("completions = %d, want 1 (stop at the flagged call)", got)
	}
	recorded := h.recorder.recorded()
	if len(recorded) != 2 || recorded[1].ToolName != "read_thing" {
		t.Fatalf("recorded calls = %+v, want seed + read_thing", recorded)
	}
	assertFlagsSaved(t, h, "injection_suspected")
}

func assertFlagsSaved(t *testing.T, h *runnerHarness, want string) {
	t.Helper()
	if h.flags.calls == 0 {
		t.Fatalf("SaveRunFlags was not called")
	}
	args := h.flags.lastArgs()
	if len(args) != 2 || args[0] != testRunID || args[1] != `["`+want+`"]` {
		t.Fatalf("SaveRunFlags args = %#v, want run id and [%q]", args, want)
	}
	finishes := h.store.finalizes()
	if len(finishes) != 1 || len(finishes[0].Flags) != 1 || finishes[0].Flags[0] != want {
		t.Fatalf("finalize flags = %+v, want [%s]", finishes, want)
	}
}

func TestRunAgentDisabled(t *testing.T) {
	t.Run("before first step", func(t *testing.T) {
		h := newHarness(t, runnerOptions{switchValues: []bool{false}})
		result, err := h.runner.Run(context.Background(), testCaseID)
		if !errors.Is(err, agent.ErrAgentDisabled) {
			t.Fatalf("Run error = %v, want ErrAgentDisabled", err)
		}
		if result.Outcome != agent.OutcomeEscalated {
			t.Fatalf("outcome = %s, want escalated", result.Outcome)
		}
		if got := len(h.cases.transitionRequests()); got != 0 {
			t.Fatalf("disabled run transitioned the case %d times, want 0", got)
		}
		if got := len(h.recorder.recorded()); got != 0 {
			t.Fatalf("disabled run executed %d tools, want 0", got)
		}
		if got := len(h.store.finalizes()); got != 1 {
			t.Fatalf("disabled run finalize rows = %d, want 1", got)
		}
	})
	t.Run("mid run", func(t *testing.T) {
		h := newHarness(t, runnerOptions{
			switchValues:  []bool{true, false},
			switchDefault: true,
			steps:         []llm.ScriptedStep{{Response: toolResponse("call_1", "read_thing", `{}`)}},
		})
		result, err := h.runner.Run(context.Background(), testCaseID)
		if !errors.Is(err, agent.ErrAgentDisabled) {
			t.Fatalf("Run error = %v, want ErrAgentDisabled", err)
		}
		if result.Outcome != agent.OutcomeEscalated {
			t.Fatalf("outcome = %s, want escalated", result.Outcome)
		}
		if got := len(h.recorder.recorded()); got != 1 {
			t.Fatalf("tools executed = %d, want only the seed", got)
		}
		if got := h.scripted.Calls(); got != 0 {
			t.Fatalf("completions = %d, want 0 after the switch turned off", got)
		}
		if got := len(h.cases.transitionRequests()); got != 0 {
			t.Fatalf("disabled run transitioned the case %d times, want 0", got)
		}
	})
}

// covers: FIN-24
func TestRunStaleTransition(t *testing.T) {
	t.Run("retry succeeds while still the owner", func(t *testing.T) {
		casesSvc := newFakeCases()
		casesSvc.transition = func(call int, req cases.TransitionRequest, current cases.Case) (cases.Case, error) {
			if call == 1 {
				current.Version = 4
				return current, cases.ErrStale
			}
			current.Status = req.Status
			current.Version = req.ExpectedVersion + 1
			return current, nil
		}
		h := newHarness(t, runnerOptions{
			cases: casesSvc,
			steps: []llm.ScriptedStep{{Response: toolResponse("call_1", "escalate", `{}`)}},
		})
		result, err := h.runner.Run(context.Background(), testCaseID)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if result.Outcome != agent.OutcomeEscalated || result.LeaseLost {
			t.Fatalf("result = %+v, want escalated without lease loss", result)
		}
		if result.CaseVersion != 5 {
			t.Fatalf("case version = %d, want 5 after the retry", result.CaseVersion)
		}
		transitions := h.cases.transitionRequests()
		if len(transitions) != 2 || transitions[0].ExpectedVersion != 3 || transitions[1].ExpectedVersion != 4 {
			t.Fatalf("transition versions = %+v, want 3 then 4", transitions)
		}
		if got := h.cases.snapshot().Status; got != cases.StatusEscalated {
			t.Fatalf("final case status = %s, want escalated", got)
		}
	})

	t.Run("changed status is never overwritten", func(t *testing.T) {
		casesSvc := newFakeCases()
		casesSvc.transition = func(_ int, _ cases.TransitionRequest, current cases.Case) (cases.Case, error) {
			current.Status = cases.StatusEscalated
			current.Version = 4
			return current, cases.ErrStale
		}
		h := newHarness(t, runnerOptions{
			cases: casesSvc,
			steps: []llm.ScriptedStep{{Response: toolResponse("call_1", "escalate", `{}`)}},
		})
		result, err := h.runner.Run(context.Background(), testCaseID)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !result.LeaseLost {
			t.Fatalf("result = %+v, want LeaseLost", result)
		}
		if result.CaseVersion != 3 {
			t.Fatalf("case version = %d, want the initial 3", result.CaseVersion)
		}
		if got := len(h.cases.transitionRequests()); got != 1 {
			t.Fatalf("transitions = %d, want 1 (no blind retry)", got)
		}
		if got := len(h.store.finalizes()); got != 1 || h.store.finalizes()[0].Outcome != agent.OutcomeEscalated {
			t.Fatalf("run was not finalized after the lost lease")
		}
	})

	t.Run("second stale is fail-safe", func(t *testing.T) {
		casesSvc := newFakeCases()
		casesSvc.transition = func(call int, _ cases.TransitionRequest, current cases.Case) (cases.Case, error) {
			current.Version = 3 + call
			return current, cases.ErrStale
		}
		h := newHarness(t, runnerOptions{
			cases: casesSvc,
			steps: []llm.ScriptedStep{{Response: toolResponse("call_1", "escalate", `{}`)}},
		})
		result, err := h.runner.Run(context.Background(), testCaseID)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !result.LeaseLost || result.CaseVersion != 3 {
			t.Fatalf("result = %+v, want lease lost at the initial version", result)
		}
		if got := len(h.cases.transitionRequests()); got != 2 {
			t.Fatalf("transitions = %d, want exactly 2", got)
		}
	})
}

func TestRunRejectsCaseThatIsNotLeased(t *testing.T) {
	casesSvc := newFakeCases()
	casesSvc.current.Status = cases.StatusOpen
	h := newHarness(t, runnerOptions{cases: casesSvc})
	_, err := h.runner.Run(context.Background(), testCaseID)
	if !errors.Is(err, agent.ErrNotLeased) {
		t.Fatalf("Run error = %v, want ErrNotLeased", err)
	}
	if got := len(h.store.finalizes()); got != 0 {
		t.Fatalf("run row was created/finalized for a non-leased case")
	}
}

func TestRunDeterministicRequestAcrossRuns(t *testing.T) {
	run := func() []llm.Request {
		h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
			{Response: toolResponse("call_1", "escalate", `{}`)},
		}})
		if _, err := h.runner.Run(context.Background(), testCaseID); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return h.scripted.Requests()
	}
	first := run()
	second := run()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("deterministic run id produced different requests")
	}
	if first[0].SessionID != "ses_"+strings.ReplaceAll(testRunID, "-", "") {
		t.Fatalf("session id is not derived from the run id: %q", first[0].SessionID)
	}
	if !json.Valid([]byte(first[0].Messages[2].Content)) {
		t.Fatalf("wrapped seed result is not JSON")
	}
	if !strings.Contains(first[0].Messages[2].Content, "UNTRUSTED:field=summary:token=") {
		t.Fatalf("deterministic wrapper token missing from the transcript")
	}
}
