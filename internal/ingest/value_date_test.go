package ingest_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
)

func utc(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

func date(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		t.Fatalf("parse date %q: %v", value, err)
	}
	return parsed.UTC()
}

func testSourceConfig() config.IngestConfig {
	return config.IngestConfig{Sources: map[string]config.SourceConfig{
		"psp_a": {
			Timezone: "America/New_York",
			Cutoff:   "17:00",
			Holidays: []string{"2026-01-01", "2026-07-03"},
		},
		// A seven-day calendar keeps weekend rolling out of the DST assertions.
		"seven_day": {
			Timezone:     "America/New_York",
			Cutoff:       "01:30",
			BusinessDays: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"},
		},
		"seven_day_1700": {
			Timezone:     "America/New_York",
			Cutoff:       "17:00",
			BusinessDays: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"},
		},
	}}
}

// covers: FIN-41, FIN-42
func TestValueDate(t *testing.T) {
	sources, err := ingest.NewSources(testSourceConfig())
	if err != nil {
		t.Fatalf("NewSources: %v", err)
	}

	cases := []struct {
		name   string
		source string
		ts     time.Time
		want   time.Time
	}{
		{
			name:   "before cutoff keeps the local date",
			source: "psp_a",
			ts:     utc(t, "2026-06-15T20:59:00Z"), // 16:59 EDT Monday
			want:   date(t, "2026-06-15"),
		},
		{
			name:   "exactly at cutoff rolls forward",
			source: "psp_a",
			ts:     utc(t, "2026-06-15T21:00:00Z"), // 17:00 EDT Monday
			want:   date(t, "2026-06-16"),
		},
		{
			name:   "after cutoff Friday rolls over the weekend",
			source: "psp_a",
			ts:     utc(t, "2026-06-19T22:00:00Z"), // 18:00 EDT Friday
			want:   date(t, "2026-06-22"),
		},
		{
			name:   "Saturday before cutoff rolls to Monday",
			source: "psp_a",
			ts:     utc(t, "2026-06-20T15:00:00Z"), // 11:00 EDT Saturday
			want:   date(t, "2026-06-22"),
		},
		{
			name:   "Sunday after cutoff rolls to Monday",
			source: "psp_a",
			ts:     utc(t, "2026-06-21T22:00:00Z"), // 18:00 EDT Sunday
			want:   date(t, "2026-06-22"),
		},
		{
			name:   "holiday Friday rolls to Monday",
			source: "psp_a",
			ts:     utc(t, "2026-07-03T15:00:00Z"), // 11:00 EDT observed holiday
			want:   date(t, "2026-07-06"),
		},
		{
			name:   "UTC date differs from the source local date",
			source: "psp_a",
			ts:     utc(t, "2026-06-16T02:00:00Z"), // 22:00 EDT Monday
			want:   date(t, "2026-06-16"),
		},
		{
			name:   "DST spring-forward just before cutoff",
			source: "seven_day",
			ts:     utc(t, "2026-03-08T06:29:00Z"), // 01:29 EST
			want:   date(t, "2026-03-08"),
		},
		{
			name:   "DST spring-forward exactly at cutoff",
			source: "seven_day",
			ts:     utc(t, "2026-03-08T06:30:00Z"), // 01:30 EST, clocks jump at 02:00
			want:   date(t, "2026-03-09"),
		},
		{
			name:   "DST spring-forward after the jump",
			source: "seven_day",
			ts:     utc(t, "2026-03-08T07:00:00Z"), // 03:00 EDT
			want:   date(t, "2026-03-09"),
		},
		{
			name:   "DST fall-back first 01:30 is before cutoff",
			source: "seven_day_1700",
			ts:     utc(t, "2026-11-01T05:30:00Z"), // 01:30 EDT
			want:   date(t, "2026-11-01"),
		},
		{
			name:   "DST fall-back second 01:30 is before cutoff",
			source: "seven_day_1700",
			ts:     utc(t, "2026-11-01T06:30:00Z"), // 01:30 EST, one hour later
			want:   date(t, "2026-11-01"),
		},
		{
			name:   "DST fall-back just before cutoff",
			source: "seven_day_1700",
			ts:     utc(t, "2026-11-01T21:59:00Z"), // 16:59 EST
			want:   date(t, "2026-11-01"),
		},
		{
			name:   "DST fall-back exactly at cutoff",
			source: "seven_day_1700",
			ts:     utc(t, "2026-11-01T22:00:00Z"), // 17:00 EST
			want:   date(t, "2026-11-02"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sources.ValueDate(tc.source, tc.ts)
			if err != nil {
				t.Fatalf("ValueDate: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("ValueDate(%s, %s) = %s, want %s", tc.source, tc.ts, got.Format("2006-01-02"), tc.want.Format("2006-01-02"))
			}
			if got.Location() != time.UTC || got.Hour() != 0 || got.Minute() != 0 {
				t.Errorf("ValueDate returned %v; business dates must be midnight UTC DATE values", got)
			}
		})
	}
}

// covers: FIN-41
func TestValueDateUnknownSource(t *testing.T) {
	sources, err := ingest.NewSources(testSourceConfig())
	if err != nil {
		t.Fatalf("NewSources: %v", err)
	}
	_, err = sources.ValueDate("nope", utc(t, "2026-06-15T12:00:00Z"))
	if !errors.Is(err, ingest.ErrUnknownSource) {
		t.Fatalf("ValueDate error = %v, want ErrUnknownSource", err)
	}
}

// covers: FIN-41
func TestNewSourcesRejectsInvalidConfig(t *testing.T) {
	cfg := config.IngestConfig{Sources: map[string]config.SourceConfig{
		"psp_a": {Timezone: "Not/AZone", Cutoff: "17:00"},
	}}
	_, err := ingest.NewSources(cfg)
	if err == nil {
		t.Fatal("NewSources accepted an invalid timezone")
	}
	if !strings.Contains(err.Error(), `"psp_a"`) {
		t.Errorf("NewSources error = %v, want it to name the source", err)
	}
}
