package bizdays_test

import (
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/bizdays"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

// legacyCalendar is the FIN-41 implementation that lived in
// internal/ingest/value_date.go before the extraction. It is kept verbatim in
// this test so equivalence is proved against the old algorithm rather than
// against a re-derivation of the new one.
type legacyCalendar struct {
	location       *time.Location
	cutoffHour     int
	cutoffMinute   int
	cutoffSecond   int
	businessDays   [7]bool
	holidayDateKey map[string]struct{}
}

func newLegacyCalendar(t *testing.T, cfg config.SourceConfig) legacyCalendar {
	t.Helper()
	location, err := cfg.Location()
	if err != nil {
		t.Fatalf("legacy location: %v", err)
	}
	hour, minute, second, err := cfg.CutoffClock()
	if err != nil {
		t.Fatalf("legacy cutoff: %v", err)
	}
	weekdays, err := cfg.Weekdays()
	if err != nil {
		t.Fatalf("legacy weekdays: %v", err)
	}
	holidays, err := cfg.HolidayKeys()
	if err != nil {
		t.Fatalf("legacy holidays: %v", err)
	}
	return legacyCalendar{
		location:       location,
		cutoffHour:     hour,
		cutoffMinute:   minute,
		cutoffSecond:   second,
		businessDays:   weekdays,
		holidayDateKey: holidays,
	}
}

func (c legacyCalendar) valueDate(ts time.Time) time.Time {
	local := ts.In(c.location)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.location)
	afterCutoff := local.Hour() > c.cutoffHour ||
		(local.Hour() == c.cutoffHour && local.Minute() > c.cutoffMinute) ||
		(local.Hour() == c.cutoffHour && local.Minute() == c.cutoffMinute && local.Second() >= c.cutoffSecond)
	if afterCutoff {
		candidate = candidate.AddDate(0, 0, 1)
	}
	const maxRollDays = 3660
	for day := 0; !c.isBusinessDay(candidate) && day < maxRollDays; day++ {
		candidate = candidate.AddDate(0, 0, 1)
	}
	year, month, day := candidate.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func (c legacyCalendar) isBusinessDay(date time.Time) bool {
	if !c.businessDays[date.Weekday()] {
		return false
	}
	_, holiday := c.holidayDateKey[date.Format("2006-01-02")]
	return !holiday
}

func testConfig() config.IngestConfig {
	return config.IngestConfig{Sources: map[string]config.SourceConfig{
		"psp_a": {
			Timezone: "America/New_York",
			Cutoff:   "17:00",
			Holidays: []string{"2026-01-01", "2026-07-03", "2026-11-26"},
		},
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
		"utc_leap": {
			Timezone: "UTC",
			Cutoff:   "17:00",
		},
	}}
}

func mustSources(t *testing.T) bizdays.Sources {
	t.Helper()
	sources, err := bizdays.NewSources(testConfig())
	if err != nil {
		t.Fatalf("NewSources: %v", err)
	}
	return sources
}

func mustCalendar(t *testing.T, source string) bizdays.Calendar {
	t.Helper()
	calendar, ok := mustSources(t).Calendar(source)
	if !ok {
		t.Fatalf("calendar %q missing", source)
	}
	return calendar
}

func utc(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed.UTC()
}

// covers: FIN-41
func TestValueDateMatchesLegacyFIN41Implementation(t *testing.T) {
	sources := mustSources(t)
	cfg := testConfig()

	// A whole year plus both DST transitions, sampled before the cutoff,
	// exactly at the cutoff, and after it.
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for day := 0; day < 366; day++ {
		date := start.AddDate(0, 0, day)
		for _, sourceName := range []string{"psp_a", "seven_day", "seven_day_1700"} {
			legacy := newLegacyCalendar(t, cfg.Sources[sourceName])
			calendar := sources[sourceName]
			location := calendar.Location()
			for _, hour := range []int{8, 17, 18} {
				local := time.Date(date.Year(), date.Month(), date.Day(), hour, 0, 0, 0, location)
				ts := local.UTC()
				want := legacy.valueDate(ts)
				got := calendar.ValueDate(ts)
				if !got.Equal(want) {
					t.Fatalf("%s ValueDate(%s) = %s, legacy %s",
						sourceName, local.Format(time.RFC3339), dateString(got), dateString(want))
				}
				if got.Location() != time.UTC || got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
					t.Fatalf("%s ValueDate(%s) = %v; want midnight UTC DATE",
						sourceName, local.Format(time.RFC3339), got)
				}
			}
		}
	}
}

func dateString(t time.Time) string { return t.Format("2006-01-02") }

