package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const resilienceTestPricing = `schema_version: 1
source_url: https://example.invalid/models
fetched_at: "2026-11-06"
models:
  - provider: opencode-go
    id: primary-model
    selector: opencode-go/primary-model
    context_limit: 100000
    output_limit: 10000
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: true
      structured_output: true
      cache: true
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: false
  - provider: opencode-go
    id: allowed-model
    selector: opencode-go/allowed-model
    context_limit: 100000
    output_limit: 10000
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: true
      structured_output: true
      cache: true
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: true
  - provider: opencode-go
    id: second-allowed-model
    selector: opencode-go/second-allowed-model
    context_limit: 100000
    output_limit: 10000
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: true
      structured_output: true
      cache: true
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: true
  - provider: opencode-go
    id: third-allowed-model
    selector: opencode-go/third-allowed-model
    context_limit: 100000
    output_limit: 10000
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: true
      structured_output: true
      cache: true
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: true
  - provider: opencode-go
    id: fourth-allowed-model
    selector: opencode-go/fourth-allowed-model
    context_limit: 100000
    output_limit: 10000
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: true
      structured_output: true
      cache: true
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: true
  - provider: opencode-go
    id: disallowed-model
    selector: opencode-go/disallowed-model
    context_limit: 100000
    output_limit: 10000
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: true
      structured_output: true
      cache: true
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: false
`

func resilienceTestRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := LoadRegistry([]byte(resilienceTestPricing))
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	return registry
}

func resilienceRequest() Request {
	return Request{
		Model:    "primary-model",
		System:   "You are a test.",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}
}

func resilienceError(kind ErrorKind, retryable bool) *ProviderError {
	return &ProviderError{
		Provider:  ProviderOpenCodeGo,
		Model:     "primary-model",
		Kind:      kind,
		Retryable: retryable,
		Message:   string(kind),
		Attempts:  1,
	}
}

type resilientHarness struct {
	resilient *Resilient
	scripted  *Scripted
	sleeper   *FakeSleeper
	clock     *clock.Fake
	metrics   *fakeMetrics
	breakers  *BreakerSet
}

func newResilienceHarness(t *testing.T, steps []ScriptedStep, mutate func(*ResilientConfig)) *resilientHarness {
	t.Helper()
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	scripted := NewScripted(steps...)
	sleeper := NewFakeSleeper(fakeClock)
	metrics := &fakeMetrics{}
	cfg := ResilientConfig{
		Provider:    ProviderOpenCodeGo,
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    10 * time.Second,
		Factor:      2,
		Jitter:      func(max int64) int64 { return max },
		Sleeper:     sleeper,
		Clock:       fakeClock,
		Registry:    resilienceTestRegistry(t),
		Metrics:     metrics,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	breakers := cfg.Breakers
	if breakers == nil {
		breakers = NewBreakerSet(BreakerConfig{}, fakeClock, metrics)
		cfg.Breakers = breakers
	}
	resilient, err := NewResilient(scripted, cfg)
	if err != nil {
		t.Fatalf("NewResilient: %v", err)
	}
	return &resilientHarness{
		resilient: resilient,
		scripted:  scripted,
		sleeper:   sleeper,
		clock:     fakeClock,
		metrics:   metrics,
		breakers:  breakers,
	}
}

func TestResilientRetriesRateLimitThenSucceeds(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: resilienceError(KindRateLimit, true)},
		{Response: Response{ActualModel: "primary-model", Message: Message{Role: RoleAssistant, Content: "ok"}}},
	}, nil)

	resp, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", resp.Attempts)
	}
	if harness.scripted.Calls() != 2 {
		t.Fatalf("inner calls = %d, want 2", harness.scripted.Calls())
	}
	durations := harness.sleeper.Durations()
	if len(durations) != 1 || durations[0] != 100*time.Millisecond {
		t.Fatalf("sleep durations = %v, want [100ms]", durations)
	}
	if len(harness.metrics.retries) != 1 {
		t.Fatalf("OnRetry calls = %d, want 1", len(harness.metrics.retries))
	}
	retry := harness.metrics.retries[0]
	if retry.Attempt != 1 || retry.Kind != KindRateLimit || retry.Delay != 100*time.Millisecond {
		t.Fatalf("OnRetry = %+v, want attempt 1 rate_limit 100ms", retry)
	}
	if len(harness.metrics.failures) != 1 || len(harness.metrics.successes) != 1 {
		t.Fatalf("failure/success metrics = %d/%d, want 1/1", len(harness.metrics.failures), len(harness.metrics.successes))
	}
	if got := harness.breakers.State(ProviderOpenCodeGo, "primary-model"); got != BreakerClosed {
		t.Fatalf("breaker state = %q, want closed", got)
	}
}

