package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/prompts"
)

// Config is the code-and-configuration context replay needs to recompute the
// canonical request hash. It deliberately contains no database, client, or
// network handle. Provider and BaseURL default to the production values, and
// Agent supplies the temperature and response-token limit used at record time.
type Config struct {
	Provider string
	BaseURL  string
	Agent    config.AgentConfig
}

func (c Config) provider() string {
	provider := strings.TrimSpace(c.Provider)
	if provider == "" {
		return llm.ProviderOpenCodeGo
	}
	return provider
}

func (c Config) baseURL() string {
	baseURL := strings.TrimSpace(c.BaseURL)
	if baseURL == "" {
		baseURL = llm.DefaultBaseURL
	}
	return strings.TrimRight(baseURL, "/")
}

// proposalToolKinds maps the five proposal tools to their stored proposal type.
// escalate is absent because it writes no proposal.
var proposalToolKinds = map[string]string{
	"propose_link":       "link",
	"propose_adjustment": "adjustment",
	"propose_write_off":  "write_off",
	"mark_duplicate":     "duplicate",
	"request_info":       "request_info",
}

// ExactReplay reconstructs a recorded proposal run entirely in memory. It
// recomputes every canonical LLM request hash from the recorded seed and
// responses, injects recorded tool results instead of executing them, drives a
// fake clock from recorded timestamps, and rebuilds the proposal through the
// production normalization path.
//
// The signature is the no-DB boundary: there is no context, pool, transaction,
// repository, or client parameter, so replay cannot open a database connection
// or reach a provider.
func ExactReplay(recording Recording, cfg Config) (Report, error) {
	rec := normalizeRecording(recording)
	report := Report{
		RunID:       rec.Run.ID,
		Mode:        ModeExact,
		Outcome:     rec.Run.Outcome,
		StepsTotal:  len(rec.LLMCalls),
		Model:       rec.Run.Model,
		Prompt:      rec.Run.PromptVersion,
		Divergences: []Divergence{},
	}
	if rec.Run.Outcome != "proposal" || rec.Proposal == nil {
		return Report{}, &UnsupportedRunError{
			RunID:       rec.Run.ID,
			Outcome:     rec.Run.Outcome,
			HasProposal: rec.Proposal != nil,
		}
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

	prompt, err := prompts.Load(rec.Run.PromptVersion)
	if err != nil {
		return Report{}, fmt.Errorf("replay: load prompt %q: %w", rec.Run.PromptVersion, err)
	}
	toolDefs, err := agent.ReplayToolDefinitions()
	if err != nil {
		return Report{}, fmt.Errorf("replay: build tool definitions: %w", err)
	}
	wrapper, err := untrusted.NewRunWrapper(rec.Run.ID)
	if err != nil {
		return Report{}, fmt.Errorf("replay: build run wrapper: %w", err)
	}
	provider := cfg.provider()
	baseURL := cfg.baseURL()
	temperature := cfg.Agent.TemperatureValue()
	toolChoice := &llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	sessionID := "ses_" + strings.ReplaceAll(rec.Run.ID, "-", "")

	if len(rec.LLMCalls) == 0 {
		return Report{}, fmt.Errorf("%w: no llm_calls rows", ErrInvalidRecording)
	}
	if len(rec.ToolCalls) == 0 || rec.ToolCalls[0].Seq != 1 || rec.ToolCalls[0].ToolName != "get_case" {
		return Report{}, fmt.Errorf("%w: first tool call is not the seq 1 get_case seed", ErrInvalidRecording)
	}

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
	// The seed get_case call is recorded through the same tool runner as every
	// model-requested call, so its stored args must match the deterministic
	// seed args the transcript was built from.
	if divergence := checkToolArgsIntegrity(messages[1].ToolCalls[0].Arguments, seed, 0); divergence != nil {
		report.Divergences = append(report.Divergences, *divergence)
		report.finalize()
		return report, nil
	}

	nextTool := 1
	repairUsed := false
	var (
		proposalKind string
		proposalArgs json.RawMessage
		proposalSeen bool
		stopped      bool
	)
	for index, call := range rec.LLMCalls {
		step := call.Seq
		var response llm.Response
		if err := json.Unmarshal(call.Response, &response); err != nil {
			return Report{}, fmt.Errorf("%w: decode response for llm seq %d: %v", ErrInvalidRecording, step, err)
		}
		requestModel := ""
		if index == 0 {
			requestModel = response.RequestedModel
			if strings.TrimSpace(requestModel) == "" {
				requestModel = llm.APIID(cfg.Agent.Model)
			}
		} else {
			requestModel = rec.LLMCalls[index-1].Model
		}
		request := llm.Request{
			Model:         requestModel,
			System:        prompt.Composed,
			Messages:      append([]llm.Message(nil), messages...),
			Tools:         toolDefs,
			ToolChoice:    toolChoice,
			Temperature:   &temperature, //floatlint:allow provider temperature, not money
			MaxTokens:     cfg.Agent.MaxTokensPerResponse,
			SessionID:     sessionID,
			PromptVersion: rec.Run.PromptVersion,
		}
		requestHash, err := llm.CanonicalRequestHash(provider, baseURL, request)
		if err != nil {
			return Report{}, fmt.Errorf("replay: hash request for llm seq %d: %w", step, err)
		}
		if requestHash != call.RequestHash {
			report.addDivergence(CodeRequestHashMismatch, step, "", "recomputed canonical request hash does not match llm_calls.request_hash")
			stopped = true
			break
		}

		fake.Set(call.StartedAt)
		report.Clock = append(report.Clock, ClockEvent{
			Kind: "llm", Step: step, At: call.StartedAt, DurationMS: call.DurationMS,
		})
		if call.DurationMS > 0 {
			fake.Advance(time.Duration(call.DurationMS) * time.Millisecond)
		}
		messages = append(messages, response.Message)

		if len(response.Message.ToolCalls) == 0 {
			if repairUsed {
				report.addDivergence(CodeStepCountMismatch, step, "", "recorded response has no tool call after the repair was already used")
				stopped = true
				break
			}
			repairUsed = true
			messages = append(messages, agent.RepairMessage(agent.ReplayRepairNoToolCall))
			continue
		}

		protocolFailure := false
		proposalInStep := false
		for _, toolCall := range response.Message.ToolCalls {
			if nextTool >= len(rec.ToolCalls) {
				report.addDivergence(CodeStepCountMismatch, step, "", "model requested a tool call with no recorded tool result")
				stopped = true
				break
			}
			recorded := rec.ToolCalls[nextTool]
			nextTool++
			if recorded.ToolName != toolCall.Name {
				report.addDivergence(CodeStepCountMismatch, step, "",
					fmt.Sprintf("recorded tool call seq %d is %q but the model requested %q", recorded.Seq, recorded.ToolName, toolCall.Name))
				stopped = true
				break
			}
			// The recorded args are the authoritative recorder input. Compare
			// them with what the model asked for before trusting the result or
			// rebuilding a proposal from them.
			if divergence := checkToolArgsIntegrity(toolCall.Arguments, recorded, step); divergence != nil {
				report.Divergences = append(report.Divergences, *divergence)
				stopped = true
				break
			}
			fake.Set(recorded.StartedAt)
			report.Clock = append(report.Clock, ClockEvent{
				Kind: "tool", Step: step, Tool: recorded.Seq, At: recorded.StartedAt,
			})
			if divergence, diagnostic := checkToolResultIntegrity(recorded, step); divergence != nil {
				report.Divergences = append(report.Divergences, *divergence)
				stopped = true
				break
			} else if diagnostic != nil {
				report.Diagnostics = append(report.Diagnostics, *diagnostic)
			}
			wrapped, err := agent.ModelVisibleResult(recorded.Result, wrapper)
			if err != nil {
				return Report{}, fmt.Errorf("%w: wrap recorded result seq %d: %v", ErrInvalidRecording, recorded.Seq, err)
			}
			messages = append(messages, agent.ToolMessage(toolCall.ID, wrapped))
			if !agent.IsJSONObject(toolCall.Arguments) {
				protocolFailure = true
			}
			// A proposal tool call is terminal only when the recorded call
			// actually succeeded. The production loop finalizes a proposal only
			// after the tool sets ProposalKey; an invalid-arguments (or other
			// failed) proposal call returns an error envelope, the loop keeps
			// going, and a later completion may repair it. Treating the tool
			// name alone as terminal would fabricate a step-count mismatch for
			// a legitimate repaired run.
			if kind, ok := proposalToolKinds[toolCall.Name]; ok && recorded.Error == nil && agent.IsJSONObject(toolCall.Arguments) {
				proposalKind = kind
				proposalArgs = append(json.RawMessage(nil), recorded.Args...)
				proposalSeen = true
				proposalInStep = true
				break
			}
		}
		if stopped {
			break
		}
		if proposalInStep {
			// The production loop finalizes a proposal immediately after its
			// tool call; any remaining calls were never executed and are not
			// recorded. Continuing would fabricate a step sequence.
			if index != len(rec.LLMCalls)-1 || nextTool != len(rec.ToolCalls) {
				report.addDivergence(CodeStepCountMismatch, step, "",
					"recording continues after the proposal tool call")
				stopped = true
			}
			break
		}
		if protocolFailure {
			if repairUsed {
				report.addDivergence(CodeStepCountMismatch, step, "",
					"recorded response has malformed arguments after the repair was already used")
				stopped = true
				break
			}
			repairUsed = true
			messages = append(messages, agent.RepairMessage(agent.ReplayRepairMalformedArgs))
		}
	}
	if stopped {
		report.finalize()
		return report, nil
	}
	if nextTool != len(rec.ToolCalls) {
		report.addDivergence(CodeStepCountMismatch, 0, "",
			fmt.Sprintf("%d recorded tool call(s) were not reached by the reconstructed loop", len(rec.ToolCalls)-nextTool))
	}
	if !proposalSeen {
		report.addDivergence(CodeStepCountMismatch, 0, "", "no proposal tool call was recorded")
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
			"rebuilt proposal digest does not match the stored proposal digest")
	}
	rebuiltKey, err := proposal.IdempotencyKey(rec.Run.CaseID, rec.Run.ID, rebuilt)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrRebuild, err)
	}
	if rebuiltKey != rec.Proposal.IdempotencyKey {
		report.addDivergence(CodeProposalFieldDiff, 0, "idempotency_key",
			"recomputed idempotency key does not match proposals.idempotency_key")
	}
	report.finalize()
	return report, nil
}

