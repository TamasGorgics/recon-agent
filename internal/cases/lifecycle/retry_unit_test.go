package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// TestAttemptCapMatchesCasesDefault pins the shared counter: the lifecycle
// retry cap must stay equal to the case service's agent-failure cap. There is
// deliberately no second counter and no config.
func TestAttemptCapMatchesCasesDefault(t *testing.T) {
	if AttemptCap != cases.DefaultMaxAttempts {
		t.Fatalf("AttemptCap = %d, want cases.DefaultMaxAttempts = %d",
			AttemptCap, cases.DefaultMaxAttempts)
	}
}

func TestRetryableProposalStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
		ok     bool
	}{
		{name: "rejected", status: "rejected", want: "rejected", ok: true},
		{name: "expired", status: "expired", want: "expired", ok: true},
		{name: "surrounding whitespace", status: "  expired ", want: "expired", ok: true},
		{name: "proposed", status: "proposed"},
		{name: "pending approval", status: "pending_approval"},
		{name: "approved", status: "approved"},
		{name: "auto approved", status: "auto_approved"},
		{name: "applied", status: "applied"},
		{name: "apply failed", status: "apply_failed"},
		{name: "superseded", status: "superseded"},
		{name: "empty", status: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := retryableProposalStatus(tc.status)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("retryableProposalStatus(%q) = %q/%v, want %q/%v",
					tc.status, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestOnProposalClosedIgnoresUnrelatedStatusWithoutDatabase(t *testing.T) {
	service, err := NewService(Options{
		DB:    stubBeginner{},
		Clock: clock.NewFake(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	for _, status := range []string{
		"proposed", "pending_approval", "approved", "auto_approved",
		"applied", "apply_failed", "superseded", "",
	} {
		// A nil transaction and malformed id prove the status check happens
		// before any database access.
		if err := service.OnProposalClosed(context.Background(), nil,
			"not-a-uuid", status, ""); err != nil {
			t.Fatalf("OnProposalClosed status %q = %v, want nil no-op", status, err)
		}
	}
}

func TestOnProposalClosedValidation(t *testing.T) {
	service, err := NewService(Options{
		DB:    stubBeginner{},
		Clock: clock.NewFake(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	tests := []struct {
		name   string
		status string
		id     string
		setTx  func() pgx.Tx
		want   error
	}{
		{name: "nil transaction", status: "rejected", id: testCaseID,
			setTx: func() pgx.Tx { return nil }, want: ErrNoTransaction},
		{name: "typed nil transaction", status: "expired", id: testCaseID,
			setTx: func() pgx.Tx { return (*connTx)(nil) }, want: ErrNoTransaction},
		{name: "malformed proposal id", status: "rejected", id: "not-a-uuid",
			setTx: func() pgx.Tx { return unopenedTx{} }, want: ErrInvalidRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := service.OnProposalClosed(context.Background(), tc.setTx(),
				tc.id, tc.status, "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRetryStalledLimit(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{in: 0, want: DefaultRetryStalledLimit},
		{in: -5, want: DefaultRetryStalledLimit},
		{in: 1, want: 1},
		{in: MaxRetryStalledLimit, want: MaxRetryStalledLimit},
		{in: MaxRetryStalledLimit + 1, want: MaxRetryStalledLimit},
	}
	for _, tc := range tests {
		if got := retryStalledLimit(tc.in); got != tc.want {
			t.Fatalf("retryStalledLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
