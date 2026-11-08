package sandbox

import (
	"testing"
	"time"
)

func TestSelectScenarioTimeMaxAndFallback(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger := []time.Time{
		time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC),
	}
	external := []time.Time{
		time.Date(2026, 1, 4, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC),
	}
	if got, want := selectScenarioTime(ledger, external, base),
		time.Date(2026, 1, 4, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("scenario time = %s, want %s", got, want)
	}
	if got := selectScenarioTime(nil, nil, base); !got.Equal(base) {
		t.Fatalf("empty fallback = %s, want %s", got, base)
	}
}

func TestSelectScenarioTimeNormalizesUTC(t *testing.T) {
	location := time.FixedZone("local", 2*60*60)
	event := time.Date(2026, 1, 4, 12, 0, 0, 0, location)
	got := selectScenarioTime([]time.Time{event}, nil, time.Time{})
	if got.Location() != time.UTC {
		t.Fatalf("location = %s, want UTC", got.Location())
	}
	if !got.Equal(event) {
		t.Fatalf("value = %s, want %s", got, event)
	}
}