// normalizeRecording sorts the seq-ordered slices defensively. LoadRecording
// already orders them; tests and future loaders are not required to.
func normalizeRecording(recording Recording) Recording {
	rec := recording
	rec.LLMCalls = append([]LLMCall(nil), recording.LLMCalls...)
	sort.SliceStable(rec.LLMCalls, func(i, j int) bool { return rec.LLMCalls[i].Seq < rec.LLMCalls[j].Seq })
	rec.ToolCalls = append([]ToolCall(nil), recording.ToolCalls...)
	sort.SliceStable(rec.ToolCalls, func(i, j int) bool { return rec.ToolCalls[i].Seq < rec.ToolCalls[j].Seq })
	return rec
}

// checkToolArgsIntegrity compares the model-requested arguments with the stored
// tool_calls.args for one call. Both sides are read back from JSONB text, so the
// shared canonical encoder (UseNumber, sorted keys, null distinct from "") makes
// legitimate key-order/whitespace/number-literal differences compare equal.
//
// Empty arguments are normalized exactly as the recorder stored them: the tool
// runner turns empty/nil arguments into {}. A recorded response cannot preserve
// the difference between an empty RawMessage and a literal JSON null (both
// marshal to null), and the runner's normalization observed one of them as {}
// (empty) or null (a literal null supplied by a scripted/cassette client). Both
// stored spellings are therefore accepted when the reconstructed request is
// null; this is confined to malformed calls, which can never record a proposal.
func checkToolArgsIntegrity(requested json.RawMessage, recorded ToolCall, step int) *Divergence {
	requestNormalized, _ := tools.NormalizeToolArgs(requested)
	requestCanonical, requestErr := tools.CanonicalJSON(requestNormalized)
	recordedCanonical, recordedErr := tools.CanonicalJSON(recorded.Args)
	if requestErr != nil || recordedErr != nil {
		divergence := buildToolArgsDivergence(step, recorded,
			fmt.Sprintf("tool arguments are not canonical JSON (requested=%v recorded=%v)", requestErr, recordedErr))
		return &divergence
	}
	if bytes.Equal(requestCanonical, recordedCanonical) {
		return nil
	}
	if bytes.Equal(requestCanonical, []byte("null")) &&
		(bytes.Equal(recordedCanonical, []byte("{}")) || bytes.Equal(recordedCanonical, []byte("null"))) {
		return nil
	}
	divergence := buildToolArgsDivergence(step, recorded,
		"model-requested tool arguments do not match tool_calls.args")
	return &divergence
}

