package agent

import (
	"encoding/json"
	"fmt"

	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// This file exports the deterministic loop seams the replay package needs.
// Nothing here changes production behavior: every function delegates to the
// same unexported helper the loop already uses.

// Repair prompts appended by the loop after a malformed or empty completion.
const (
	ReplayRepairNoToolCall    = repairNoToolCall
	ReplayRepairMalformedArgs = repairMalformedArgs
)

// InitialTranscript rebuilds the deterministic seed transcript for one case.
func InitialTranscript(caseID string, seedResult json.RawMessage) []llm.Message {
	return initialTranscript(caseID, seedResult)
}

// ToolMessage appends one model-visible tool result keyed by its call id.
func ToolMessage(callID string, wrapped json.RawMessage) llm.Message {
	return toolMessage(callID, wrapped)
}

// EnsureToolCallIDs assigns stable synthetic ids to provider tool calls that
// have none. Rerun uses it exactly as the production loop does so the
// assistant tool_call and its injected tool result always share an id.
func EnsureToolCallIDs(calls []llm.ToolCall, next int) int {
	return ensureToolCallIDs(calls, next)
}

// RepairMessage appends one exact repair string.
func RepairMessage(reason string) llm.Message {
	return repairMessage(reason)
}

// ModelVisibleResult re-wraps a stored envelope with the run-id-derived
// untrusted boundary token, producing the transcript bytes the loop sent.
func ModelVisibleResult(envelope json.RawMessage, wrapper tools.FieldWrapper) (json.RawMessage, error) {
	return modelVisibleResult(envelope, wrapper)
}

// IsJSONObject reports whether raw is a syntactically valid non-null JSON
// object. It mirrors the loop's malformed-arguments branch.
func IsJSONObject(raw json.RawMessage) bool {
	return isJSONObject(raw)
}

// CanonicalToolCallKey hashes a tool name and its canonical arguments. It is
// the production dedupe/injection key, exported for replay matching.
func CanonicalToolCallKey(name string, args json.RawMessage) (string, error) {
	return canonicalCallKey(name, args)
}

// ReplayToolDefinitions returns the exact production tool-definition list
// without any database or provider dependency. The tools are registered only
// for their names, descriptions, and schemas; replay never calls them.
func ReplayToolDefinitions() ([]llm.ToolDef, error) {
	registry := tools.NewRegistry()
	if err := tools.RegisterPart1(registry, tools.Deps{}); err != nil {
		return nil, fmt.Errorf("agent: register replay read tools: %w", err)
	}
	if err := tools.RegisterPart2(registry, tools.Deps{}); err != nil {
		return nil, fmt.Errorf("agent: register replay read tools: %w", err)
	}
	if err := proposal.RegisterDefinitions(registry); err != nil {
		return nil, fmt.Errorf("agent: register replay proposal tools: %w", err)
	}
	return toolDefinitions(registry)
}
