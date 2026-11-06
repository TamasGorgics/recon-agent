package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestAgentDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	agent := cfg.Agent
	if got, want := agent.PromptVersion, "v1"; got != want {
		t.Errorf("PromptVersion = %q, want %q", got, want)
	}
	if got, want := agent.Model, DefaultLLMModel; got != want {
		t.Errorf("Model = %q, want llm.model %q", got, want)
	}
	if got, want := agent.MaxSteps, 15; got != want {
		t.Errorf("MaxSteps = %d, want %d", got, want)
	}
	if got, want := agent.MaxToolCalls, 15; got != want {
		t.Errorf("MaxToolCalls = %d, want %d", got, want)
	}
	if got, want := agent.MaxTotalTokens, int64(100000); got != want {
		t.Errorf("MaxTotalTokens = %d, want %d", got, want)
	}
	if got, want := agent.MaxCostUSD.String(), "0.50"; got != want {
		t.Errorf("MaxCostUSD = %q, want %q", got, want)
	}
	if got, want := agent.MaxWallClock, 5*time.Minute; got != want {
		t.Errorf("MaxWallClock = %s, want %s", got, want)
	}
	if got, want := agent.TemperatureValue(), 0.0; got != want {
		t.Errorf("TemperatureValue = %v, want %v", got, want)
	}
	if got, want := agent.MaxTokensPerResponse, 2048; got != want {
		t.Errorf("MaxTokensPerResponse = %d, want %d", got, want)
	}
	if err := agent.Validate(); err != nil {
		t.Errorf("default Agent.Validate() = %v, want nil", err)
	}
}

func TestAgentZeroCostCapIsValid(t *testing.T) {
	clearEnv(t)
	cfg := DefaultAgentConfig()
	cfg.MaxCostUSD = "0"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected a zero cap: %v", err)
	}
	if !cfg.MaxCost().IsZero() {
		t.Fatalf("MaxCost() = %s, want zero", cfg.MaxCost())
	}

	path := writeConfig(t, `
database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
agent:
  max_cost_usd: "0"
`)
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("Validate() rejected YAML max_cost_usd 0: %v", err)
	}
	if !loaded.Agent.MaxCost().IsZero() {
		t.Fatalf("loaded MaxCost() = %s, want zero", loaded.Agent.MaxCost())
	}
}

func TestAgentYAMLAndModelFallback(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
llm:
  model: deepseek-v4.1-flash
agent:
  prompt_version: v1
  max_steps: 7
  max_tool_calls: 9
  max_total_tokens: 4200
  max_cost_usd: "0.25"
  max_wall_clock: 90s
  temperature: 0.3
  max_tokens_per_response: 512
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Agent.Model, "deepseek-v4.1-flash"; got != want {
		t.Errorf("Agent.Model fallback = %q, want %q", got, want)
	}
	if got, want := cfg.Agent.MaxSteps, 7; got != want {
		t.Errorf("MaxSteps = %d, want %d", got, want)
	}
	if got, want := cfg.Agent.TemperatureValue(), 0.3; got != want {
		t.Errorf("TemperatureValue = %v, want %v", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestAgentModelNormalizesSelector(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
agent:
  model: opencode-go/glm-5.3-flash
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Agent.Model, "glm-5.3-flash"; got != want {
		t.Errorf("Agent.Model = %q, want normalized %q", got, want)
	}
}

func TestAgentRejectsYAMLFloatCost(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
agent:
  max_cost_usd: 0.50
`)
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted a YAML float for agent.max_cost_usd")
	}
}

func TestAgentValidationRanges(t *testing.T) {
	valid := DefaultAgentConfig()
	tests := []struct {
		name   string
		mutate func(*AgentConfig)
	}{
		{"empty prompt version", func(c *AgentConfig) { c.PromptVersion = " " }},
		{"bad model", func(c *AgentConfig) { c.Model = "../escape" }},
		{"zero steps", func(c *AgentConfig) { c.MaxSteps = 0 }},
		{"too many steps", func(c *AgentConfig) { c.MaxSteps = 51 }},
		{"zero tool calls", func(c *AgentConfig) { c.MaxToolCalls = 0 }},
		{"too many tool calls", func(c *AgentConfig) { c.MaxToolCalls = 51 }},
		{"zero tokens", func(c *AgentConfig) { c.MaxTotalTokens = 0 }},
		{"negative cost", func(c *AgentConfig) { c.MaxCostUSD = DecimalString("-0.01") }},
		{"bad cost", func(c *AgentConfig) { c.MaxCostUSD = DecimalString("fifty cents") }},
		{"short wall clock", func(c *AgentConfig) { c.MaxWallClock = 29 * time.Second }},
		{"long wall clock", func(c *AgentConfig) { c.MaxWallClock = 31 * time.Minute }},
		{"negative temperature", func(c *AgentConfig) { zero := -0.1; c.Temperature = &zero }},
		{"hot temperature", func(c *AgentConfig) { hot := 2.1; c.Temperature = &hot }},
		{"zero max tokens", func(c *AgentConfig) { c.MaxTokensPerResponse = 0 }},
		{"too many max tokens", func(c *AgentConfig) { c.MaxTokensPerResponse = 8193 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			temperature := *valid.Temperature
			cfg.Temperature = &temperature
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() accepted invalid config: %+v", cfg)
			}
		})
	}
}

func TestAgentEnvOverlayAndRuntimeValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDatabaseAppURL, "postgres://app:app@db:5432/recon?sslmode=disable")
	t.Setenv(EnvDatabaseAgentURL, "postgres://agent:agent@db:5432/recon?sslmode=disable")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Database.AppURL; got != "postgres://app:app@db:5432/recon?sslmode=disable" {
		t.Errorf("Database.AppURL = %q", got)
	}
	if got := cfg.Database.AgentURL; got != "postgres://agent:agent@db:5432/recon?sslmode=disable" {
		t.Errorf("Database.AgentURL = %q", got)
	}
	if err := cfg.ValidateAgentRuntime(); err != nil {
		t.Errorf("ValidateAgentRuntime() = %v, want nil", err)
	}

	cfg.Database.AgentURL = ""
	err = cfg.ValidateAgentRuntime()
	if err == nil {
		t.Fatal("ValidateAgentRuntime() accepted a missing agent_url")
	}
	if !strings.Contains(err.Error(), "DATABASE_AGENT_URL") || strings.Contains(err.Error(), "agent:agent") {
		t.Fatalf("ValidateAgentRuntime() error = %v, want the key name and no secret", err)
	}
}

func TestAgentModelEmptyUsesLLMModel(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
llm:
  model: opencode-go/glm-5.3-flash
agent:
  model: ""
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Agent.Model, "glm-5.3-flash"; got != want {
		t.Errorf("Agent.Model = %q, want normalised llm.model %q", got, want)
	}
}
