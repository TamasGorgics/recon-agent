package replay_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/prompts"
)

// integrityStep is one model-requested tool call and its recorded outcome. A
// nil args value models a provider null/empty argument payload: the production
// loop treats it as a malformed call, records the normalized body, and asks the
// model to repair.
type integrityStep struct {
	name    string
	args    json.RawMessage
	result  json.RawMessage
	errCode string
}

var integrityProposalKinds = map[string]string{
	"propose_link":       "link",
	"propose_adjustment": "adjustment",
	"propose_write_off":  "write_off",
	"mark_duplicate":     "duplicate",
	"request_info":       "request_info",
}

// syntheticScriptRecording builds a complete, self-consistent recording for a
// scripted sequence of tool calls. It mirrors the production request transcript
// (seed, response message, wrapped tool result, and one malformed-args repair)
// so each recomputed request hash is what the production loop would have used.
func syntheticScriptRecording(t *testing.T, steps []integrityStep) replay.Recording {
	t.Helper()
	if len(steps) == 0 {
		t.Fatal("syntheticScriptRecording needs at least one step")
	}
	cfg := syntheticConfig()
	prompt, err := prompts.Load(testPrompt)
	if err != nil {
		t.Fatalf("load prompt: %v", err)
	}
	toolDefs, err := agent.ReplayToolDefinitions()
	if err != nil {
		t.Fatalf("tool definitions: %v", err)
	}
	wrapper, err := untrusted.NewRunWrapper(testRunID)
	if err != nil {
		t.Fatalf("run wrapper: %v", err)
	}

	seedArgs := mustJSON(t, map[string]any{"case_id": testCaseID})
	seedEnvelope := mustJSON(t, map[string]any{
		"data":             map[string]any{"id": testCaseID, "status": "in_agent"},
		"refs":             []string{"case:" + testCaseID},
		"untrusted_fields": []string{},
		"truncated":        false,
	})
	seedHash, err := tools.CanonicalJSONHash(seedEnvelope)
	if err != nil {
		t.Fatalf("seed canonical hash: %v", err)
	}
	seedVisible, err := agent.ModelVisibleResult(seedEnvelope, wrapper)
	if err != nil {
		t.Fatalf("wrap seed: %v", err)
	}
	messages := agent.InitialTranscript(testCaseID, seedVisible)

	temperature := cfg.Agent.TemperatureValue()
	toolCalls := []replay.ToolCall{{
		Seq:                 1,
		ToolName:            "get_case",
		Args:                seedArgs,
		Result:              seedEnvelope,
		ResultCanonicalHash: &seedHash,
		StartedAt:           testSeedTime,
	}}
	llmCalls := make([]replay.LLMCall, 0, len(steps))
	repairUsed := false
	for i, step := range steps {
		request := llm.Request{
			Model:         cfg.Agent.Model,
			System:        prompt.Composed,
			Messages:      append([]llm.Message(nil), messages...),
			Tools:         toolDefs,
			ToolChoice:    &llm.ToolChoice{Mode: llm.ToolChoiceAuto},
			Temperature:   &temperature, //floatlint:allow provider temperature, not money
			MaxTokens:     cfg.Agent.MaxTokensPerResponse,
			SessionID:     "ses_" + strings.ReplaceAll(testRunID, "-", ""),
			PromptVersion: testPrompt,
		}
		requestHash, err := llm.CanonicalRequestHash(cfg.Provider, cfg.BaseURL, request)
		if err != nil {
			t.Fatalf("step %d request hash: %v", i+1, err)
		}
		callID := fmt.Sprintf("call_%d", i+1)
		response := llm.Response{
			Provider:       cfg.Provider,
			RequestedModel: llm.APIID(cfg.Agent.Model),
			ActualModel:    cfg.Agent.Model,
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: callID, Name: step.name, Arguments: step.args}},
			},
			StopReason: llm.StopToolCalls,
		}
		responseBody, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("step %d response: %v", i+1, err)
		}
		usageBody, err := json.Marshal(response.Usage)
		if err != nil {
			t.Fatalf("step %d usage: %v", i+1, err)
		}
		started := testLLMTime.Add(time.Duration(i) * time.Second)
		llmCalls = append(llmCalls, replay.LLMCall{
			Seq:         i + 1,
			Model:       cfg.Agent.Model,
			RequestHash: requestHash,
			Response:    responseBody,
			Usage:       usageBody,
			StartedAt:   started,
			DurationMS:  5,
		})
		messages = append(messages, response.Message)

		normalizedArgs, _ := tools.NormalizeToolArgs(step.args)
		resultHash, err := tools.CanonicalJSONHash(step.result)
		if err != nil {
			t.Fatalf("step %d result hash: %v", i+1, err)
		}
		var errorCode *string
		if step.errCode != "" {
			code := step.errCode
			errorCode = &code
		}
		toolCalls = append(toolCalls, replay.ToolCall{
			Seq:                 i + 2,
			ToolName:            step.name,
			Args:                normalizedArgs,
			Result:              step.result,
			ResultCanonicalHash: &resultHash,
			Error:               errorCode,
			StartedAt:           testProposalTime.Add(time.Duration(i) * time.Second),
		})
		wrappedResult, err := agent.ModelVisibleResult(step.result, wrapper)
		if err != nil {
			t.Fatalf("step %d wrap result: %v", i+1, err)
		}
		messages = append(messages, agent.ToolMessage(callID, wrappedResult))
		if !agent.IsJSONObject(step.args) && !repairUsed {
			repairUsed = true
			messages = append(messages, agent.RepairMessage(agent.ReplayRepairMalformedArgs))
		}
	}

	last := steps[len(steps)-1]
	kind, ok := integrityProposalKinds[last.name]
	if !ok {
		t.Fatalf("last step %q is not a proposal tool", last.name)
	}
	fields, err := proposal.Rebuild(kind, last.args, testCaseID)
	if err != nil {
		t.Fatalf("rebuild proposal: %v", err)
	}
	key, err := proposal.IdempotencyKey(testCaseID, testRunID, fields)
	if err != nil {
		t.Fatalf("idempotency key: %v", err)
	}
	payload, err := json.Marshal(fields.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	evidence, err := json.Marshal(fields.Evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}

	return replay.Recording{
		Run: replay.Run{
			ID:            testRunID,
			CaseID:        testCaseID,
			Model:         cfg.Agent.Model,
			PromptVersion: testPrompt,
			Outcome:       "proposal",
			StartedAt:     testSeedTime,
		},
		LLMCalls:  llmCalls,
		ToolCalls: toolCalls,
		Proposal: &replay.Proposal{
			ID:             "33333333-3333-4333-8333-333333333333",
			Type:           fields.Type,
			Payload:        payload,
			Rationale:      fields.Rationale,
			Evidence:       evidence,
			Confidence:     fields.Confidence,
			PatternClaimed: fields.PatternClaimed,
			StateHash:      testStateHash,
			IdempotencyKey: key,
		},
	}
}