func buildToolArgsDivergence(step int, call ToolCall, detail string) Divergence {
	field := "args"
	full := fmt.Sprintf("%s (tool_calls.seq %d, %s)", detail, call.Seq, call.ToolName)
	return Divergence{Code: CodeToolResultMismatch, Step: step, Field: &field, Detail: &full}
}

// checkToolResultIntegrity recomputes the canonical result hash from the loaded
// row. A non-NULL stored hash must match exactly (hard stop); a NULL legacy hash
// yields the approved diagnostic instead.
func checkToolResultIntegrity(call ToolCall, step int) (*Divergence, *Diagnostic) {
	if call.ResultCanonicalHash == nil {
		return nil, &Diagnostic{
			Code:   CodeToolResultHashUnavailable,
			Step:   step,
			Detail: fmt.Sprintf("tool_calls.seq %d (%s) predates result_canonical_hash; integrity check skipped", call.Seq, call.ToolName),
		}
	}
	computed, err := tools.CanonicalJSONHash(call.Result)
	if err != nil {
		divergence := buildToolResultDivergence(step, call, "recorded tool result is not valid JSON")
		return &divergence, nil
	}
	if computed != *call.ResultCanonicalHash {
		divergence := buildToolResultDivergence(step, call, "recomputed canonical tool result hash does not match tool_calls.result_canonical_hash")
		return &divergence, nil
	}
	return nil, nil
}

