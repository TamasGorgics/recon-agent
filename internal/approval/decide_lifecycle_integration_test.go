//go:build integration

// covers: FIN-24, FIN-30, FIN-33

package approval_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// hookedService rebuilds the fixture's approval service with an M4-10
// proposal-closed hook. Everything else (pool, state, verifier, limits, policy)
// is the fixture's normal dependency set.
func (f *fixture) hookedService(t *testing.T, hook approval.ProposalClosedHook) *approval.Service {
	t.Helper()
	service, err := approval.NewService(approval.Options{
		Pool:             f.app,
		Clock:            f.clock,
		State:            f.state,
		Verifier:         f.verifier,
		Limits:           f.limits,
		Policy:           approval.Policy{Engine: f.engine, Loaded: f.loaded},
		TTL:              testTTL,
		OnProposalClosed: hook,
	})
	if err != nil {
		t.Fatalf("approval.NewService with hook: %v", err)
	}
	return service
}

// lifecycleService builds the real M4-10 service on the fixture pools.
func (f *fixture) lifecycleService(t *testing.T) *lifecycle.Service {
	t.Helper()
	service, err := lifecycle.NewService(lifecycle.Options{
		DB:    f.app,
		Clock: f.clock,
	})
	if err != nil {
		t.Fatalf("lifecycle.NewService: %v", err)
	}
	return service
}

// pendingProposalInCaseStatus routes a T1 proposal and seeds its case in the
// supplied status, so the M4-10 hook sees a real proposal_pending case.
func (f *fixture) pendingProposalInCaseStatus(t *testing.T, caseStatus string) (proposalID, caseID string) {
	t.Helper()
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT1)
	proposalID, caseID, _ = f.seedProposal(proposalSpec{
		Type:       "link",
		Payload:    linkPayload(),
		CaseStatus: caseStatus,
	})
	if _, err := f.service.Request(context.Background(), proposalID); err != nil {
		f.t.Fatalf("Request pending proposal: %v", err)
	}
	return proposalID, caseID
}

func TestDecideRejectAdvancesCaseInDecisionTransaction(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID := f.pendingProposalInCaseStatus(t, "proposal_pending")
	approverID, _ := f.seedApprover(approverSpec{Role: "approver"})
	proposalAuditsBefore := len(f.audits(proposalID))

	service := f.hookedService(t, f.lifecycleService(t).OnProposalClosed)
	result, err := service.Decide(context.Background(), proposalID,
		approval.DecisionRejected, approverID, "slack")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if result.Status != "rejected" || result.Replayed {
		t.Fatalf("result = %+v, want a fresh rejected decision", result)
	}

	// M4-11 rows/audits are unchanged by the hook.
	if rows := f.approvalRows(proposalID); len(rows) != 1 {
		t.Fatalf("approvals rows = %d, want 1", len(rows))
	}
	if got := len(f.audits(proposalID)); got != proposalAuditsBefore+2 {
		t.Fatalf("proposal audits = %d, want %d", got, proposalAuditsBefore+2)
	}

	// The real case advanced in the same transaction.
	stored := f.readCase(caseID)
	if stored.Status != "open" || stored.Attempts != 1 || stored.Version != 1 {
		t.Fatalf("case = %s/attempts %d/version %d, want open/1/1",
			stored.Status, stored.Attempts, stored.Version)
	}
	caseAudits := f.caseAudits(caseID)
	if len(caseAudits) != 1 {
		t.Fatalf("case audits = %d, want 1", len(caseAudits))
	}
	if caseAudits[0].Action != "case.transition" ||
		caseAudits[0].ActorType != "system" || caseAudits[0].ActorID != "case-lifecycle" {
		t.Fatalf("case audit = %+v, want system:case-lifecycle case.transition", caseAudits[0])
	}
	after := decodeObject(t, caseAudits[0].After)
	if after["status"] != "open" || after["reason"] != lifecycle.ReasonProposalRejected {
		t.Fatalf("case transition after = %+v, want open/%s",
			after, lifecycle.ReasonProposalRejected)
	}

	// Replaying the same rejection changes neither decision nor case.
	replay, err := service.Decide(context.Background(), proposalID,
		approval.DecisionRejected, approverID, "slack")
	if err != nil {
		t.Fatalf("replayed Decide: %v", err)
	}
	if !replay.Replayed {
		t.Fatalf("replayed result = %+v, want Replayed", replay)
	}
	if got := f.readCase(caseID); got != stored {
		t.Fatalf("replayed decision changed case: %+v -> %+v", stored, got)
	}
}

