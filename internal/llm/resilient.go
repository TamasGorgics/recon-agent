package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const (
	// DefaultResilientMaxAttempts is the total attempt budget per model.
	DefaultResilientMaxAttempts = 3
	// DefaultResilientBaseDelay is the first backoff bound.
	DefaultResilientBaseDelay = 500 * time.Millisecond
	// DefaultResilientMaxDelay caps jittered and Retry-After delays.
	DefaultResilientMaxDelay = 30 * time.Second
	// DefaultResilientFactor is the exponential backoff factor.
	DefaultResilientFactor = 2

	maxResilientAttempts = 5
)

// ResilientConfig configures the retry, breaker, and fallback wrapper. Zero
// values select the defaults.
type ResilientConfig struct {
	Provider    string
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Factor      int
	Jitter      func(max int64) int64
	Sleeper     Sleeper
	Clock       clock.Clock
	Breakers    *BreakerSet
	Fallbacks   []string
	Registry    *Registry
	Metrics     Metrics
}

// Resilient wraps a Client with bounded retries, per-model circuit breakers, and
// an allowlisted fallback chain. It never performs retries or fallbacks for
// fatal classification errors.
type Resilient struct {
	inner       Client
	provider    string
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
	factor      int
	jitter      func(max int64) int64
	sleeper     Sleeper
	clock       clock.Clock
	breakers    *BreakerSet
	fallbacks   []string
	metrics     Metrics
}

// NewResilient validates cfg and wraps inner. Fallback models must exist in the
// pricing registry and be marked allowed_fallback.
func NewResilient(inner Client, cfg ResilientConfig) (*Resilient, error) {
	if inner == nil {
		return nil, errors.New("llm: resilient requires an inner client")
	}
	if cfg.MaxAttempts < 0 || cfg.MaxAttempts > maxResilientAttempts {
		return nil, fmt.Errorf("llm: max attempts must be between 1 and %d, got %d", maxResilientAttempts, cfg.MaxAttempts)
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = DefaultResilientMaxAttempts
	}
	if cfg.BaseDelay < 0 {
		return nil, fmt.Errorf("llm: base delay must not be negative, got %s", cfg.BaseDelay)
	}
	if cfg.BaseDelay == 0 {
		cfg.BaseDelay = DefaultResilientBaseDelay
	}
	if cfg.MaxDelay < 0 {
		return nil, fmt.Errorf("llm: max delay must not be negative, got %s", cfg.MaxDelay)
	}
	if cfg.MaxDelay == 0 {
		cfg.MaxDelay = DefaultResilientMaxDelay
	}
	if cfg.Factor < 0 {
		return nil, fmt.Errorf("llm: backoff factor must not be negative, got %d", cfg.Factor)
	}
	if cfg.Factor == 0 {
		cfg.Factor = DefaultResilientFactor
	}

	registry := cfg.Registry
	if registry == nil {
		registry = DefaultRegistry()
	}
	if len(cfg.Fallbacks) > maxFallbackChain {
		return nil, fmt.Errorf("llm: fallback chain has %d models, maximum is %d", len(cfg.Fallbacks), maxFallbackChain)
	}
	fallbacks := make([]string, 0, len(cfg.Fallbacks))
	seen := make(map[string]struct{}, len(cfg.Fallbacks))
	for _, raw := range cfg.Fallbacks {
		id := APIID(strings.TrimSpace(raw))
		if id == "" {
			return nil, errors.New("llm: fallback model is empty")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("llm: fallback %q is duplicated", Redact(id))
		}
		model, err := registry.Get(id)
		if err != nil {
			return nil, fmt.Errorf("llm: fallback %q: %w", Redact(id), err)
		}
		if !model.AllowedFallback {
			return nil, fmt.Errorf("llm: fallback %q is not allowed_fallback", Redact(id))
		}
		seen[id] = struct{}{}
		fallbacks = append(fallbacks, id)
	}

	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = ProviderOpenCodeGo
	}
	timeSource := cfg.Clock
	if timeSource == nil {
		timeSource = clock.Real{}
	}
	sleeper := cfg.Sleeper
	if sleeper == nil {
		sleeper = RealSleeper{}
	}
	jitter := cfg.Jitter
	if jitter == nil {
		jitter = defaultJitter
	}
	breakers := cfg.Breakers
	if breakers == nil {
		breakers = NewBreakerSet(BreakerConfig{}, timeSource, cfg.Metrics)
	}

	return &Resilient{
		inner:       inner,
		provider:    provider,
		maxAttempts: cfg.MaxAttempts,
		baseDelay:   cfg.BaseDelay,
		maxDelay:    cfg.MaxDelay,
		factor:      cfg.Factor,
		jitter:      jitter,
		sleeper:     sleeper,
		clock:       timeSource,
		breakers:    breakers,
		fallbacks:   fallbacks,
		metrics:     cfg.Metrics,
	}, nil
}

