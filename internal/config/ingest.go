package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// IngestConfig is the per-source ingestion configuration (FIN-41). Each source
// names the timezone and settlement cutoff used to derive business value dates
// (FIN-42) and the business-day calendar used to roll weekends and holidays.
//
// YAML shape:
//
//	ingest:
//	  sources:
//	    psp_a:
//	      timezone: America/New_York
//	      cutoff: "17:00"
//	      business_days: [mon, tue, wed, thu, fri]
//	      holidays: ["2026-01-01", "2026-07-03"]
type IngestConfig struct {
	Sources map[string]SourceConfig `yaml:"sources"`
}

// SourceConfig is one settlement source's business calendar. Timezone and
// Cutoff are required for every configured source. BusinessDays defaults to
// Monday through Friday when omitted; Holidays are additional non-business
// dates in the source timezone.
type SourceConfig struct {
	Timezone     string   `yaml:"timezone"`
	Cutoff       string   `yaml:"cutoff"`
	BusinessDays []string `yaml:"business_days"`
	Holidays     []string `yaml:"holidays"`
}

// DefaultBusinessDayNames is the weekday set used when a source config omits
// business_days.
var DefaultBusinessDayNames = []string{"mon", "tue", "wed", "thu", "fri"}

// weekdayNames maps the accepted config spelling to time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

// Validate checks every configured source. A config without sources is valid:
// commands that do not ingest have no reason to configure one.
func (c IngestConfig) Validate() error {
	for _, name := range sortedSourceNames(c.Sources) {
		if err := c.Sources[name].Validate(name); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks one source's timezone, cutoff, business days, and holidays.
// name is only used to make the error messages actionable.
func (s SourceConfig) Validate(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("config: ingest source name is empty")
	}
	if _, err := s.Location(); err != nil {
		return fmt.Errorf("config: ingest source %q: %w", name, err)
	}
	if _, _, _, err := s.CutoffClock(); err != nil {
		return fmt.Errorf("config: ingest source %q: %w", name, err)
	}
	if _, err := s.Weekdays(); err != nil {
		return fmt.Errorf("config: ingest source %q: %w", name, err)
	}
	if _, err := s.HolidayKeys(); err != nil {
		return fmt.Errorf("config: ingest source %q: %w", name, err)
	}
	return nil
}

// Location resolves the source's explicit IANA timezone.
func (s SourceConfig) Location() (*time.Location, error) {
	if strings.TrimSpace(s.Timezone) == "" {
		return nil, errors.New("timezone is required")
	}
	location, err := time.LoadLocation(strings.TrimSpace(s.Timezone))
	if err != nil {
		return nil, fmt.Errorf("timezone %q is not a valid IANA location: %w", s.Timezone, err)
	}
	return location, nil
}

// CutoffClock parses the local settlement cutoff. Accepted formats are
// "HH:MM" and "HH:MM:SS" in 24-hour time.
func (s SourceConfig) CutoffClock() (hour, minute, second int, err error) {
	value := strings.TrimSpace(s.Cutoff)
	if value == "" {
		return 0, 0, 0, errors.New("cutoff is required (for example 17:00)")
	}
	for _, layout := range []string{"15:04", "15:04:05"} {
		parsed, parseErr := time.Parse(layout, value)
		if parseErr == nil {
			return parsed.Hour(), parsed.Minute(), parsed.Second(), nil
		}
	}
	return 0, 0, 0, fmt.Errorf("cutoff %q is not HH:MM or HH:MM:SS", s.Cutoff)
}

// Weekdays returns the configured business-day set indexed by time.Weekday.
// An omitted list means Monday through Friday. At least one day is required;
// otherwise a value date could never settle.
func (s SourceConfig) Weekdays() ([7]bool, error) {
	names := s.BusinessDays
	if len(names) == 0 {
		names = DefaultBusinessDayNames
	}
	var set [7]bool
	for _, raw := range names {
		key := strings.ToLower(strings.TrimSpace(raw))
		weekday, ok := weekdayNames[key]
		if !ok {
			return set, fmt.Errorf("business day %q is not one of sun, mon, tue, wed, thu, fri, sat", raw)
		}
		set[weekday] = true
	}
	for _, business := range set {
		if business {
			return set, nil
		}
	}
	return set, errors.New("business_days must contain at least one day")
}

// HolidayKeys returns the configured holidays as YYYY-MM-DD keys in the source
// timezone.
func (s SourceConfig) HolidayKeys() (map[string]struct{}, error) {
	holidays := make(map[string]struct{}, len(s.Holidays))
	for _, raw := range s.Holidays {
		value := strings.TrimSpace(raw)
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			return nil, fmt.Errorf("holiday %q is not YYYY-MM-DD", raw)
		}
		holidays[parsed.Format("2006-01-02")] = struct{}{}
	}
	return holidays, nil
}

func sortedSourceNames(sources map[string]SourceConfig) []string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
