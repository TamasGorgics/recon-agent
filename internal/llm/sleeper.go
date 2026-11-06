package llm

import (
	"context"
	"sync"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// Sleeper pauses a retry without extending clock.Clock. ADR 0003 keeps timers
// out of the domain clock; resilience therefore injects a separate sleeper.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// RealSleeper is a Sleeper backed by a timer.
type RealSleeper struct{}

// Sleep waits for d or until ctx is done, whichever comes first. Non-positive
// durations return immediately.
func (RealSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// FakeSleeper records requested sleeps and optionally advances a clock.Fake.
// It never blocks, so resilience tests are deterministic.
type FakeSleeper struct {
	mu        sync.Mutex
	clock     *clock.Fake
	durations []time.Duration
}

// NewFakeSleeper returns a FakeSleeper. When clk is non-nil, each sleep advances
// it by the recorded duration.
func NewFakeSleeper(clk *clock.Fake) *FakeSleeper {
	return &FakeSleeper{clock: clk}
}

// Sleep records a non-negative duration and advances the fake clock.
func (f *FakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d < 0 {
		d = 0
	}
	f.mu.Lock()
	f.durations = append(f.durations, d)
	f.mu.Unlock()
	if f.clock != nil {
		f.clock.Advance(d)
	}
	return nil
}

// Durations returns every recorded sleep in order.
func (f *FakeSleeper) Durations() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.durations...)
}

// Calls returns the number of recorded sleeps.
func (f *FakeSleeper) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.durations)
}