func integritySuccessEnvelope(t *testing.T, status string) json.RawMessage {
	t.Helper()
	return mustJSON(t, map[string]any{
		"data":             map[string]any{"status": status},
		"refs":             []string{},
		"untrusted_fields": []string{},
		"truncated":        false,
	})
}

func integrityErrorEnvelope(t *testing.T, code string) json.RawMessage {
	t.Helper()
	return mustJSON(t, map[string]any{
		"error":            map[string]any{"code": code, "message": "invalid tool arguments"},
		"refs":             []string{},
		"untrusted_fields": []string{},
		"truncated":        false,
	})
}

func integrityProposalArgs(t *testing.T) json.RawMessage {
	t.Helper()
	return mustJSON(t, map[string]any{
		"case_id":    testCaseID,
		"rationale":  "Need the settlement reference before deciding.",
		"evidence":   []string{"case:" + testCaseID},
		"confidence": 0.5,
		"question":   "Which settlement reference matches this case?",
	})
}

func integrityInvestigationStep(t *testing.T) integrityStep {
	t.Helper()
	return integrityStep{
		name: "lookup_external_record",
		args: mustJSON(t, map[string]any{
			"case_id":      testCaseID,
			"external_ref": "ext-0001",
		}),
		result: integritySuccessEnvelope(t, "found"),
	}
}

