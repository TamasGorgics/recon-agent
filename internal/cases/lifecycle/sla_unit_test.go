package lifecycle

import (
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/bizdays"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

func calendarForTest(t *testing.T, cfg config.SourceConfig) bizdays.Calendar {
	t.Helper()
	calendar, err := bizdays.NewCalendar("test", cfg)
	if err != nil {
		t.Fatalf("NewCalendar: %v", err)
	}
	return calendar
}

// covers: FIN-41
func TestSLADueDateCutoffWeekendsHolidaysAndDST(t *testing.T) {
	newYork := calendarForTest(t, config.SourceConfig{
		Timezone: "America/New_York",
		Cutoff:   "17:00",
		Holidays: []string{"2026-11-11"},
	})
	utc := calendarForTest(t, config.SourceConfig{
		Timezone: "UTC",
		Cutoff:   "17:00",
	})

	ts := func(value string) time.Time {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatalf("parse %q: %v", value, err)
		}
		return parsed
	}

	cases := []struct {
		name         string
		calendar     bizdays.Calendar
		anchor       time.Time
		businessDays int
		want         string
	}{
		{
			name:         "before cutoff starts on the anchor date",
			calendar:     newYork,
			anchor:       ts("2026-11-02T15:00:00Z"), // Mon 10:00 EST
			businessDays: 1,
			want:         "2026-11-03",
		},
		{
			name:         "exactly at cutoff starts on the next business day",
			calendar:     newYork,
			anchor:       ts("2026-11-02T22:00:00Z"), // Mon 17:00 EST
			businessDays: 1,
			want:         "2026-11-04",
		},
		{
			name:         "Friday after cutoff starts Monday and counts after it",
			calendar:     newYork,
			anchor:       ts("2026-11-06T23:00:00Z"), // Fri 18:00 EST
			businessDays: 1,
			want:         "2026-11-10",
		},
		{
			name:         "Saturday before cutoff counts the next business day first",
			calendar:     newYork,
			anchor:       ts("2026-11-07T15:00:00Z"), // Sat 10:00 EST
			businessDays: 1,
			want:         "2026-11-09",
		},
		{
			name:         "configured holiday is skipped",
			calendar:     newYork,
			anchor:       ts("2026-11-06T15:00:00Z"), // Fri 10:00 EST
			businessDays: 3,
			want:         "2026-11-12", // Mon 9, Tue 10, Wed 11 holiday, Thu 12
		},
		{
			name:         "DST spring-forward week is date-only arithmetic",
			calendar:     newYork,
			anchor:       ts("2026-03-06T22:00:00Z"), // Fri 17:00 EST, clocks jump Sunday
			businessDays: 1,
			want:         "2026-03-10", // start Mon 9, due Tue 10
		},
		{
			name:         "DST fall-back does not shift the due date",
			calendar:     newYork,
			anchor:       ts("2026-10-30T16:00:00Z"), // Fri 12:00 EDT
			businessDays: 2,
			want:         "2026-11-03", // Mon 2, Tue 3
		},
		{
			name:         "leap day is a business day",
			calendar:     utc,
			anchor:       ts("2024-02-28T10:00:00Z"),
			businessDays: 1,
			want:         "2024-02-29",
		},
		{
			name:         "day after leap day rolls over the weekend",
			calendar:     utc,
			anchor:       ts("2024-03-01T10:00:00Z"),
			businessDays: 1,
			want:         "2024-03-04",
		},
		{
			name:         "UTC instant maps to the source-local date",
			calendar:     newYork,
			anchor:       ts("2026-11-02T02:00:00Z"), // Sun 21:00 EST, after cutoff
			businessDays: 1,
			want:         "2026-11-03", // starts Mon 2, due Tue 3
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slaDueDate(tc.calendar, tc.anchor, tc.businessDays)
			if got.Format("2006-01-02") != tc.want {
				t.Fatalf("slaDueDate(%s, %d) = %s, want %s",
					tc.anchor, tc.businessDays, got.Format("2006-01-02"), tc.want)
			}
			if got.Location() != time.UTC || got.Hour() != 0 || got.Minute() != 0 {
				t.Fatalf("slaDueDate returned %v; want a midnight UTC DATE", got)
			}
		})
	}
}

// covers: FIN-41
func TestClassifySLABreaks(t *testing.T) {
	ledger := slaBreak{status: "open", side: "ledger_only", source: "psp_a"}
	cases := []struct {
		name     string
		breaks   []slaBreak
		source   string
		mixed    bool
		eligible bool
	}{
		{
			name:     "all ledger-only is eligible",
			breaks:   []slaBreak{ledger, {status: "in_case", side: "ledger_only", source: "psp_a"}},
			source:   "psp_a",
			eligible: true,
		},
		{
			name:     "void non-ledger break does not exclude",
			breaks:   []slaBreak{ledger, {status: "void", side: "external_only", source: "psp_a"}},
			source:   "psp_a",
			eligible: true,
		},
		{
			name:     "non-void external-only excludes",
			breaks:   []slaBreak{ledger, {status: "open", side: "external_only", source: "psp_a"}},
			eligible: false,
		},
		{
			name:     "amount mismatch excludes",
			breaks:   []slaBreak{{status: "in_case", side: "amount_mismatch", source: "psp_a"}},
			eligible: false,
		},
		{
			name:     "mixed sources are flagged",
			breaks:   []slaBreak{ledger, {status: "open", side: "ledger_only", source: "bank_x"}},
			mixed:    true,
			eligible: true,
		},
		{
			name:     "no breaks is ineligible",
			breaks:   nil,
			eligible: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, mixed, eligible := classifySLABreaks(tc.breaks)
			if source != tc.source || mixed != tc.mixed || eligible != tc.eligible {
				t.Fatalf("classifySLABreaks = (%q, %v, %v), want (%q, %v, %v)",
					source, mixed, eligible, tc.source, tc.mixed, tc.eligible)
			}
		})
	}
}

// covers: FIN-41
func TestEarliestDetectedAtIncludesVoidBreaks(t *testing.T) {
	early := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	late := early.Add(48 * time.Hour)
	got := earliestDetectedAt([]slaBreak{
		{status: "open", detectedAt: late},
		{status: "void", detectedAt: early},
	})
	if !got.Equal(early) {
		t.Fatalf("earliestDetectedAt = %s, want %s", got, early)
	}
}

// covers: FIN-41
func TestNewServiceSLAConfigValidation(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	service, err := NewService(Options{DB: stubBeginner{}, Clock: fake})
	if err != nil {
		t.Fatalf("NewService default: %v", err)
	}
	if got, want := service.slaBusinessDays, config.DefaultMissingSettlementSLABusinessDays; got != want {
		t.Fatalf("default SLA = %d, want %d", got, want)
	}

	for _, days := range []int{1, 365} {
		service, err := NewService(Options{
			DB: stubBeginner{}, Clock: fake, MissingSettlementSLABusinessDays: days,
		})
		if err != nil {
			t.Fatalf("NewService(%d): %v", days, err)
		}
		if service.slaBusinessDays != days {
			t.Fatalf("SLA = %d, want %d", service.slaBusinessDays, days)
		}
	}

	for _, days := range []int{-1, 366} {
		_, err := NewService(Options{
			DB: stubBeginner{}, Clock: fake, MissingSettlementSLABusinessDays: days,
		})
		if err == nil {
			t.Fatalf("NewService accepted SLA %d", days)
		}
	}
}
