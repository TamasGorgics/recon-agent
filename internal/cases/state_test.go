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

// TestTransitionOptimisticConcurrency covers FIN-24's stale-writer rule: a
// guarded update succeeds once, a stale expected version returns ErrStale,
// and the stale attempt changes no row and writes no audit entry.
func TestTransitionOptimisticConcurrency(t *testing.T) {
	f := newFixture(t)
	seeded := seedOneCase(t, f, "transition-stale", 3300)

	leased, err := f.service.Lease(context.Background(), "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if got, want := leased.Version, 1; got != want {
		t.Fatalf("version after lease = %d, want %d", got, want)
	}

	escalated, err := f.service.Transition(context.Background(), cases.TransitionRequest{
		CaseID:           leased.ID,
		ExpectedVersion:  leased.Version,
		Status:           cases.StatusEscalated,
		EscalationReason: "manager_review",
	})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if escalated.Status != cases.StatusEscalated {
		t.Fatalf("status = %s, want %s", escalated.Status, cases.StatusEscalated)
	}
	if got, want := escalated.Version, 2; got != want {
		t.Fatalf("version after transition = %d, want %d", got, want)
	}
	if escalated.LeasedBy != nil || escalated.LeaseExpiresAt != nil {
		t.Fatalf("transitioned case still leased: %+v", escalated)
	}
	if escalated.EscalationReason == nil || *escalated.EscalationReason != "manager_review" {
		t.Fatalf("escalation_reason = %v, want manager_review", escalated.EscalationReason)
	}
	if got, want := f.auditCount(t, seeded.ID, cases.ActionCaseTransition), 1; got != want {
		t.Fatalf("transition audit rows = %d, want %d", got, want)
	}

	auditsBefore := f.auditTotal(t)
	_, err = f.service.Transition(context.Background(), cases.TransitionRequest{
		CaseID:          leased.ID,
		ExpectedVersion: 1, // stale
		Status:          cases.StatusResolved,
	})
	if !errors.Is(err, cases.ErrStale) {
		t.Fatalf("stale Transition error = %v, want ErrStale", err)
	}
	after := f.getCase(t, leased.ID)
	if after.Status != cases.StatusEscalated || after.Version != 2 {
		t.Fatalf("stale transition changed case: status=%s version=%d, want escalated/2", after.Status, after.Version)
	}
	if after.ResolvedAt != nil {
		t.Fatalf("stale transition set resolved_at = %v, want nil", after.ResolvedAt)
	}
	if got, want := f.auditTotal(t), auditsBefore; got != want {
		t.Fatalf("audit rows after stale transition = %d, want %d", got, want)
	}

	resolved, err := f.service.Transition(context.Background(), cases.TransitionRequest{
		CaseID:          leased.ID,
		ExpectedVersion: 2,
		Status:          cases.StatusResolved,
		ResolutionType:  "adjustment",
	})
	if err != nil {
		t.Fatalf("resolve Transition: %v", err)
	}
	if resolved.Status != cases.StatusResolved || resolved.Version != 3 {
		t.Fatalf("resolved case = %s version %d, want resolved/3", resolved.Status, resolved.Version)
	}
	if resolved.ResolvedAt == nil {
		t.Fatal("resolved_at = nil, want a timestamp")
	}
	if resolved.ResolutionType == nil || *resolved.ResolutionType != "adjustment" {
		t.Fatalf("resolution_type = %v, want adjustment", resolved.ResolutionType)
	}
}

// TestTransitionRejectsUnknownStatus keeps the status machine closed.
func TestTransitionRejectsUnknownStatus(t *testing.T) {
	f := newFixture(t)
	seeded := seedOneCase(t, f, "transition-invalid", 100)
	if _, err := f.service.Transition(context.Background(), cases.TransitionRequest{
		CaseID:          seeded.ID,
		ExpectedVersion: seeded.Version,
		Status:          cases.Status("reviewing"),
	}); !errors.Is(err, cases.ErrInvalidStatus) {
		t.Fatalf("invalid status Transition error = %v, want ErrInvalidStatus", err)
	}
	if got := f.getCase(t, seeded.ID); got.Status != seeded.Status || got.Version != seeded.Version {
		t.Fatalf("invalid transition changed case: %+v", got)
	}
}

// TestTransitionUnknownCase reports a typed not-found error.
func TestTransitionUnknownCase(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Transition(context.Background(), cases.TransitionRequest{
		CaseID:          syntheticLedgerID(),
		ExpectedVersion: 0,
		Status:          cases.StatusEscalated,
	}); !errors.Is(err, cases.ErrCaseNotFound) {
		t.Fatalf("unknown case Transition error = %v, want ErrCaseNotFound", err)
	}
}

// TestTransitionToResolvedFromOpenIsNotLeased guards the status/lease
// coherence check at the API level too.
func TestTransitionToResolvedFromOpenIsNotLeased(t *testing.T) {
	f := newFixture(t)
	seeded := seedOneCase(t, f, "transition-open-resolved", 100)
	result, err := f.service.Transition(context.Background(), cases.TransitionRequest{
		CaseID:          seeded.ID,
		ExpectedVersion: 0,
		Status:          cases.StatusResolved,
		ResolutionType:  "write_off",
	})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if result.ResolvedAt == nil || result.LeaseExpiresAt != nil {
		t.Fatalf("resolved case = %+v, want resolved_at set and no lease", result)
	}
}
