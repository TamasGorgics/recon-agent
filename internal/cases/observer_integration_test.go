//go:build integration

package cases_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

type observedCaseAge struct {
	status  string
	seconds float64
}

type recordingCaseObserver struct {
	ages     []observedCaseAge
	failures []string
}

func (o *recordingCaseObserver) CaseAge(status string, ageSeconds float64) {
	o.ages = append(o.ages, observedCaseAge{status: status, seconds: ageSeconds})
}

func (o *recordingCaseObserver) CaseFailed(code string) { o.failures = append(o.failures, code) }

func observedCaseService(t *testing.T, f *fixture, observer cases.Observer) *cases.Service {
	t.Helper()
	actor, err := audit.SystemActor("cases")
	if err != nil {
		t.Fatalf("system actor: %v", err)
	}
	svc, err := cases.NewService(f.pool, f.clock, actor, cases.WithObserver(observer))
	if err != nil {
		t.Fatalf("new observed case service: %v", err)
	}
	return svc
}

// TestCaseObserverRecordsLifecycle proves create, lease, escalate, and resolve
// each emit exactly one post-commit observation with the row's created_at age.
// Resolve must use resolved_at, so the final age equals create-to-resolve.
func TestCaseObserverRecordsLifecycle(t *testing.T) {
	f := newFixture(t)
	observer := &recordingCaseObserver{}
	svc := observedCaseService(t, f, observer)
	ctx := context.Background()

	batch := insertBatch(t, f.db)
	ref := f.addExternal(t, "observer-lifecycle", "payout", -700, batch)
	created, err := svc.CreateBreaks(ctx, cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: f.runID,
		Breaks:     []match.BreakCandidate{externalOnlyCandidate(t, f.scopeID, "USD", ref, 700)},
	})
	if err != nil {
		t.Fatalf("CreateBreaks: %v", err)
	}
	if len(created.Cases) != 1 {
		t.Fatalf("created cases = %d, want 1", len(created.Cases))
	}
	if len(observer.ages) != 1 || observer.ages[0].status != "open" || observer.ages[0].seconds != 0 {
		t.Fatalf("create observation = %+v, want [open 0]", observer.ages)
	}

	leased, err := svc.Lease(ctx, "worker-observer", time.Minute)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if got := observer.ages[len(observer.ages)-1]; got.status != "in_agent" || got.seconds != 0 {
		t.Fatalf("lease observation = %+v, want in_agent 0", got)
	}

	f.clock.Advance(2 * time.Minute)
	escalated, err := svc.Transition(ctx, cases.TransitionRequest{
		CaseID:           leased.ID,
		ExpectedVersion:  leased.Version,
		Status:           cases.StatusEscalated,
		EscalationReason: "manual",
	})
	if err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if got := observer.ages[len(observer.ages)-1]; got.status != "escalated" || got.seconds != 120 {
		t.Fatalf("escalate observation = %+v, want escalated 120", got)
	}

	f.clock.Advance(time.Minute)
	resolved, err := svc.Transition(ctx, cases.TransitionRequest{
		CaseID:          escalated.ID,
		ExpectedVersion: escalated.Version,
		Status:          cases.StatusResolved,
		ResolutionType:  "adjustment",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ResolvedAt == nil {
		t.Fatal("resolved_at is nil")
	}
	if got := observer.ages[len(observer.ages)-1]; got.status != "resolved" || got.seconds != 180 {
		t.Fatalf("resolve observation = %+v, want resolved 180", got)
	}

	// An empty queue is a normal condition: no CaseFailed observation.
	before := len(observer.failures)
	if _, err := svc.Lease(ctx, "worker-observer", time.Minute); !errors.Is(err, cases.ErrNoCase) {
		t.Fatalf("empty Lease error = %v, want ErrNoCase", err)
	}
	if len(observer.failures) != before {
		t.Fatalf("empty queue failures = %v, want none", observer.failures)
	}
}

// TestCaseObserverRecordsBoundedFailures proves invalid requests and not-found
// transitions emit bounded codes and no age observation.
func TestCaseObserverRecordsBoundedFailures(t *testing.T) {
	f := newFixture(t)
	observer := &recordingCaseObserver{}
	svc := observedCaseService(t, f, observer)
	ctx := context.Background()

	if _, err := svc.CreateCase(ctx, nil); !errors.Is(err, cases.ErrNoBreaks) {
		t.Fatalf("CreateCase(nil) error = %v, want ErrNoBreaks", err)
	}
	if _, err := svc.Transition(ctx, cases.TransitionRequest{
		CaseID:          "00000000-0000-4000-8000-000000000000",
		ExpectedVersion: 0,
		Status:          cases.Status("not_a_status"),
	}); !errors.Is(err, cases.ErrInvalidStatus) {
		t.Fatalf("invalid status error = %v, want ErrInvalidStatus", err)
	}
	if _, err := svc.Transition(ctx, cases.TransitionRequest{
		CaseID:          "00000000-0000-4000-8000-000000000000",
		ExpectedVersion: 0,
		Status:          cases.StatusInAgent,
	}); !errors.Is(err, cases.ErrCaseNotFound) {
		t.Fatalf("missing case error = %v, want ErrCaseNotFound", err)
	}

	want := []string{cases.ErrorCodeInvalidRequest, cases.ErrorCodeInvalidRequest, cases.ErrorCodeNotFound}
	if len(observer.failures) != len(want) {
		t.Fatalf("failures = %v, want %v", observer.failures, want)
	}
	for i := range want {
		if observer.failures[i] != want[i] {
			t.Fatalf("failures = %v, want %v", observer.failures, want)
		}
	}
	if len(observer.ages) != 0 {
		t.Fatalf("failed calls observed ages: %+v", observer.ages)
	}
}
