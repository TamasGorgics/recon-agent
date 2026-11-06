package llm

import (
	"strings"
	"sync"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// BreakerState is the state of one provider/model circuit breaker.
type BreakerState string

const (
	// BreakerClosed allows calls and counts failures.
	BreakerClosed BreakerState = "closed"
	// BreakerOpen rejects calls until the open duration elapses.
	BreakerOpen BreakerState = "open"
	// BreakerHalfOpen allows exactly one probe call.
	BreakerHalfOpen BreakerState = "half_open"
)

const (
	// DefaultBreakerFailureThreshold is the number of consecutive exhausted
	// availability failures that opens a breaker.
	DefaultBreakerFailureThreshold = 5
	// DefaultBreakerOpenDuration is how long an open breaker rejects calls.
	DefaultBreakerOpenDuration = 30 * time.Second
	// DefaultBreakerHalfOpenProbes is the number of half-open probes.
	DefaultBreakerHalfOpenProbes = 1
)

// BreakerConfig configures a BreakerSet. Zero values select the defaults.
type BreakerConfig struct {
	FailureThreshold int
	OpenDuration     time.Duration
	HalfOpenProbes   int
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = DefaultBreakerFailureThreshold
	}
	if c.OpenDuration <= 0 {
		c.OpenDuration = DefaultBreakerOpenDuration
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = DefaultBreakerHalfOpenProbes
	}
	// The spec allows exactly one half-open probe.
	if c.HalfOpenProbes > 1 {
		c.HalfOpenProbes = 1
	}
	return c
}

type breaker struct {
	state         BreakerState
	failures      int
	openedAt      time.Time
	probeInFlight bool
}

// BreakerSet holds per provider/model breakers. It is safe for concurrent use
// and evaluates state lazily on Allow using the injected clock (no timers).
type BreakerSet struct {
	mu       sync.Mutex
	clock    clock.Clock
	config   BreakerConfig
	metrics  Metrics
	breakers map[string]*breaker
}

// NewBreakerSet returns an empty breaker set with defaults applied.
func NewBreakerSet(config BreakerConfig, clk clock.Clock, metrics Metrics) *BreakerSet {
	if clk == nil {
		clk = clock.Real{}
	}
	return &BreakerSet{
		clock:    clk,
		config:   config.withDefaults(),
		metrics:  metrics,
		breakers: make(map[string]*breaker),
	}
}

func normalizeBreakerKey(provider, model string) (string, string) {
	provider = strings.TrimSpace(provider)
	model = APIID(strings.TrimSpace(model))
	return provider, model
}

func breakerKey(provider, model string) string {
	return provider + "/" + model
}

// getLocked returns the breaker for a key, creating a closed breaker if needed.
func (b *BreakerSet) getLocked(provider, model string) *breaker {
	key := breakerKey(provider, model)
	entry, ok := b.breakers[key]
	if !ok {
		entry = &breaker{state: BreakerClosed}
		b.breakers[key] = entry
	}
	return entry
}

// Allow reports whether a call may proceed. A rejected open breaker returns the
// remaining open duration. The transition to half-open is evaluated lazily.
func (b *BreakerSet) Allow(provider, model string) (bool, time.Duration) {
	provider, model = normalizeBreakerKey(provider, model)
	var (
		allowed    bool
		retryAfter time.Duration
		from, to   BreakerState
		changed    bool
	)

	b.mu.Lock()
	entry := b.getLocked(provider, model)
	switch entry.state {
	case BreakerOpen:
		remaining := b.config.OpenDuration - b.clock.Now().Sub(entry.openedAt)
		if remaining > 0 {
			retryAfter = remaining
			break
		}
		entry.state = BreakerHalfOpen
		entry.probeInFlight = true
		from, to, changed = BreakerOpen, BreakerHalfOpen, true
		allowed = true
	case BreakerHalfOpen:
		if !entry.probeInFlight {
			entry.probeInFlight = true
			allowed = true
		}
	default:
		allowed = true
	}
	b.mu.Unlock()

	if changed && b.metrics != nil {
		b.metrics.OnBreakerState(provider, model, from, to)
	}
	return allowed, retryAfter
}

