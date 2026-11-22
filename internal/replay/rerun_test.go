package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/prompts"
)

// recordedStepResponse decodes the recorded normalized response for a 1-based
// llm_calls.seq.
func recordedStepResponse(t *testing.T, rec replay.Recording, seq int) llm.Response {
	t.Helper()
	for _, call := range rec.LLMCalls {
		if call.Seq != seq {
			continue
		}
		var response llm.Response
		if err := json.Unmarshal(call.Response, &response); err != nil {
			t.Fatalf("decode recorded response seq %d: %v", seq, err)
		}
		return response
	}
	t.Fatalf("recording has no llm call at seq %d", seq)
	return llm.Response{}
}

// rerunToolResponse builds one assistant response that requests a single tool.
func rerunToolResponse(name string, args json.RawMessage, callID string) llm.Response {
	return llm.Response{
		ActualModel: config.DefaultLLMModel,
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:        callID,
				Name:      name,
				Arguments: args,
			}},
		},
		StopReason: llm.StopToolCalls,
	}
}

// changedProposalArgs returns the synthetic proposal arguments with a different
// rationale, so the rebuilt digest must differ.
func changedProposalArgs(t *testing.T, runCaseID, rationale string) json.RawMessage {
	t.Helper()
	return mustJSON(t, map[string]any{
		"case_id":    runCaseID,
		"rationale":  rationale,
		"evidence":   []string{"case:" + runCaseID},
		"confidence": 0.5,
		"question":   "Which settlement reference matches this case?",
	})
}

func toolMessageContent(t *testing.T, request llm.Request, callID string) string {
	t.Helper()
	for _, message := range request.Messages {
		if message.Role == llm.RoleTool && message.ToolCallID == callID {
			return message.Content
		}
	}
	t.Fatalf("request transcript has no tool message for call id %q", callID)
	return ""
}

// TestRerunIdenticalWithScriptedClient proves the cassette-shaped path: the
// recorded response yields the same proposal digest through the rerun loop and
// the client is called exactly once (there is no continuation past the
// proposal).
func TestRerunIdenticalWithScriptedClient(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	scripted := llm.NewScripted(llm.ScriptedStep{Response: recordedStepResponse(t, rec, 1)})

	report, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{
		Client: scripted,
		Config: syntheticConfig(),
	})
	if err != nil {
		t.Fatalf("Rerun(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false: divergences=%+v diagnostics=%+v", report.Divergences, report.Diagnostics)
	}
	if report.Mode != replay.ModeRerun {
		t.Fatalf("mode = %q, want %q", report.Mode, replay.ModeRerun)
	}
	if report.ProposalHash == "" || report.ProposalHash != report.StoredHash {
		t.Fatalf("proposal_hash = %q, stored_hash = %q", report.ProposalHash, report.StoredHash)
	}
	if len(report.Divergences) != 0 {
		t.Fatalf("divergences = %+v, want none", report.Divergences)
	}
	if scripted.Calls() != 1 {
		t.Fatalf("scripted calls = %d, want 1", scripted.Calls())
	}
	requests := scripted.Requests()
	if len(requests) != 1 || requests[0].PromptVersion != testPrompt {
		t.Fatalf("requests = %+v, want one with prompt %q", requests, testPrompt)
	}
}

