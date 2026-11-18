package ingest

import (
	"fmt"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/bizdays"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

// Calendar is the FIN-41 business calendar shared with the case lifecycle.
// It is an alias so the ingest package keeps its existing public shape while
// all arithmetic lives in internal/bizdays.
type Calendar = bizdays.Calendar

// Sources is the configured calendar per source name.
type Sources map[string]Calendar

// NewCalendar builds a Calendar from validated per-source config.
func NewCalendar(source string, cfg config.SourceConfig) (Calendar, error) {
	calendar, err := bizdays.NewCalendar(source, cfg)
	if err != nil {
		return Calendar{}, fmt.Errorf("ingest: %w", err)
	}
	return calendar, nil
}

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

// DateOnly normalizes a timestamp or DATE to midnight UTC, the in-memory form
// used for every DATE column.
func DateOnly(t time.Time) time.Time {
	return bizdays.DateOnly(t)
}
