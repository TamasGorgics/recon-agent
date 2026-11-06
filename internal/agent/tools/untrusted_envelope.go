package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// FieldWrapper wraps one untrusted field value for the model-visible
// transcript. *untrusted.Wrapper implements it.
type FieldWrapper interface {
	Wrap(field, value string) string
}

// WrapUntrustedEnvelope projects a tool result envelope for the model by
// rewriting every field named by an object-local untrusted_fields array into a
// wrapped block. A declared name resolves against the record object itself and
// its own raw payload; non-string values are left alone. The raw payload is
// never treated as metadata, so attacker-supplied raw.untrusted_fields is
// ignored. Error envelopes and refs/truncated keys are preserved.
//
// The persisted tool_calls.result stays the runner's canonical envelope; only
// the model-visible copy is wrapped.
func WrapUntrustedEnvelope(envelope json.RawMessage, wrapper FieldWrapper) (json.RawMessage, error) {
	if wrapper == nil {
		return nil, errors.New("tools: wrap untrusted envelope: nil wrapper")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(envelope))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("tools: wrap untrusted envelope: %w", err)
	}
	wrapUntrustedFields(value, wrapper)
	out, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("tools: wrap untrusted envelope: %w", err)
	}
	return out, nil
}

// wrapUntrustedFields mirrors walkUntrusted's object-local rule while
// replacing the resolved strings. It is deliberately not exported.
func wrapUntrustedFields(node any, wrapper FieldWrapper) {
	switch typed := node.(type) {
	case map[string]any:
		if declared, ok := typed["untrusted_fields"].([]any); ok {
			raw, _ := typed["raw"].(map[string]any)
			for _, item := range declared {
				field, ok := item.(string)
				if !ok || field == "" {
					continue
				}
				if value, ok := typed[field].(string); ok {
					typed[field] = wrapper.Wrap(field, value)
				}
				if raw != nil {
					if value, ok := raw[field].(string); ok {
						raw[field] = wrapper.Wrap(field, value)
					}
				}
			}
		}
		for key, child := range typed {
			if key == "raw" || key == "untrusted_fields" {
				continue
			}
			wrapUntrustedFields(child, wrapper)
		}
	case []any:
		for _, child := range typed {
			wrapUntrustedFields(child, wrapper)
		}
	}
}
