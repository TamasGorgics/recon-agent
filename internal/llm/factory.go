package llm

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

// Options supplies process-boundary dependencies to NewClient. Zero values use
// production defaults.
type Options struct {
	HTTPClient *http.Client
	Clock      clock.Clock
	Logger     *slog.Logger
	Sleeper    Sleeper
	Jitter     func(max int64) int64
	Metrics    Metrics
	Registry   *Registry
	Breakers   *BreakerSet
}

// NewClient builds the production client stack: Resilient over the OpenCode
// adapter. It validates the fallback allowlist against the pricing registry and
// never prints the API key.
func NewClient(cfg config.LLMConfig, opts Options) (Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("%w: OPENCODE_API_KEY is required for live LLM calls", ErrNotConfigured)
	}
	if err := cfg.ValidateLive(); err != nil {
		return nil, err
	}

	registry := opts.Registry
	if registry == nil {
		registry = DefaultRegistry()
	}
	primary, err := registry.Get(cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("llm: primary model: %w", err)
	}
	if err := registry.ValidateFallback(primary.ID, cfg.FallbackModels); err != nil {
		return nil, err
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}
	timeSource := opts.Clock
	if timeSource == nil {
		timeSource = clock.Real{}
	}
	breakers := opts.Breakers
	if breakers == nil {
		breakers = NewBreakerSet(BreakerConfig{}, timeSource, opts.Metrics)
	}

	adapter, err := NewOpenCode(OpenCodeOptions{
		BaseURL:    cfg.BaseURL,
		APIKey:     cfg.APIKey,
		HTTPClient: httpClient,
		Clock:      timeSource,
		Logger:     opts.Logger,
		Registry:   registry,
	})
	if err != nil {
		return nil, err
	}

	return NewResilient(adapter, ResilientConfig{
		Provider:    ProviderOpenCodeGo,
		MaxAttempts: cfg.MaxAttempts,
		Jitter:      opts.Jitter,
		Sleeper:     opts.Sleeper,
		Clock:       timeSource,
		Breakers:    breakers,
		Fallbacks:   cfg.FallbackModels,
		Registry:    registry,
		Metrics:     opts.Metrics,
	})
}
