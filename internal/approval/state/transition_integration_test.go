//go:build integration

// covers: FIN-24, FIN-30, FIN-33
package state_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// TestTransitionLegalMatrix walks all 17 legal edges as the real recon_app
// login. Each edge succeeds with its permitted actor type, returns and stores
// exactly one version bump, writes exactly one audit row, and leaves every
// other proposal column untouched.
func TestTransitionLegalMatrix(t *testing.T) {
	f := newFixture(t)
	for i, want := range specEdges {
		index := i
		edge := want
		t.Run(fmt.Sprintf("%02d_%s_to_%s", i+1, edge.from, edge.to), func(t *testing.T) {
			id := f.seed(t, edge.from)
			actor := systemActor(t, "test")
			if edge.actor == audit.ActorHuman {
				actor = humanActor(t, "alice")
			}
			got, err := f.transition(state.TransitionRequest{
				ProposalID:      id,
				To:              edge.to,
				ExpectedVersion: 0,
				Actor:           actor,
				Reason:          legalReason(edge),
			})
			if err != nil {
				t.Fatalf("edge %d Transition: %v", index+1, err)
			}
			if got.ID != id || got.Status != edge.to || got.Version != 1 {
				t.Fatalf("returned proposal = %+v, want id %s status %s version 1",
					got, id, edge.to)
			}
			stored := f.get(t, id)
			if stored.Status != edge.to || stored.Version != 1 {
				t.Fatalf("stored proposal = %s/version %d, want %s/version 1",
					stored.Status, stored.Version, edge.to)
			}
			if stored.StateHash != "state-hash-keep" || stored.Rationale != "keep rationale" {
				t.Fatalf("transition changed non-state columns: %+v", stored)
			}
			if len(stored.Payload) != 1 || stored.Payload["marker"] != "keep" {
				t.Fatalf("transition changed payload: %v", stored.Payload)
			}
			if stored.ExpiresAt != nil || stored.Supersedes != nil {
				t.Fatalf("transition changed expires_at/supersedes: %+v", stored)
			}
			rows := f.audits(t, id)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %d, want exactly 1", len(rows))
			}
			if rows[0].Action != state.ActionProposalTransition ||
				rows[0].EntityType != state.EntityProposal {
				t.Fatalf("audit row = %+v, want %s/%s",
					rows[0], state.ActionProposalTransition, state.EntityProposal)
			}
			before := jsonObject(t, rows[0].Before)
			after := jsonObject(t, rows[0].After)
			if before["status"] != string(edge.from) || before["version"] != float64(0) {
				t.Fatalf("audit before = %v, want status %s version 0", before, edge.from)
			}
			if after["status"] != string(edge.to) || after["version"] != float64(1) {
				t.Fatalf("audit after = %v, want status %s version 1", after, edge.to)
			}
			reason := legalReason(edge)
			if reason == "" {
				if _, ok := after["reason"]; ok {
					t.Fatalf("audit after has reason %v, want none", after["reason"])
				}
			} else if after["reason"] != reason {
				t.Fatalf("audit after reason = %v, want %q", after["reason"], reason)
			}
		})
	}
}

// TestTransitionIllegalMatrix walks every non-edge pair. The expected typed
// error is ErrAlreadyInState for a self-transition, ErrTerminalState for a
// terminal source, and ErrIllegalTransition otherwise; the row and audit table
// stay untouched in every case.
func TestTransitionIllegalMatrix(t *testing.T) {
	f := newFixture(t)
	system := systemActor(t, "test")
	for _, from := range state.AllStatuses() {
		for _, to := range state.AllStatuses() {
			if _, legal := findSpecEdge(from, to); legal {
				continue
			}
			from, to := from, to
			t.Run(fmt.Sprintf("%s_to_%s", from, to), func(t *testing.T) {
				id := f.seed(t, from)
				_, err := f.transition(state.TransitionRequest{
					ProposalID:      id,
					To:              to,
					ExpectedVersion: 0,
					Actor:           system,
				})
				switch {
				case from == to:
					if !errors.Is(err, state.ErrAlreadyInState) {
						t.Fatalf("self-transition error = %v, want ErrAlreadyInState", err)
					}
				case state.Terminal(from):
					if !errors.Is(err, state.ErrTerminalState) {
						t.Fatalf("terminal error = %v, want ErrTerminalState", err)
					}
					if !errors.Is(err, state.ErrIllegalTransition) {
						t.Fatalf("terminal error = %v, want it to satisfy ErrIllegalTransition", err)
					}
				default:
					if !errors.Is(err, state.ErrIllegalTransition) {
						t.Fatalf("illegal edge error = %v, want ErrIllegalTransition", err)
					}
				}
				stored := f.get(t, id)
				if stored.Status != from || stored.Version != 0 {
					t.Fatalf("illegal transition changed proposal: %s/version %d", stored.Status, stored.Version)
				}
				if got := f.auditCount(t, id); got != 0 {
					t.Fatalf("illegal transition wrote %d audit rows, want 0", got)
				}
			})
		}
	}
}