func integrityProposalStep(t *testing.T) integrityStep {
	t.Helper()
	return integrityStep{
		name:   "request_info",
		args:   integrityProposalArgs(t),
		result: integritySuccessEnvelope(t, "recorded"),
	}
}

// TestExactReplayToolArgsMismatch proves the stored tool_calls.args are compared
// with the model-requested arguments for the seed, an investigation tool, and a
// proposal tool. Each mismatch is a tool_result_mismatch hard stop with
// field=args, makes identical false, and produces no downstream divergence.
func TestExactReplayToolArgsMismatch(t *testing.T) {
	t.Parallel()
	seedOnly := func(t *testing.T) replay.Recording { return syntheticRecording(t) }
	investigation := func(t *testing.T) replay.Recording {
		return syntheticScriptRecording(t, []integrityStep{
			integrityInvestigationStep(t),
			integrityProposalStep(t),
		})
	}
	proposalRun := func(t *testing.T) replay.Recording {
		return syntheticScriptRecording(t, []integrityStep{
			integrityInvestigationStep(t),
			integrityProposalStep(t),
		})
	}

	tests := []struct {
		name     string
		build    func(*testing.T) replay.Recording
		toolIdx  int
		wantArg  json.RawMessage
		wantStep int
	}{
		{
			name:     "seed get_case args",
			build:    seedOnly,
			toolIdx:  0,
			wantArg:  json.RawMessage(`{"case_id":"99999999-9999-4999-8999-999999999999"}`),
			wantStep: 0,
		},
		{
			name:     "investigation args",
			build:    investigation,
			toolIdx:  1,
			wantArg:  json.RawMessage(`{"case_id":"` + testCaseID + `","external_ref":"ext-tampered"}`),
			wantStep: 1,
		},
		{
			name:     "proposal args",
			build:    proposalRun,
			toolIdx:  2,
			wantArg:  json.RawMessage(`{"case_id":"` + testCaseID + `","rationale":"tampered","evidence":["case:` + testCaseID + `"],"confidence":0.5,"question":"tampered?"}`),
			wantStep: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rec := test.build(t)
			rec.ToolCalls[test.toolIdx].Args = test.wantArg
			report, err := replay.ExactReplay(rec, syntheticConfig())
			if err != nil {
				t.Fatalf("ExactReplay(): %v", err)
			}
			if report.Identical {
				t.Fatal("identical = true for a tool-args mismatch")
			}
			if len(report.Divergences) != 1 {
				t.Fatalf("divergences = %+v, want one hard stop", report.Divergences)
			}
			divergence := report.Divergences[0]
			if divergence.Code != replay.CodeToolResultMismatch || divergence.Step != test.wantStep {
				t.Fatalf("divergence = %+v, want tool_result_mismatch at step %d", divergence, test.wantStep)
			}
			if divergence.Field == nil || *divergence.Field != "args" {
				t.Fatalf("divergence field = %v, want args", divergence.Field)
			}
		})
	}
}

// TestExactReplayResponseArgsDisagreeWithRecording rewrites the arguments in
// the recorded LLM response while leaving tool_calls.args intact. The last step
// has no later request hash to expose the edit, so the direct args comparison
// must catch it: replay rebuilds the proposal from the recorded args only after
// proving they match what the model asked for.
func TestExactReplayResponseArgsDisagreeWithRecording(t *testing.T) {
	t.Parallel()
	rec := syntheticScriptRecording(t, []integrityStep{
		integrityInvestigationStep(t),
		integrityProposalStep(t),
	})
	var response llm.Response
	if err := json.Unmarshal(rec.LLMCalls[1].Response, &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	response.Message.ToolCalls[0].Arguments = json.RawMessage(
		`{"case_id":"` + testCaseID + `","rationale":"changed in the response","evidence":["case:` + testCaseID + `"],"confidence":0.5,"question":"Which settlement reference matches this case?"}`)
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}
	rec.LLMCalls[1].Response = body

	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if report.Identical {
		t.Fatal("identical = true for a response/recorded-args disagreement")
	}
	if len(report.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want one hard stop", report.Divergences)
	}
	divergence := report.Divergences[0]
	if divergence.Code != replay.CodeToolResultMismatch || divergence.Step != 2 ||
		divergence.Field == nil || *divergence.Field != "args" {
		t.Fatalf("divergence = %+v, want tool_result_mismatch(field=args) at step 2", divergence)
	}
}