func buildToolResultDivergence(step int, call ToolCall, detail string) Divergence {
	full := fmt.Sprintf("%s (tool_calls.seq %d, %s)", detail, call.Seq, call.ToolName)
	return Divergence{Code: CodeToolResultMismatch, Step: step, Detail: &full}
}

// storedFields projects the stored proposal row onto the digest field set.
func storedFields(stored *Proposal) (proposal.Fields, error) {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(stored.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return proposal.Fields{}, fmt.Errorf("decode stored payload: %w", err)
	}
	var evidence []string
	if err := json.Unmarshal(stored.Evidence, &evidence); err != nil {
		return proposal.Fields{}, fmt.Errorf("decode stored evidence: %w", err)
	}
	confidence, err := proposal.NormalizeConfidence(stored.Confidence)
	if err != nil {
		return proposal.Fields{}, fmt.Errorf("normalize stored confidence: %w", err)
	}
	var pattern *string
	if stored.PatternClaimed != nil {
		claimed := *stored.PatternClaimed
		pattern = &claimed
	}
	return proposal.Fields{
		Type:           stored.Type,
		Payload:        payload,
		Rationale:      stored.Rationale,
		Evidence:       evidence,
		Confidence:     confidence,
		PatternClaimed: pattern,
	}, nil
}

func isLowerHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
