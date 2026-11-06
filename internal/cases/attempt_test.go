//go:build integration

// covers: FIN-24, FIN-30

package cases_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
)

// TestRecordFailedAttemptIncrementsAndReleases proves the worker-owned attempt
// path is atomic: it increments once, clears the lease, audits with the system
// actor, and rejects a caller that no longer owns the lease.
func TestRecordFailedAttemptIncrementsAndReleases(t *testing.T) {
	f := newFixture(t)
	c := seedOneCase(t, f, "attempt-happy", 1_500)

	if _, err := f.service.Lease(context.Background(), "worker-a", time.Minute); err != nil {
		t.Fatalf("Lease: %v", err)
	}
	updated, err := f.service.RecordFailedAttempt(context.Background(), c.ID, "worker-a", 3)
	if err != nil {
		t.Fatalf("RecordFailedAttempt: %v", err)
	}
	if updated.Status != cases.StatusOpen {
		t.Fatalf("status = %s, want open", updated.Status)
	}
	if updated.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", updated.Attempts)
	}
	if updated.LeasedBy != nil || updated.LeaseExpiresAt != nil {
		t.Fatalf("lease not cleared: %v/%v", updated.LeasedBy, updated.LeaseExpiresAt)
	}
	if updated.Version != 2 {
		t.Fatalf("version = %d, want 2", updated.Version)
	}
	if got, want := f.auditCount(t, c.ID, cases.ActionCaseAttempt), 1; got != want {
		t.Fatalf("attempt audit rows = %d, want %d", got, want)
	}

	// A non-owner can never record the attempt.
	if _, err := f.service.Lease(context.Background(), "worker-b", time.Minute); err != nil {
		t.Fatalf("Lease worker-b: %v", err)
	}
	if _, err := f.service.RecordFailedAttempt(context.Background(), c.ID, "worker-a", 3); !errors.Is(err, cases.ErrNotLeaseOwner) {
		t.Fatalf("non-owner attempt error = %v, want ErrNotLeaseOwner", err)
	}
	unchanged := f.getCase(t, c.ID)
	if unchanged.Attempts != 1 || unchanged.Version != 3 {
		t.Fatalf("rejected attempt changed case: attempts=%d version=%d", unchanged.Attempts, unchanged.Version)
	}
	if got, want := f.auditCount(t, c.ID, cases.ActionCaseAttempt), 1; got != want {
		t.Fatalf("attempt audit rows after rejection = %d, want %d", got, want)
	}

	// After Release there is no lease to record against.
	if _, err := f.service.Release(context.Background(), c.ID, "worker-b"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := f.service.RecordFailedAttempt(context.Background(), c.ID, "worker-b", 3); !errors.Is(err, cases.ErrNotLeased) {
		t.Fatalf("attempt after release error = %v, want ErrNotLeased", err)
	}
	after := f.getCase(t, c.ID)
	if after.Attempts != 1 {
		t.Fatalf("attempts after release attempt = %d, want 1", after.Attempts)
	}
}

