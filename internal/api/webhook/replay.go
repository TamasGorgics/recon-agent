package webhook

import (
	"errors"
	"sync"
	"time"
)

// Replay cache errors. ErrReplay means the request is an exact replay of a
// previously accepted timestamp/signature pair. ErrReplayCacheFull means the
// cache hit its capacity and refused the new marker; callers fail closed.
var (
	// ErrReplay reports a replay marker that is still inside its TTL.
	ErrReplay = errors.New("webhook: replay detected")
	// ErrReplayCacheFull reports that the cache cannot store a new marker
	// without evicting a live one.
	ErrReplayCacheFull = errors.New("webhook: replay cache full")
)

// DefaultReplayCapacity is the process-local replay cache capacity.
const DefaultReplayCapacity = 4096

// ReplayCache records accepted webhook timestamp/signature pairs for the
// lifetime of the tolerance window. Implementations must be safe for
// concurrent use.
type ReplayCache interface {
	// CheckAndStore returns ErrReplay when key is present and unexpired. It
	// otherwise stores key until now+ttl and returns nil. ErrReplayCacheFull
	// means no entry was stored.
	CheckAndStore(key string, now time.Time, ttl time.Duration) error
}

// MemoryReplayCache is a mutex-protected, lazily pruned replay cache. It never
// evicts a live marker: a full cache fails closed with ErrReplayCacheFull.
// There is deliberately no background goroutine.
type MemoryReplayCache struct {
	mu       sync.Mutex
	entries  map[string]time.Time
	capacity int
}

// NewMemoryReplayCache returns a cache with the given capacity. A non-positive
// capacity selects DefaultReplayCapacity.
func NewMemoryReplayCache(capacity int) *MemoryReplayCache {
	if capacity <= 0 {
		capacity = DefaultReplayCapacity
	}
	return &MemoryReplayCache{
		entries:  make(map[string]time.Time),
		capacity: capacity,
	}
}

// CheckAndStore implements ReplayCache.
func (c *MemoryReplayCache) CheckAndStore(key string, now time.Time, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if expires, ok := c.entries[key]; ok {
		if expires.After(now) {
			return ErrReplay
		}
		delete(c.entries, key)
	}

	for candidate, expires := range c.entries {
		if !expires.After(now) {
			delete(c.entries, candidate)
		}
	}
	if len(c.entries) >= c.capacity {
		return ErrReplayCacheFull
	}
	c.entries[key] = now.Add(ttl)
	return nil
}