// covers: FIN-41
func TestAtOrAfterCutoff(t *testing.T) {
	cases := []struct {
		name   string
		source string
		ts     time.Time
		want   bool
	}{
		{
			name:   "before cutoff",
			source: "psp_a",
			ts:     utc(t, "2026-06-15T20:59:59Z"), // 16:59:59 EDT
			want:   false,
		},
		{
			name:   "exactly at cutoff",
			source: "psp_a",
			ts:     utc(t, "2026-06-15T21:00:00Z"), // 17:00 EDT
			want:   true,
		},
		{
			name:   "after cutoff",
			source: "psp_a",
			ts:     utc(t, "2026-06-15T21:00:01Z"),
			want:   true,
		},
		{
			name:   "DST spring-forward exactly at cutoff",
			source: "seven_day",
			ts:     utc(t, "2026-03-08T06:30:00Z"), // 01:30 EST, jump at 02:00
			want:   true,
		},
		{
			name:   "DST spring-forward before cutoff",
			source: "seven_day",
			ts:     utc(t, "2026-03-08T06:29:59Z"),
			want:   false,
		},
		{
			name:   "DST fall-back just before cutoff",
			source: "seven_day_1700",
			ts:     utc(t, "2026-11-01T21:59:00Z"), // 16:59 EST, after the repeated hour
			want:   false,
		},
		{
			name:   "DST fall-back exactly at cutoff",
			source: "seven_day_1700",
			ts:     utc(t, "2026-11-01T22:00:00Z"), // 17:00 EST
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calendar := mustCalendar(t, tc.source)
			if got := calendar.AtOrAfterCutoff(tc.ts); got != tc.want {
				t.Fatalf("AtOrAfterCutoff(%s) = %v, want %v", tc.ts, got, tc.want)
			}
		})
	}
}

// covers: FIN-41
func TestBusinessDayArithmetic(t *testing.T) {
	psp := mustCalendar(t, "psp_a")
	leap := mustCalendar(t, "utc_leap")

	cases := []struct {
		name  string
		cal   bizdays.Calendar
		start time.Time
		n     int
		want  string
	}{
		{
			name:  "Friday plus one rolls over the weekend",
			cal:   psp,
			start: time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC),
			n:     1,
			want:  "2026-06-22",
		},
		{
			name:  "weekend start counts the next business day first",
			cal:   psp,
			start: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
			n:     1,
			want:  "2026-06-22",
		},
		{
			name:  "configured holiday is skipped",
			cal:   psp,
			start: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
			n:     1,
			want:  "2026-07-06",
		},
		{
			name:  "thanksgiving holiday is skipped",
			cal:   psp,
			start: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
			n:     1,
			want:  "2026-11-27",
		},
		{
			name:  "leap day is a business day",
			cal:   leap,
			start: time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC),
			n:     1,
			want:  "2024-02-29",
		},
		{
			name:  "day after leap day rolls over the weekend",
			cal:   leap,
			start: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
			n:     1,
			want:  "2024-03-04",
		},
		{
			name:  "DST spring-forward weekend is not a business day",
			cal:   psp,
			start: time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC),
			n:     1,
			want:  "2026-03-09",
		},
		{
			name:  "DST spring-forward does not shift a business date",
			cal:   psp,
			start: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
			n:     2,
			want:  "2026-03-11",
		},
		{
			name:  "DST fall-back weekend is not a business day",
			cal:   psp,
			start: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC),
			n:     1,
			want:  "2026-11-02",
		},
		{
			name:  "DST fall-back does not shift a business date",
			cal:   psp,
			start: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
			n:     2,
			want:  "2026-11-04",
		},
		{
			name:  "zero business days returns the start date",
			cal:   psp,
			start: time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC),
			n:     0,
			want:  "2026-06-19",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cal.AddBusinessDays(tc.start, tc.n)
			if dateString(got) != tc.want {
				t.Fatalf("AddBusinessDays(%s, %d) = %s, want %s",
					dateString(tc.start), tc.n, dateString(got), tc.want)
			}
			if got.Location() != time.UTC || got.Hour() != 0 || got.Minute() != 0 {
				t.Fatalf("AddBusinessDays returned %v; want midnight UTC", got)
			}
		})
	}
}

// covers: FIN-41
func TestValueDateCutoffAndHolidayEdges(t *testing.T) {
	calendar := mustCalendar(t, "psp_a")
	cases := []struct {
		name string
		ts   time.Time
		want string
	}{
		{
			name: "before cutoff keeps the local date",
			ts:   utc(t, "2026-06-15T20:59:59Z"),
			want: "2026-06-15",
		},
		{
			name: "exactly at cutoff rolls forward",
			ts:   utc(t, "2026-06-15T21:00:00Z"),
			want: "2026-06-16",
		},
		{
			name: "holiday rolls forward to Monday",
			ts:   utc(t, "2026-07-03T15:00:00Z"),
			want: "2026-07-06",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dateString(calendar.ValueDate(tc.ts)); got != tc.want {
				t.Fatalf("ValueDate(%s) = %s, want %s", tc.ts, got, tc.want)
			}
		})
	}
}
