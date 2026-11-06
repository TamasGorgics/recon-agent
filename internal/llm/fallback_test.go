package llm

import (
	"context"
	"testing"
	"time"
)

func fallbackHarness(t *testing.T, steps []ScriptedStep, mutate func(*ResilientConfig)) *resilientHarness {
	t.Helper()
	return newResilienceHarness(t, steps, func(cfg *ResilientConfig) {
		cfg.Fallbacks = []string{"allowed-model"}
		if mutate != nil {
			mutate(cfg)
		}
	})
}

func TestResilientFallbackAfterRetryableExhaustion(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindServer, true)},
		{Response: Response{ActualModel: "allowed-model", Message: Message{Role: RoleAssistant, Content: "fallback ok"}}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 2
	})

	resp, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if harness.scripted.Calls() != 3 {
		t.Fatalf("inner calls = %d, want 3", harness.scripted.Calls())
	}
	requests := harness.scripted.Requests()
	wantModels := []string{"primary-model", "primary-model", "allowed-model"}
	for i, want := range wantModels {
		if requests[i].Model != want {
			t.Errorf("request %d model = %q, want %q", i, requests[i].Model, want)
		}
	}
	if resp.RequestedModel != "primary-model" {
		t.Errorf("RequestedModel = %q, want primary-model", resp.RequestedModel)
	}
	if resp.ActualModel != "allowed-model" {
		t.Errorf("ActualModel = %q, want allowed-model", resp.ActualModel)
	}
	if !resp.FallbackUsed {
		t.Error("FallbackUsed = false, want true")
	}
	if resp.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", resp.Attempts)
	}
	if got := len(harness.sleeper.Durations()); got != 1 {
		t.Errorf("sleeps = %d, want 1 (only between primary attempts)", got)
	}
	if len(harness.metrics.fallbacks) != 1 {
		t.Fatalf("OnFallback calls = %d, want 1", len(harness.metrics.fallbacks))
	}
	fallbackEvent := harness.metrics.fallbacks[0]
	if fallbackEvent.Model != "primary-model" || fallbackEvent.ToModel != "allowed-model" {
		t.Errorf("OnFallback = %s -> %s, want primary-model -> allowed-model", fallbackEvent.Model, fallbackEvent.ToModel)
	}
	if fallbackEvent.Err == nil || fallbackEvent.Err.Kind != KindServer {
		t.Errorf("OnFallback err = %+v, want server error", fallbackEvent.Err)
	}
}

func TestResilientFallbackWhenPrimaryBreakerOpen(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Response: Response{ActualModel: "allowed-model"}},
	}, nil)
	for i := 0; i < 5; i++ {
		harness.breakers.Failure(ProviderOpenCodeGo, "primary-model")
	}

	resp, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if harness.scripted.Calls() != 1 {
		t.Fatalf("inner calls = %d, want 1 (primary skipped)", harness.scripted.Calls())
	}
	if resp.FallbackUsed != true || resp.Attempts != 1 {
		t.Fatalf("fallback/attempts = %v/%d, want true/1", resp.FallbackUsed, resp.Attempts)
	}
	if len(harness.metrics.rejections) != 1 {
		t.Fatalf("OnReject calls = %d, want 1", len(harness.metrics.rejections))
	}
	if len(harness.metrics.fallbacks) != 1 {
		t.Fatalf("OnFallback calls = %d, want 1", len(harness.metrics.fallbacks))
	}
	if harness.metrics.fallbacks[0].Err == nil || harness.metrics.fallbacks[0].Err.Kind != KindCircuitOpen {
		t.Fatalf("fallback error = %+v, want circuit_open", harness.metrics.fallbacks[0].Err)
	}
}

func TestResilientNoFallbackOnFatalError(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Err: resilienceError(KindAuth, false)},
	}, nil)

	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindAuth {
		t.Fatalf("error = %v, want auth", err)
	}
	if harness.scripted.Calls() != 1 {
		t.Fatalf("inner calls = %d, want 1", harness.scripted.Calls())
	}
	if len(harness.metrics.fallbacks) != 0 {
		t.Fatal("fallback ran for a fatal auth error")
	}
}

func TestResilientNoFallbackOnBudgetExhaustion(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Err: resilienceError(KindBudget, false)},
	}, nil)

	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if !IsBudgetExhausted(err) {
		t.Fatalf("IsBudgetExhausted(%v) = false", err)
	}
	if harness.scripted.Calls() != 1 {
		t.Fatalf("inner calls = %d, want 1", harness.scripted.Calls())
	}
	if len(harness.metrics.fallbacks) != 0 {
		t.Fatal("fallback ran for budget exhaustion")
	}
}