func TestResilientExhaustsRetriesWithoutSleepingAfterFinalAttempt(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindServer, true)},
	}, nil)

	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindServer {
		t.Fatalf("error = %v, want server ProviderError", err)
	}
	if pe.Attempts != 3 {
		t.Fatalf("error Attempts = %d, want 3", pe.Attempts)
	}
	if harness.scripted.Calls() != 3 {
		t.Fatalf("inner calls = %d, want exactly 3", harness.scripted.Calls())
	}
	if got := len(harness.sleeper.Durations()); got != 2 {
		t.Fatalf("sleeps = %d, want 2 (never after the final attempt)", got)
	}
	if got := harness.breakers.State(ProviderOpenCodeGo, "primary-model"); got != BreakerClosed {
		t.Fatalf("breaker state after one exhausted call = %q, want closed (failure count 1)", got)
	}
}

func TestResilientFatalErrorsAreNotRetried(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	tests := []struct {
		name string
		err  *ProviderError
		kind ErrorKind
	}{
		{"auth", resilienceError(KindAuth, false), KindAuth},
		{"auth mislabeled retryable", resilienceError(KindAuth, true), KindAuth},
		{"model not found", resilienceError(KindModelNotFound, false), KindModelNotFound},
		{"invalid request", resilienceError(KindInvalidRequest, false), KindInvalidRequest},
		{"context length", resilienceError(KindContextLength, false), KindContextLength},
		{"account restricted", resilienceError(KindAccountRestricted, false), KindAccountRestricted},
		{"invalid response", resilienceError(KindInvalidResponse, false), KindInvalidResponse},
		{"unknown", resilienceError(KindUnknown, false), KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			harness := newResilienceHarness(t, []ScriptedStep{{Err: tt.err}}, nil)
			_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
			pe, ok := AsProviderError(err)
			if !ok || pe.Kind != tt.kind {
				t.Fatalf("error = %v, want kind %q", err, tt.kind)
			}
			if harness.scripted.Calls() != 1 {
				t.Fatalf("inner calls = %d, want 1", harness.scripted.Calls())
			}
			if got := len(harness.sleeper.Durations()); got != 0 {
				t.Fatalf("sleeps = %d, want 0", got)
			}
		})
	}
}

func TestResilientBudgetExhaustionIsNotRetried(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	budget := resilienceError(KindBudget, false)
	budget.RetryAfter = time.Hour
	harness := newResilienceHarness(t, []ScriptedStep{{Err: budget}}, nil)

	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if !IsBudgetExhausted(err) {
		t.Fatalf("IsBudgetExhausted(%v) = false", err)
	}
	if harness.scripted.Calls() != 1 {
		t.Fatalf("inner calls = %d, want 1", harness.scripted.Calls())
	}
	if got := len(harness.sleeper.Durations()); got != 0 {
		t.Fatalf("sleeps = %d, want 0 for budget exhaustion", got)
	}
}

