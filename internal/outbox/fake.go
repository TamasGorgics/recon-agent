package outbox

import (
	"context"
	"errors"
	"sync"
)

var (
	errFakeAlwaysFail    = errors.New("outbox: fake channel always fails")
	errFakeFailAfterSend = errors.New("outbox: fake channel failed after send")
)

var _ Handler = (*FakeChannel)(nil)

// FakeChannel is a concurrency-safe test handler that records side effects and
// deduplicates them by idempotency key. It performs no network I/O.
//
// The first Handle call for an idempotency key records one message and one
// side effect. A redelivery of the same key returns nil without a second side
// effect, which is the idempotent behavior the outbox contract requires.
//
// SetAlwaysFail makes every Handle call return an error without recording a
// side effect, for dead-letter tests. SetFailAfterSend makes the first Handle
// call for a key record the side effect and then return an error, simulating a
// crash after send but before the delivery mark; a later redelivery of the
// same key deduplicates and succeeds.
type FakeChannel struct {
	mu            sync.Mutex
	delivered     map[string]int
	messages      []Message
	failAfterSend bool
	alwaysFail    bool
}

// NewFakeChannel returns an empty FakeChannel.
func NewFakeChannel() *FakeChannel {
	return &FakeChannel{delivered: make(map[string]int)}
}

// Handle records one side effect per idempotency key and returns nil. The
// configured failure mode is applied before the dedupe check only for
// AlwaysFail; FailAfterSend fails the first send only.
func (f *FakeChannel) Handle(ctx context.Context, msg Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alwaysFail {
		return errFakeAlwaysFail
	}
	if f.delivered == nil {
		f.delivered = make(map[string]int)
	}
	if f.delivered[msg.IdempotencyKey] > 0 {
		return nil
	}
	f.delivered[msg.IdempotencyKey]++
	f.messages = append(f.messages, msg)
	if f.failAfterSend {
		return errFakeFailAfterSend
	}
	return nil
}

// Delivered reports how many unique side effects were recorded for idemKey.
// The deduplicating FakeChannel returns 0 or 1.
func (f *FakeChannel) Delivered(idemKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.delivered[idemKey]
}

// Messages returns a copy of the recorded messages in first-delivery order.
func (f *FakeChannel) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, len(f.messages))
	copy(out, f.messages)
	return out
}

// SetFailAfterSend enables or disables recording a side effect and then
// failing the first delivery of a key.
func (f *FakeChannel) SetFailAfterSend(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAfterSend = enabled
}

// SetAlwaysFail enables or disables failing every invocation without recording
// a side effect. It takes precedence over FailAfterSend.
func (f *FakeChannel) SetAlwaysFail(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alwaysFail = enabled
}
