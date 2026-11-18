//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// closeProposal runs the same-transaction hook the way approval.Decide does.
func (f *fixture) closeProposal(t *testing.T, proposalID, status, reason string) error {
	t.Helper()
	return db.WithTx(context.Background(), f.app, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.service.OnProposalClosed(ctx, tx, proposalID, status, reason)
		})
}

// setCaseAttempts seeds the shared attempt counter (agent failures and
// proposal retries write the same cases.attempts column).
func (f *fixture) setCaseAttempts(t *testing.T, caseID string, attempts int) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE cases SET attempts = $2 WHERE id = $1::uuid`, caseID, attempts); err != nil {
		t.Fatalf("set case %s attempts=%d: %v", caseID, attempts, err)
	}
}

// caseTransitionAudits returns the case.transition rows for one case.
func (f *fixture) caseTransitionAudits(t *testing.T, caseID string) []auditRow {
	t.Helper()
	out := make([]auditRow, 0)
	for _, row := range f.audits(t, caseID) {
		if row.Action == "case.transition" {
			out = append(out, row)
		}
	}
	return out
}

func TestOnProposalClosedRetriesAndEscalatesAtCap(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		proposalStatus string
		attempts       int
		wantStatus     string
		wantAttempts   int
		wantReason     string
		wantEscalated  bool
	}{
		{proposalStatus: "rejected", attempts: 0, wantStatus: "open",
			wantAttempts: 1, wantReason: lifecycle.ReasonProposalRejected},
		{proposalStatus: "rejected", attempts: 1, wantStatus: "open",
			wantAttempts: 2, wantReason: lifecycle.ReasonProposalRejected},
		{proposalStatus: "rejected", attempts: 2, wantStatus: "escalated",
			wantAttempts: 3, wantReason: lifecycle.ReasonAttemptsExhausted,
			wantEscalated: true},
		{proposalStatus: "expired", attempts: 0, wantStatus: "open",
			wantAttempts: 1, wantReason: lifecycle.ReasonProposalExpired},
		{proposalStatus: "expired", attempts: 1, wantStatus: "open",
			wantAttempts: 2, wantReason: lifecycle.ReasonProposalExpired},
		{proposalStatus: "expired", attempts: 2, wantStatus: "escalated",
			wantAttempts: 3, wantReason: lifecycle.ReasonAttemptsExhausted,
			wantEscalated: true},
	}
	for _, tc := range tests {
		name := fmt.Sprintf("%s/attempts-%d", tc.proposalStatus, tc.attempts)
		t.Run(name, func(t *testing.T) {
			caseID := f.seedCase(t, "proposal_pending", "")
			f.setCaseAttempts(t, caseID, tc.attempts)
			proposalID := f.seedProposal(t, caseID, tc.proposalStatus)

			if err := f.closeProposal(t, proposalID, tc.proposalStatus, "decision-source"); err != nil {
				t.Fatalf("OnProposalClosed: %v", err)
			}

			stored := f.caseRow(t, caseID)
			if stored.Status != tc.wantStatus || stored.Attempts != tc.wantAttempts {
				t.Fatalf("case = %s/attempts %d, want %s/%d",
					stored.Status, stored.Attempts, tc.wantStatus, tc.wantAttempts)
			}
			if stored.Version != 1 {
				t.Fatalf("case version = %d, want 1 (one transition, attempts must not bump it again)",
					stored.Version)
			}
			if tc.wantEscalated {
				if stored.EscalationReason == nil || *stored.EscalationReason != lifecycle.ReasonAttemptsExhausted {
					t.Fatalf("escalation_reason = %v, want %s",
						stored.EscalationReason, lifecycle.ReasonAttemptsExhausted)
				}
			} else if stored.EscalationReason != nil {
				t.Fatalf("escalation_reason = %q, want nil on open", *stored.EscalationReason)
			}

			transitions := f.caseTransitionAudits(t, caseID)
			if len(transitions) != 1 {
				t.Fatalf("case.transition audits = %d, want 1", len(transitions))
			}
			after := jsonObject(t, transitions[0].After)
			if after["status"] != tc.wantStatus || after["reason"] != tc.wantReason {
				t.Fatalf("transition after = %+v, want %s/%s",
					after, tc.wantStatus, tc.wantReason)
			}
			if transitions[0].ActorType != "system" ||
				transitions[0].ActorID != "case-lifecycle" {
				t.Fatalf("transition actor = %s:%s, want system:case-lifecycle",
					transitions[0].ActorType, transitions[0].ActorID)
			}

			// The repeated hook sees open/escalated: no second increment or
			// transition.
			if err := f.closeProposal(t, proposalID, tc.proposalStatus, "decision-source"); err != nil {
				t.Fatalf("repeated OnProposalClosed: %v", err)
			}
			if repeated := f.caseRow(t, caseID); !equalStoredCase(repeated, stored) {
				t.Fatalf("repeated hook changed case: before %+v after %+v", stored, repeated)
			}
			if got := len(f.caseTransitionAudits(t, caseID)); got != 1 {
				t.Fatalf("case.transition audits after replay = %d, want 1", got)
			}
		})
	}
}

// TestOnProposalClosedCountsPriorAttempts pins the shared counter: attempts
// already consumed by agent failures (the same cases.attempts column) count
// toward the retry cap, so two prior failures plus one rejection escalate.
func TestOnProposalClosedCountsPriorAttempts(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	f.setCaseAttempts(t, caseID, 2)
	proposalID := f.seedProposal(t, caseID, "rejected")

	if err := f.closeProposal(t, proposalID, "rejected", "slack"); err != nil {
		t.Fatalf("OnProposalClosed: %v", err)
	}
	stored := f.caseRow(t, caseID)
	if stored.Status != "escalated" || stored.Attempts != 3 {
		t.Fatalf("case = %s/attempts %d, want escalated/3", stored.Status, stored.Attempts)
	}
	if stored.EscalationReason == nil || *stored.EscalationReason != "attempts_exhausted" {
		t.Fatalf("escalation_reason = %v, want attempts_exhausted", stored.EscalationReason)
	}
}

func TestOnProposalClosedIgnoresOtherProposalStatuses(t *testing.T) {
	f := newFixture(t)
	for _, status := range []string{
		"proposed", "pending_approval", "approved", "auto_approved",
		"applied", "apply_failed", "superseded",
	} {
		t.Run(status, func(t *testing.T) {
			caseID := f.seedCase(t, "proposal_pending", "")
			proposalID := f.seedProposal(t, caseID, status)
			if err := f.closeProposal(t, proposalID, status, ""); err != nil {
				t.Fatalf("OnProposalClosed(%s): %v", status, err)
			}
			stored := f.caseRow(t, caseID)
			if stored.Status != "proposal_pending" || stored.Version != 0 || stored.Attempts != 0 {
				t.Fatalf("case = %+v, want untouched proposal_pending/0/0", stored)
			}
			if got := len(f.audits(t, caseID)); got != 0 {
				t.Fatalf("case audits = %d, want 0", got)
			}
		})
	}
}

func TestOnProposalClosedNeverReopensOtherStatuses(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		status     string
		resolution string
		wantSkip   bool
	}{
		{status: "open"},
		{status: "escalated"},
		{status: "resolved", resolution: "manual", wantSkip: true},
		{status: "in_agent", wantSkip: true},
		{status: "waiting_info", wantSkip: true},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			caseID := f.seedCase(t, tc.status, tc.resolution)
			proposalID := f.seedProposal(t, caseID, "rejected")
			before := f.caseRow(t, caseID)

			if err := f.closeProposal(t, proposalID, "rejected", "slack"); err != nil {
				t.Fatalf("OnProposalClosed: %v", err)
			}
			if after := f.caseRow(t, caseID); !equalStoredCase(after, before) {
				t.Fatalf("case changed from %+v to %+v", before, after)
			}

			audits := f.audits(t, caseID)
			if !tc.wantSkip {
				if len(audits) != 0 {
					t.Fatalf("audits = %d, want 0 for idempotent open/escalated no-op", len(audits))
				}
				return
			}
			if len(audits) != 1 || audits[0].Action != lifecycle.ActionCaseRetrySkipped {
				t.Fatalf("audits = %+v, want one %s", audits, lifecycle.ActionCaseRetrySkipped)
			}
			after := jsonObject(t, audits[0].After)
			if after["proposal_id"] != proposalID || after["skipped_reason"] != "case_not_proposal_pending" {
				t.Fatalf("retry_skipped after = %+v, want proposal id and skip reason", after)
			}

			// A repeated hook for the same (case, proposal) must not append a
			// second skip audit.
			if err := f.closeProposal(t, proposalID, "rejected", "slack"); err != nil {
				t.Fatalf("repeated OnProposalClosed: %v", err)
			}
			if got := len(f.audits(t, caseID)); got != 1 {
				t.Fatalf("audits after replay = %d, want 1", got)
			}
		})
	}
}

func TestRetryStalledProposalsProcessesBatchesAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	stalled := make([]string, 0, 3)
	for attempts := 0; attempts < 3; attempts++ {
		caseID := f.seedCase(t, "proposal_pending", "")
		f.setCaseAttempts(t, caseID, attempts)
		f.seedProposal(t, caseID, []string{"rejected", "expired", "rejected"}[attempts])
		stalled = append(stalled, caseID)
	}
	openCase := f.seedCase(t, "open", "")
	f.seedProposal(t, openCase, "rejected")
	approvedCase := f.seedCase(t, "proposal_pending", "")
	f.seedProposal(t, approvedCase, "approved")
	resolvedCase := f.seedCase(t, "resolved", "manual")
	f.seedProposal(t, resolvedCase, "expired")

	result, err := f.service.RetryStalledProposals(context.Background(), 10)
	if err != nil {
		t.Fatalf("RetryStalledProposals: %v", err)
	}
	want := lifecycle.RetryStalledResult{Processed: 3, Retried: 2, Escalated: 1}
	if result != want {
		t.Fatalf("result = %+v, want %+v", result, want)
	}
	for i, caseID := range stalled {
		stored := f.caseRow(t, caseID)
		if i < 2 {
			if stored.Status != "open" || stored.Attempts != i+1 {
				t.Fatalf("stalled case %d = %s/attempts %d, want open/%d",
					i, stored.Status, stored.Attempts, i+1)
			}
		} else if stored.Status != "escalated" || stored.Attempts != 3 {
			t.Fatalf("stalled case %d = %s/attempts %d, want escalated/3",
				i, stored.Status, stored.Attempts)
		}
	}
	if got := f.caseRow(t, openCase); got.Status != "open" || got.Attempts != 0 {
		t.Fatalf("open case = %+v, want untouched", got)
	}
	if got := f.caseRow(t, approvedCase); got.Status != "proposal_pending" || got.Attempts != 0 {
		t.Fatalf("approved-proposal case = %+v, want untouched", got)
	}
	if got := f.caseRow(t, resolvedCase); got.Status != "resolved" || got.Attempts != 0 {
		t.Fatalf("resolved case = %+v, want untouched", got)
	}

	replay, err := f.service.RetryStalledProposals(context.Background(), 10)
	if err != nil {
		t.Fatalf("repeated RetryStalledProposals: %v", err)
	}
	if replay != (lifecycle.RetryStalledResult{}) {
		t.Fatalf("repeated sweep = %+v, want zero", replay)
	}
}

func TestRetryStalledProposalsHonoursLimitAndSkipsLocked(t *testing.T) {
	f := newFixture(t)
	casesCreated := make([]string, 0, 3)
	proposals := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		caseID := f.seedCase(t, "proposal_pending", "")
		proposals = append(proposals, f.seedProposal(t, caseID, "rejected"))
		casesCreated = append(casesCreated, caseID)
	}

	lockCtx := context.Background()
	lockTx, err := f.db.Pool.Begin(lockCtx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.WithoutCancel(lockCtx)) }()
	var lockedID string
	if err := lockTx.QueryRow(lockCtx,
		`SELECT id::text FROM proposals WHERE id = $1::uuid FOR UPDATE`,
		proposals[0]).Scan(&lockedID); err != nil {
		t.Fatalf("lock proposal %s: %v", proposals[0], err)
	}

	result, err := f.service.RetryStalledProposals(context.Background(), 10)
	if err != nil {
		t.Fatalf("RetryStalledProposals: %v", err)
	}
	if result != (lifecycle.RetryStalledResult{Processed: 2, Retried: 2}) {
		t.Fatalf("result = %+v, want two processed/retried (locked row skipped)", result)
	}
	if got := f.caseRow(t, casesCreated[0]); got.Status != "proposal_pending" || got.Attempts != 0 {
		t.Fatalf("locked case = %+v, want untouched", got)
	}

	if err := lockTx.Rollback(lockCtx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	limited, err := f.service.RetryStalledProposals(context.Background(), 1)
	if err != nil {
		t.Fatalf("limited RetryStalledProposals: %v", err)
	}
	if limited != (lifecycle.RetryStalledResult{Processed: 1, Retried: 1}) {
		t.Fatalf("limited sweep = %+v, want one processed/retried", limited)
	}
	if got := f.caseRow(t, casesCreated[0]); got.Status != "open" || got.Attempts != 1 {
		t.Fatalf("previously locked case = %+v, want open/1", got)
	}
}

func TestRetryHookAndSweepProduceSameCaseState(t *testing.T) {
	f := newFixture(t)
	hookCase := f.seedCase(t, "proposal_pending", "")
	hookProposal := f.seedProposal(t, hookCase, "rejected")
	sweepCase := f.seedCase(t, "proposal_pending", "")
	f.seedProposal(t, sweepCase, "rejected")

	if err := f.closeProposal(t, hookProposal, "rejected", "slack"); err != nil {
		t.Fatalf("OnProposalClosed: %v", err)
	}
	result, err := f.service.RetryStalledProposals(context.Background(), 10)
	if err != nil {
		t.Fatalf("RetryStalledProposals: %v", err)
	}
	if result != (lifecycle.RetryStalledResult{Processed: 1, Retried: 1}) {
		t.Fatalf("sweep = %+v, want only the second case", result)
	}

	fromHook := f.caseRow(t, hookCase)
	fromSweep := f.caseRow(t, sweepCase)
	if !equalStoredCase(fromHook, fromSweep) {
		t.Fatalf("hook case = %+v, sweep case = %+v, want identical state",
			fromHook, fromSweep)
	}
	hookAudit := f.caseTransitionAudits(t, hookCase)
	sweepAudit := f.caseTransitionAudits(t, sweepCase)
	if len(hookAudit) != 1 || len(sweepAudit) != 1 {
		t.Fatalf("transition audits = %d/%d, want 1/1", len(hookAudit), len(sweepAudit))
	}
	hookAfter := jsonObject(t, hookAudit[0].After)
	sweepAfter := jsonObject(t, sweepAudit[0].After)
	if hookAfter["status"] != sweepAfter["status"] || hookAfter["reason"] != sweepAfter["reason"] {
		t.Fatalf("hook audit after = %+v, sweep audit after = %+v, want same status/reason",
			hookAfter, sweepAfter)
	}
	if err := f.closeProposal(t, hookProposal, "rejected", "slack"); err != nil {
		t.Fatalf("replayed hook after sweep: %v", err)
	}
	if got := f.caseRow(t, hookCase); !equalStoredCase(got, fromHook) {
		t.Fatalf("replayed hook changed case: %+v -> %+v", fromHook, got)
	}
}