// TestTransitionStaleVersion covers FIN-24: a stale expected version loses
// before legality is considered, changes no row, and writes no audit entry.
func TestTransitionStaleVersion(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, state.StatusPendingApproval)
	human := humanActor(t, "alice")

	if _, err := f.transition(state.TransitionRequest{
		ProposalID:      id,
		To:              state.StatusApproved,
		ExpectedVersion: 7,
		Actor:           human,
	}); !errors.Is(err, state.ErrStale) {
		t.Fatalf("stale error = %v, want ErrStale", err)
	}
	// Stale takes precedence over an illegal target.
	if _, err := f.transition(state.TransitionRequest{
		ProposalID:      id,
		To:              state.StatusApplied,
		ExpectedVersion: 7,
		Actor:           human,
	}); !errors.Is(err, state.ErrStale) {
		t.Fatalf("stale illegal error = %v, want ErrStale", err)
	}
	stored := f.get(t, id)
	if stored.Status != state.StatusPendingApproval || stored.Version != 0 {
		t.Fatalf("stale transition changed proposal: %s/version %d", stored.Status, stored.Version)
	}
	if got := f.auditCount(t, id); got != 0 {
		t.Fatalf("stale transition wrote %d audit rows, want 0", got)
	}
}

// TestTransitionAuditInTransaction covers FIN-30: returning an error from the
// caller's WithTx callback removes both the status change and the audit row.
func TestTransitionAuditInTransaction(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, state.StatusProposed)
	forced := errors.New("force rollback")

	err := db.WithTx(context.Background(), f.app, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := f.service.Transition(ctx, tx, state.TransitionRequest{
				ProposalID:      id,
				To:              state.StatusPendingApproval,
				ExpectedVersion: 0,
				Actor:           systemActor(t, "test"),
			}); err != nil {
				return err
			}
			return forced
		})
	if !errors.Is(err, forced) {
		t.Fatalf("callback error = %v, want the forced rollback", err)
	}
	stored := f.get(t, id)
	if stored.Status != state.StatusProposed || stored.Version != 0 {
		t.Fatalf("rolled-back transition changed proposal: %s/version %d", stored.Status, stored.Version)
	}
	if got := f.auditCount(t, id); got != 0 {
		t.Fatalf("rolled-back transition wrote %d audit rows, want 0", got)
	}
}

