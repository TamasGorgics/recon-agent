package llm

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

func testLLMConfig() config.LLMConfig {
	cfg := config.DefaultLLMConfig()
	cfg.APIKey = "test-key-not-real"
	cfg.Provider = "opencode"
	cfg.BaseURL = "https://example.invalid/openai/v1"
	cfg.Model = "primary-model"
	cfg.Timeout = 12 * time.Second
	cfg.MaxAttempts = 3
	return cfg
}

func TestNewClientRequiresAPIKey(t *testing.T) {
	cfg := testLLMConfig()
	cfg.APIKey = ""

	_, err := NewClient(cfg, Options{Registry: resilienceTestRegistry(t)})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("error = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "OPENCODE_API_KEY") {
		t.Fatalf("error = %q, want it to name OPENCODE_API_KEY", err)
	}

	cfg.APIKey = "   "
	if _, err := NewClient(cfg, Options{Registry: resilienceTestRegistry(t)}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("whitespace key error = %v, want ErrNotConfigured", err)
	}
}

func TestNewClientRejectsUnknownPrimaryModel(t *testing.T) {
	cfg := testLLMConfig()
	cfg.Model = "missing-model"

	_, err := NewClient(cfg, Options{Registry: resilienceTestRegistry(t)})
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("error = %v, want ErrUnknownModel", err)
	}
}

func TestNewClientEnforcesFallbackAllowlist(t *testing.T) {
	registry := resilienceTestRegistry(t)

	cfg := testLLMConfig()
	cfg.FallbackModels = []string{"disallowed-model"}
	if _, err := NewClient(cfg, Options{Registry: registry}); err == nil {
		t.Fatal("NewClient accepted a disallowed fallback")
	}

	cfg.FallbackModels = []string{"missing-model"}
	if _, err := NewClient(cfg, Options{Registry: registry}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown fallback error = %v, want ErrUnknownModel", err)
	}

	cfg.FallbackModels = []string{"allowed-model"}
	client, err := NewClient(cfg, Options{Registry: registry})
	if err != nil {
		t.Fatalf("NewClient with an allowed fallback: %v", err)
	}
	resilient, ok := client.(*Resilient)
	if !ok {
		t.Fatalf("client type = %T, want *Resilient", client)
	}
	if len(resilient.fallbacks) != 1 || resilient.fallbacks[0] != "allowed-model" {
		t.Fatalf("fallbacks = %v, want [allowed-model]", resilient.fallbacks)
	}
}

func TestNewClientFallbackStructuralValidation(t *testing.T) {
	registry := resilienceTestRegistry(t)
	tests := []struct {
		name      string
		fallbacks []string
	}{
		{"equals primary", []string{"primary-model"}},
		{"duplicate", []string{"allowed-model", "allowed-model"}},
		{"too many", []string{"allowed-model", "second-allowed-model", "third-allowed-model", "fourth-allowed-model"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testLLMConfig()
			cfg.FallbackModels = tt.fallbacks
			if _, err := NewClient(cfg, Options{Registry: registry}); err == nil {
				t.Fatal("NewClient accepted an invalid fallback chain")
			}
		})
	}
}

