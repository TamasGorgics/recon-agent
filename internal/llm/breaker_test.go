package llm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

type metricsEvent struct {
	Provider string
	Model    string
	Attempt  int
	Kind     ErrorKind
	Delay    time.Duration
	From     BreakerState
	To       BreakerState
	Response Response
	Err      *ProviderError
	ToModel  string
}

type fakeMetrics struct {
	mu          sync.Mutex
	retries     []metricsEvent
	states      []metricsEvent
	successes   []metricsEvent
	failures    []metricsEvent
	rejections  []metricsEvent
	fallbacks   []metricsEvent
	onSuccessFn func(Response)
}

func (m *fakeMetrics) OnRetry(provider, model string, attempt int, kind ErrorKind, delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries = append(m.retries, metricsEvent{Provider: provider, Model: model, Attempt: attempt, Kind: kind, Delay: delay})
}

func (m *fakeMetrics) OnBreakerState(provider, model string, from, to BreakerState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states = append(m.states, metricsEvent{Provider: provider, Model: model, From: from, To: to})
}

func (m *fakeMetrics) OnSuccess(provider, model string, resp Response) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.successes = append(m.successes, metricsEvent{Provider: provider, Model: model, Response: resp})
	if m.onSuccessFn != nil {
		m.onSuccessFn(resp)
	}
}

func (m *fakeMetrics) OnFailure(provider, model string, err *ProviderError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = append(m.failures, metricsEvent{Provider: provider, Model: model, Err: err})
}

func (m *fakeMetrics) OnReject(provider, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rejections = append(m.rejections, metricsEvent{Provider: provider, Model: model})
}

func (m *fakeMetrics) OnFallback(from, to string, err *ProviderError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallbacks = append(m.fallbacks, metricsEvent{Model: from, ToModel: to, Err: err})
}

func (m *fakeMetrics) counts() (retries, states, successes, failures, rejects, fallbacks int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.retries), len(m.states), len(m.successes), len(m.failures), len(m.rejections), len(m.fallbacks)
}

func TestBreakerDefaultsOpenAfterFiveFailures(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	metrics := &fakeMetrics{}
	breakers := NewBreakerSet(BreakerConfig{}, fakeClock, metrics)
	const provider, model = ProviderOpenCodeGo, "deepseek-v4.1-flash"

	for i := 0; i < 4; i++ {
		allowed, retryAfter := breakers.Allow(provider, model)
		if !allowed || retryAfter != 0 {
			t.Fatalf("Allow #%d = %v, %s; want true, 0", i+1, allowed, retryAfter)
		}
		breakers.Failure(provider, model)
	}
	if got := breakers.State(provider, model); got != BreakerClosed {
		t.Fatalf("State after 4 failures = %q, want closed", got)
	}

	allowed, _ := breakers.Allow(provider, model)
	if !allowed {
		t.Fatal("Allow #5 rejected before the threshold")
	}
	breakers.Failure(provider, model)
	if got := breakers.State(provider, model); got != BreakerOpen {
		t.Fatalf("State after 5 failures = %q, want open", got)
	}

	allowed, retryAfter := breakers.Allow(provider, model)
	if allowed {
		t.Fatal("open breaker allowed a call")
	}
	if retryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %s, want 30s", retryAfter)
	}
	fakeClock.Advance(29 * time.Second)
	allowed, retryAfter = breakers.Allow(provider, model)
	if allowed || retryAfter != time.Second {
		t.Fatalf("Allow at 29s = %v, %s; want false, 1s", allowed, retryAfter)
	}

	fakeClock.Advance(time.Second)
	allowed, _ = breakers.Allow(provider, model)
	if !allowed {
		t.Fatal("half-open breaker rejected its probe")
	}
	if got := breakers.State(provider, model); got != BreakerHalfOpen {
		t.Fatalf("State = %q, want half_open", got)
	}
	allowed, _ = breakers.Allow(provider, model)
	if allowed {
		t.Fatal("half-open breaker allowed a second concurrent probe")
	}

	breakers.Success(provider, model)
	if got := breakers.State(provider, model); got != BreakerClosed {
		t.Fatalf("State after probe success = %q, want closed", got)
	}
	allowed, _ = breakers.Allow(provider, model)
	if !allowed {
		t.Fatal("closed breaker rejected after probe success")
	}

	if retries, states, successes, _, _, _ := metrics.counts(); retries != 0 || successes != 0 || states != 3 {
		t.Fatalf("metrics retries/states/successes = %d/%d/%d, want 0/3/0", retries, states, successes)
	}
	if len(metrics.states) != 3 {
		t.Fatalf("state transitions = %d, want 3", len(metrics.states))
	}
	wantStates := []metricsEvent{
		{From: BreakerClosed, To: BreakerOpen},
		{From: BreakerOpen, To: BreakerHalfOpen},
		{From: BreakerHalfOpen, To: BreakerClosed},
	}
	for i, want := range wantStates {
		if metrics.states[i].From != want.From || metrics.states[i].To != want.To {
			t.Errorf("transition %d = %s->%s, want %s->%s", i, metrics.states[i].From, metrics.states[i].To, want.From, want.To)
		}
	}
}