func TestResilientContextCanceledBeforeCall(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := newResilienceHarness(t, []ScriptedStep{
		{Response: Response{ActualModel: "primary-model"}},
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := harness.resilient.Complete(ctx, resilienceRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindCanceled || pe.Retryable {
		t.Fatalf("error = %+v, want canceled non-retryable ProviderError", pe)
	}
	if harness.scripted.Calls() != 0 {
		t.Fatalf("inner calls = %d, want 0", harness.scripted.Calls())
	}
}

type cancelOnSleep struct {
	cancel func()
	calls  int
}

func (s *cancelOnSleep) Sleep(ctx context.Context, _ time.Duration) error {
	s.calls++
	s.cancel()
	return ctx.Err()
}

func TestResilientContextCanceledDuringSleep(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeper := &cancelOnSleep{cancel: cancel}
	scripted := NewScripted(ScriptedStep{Err: resilienceError(KindServer, true)})
	resilient, err := NewResilient(scripted, ResilientConfig{
		Provider:    ProviderOpenCodeGo,
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Second,
		Factor:      2,
		Jitter:      func(max int64) int64 { return max },
		Sleeper:     sleeper,
		Clock:       fakeClock,
		Registry:    resilienceTestRegistry(t),
	})
	if err != nil {
		t.Fatalf("NewResilient: %v", err)
	}

	_, err = resilient.Complete(ctx, resilienceRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindCanceled {
		t.Fatalf("error = %+v, want canceled ProviderError", pe)
	}
	if scripted.Calls() != 1 || sleeper.calls != 1 {
		t.Fatalf("calls/sleeps = %d/%d, want 1/1", scripted.Calls(), sleeper.calls)
	}
}

func TestResilientFullJitterDelays(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	var jitterCaps []int64
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindServer, true)},
		{Response: Response{ActualModel: "primary-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 4
		cfg.BaseDelay = 100 * time.Millisecond
		cfg.MaxDelay = 10 * time.Second
		cfg.Factor = 2
		cfg.Jitter = func(max int64) int64 {
			jitterCaps = append(jitterCaps, max)
			return max / 2
		}
	})

	if _, err := harness.resilient.Complete(context.Background(), resilienceRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	wantCaps := []int64{
		int64(100 * time.Millisecond),
		int64(200 * time.Millisecond),
		int64(400 * time.Millisecond),
	}
	if len(jitterCaps) != len(wantCaps) {
		t.Fatalf("jitter caps = %v, want %v", jitterCaps, wantCaps)
	}
	for i := range wantCaps {
		if jitterCaps[i] != wantCaps[i] {
			t.Errorf("jitter cap %d = %d, want %d", i, jitterCaps[i], wantCaps[i])
		}
	}
	wantDelays := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}
	durations := harness.sleeper.Durations()
	if len(durations) != len(wantDelays) {
		t.Fatalf("sleep durations = %v, want %v", durations, wantDelays)
	}
	for i := range wantDelays {
		if durations[i] != wantDelays[i] {
			t.Errorf("sleep %d = %s, want %s", i, durations[i], wantDelays[i])
		}
	}
}

func TestResilientBackoffCap(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	var jitterCaps []int64
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindServer, true)},
		{Response: Response{ActualModel: "primary-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 3
		cfg.BaseDelay = 100 * time.Millisecond
		cfg.MaxDelay = 500 * time.Millisecond
		cfg.Factor = 10
		cfg.Jitter = func(max int64) int64 {
			jitterCaps = append(jitterCaps, max)
			return max
		}
	})

	if _, err := harness.resilient.Complete(context.Background(), resilienceRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(jitterCaps) != 2 {
		t.Fatalf("jitter caps = %v, want 2 entries", jitterCaps)
	}
	if jitterCaps[0] != int64(100*time.Millisecond) || jitterCaps[1] != int64(500*time.Millisecond) {
		t.Fatalf("jitter caps = %v, want [100ms 500ms]", jitterCaps)
	}
}

func TestResilientRetryAfterIsClamped(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	first := resilienceError(KindRateLimit, true)
	first.RetryAfter = 2 * time.Second
	second := resilienceError(KindRateLimit, true)
	third := resilienceError(KindRateLimit, true)
	third.RetryAfter = 100 * time.Second
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: first},
		{Err: second},
		{Err: third},
		{Response: Response{ActualModel: "primary-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 4
		cfg.BaseDelay = 100 * time.Millisecond
		cfg.MaxDelay = 30 * time.Second
		cfg.Jitter = func(int64) int64 { return 0 }
	})

	if _, err := harness.resilient.Complete(context.Background(), resilienceRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	durations := harness.sleeper.Durations()
	want := []time.Duration{2 * time.Second, 0, 30 * time.Second}
	if len(durations) != len(want) {
		t.Fatalf("sleep durations = %v, want %v", durations, want)
	}
	for i := range want {
		if durations[i] != want[i] {
			t.Errorf("sleep %d = %s, want %s", i, durations[i], want[i])
		}
	}
}

func TestResilientRetryAfterLongerThanDeadlineReturnsError(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	limited := resilienceError(KindRateLimit, true)
	limited.RetryAfter = time.Minute
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: limited},
		{Response: Response{ActualModel: "primary-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxDelay = 30 * time.Second
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Align the injected clock with the real deadline so the remaining time is
	// deterministic and close to ten seconds.
	harness.clock.Set(time.Now())

	_, err := harness.resilient.Complete(ctx, resilienceRequest())
	if err == nil {
		t.Fatal("Complete succeeded despite a retry delay beyond the deadline")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline error was returned instead of the provider error")
	}
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindRateLimit {
		t.Fatalf("error = %+v, want rate_limit", pe)
	}
	if pe.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", pe.Attempts)
	}
	if got := len(harness.sleeper.Durations()); got != 0 {
		t.Fatalf("sleeps = %d, want 0 when Retry-After exceeds the deadline", got)
	}
}

func TestResilientDeadlineLimitedRetryDoesNotFallBack(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	limited := resilienceError(KindRateLimit, true)
	limited.RetryAfter = time.Minute
	harness := fallbackHarness(t, []ScriptedStep{
		{Err: limited},
		{Response: Response{ActualModel: "allowed-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxDelay = 30 * time.Second
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	harness.clock.Set(time.Now())

	_, err := harness.resilient.Complete(ctx, resilienceRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindRateLimit {
		t.Fatalf("error = %v, want rate_limit", err)
	}
	if harness.scripted.Calls() != 1 {
		t.Fatalf("inner calls = %d, want 1", harness.scripted.Calls())
	}
	if len(harness.metrics.fallbacks) != 0 {
		t.Fatal("fallback ran even though retries stopped at the context deadline")
	}
}

func TestResilientRetryAfterZeroFallsBackToJitter(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: resilienceError(KindRateLimit, true)},
		{Response: Response{ActualModel: "primary-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.Jitter = func(max int64) int64 { return max }
	})
	if _, err := harness.resilient.Complete(context.Background(), resilienceRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	durations := harness.sleeper.Durations()
	if len(durations) != 1 || durations[0] != 100*time.Millisecond {
		t.Fatalf("sleep durations = %v, want [100ms] from jitter", durations)
	}
}

func TestResilientSanitizesInnerErrors(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	leaky := &ProviderError{
		Provider:  ProviderOpenCodeGo,
		Model:     "primary-model",
		Kind:      KindServer,
		Retryable: true,
		Message:   "boom oc_sk_synthetic_secret",
		Body:      []byte(`{"key":"oc_sk_synthetic_secret"}`),
	}
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: leaky},
		{Err: leaky},
		{Err: leaky},
	}, nil)

	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if err == nil {
		t.Fatal("Complete succeeded")
	}
	if strings.Contains(err.Error(), "oc_sk_synthetic_secret") {
		t.Fatal("final error leaked a synthetic API key")
	}
	pe, ok := AsProviderError(err)
	if !ok {
		t.Fatalf("error is not a ProviderError: %v", err)
	}
	if strings.Contains(string(pe.Body), "oc_sk_synthetic_secret") {
		t.Fatal("final error body leaked a synthetic API key")
	}
	if pe.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", pe.Attempts)
	}
}

func TestNewResilientValidation(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	scripted := NewScripted()
	registry := resilienceTestRegistry(t)
	tests := []struct {
		name string
		cfg  ResilientConfig
	}{
		{"max attempts too high", ResilientConfig{MaxAttempts: 6, Registry: registry}},
		{"max attempts negative", ResilientConfig{MaxAttempts: -1, Registry: registry}},
		{"negative base delay", ResilientConfig{BaseDelay: -time.Second, Registry: registry}},
		{"negative max delay", ResilientConfig{MaxDelay: -time.Second, Registry: registry}},
		{"negative factor", ResilientConfig{Factor: -2, Registry: registry}},
		{"fallback too long", ResilientConfig{Fallbacks: []string{"allowed-model", "second-allowed-model", "third-allowed-model", "fourth-allowed-model"}, Registry: registry}},
		{"fallback unknown", ResilientConfig{Fallbacks: []string{"missing-model"}, Registry: registry}},
		{"fallback disallowed", ResilientConfig{Fallbacks: []string{"disallowed-model"}, Registry: registry}},
		{"fallback duplicate", ResilientConfig{Fallbacks: []string{"allowed-model", "allowed-model"}, Registry: registry}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewResilient(scripted, tt.cfg); err == nil {
				t.Fatal("NewResilient accepted an invalid config")
			}
		})
	}
	if _, err := NewResilient(nil, ResilientConfig{Registry: registry}); err == nil {
		t.Fatal("NewResilient accepted a nil inner client")
	}
}

func TestResilientBreakerOpensAfterExhaustedFailures(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	steps := make([]ScriptedStep, 5)
	for i := range steps {
		steps[i] = ScriptedStep{Err: resilienceError(KindServer, true)}
	}
	harness := newResilienceHarness(t, steps, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 1
	})

	for i := 0; i < 5; i++ {
		if _, err := harness.resilient.Complete(context.Background(), resilienceRequest()); err == nil {
			t.Fatalf("Complete #%d succeeded", i+1)
		}
	}
	if got := harness.breakers.State(ProviderOpenCodeGo, "primary-model"); got != BreakerOpen {
		t.Fatalf("breaker state = %q, want open after five exhausted calls", got)
	}

	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindCircuitOpen {
		t.Fatalf("error = %v, want circuit_open", err)
	}
	if pe.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %s, want 30s", pe.RetryAfter)
	}
	if pe.Attempts != 0 {
		t.Fatalf("Attempts = %d, want 0 for a fast rejection", pe.Attempts)
	}
	if harness.scripted.Calls() != 5 {
		t.Fatalf("inner calls = %d, want 5 (no call while open)", harness.scripted.Calls())
	}
	if len(harness.metrics.rejections) != 1 {
		t.Fatalf("OnReject calls = %d, want 1", len(harness.metrics.rejections))
	}
}

