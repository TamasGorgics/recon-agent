//go:build live

package llm_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

func liveTemperatureZero() *float64 { //floatlint:allow provider temperature, not money
	value := 0.0 //floatlint:allow provider temperature, not money
	return &value
}

func liveSessionID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random session: %v", err)
	}
	return "ses_live_" + hex.EncodeToString(raw)
}

func TestLiveOpenCode(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("OPENCODE_API_KEY"))
	if key == "" || os.Getenv("CI") != "" {
		t.Skip("live test requires OPENCODE_API_KEY and no CI")
	}
	baseURL := strings.TrimSpace(os.Getenv("OPENCODE_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("OPENCODE_MODEL"))
	if model == "" {
		model = llm.DefaultModel
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client, err := llm.NewOpenCode(llm.OpenCodeOptions{
		BaseURL:      baseURL,
		APIKey:       key,
		Clock:        clock.Real{},
		Logger:       logger,
		UserAgent:    "recon-agent/live-test",
		ClientHeader: "recon-agent/live-test",
	})
	if err != nil {
		t.Fatalf("NewOpenCode: %v", err)
	}
	session := liveSessionID(t)

	t.Run("text", func(t *testing.T) {
		response, err := client.Complete(context.Background(), llm.Request{
			Model:         model,
			System:        "You are a deterministic smoke test.",
			Messages:      []llm.Message{{Role: llm.RoleUser, Content: "Reply with the single word: pong"}},
			Temperature:   liveTemperatureZero(),
			MaxTokens:     32,
			SessionID:     session,
			PromptVersion: "live",
		})
		if err != nil {
			t.Fatalf("Complete text: %v", err)
		}
		if response.ID == "" {
			t.Error("response ID is empty")
		}
		if response.Message.Content == "" && response.Message.ReasoningContent == "" {
			t.Error("response has neither content nor reasoning")
		}
		if response.Usage.PromptTokens <= 0 {
			t.Errorf("prompt tokens = %d, want > 0", response.Usage.PromptTokens)
		}
		if !response.Usage.ListCostReported {
			t.Error("opencode.list_cost was not reported")
		}
		if !response.Usage.AllowanceReported {
			t.Error("opencode.allowance was not reported")
		}
		if response.Usage.BillingSource == "" {
			t.Error("billing source is empty")
		}
		if response.ActualModel != model {
			t.Errorf("actual model = %q, want %q", response.ActualModel, model)
		}
		if strings.Contains(string(response.Raw), "oc_sk_") {
			t.Error("raw response contains a key prefix")
		}
	})

	t.Run("tool", func(t *testing.T) {
		response, err := client.Complete(context.Background(), llm.Request{
			Model:  model,
			System: "You must call the provided tool.",
			Messages: []llm.Message{{
				Role:    llm.RoleUser,
				Content: "Call get_ping with no arguments.",
			}},
			Tools: []llm.ToolDef{{
				Name:        "get_ping",
				Description: "Return pong.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			}},
			ToolChoice:  &llm.ToolChoice{Mode: llm.ToolChoiceTool, Name: "get_ping"},
			Temperature: liveTemperatureZero(),
			// Reasoning is enabled for this model and its tokens count toward
			// MaxTokens. A budget of 32 can be exhausted before the tool call is
			// emitted (stop reason "length"), so use 256 for a stable smoke test.
			MaxTokens:     256,
			SessionID:     session,
			PromptVersion: "live",
		})
		if err != nil {
			t.Fatalf("Complete tool: %v", err)
		}
		if response.StopReason != llm.StopToolCalls {
			t.Errorf("stop reason = %q, want %q", response.StopReason, llm.StopToolCalls)
		}
		if len(response.Message.ToolCalls) != 1 {
			t.Fatalf("tool calls = %v, want one", response.Message.ToolCalls)
		}
		call := response.Message.ToolCalls[0]
		if call.Name != "get_ping" {
			t.Errorf("tool name = %q", call.Name)
		}
		arguments := strings.TrimSpace(string(call.Arguments))
		if arguments == "" {
			arguments = "{}"
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
			t.Errorf("tool arguments are not JSON: %v (%q)", err, arguments)
		}
		if response.Usage.PromptTokens <= 0 {
			t.Errorf("prompt tokens = %d, want > 0", response.Usage.PromptTokens)
		}
	})

	if strings.Contains(logs.String(), "oc_sk_") {
		t.Error("captured logs contain an API key prefix")
	}
	if strings.Contains(logs.String(), session) {
		t.Error("captured logs contain the session ID")
	}
}