// TestTransitionAuditPayload pins the exact before/after shapes, actor, fake
// clock timestamp, trace ID handling, and the absence of rationale/payload
// text.
func TestTransitionAuditPayload(t *testing.T) {
	f := newFixture(t)
	clockTime := time.Date(2026, 11, 12, 13, 14, 15, 987654321, time.UTC)
	f.clock.Set(clockTime)

	approvedID := f.seed(t, state.StatusPendingApproval)
	if _, err := f.transition(state.TransitionRequest{
		ProposalID:      approvedID,
		To:              state.StatusApproved,
		ExpectedVersion: 0,
		Actor:           humanActor(t, "alice"),
	}); err != nil {
		t.Fatalf("approve Transition: %v", err)
	}
	rows := f.audits(t, approvedID)
	if len(rows) != 1 {
		t.Fatalf("approve audit rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != string(audit.ActorHuman) || row.ActorID != "alice" {
		t.Fatalf("audit actor = %s:%s, want human:alice", row.ActorType, row.ActorID)
	}
	if !row.Timestamp.Equal(clockTime.Truncate(time.Microsecond)) {
		t.Fatalf("audit timestamp = %s, want %s (fake clock, microsecond precision)",
			row.Timestamp, clockTime.Truncate(time.Microsecond))
	}
	if row.TraceID != nil {
		t.Fatalf("audit trace_id = %q, want NULL", *row.TraceID)
	}
	before := jsonObject(t, row.Before)
	after := jsonObject(t, row.After)
	if got, want := objectKeys(before), []string{"status", "version"}; !equalStrings(got, want) {
		t.Fatalf("before keys = %v, want %v", got, want)
	}
	if got, want := objectKeys(after), []string{"status", "version"}; !equalStrings(got, want) {
		t.Fatalf("after keys = %v, want %v", got, want)
	}
	if before["status"] != "pending_approval" || before["version"] != float64(0) {
		t.Fatalf("before = %v, want pending_approval/0", before)
	}
	if after["status"] != "approved" || after["version"] != float64(1) {
		t.Fatalf("after = %v, want approved/1", after)
	}

	rejectedID := f.seed(t, state.StatusPendingApproval)
	traceID := "trace-42"
	if _, err := f.transition(state.TransitionRequest{
		ProposalID:      rejectedID,
		To:              state.StatusRejected,
		ExpectedVersion: 0,
		Actor:           humanActor(t, "bob"),
		Reason:          "  duplicate charge  ",
		TraceID:         &traceID,
	}); err != nil {
		t.Fatalf("reject Transition: %v", err)
	}
	rejectedRows := f.audits(t, rejectedID)
	if len(rejectedRows) != 1 {
		t.Fatalf("reject audit rows = %d, want 1", len(rejectedRows))
	}
	rejectedRow := rejectedRows[0]
	if rejectedRow.TraceID == nil || *rejectedRow.TraceID != traceID {
		t.Fatalf("audit trace_id = %v, want %q", rejectedRow.TraceID, traceID)
	}
	rejectedAfter := jsonObject(t, rejectedRow.After)
	if got, want := objectKeys(rejectedAfter), []string{"reason", "status", "version"}; !equalStrings(got, want) {
		t.Fatalf("after keys = %v, want %v", got, want)
	}
	if rejectedAfter["reason"] != "duplicate charge" {
		t.Fatalf("after.reason = %v, want trimmed %q", rejectedAfter["reason"], "duplicate charge")
	}
	for _, raw := range [][]byte{
		rejectedRow.Before, rejectedRow.After,
		row.Before, row.After,
	} {
		text := string(raw)
		for _, forbidden := range []string{"keep rationale", "marker", "payload"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("audit payload %s contains %q", text, forbidden)
			}
		}
	}
}

// TestTransitionOnlyStatusVersionMutate proves a seeded expires_at and
// supersedes_proposal_id survive a transition unchanged.
func TestTransitionOnlyStatusVersionMutate(t *testing.T) {
	f := newFixture(t)
	reference := f.seed(t, state.StatusSuperseded)
	at := f.clock.Now().Add(48 * time.Hour)
	id := f.seed(t, state.StatusPendingApproval, withVersion(3),
		withExpiry(at), withSupersedes(reference))

	got, err := f.transition(state.TransitionRequest{
		ProposalID:      id,
		To:              state.StatusApproved,
		ExpectedVersion: 3,
		Actor:           humanActor(t, "alice"),
	})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if got.Version != 4 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(at) {
		t.Fatalf("returned proposal = %+v, want version 4 and expiry %s", got, at)
	}
	if got.SupersedesProposalID == nil || *got.SupersedesProposalID != reference {
		t.Fatalf("returned supersedes = %v, want %s", got.SupersedesProposalID, reference)
	}
	stored := f.get(t, id)
	if stored.Version != 4 || stored.Status != state.StatusApproved {
		t.Fatalf("stored status/version = %s/%d, want approved/4", stored.Status, stored.Version)
	}
	if stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(at) {
		t.Fatalf("stored expires_at = %v, want %s", stored.ExpiresAt, at)
	}
	if stored.Supersedes == nil || *stored.Supersedes != reference {
		t.Fatalf("stored supersedes = %v, want %s", stored.Supersedes, reference)
	}
	if got := f.auditCount(t, id); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}