func TestNewClientWiresConfiguration(t *testing.T) {
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	sleeper := NewFakeSleeper(fakeClock)
	metrics := &fakeMetrics{}
	cfg := testLLMConfig()
	cfg.MaxAttempts = 5
	cfg.FallbackModels = []string{"allowed-model"}
	registry := resilienceTestRegistry(t)

	client, err := NewClient(cfg, Options{
		Clock:    fakeClock,
		Sleeper:  sleeper,
		Metrics:  metrics,
		Registry: registry,
		Jitter:   func(max int64) int64 { return max },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	resilient, ok := client.(*Resilient)
	if !ok {
		t.Fatalf("client type = %T, want *Resilient", client)
	}
	if resilient.provider != ProviderOpenCodeGo {
		t.Errorf("provider = %q, want %q", resilient.provider, ProviderOpenCodeGo)
	}
	if resilient.maxAttempts != 5 {
		t.Errorf("maxAttempts = %d, want 5", resilient.maxAttempts)
	}
	if resilient.sleeper != Sleeper(sleeper) {
		t.Error("injected sleeper was not wired")
	}
	if resilient.clock != clock.Clock(fakeClock) {
		t.Error("injected clock was not wired")
	}
	if len(resilient.fallbacks) != 1 || resilient.fallbacks[0] != "allowed-model" {
		t.Errorf("fallbacks = %v, want [allowed-model]", resilient.fallbacks)
	}
	if resilient.breakers == nil {
		t.Error("breaker set was not created")
	}
	adapter, ok := resilient.inner.(*OpenCode)
	if !ok {
		t.Fatalf("inner client type = %T, want *OpenCode", resilient.inner)
	}
	if adapter.registry != registry {
		t.Error("registry was not passed to the adapter")
	}
	if adapter.httpClient == nil || adapter.httpClient.Timeout != cfg.Timeout {
		t.Errorf("HTTP timeout = %v, want %s", adapter.httpClient, cfg.Timeout)
	}
}

func TestNewClientKeepsInjectedHTTPClient(t *testing.T) {
	httpClient := &http.Client{Timeout: time.Second}
	cfg := testLLMConfig()
	cfg.FallbackModels = nil
	client, err := NewClient(cfg, Options{
		HTTPClient: httpClient,
		Registry:   resilienceTestRegistry(t),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	resilient := client.(*Resilient)
	adapter := resilient.inner.(*OpenCode)
	if adapter.httpClient != httpClient {
		t.Fatal("injected HTTP client was replaced")
	}
}

func TestNewClientErrorsDoNotPrintAPIKey(t *testing.T) {
	cfg := testLLMConfig()
	cfg.APIKey = "oc_sk_synthetic_never_print"
	cfg.FallbackModels = []string{"disallowed-model"}

	_, err := NewClient(cfg, Options{Registry: resilienceTestRegistry(t)})
	if err == nil {
		t.Fatal("NewClient accepted a disallowed fallback")
	}
	if strings.Contains(err.Error(), cfg.APIKey) {
		t.Fatalf("error leaked the API key: %v", err)
	}
}

func TestNewClientRedactsSecretShapedModels(t *testing.T) {
	tests := []struct {
		name      string
		edit      func(*config.LLMConfig)
		forbidden string
	}{
		{
			name:      "primary api key",
			edit:      func(cfg *config.LLMConfig) { cfg.Model = "oc_sk_live_deadbeef" },
			forbidden: "oc_sk_live_deadbeef",
		},
		{
			name:      "primary session",
			edit:      func(cfg *config.LLMConfig) { cfg.Model = "ses_live_deadbeef" },
			forbidden: "ses_live_deadbeef",
		},
		{
			name:      "primary bearer",
			edit:      func(cfg *config.LLMConfig) { cfg.Model = "Bearer abc.def-123" },
			forbidden: "abc.def-123",
		},
		{
			name:      "fallback api key",
			edit:      func(cfg *config.LLMConfig) { cfg.FallbackModels = []string{"oc_sk_live_deadbeef"} },
			forbidden: "oc_sk_live_deadbeef",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testLLMConfig()
			cfg.APIKey = "oc_sk_synthetic_never_print"
			tt.edit(&cfg)
			_, err := NewClient(cfg, Options{Registry: resilienceTestRegistry(t)})
			if err == nil {
				t.Fatal("NewClient accepted a secret-shaped model")
			}
			if strings.Contains(err.Error(), tt.forbidden) {
				t.Fatalf("error leaked a secret-shaped model: %v", err)
			}
		})
	}
}

func TestNewResilientRedactsSecretShapedFallback(t *testing.T) {
	inner := &countingClient{fn: func(Request) (Response, error) { return Response{}, nil }}
	_, err := NewResilient(inner, ResilientConfig{Fallbacks: []string{"oc_sk_live_deadbeef"}})
	if err == nil {
		t.Fatal("NewResilient accepted a secret-shaped fallback")
	}
	if strings.Contains(err.Error(), "oc_sk_live_deadbeef") {
		t.Fatalf("error leaked a secret-shaped fallback: %v", err)
	}
}
