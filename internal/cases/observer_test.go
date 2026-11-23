package cases

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCaseAgeSecondsUsesResolvedAt(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	resolved := created.Add(90 * time.Minute)
	now := created.Add(5 * time.Hour)

	casesUnderTest := []struct {
		name string
		c    Case
		want float64 //floatlint:allow the Prometheus histogram boundary is float64 seconds
	}{
		{"open uses clock", Case{Status: StatusOpen, CreatedAt: created}, now.Sub(created).Seconds()},
		{"resolved uses resolved_at", Case{
			Status: StatusResolved, CreatedAt: created, ResolvedAt: &resolved,
		}, 90 * 60},
		{"resolved without timestamp uses clock", Case{Status: StatusResolved, CreatedAt: created}, now.Sub(created).Seconds()},
		{"negative age clamps to zero", Case{Status: StatusOpen, CreatedAt: now.Add(time.Hour)}, 0},
	}
	for _, tc := range casesUnderTest {
		t.Run(tc.name, func(t *testing.T) {
			if got := caseAgeSeconds(tc.c, now); got != tc.want {
				t.Fatalf("caseAgeSeconds = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCaseErrorCodeMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"empty queue is not an error", ErrNoCase, ""},
		{"canceled is timeout", context.Canceled, ErrorCodeTimeout},
		{"deadline is timeout", fmt.Errorf("wrap: %w", context.DeadlineExceeded), ErrorCodeTimeout},
		{"case not found", ErrCaseNotFound, ErrorCodeNotFound},
		{"break not found", fmt.Errorf("wrap: %w", ErrBreakNotFound), ErrorCodeNotFound},
		{"stale is conflict", ErrStale, ErrorCodeConflict},
		{"not lease owner is conflict", ErrNotLeaseOwner, ErrorCodeConflict},
		{"not leased is conflict", ErrNotLeased, ErrorCodeConflict},
		{"lease expired is conflict", ErrLeaseExpired, ErrorCodeConflict},
		{"invalid request", ErrInvalidRequest, ErrorCodeInvalidRequest},
		{"invalid status", ErrInvalidStatus, ErrorCodeInvalidRequest},
		{"invalid ttl", ErrInvalidTTL, ErrorCodeInvalidRequest},
		{"no breaks", ErrNoBreaks, ErrorCodeInvalidRequest},
		{"break in case", ErrBreakInCase, ErrorCodeInvalidRequest},
		{"break not open", ErrBreakNotOpen, ErrorCodeInvalidRequest},
		{"unknown is internal", errors.New("boom"), ErrorCodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := caseErrorCode(tc.err); got != tc.want {
				t.Fatalf("caseErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
