package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

// Agent defaults from the M2-09 locked contract.
const (
	DefaultAgentPromptVersion        = "v1"
	DefaultAgentMaxSteps             = 15
	DefaultAgentMaxToolCalls         = 15
	DefaultAgentMaxTotalTokens       = 100000
	DefaultAgentMaxCostUSD           = "0.50"
	DefaultAgentMaxWallClock         = 5 * time.Minute
	DefaultAgentTemperature          = 0.0
	DefaultAgentMaxTokensPerResponse = 2048
)

// DecimalString is a YAML scalar that must be quoted. It prevents YAML from
// silently coercing a float such as 0.50 into a money string.
type DecimalString string

// UnmarshalYAML accepts only a scalar string tag.
func (d *DecimalString) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("decimal values must be quoted strings")
	}
	*d = DecimalString(strings.TrimSpace(node.Value))
	return nil
}

// String returns the underlying decimal text.
func (d DecimalString) String() string { return string(d) }

// AgentConfig configures one bounded agent run. Monetary limits are decimal
// strings so a YAML float is a configuration error rather than silently
// rounded money.
type AgentConfig struct {
	PromptVersion        string        `yaml:"prompt_version"`
	Model                string        `yaml:"model"`
	MaxSteps             int           `yaml:"max_steps"`
	MaxToolCalls         int           `yaml:"max_tool_calls"`
	MaxTotalTokens       int64         `yaml:"max_total_tokens"`
	MaxCostUSD           DecimalString `yaml:"max_cost_usd"`
	MaxWallClock         time.Duration `yaml:"max_wall_clock"`
	Temperature          *float64      `yaml:"temperature"` //floatlint:allow provider temperature, not money
	MaxTokensPerResponse int           `yaml:"max_tokens_per_response"`
}

// DefaultAgentConfig returns the locked M2-09 defaults. The temperature pointer
// is a fresh allocation so callers cannot mutate the package default.
func DefaultAgentConfig() AgentConfig {
	temperature := DefaultAgentTemperature
	return AgentConfig{
		PromptVersion:        DefaultAgentPromptVersion,
		MaxSteps:             DefaultAgentMaxSteps,
		MaxToolCalls:         DefaultAgentMaxToolCalls,
		MaxTotalTokens:       DefaultAgentMaxTotalTokens,
		MaxCostUSD:           DefaultAgentMaxCostUSD,
		MaxWallClock:         DefaultAgentMaxWallClock,
		Temperature:          &temperature,
		MaxTokensPerResponse: DefaultAgentMaxTokensPerResponse,
	}
}

// MaxCost returns the parsed per-run cost cap. Zero is a zero-dollar budget,
// not an unlimited one. Callers must run Validate first; an invalid value
// returns zero, which Validate rejects before a runner is built.
func (c AgentConfig) MaxCost() decimal.Decimal {
	value, err := decimal.NewFromString(strings.TrimSpace(c.MaxCostUSD.String()))
	if err != nil {
		return decimal.Zero
	}
	return value
}

// TemperatureValue returns the configured temperature, defaulting a nil
// pointer to zero (the locked default).
func (c AgentConfig) TemperatureValue() float64 {
	if c.Temperature == nil {
		return DefaultAgentTemperature
	}
	return *c.Temperature
}

// Validate checks the M2-09 agent block ranges. Prompt version existence is
// validated by the prompts loader when a run starts.
func (c AgentConfig) Validate() error {
	if strings.TrimSpace(c.PromptVersion) == "" {
		return errors.New("config: agent.prompt_version is required")
	}
	model := NormalizeLLMModel(c.Model)
	if c.Model != "" {
		if model == "" || !llmModelPattern.MatchString(model) {
			return fmt.Errorf("config: agent.model %q is not a valid API model ID or opencode-go selector", redactLLMValue(c.Model))
		}
	}
	if c.MaxSteps < 1 || c.MaxSteps > 50 {
		return fmt.Errorf("config: agent.max_steps must be between 1 and 50, got %d", c.MaxSteps)
	}
	if c.MaxToolCalls < 1 || c.MaxToolCalls > 50 {
		return fmt.Errorf("config: agent.max_tool_calls must be between 1 and 50, got %d", c.MaxToolCalls)
	}
	if c.MaxTotalTokens <= 0 {
		return fmt.Errorf("config: agent.max_total_tokens must be positive, got %d", c.MaxTotalTokens)
	}
	cost, err := decimal.NewFromString(strings.TrimSpace(c.MaxCostUSD.String()))
	if err != nil {
		return errors.New("config: agent.max_cost_usd must be a decimal string")
	}
	if cost.IsNegative() {
		return fmt.Errorf("config: agent.max_cost_usd must be non-negative, got %q", c.MaxCostUSD)
	}
	if c.MaxWallClock < 30*time.Second || c.MaxWallClock > 30*time.Minute {
		return fmt.Errorf("config: agent.max_wall_clock must be between 30s and 30m, got %s", c.MaxWallClock)
	}
	temperature := c.TemperatureValue()
	if temperature < 0 || temperature > 2 {
		return fmt.Errorf("config: agent.temperature must be between 0 and 2, got %v", temperature)
	}
	if c.MaxTokensPerResponse < 1 || c.MaxTokensPerResponse > 8192 {
		return fmt.Errorf("config: agent.max_tokens_per_response must be between 1 and 8192, got %d", c.MaxTokensPerResponse)
	}
	return nil
}

// normalizeAgentConfig trims values and applies the llm.model fallback and the
// temperature pointer default. It never mutates the caller's pointer target.
func normalizeAgentConfig(agent AgentConfig, llmModel string) AgentConfig {
	agent.PromptVersion = strings.TrimSpace(agent.PromptVersion)
	agent.Model = strings.TrimSpace(agent.Model)
	if agent.Model == "" {
		agent.Model = NormalizeLLMModel(llmModel)
	} else {
		agent.Model = NormalizeLLMModel(agent.Model)
	}
	agent.MaxCostUSD = DecimalString(strings.TrimSpace(agent.MaxCostUSD.String()))
	if agent.Temperature == nil {
		temperature := DefaultAgentTemperature
		agent.Temperature = &temperature
	}
	return agent
}

// ValidateAgentRuntime requires the role DSNs an agent run needs. The error
// names the environment/YAML key and never prints a secret.
func (c Config) ValidateAgentRuntime() error {
	if strings.TrimSpace(c.Database.AgentURL) == "" {
		return errors.New("config: database.agent_url is required (set DATABASE_AGENT_URL or database.agent_url in CONFIG_FILE)")
	}
	return nil
}
