package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

const (
	seedToolCallID = "seed_get_case"

	repairMalformedArgs = "Your tool call arguments were not valid JSON. Reply with a single tool call whose arguments are a JSON object matching the tool schema."
	repairNoToolCall    = "Continue the investigation with a tool call, or call escalate."
)

// seedUserMessage is the only free-form user text in a run. It contains the
// case UUID and nothing time- or environment-dependent.
func seedUserMessage(caseID string) string {
	return "Investigate reconciliation case " + caseID + ". Start from the case data returned by get_case."
}

// initialTranscript builds the deterministic seed: user prompt, one assistant
// seed call, and its tool result.
func initialTranscript(caseID string, seedResult json.RawMessage) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleUser, Content: seedUserMessage(caseID)},
		{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:        seedToolCallID,
				Name:      "get_case",
				Arguments: json.RawMessage(fmt.Sprintf(`{"case_id":%q}`, caseID)),
			}},
		},
		{Role: llm.RoleTool, ToolCallID: seedToolCallID, Content: string(seedResult)},
	}
}

// ensureToolCallIDs replaces missing or blank provider tool-call ids with stable
// synthetic ids and returns the next synthetic sequence number. Existing
// provider ids are preserved. The same message is used for the assistant
// tool_call and its tool result, so the next request is internally consistent.
func ensureToolCallIDs(calls []llm.ToolCall, next int) int {
	used := make(map[string]struct{}, len(calls))
	for i := range calls {
		calls[i].ID = strings.TrimSpace(calls[i].ID)
		if calls[i].ID != "" {
			used[calls[i].ID] = struct{}{}
		}
	}
	for i := range calls {
		if calls[i].ID != "" {
			continue
		}
		for {
			id := fmt.Sprintf("call_%d", next)
			next++
			if _, exists := used[id]; exists {
				continue
			}
			calls[i].ID = id
			used[id] = struct{}{}
			break
		}
	}
	return next
}

// toolMessage appends one model-visible tool result keyed by the assistant
// message's tool-call id. The caller must pass an id assigned by
// ensureToolCallIDs so the two transcript messages match.
func toolMessage(callID string, wrapped json.RawMessage) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: callID, Content: string(wrapped)}
}

// repairMessage appends the exact repair string after the assistant response.
func repairMessage(reason string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: reason}
}

// isJSONObject reports whether raw is a syntactically valid non-null JSON
// object. Empty arguments are not an object.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return false
	}
	return object != nil
}

// canonicalCallKey hashes the tool name and the canonical JSON arguments. The
// provider call id is deliberately excluded. The result is lowercase hex.
func canonicalCallKey(name string, args json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("agent: canonicalize tool arguments: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errors.New("agent: canonicalize tool arguments: trailing data")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("agent: canonicalize tool arguments: %w", err)
	}
	hasher := sha256.New()
	hasher.Write([]byte(name))
	hasher.Write([]byte{0})
	hasher.Write(canonical)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// modelVisibleResult wraps the envelope's declared untrusted fields for the
// transcript. The caller keeps the unwrapped envelope for tool_calls.
func modelVisibleResult(envelope json.RawMessage, wrapper tools.FieldWrapper) (json.RawMessage, error) {
	if len(envelope) == 0 {
		return nil, errors.New("agent: empty tool envelope")
	}
	return tools.WrapUntrustedEnvelope(envelope, wrapper)
}

// responseUsageJSON returns the normalized usage object for llm_calls.usage.
func responseUsageJSON(response llm.Response) (json.RawMessage, error) {
	usage, err := json.Marshal(response.Usage)
	if err != nil {
		return nil, fmt.Errorf("agent: encode llm usage: %w", err)
	}
	return usage, nil
}

// responseJSON returns the full normalized response for llm_calls.response.
func responseJSON(response llm.Response) (json.RawMessage, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("agent: encode llm response: %w", err)
	}
	return data, nil
}