func TestBreakerFailedProbeReopens(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	breakers := NewBreakerSet(BreakerConfig{}, fakeClock, nil)
	const provider, model = ProviderOpenCodeGo, "deepseek-v4.1-flash"
	for i := 0; i < 5; i++ {
		breakers.Failure(provider, model)
	}
	fakeClock.Advance(30 * time.Second)
	if allowed, _ := breakers.Allow(provider, model); !allowed {
		t.Fatal("half-open probe was rejected")
	}
	breakers.Failure(provider, model)
	if got := breakers.State(provider, model); got != BreakerOpen {
		t.Fatalf("State = %q, want open after failed probe", got)
	}
	allowed, retryAfter := breakers.Allow(provider, model)
	if allowed || retryAfter != 30*time.Second {
		t.Fatalf("Allow after failed probe = %v, %s; want false, 30s", allowed, retryAfter)
	}
}

func TestBreakerRecordOutcomeReopensProbeOnNonCountedFailure(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	breakers := NewBreakerSet(BreakerConfig{}, fakeClock, nil)
	const provider, model = ProviderOpenCodeGo, "m"
	for i := 0; i < 5; i++ {
		breakers.Failure(provider, model)
	}
	fakeClock.Advance(30 * time.Second)
	if allowed, _ := breakers.Allow(provider, model); !allowed {
		t.Fatal("half-open probe was rejected")
	}
	breakers.RecordOutcome(provider, model, &ProviderError{Kind: KindAuth})
	if got := breakers.State(provider, model); got != BreakerOpen {
		t.Fatalf("State = %q, want open after a non-counted probe failure", got)
	}
	allowed, retryAfter := breakers.Allow(provider, model)
	if allowed || retryAfter != 30*time.Second {
		t.Fatalf("Allow after non-counted probe failure = %v, %s; want false, 30s", allowed, retryAfter)
	}
}

func TestBreakerSuccessResetsFailureCount(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	breakers := NewBreakerSet(BreakerConfig{}, fakeClock, nil)
	const provider, model = ProviderOpenCodeGo, "deepseek-v4.1-flash"
	for i := 0; i < 4; i++ {
		breakers.Failure(provider, model)
	}
	breakers.Success(provider, model)
	for i := 0; i < 4; i++ {
		breakers.Failure(provider, model)
	}
	if got := breakers.State(provider, model); got != BreakerClosed {
		t.Fatalf("State = %q, want closed after a success reset", got)
	}
	breakers.Failure(provider, model)
	if got := breakers.State(provider, model); got != BreakerOpen {
		t.Fatalf("State = %q, want open on the fifth failure after reset", got)
	}
}

func TestBreakerKeysSeparateModels(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	breakers := NewBreakerSet(BreakerConfig{}, fakeClock, nil)
	for i := 0; i < 5; i++ {
		breakers.Failure(ProviderOpenCodeGo, "deepseek-v4.1-flash")
	}
	if allowed, _ := breakers.Allow(ProviderOpenCodeGo, "deepseek-v4.1-flash"); allowed {
		t.Fatal("opened model was allowed")
	}
	if allowed, _ := breakers.Allow(ProviderOpenCodeGo, "glm-5.3-flash"); !allowed {
		t.Fatal("breaker for a different model was affected")
	}
}

