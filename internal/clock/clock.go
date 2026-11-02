package clock

import (
	"sync"
	"time"
)

// Clock is the time source used by domain packages. Tests, evals, and replays
// inject a deterministic implementation instead of reading the wall clock.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// Since returns the time elapsed since t.
	Since(t time.Time) time.Duration
}

// Real is a Clock backed by the system wall clock. Use it at process
// boundaries only; domain packages receive a Clock instead.
type Real struct{}

// Now returns the current local time.
func (Real) Now() time.Time { return time.Now() }

// Since returns the time elapsed since t.
func (Real) Since(t time.Time) time.Duration { return time.Since(t) }

// Fake is a manually controlled Clock for tests and replays. All methods are
// safe for concurrent use. Fake deliberately has no timers, After, or Ticker;
// those are deferred until a milestone needs them.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake clock set to now.
func NewFake(now time.Time) *Fake {
	return &Fake{now: now}
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since returns the duration elapsed on the fake clock since t.
func (f *Fake) Since(t time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now.Sub(t)
}

// Advance moves the fake clock forward by d. A negative d moves it backward.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set sets the fake clock's current time to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}
