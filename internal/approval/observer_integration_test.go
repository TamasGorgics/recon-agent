//go:build integration

package approval_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval"
)

// recordingApprovalObserver captures post-commit decision observations.
type recordingApprovalObserver struct {
	mu        sync.Mutex
	decisions []recordedDecision
	failures  []string
}

type recordedDecision struct {
	tier    string
	latency float64 //floatlint:allow the Prometheus histogram boundary is float64 seconds
}

func (o *recordingApprovalObserver) ApprovalDecided(tier string, latencySeconds float64) { //floatlint:allow the Prometheus histogram boundary is float64 seconds
	o.mu.Lock()
	defer o.mu.Unlock()
	o.decisions = append(o.decisions, recordedDecision{tier: tier, latency: latencySeconds})
}

func (o *recordingApprovalObserver) ApprovalFailed(code string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures = append(o.failures, code)
}

func (o *recordingApprovalObserver) snapshot() ([]recordedDecision, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]recordedDecision(nil), o.decisions...), append([]string(nil), o.failures...)
}

func (f *fixture) serviceWithObserver(observer approval.Observer) *approval.Service {
	f.t.Helper()
	svc, err := approval.NewService(approval.Options{
		Pool:     f.app,
		Clock:    f.clock,
		State:    f.state,
		Verifier: f.verifier,
		Limits:   f.limits,
		Policy:   approval.Policy{Engine: f.engine, Loaded: f.loaded},
		TTL:      testTTL,
		Observer: observer,
	})
	if err != nil {
		f.t.Fatalf("approval.NewService with observer: %v", err)
	}
	return svc
}

// TestDecideObserverRecordsTierLatencyOnce proves a fresh T1 decision emits
// one latency sample for its tier, a replay emits nothing, and the stored
// decision time anchors the sample.
func TestDecideObserverRecordsTierLatencyOnce(t *testing.T) {
	f := newFixture(t)
	observer := &recordingApprovalObserver{}
	svc := f.serviceWithObserver(observer)
	proposalID, _, _ := f.pendingProposal(t)
	approverID, _ := f.seedApprover(approverSpec{Role: "approver"})

	createdAt := f.testNowUTC.Add(-2 * time.Hour)
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE proposals SET created_at = $2 WHERE id = $1::uuid`, proposalID, createdAt); err != nil {
		t.Fatalf("set proposal created_at: %v", err)
	}

	result, err := svc.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if result.Replayed {
		t.Fatal("first decision reported a replay")
	}
	decisions, failures := observer.snapshot()
	if len(failures) != 0 {
		t.Fatalf("fresh decision emitted failures: %v", failures)
	}
	if len(decisions) != 1 {
		t.Fatalf("fresh decision emitted %d latency samples, want 1: %+v", len(decisions), decisions)
	}
	if decisions[0].tier != "T1" {
		t.Fatalf("latency tier = %q, want T1", decisions[0].tier)
	}
	if got, want := decisions[0].latency, 7200.0; got != want {
		t.Fatalf("latency = %v, want %v", got, want)
	}

	replay, err := svc.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api")
	if err != nil {
		t.Fatalf("replay Decide: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("second identical decision was not reported as a replay")
	}
	decisions, failures = observer.snapshot()
	if len(decisions) != 1 {
		t.Fatalf("replay emitted latency samples: %+v", decisions)
	}
	if len(failures) != 0 {
		t.Fatalf("replay emitted failures: %v", failures)
	}
}

// TestDecideObserverEmitsNoLatencyBeforeDecision proves a non-pending
// proposal and an unknown proposal emit only bounded failure codes, never a
// latency sample.
func TestDecideObserverEmitsNoLatencyBeforeDecision(t *testing.T) {
	f := newFixture(t)
	observer := &recordingApprovalObserver{}
	svc := f.serviceWithObserver(observer)
	approverID, _ := f.seedApprover(approverSpec{Role: "approver"})

	proposedID, _, _ := f.seedProposal(proposalSpec{Type: "link", Payload: linkPayload()})
	_, err := svc.Decide(context.Background(), proposedID,
		approval.DecisionApproved, approverID, "api")
	if !errors.Is(err, approval.ErrNotPending) {
		t.Fatalf("Decide(non-pending) error = %v, want ErrNotPending", err)
	}

	_, err = svc.Decide(context.Background(),
		"99999999-9999-4999-8999-999999999999",
		approval.DecisionApproved, approverID, "api")
	if !errors.Is(err, approval.ErrProposalNotFound) {
		t.Fatalf("Decide(unknown) error = %v, want ErrProposalNotFound", err)
	}

	decisions, failures := observer.snapshot()
	if len(decisions) != 0 {
		t.Fatalf("failed decisions emitted latency: %+v", decisions)
	}
	wantFailures := []string{approval.ErrorCodeConflict, approval.ErrorCodeNotFound}
	if len(failures) != len(wantFailures) {
		t.Fatalf("failures = %v, want %v", failures, wantFailures)
	}
	for i, want := range wantFailures {
		if failures[i] != want {
			t.Fatalf("failure[%d] = %q, want %q", i, failures[i], want)
		}
	}
}

// TestDecideByAPIKeyObserverCountsPreDelegationFailureOnce proves the API-key
// wrapper records its own lookup failure exactly once instead of double
// counting through the delegated decision path.
func TestDecideByAPIKeyObserverCountsPreDelegationFailureOnce(t *testing.T) {
	f := newFixture(t)
	observer := &recordingApprovalObserver{}
	svc := f.serviceWithObserver(observer)

	_, err := svc.DecideByAPIKey(context.Background(),
		"11111111-2222-4333-8444-555555555555",
		approval.DecisionApproved, "no-such-key", "api")
	if !errors.Is(err, approval.ErrApproverNotFound) {
		t.Fatalf("DecideByAPIKey error = %v, want ErrApproverNotFound", err)
	}
	decisions, failures := observer.snapshot()
	if len(decisions) != 0 {
		t.Fatalf("failed API-key decision emitted latency: %+v", decisions)
	}
	if len(failures) != 1 || failures[0] != approval.ErrorCodeNotFound {
		t.Fatalf("failures = %v, want exactly [not_found]", failures)
	}
}