func TestBreakerCountsOnlyAvailabilityFailures(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	nonCounted := []ErrorKind{
		KindAuth, KindInvalidRequest, KindModelNotFound, KindContextLength,
		KindBudget, KindAccountRestricted, KindRateLimit, KindCassetteMiss,
		KindCanceled, KindCircuitOpen, KindInvalidResponse, KindUnknown,
	}
	for _, kind := range nonCounted {
		t.Run(string(kind), func(t *testing.T) {
			breakers := NewBreakerSet(BreakerConfig{}, fakeClock, nil)
			for i := 0; i < 10; i++ {
				breakers.RecordFailure(ProviderOpenCodeGo, "m", &ProviderError{Kind: kind})
			}
			if got := breakers.State(ProviderOpenCodeGo, "m"); got != BreakerClosed {
				t.Fatalf("State = %q, want closed for non-counted kind %q", got, kind)
			}
		})
	}
	for _, kind := range []ErrorKind{KindTimeout, KindUnavailable, KindOverloaded, KindServer} {
		t.Run(string(kind), func(t *testing.T) {
			breakers := NewBreakerSet(BreakerConfig{}, fakeClock, nil)
			for i := 0; i < 5; i++ {
				breakers.RecordFailure(ProviderOpenCodeGo, "m", &ProviderError{Kind: kind})
			}
			if got := breakers.State(ProviderOpenCodeGo, "m"); got != BreakerOpen {
				t.Fatalf("State = %q, want open for counted kind %q", got, kind)
			}
		})
	}
	if counts, retryAfter := NewBreakerSet(BreakerConfig{}, fakeClock, nil).Allow(ProviderOpenCodeGo, "m"); counts && retryAfter != 0 {
		t.Fatal("Allow on a fresh breaker returned a retry delay")
	}
}

func TestBreakerConcurrentHalfOpenAllowsOneProbe(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	breakers := NewBreakerSet(BreakerConfig{}, fakeClock, nil)
	for i := 0; i < 5; i++ {
		breakers.Failure(ProviderOpenCodeGo, "m")
	}
	fakeClock.Advance(30 * time.Second)

	const callers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCount := 0
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allowed, _ := breakers.Allow(ProviderOpenCodeGo, "m"); allowed {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowedCount != 1 {
		t.Fatalf("half-open allowed %d probes, want exactly 1", allowedCount)
	}
}

func TestBreakerClockMovedBackwardKeepsOpen(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	fakeClock := clock.NewFake(now)
	breakers := NewBreakerSet(BreakerConfig{}, fakeClock, nil)
	for i := 0; i < 5; i++ {
		breakers.Failure(ProviderOpenCodeGo, "m")
	}
	fakeClock.Set(now.Add(-time.Minute))
	allowed, retryAfter := breakers.Allow(ProviderOpenCodeGo, "m")
	if allowed {
		t.Fatal("breaker with a backward clock allowed a call")
	}
	if retryAfter <= 30*time.Second {
		t.Fatalf("RetryAfter = %s, want > 30s when the clock moved backward", retryAfter)
	}
}

func TestNopMetricsIsSafe(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	var metrics Metrics = NopMetrics{}
	metrics.OnRetry(ProviderOpenCodeGo, "m", 1, KindServer, time.Second)
	metrics.OnBreakerState(ProviderOpenCodeGo, "m", BreakerClosed, BreakerOpen)
	metrics.OnSuccess(ProviderOpenCodeGo, "m", Response{})
	metrics.OnFailure(ProviderOpenCodeGo, "m", &ProviderError{Kind: KindServer})
	metrics.OnReject(ProviderOpenCodeGo, "m")
	metrics.OnFallback("a", "b", &ProviderError{Kind: KindServer})

	breakers := NewBreakerSet(BreakerConfig{}, clock.NewFake(time.Now()), nil)
	if allowed, _ := breakers.Allow(ProviderOpenCodeGo, "m"); !allowed {
		t.Fatal("nil metrics broke the breaker")
	}
	_ = context.Background()
}