// Success records a successful call, resetting failures and closing the
// breaker.
func (b *BreakerSet) Success(provider, model string) {
	provider, model = normalizeBreakerKey(provider, model)
	var (
		from, to BreakerState
		changed  bool
	)

	b.mu.Lock()
	entry := b.getLocked(provider, model)
	from = entry.state
	entry.state = BreakerClosed
	entry.failures = 0
	entry.probeInFlight = false
	to = BreakerClosed
	changed = from != to
	b.mu.Unlock()

	if changed && b.metrics != nil {
		b.metrics.OnBreakerState(provider, model, from, to)
	}
}

// Failure records one exhausted model call, opening or reopening the breaker.
func (b *BreakerSet) Failure(provider, model string) {
	provider, model = normalizeBreakerKey(provider, model)
	var (
		from, to BreakerState
		changed  bool
	)

	b.mu.Lock()
	entry := b.getLocked(provider, model)
	switch entry.state {
	case BreakerClosed:
		entry.failures++
		if entry.failures >= b.config.FailureThreshold {
			entry.state = BreakerOpen
			entry.openedAt = b.clock.Now()
			entry.probeInFlight = false
			from, to, changed = BreakerClosed, BreakerOpen, true
		}
	case BreakerHalfOpen:
		entry.state = BreakerOpen
		entry.openedAt = b.clock.Now()
		entry.probeInFlight = false
		from, to, changed = BreakerHalfOpen, BreakerOpen, true
	case BreakerOpen:
		// Already open; keep the original deadline.
	}
	b.mu.Unlock()

	if changed && b.metrics != nil {
		b.metrics.OnBreakerState(provider, model, from, to)
	}
}

// RecordFailure counts err only when it indicates provider unavailability.
func (b *BreakerSet) RecordFailure(provider, model string, err *ProviderError) {
	if err == nil || !countsAsBreakerFailure(err.Kind) {
		return
	}
	b.Failure(provider, model)
}

// RecordOutcome records the result of one completed model call. A successful
// call closes and resets the breaker. A failed half-open probe reopens the
// breaker for a fresh open duration regardless of error classification, so a
// fatal, auth, canceled, or invalid-response probe cannot leave the breaker
// permanently half-open. Closed-state failures count only availability kinds.
func (b *BreakerSet) RecordOutcome(provider, model string, err *ProviderError) {
	if err == nil {
		b.Success(provider, model)
		return
	}
	if countsAsBreakerFailure(err.Kind) {
		b.Failure(provider, model)
		return
	}
	b.probeFailed(provider, model)
}

// probeFailed consumes a half-open probe whose call did not succeed. Every
// probe outcome must transition the breaker; non-counted kinds do not open a
// closed breaker but still reopen a failed probe.
func (b *BreakerSet) probeFailed(provider, model string) {
	provider, model = normalizeBreakerKey(provider, model)
	var changed bool

	b.mu.Lock()
	entry := b.getLocked(provider, model)
	if entry.state == BreakerHalfOpen {
		entry.state = BreakerOpen
		entry.openedAt = b.clock.Now()
		entry.probeInFlight = false
		changed = true
	}
	b.mu.Unlock()

	if changed && b.metrics != nil {
		b.metrics.OnBreakerState(provider, model, BreakerHalfOpen, BreakerOpen)
	}
}

// State returns the current state without advancing an open timer.
func (b *BreakerSet) State(provider, model string) BreakerState {
	provider, model = normalizeBreakerKey(provider, model)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.getLocked(provider, model).state
}

// countsAsBreakerFailure reports whether an exhausted call contributes to the
// breaker. Fatal, budget, rate-limit, and cancellation errors do not.
func countsAsBreakerFailure(kind ErrorKind) bool {
	switch kind {
	case KindTimeout, KindUnavailable, KindOverloaded, KindServer:
		return true
	default:
		return false
	}
}
