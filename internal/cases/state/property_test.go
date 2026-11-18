package state

import (
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// model is the in-memory case row used by the property test. It applies the
// same field rules as the SQL update so the properties can be checked without a
// database; the integration tests prove the SQL and audit behavior.
type model struct {
	status     Status
	version    int
	attempts   int
	escalation *string
	resolution *string
	resolvedAt *time.Time
	leaseOwner *string
	leaseUntil *time.Time
	audits     int
}

func (m model) snapshot() Snapshot {
	return Snapshot{
		CaseID:           testCaseID,
		Status:           m.status,
		Version:          m.version,
		Attempts:         m.attempts,
		EscalationReason: m.escalation,
		ResolutionType:   m.resolution,
		ResolvedAt:       m.resolvedAt,
		LeaseOwner:       m.leaseOwner,
		LeaseExpiresAt:   m.leaseUntil,
	}
}

func randomModel(t *rapid.T) model {
	status := AllStatuses()[rapid.IntRange(0, len(AllStatuses())-1).Draw(t, "initial_status")]
	m := model{status: status, version: 1, attempts: rapid.IntRange(0, 3).Draw(t, "attempts")}
	switch status {
	case StatusInAgent:
		owner := "worker-1"
		until := testNow.Add(time.Hour)
		m.leaseOwner = &owner
		m.leaseUntil = &until
	case StatusEscalated:
		reason := EscalationManual
		m.escalation = &reason
	case StatusResolved:
		resolution := "manual"
		at := testNow.Add(-time.Hour)
		m.resolution = &resolution
		m.resolvedAt = &at
	}
	return m
}

// applyModel mirrors the column rules in Transition's UPDATE. It must stay
// deliberately independent from the SQL text so a divergence is test-visible.
func applyModel(m model, to Status, change plan, now time.Time) model {
	next := m
	next.status = to
	next.version++
	if to != StatusInAgent {
		next.leaseOwner = nil
		next.leaseUntil = nil
	}
	if to == StatusEscalated {
		reason := change.reason
		next.escalation = &reason
	} else {
		next.escalation = nil
	}
	if to == StatusResolved {
		at := now
		resolution := change.resolution
		next.resolvedAt = &at
		next.resolution = &resolution
	} else {
		next.resolvedAt = nil
		next.resolution = nil
	}
	next.audits++
	return next
}

func checkInvariants(t *rapid.T, m model, successes int) {
	if m.version != 1+successes {
		t.Fatalf("version = %d, want 1+%d successes", m.version, successes)
	}
	if m.audits != successes {
		t.Fatalf("audit count = %d, want %d successes", m.audits, successes)
	}
	if m.attempts < 0 || m.attempts > 3 {
		t.Fatalf("attempts changed to %d", m.attempts)
	}
	if m.status != StatusInAgent && (m.leaseOwner != nil || m.leaseUntil != nil) {
		t.Fatalf("status %s retained a lease", m.status)
	}
	if (m.leaseOwner == nil) != (m.leaseUntil == nil) {
		t.Fatalf("lease fields are incoherent: owner=%v until=%v", m.leaseOwner, m.leaseUntil)
	}
	if m.status == StatusResolved {
		if m.resolution == nil || m.resolvedAt == nil {
			t.Fatalf("resolved case is missing resolution/resolved_at: %+v", m)
		}
	} else if m.resolution != nil || m.resolvedAt != nil {
		t.Fatalf("non-resolved case carries resolution/resolved_at: %+v", m)
	}
	if m.status == StatusEscalated {
		if m.escalation == nil {
			t.Fatal("escalated case is missing escalation_reason")
		}
	} else if m.escalation != nil {
		t.Fatalf("non-escalated case carries escalation_reason: %+v", m)
	}
}

func isExpectedError(err error) bool {
	for _, sentinel := range []error{
		ErrInvalidRequest, ErrInvalidStatus, ErrIllegalTransition,
		ErrTerminalState, ErrAlreadyInState, ErrStale, ErrActorNotAllowed,
		ErrReasonRequired, ErrInvalidReason, ErrResolutionRequired,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// TestPropertySequences runs deterministic randomized event sequences against
// the pure decision layer. rapid shrinks a failing sequence; the seed is pinned
// unless the caller passes -rapid.seed.
func TestPropertySequences(t *testing.T) {
	if f := flag.Lookup("rapid.seed"); f != nil && f.Value.String() == "0" {
		if err := flag.Set("rapid.seed", "20261118"); err != nil {
			t.Fatalf("pin rapid seed: %v", err)
		}
	}

	actors := []string{
		"human:alice", "human:operator", "system:worker", "system:agent-loop",
		"system:matcher", "agent:model", "broken", "", "Human:alice",
		"human:" + strings.Repeat("a", 129),
	}
	reasons := []string{
		"", "reviewed", "attempts_exhausted", "missing_settlement_sla",
		"manual", "partial_late_match", strings.Repeat("a", 501), "bad\x00",
		"line one\nline two", "  spaced  ",
	}
	resolutions := []string{
		"", "manual", "superseded_by_match", "Bad!", "a" + strings.Repeat("b", 63),
		strings.Repeat("a", 65), "  manual  ",
	}

	rapid.Check(t, func(t *rapid.T) {
		current := randomModel(t)
		startAttempts := current.attempts
		successes := 0
		checkInvariants(t, current, successes)

		events := rapid.IntRange(1, 25).Draw(t, "events")
		for i := 0; i < events; i++ {
			to := AllStatuses()[rapid.IntRange(0, len(AllStatuses())-1).Draw(t, "to")]
			actor := rapid.SampledFrom(actors).Draw(t, "actor")
			reason := rapid.SampledFrom(reasons).Draw(t, "reason")
			resolution := rapid.SampledFrom(resolutions).Draw(t, "resolution")
			expected := rapid.SampledFrom([]int{current.version, current.version + 1, 0}).
				Draw(t, "expected")
			now := testNow.Add(time.Duration(i) * time.Second)

			req := TransitionRequest{
				CaseID:          testCaseID,
				To:              to,
				ExpectedVersion: expected,
				Actor:           actor,
				Reason:          reason,
				ResolutionType:  resolution,
				Now:             now,
			}
			before := current.snapshot()
			parsed, err := parseRequest(req)
			if err != nil {
				if !isExpectedError(err) {
					t.Fatalf("parseRequest returned %v, want a typed sentinel", err)
				}
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("parseRequest returned %v, want ErrInvalidRequest", err)
				}
				checkInvariants(t, current, successes)
				continue
			}
			change, err := decide(before, parsed)
			if err != nil {
				if !isExpectedError(err) {
					t.Fatalf("decide returned %v, want a typed sentinel", err)
				}
				checkInvariants(t, current, successes)
				continue
			}
			if expected != current.version {
				t.Fatalf("success with stale version %d against %d", expected, current.version)
			}
			if _, ok := lookupEdge(current.status, to); !ok {
				t.Fatalf("success on an absent edge %s -> %s", current.status, to)
			}
			next := applyModel(current, to, change, now)
			successes++
			if next.attempts != startAttempts {
				t.Fatalf("attempts changed from %d to %d", startAttempts, next.attempts)
			}
			current = next
			checkInvariants(t, current, successes)
		}
	})
}
