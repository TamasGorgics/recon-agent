package limits

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

func TestSumDebitLines(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		currency string
		want     int64
		wantErr  string
	}{
		{"missing", "", "USD", 0, ""},
		{"null", "null", "USD", 0, ""},
		{"empty array", "[]", "USD", 0, ""},
		{"debit only", `[{"amount_minor":100,"currency":"USD","direction":"debit"}]`, "USD", 100, ""},
		{"mixed lines", `[
			{"amount_minor":100,"currency":"USD","direction":"debit"},
			{"amount_minor":50,"currency":"USD","direction":"credit"},
			{"amount_minor":70,"currency":"EUR","direction":"debit"}
		]`, "USD", 100, ""},
		{"multiple debits", `[
			{"amount_minor":100,"currency":"USD","direction":"debit"},
			{"amount_minor":250,"currency":"USD","direction":"debit"}
		]`, "USD", 350, ""},
		{"zero amount", `[{"amount_minor":0,"currency":"USD","direction":"debit"}]`, "USD", 0, ""},
		{"object lines", `{}`, "USD", 0, "not an array"},
		{"line not object", `[1]`, "USD", 0, "not an object"},
		{"missing amount", `[{"currency":"USD","direction":"debit"}]`, "USD", 0, "missing"},
		{"string amount", `[{"amount_minor":"100","currency":"USD","direction":"debit"}]`, "USD", 0, "not an integer"},
		{"float amount", `[{"amount_minor":1.5,"currency":"USD","direction":"debit"}]`, "USD", 0, "not an integer"},
		{"negative amount", `[{"amount_minor":-1,"currency":"USD","direction":"debit"}]`, "USD", 0, "negative"},
		{"missing currency", `[{"amount_minor":100,"direction":"debit"}]`, "USD", 0, "missing"},
		{"missing direction", `[{"amount_minor":100,"currency":"USD"}]`, "USD", 0, "missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := sumDebitLines([]byte(test.raw), test.currency)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("sumDebitLines() error = %v, want nil", err)
				}
				if got != test.want {
					t.Fatalf("sumDebitLines() = %d, want %d", got, test.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("sumDebitLines() = %d, want error containing %q", got, test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("sumDebitLines() error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestSumDebitLinesOverflow(t *testing.T) {
	max := int64(^uint64(0) >> 1)
	raw := `[
		{"amount_minor":` + strconv.FormatInt(max, 10) + `,"currency":"USD","direction":"debit"},
		{"amount_minor":1,"currency":"USD","direction":"debit"}
	]`
	if _, err := sumDebitLines([]byte(raw), "USD"); err == nil {
		t.Fatal("sumDebitLines accepted an overflowing total")
	}
}

func TestExceedsDailyCap(t *testing.T) {
	tests := []struct {
		name       string
		cumulative int64
		value      int64
		capMinor   int64
		want       bool
	}{
		{"empty", 0, 100, 10_000, false},
		{"below", 8_000, 1_000, 10_000, false},
		{"exactly at cap", 9_000, 1_000, 10_000, false},
		{"above cap", 9_000, 1_001, 10_000, true},
		{"cumulative at cap with zero value", 10_000, 0, 10_000, true},
		{"cumulative over cap with zero value", 10_001, 0, 10_000, true},
		{"zero cap", 0, 0, 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exceedsDailyCap(test.cumulative, test.value, test.capMinor); got != test.want {
				t.Fatalf("exceedsDailyCap(%d, %d, %d) = %v, want %v",
					test.cumulative, test.value, test.capMinor, got, test.want)
			}
		})
	}
}

func TestBreakRateExceeded(t *testing.T) {
	tests := []struct {
		name      string
		breaks    int64
		matches   int64
		threshold int
		minSample int
		want      bool
	}{
		{"below min sample at 100 percent", 19, 0, 1_000, 20, false},
		{"at min sample at 100 percent", 20, 0, 1_000, 20, true},
		{"normal mix below threshold", 1, 19, 1_000, 20, false},
		{"exactly at threshold", 2, 18, 1_000, 20, true},
		{"above threshold", 3, 17, 1_000, 20, true},
		{"below min sample with matches", 19, 0, 1, 20, false},
		{"zero events", 0, 0, 1_000, 20, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := breakRateExceeded(test.breaks, test.matches, test.threshold, test.minSample); got != test.want {
				t.Fatalf("breakRateExceeded(%d, %d, %d, %d) = %v, want %v",
					test.breaks, test.matches, test.threshold, test.minSample, got, test.want)
			}
		})
	}
}

func TestNewServiceRejectsInvalidOptions(t *testing.T) {
	if _, err := NewService(Options{}); err == nil {
		t.Fatal("NewService accepted nil pool and clock")
	}
	pool := &pgxpool.Pool{}
	if _, err := NewService(Options{Pool: pool}); err == nil {
		t.Fatal("NewService accepted a nil clock")
	}
	if _, err := NewService(Options{
		Pool:  pool,
		Clock: clock.NewFake(testClockTime),
		Config: config.AutoApplyLimitsConfig{
			BreakRateThresholdBPS: 10_001,
		},
	}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("NewService invalid config error = %v, want ErrInvalidOptions", err)
	}
}

func TestCheckTierRouting(t *testing.T) {
	// A zero Service is safe here: tier routing happens before any pool use, so
	// these cases also prove an invalid tier never reaches the switch or
	// cumulative queries.
	svc := &Service{}
	tests := []struct {
		name        string
		tier        policy.Tier
		wantAllowed bool
		wantTier    policy.Tier
		wantReason  string
	}{
		{"T1 passthrough", policy.TierT1, true, policy.TierT1, ReasonNotT0},
		{"T2 passthrough", policy.TierT2, true, policy.TierT2, ReasonNotT0},
		{"empty tier", "", false, policy.TierT1, ReasonInvalidTier},
		{"unknown tier", "T9", false, policy.TierT1, ReasonInvalidTier},
		{"lower-case tier", "t0", false, policy.TierT1, ReasonInvalidTier},
		{"padded tier", "T0 ", false, policy.TierT1, ReasonInvalidTier},
		{"truncated tier", "T", false, policy.TierT1, ReasonInvalidTier},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := svc.Check(context.Background(), Candidate{
				Currency:   "USD",
				ValueMinor: 100,
				Tier:       test.tier,
			})
			if err != nil {
				t.Fatalf("Check(%q): %v", test.tier, err)
			}
			if result.Allowed != test.wantAllowed || result.Tier != test.wantTier || result.Reason != test.wantReason {
				t.Fatalf("Check(%q) = %+v, want allowed=%v tier=%s reason=%s",
					test.tier, result, test.wantAllowed, test.wantTier, test.wantReason)
			}
			if result.BreakerTripped || result.SwitchEnabled {
				t.Fatalf("Check(%q) = %+v, want no breaker interaction", test.tier, result)
			}
		})
	}
}

var testClockTime = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
