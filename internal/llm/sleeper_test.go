package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

func TestFakeSleeperRecordsAndAdvancesClock(t *testing.T) {
	fakeClock := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	sleeper := NewFakeSleeper(fakeClock)

	if err := sleeper.Sleep(context.Background(), 1500*time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if err := sleeper.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep(0): %v", err)
	}
	if err := sleeper.Sleep(context.Background(), -time.Second); err != nil {
		t.Fatalf("Sleep(negative): %v", err)
	}

	durations := sleeper.Durations()
	want := []time.Duration{1500 * time.Millisecond, 0, 0}
	if len(durations) != len(want) {
		t.Fatalf("Durations = %v, want %v", durations, want)
	}
	for i := range want {
		if durations[i] != want[i] {
			t.Fatalf("Durations[%d] = %s, want %s", i, durations[i], want[i])
		}
	}
	if got := sleeper.Calls(); got != 3 {
		t.Fatalf("Calls = %d, want 3", got)
	}
	if got := fakeClock.Now().Sub(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)); got != 1500*time.Millisecond {
		t.Fatalf("fake clock advanced %s, want 1.5s", got)
	}
}

func TestFakeSleeperReturnsContextErrorWithoutRecording(t *testing.T) {
	sleeper := NewFakeSleeper(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sleeper.Sleep(ctx, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep error = %v, want context.Canceled", err)
	}
	if sleeper.Calls() != 0 {
		t.Fatalf("Calls = %d, want 0 for a canceled sleep", sleeper.Calls())
	}
}

func TestRealSleeperZeroAndCanceled(t *testing.T) {
	sleeper := RealSleeper{}
	if err := sleeper.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep(0) = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleeper.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep error = %v, want context.Canceled", err)
	}
}

func TestRealSleeperHonorsDeadline(t *testing.T) {
	sleeper := RealSleeper{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := sleeper.Sleep(ctx, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sleep error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Sleep blocked for %s, want deadline-bounded wait", elapsed)
	}
}
