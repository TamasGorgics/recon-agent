//go:build integration

package replay_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

const scenarioScriptPath = "testdata/scenarios.json"

const scenarioCasePlaceholder = "{{case_id}}"

// scenarioScript is the committed deterministic response sequence for the
// 20-run gate. One script serves every case because arguments are templated by
// case id instead of hand-written per case.
type scenarioScript struct {
	Schema    int                 `json:"schema"`
	Scenarios map[string]scenario `json:"scenarios"`
}

type scenario struct {
	Steps []scenarioStep `json:"steps"`
}

type scenarioStep struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

func loadScenarioScript(t *testing.T) scenarioScript {
	t.Helper()
	data, err := os.ReadFile(scenarioScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scenarioScriptPath, err)
	}
	var script scenarioScript
	if err := json.Unmarshal(data, &script); err != nil {
		t.Fatalf("decode %s: %v", scenarioScriptPath, err)
	}
	if script.Schema != 1 {
		t.Fatalf("%s schema = %d, want 1", scenarioScriptPath, script.Schema)
	}
	if len(script.Scenarios) != 3 {
		t.Fatalf("%s has %d scenarios, want 3", scenarioScriptPath, len(script.Scenarios))
	}
	return script
}

// scriptedClient renders the scenario into one deterministic llm.Client for a
// single case. Placeholders are substituted in raw argument bytes so the
// proposal args exactly match the case under test.
func (s scenarioScript) scriptedClient(t *testing.T, name, caseID string, cfg config.Config) *llm.Scripted {
	t.Helper()
	steps, ok := s.Scenarios[name]
	if !ok {
		t.Fatalf("scenario %q is missing from %s", name, scenarioScriptPath)
	}
	if len(steps.Steps) == 0 {
		t.Fatalf("scenario %q has no steps", name)
	}
	scripted := make([]llm.ScriptedStep, 0, len(steps.Steps))
	for index, step := range steps.Steps {
		args := bytes.ReplaceAll(append(json.RawMessage(nil), step.Arguments...),
			[]byte(scenarioCasePlaceholder), []byte(caseID))
		if bytes.Contains(args, []byte("{{")) {
			t.Fatalf("scenario %q step %d has an unresolved placeholder: %s", name, index+1, args)
		}
		if !json.Valid(args) || !strings.HasPrefix(strings.TrimSpace(string(args)), "{") {
			t.Fatalf("scenario %q step %d arguments are not a JSON object: %s", name, index+1, args)
		}
		scripted = append(scripted, llm.ScriptedStep{Response: llm.Response{
			Provider:       cfg.LLM.Provider,
			RequestedModel: cfg.Agent.Model,
			ActualModel:    cfg.Agent.Model,
			Message: llm.Message{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{
					ID:        fmt.Sprintf("call_%d", index+1),
					Name:      step.Tool,
					Arguments: args,
				}},
			},
			StopReason: llm.StopToolCalls,
			Usage:      llm.Usage{PromptTokens: 180, CompletionTokens: 24, TotalTokens: 204},
			Latency:    time.Duration(index+1) * time.Millisecond,
		}})
	}
	return llm.NewScripted(scripted...)
}
