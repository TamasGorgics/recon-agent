//go:build integration

// covers: FIN-24, FIN-30
package state_test

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases/state"
)

// TestRandomSequenceAuditCount drives a deterministic fixed-seed event
// sequence against a real case. It asserts that only legal edges succeed, each
// success increments the version exactly once and writes exactly one audit row,
// failures leave the row untouched, the lease invariant holds, and attempts do
// not move.
func TestRandomSequenceAuditCount(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusOpen, 1))
	rng := rand.New(rand.NewPCG(20261118, 42))

	actors := []string{
		"system:worker", "system:agent-loop", "system:matcher",
		"human:alice", "human:operator", "agent:model", "broken", "",
	}
	reasons := []string{
		"", "reviewed", "attempts_exhausted", "missing_settlement_sla",
		"manual", "partial_late_match", strings.Repeat("a", 501), "bad\x00",
	}
	resolutions := []string{
		"", "manual", "superseded_by_match", "Bad!", strings.Repeat("a", 65),
	}

	current := f.get(t, id)
	successes, failures := 0, 0

	for i := 0; i < 60; i++ {
		from := current.Status
		var req state.TransitionRequest
		if rng.IntN(10) < 7 {
			candidates := edgesFrom(from)
			edge := candidates[rng.IntN(len(candidates))]
			req = state.TransitionRequest{
				CaseID:          id,
				To:              edge.to,
				ExpectedVersion: current.Version,
				Actor:           edge.actor,
				Reason:          edge.reason,
				ResolutionType:  edge.resolution,
				TraceID:         "random-sequence",
				Now:             testNow.Add(time.Duration(i) * time.Second),
			}
		} else {
			req = state.TransitionRequest{
				CaseID:          id,
				To:              state.AllStatuses()[rng.IntN(6)],
				ExpectedVersion: []int{current.Version, current.Version + 1, current.Version - 1, 0}[rng.IntN(4)],
				Actor:           actors[rng.IntN(len(actors))],
				Reason:          reasons[rng.IntN(len(reasons))],
				ResolutionType:  resolutions[rng.IntN(len(resolutions))],
				Now:             testNow.Add(time.Duration(i) * time.Second),
			}
		}

		snapshot, err := f.transition(t, req)
		if err != nil {
			if !isTypedError(err) {
				t.Fatalf("event %d (%s -> %s): untyped error %v", i, from, req.To, err)
			}
			stored := f.get(t, id)
			if stored.Status != current.Status || stored.Version != current.Version {
				t.Fatalf("event %d error changed the case: %+v, want %+v",
					i, stored, current)
			}
			failures++
			continue
		}

		if _, ok := findSpecEdge(from, req.To); !ok {
			t.Fatalf("event %d: illegal edge %s -> %s succeeded", i, from, req.To)
		}
		if snapshot.Status != req.To || snapshot.Version != current.Version+1 {
			t.Fatalf("event %d: snapshot = %s/version %d, want %s/%d",
				i, snapshot.Status, snapshot.Version, req.To, current.Version+1)
		}
		if snapshot.Status != state.StatusInAgent &&
			(snapshot.LeaseOwner != nil || snapshot.LeaseExpiresAt != nil) {
			t.Fatalf("event %d: %s retained a lease: %+v", i, snapshot.Status, snapshot)
		}
		if snapshot.Attempts != 2 {
			t.Fatalf("event %d: attempts = %d, want 2", i, snapshot.Attempts)
		}
		current = snapshot
		successes++
	}

	if successes == 0 {
		t.Fatal("random sequence produced no successful transitions")
	}
	if failures == 0 {
		t.Fatal("random sequence produced no failed transitions")
	}
	stored := f.get(t, id)
	if stored.Version != 1+successes {
		t.Fatalf("version = %d, want 1+%d successes", stored.Version, successes)
	}
	if stored.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", stored.Attempts)
	}
	if got := f.auditCount(t, id); got != successes {
		t.Fatalf("audit rows = %d, want %d successes", got, successes)
	}
}

func findSpecEdge(from, to state.Status) (specEdge, bool) {
	for _, edge := range specEdges {
		if edge.from == from && edge.to == to {
			return edge, true
		}
	}
	return specEdge{}, false
}

func edgesFrom(from state.Status) []specEdge {
	var out []specEdge
	for _, edge := range specEdges {
		if edge.from == from {
			out = append(out, edge)
		}
	}
	if len(out) == 0 {
		panic("no legal edge from " + string(from))
	}
	return out
}

func isTypedError(err error) bool {
	for _, sentinel := range []error{
		state.ErrInvalidRequest, state.ErrInvalidStatus, state.ErrIllegalTransition,
		state.ErrTerminalState, state.ErrAlreadyInState, state.ErrStale,
		state.ErrActorNotAllowed, state.ErrReasonRequired, state.ErrInvalidReason,
		state.ErrResolutionRequired,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