// Complete runs the request through retry, breaker, and fallback. Attempts is
// the total across every model tried.
func (r *Resilient) Complete(ctx context.Context, req Request) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestedModel := APIID(strings.TrimSpace(req.Model))
	if err := req.Validate(); err != nil {
		pe := newRequestError(r.provider, requestedModel, err.Error())
		pe.Attempts = 0
		return Response{}, pe
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Response{}, r.canceledError(requestedModel, ctxErr, 0)
	}

	tried := map[string]struct{}{requestedModel: {}}
	current := requestedModel
	totalAttempts := 0

	for {
		allowed, retryAfter := r.breakers.Allow(r.provider, current)
		if !allowed {
			openErr := &ProviderError{
				Provider:   r.provider,
				Model:      current,
				Kind:       KindCircuitOpen,
				Retryable:  false,
				RetryAfter: retryAfter,
				Message:    "circuit breaker open",
				Attempts:   totalAttempts,
			}
			r.fireReject(current)
			next, ok := r.nextFallback(tried)
			if !ok {
				return Response{}, openErr
			}
			r.fireFallback(current, next, openErr)
			tried[next] = struct{}{}
			current = next
			continue
		}

		resp, providerErr, attempts, canFallback := r.attemptModel(ctx, req, current)
		totalAttempts += attempts
		if providerErr == nil {
			return r.successResponse(resp, requestedModel, current, totalAttempts), nil
		}
		providerErr.Attempts = totalAttempts

		r.breakers.RecordOutcome(r.provider, current, providerErr)
		if !canFallback {
			return Response{}, providerErr
		}
		next, ok := r.nextFallback(tried)
		if !ok {
			return Response{}, providerErr
		}
		r.fireFallback(current, next, providerErr)
		tried[next] = struct{}{}
		current = next
	}
}

// attemptModel makes up to MaxAttempts calls for one model and returns the typed
// final error plus whether the model exhausted its retries with a retryable
// error and may fall back. It never sleeps after the final attempt.
func (r *Resilient) attemptModel(ctx context.Context, req Request, model string) (Response, *ProviderError, int, bool) {
	callReq := req
	callReq.Model = model
	attempts := 0

	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, r.canceledError(model, ctxErr, attempts), attempts, false
		}
		attempts++
		resp, err := r.inner.Complete(ctx, callReq)
		if err == nil {
			return resp, nil, attempts, false
		}

		providerErr := r.typedError(err, model, attempts)
		r.fireFailure(model, providerErr)
		if !providerErr.Retryable || isFatalKind(providerErr.Kind) {
			return Response{}, providerErr, attempts, false
		}
		if attempt == r.maxAttempts {
			return Response{}, providerErr, attempts, true
		}

		delay := r.retryDelay(attempt, providerErr.RetryAfter)
		if deadline, ok := ctx.Deadline(); ok {
			remaining := deadline.Sub(r.clock.Now())
			if remaining <= 0 {
				return Response{}, r.canceledError(model, context.DeadlineExceeded, attempts), attempts, false
			}
			if delay > remaining {
				// The retry would wake after the deadline; surface the provider
				// error instead of sleeping and do not move to a fallback.
				return Response{}, providerErr, attempts, false
			}
		}
		r.fireRetry(model, attempt, providerErr.Kind, delay)
		if sleepErr := r.sleeper.Sleep(ctx, delay); sleepErr != nil {
			cause := ctx.Err()
			if cause == nil {
				cause = sleepErr
			}
			return Response{}, r.canceledError(model, cause, attempts), attempts, false
		}
	}

	return Response{}, r.canceledError(model, context.Canceled, attempts), attempts, false
}