func TestDecideLateExpiryAdvancesCaseInDecisionTransaction(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID := f.pendingProposalInCaseStatus(t, "proposal_pending")
	approverID, _ := f.seedApprover(approverSpec{})
	f.clock.Advance(testTTL)

	service := f.hookedService(t, f.lifecycleService(t).OnProposalClosed)
	_, err := service.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api")
	if !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("late Decide error = %v, want ErrExpired", err)
	}
	if stored := f.readProposal(proposalID); stored.Status != "expired" {
		t.Fatalf("proposal status = %q, want expired", stored.Status)
	}
	stored := f.readCase(caseID)
	if stored.Status != "open" || stored.Attempts != 1 || stored.Version != 1 {
		t.Fatalf("case = %s/attempts %d/version %d, want open/1/1",
			stored.Status, stored.Attempts, stored.Version)
	}
	caseAudits := f.caseAudits(caseID)
	if len(caseAudits) != 1 {
		t.Fatalf("case audits = %d, want 1", len(caseAudits))
	}
	after := decodeObject(t, caseAudits[0].After)
	if after["status"] != "open" || after["reason"] != lifecycle.ReasonProposalExpired {
		t.Fatalf("case transition after = %+v, want open/%s",
			after, lifecycle.ReasonProposalExpired)
	}
}

func TestDecideHookFailureRollsBackDecision(t *testing.T) {
	boom := errors.New("injected lifecycle failure")
	tests := []struct {
		name     string
		decision approval.Decision
		advance  bool
	}{
		{name: "rejected", decision: approval.DecisionRejected},
		{name: "late expiry", decision: approval.DecisionApproved, advance: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			proposalID, caseID := f.pendingProposalInCaseStatus(t, "proposal_pending")
			approverID, _ := f.seedApprover(approverSpec{})
			proposalAuditsBefore := len(f.audits(proposalID))
			beforeProposal := f.readProposal(proposalID)
			beforeCase := f.readCase(caseID)
			if tc.advance {
				f.clock.Advance(testTTL)
			}
			service := f.hookedService(t, func(context.Context, pgx.Tx, string, string, string) error {
				return boom
			})

			_, err := service.Decide(context.Background(), proposalID,
				tc.decision, approverID, "slack")
			if !errors.Is(err, boom) {
				t.Fatalf("Decide error = %v, want the hook error", err)
			}

			afterProposal := f.readProposal(proposalID)
			if afterProposal.Status != beforeProposal.Status || afterProposal.Version != beforeProposal.Version {
				t.Fatalf("proposal changed from %s/%d to %s/%d, want rollback",
					beforeProposal.Status, beforeProposal.Version,
					afterProposal.Status, afterProposal.Version)
			}
			if rows := f.approvalRows(proposalID); len(rows) != 0 {
				t.Fatalf("approvals rows = %d, want 0 after rollback", len(rows))
			}
			if _, ok := f.auditByAction(proposalID, approval.ActionApprovalDecided); ok {
				t.Fatal("approval.decided audit survived the rollback")
			}
			if got := len(f.audits(proposalID)); got != proposalAuditsBefore {
				t.Fatalf("proposal audits = %d, want %d after rollback", got, proposalAuditsBefore)
			}
			if got := f.readCase(caseID); got != beforeCase {
				t.Fatalf("case changed from %+v to %+v, want rollback", beforeCase, got)
			}
			if got := len(f.caseAudits(caseID)); got != 0 {
				t.Fatalf("case audits = %d, want 0 after rollback", got)
			}
		})
	}
}