// TestTransitionActorAndReasonErrors covers FIN-33 and the reason rules: every
// rejected request leaves the row and audit table untouched.
func TestTransitionActorAndReasonErrors(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name   string
		status state.Status
		to     state.Status
		actor  audit.Actor
		reason string
		want   error
	}{
		{
			name:   "agent cannot route a proposal",
			status: state.StatusProposed,
			to:     state.StatusPendingApproval,
			actor:  agentActor(t, "model"),
			want:   state.ErrActorNotAllowed,
		},
		{
			name:   "human cannot run an executor edge",
			status: state.StatusApproved,
			to:     state.StatusApplied,
			actor:  humanActor(t, "alice"),
			want:   state.ErrActorNotAllowed,
		},
		{
			name:   "system cannot satisfy human approval",
			status: state.StatusPendingApproval,
			to:     state.StatusApproved,
			actor:  systemActor(t, "approval"),
			want:   state.ErrActorNotAllowed,
		},
		{
			name:   "rejection requires a reason",
			status: state.StatusPendingApproval,
			to:     state.StatusRejected,
			actor:  humanActor(t, "alice"),
			reason: "   ",
			want:   state.ErrReasonRequired,
		},
		{
			name:   "apply_failed requires a reason",
			status: state.StatusApproved,
			to:     state.StatusApplyFailed,
			actor:  systemActor(t, "executor"),
			want:   state.ErrReasonRequired,
		},
		{
			name:   "apply_failed reason must be in the enum",
			status: state.StatusApproved,
			to:     state.StatusApplyFailed,
			actor:  systemActor(t, "executor"),
			reason: "bogus",
			want:   state.ErrInvalidReason,
		},
		{
			name:   "reason rejects control characters",
			status: state.StatusPendingApproval,
			to:     state.StatusRejected,
			actor:  humanActor(t, "alice"),
			reason: "bad\nreason",
			want:   state.ErrInvalidReason,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			id := f.seed(t, tc.status)
			_, err := f.transition(state.TransitionRequest{
				ProposalID:      id,
				To:              tc.to,
				ExpectedVersion: 0,
				Actor:           tc.actor,
				Reason:          tc.reason,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			stored := f.get(t, id)
			if stored.Status != tc.status || stored.Version != 0 {
				t.Fatalf("failed transition changed proposal: %s/version %d", stored.Status, stored.Version)
			}
			if got := f.auditCount(t, id); got != 0 {
				t.Fatalf("failed transition wrote %d audit rows, want 0", got)
			}
		})
	}

	// The enum itself is accepted and audit-visible.
	id := f.seed(t, state.StatusApproved)
	got, err := f.transition(state.TransitionRequest{
		ProposalID:      id,
		To:              state.StatusApplyFailed,
		ExpectedVersion: 0,
		Actor:           systemActor(t, "executor"),
		Reason:          "break_resolved",
	})
	if err != nil {
		t.Fatalf("apply_failed with enum reason: %v", err)
	}
	if got.Status != state.StatusApplyFailed || got.Version != 1 {
		t.Fatalf("apply_failed result = %s/version %d, want apply_failed/1", got.Status, got.Version)
	}
	rows := f.audits(t, id)
	if len(rows) != 1 || jsonObject(t, rows[0].After)["reason"] != "break_resolved" {
		t.Fatalf("apply_failed audit rows = %+v, want one with reason break_resolved", rows)
	}
}

// TestTransitionAlreadyInStateAndTerminal pins the self-transition and terminal
// precedence: no update, no version bump, no audit row.
func TestTransitionAlreadyInStateAndTerminal(t *testing.T) {
	f := newFixture(t)
	system := systemActor(t, "test")
	for _, status := range state.AllStatuses() {
		status := status
		t.Run("self_"+string(status), func(t *testing.T) {
			id := f.seed(t, status)
			_, err := f.transition(state.TransitionRequest{
				ProposalID:      id,
				To:              status,
				ExpectedVersion: 0,
				Actor:           system,
				Reason:          "ignored",
			})
			if !errors.Is(err, state.ErrAlreadyInState) {
				t.Fatalf("self transition error = %v, want ErrAlreadyInState", err)
			}
			stored := f.get(t, id)
			if stored.Status != status || stored.Version != 0 {
				t.Fatalf("self transition changed proposal: %s/version %d", stored.Status, stored.Version)
			}
			if got := f.auditCount(t, id); got != 0 {
				t.Fatalf("self transition wrote %d audit rows, want 0", got)
			}
		})
	}

	for _, status := range []state.Status{
		state.StatusApplied, state.StatusRejected, state.StatusExpired, state.StatusSuperseded,
	} {
		status := status
		t.Run("terminal_"+string(status), func(t *testing.T) {
			id := f.seed(t, status)
			_, err := f.transition(state.TransitionRequest{
				ProposalID:      id,
				To:              state.StatusApproved,
				ExpectedVersion: 0,
				Actor:           system,
			})
			if !errors.Is(err, state.ErrTerminalState) {
				t.Fatalf("terminal transition error = %v, want ErrTerminalState", err)
			}
			if !errors.Is(err, state.ErrIllegalTransition) {
				t.Fatalf("terminal transition error = %v, want ErrIllegalTransition too", err)
			}
			stored := f.get(t, id)
			if stored.Status != status || stored.Version != 0 {
				t.Fatalf("terminal transition changed proposal: %s/version %d", stored.Status, stored.Version)
			}
			if got := f.auditCount(t, id); got != 0 {
				t.Fatalf("terminal transition wrote %d audit rows, want 0", got)
			}
		})
	}
}