// retryDelay returns the full-jitter delay for a failed attempt, or the clamped
// Retry-After value when the provider supplied one.
func (r *Resilient) retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > r.maxDelay {
			return r.maxDelay
		}
		return retryAfter
	}

	delay := r.baseDelay
	for i := 1; i < attempt; i++ {
		if delay >= r.maxDelay || r.factor <= 1 {
			if delay > r.maxDelay {
				delay = r.maxDelay
			}
			break
		}
		if delay > r.maxDelay/time.Duration(r.factor) {
			delay = r.maxDelay
			break
		}
		delay *= time.Duration(r.factor)
	}
	if delay > r.maxDelay {
		delay = r.maxDelay
	}
	if delay <= 0 {
		return 0
	}
	jittered := r.jitter(int64(delay))
	if jittered < 0 {
		jittered = 0
	}
	if jittered > int64(delay) {
		jittered = int64(delay)
	}
	return time.Duration(jittered)
}

func defaultJitter(max int64) int64 {
	if max <= 0 {
		return 0
	}
	return rand.Int64N(max + 1)
}

func (r *Resilient) nextFallback(tried map[string]struct{}) (string, bool) {
	for _, fallback := range r.fallbacks {
		if _, ok := tried[fallback]; ok {
			continue
		}
		return fallback, true
	}
	return "", false
}

func (r *Resilient) successResponse(resp Response, requestedModel, actualFallback string, attempts int) Response {
	actualModel := APIID(strings.TrimSpace(resp.ActualModel))
	if actualModel == "" {
		actualModel = actualFallback
	}
	if resp.Provider == "" {
		resp.Provider = r.provider
	}
	resp.RequestedModel = requestedModel
	resp.ActualModel = actualModel
	resp.FallbackUsed = actualModel != requestedModel
	resp.Attempts = attempts
	r.breakers.RecordOutcome(r.provider, actualFallback, nil)
	r.fireSuccess(actualFallback, resp)
	return resp
}

// typedError normalizes any inner error into a redacted ProviderError.
func (r *Resilient) typedError(err error, model string, attempts int) *ProviderError {
	if pe, ok := AsProviderError(err); ok {
		out := *pe
		if out.Provider == "" {
			out.Provider = r.provider
		}
		out.Model = model
		out.Message = Redact(out.Message)
		if len(out.Body) > 0 {
			out.Body = redactedBody(out.Body)
		}
		out.Attempts = attempts
		return &out
	}
	return &ProviderError{
		Provider:  r.provider,
		Model:     model,
		Kind:      KindUnknown,
		Retryable: false,
		Message:   Redact(err.Error()),
		Cause:     err,
		Attempts:  attempts,
	}
}

func (r *Resilient) canceledError(model string, cause error, attempts int) *ProviderError {
	if cause == nil {
		cause = context.Canceled
	}
	return &ProviderError{
		Provider:  r.provider,
		Model:     model,
		Kind:      KindCanceled,
		Retryable: false,
		Message:   Redact(cause.Error()),
		Cause:     cause,
		Attempts:  attempts,
	}
}

func (r *Resilient) fireRetry(model string, attempt int, kind ErrorKind, delay time.Duration) {
	if r.metrics != nil {
		r.metrics.OnRetry(r.provider, model, attempt, kind, delay)
	}
}

func (r *Resilient) fireSuccess(model string, resp Response) {
	if r.metrics != nil {
		r.metrics.OnSuccess(r.provider, model, resp)
	}
}

func (r *Resilient) fireFailure(model string, err *ProviderError) {
	if r.metrics != nil {
		r.metrics.OnFailure(r.provider, model, err)
	}
}

func (r *Resilient) fireReject(model string) {
	if r.metrics != nil {
		r.metrics.OnReject(r.provider, model)
	}
}

func (r *Resilient) fireFallback(from, to string, err *ProviderError) {
	if r.metrics != nil {
		r.metrics.OnFallback(from, to, err)
	}
}
