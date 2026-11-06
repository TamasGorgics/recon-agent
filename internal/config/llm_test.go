package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLLMDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	llm := cfg.LLM
	if llm.Provider != DefaultLLMProvider {
		t.Errorf("Provider = %q, want %q", llm.Provider, DefaultLLMProvider)
	}
	if llm.BaseURL != DefaultLLMBaseURL {
		t.Errorf("BaseURL = %q, want %q", llm.BaseURL, DefaultLLMBaseURL)
	}
	if llm.Model != DefaultLLMModel {
		t.Errorf("Model = %q, want %q", llm.Model, DefaultLLMModel)
	}
	if len(llm.FallbackModels) != 0 {
		t.Errorf("FallbackModels = %v, want empty", llm.FallbackModels)
	}
	if llm.Timeout != DefaultLLMTimeout {
		t.Errorf("Timeout = %s, want %s", llm.Timeout, DefaultLLMTimeout)
	}
	if llm.MaxAttempts != DefaultLLMMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", llm.MaxAttempts, DefaultLLMMaxAttempts)
	}
	if llm.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", llm.APIKey)
	}
}

func TestLLMYAMLOverrides(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
llm:
  provider: opencode
  base_url: https://example.test/openai/v1
  model: opencode-go/glm-5.3-flash
  fallback_models: ["deepseek-v4.1-flash"]
  timeout: 30s
  max_attempts: 2
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.LLM.BaseURL, "https://example.test/openai/v1"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.LLM.Model, "glm-5.3-flash"; got != want {
		t.Errorf("Model = %q, want normalized %q", got, want)
	}
	if got, want := cfg.LLM.FallbackModels, []string{"deepseek-v4.1-flash"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("FallbackModels = %v, want %v", got, want)
	}
	if got, want := cfg.LLM.Timeout, 30*time.Second; got != want {
		t.Errorf("Timeout = %s, want %s", got, want)
	}
	if got, want := cfg.LLM.MaxAttempts, 2; got != want {
		t.Errorf("MaxAttempts = %d, want %d", got, want)
	}
}

func TestLLMYAMLAPIKeyRejected(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
llm:
  api_key: should-not-load
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted llm.api_key from YAML")
	}
}

func TestLLMEnvEmptyIsUnset(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvOpenCodeModel, "")
	cfg, err := LoadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.LLM.Model, DefaultLLMModel; got != want {
		t.Errorf("Model = %q, want YAML %q", got, want)
	}
}

func TestLLMValidateWithoutKey(t *testing.T) {
	cfg := DefaultLLMConfig()
	cfg.APIKey = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate without key = %v, want nil", err)
	}
	if err := cfg.ValidateLive(); err == nil {
		t.Fatal("ValidateLive accepted an empty key")
	} else if !strings.Contains(err.Error(), EnvOpenCodeAPIKey) {
		t.Errorf("ValidateLive error = %v, want it to name %s", err, EnvOpenCodeAPIKey)
	}
}

func TestLLMValidateLiveWithKey(t *testing.T) {
	cfg := DefaultLLMConfig()
	cfg.APIKey = "synthetic-key"
	if err := cfg.ValidateLive(); err != nil {
		t.Fatalf("ValidateLive = %v, want nil", err)
	}
}

func TestLLMValidateStructuralErrors(t *testing.T) {
	tests := []struct {
		name string
		edit func(*LLMConfig)
		want string
	}{
		{"provider", func(c *LLMConfig) { c.Provider = "anthropic" }, "provider"},
		{"base scheme", func(c *LLMConfig) { c.BaseURL = "http://example.test/v1" }, "base_url"},
		{"base relative", func(c *LLMConfig) { c.BaseURL = "/v1" }, "base_url"},
		{"model empty", func(c *LLMConfig) { c.Model = "  " }, "model"},
		{"model invalid", func(c *LLMConfig) { c.Model = "no spaces allowed" }, "model"},
		{"timeout low", func(c *LLMConfig) { c.Timeout = 999 * time.Millisecond }, "timeout"},
		{"timeout high", func(c *LLMConfig) { c.Timeout = 11 * time.Minute }, "timeout"},
		{"attempts low", func(c *LLMConfig) { c.MaxAttempts = 0 }, "max_attempts"},
		{"attempts high", func(c *LLMConfig) { c.MaxAttempts = 6 }, "max_attempts"},
		{"fallback empty", func(c *LLMConfig) { c.FallbackModels = []string{" "} }, "fallback"},
		{"fallback duplicate", func(c *LLMConfig) { c.FallbackModels = []string{"glm-5.3-flash", "opencode-go/glm-5.3-flash"} }, "duplicated"},
		{"fallback primary", func(c *LLMConfig) { c.FallbackModels = []string{DefaultLLMModel} }, "equals"},
		{"fallback too many", func(c *LLMConfig) { c.FallbackModels = []string{"a", "b", "c", "d"} }, "maximum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultLLMConfig()
			tt.edit(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", cfg)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLLMValidationRedactsSecretShapedValues(t *testing.T) {
	tests := []struct {
		name      string
		edit      func(*LLMConfig)
		forbidden string
	}{
		{
			name:      "api key model",
			edit:      func(c *LLMConfig) { c.Model = "oc_sk_live_deadbeef extra" },
			forbidden: "oc_sk_live_deadbeef",
		},
		{
			name:      "session model",
			edit:      func(c *LLMConfig) { c.Model = "ses_live_deadbeef extra" },
			forbidden: "ses_live_deadbeef",
		},
		{
			name:      "bearer model",
			edit:      func(c *LLMConfig) { c.Model = "Bearer abc.def-123" },
			forbidden: "abc.def-123",
		},
		{
			name:      "api key fallback",
			edit:      func(c *LLMConfig) { c.FallbackModels = []string{"oc_sk_live_deadbeef extra"} },
			forbidden: "oc_sk_live_deadbeef",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultLLMConfig()
			tt.edit(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", cfg)
			}
			if strings.Contains(err.Error(), tt.forbidden) {
				t.Fatalf("Validate error leaked a secret-shaped value: %v", err)
			}
		})
	}
}

func TestConfigValidateDoesNotRequireLLMKey(t *testing.T) {
	clearEnv(t)
	cfg := defaults()
	cfg.Database.URL = "postgres://recon:recon@localhost:5432/recon?sslmode=disable"
	cfg.LLM.APIKey = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Config.Validate without LLM key = %v, want nil", err)
	}
}

func TestEnvExampleHasNoAnthropicKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ANTHROPIC_API_KEY") {
		t.Fatal(".env.example still lists ANTHROPIC_API_KEY")
	}
	for _, name := range []string{EnvOpenCodeAPIKey, EnvOpenCodeBaseURL, EnvOpenCodeModel} {
		if !strings.Contains(string(data), name+"=") {
			t.Errorf(".env.example is missing %s=", name)
		}
	}
}

func TestValidYAMLHasNoSecret(t *testing.T) {
	data, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(data))
	for _, forbidden := range []string{"api_key", "secret", "token", "oc_sk_"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("valid.yaml contains %q", forbidden)
		}
	}
}
