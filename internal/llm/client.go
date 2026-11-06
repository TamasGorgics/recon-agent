package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

const (
	// ProviderOpenCodeGo is the provider ID used in responses, pricing, and
	// cassette keys.
	ProviderOpenCodeGo = "opencode-go"
	// DefaultBaseURL is the OpenCode Go Console Inference base URL.
	DefaultBaseURL = "https://opencode.ai/inference/go/openai/v1"
	// DefaultModel is the default agent model.
	DefaultModel = "deepseek-v4.1-flash"
	// DefaultUserAgent identifies this client to the provider.
	DefaultUserAgent = "recon-agent/0.1"
)

var (
	apiModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	selectorPrefix  = ProviderOpenCodeGo + "/"
)

// APIID strips the optional "opencode-go/" selector prefix and returns the bare
// API model ID used on the wire and in the pricing registry.
func APIID(model string) string {
	return strings.TrimPrefix(model, selectorPrefix)
}

// Selector returns the fully qualified selector for an API model ID. An
// already-qualified selector is returned unchanged.
func Selector(model string) string {
	if strings.HasPrefix(model, selectorPrefix) {
		return model
	}
	return selectorPrefix + model
}

// ValidateModelID reports whether model is a valid API ID or opencode-go
// selector.
func ValidateModelID(model string) error {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return errors.New("model is required")
	}
	id := APIID(trimmed)
	if !apiModelPattern.MatchString(id) {
		return fmt.Errorf("model %q is not a valid API model ID or opencode-go selector", Redact(model))
	}
	return nil
}

// Validate checks the provider-neutral request invariants shared by the adapter
// and wrappers.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("llm: model is required")
	}
	if strings.TrimSpace(r.System) == "" && len(r.Messages) == 0 {
		return errors.New("llm: system or messages are required")
	}
	if r.Temperature != nil {
		temperature := *r.Temperature
		if math.IsNaN(temperature) || math.IsInf(temperature, 0) || temperature < 0 || temperature > 2 {
			return fmt.Errorf("llm: temperature %v is outside [0,2]", temperature)
		}
	}
	if r.MaxTokens < 0 {
		return fmt.Errorf("llm: max_tokens %d must be >= 0", r.MaxTokens)
	}

	seen := make(map[string]struct{}, len(r.Tools))
	for i, tool := range r.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return fmt.Errorf("llm: tool %d has an empty name", i)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("llm: tool name %q is duplicated", name)
		}
		seen[name] = struct{}{}
		if !isJSONObject(tool.Parameters) {
			return fmt.Errorf("llm: tool %q parameters must be a JSON object", name)
		}
	}

	if r.ToolChoice != nil {
		if len(r.Tools) == 0 {
			return errors.New("llm: tool_choice requires at least one tool")
		}
		switch r.ToolChoice.Mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		case ToolChoiceTool:
			if strings.TrimSpace(r.ToolChoice.Name) == "" {
				return errors.New("llm: tool_choice mode tool requires a name")
			}
		default:
			return fmt.Errorf("llm: tool_choice mode %q is invalid", r.ToolChoice.Mode)
		}
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return obj != nil
}