// TestExactReplayToolArgsCanonicalEquivalence proves legitimate recorded args
// compare equal after canonicalization: key order, whitespace, and a large
// integer written with UseNumber do not manufacture a divergence. The stored
// JSONB text and the response both come from PostgreSQL in production, so this
// mirrors a JSONB round-trip.
func TestExactReplayToolArgsCanonicalEquivalence(t *testing.T) {
	t.Parallel()
	rec := syntheticScriptRecording(t, []integrityStep{
		{
			name: "lookup_external_record",
			args: json.RawMessage(`{
				"case_id": "` + testCaseID + `",
				"external_ref": "ext-0001",
				"amount_minor": 9007199254740993,
				"confidence": 1.50
			}`),
			result: integritySuccessEnvelope(t, "found"),
		},
		integrityProposalStep(t),
	})
	// Re-encode the investigation args with a different key order and compact
	// whitespace, exactly as JSONB text may present them on load.
	rec.ToolCalls[1].Args = json.RawMessage(`{"confidence":1.50,"amount_minor":9007199254740993,"external_ref":"ext-0001","case_id":"` + testCaseID + `"}`)
	// The proposal args are also stored with different key order.
	rec.ToolCalls[2].Args = json.RawMessage(`{"question":"Which settlement reference matches this case?","confidence":0.5,"evidence":["case:` + testCaseID + `"],"rationale":"Need the settlement reference before deciding.","case_id":"` + testCaseID + `"}`)

	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false for canonical-equivalent args, divergences = %+v", report.Divergences)
	}
	if len(report.Divergences) != 0 {
		t.Fatalf("divergences = %+v, want none", report.Divergences)
	}
}

// TestExactReplayMalformedProposalRepair reconstructs the production shapes
// where a proposal attempt fails validation and the run later records a valid
// proposal. Exact replay must mirror the production flow, consume both recorded
// calls, and must not report a false step_count_mismatch.
//
//   - syntactic: null/empty arguments produce invalid_arguments, the loop
//     appends the malformed-arguments repair message, and the model retries;
//   - schema: a valid JSON object that fails schema validation produces
//     invalid_arguments without a repair message, and the model retries from
//     the error tool result.
func TestExactReplayMalformedProposalRepair(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		step integrityStep
	}{
		{
			name: "syntactic malformed args",
			step: integrityStep{
				name:    "request_info",
				args:    nil,
				result:  integrityErrorEnvelope(t, "invalid_arguments"),
				errCode: "invalid_arguments",
			},
		},
		{
			name: "schema-invalid object args",
			step: integrityStep{
				name:    "request_info",
				args:    json.RawMessage(`{"case_id":"` + testCaseID + `"}`),
				result:  integrityErrorEnvelope(t, "invalid_arguments"),
				errCode: "invalid_arguments",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rec := syntheticScriptRecording(t, []integrityStep{
				test.step,
				integrityProposalStep(t),
			})
			report, err := replay.ExactReplay(rec, syntheticConfig())
			if err != nil {
				t.Fatalf("ExactReplay(): %v", err)
			}
			if !report.Identical {
				t.Fatalf("repaired run is not identical: divergences = %+v", report.Divergences)
			}
			if len(report.Divergences) != 0 {
				t.Fatalf("divergences = %+v, want none", report.Divergences)
			}
			if report.StepsTotal != 2 {
				t.Fatalf("steps_total = %d, want 2", report.StepsTotal)
			}
			if len(report.Clock) != 5 {
				t.Fatalf("clock events = %+v, want seed + two llm + two tool events", report.Clock)
			}
		})
	}
}
