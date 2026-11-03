// covers: FIN-43
package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFakeNow(t *testing.T) {
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
}

func TestFakeSince(t *testing.T) {
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	f := NewFake(start.Add(90 * time.Second))
	if got, want := f.Since(start), 90*time.Second; got != want {
		t.Fatalf("Since(start) = %v, want %v", got, want)
	}
	if got, want := f.Since(start.Add(2*time.Minute)), -30*time.Second; got != want {
		t.Fatalf("Since(future) = %v, want %v", got, want)
	}
}

func TestFakeAdvance(t *testing.T) {
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)
	f.Advance(5 * time.Minute)
	if got, want := f.Now(), start.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", got, want)
	}
	f.Advance(-2 * time.Minute)
	if got, want := f.Now(), start.Add(3*time.Minute); !got.Equal(want) {
		t.Fatalf("Now() after negative Advance = %v, want %v", got, want)
	}
}

func TestFakeSet(t *testing.T) {
	f := NewFake(time.Unix(0, 0))
	want := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	f.Set(want)
	if got := f.Now(); !got.Equal(want) {
		t.Fatalf("Now() after Set = %v, want %v", got, want)
	}
	if got := f.Since(want); got != 0 {
		t.Fatalf("Since(now) = %v, want 0", got)
	}
}

func TestFakeConcurrentAccess(t *testing.T) {
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				switch j % 4 {
				case 0:
					f.Advance(time.Millisecond)
				case 1:
					_ = f.Now()
				case 2:
					_ = f.Since(start)
				case 3:
					f.Set(start.Add(time.Duration(worker*1000+j) * time.Millisecond))
				}
			}
		}(i)
	}
	wg.Wait()

	want := time.Date(2028, 6, 7, 8, 9, 10, 0, time.UTC)
	f.Set(want)
	if got := f.Now(); !got.Equal(want) {
		t.Fatalf("Now() after concurrent access = %v, want %v", got, want)
	}
}

func TestReal(t *testing.T) {
	var c Clock = Real{}
	start := c.Now()
	if start.IsZero() {
		t.Fatal("Real.Now() returned the zero time")
	}
	if got := c.Since(start); got < 0 {
		t.Fatalf("Real.Since(start) = %v, want >= 0", got)
	}
}
