// covers: FIN-22

package outbox_test

import (
	"context"
	"sync"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

func testMessage(key string) outbox.Message {
	return outbox.Message{
		ID:             "id-" + key,
		Topic:          "approval.request",
		Payload:        []byte(`{"key":"` + key + `"}`),
		IdempotencyKey: key,
	}
}

// TestFakeChannelDeduplicatesByKey proves a redelivery has one side effect.
func TestFakeChannelDeduplicatesByKey(t *testing.T) {
	channel := outbox.NewFakeChannel()
	ctx := context.Background()
	msg := testMessage("dedupe-1")

	for i := 0; i < 3; i++ {
		if err := channel.Handle(ctx, msg); err != nil {
			t.Fatalf("Handle call %d: %v", i+1, err)
		}
	}
	if got := channel.Delivered(msg.IdempotencyKey); got != 1 {
		t.Fatalf("Delivered(%q) = %d, want 1", msg.IdempotencyKey, got)
	}
	if got := len(channel.Messages()); got != 1 {
		t.Fatalf("Messages length = %d, want 1", got)
	}
	if err := channel.Handle(ctx, testMessage("dedupe-2")); err != nil {
		t.Fatalf("Handle second key: %v", err)
	}
	if got := len(channel.Messages()); got != 2 {
		t.Fatalf("Messages length after second key = %d, want 2", got)
	}
}

// TestFakeChannelFailAfterSend proves the crash-after-send simulation records
// the side effect, fails once, and deduplicates the redelivery.
func TestFakeChannelFailAfterSend(t *testing.T) {
	channel := outbox.NewFakeChannel()
	channel.SetFailAfterSend(true)
	ctx := context.Background()
	msg := testMessage("crash-after-send")

	if err := channel.Handle(ctx, msg); err == nil {
		t.Fatal("first Handle with FailAfterSend returned nil, want error")
	}
	if got := channel.Delivered(msg.IdempotencyKey); got != 1 {
		t.Fatalf("Delivered after failed send = %d, want 1", got)
	}
	if err := channel.Handle(ctx, msg); err != nil {
		t.Fatalf("redelivery with FailAfterSend: %v, want nil dedupe", err)
	}
	if got := channel.Delivered(msg.IdempotencyKey); got != 1 {
		t.Fatalf("Delivered after redelivery = %d, want 1", got)
	}
	if got := len(channel.Messages()); got != 1 {
		t.Fatalf("Messages length = %d, want 1", got)
	}
}

// TestFakeChannelAlwaysFail proves AlwaysFail records nothing and takes
// precedence over dedupe and FailAfterSend.
func TestFakeChannelAlwaysFail(t *testing.T) {
	channel := outbox.NewFakeChannel()
	channel.SetFailAfterSend(true)
	channel.SetAlwaysFail(true)
	ctx := context.Background()
	msg := testMessage("always-fail")

	for i := 0; i < 2; i++ {
		if err := channel.Handle(ctx, msg); err == nil {
			t.Fatalf("Handle call %d returned nil, want error", i+1)
		}
	}
	if got := channel.Delivered(msg.IdempotencyKey); got != 0 {
		t.Fatalf("Delivered = %d, want 0", got)
	}
	if got := len(channel.Messages()); got != 0 {
		t.Fatalf("Messages length = %d, want 0", got)
	}
}

// TestFakeChannelConcurrent proves the recorder is race-safe and still
// deduplicates concurrent redeliveries of one key.
func TestFakeChannelConcurrent(t *testing.T) {
	channel := outbox.NewFakeChannel()
	ctx := context.Background()
	msg := testMessage("concurrent")

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if err := channel.Handle(ctx, msg); err != nil {
				t.Errorf("Handle: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := channel.Delivered(msg.IdempotencyKey); got != 1 {
		t.Fatalf("Delivered = %d, want 1", got)
	}
	if got := len(channel.Messages()); got != 1 {
		t.Fatalf("Messages length = %d, want 1", got)
	}
}

// TestFakeChannelCanceledContext proves a canceled handler context does not
// record a side effect.
func TestFakeChannelCanceledContext(t *testing.T) {
	channel := outbox.NewFakeChannel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := channel.Handle(ctx, testMessage("canceled")); err == nil {
		t.Fatal("Handle with canceled context returned nil, want error")
	}
	if got := len(channel.Messages()); got != 0 {
		t.Fatalf("Messages length = %d, want 0", got)
	}
}
