package ingest

import (
	"fmt"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// Calendar is one source's configured business calendar (FIN-41): an explicit
// IANA timezone, a local settlement cutoff, business weekdays, and holidays.
// It is immutable after construction.
type Calendar struct {
	source         string
	location       *time.Location
	cutoffHour     int
	cutoffMinute   int
	cutoffSecond   int
	businessDays   [7]bool
	holidayDateKey map[string]struct{}
}

// NewCalendar builds a Calendar from validated per-source config.
func NewCalendar(source string, cfg config.SourceConfig) (Calendar, error) {
	location, err := cfg.Location()
	if err != nil {
		return Calendar{}, fmt.Errorf("ingest: source %q: %w", source, err)
	}
	hour, minute, second, err := cfg.CutoffClock()
	if err != nil {
		return Calendar{}, fmt.Errorf("ingest: source %q: %w", source, err)
	}
	weekdays, err := cfg.Weekdays()
	if err != nil {
		return Calendar{}, fmt.Errorf("ingest: source %q: %w", source, err)
	}
	holidays, err := cfg.HolidayKeys()
	if err != nil {
		return Calendar{}, fmt.Errorf("ingest: source %q: %w", source, err)
	}
	return Calendar{
		source:         source,
		location:       location,
		cutoffHour:     hour,
		cutoffMinute:   minute,
		cutoffSecond:   second,
		businessDays:   weekdays,
		holidayDateKey: holidays,
	}, nil
}

// Sources is the configured calendar per source name.
type Sources map[string]Calendar

// NewSources builds every configured source calendar.
func NewSources(cfg config.IngestConfig) (Sources, error) {
	sources := make(Sources, len(cfg.Sources))
	for name, sourceCfg := range cfg.Sources {
		calendar, err := NewCalendar(name, sourceCfg)
		if err != nil {
			return nil, err
		}
		sources[name] = calendar
	}
	return sources, nil
}

// Calendar returns the configured calendar for source.
func (s Sources) Calendar(source string) (Calendar, bool) {
	calendar, ok := s[source]
	return calendar, ok
}

// ValueDate derives the business value date for ts in source's timezone
// (FIN-42): before the local cutoff the candidate date is the same local date,
// at or after the cutoff it is the next local date. Weekends and configured
// holidays then roll forward to the next business day. The returned DATE is
// midnight UTC; timestamps themselves are never rewritten.
func (s Sources) ValueDate(source string, ts time.Time) (time.Time, error) {
	calendar, ok := s[source]
	if !ok {
		return time.Time{}, fmt.Errorf("%w: %q", ErrUnknownSource, source)
	}
	return calendar.ValueDate(ts), nil
}

// Source returns the configured source name.
func (c Calendar) Source() string { return c.source }

// Location returns the configured timezone.
func (c Calendar) Location() *time.Location { return c.location }

// ValueDate derives the business value date for ts (see Sources.ValueDate).
//
// The comparison is on the source-local wall clock, not on an absolute
// instant: `time.Date` can normalize a cutoff that falls inside a DST gap, so
// comparing hour/minute/second is the only representation that behaves the
// same on spring-forward and fall-back days.
func (c Calendar) ValueDate(ts time.Time) time.Time {
	local := ts.In(c.location)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.location)
	afterCutoff := local.Hour() > c.cutoffHour ||
		(local.Hour() == c.cutoffHour && local.Minute() > c.cutoffMinute) ||
		(local.Hour() == c.cutoffHour && local.Minute() == c.cutoffMinute && local.Second() >= c.cutoffSecond)
	if afterCutoff {
		candidate = candidate.AddDate(0, 0, 1)
	}
	// Roll weekends and configured holidays forward. A validated calendar
	// always contains at least one business weekday; the bound is only a guard
	// against an unrepresentable holiday list.
	const maxRollDays = 3660
	for day := 0; !c.isBusinessDay(candidate) && day < maxRollDays; day++ {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return DateOnly(candidate)
}

// isBusinessDay reports whether date (in the source timezone) is a configured
// business day and not a holiday.
func (c Calendar) isBusinessDay(date time.Time) bool {
	if !c.businessDays[date.Weekday()] {
		return false
	}
	_, holiday := c.holidayDateKey[date.Format("2006-01-02")]
	return !holiday
}

// DateOnly normalizes a timestamp or DATE to midnight UTC, the in-memory form
// used for every DATE column.
func DateOnly(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
