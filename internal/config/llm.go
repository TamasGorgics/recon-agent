package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultLLMProvider is the only provider enabled in M2.
	DefaultLLMProvider = "opencode"
	// DefaultLLMBaseURL is the OpenCode Go Console Inference base URL.
	DefaultLLMBaseURL = "https://opencode.ai/inference/go/openai/v1"
	// DefaultLLMModel is the default agent model API ID.
	DefaultLLMModel = "deepseek-v4.1-flash"
	// DefaultLLMTimeout is the default HTTP timeout.
	DefaultLLMTimeout = 90 * time.Second
	// DefaultLLMMaxAttempts is the default total attempt budget.
	DefaultLLMMaxAttempts = 3
	// DefaultLLMProviderSelector is the provider selector prefix.
	DefaultLLMProviderSelector = "opencode-go/"
	maxLLMFallbackModels       = 3
)

var (
	llmModelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	llmAPIKeyPattern  = regexp.MustCompile(`oc_sk_[A-Za-z0-9_-]+`)
	llmBearerPattern  = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/_=-]+`)
	llmSessionPattern = regexp.MustCompile(`ses_[A-Za-z0-9_-]+`)
)

const redactedConfigValue = "[REDACTED]"

// redactLLMValue mirrors the llm package's secret classes for configuration
// errors. The llm package imports config, so the patterns cannot be shared
// without an import cycle.
func redactLLMValue(value string) string {
	value = llmAPIKeyPattern.ReplaceAllString(value, redactedConfigValue)
	value = llmBearerPattern.ReplaceAllString(value, redactedConfigValue)
	value = llmSessionPattern.ReplaceAllString(value, redactedConfigValue)
	return value
}

// LLMConfig is the provider-neutral LLM configuration. The API key is
// environment-only: secrets never come from YAML.
type LLMConfig struct {
	Provider       string        `yaml:"provider"`
	BaseURL        string        `yaml:"base_url"`
	APIKey         string        `yaml:"-"`
	Model          string        `yaml:"model"`
	FallbackModels []string      `yaml:"fallback_models"`
	Timeout        time.Duration `yaml:"timeout"`
	MaxAttempts    int           `yaml:"max_attempts"`
}

// DefaultLLMConfig returns the M2 defaults.
func DefaultLLMConfig() LLMConfig {
	return LLMConfig{
		Provider:       DefaultLLMProvider,
		BaseURL:        DefaultLLMBaseURL,
		Model:          DefaultLLMModel,
		FallbackModels: nil,
		Timeout:        DefaultLLMTimeout,
		MaxAttempts:    DefaultLLMMaxAttempts,
	}
}

// NormalizeLLMModel strips the opencode-go selector prefix and trims space.
func NormalizeLLMModel(model string) string {
	model = strings.TrimSpace(model)
	return strings.TrimPrefix(model, DefaultLLMProviderSelector)
}

// Validate checks structural LLM configuration. It does not require the API
// key, so commands that never call the provider still validate.
func (c LLMConfig) Validate() error {
	if strings.TrimSpace(c.Provider) != DefaultLLMProvider {
		return fmt.Errorf("config: llm.provider must be %q", DefaultLLMProvider)
	}
	base, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return errors.New("config: llm.base_url must be an absolute https URL")
	}
	model := NormalizeLLMModel(c.Model)
	if model == "" {
		return errors.New("config: llm.model is required")
	}
	if !llmModelPattern.MatchString(model) {
		return fmt.Errorf("config: llm.model %q is not a valid API model ID or opencode-go selector", redactLLMValue(c.Model))
	}
	if c.Timeout < time.Second || c.Timeout > 10*time.Minute {
		return fmt.Errorf("config: llm.timeout must be between 1s and 10m, got %s", c.Timeout)
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 5 {
		return fmt.Errorf("config: llm.max_attempts must be between 1 and 5, got %d", c.MaxAttempts)
	}
	if len(c.FallbackModels) > maxLLMFallbackModels {
		return fmt.Errorf("config: llm.fallback_models has %d entries, maximum is %d", len(c.FallbackModels), maxLLMFallbackModels)
	}
	seen := make(map[string]struct{}, len(c.FallbackModels))
	for _, raw := range c.FallbackModels {
		fallback := NormalizeLLMModel(raw)
		if fallback == "" {
			return errors.New("config: llm.fallback_models contains an empty model")
		}
		if !llmModelPattern.MatchString(fallback) {
			return fmt.Errorf("config: llm.fallback_models entry %q is not a valid API model ID or opencode-go selector", redactLLMValue(raw))
		}
		if fallback == model {
			return fmt.Errorf("config: llm.fallback_models entry %q equals llm.model", redactLLMValue(fallback))
		}
		if _, duplicate := seen[fallback]; duplicate {
			return fmt.Errorf("config: llm.fallback_models entry %q is duplicated", redactLLMValue(fallback))
		}
		seen[fallback] = struct{}{}
	}
	return nil
}

// ValidateLive additionally requires the environment-only API key. Error
// messages name OPENCODE_API_KEY but never print a value.
func (c LLMConfig) ValidateLive() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return errors.New("config: OPENCODE_API_KEY is required for live LLM calls")
	}
	return nil
}

func normalizeLLMConfig(c LLMConfig) LLMConfig {
	c.Provider = strings.TrimSpace(c.Provider)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.Model = NormalizeLLMModel(c.Model)
	if c.FallbackModels == nil {
		c.FallbackModels = []string{}
	}
	for i := range c.FallbackModels {
		c.FallbackModels[i] = NormalizeLLMModel(c.FallbackModels[i])
	}
	return c
}