func TestResilientFatalFallbackStopsChain(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindModelNotFound, false)},
		{Response: Response{ActualModel: "third-allowed-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 1
		cfg.Fallbacks = []string{"allowed-model", "third-allowed-model"}
	})

	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindModelNotFound {
		t.Fatalf("error = %v, want model_not_found", err)
	}
	if harness.scripted.Calls() != 2 {
		t.Fatalf("inner calls = %d, want 2 (chain stopped after the fatal fallback)", harness.scripted.Calls())
	}
	requests := harness.scripted.Requests()
	if requests[1].Model != "allowed-model" {
		t.Fatalf("second request model = %q, want allowed-model", requests[1].Model)
	}
	if pe.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", pe.Attempts)
	}
}

func TestResilientAllModelsFailReturnsLastErrorWithTotalAttempts(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := newResilienceHarness(t, []ScriptedStep{
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindUnavailable, true)},
		{Err: resilienceError(KindUnavailable, true)},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 2
		cfg.Fallbacks = []string{"allowed-model"}
	})

	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindUnavailable {
		t.Fatalf("error = %v, want the last unavailable error", err)
	}
	if pe.Attempts != 4 {
		t.Fatalf("Attempts = %d, want 4", pe.Attempts)
	}
	if harness.scripted.Calls() != 4 {
		t.Fatalf("inner calls = %d, want 4", harness.scripted.Calls())
	}
}

func TestResilientFallbackObservesActualModelFromProvider(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Err: resilienceError(KindServer, true)},
		{Err: resilienceError(KindServer, true)},
		{Response: Response{ActualModel: "allowed-model-alias"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 2
	})

	resp, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.RequestedModel != "primary-model" || resp.ActualModel != "allowed-model-alias" {
		t.Fatalf("requested/actual = %q/%q, want primary-model/allowed-model-alias", resp.RequestedModel, resp.ActualModel)
	}
	if !resp.FallbackUsed {
		t.Fatal("FallbackUsed = false")
	}
}

func TestResilientFallbackBreakerIsPerModel(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Response: Response{ActualModel: "allowed-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 1
	})

	// Open the primary breaker; the fallback must still be callable.
	for i := 0; i < 5; i++ {
		harness.breakers.Failure(ProviderOpenCodeGo, "primary-model")
	}
	if _, err := harness.resilient.Complete(context.Background(), resilienceRequest()); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	if got := harness.breakers.State(ProviderOpenCodeGo, "allowed-model"); got != BreakerClosed {
		t.Fatalf("fallback breaker = %q, want closed", got)
	}
}

func TestResilientFallbackNoSleepBetweenModels(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Err: resilienceError(KindServer, true)},
		{Response: Response{ActualModel: "allowed-model"}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 1
	})
	start := harness.clock.Now()
	if _, err := harness.resilient.Complete(context.Background(), resilienceRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := harness.sleeper.Durations(); len(got) != 0 {
		t.Fatalf("sleeps = %v, want none between models", got)
	}
	if !harness.clock.Now().Equal(start) {
		t.Fatalf("fake clock advanced from %s to %s without a sleep", start, harness.clock.Now())
	}
}

func TestResilientFallbackResponseAttemptsAndProvider(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Err: resilienceError(KindOverloaded, true)},
		{Err: resilienceError(KindOverloaded, true)},
		{Response: Response{ActualModel: "allowed-model", Provider: ""}},
	}, func(cfg *ResilientConfig) {
		cfg.MaxAttempts = 2
	})
	resp, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Provider != ProviderOpenCodeGo {
		t.Fatalf("Provider = %q, want %q", resp.Provider, ProviderOpenCodeGo)
	}
	if resp.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", resp.Attempts)
	}
	if !resp.FallbackUsed {
		t.Fatal("FallbackUsed = false")
	}
}

func TestResilientFallbackAfterAllBreakersOpen(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	harness := fallbackHarness(t, []ScriptedStep{
		{Response: Response{ActualModel: "allowed-model"}},
	}, nil)
	for i := 0; i < 5; i++ {
		harness.breakers.Failure(ProviderOpenCodeGo, "primary-model")
		harness.breakers.Failure(ProviderOpenCodeGo, "allowed-model")
	}
	_, err := harness.resilient.Complete(context.Background(), resilienceRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindCircuitOpen {
		t.Fatalf("error = %v, want circuit_open", err)
	}
	if harness.scripted.Calls() != 0 {
		t.Fatalf("inner calls = %d, want 0", harness.scripted.Calls())
	}
	if pe.RetryAfter <= 0 || pe.RetryAfter > 30*time.Second {
		t.Fatalf("RetryAfter = %s, want (0,30s]", pe.RetryAfter)
	}
}