func TestResilientRateLimitDoesNotOpenBreaker(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	steps := make([]ScriptedStep, 5)
	for i := range steps {
		steps[i] = ScriptedStep{Err: resilienceError(KindRateLimit, true)}
	}
	harness := newResilienceHarness(t, steps, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 1
	})
	for i := 0; i < 5; i++ {
		_, _ = harness.resilient.Complete(context.Background(), resilienceRequest())
	}
	if got := harness.breakers.State(ProviderOpenCodeGo, "primary-model"); got != BreakerClosed {
		t.Fatalf("breaker state = %q, want closed (rate limits do not count)", got)
	}
}

func TestResilientNonCountedProbeFailureReopensBreaker(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	nonCounted := []ErrorKind{
		KindAuth, KindInvalidRequest, KindModelNotFound, KindContextLength,
		KindBudget, KindAccountRestricted, KindRateLimit, KindCassetteMiss,
		KindCanceled, KindCircuitOpen, KindInvalidResponse, KindUnknown,
	}
	for _, kind := range nonCounted {
		t.Run(string(kind), func(t *testing.T) {
			harness := newResilienceHarness(t, []ScriptedStep{
				{Err: resilienceError(kind, false)},
				{Response: Response{ActualModel: "primary-model", Message: Message{Role: RoleAssistant, Content: "recovered"}}},
			}, nil)
			const model = "primary-model"
			for i := 0; i < 5; i++ {
				harness.breakers.Failure(ProviderOpenCodeGo, model)
			}
			harness.clock.Advance(30 * time.Second)

			_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
			if pe, ok := AsProviderError(err); !ok || pe.Kind != kind {
				t.Fatalf("probe error = %v, want kind %q", err, kind)
			}
			if got := harness.breakers.State(ProviderOpenCodeGo, model); got != BreakerOpen {
				t.Fatalf("state after non-counted probe failure = %q, want open", got)
			}

			_, err = harness.resilient.Complete(context.Background(), resilienceRequest())
			pe, ok := AsProviderError(err)
			if !ok || pe.Kind != KindCircuitOpen {
				t.Fatalf("call while reopened = %v, want circuit_open", err)
			}
			if pe.RetryAfter != 30*time.Second {
				t.Fatalf("RetryAfter = %s, want 30s", pe.RetryAfter)
			}
			if harness.scripted.Calls() != 1 {
				t.Fatalf("inner calls while open = %d, want 1", harness.scripted.Calls())
			}

			harness.clock.Advance(30 * time.Second)
			resp, err := harness.resilient.Complete(context.Background(), resilienceRequest())
			if err != nil {
				t.Fatalf("recovery probe: %v", err)
			}
			if resp.Message.Content != "recovered" {
				t.Fatalf("recovery content = %q, want recovered", resp.Message.Content)
			}
			if got := harness.breakers.State(ProviderOpenCodeGo, model); got != BreakerClosed {
				t.Fatalf("state after recovery probe = %q, want closed", got)
			}
			if harness.scripted.Calls() != 2 {
				t.Fatalf("inner calls = %d, want 2", harness.scripted.Calls())
			}
		})
	}
}