// TestRerunDifferentPromptReportsDigestDivergence proves a --prompt override
// reaches the request and a different proposal is reported through the standard
// divergence channel, not silently accepted.
func TestRerunDifferentPromptReportsDigestDivergence(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	response := recordedStepResponse(t, rec, 1)
	response.Message.ToolCalls[0].Arguments = changedProposalArgs(t, testCaseID,
		"A different rationale under the rerun prompt.")
	scripted := llm.NewScripted(llm.ScriptedStep{Response: response})

	report, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{
		Client: scripted,
		Prompt: "v2",
		Config: syntheticConfig(),
	})
	if err != nil {
		t.Fatalf("Rerun(): %v", err)
	}
	if report.Identical {
		t.Fatal("identical = true for a different-prompt rerun")
	}
	if report.Prompt != "v2" {
		t.Fatalf("prompt = %q, want v2", report.Prompt)
	}
	if len(report.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want one digest mismatch", report.Divergences)
	}
	divergence := report.Divergences[0]
	if divergence.Code != replay.CodeProposalDigestMismatch || divergence.Step != 0 {
		t.Fatalf("divergence = %+v, want proposal_digest_mismatch at step 0", divergence)
	}
	if report.ProposalHash == report.StoredHash {
		t.Fatalf("proposal_hash and stored_hash are equal: %s", report.ProposalHash)
	}
	// A rerun never requires idempotency-key equality (spec A10).
	for _, divergence := range report.Divergences {
		if divergence.Code == replay.CodeProposalFieldDiff && divergence.Field != nil &&
			*divergence.Field == "idempotency_key" {
			t.Fatalf("rerun reported an idempotency-key diff: %+v", divergence)
		}
	}

	v2, err := prompts.Load("v2")
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	requests := scripted.Requests()
	if len(requests) != 1 || requests[0].PromptVersion != "v2" || requests[0].System != v2.Composed {
		t.Fatalf("request did not use the v2 prompt: %+v", requests)
	}
}

// TestRerunModelOverrideReachesRequest proves --model overrides the recorded
// model in the request while the digest remains unaffected by the model alone.
func TestRerunModelOverrideReachesRequest(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	const override = "another-model"
	scripted := llm.NewScripted(llm.ScriptedStep{
		Response: recordedStepResponse(t, rec, 1),
		Check: func(request llm.Request) error {
			if request.Model != override {
				return errors.New("request model was not overridden")
			}
			return nil
		},
	})

	report, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{
		Client: scripted,
		Model:  override,
		Config: syntheticConfig(),
	})
	if err != nil {
		t.Fatalf("Rerun(): %v", err)
	}
	if !report.Identical || report.Model != override {
		t.Fatalf("report = %+v, want identical with model %q", report, override)
	}
}

// TestRerunUnrecordedToolIsNeverExecuted proves an unmatched tool request is a
// tool_call_unrecorded hard stop. Rerun has no tool executor or live client at
// all, so the only observable continuation would be another completion; the
// scripted call count proves none was issued.
func TestRerunUnrecordedToolIsNeverExecuted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call llm.ToolCall
	}{
		{
			name: "unrecorded tool",
			call: llm.ToolCall{ID: "call_1", Name: "get_fee_schedule",
				Arguments: mustJSON(t, map[string]any{"case_id": testCaseID})},
		},
		{
			name: "seed call cannot be reused",
			call: llm.ToolCall{ID: "call_1", Name: "get_case",
				Arguments: mustJSON(t, map[string]any{"case_id": testCaseID})},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rec := syntheticRecording(t)
			scripted := llm.NewScripted(llm.ScriptedStep{Response: llm.Response{
				ActualModel: config.DefaultLLMModel,
				Message:     llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{test.call}},
				StopReason:  llm.StopToolCalls,
			}})

			report, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{
				Client: scripted,
				Config: syntheticConfig(),
			})
			if err != nil {
				t.Fatalf("Rerun(): %v", err)
			}
			if report.Identical {
				t.Fatal("identical = true for an unrecorded tool call")
			}
			if len(report.Divergences) != 1 {
				t.Fatalf("divergences = %+v, want one hard stop", report.Divergences)
			}
			divergence := report.Divergences[0]
			if divergence.Code != replay.CodeToolCallUnrecorded || divergence.Step != 1 {
				t.Fatalf("divergence = %+v, want tool_call_unrecorded at step 1", divergence)
			}
			if report.ProposalHash != "" {
				t.Fatalf("proposal_hash = %q, want none after a hard stop", report.ProposalHash)
			}
			if scripted.Calls() != 1 {
				t.Fatalf("scripted calls = %d, want 1 (the hard stop must not continue)", scripted.Calls())
			}
			if len(report.Clock) != 2 {
				t.Fatalf("clock events = %+v, want the seed plus the one attempted step (no tool event)", report.Clock)
			}
		})
	}
}

