package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// covers: FIN-41
func TestLoadFileIngestSection(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "ingest:\n" +
		"  sources:\n" +
		"    psp_a:\n" +
		"      timezone: America/New_York\n" +
		"      cutoff: \"17:00\"\n" +
		"      business_days: [mon, tue, wed, thu, fri]\n" +
		"      holidays: [\"2026-01-01\", \"2026-07-03\"]\n" +
		"    bank_x:\n" +
		"      timezone: Europe/Stockholm\n" +
		"      cutoff: \"15:30:00\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if err := cfg.Ingest.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	psp, ok := cfg.Ingest.Sources["psp_a"]
	if !ok {
		t.Fatal("psp_a source is missing")
	}
	location, err := psp.Location()
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if got, want := location.String(), "America/New_York"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	hour, minute, second, err := psp.CutoffClock()
	if err != nil {
		t.Fatalf("CutoffClock: %v", err)
	}
	if hour != 17 || minute != 0 || second != 0 {
		t.Errorf("cutoff = %02d:%02d:%02d, want 17:00:00", hour, minute, second)
	}
	weekdays, err := psp.Weekdays()
	if err != nil {
		t.Fatalf("Weekdays: %v", err)
	}
	for _, weekday := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday} {
		if !weekdays[weekday] {
			t.Errorf("weekday %s is not a business day", weekday)
		}
	}
	for _, weekday := range []time.Weekday{time.Saturday, time.Sunday} {
		if weekdays[weekday] {
			t.Errorf("weekday %s must not be a business day", weekday)
		}
	}
	holidays, err := psp.HolidayKeys()
	if err != nil {
		t.Fatalf("HolidayKeys: %v", err)
	}
	if len(holidays) != 2 {
		t.Errorf("holidays = %v, want 2 entries", holidays)
	}
	if _, ok := holidays["2026-07-03"]; !ok {
		t.Errorf("holidays = %v, want 2026-07-03", holidays)
	}

	bank := cfg.Ingest.Sources["bank_x"]
	hour, minute, second, err = bank.CutoffClock()
	if err != nil {
		t.Fatalf("bank CutoffClock: %v", err)
	}
	if hour != 15 || minute != 30 || second != 0 {
		t.Errorf("bank cutoff = %02d:%02d:%02d, want 15:30:00", hour, minute, second)
	}
}

// covers: FIN-41
func TestSourceConfigDefaultsBusinessDays(t *testing.T) {
	source := SourceConfig{Timezone: "UTC", Cutoff: "17:00"}
	weekdays, err := source.Weekdays()
	if err != nil {
		t.Fatalf("Weekdays: %v", err)
	}
	if !weekdays[time.Monday] || !weekdays[time.Friday] {
		t.Error("default business days do not include the working week")
	}
	if weekdays[time.Saturday] || weekdays[time.Sunday] {
		t.Error("default business days include the weekend")
	}
}

// covers: FIN-41
func TestSourceConfigValidationErrors(t *testing.T) {
	cases := []struct {
		name   string
		source SourceConfig
		want   string
	}{
		{"missing timezone", SourceConfig{Cutoff: "17:00"}, "timezone"},
		{"bad timezone", SourceConfig{Timezone: "Mars/Olympus", Cutoff: "17:00"}, "timezone"},
		{"missing cutoff", SourceConfig{Timezone: "UTC"}, "cutoff"},
		{"bad cutoff hour", SourceConfig{Timezone: "UTC", Cutoff: "25:00"}, "cutoff"},
		{"bad cutoff minute", SourceConfig{Timezone: "UTC", Cutoff: "17:60"}, "cutoff"},
		{"bad cutoff format", SourceConfig{Timezone: "UTC", Cutoff: "5pm"}, "cutoff"},
		{"bad business day", SourceConfig{Timezone: "UTC", Cutoff: "17:00", BusinessDays: []string{"funday"}}, "business day"},
		{"bad holiday", SourceConfig{Timezone: "UTC", Cutoff: "17:00", Holidays: []string{"2026-13-01"}}, "holiday"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.source.Validate("psp_a")
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.source)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// covers: FIN-41
func TestIngestConfigValidateEmpty(t *testing.T) {
	var cfg IngestConfig
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(empty) = %v, want nil", err)
	}
}
