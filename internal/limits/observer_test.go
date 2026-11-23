package limits

import (
	"context"
	"errors"
	"testing"
)

func TestCapUsageRatio(t *testing.T) {
	tests := []struct {
		name       string
		cumulative int64
		capMinor   int64
		want       float64 //floatlint:allow the Prometheus gauge boundary is float64 ratio
		wantOK     bool
	}{
		{"zero cap", 0, 0, 0, false},
		{"negative cap", 10, -1, 0, false},
		{"zero cumulative", 0, 10_000, 0, true},
		{"sixty percent", 6_000, 10_000, 0.6, true},
		{"negative cumulative", -5, 10_000, 0, true},
		{"exactly at cap", 10_000, 10_000, 1, true},
		{"over cap", 12_000, 10_000, 1, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := capUsageRatio(test.cumulative, test.capMinor)
			if ok != test.wantOK {
				t.Fatalf("capUsageRatio(%d, %d) ok = %v, want %v",
					test.cumulative, test.capMinor, ok, test.wantOK)
			}
			if got != test.want {
				t.Fatalf("capUsageRatio(%d, %d) = %v, want %v",
					test.cumulative, test.capMinor, got, test.want)
			}
		})
	}
}

func TestCheckErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"context canceled", context.Canceled, ErrorCodeTimeout},
		{"deadline", context.DeadlineExceeded, ErrorCodeTimeout},
		{"switch missing", ErrSwitchMissing, ErrorCodeInternal},
		{"unknown", errors.New("limits: query failed"), ErrorCodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := checkErrorCode(test.err); got != test.want {
				t.Fatalf("checkErrorCode(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}

func TestObserveCheckNilObserverIsSafe(t *testing.T) {
	var svc *Service
	svc.observeCheck("USD", Result{CapMinor: 10}, nil)
	(&Service{}).observeCheck("USD", Result{Reason: ReasonDailyCapExceeded}, nil)
}