// TestRerunConsumesEachRecordedCallOnce proves the matcher consumes recorded
// calls in ascending seq order and never reuses one: two identical requests get
// seq 3 then seq 4, and a third identical request is unrecorded.
func TestRerunConsumesEachRecordedCallOnce(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	args := mustJSON(t, map[string]any{"case_id": testCaseID, "signature": strings.Repeat("a", 64)})
	firstResult := mustJSON(t, map[string]any{"data": map[string]any{"marker": "result-a"}})
	secondResult := mustJSON(t, map[string]any{"data": map[string]any{"marker": "result-b"}})
	rec.ToolCalls = append(rec.ToolCalls,
		recordedReadCall(t, 3, "get_history", args, firstResult),
		recordedReadCall(t, 4, "get_history", args, secondResult),
	)

	scripted := llm.NewScripted(
		llm.ScriptedStep{Response: rerunToolResponse("get_history", args, "call_1")},
		llm.ScriptedStep{Response: rerunToolResponse("get_history", args, "call_2")},
		llm.ScriptedStep{Response: rerunToolResponse("get_history", args, "call_3")},
	)
	report, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{
		Client: scripted,
		Config: syntheticConfig(),
	})
	if err != nil {
		t.Fatalf("Rerun(): %v", err)
	}
	if len(report.Divergences) != 1 || report.Divergences[0].Code != replay.CodeToolCallUnrecorded ||
		report.Divergences[0].Step != 3 {
		t.Fatalf("divergences = %+v, want tool_call_unrecorded at step 3", report.Divergences)
	}
	requests := scripted.Requests()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	// Step 1 has only the seed tool message; step 2 sees seq 3's result; step 3
	// sees seq 3 then seq 4 in ascending recorded order.
	if got := toolMessageContent(t, requests[1], "call_1"); !strings.Contains(got, "result-a") {
		t.Fatalf("first injected result = %q, want result-a", got)
	}
	if got := toolMessageContent(t, requests[2], "call_1"); !strings.Contains(got, "result-a") {
		t.Fatalf("second request first result = %q, want result-a", got)
	}
	if got := toolMessageContent(t, requests[2], "call_2"); !strings.Contains(got, "result-b") {
		t.Fatalf("second request second result = %q, want result-b", got)
	}
}

// TestRerunIgnoresStoredIdempotencyKey pins spec A10: rerun compares the digest
// only, so a stored key difference does not change identical.
func TestRerunIgnoresStoredIdempotencyKey(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.Proposal.IdempotencyKey = strings.Repeat("c", 64)
	scripted := llm.NewScripted(llm.ScriptedStep{Response: recordedStepResponse(t, rec, 1)})

	report, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{
		Client: scripted,
		Config: syntheticConfig(),
	})
	if err != nil {
		t.Fatalf("Rerun(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false for a rerun key difference, divergences = %+v", report.Divergences)
	}
	if len(report.Divergences) != 0 {
		t.Fatalf("divergences = %+v, want none", report.Divergences)
	}
}

func TestRerunRequiresClient(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	if _, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{Config: syntheticConfig()}); !errors.Is(err, replay.ErrNoClient) {
		t.Fatalf("error = %v, want ErrNoClient", err)
	}
}

func TestRerunRequiresStoredProposal(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.Proposal = nil
	rec.Run.Outcome = "escalated"
	_, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{
		Client: llm.NewScripted(),
		Config: syntheticConfig(),
	})
	if !errors.Is(err, replay.ErrUnsupportedRun) {
		t.Fatalf("error = %v, want ErrUnsupportedRun", err)
	}
	var unsupported *replay.UnsupportedRunError
	if !errors.As(err, &unsupported) || unsupported.Mode != replay.ModeRerun {
		t.Fatalf("error = %v, want UnsupportedRunError with mode rerun", err)
	}
}

