package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// TestModelBAutoToolCallContract proves glm-5.3-flash works through the same
// OpenAI-compatible adapter as the primary model: tools plus tool_choice auto,
// a parsed tool call, inbound reasoning_content preserved, and correct model
// attribution. There is no provider- or model-specific branch.
func TestModelBAutoToolCallContract(t *testing.T) {
	handler, captured := captureHandler(t, `{
		"id":"chatcmpl-glm-tool","model":"glm-5.3-flash",
		"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"checked the case","tool_calls":[
			{"id":"call-glm-1","type":"function","function":{"name":"get_ping","arguments":"{\"count\":1}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":21,"completion_tokens":9,
			"opencode":{"billing_source":"go","list_cost":0.00000315,"allowance":0.00000189,
				"tokens":{"input":21,"output":9,"reasoning":4}}}
	}`)
	server := newServer(t, handler)
	adapter, _ := newTestAdapter(t, server, nil)

	response, err := adapter.Complete(context.Background(), Request{
		Model:  "glm-5.3-flash",
		System: "tools available",
		Messages: []Message{
			{Role: RoleAssistant, Content: "", ReasoningContent: "previous reasoning",
				ToolCalls: []ToolCall{{ID: "call-glm-0", Name: "get_ping"}}},
			{Role: RoleTool, Content: "pong", ToolCallID: "call-glm-0"},
		},
		Tools:         []ToolDef{{Name: "get_ping", Description: "ping", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:    &ToolChoice{Mode: ToolChoiceAuto},
		MaxTokens:     256,
		SessionID:     "ses_glm_contract_1",
		PromptVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	_, _, _, rawBody := captured.snapshot()
	body := decodeBody(t, rawBody)
	if body["model"] != "glm-5.3-flash" {
		t.Errorf("request model = %#v, want glm-5.3-flash", body["model"])
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %#v, want auto", body["tool_choice"])
	}
	if stream, ok := body["stream"].(bool); !ok || stream {
		t.Errorf("stream = %#v, want false", body["stream"])
	}
	messages := body["messages"].([]any)
	assistant := messages[1].(map[string]any)
	if _, ok := assistant["reasoning_content"]; ok {
		t.Error("reasoning_content was sent outbound")
	}

	if response.StopReason != StopToolCalls {
		t.Errorf("StopReason = %q, want tool_calls", response.StopReason)
	}
	if len(response.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one", response.Message.ToolCalls)
	}
	call := response.Message.ToolCalls[0]
	if call.ID != "call-glm-1" || call.Name != "get_ping" || string(call.Arguments) != `{"count":1}` {
		t.Errorf("tool call = %#v", call)
	}
	if response.Message.ReasoningContent != "checked the case" {
		t.Errorf("inbound reasoning_content = %q", response.Message.ReasoningContent)
	}
	if response.RequestedModel != "glm-5.3-flash" || response.ActualModel != "glm-5.3-flash" {
		t.Errorf("model attribution = %q/%q, want glm-5.3-flash", response.RequestedModel, response.ActualModel)
	}
}

func TestModelBAdvertisesOnlyAutoToolChoice(t *testing.T) {
	model, err := DefaultRegistry().Get("glm-5.3-flash")
	if err != nil {
		t.Fatalf("Get(glm-5.3-flash): %v", err)
	}
	if !model.Capabilities.Tools {
		t.Fatal("glm-5.3-flash does not advertise tools")
	}
	if len(model.Capabilities.ToolChoice) != 1 || model.Capabilities.ToolChoice[0] != string(ToolChoiceAuto) {
		t.Fatalf("glm-5.3-flash tool_choice = %v, want [auto]", model.Capabilities.ToolChoice)
	}
	if model.AllowedFallback {
		t.Fatal("glm-5.3-flash is allowed_fallback; model B must be explicit-only")
	}
}

func TestModelBCannotBeConfiguredAsAutomaticFallback(t *testing.T) {
	cfg := config.DefaultLLMConfig()
	cfg.APIKey = "test-key-not-real"
	cfg.FallbackModels = []string{"glm-5.3-flash"}
	_, err := NewClient(cfg, Options{})
	if err == nil {
		t.Fatal("NewClient accepted glm-5.3-flash as an automatic fallback")
	}
	if !strings.Contains(err.Error(), "allowed_fallback") {
		t.Fatalf("fallback error = %v, want allowed_fallback", err)
	}
}