func TestTransitionNotFound(t *testing.T) {
	f := newFixture(t)
	_, err := f.transition(state.TransitionRequest{
		ProposalID:      "00000000-0000-4000-8000-000000000099",
		To:              state.StatusPendingApproval,
		ExpectedVersion: 0,
		Actor:           systemActor(t, "test"),
	})
	if !errors.Is(err, state.ErrProposalNotFound) {
		t.Fatalf("not-found error = %v, want ErrProposalNotFound", err)
	}
}

type raceResult struct {
	name     string
	proposal state.Proposal
	err      error
}

func runRace(t *testing.T, f *fixture, id string, requests []state.TransitionRequest) []raceResult {
	t.Helper()
	start := make(chan struct{})
	results := make(chan raceResult, len(requests))
	for i, req := range requests {
		req := req
		name := fmt.Sprintf("goroutine-%d", i)
		go func() {
			<-start
			proposal, err := f.transition(req)
			results <- raceResult{name: name, proposal: proposal, err: err}
		}()
	}
	close(start)
	out := make([]raceResult, 0, len(requests))
	for range requests {
		out = append(out, <-results)
	}
	return out
}

// TestTransitionConcurrentApproveReject covers FIN-24/FIN-30: exactly one
// writer wins, the loser is stale, and exactly one audit row survives.
func TestTransitionConcurrentApproveReject(t *testing.T) {
	f := newFixture(t)
	approve := humanActor(t, "alice")
	reject := humanActor(t, "bob")
	for iteration := 0; iteration < 10; iteration++ {
		id := f.seed(t, state.StatusPendingApproval)
		results := runRace(t, f, id, []state.TransitionRequest{
			{ProposalID: id, To: state.StatusApproved, ExpectedVersion: 0, Actor: approve},
			{ProposalID: id, To: state.StatusRejected, ExpectedVersion: 0, Actor: reject, Reason: "duplicate"},
		})
		winners, stale := 0, 0
		for _, result := range results {
			switch {
			case result.err == nil:
				winners++
			case errors.Is(result.err, state.ErrStale):
				stale++
			default:
				t.Fatalf("iteration %d: unexpected race error: %v", iteration, result.err)
			}
		}
		if winners != 1 || stale != 1 {
			t.Fatalf("iteration %d: winners=%d stale=%d, want 1/1", iteration, winners, stale)
		}
		stored := f.get(t, id)
		if stored.Version != 1 {
			t.Fatalf("iteration %d: version = %d, want 1", iteration, stored.Version)
		}
		if stored.Status != state.StatusApproved && stored.Status != state.StatusRejected {
			t.Fatalf("iteration %d: final status = %s, want approved or rejected", iteration, stored.Status)
		}
		if got := f.auditCount(t, id); got != 1 {
			t.Fatalf("iteration %d: audit rows = %d, want 1", iteration, got)
		}
	}
}

// TestTransitionConcurrentSameDecision races two identical approvals: one
// winner, one stale loser, one audit row.
func TestTransitionConcurrentSameDecision(t *testing.T) {
	f := newFixture(t)
	approve := humanActor(t, "alice")
	for iteration := 0; iteration < 10; iteration++ {
		id := f.seed(t, state.StatusPendingApproval)
		results := runRace(t, f, id, []state.TransitionRequest{
			{ProposalID: id, To: state.StatusApproved, ExpectedVersion: 0, Actor: approve},
			{ProposalID: id, To: state.StatusApproved, ExpectedVersion: 0, Actor: approve},
		})
		winners, stale := 0, 0
		for _, result := range results {
			switch {
			case result.err == nil:
				winners++
			case errors.Is(result.err, state.ErrStale):
				stale++
			default:
				t.Fatalf("iteration %d: unexpected race error: %v", iteration, result.err)
			}
		}
		if winners != 1 || stale != 1 {
			t.Fatalf("iteration %d: winners=%d stale=%d, want 1/1", iteration, winners, stale)
		}
		stored := f.get(t, id)
		if stored.Status != state.StatusApproved || stored.Version != 1 {
			t.Fatalf("iteration %d: final = %s/version %d, want approved/1",
				iteration, stored.Status, stored.Version)
		}
		if got := f.auditCount(t, id); got != 1 {
			t.Fatalf("iteration %d: audit rows = %d, want 1", iteration, got)
		}
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
