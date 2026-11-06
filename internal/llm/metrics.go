package llm

import "time"

// Metrics receives resilience signals. It is a hook interface so M2-03 has no
// metrics backend dependency; M5-03 wires the concrete implementation. Methods
// must not call back into the Resilient or BreakerSet that invoked them.
type Metrics interface {
	OnRetry(provider, model string, attempt int, kind ErrorKind, delay time.Duration)
	OnBreakerState(provider, model string, from, to BreakerState)
	OnSuccess(provider, model string, resp Response)
	OnFailure(provider, model string, err *ProviderError)
	OnReject(provider, model string)
	OnFallback(from, to string, err *ProviderError)
}

// NopMetrics is a no-op Metrics implementation.
type NopMetrics struct{}

// OnRetry implements Metrics.
func (NopMetrics) OnRetry(string, string, int, ErrorKind, time.Duration) {}

// OnBreakerState implements Metrics.
func (NopMetrics) OnBreakerState(string, string, BreakerState, BreakerState) {}

// OnSuccess implements Metrics.
func (NopMetrics) OnSuccess(string, string, Response) {}

// OnFailure implements Metrics.
func (NopMetrics) OnFailure(string, string, *ProviderError) {}

// OnReject implements Metrics.
func (NopMetrics) OnReject(string, string) {}

// OnFallback implements Metrics.
func (NopMetrics) OnFallback(string, string, *ProviderError) {}
