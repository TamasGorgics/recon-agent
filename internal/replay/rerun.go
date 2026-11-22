package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/prompts"
)

// ModeRerun is the rerun mode reported by Report.Mode.
const ModeRerun = "rerun"

// Rerun-mode errors. They are operational failures (exit 2), never a
// divergence: a missing cassette is a missing recording, not a behavioral
// difference.
var (
	// ErrNoClient means Rerun was called without an LLM client. Tests inject a
	// scripted client; the CLI injects a cassette (default) or live client.
	ErrNoClient = errors.New("replay: rerun requires an LLM client")
	// ErrCassetteMiss wraps llm.ErrCassetteMiss so the CLI can map a cassette
	// miss without importing the llm error surface.
	ErrCassetteMiss = errors.New("replay: cassette miss")
)

// RerunOptions configures a rerun. Client is the injected LLM boundary and is
// the only way rerun can reach recorded or live completions; Rerun itself has
// no tool executor, database handle, or network client. Model and Prompt
// override the recorded values when non-empty.
type RerunOptions struct {
	Client llm.Client
	Model  string
	Prompt string
	Config Config
}

// Rerun reconstructs the recorded seed and investigation transcript, then
// drives a model with the recorded tool results injected instead of executed.
// A model-requested tool call is matched by (tool_name, canonical arguments)
// against unconsumed recorded calls in ascending seq order; an unmatched
// request is the tool_call_unrecorded hard stop and is never executed. The
// rerun's proposal is rebuilt through the production normalization path and
// compared by digest only: the idempotency key is deliberately not required,
// because a rerun produces a new proposal.
func Rerun(ctx context.Context, recording Recording, opts RerunOptions) (Report, error) {
	if opts.Client == nil {
		return Report{}, ErrNoClient
	}
	rec := normalizeRecording(recording)
	if rec.Proposal == nil {
		return Report{}, &UnsupportedRunError{
			RunID:       rec.Run.ID,
			Outcome:     rec.Run.Outcome,
			HasProposal: false,
			Mode:        ModeRerun,
		}
	}

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = recordedRequestModel(rec)
	}
	promptVersion := strings.TrimSpace(opts.Prompt)
	if promptVersion == "" {
		promptVersion = strings.TrimSpace(rec.Run.PromptVersion)
	}
	if promptVersion == "" {
		promptVersion = strings.TrimSpace(opts.Config.Agent.PromptVersion)
	}
	if model == "" {
		model = strings.TrimSpace(opts.Config.Agent.Model)
	}
	if model == "" || promptVersion == "" {
		return Report{}, fmt.Errorf("%w: the recording has no model or prompt and no override was supplied", ErrInvalidRecording)
	}

	report := Report{
		RunID:       rec.Run.ID,
		Mode:        ModeRerun,
		Outcome:     rec.Run.Outcome,
		Model:       model,
		Prompt:      promptVersion,
		Divergences: []Divergence{},
	}
	report.StateHash = rec.Proposal.StateHash
	if !isLowerHex64(report.StateHash) {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Code:   CodeStateHashInvalid,
			Detail: "stored state_hash is not 64 lowercase hex characters",
		})
	}

	stored, err := storedFields(rec.Proposal)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrRebuild, err)
	}
	storedHash, err := proposal.Digest(stored)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrRebuild, err)
	}
	report.StoredHash = storedHash

	prompt, err := prompts.Load(promptVersion)
	if err != nil {
		return Report{}, fmt.Errorf("replay: load prompt %q: %w", promptVersion, err)
	}
	toolDefs, err := agent.ReplayToolDefinitions()
	if err != nil {
		return Report{}, fmt.Errorf("replay: build tool definitions: %w", err)
	}
	wrapper, err := untrusted.NewRunWrapper(rec.Run.ID)
	if err != nil {
		return Report{}, fmt.Errorf("replay: build run wrapper: %w", err)
	}
	temperature := opts.Config.Agent.TemperatureValue()
	toolChoice := &llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	sessionID := "ses_" + strings.ReplaceAll(rec.Run.ID, "-", "")

	if len(rec.ToolCalls) == 0 || rec.ToolCalls[0].Seq != 1 || rec.ToolCalls[0].ToolName != "get_case" {
		return Report{}, fmt.Errorf("%w: first tool call is not the seq 1 get_case seed", ErrInvalidRecording)
	}
	index := newRecordingToolIndex(rec.ToolCalls)
	fake := clock.NewFake(rec.ToolCalls[0].StartedAt)
	seed := rec.ToolCalls[0]
	fake.Set(seed.StartedAt)
	report.Clock = append(report.Clock, ClockEvent{
		Kind: "tool", Step: 0, Tool: seed.Seq, At: seed.StartedAt, DurationMS: seed.DurationMS,
	})
	if seed.DurationMS > 0 {
		fake.Advance(time.Duration(seed.DurationMS) * time.Millisecond)
	}
	if divergence, diagnostic := checkToolResultIntegrity(seed, 0); divergence != nil {
		report.Divergences = append(report.Divergences, *divergence)
		report.finalize()
		return report, nil
	} else if diagnostic != nil {
		report.Diagnostics = append(report.Diagnostics, *diagnostic)
	}
	seedVisible, err := agent.ModelVisibleResult(seed.Result, wrapper)
	if err != nil {
		return Report{}, fmt.Errorf("replay: rebuild seed transcript: %w", err)
	}
	messages := agent.InitialTranscript(rec.Run.CaseID, seedVisible)

	maxSteps := opts.Config.Agent.MaxSteps
	if maxSteps <= 0 {
		maxSteps = config.DefaultAgentMaxSteps
	}
	nextToolCallID := 2 // the seed assistant message already owns call id 1 (seed_get_case)
	repairUsed := false
	step := 0
	var (
		proposalKind string
		proposalArgs json.RawMessage
		proposalSeen bool
		stopped      bool
	)
	for {
		if step >= maxSteps {
			report.addDivergence(CodeStepCountMismatch, step, "",
				fmt.Sprintf("rerun exceeded agent.max_steps=%d", maxSteps))
			stopped = true
			break
		}
		step++
		report.StepsTotal = step
		if recorded, ok := recordedLLMCall(rec.LLMCalls, step); ok {
			fake.Set(recorded.StartedAt)
			report.Clock = append(report.Clock, ClockEvent{
				Kind: "llm", Step: step, At: recorded.StartedAt, DurationMS: recorded.DurationMS,
			})
			if recorded.DurationMS > 0 {
				fake.Advance(time.Duration(recorded.DurationMS) * time.Millisecond)
			}
		} else {
			// Beyond the recorded step count the fake clock holds the last
			// recorded instant: a rerun never reads the wall clock.
			report.Clock = append(report.Clock, ClockEvent{Kind: "llm", Step: step, At: fake.Now()})
		}

		request := llm.Request{
			Model:         model,
			System:        prompt.Composed,
			Messages:      append([]llm.Message(nil), messages...),
			Tools:         toolDefs,
			ToolChoice:    toolChoice,
			Temperature:   &temperature, //floatlint:allow provider temperature, not money
			MaxTokens:     opts.Config.Agent.MaxTokensPerResponse,
			SessionID:     sessionID,
			PromptVersion: promptVersion,
		}
		response, err := opts.Client.Complete(ctx, request)
		if err != nil {
			if errors.Is(err, llm.ErrCassetteMiss) {
				return Report{}, fmt.Errorf("%w: %w", ErrCassetteMiss, err)
			}
			return Report{}, fmt.Errorf("replay: rerun completion at step %d: %w", step, err)
		}
		nextToolCallID = agent.EnsureToolCallIDs(response.Message.ToolCalls, nextToolCallID)
		if actual := strings.TrimSpace(response.ActualModel); actual != "" {
			model = actual
		}
		messages = append(messages, response.Message)

		if len(response.Message.ToolCalls) == 0 {
			if repairUsed {
				report.addDivergence(CodeStepCountMismatch, step, "",
					"rerun completion has no tool call after the repair was already used")
				stopped = true
				break
			}
			repairUsed = true
			messages = append(messages, agent.RepairMessage(agent.ReplayRepairNoToolCall))
			continue
		}

		protocolFailure := false
		proposalInStep := false
		for _, call := range response.Message.ToolCalls {
			switch {
			case call.Name == "escalate":
				// The production loop finalizes an escalation immediately after
				// the tool call, exactly like a proposal; rerun must not invent
				// a continuation.
				stopped = true
			case call.Name != "" && proposalToolKinds[call.Name] != "":
				if !agent.IsJSONObject(call.Arguments) {
					protocolFailure = true
					continue
				}
				proposalKind = proposalToolKinds[call.Name]
				proposalArgs = append(json.RawMessage(nil), call.Arguments...)
				proposalSeen = true
				proposalInStep = true
			default:
				recorded, found, matchErr := index.match(call.Name, call.Arguments)
				if matchErr != nil {
					report.addDivergence(CodeToolCallUnrecorded, step, "",
						fmt.Sprintf("tool %q arguments cannot be matched to a recorded call: %v", call.Name, matchErr))
					stopped = true
				} else if !found {
					report.addDivergence(CodeToolCallUnrecorded, step, "",
						fmt.Sprintf("model requested %q with arguments that match no unconsumed recorded tool call", call.Name))
					stopped = true
				} else {
					fake.Set(recorded.StartedAt)
					report.Clock = append(report.Clock, ClockEvent{
						Kind: "tool", Step: step, Tool: recorded.Seq, At: recorded.StartedAt,
					})
					if divergence, diagnostic := checkToolResultIntegrity(*recorded, step); divergence != nil {
						report.Divergences = append(report.Divergences, *divergence)
						stopped = true
					} else {
						if diagnostic != nil {
							report.Diagnostics = append(report.Diagnostics, *diagnostic)
						}
						wrapped, wrapErr := agent.ModelVisibleResult(recorded.Result, wrapper)
						if wrapErr != nil {
							return Report{}, fmt.Errorf("%w: wrap recorded result seq %d: %v", ErrInvalidRecording, recorded.Seq, wrapErr)
						}
						messages = append(messages, agent.ToolMessage(call.ID, wrapped))
					}
				}
			}
			if stopped || proposalInStep {
				break
			}
		}
		if stopped || proposalInStep {
			break
		}
		if protocolFailure {
			if repairUsed {
				report.addDivergence(CodeStepCountMismatch, step, "",
					"rerun completion has malformed tool arguments after the repair was already used")
				stopped = true
				break
			}
			repairUsed = true
			messages = append(messages, agent.RepairMessage(agent.ReplayRepairMalformedArgs))
		}
	}
	if stopped {
		if !proposalSeen && len(report.Divergences) == 0 {
			report.addDivergence(CodeProposalDigestMismatch, 0, "", "rerun ended without producing a proposal")
		}
		report.finalize()
		return report, nil
	}
	if !proposalSeen {
		report.addDivergence(CodeProposalDigestMismatch, 0, "", "rerun ended without producing a proposal")
		report.finalize()
		return report, nil
	}

	rebuilt, err := proposal.Rebuild(proposalKind, proposalArgs, rec.Run.CaseID)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrRebuild, err)
	}
	rebuiltHash, err := proposal.Digest(rebuilt)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrRebuild, err)
	}
	report.ProposalHash = rebuiltHash
	if rebuiltHash != storedHash {
		report.addDivergence(CodeProposalDigestMismatch, 0, "",
			"rerun proposal digest does not match the stored proposal digest")
	}
	report.finalize()
	return report, nil
}