func TestResilientProbeCancellationReopensBreaker(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness := newResilienceHarness(t, []ScriptedStep{
		{
			Err: resilienceError(KindServer, true),
			Check: func(Request) error {
				cancel()
				return nil
			},
		},
		{Response: Response{ActualModel: "primary-model", Message: Message{Role: RoleAssistant, Content: "recovered"}}},
	}, nil)
	const model = "primary-model"
	for i := 0; i < 5; i++ {
		harness.breakers.Failure(ProviderOpenCodeGo, model)
	}
	harness.clock.Advance(30 * time.Second)

	_, err := harness.resilient.Complete(ctx, resilienceRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}
	if pe, ok := AsProviderError(err); !ok || pe.Kind != KindCanceled {
		t.Fatalf("cancellation error = %v, want kind canceled", err)
	}
	if got := harness.breakers.State(ProviderOpenCodeGo, model); got != BreakerOpen {
		t.Fatalf("state after canceled probe = %q, want open", got)
	}

	_, err = harness.resilient.Complete(context.Background(), resilienceRequest())
	if pe, ok := AsProviderError(err); !ok || pe.Kind != KindCircuitOpen {
		t.Fatalf("call while reopened = %v, want circuit_open", err)
	}
	if harness.scripted.Calls() != 1 {
		t.Fatalf("inner calls while open = %d, want 1", harness.scripted.Calls())
	}

	harness.clock.Advance(30 * time.Second)
	resp, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if err != nil {
		t.Fatalf("recovery probe: %v", err)
	}
	if resp.Message.Content != "recovered" {
		t.Fatalf("recovery content = %q, want recovered", resp.Message.Content)
	}
	if got := harness.breakers.State(ProviderOpenCodeGo, model); got != BreakerClosed {
		t.Fatalf("state after recovery probe = %q, want closed", got)
	}
}

func TestNewResilientDefaults(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	resilient, err := NewResilient(NewScripted(), ResilientConfig{})
	if err != nil {
		t.Fatalf("NewResilient: %v", err)
	}
	if resilient.maxAttempts != 3 {
		t.Errorf("maxAttempts = %d, want 3", resilient.maxAttempts)
	}
	if resilient.baseDelay != 500*time.Millisecond {
		t.Errorf("baseDelay = %s, want 500ms", resilient.baseDelay)
	}
	if resilient.maxDelay != 30*time.Second {
		t.Errorf("maxDelay = %s, want 30s", resilient.maxDelay)
	}
	if resilient.factor != 2 {
		t.Errorf("factor = %d, want 2", resilient.factor)
	}
	if resilient.provider != ProviderOpenCodeGo {
		t.Errorf("provider = %q, want %q", resilient.provider, ProviderOpenCodeGo)
	}
	if len(resilient.fallbacks) != 0 {
		t.Errorf("fallbacks = %v, want empty", resilient.fallbacks)
	}
}