// TestRecordFailedAttemptEscalatesAtCap proves the case is escalated exactly on
// the maxAttempts-th failed attempt, with reason agent_failed, and that no
// fourth attempt can be recorded.
func TestRecordFailedAttemptEscalatesAtCap(t *testing.T) {
	f := newFixture(t)
	c := seedOneCase(t, f, "attempt-cap", 900)

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := f.service.Lease(context.Background(), "worker-a", time.Minute); err != nil {
			t.Fatalf("Lease attempt %d: %v", attempt, err)
		}
		updated, err := f.service.RecordFailedAttempt(context.Background(), c.ID, "worker-a", 3)
		if err != nil {
			t.Fatalf("RecordFailedAttempt attempt %d: %v", attempt, err)
		}
		if updated.Status != cases.StatusOpen || updated.Attempts != attempt {
			t.Fatalf("attempt %d = %s/%d, want open/%d", attempt, updated.Status, updated.Attempts, attempt)
		}
	}

	if _, err := f.service.Lease(context.Background(), "worker-a", time.Minute); err != nil {
		t.Fatalf("Lease final attempt: %v", err)
	}
	final, err := f.service.RecordFailedAttempt(context.Background(), c.ID, "worker-a", 3)
	if err != nil {
		t.Fatalf("RecordFailedAttempt final: %v", err)
	}
	if final.Status != cases.StatusEscalated {
		t.Fatalf("final status = %s, want escalated", final.Status)
	}
	if final.EscalationReason == nil || *final.EscalationReason != "agent_failed" {
		t.Fatalf("escalation reason = %v, want agent_failed", final.EscalationReason)
	}
	if final.Attempts != 3 {
		t.Fatalf("final attempts = %d, want 3", final.Attempts)
	}
	if final.LeasedBy != nil {
		t.Fatalf("escalated case still leased by %v", final.LeasedBy)
	}
	if _, err := f.service.RecordFailedAttempt(context.Background(), c.ID, "worker-a", 3); !errors.Is(err, cases.ErrNotLeased) {
		t.Fatalf("fourth attempt error = %v, want ErrNotLeased", err)
	}
	if got, want := f.auditCount(t, c.ID, cases.ActionCaseAttempt), 3; got != want {
		t.Fatalf("attempt audit rows = %d, want %d", got, want)
	}
	if _, err := f.service.Lease(context.Background(), "worker-a", time.Minute); !errors.Is(err, cases.ErrNoCase) {
		t.Fatalf("Lease after escalation = %v, want ErrNoCase", err)
	}
}

// TestRecordFailedAttemptLostLeaseNoDoubleIncrement proves a stale worker cannot
// release or increment a lease that was reclaimed by another worker.
func TestRecordFailedAttemptLostLeaseNoDoubleIncrement(t *testing.T) {
	f := newFixture(t)
	c := seedOneCase(t, f, "attempt-lost", 700)

	if _, err := f.service.Lease(context.Background(), "worker-a", time.Minute); err != nil {
		t.Fatalf("Lease: %v", err)
	}
	f.clock.Advance(2 * time.Minute)
	reclaimed, err := f.service.ReclaimExpired(context.Background())
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", reclaimed)
	}
	after := f.getCase(t, c.ID)
	if after.Status != cases.StatusOpen || after.Attempts != 1 {
		t.Fatalf("reclaimed case = %s/%d, want open/1", after.Status, after.Attempts)
	}

	// The previous owner wakes up late: both the attempt and the release are
	// no-ops that report a lost lease.
	if _, err := f.service.RecordFailedAttempt(context.Background(), c.ID, "worker-a", 3); !errors.Is(err, cases.ErrNotLeased) {
		t.Fatalf("late attempt error = %v, want ErrNotLeased", err)
	}
	if _, err := f.service.Release(context.Background(), c.ID, "worker-a"); !errors.Is(err, cases.ErrNotLeased) {
		t.Fatalf("late release error = %v, want ErrNotLeased", err)
	}
	final := f.getCase(t, c.ID)
	if final.Attempts != 1 {
		t.Fatalf("attempts after late release/attempt = %d, want 1", final.Attempts)
	}
}

// TestRecordFailedAttemptValidation covers the request guards.
func TestRecordFailedAttemptValidation(t *testing.T) {
	f := newFixture(t)
	c := seedOneCase(t, f, "attempt-validation", 100)
	if _, err := f.service.Lease(context.Background(), "worker-a", time.Minute); err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if _, err := f.service.RecordFailedAttempt(context.Background(), "", "worker-a", 3); !errors.Is(err, cases.ErrInvalidRequest) {
		t.Fatalf("empty case error = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.service.RecordFailedAttempt(context.Background(), c.ID, " ", 3); !errors.Is(err, cases.ErrInvalidRequest) {
		t.Fatalf("empty worker error = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.service.RecordFailedAttempt(context.Background(), c.ID, "worker-a", 0); !errors.Is(err, cases.ErrInvalidRequest) {
		t.Fatalf("zero max attempts error = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.service.RecordFailedAttempt(context.Background(), "11111111-1111-4111-8111-111111111111", "worker-a", 3); !errors.Is(err, cases.ErrCaseNotFound) {
		t.Fatalf("unknown case error = %v, want ErrCaseNotFound", err)
	}
}
