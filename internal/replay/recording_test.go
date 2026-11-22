package replay_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/prompts"
)

const (
	testRunID     = "11111111-1111-4111-8111-111111111111"
	testCaseID    = "22222222-2222-4222-8222-222222222222"
	testPrompt    = "v1"
	testStateHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var (
	testSeedTime     = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	testLLMTime      = time.Date(2026, 3, 1, 10, 0, 5, 0, time.UTC)
	testProposalTime = time.Date(2026, 3, 1, 10, 0, 7, 0, time.UTC)
)

func syntheticConfig() replay.Config {
	agentConfig := config.DefaultAgentConfig()
	agentConfig.Model = config.DefaultLLMModel
	agentConfig.PromptVersion = testPrompt
	return replay.Config{
		Provider: config.DefaultLLMProvider,
		BaseURL:  config.DefaultLLMBaseURL,
		Agent:    agentConfig,
	}
}

// syntheticRecording builds a complete, self-consistent recording for the
// engine tests. It mirrors the reconstruction path (and the production loop's
// request construction) so a mutation isolates exactly one rule.
func syntheticRecording(t *testing.T) replay.Recording {
	t.Helper()
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

	proposalArgs := mustJSON(t, map[string]any{
		"case_id":    testCaseID,
		"rationale":  "Need the settlement reference before deciding.",
		"evidence":   []string{"case:" + testCaseID},
		"confidence": 0.5,
		"question":   "Which settlement reference matches this case?",
	})
	proposalEnvelope := mustJSON(t, map[string]any{
		"data":             map[string]any{"status": "recorded"},
		"refs":             []string{},
		"untrusted_fields": []string{},
		"truncated":        false,
	})
	proposalResultHash, err := tools.CanonicalJSONHash(proposalEnvelope)
	if err != nil {
		t.Fatalf("proposal canonical hash: %v", err)
	}

	fields, err := proposal.Rebuild("request_info", proposalArgs, testCaseID)
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

	seedVisible, err := agent.ModelVisibleResult(seedEnvelope, wrapper)
	if err != nil {
		t.Fatalf("wrap seed: %v", err)
	}
	messages := agent.InitialTranscript(testCaseID, seedVisible)
	temperature := cfg.Agent.TemperatureValue()
	request := llm.Request{
		Model:         cfg.Agent.Model,
		System:        prompt.Composed,
		Messages:      messages,
		Tools:         toolDefs,
		ToolChoice:    &llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		Temperature:   &temperature, //floatlint:allow provider temperature, not money
		MaxTokens:     cfg.Agent.MaxTokensPerResponse,
		SessionID:     "ses_" + strings.ReplaceAll(testRunID, "-", ""),
		PromptVersion: testPrompt,
	}
	requestHash, err := llm.CanonicalRequestHash(cfg.Provider, cfg.BaseURL, request)
	if err != nil {
		t.Fatalf("request hash: %v", err)
	}
	response := llm.Response{
		Provider:       cfg.Provider,
		RequestedModel: llm.APIID(cfg.Agent.Model),
		ActualModel:    cfg.Agent.Model,
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:        "call_1",
				Name:      "request_info",
				Arguments: proposalArgs,
			}},
		},
		StopReason: llm.StopToolCalls,
	}
	responseBody, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	usageBody, err := json.Marshal(response.Usage)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
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
		LLMCalls: []replay.LLMCall{{
			Seq:         1,
			Model:       cfg.Agent.Model,
			RequestHash: requestHash,
			Response:    responseBody,
			Usage:       usageBody,
			StartedAt:   testLLMTime,
			DurationMS:  5,
		}},
		ToolCalls: []replay.ToolCall{
			{
				Seq:                 1,
				ToolName:            "get_case",
				Args:                seedArgs,
				Result:              seedEnvelope,
				ResultCanonicalHash: &seedHash,
				StartedAt:           testSeedTime,
			},
			{
				Seq:                 2,
				ToolName:            "request_info",
				Args:                proposalArgs,
				Result:              proposalEnvelope,
				ResultCanonicalHash: &proposalResultHash,
				StartedAt:           testProposalTime,
			},
		},
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

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