// TestRerunCassetteDefaultAndMiss proves the cassette default path: a cassette
// recorded from the exact rerun request replays offline, while an empty
// directory is an operational cassette miss rather than a divergence.
func TestRerunCassetteDefaultAndMiss(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	cfg := syntheticConfig()
	response := recordedStepResponse(t, rec, 1)

	// Capture the exact request the rerun will issue.
	probe := llm.NewScripted(llm.ScriptedStep{Response: response})
	if _, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{Client: probe, Config: cfg}); err != nil {
		t.Fatalf("probe rerun: %v", err)
	}
	requests := probe.Requests()
	if len(requests) != 1 {
		t.Fatalf("probe requests = %d, want 1", len(requests))
	}

	dir := t.TempDir()
	record, err := llm.NewCassette(llm.NewScripted(llm.ScriptedStep{Response: response}), llm.CassetteOptions{
		Mode:     llm.CassetteRecord,
		Dir:      dir,
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
	})
	if err != nil {
		t.Fatalf("new recording cassette: %v", err)
	}
	if _, err := record.Complete(context.Background(), requests[0]); err != nil {
		t.Fatalf("record cassette: %v", err)
	}

	replayCassette, err := llm.NewCassette(nil, llm.CassetteOptions{
		Mode:     llm.CassetteReplay,
		Dir:      dir,
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
	})
	if err != nil {
		t.Fatalf("new replay cassette: %v", err)
	}
	report, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{Client: replayCassette, Config: cfg})
	if err != nil {
		t.Fatalf("cassette rerun: %v", err)
	}
	if !report.Identical {
		t.Fatalf("cassette rerun identical = false: %+v", report.Divergences)
	}

	missing, err := llm.NewCassette(nil, llm.CassetteOptions{
		Mode:     llm.CassetteReplay,
		Dir:      t.TempDir(),
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
	})
	if err != nil {
		t.Fatalf("new empty replay cassette: %v", err)
	}
	if _, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{Client: missing, Config: cfg}); !errors.Is(err, replay.ErrCassetteMiss) {
		t.Fatalf("error = %v, want ErrCassetteMiss", err)
	} else if !errors.Is(err, llm.ErrCassetteMiss) {
		t.Fatalf("error = %v, want it to wrap llm.ErrCassetteMiss", err)
	}
}

// covers: FIN-43
// TestRerunFakeClockStable proves the rerun clock uses recorded timestamps and
// is identical across two runs (no wall-clock read).
func TestRerunFakeClockStable(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	run := func() replay.Report {
		scripted := llm.NewScripted(llm.ScriptedStep{Response: recordedStepResponse(t, rec, 1)})
		report, err := replay.Rerun(context.Background(), rec, replay.RerunOptions{
			Client: scripted,
			Config: syntheticConfig(),
		})
		if err != nil {
			t.Fatalf("Rerun(): %v", err)
		}
		return report
	}
	first, second := run(), run()
	if !reflect.DeepEqual(first.Clock, second.Clock) {
		t.Fatalf("clock events are not stable: %+v vs %+v", first.Clock, second.Clock)
	}
	want := []replay.ClockEvent{
		{Kind: "tool", Step: 0, Tool: 1, At: testSeedTime, DurationMS: 0},
		{Kind: "llm", Step: 1, At: testLLMTime, DurationMS: 5},
	}
	if !reflect.DeepEqual(first.Clock, want) {
		t.Fatalf("clock events = %+v, want %+v", first.Clock, want)
	}
}

// recordedReadCall appends one self-consistent recorded read-tool call.
func recordedReadCall(t *testing.T, seq int, name string, args, result json.RawMessage) replay.ToolCall {
	t.Helper()
	hash, err := tools.CanonicalJSONHash(result)
	if err != nil {
		t.Fatalf("canonical result hash: %v", err)
	}
	return replay.ToolCall{
		Seq:                 seq,
		ToolName:            name,
		Args:                args,
		Result:              result,
		ResultCanonicalHash: &hash,
		StartedAt:           time.Date(2026, 3, 1, 10, 0, 6, 0, time.UTC),
	}
}