// recordedRequestModel returns the model the first recorded request was made
// with. agent_runs.model is rewritten to the final actual model by FinishRun,
// so the recorded response's requested_model is the faithful step-1 value.
func recordedRequestModel(rec Recording) string {
	for _, call := range rec.LLMCalls {
		var response llm.Response
		if err := json.Unmarshal(call.Response, &response); err != nil {
			continue
		}
		if model := strings.TrimSpace(response.RequestedModel); model != "" {
			return model
		}
	}
	return strings.TrimSpace(rec.Run.Model)
}

// recordedLLMCall returns the recorded completion for a 1-based step.
func recordedLLMCall(calls []LLMCall, step int) (LLMCall, bool) {
	for _, call := range calls {
		if call.Seq == step {
			return call, true
		}
	}
	return LLMCall{}, false
}

// recordingToolIndex matches model-requested tool calls against unconsumed
// recorded calls in ascending seq order. The match key is the production
// (tool_name, canonical arguments) hash, so replay cannot drift from the
// loop's own canonicalization. The seed at seq 1 is injected separately and is
// marked consumed so a later identical get_case request cannot reuse it.
type recordingToolIndex struct {
	calls    []ToolCall
	keys     []string
	consumed []bool
}

func newRecordingToolIndex(calls []ToolCall) *recordingToolIndex {
	index := &recordingToolIndex{
		calls:    calls,
		keys:     make([]string, len(calls)),
		consumed: make([]bool, len(calls)),
	}
	for i, call := range calls {
		// A row whose stored arguments cannot be canonicalized can never match;
		// the empty key is skipped by match.
		if key, err := agent.CanonicalToolCallKey(call.ToolName, call.Args); err == nil {
			index.keys[i] = key
		}
	}
	if len(calls) > 0 {
		index.consumed[0] = true
	}
	return index
}

// match consumes and returns the first unconsumed recorded call whose
// (tool_name, canonical arguments) key equals the requested call. Each
// recorded call is consumed at most once.
func (i *recordingToolIndex) match(name string, args json.RawMessage) (*ToolCall, bool, error) {
	key, err := agent.CanonicalToolCallKey(name, args)
	if err != nil {
		return nil, false, err
	}
	for pos := range i.calls {
		if i.consumed[pos] || i.keys[pos] == "" {
			continue
		}
		if i.keys[pos] == key {
			i.consumed[pos] = true
			call := i.calls[pos]
			return &call, true, nil
		}
	}
	return nil, false, nil
}
