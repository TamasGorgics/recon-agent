//go:build integration

// covers: FIN-30, FIN-33
package approval_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// pendingProposal routes a T1 link proposal through Request so Decide operates
// on the real state machine row (status pending_approval, version 1, a TTL).
func (f *fixture) pendingProposal(t *testing.T) (proposalID, caseID, runID string) {
	t.Helper()
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT1)
	proposalID, caseID, runID = f.seedProposal(proposalSpec{
		Type:    "link",
		Payload: linkPayload(),
	})
	if _, err := f.service.Request(context.Background(), proposalID); err != nil {
		f.t.Fatalf("Request pending proposal: %v", err)
	}
	return proposalID, caseID, runID
}

func boolPtr(value bool) *bool { return &value }

func TestDecideApproveWritesApprovalTransitionAndAudit(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID, _ := f.pendingProposal(t)
	approverID, _ := f.seedApprover(approverSpec{Role: "approver"})
	before := f.readProposal(proposalID)
	auditsBefore := len(f.audits(proposalID))

	result, err := f.service.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := approval.DecisionResult{
		ProposalID: proposalID,
		Status:     "approved",
		Decision:   approval.DecisionApproved,
		ApproverID: approverID,
		DecidedAt:  f.testNowUTC,
		Replayed:   false,
	}
	assertDecisionResult(t, result, want)

	stored := f.readProposal(proposalID)
	if stored.Status != "approved" || stored.Version != before.Version+1 {
		t.Fatalf("stored status/version = %s/%d, want approved/%d",
			stored.Status, stored.Version, before.Version+1)
	}
	if before.ExpiresAt == nil || stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(*before.ExpiresAt) {
		t.Fatalf("expires_at changed on decision: before %v after %v", before.ExpiresAt, stored.ExpiresAt)
	}
	if got := f.readCaseStatus(caseID); got != "open" {
		t.Fatalf("case status = %q, want unchanged open (M4-10 owns lifecycle)", got)
	}

	rows := f.approvalRows(proposalID)
	if len(rows) != 1 {
		t.Fatalf("approvals rows = %d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Channel != "api" || row.ExternalRef != nil || row.ApproverID != approverID ||
		row.Decision != "approved" || !row.SignatureValid {
		t.Fatalf("approval row = %+v, want api/NULL/%s/approved/signature_valid", row, approverID)
	}
	if !row.DecidedAt.Equal(f.testNowUTC) {
		t.Fatalf("decided_at = %v, want %v", row.DecidedAt, f.testNowUTC)
	}

	audits := f.audits(proposalID)
	if len(audits) != auditsBefore+2 {
		t.Fatalf("audit rows = %d, want %d decision writes: %+v",
			len(audits), auditsBefore+2, audits)
	}
	transition := audits[auditsBefore]
	if transition.Action != state.ActionProposalTransition || transition.ActorType != "human" ||
		transition.ActorID != approverID {
		t.Fatalf("decision transition = %+v, want human:%s proposal.transition", transition, approverID)
	}
	afterTransition := decodeObject(t, transition.After)
	if afterTransition["status"] != "approved" {
		t.Fatalf("transition after status = %v, want approved", afterTransition["status"])
	}
	if _, ok := afterTransition["reason"]; ok {
		t.Fatalf("approved transition has reason %v, want absent/empty", afterTransition["reason"])
	}

	decided := audits[auditsBefore+1]
	if decided.Action != approval.ActionApprovalDecided || decided.ActorType != "human" ||
		decided.ActorID != approverID {
		t.Fatalf("decision audit = %+v, want human:%s %s",
			decided, approverID, approval.ActionApprovalDecided)
	}
	beforeDecision := decodeObject(t, decided.Before)
	if beforeDecision["status"] != "pending_approval" {
		t.Fatalf("decision audit before status = %v, want pending_approval", beforeDecision["status"])
	}
	afterDecision := decodeObject(t, decided.After)
	wantAfter := map[string]any{
		"status":      "approved",
		"version":     float64(before.Version + 1),
		"decision":    "approved",
		"source":      "api",
		"approver_id": approverID,
	}
	for key, wantValue := range wantAfter {
		if afterDecision[key] != wantValue {
			t.Fatalf("decision audit after[%s] = %v, want %v", key, afterDecision[key], wantValue)
		}
	}
}

func TestDecideRejectWritesApprovalTransitionAndAudit(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID, _ := f.pendingProposal(t)
	approverID, _ := f.seedApprover(approverSpec{Role: "admin"})
	auditsBefore := len(f.audits(proposalID))

	result, err := f.service.Decide(context.Background(), proposalID,
		approval.DecisionRejected, approverID, "slack")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	assertDecisionResult(t, result, approval.DecisionResult{
		ProposalID: proposalID,
		Status:     "rejected",
		Decision:   approval.DecisionRejected,
		ApproverID: approverID,
		DecidedAt:  f.testNowUTC,
		Replayed:   false,
	})

	stored := f.readProposal(proposalID)
	if stored.Status != "rejected" || stored.Version != 2 {
		t.Fatalf("stored status/version = %s/%d, want rejected/2", stored.Status, stored.Version)
	}
	if got := f.readCaseStatus(caseID); got != "open" {
		t.Fatalf("case status = %q, want unchanged open (M4-10 owns lifecycle)", got)
	}

	rows := f.approvalRows(proposalID)
	if len(rows) != 1 {
		t.Fatalf("approvals rows = %d, want 1", len(rows))
	}
	if rows[0].Channel != "slack" || rows[0].Decision != "rejected" {
		t.Fatalf("approval row = %+v, want slack/rejected", rows[0])
	}

	audits := f.audits(proposalID)
	if len(audits) != auditsBefore+2 {
		t.Fatalf("audit rows = %d, want %d", len(audits), auditsBefore+2)
	}
	transition := audits[auditsBefore]
	afterTransition := decodeObject(t, transition.After)
	if afterTransition["status"] != "rejected" {
		t.Fatalf("transition status = %v, want rejected", afterTransition["status"])
	}
	// pending_approval -> rejected requires a reason; Decide uses the source.
	if afterTransition["reason"] != "slack" {
		t.Fatalf("rejected transition reason = %v, want slack", afterTransition["reason"])
	}
	decided := decodeObject(t, audits[auditsBefore+1].After)
	if decided["status"] != "rejected" || decided["decision"] != "rejected" || decided["source"] != "slack" {
		t.Fatalf("decision audit after = %+v, want rejected/slack", decided)
	}
}

func TestDecideReplayIsNoOp(t *testing.T) {
	f := newFixture(t)
	proposalID, _, _ := f.pendingProposal(t)
	approverID, _ := f.seedApprover(approverSpec{})

	first, err := f.service.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api")
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	storedBefore := f.readProposal(proposalID)
	auditsBefore := len(f.audits(proposalID))

	second, err := f.service.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api")
	if err != nil {
		t.Fatalf("replayed Decide: %v", err)
	}
	if !second.Replayed {
		t.Fatal("second Decide Replayed = false, want true")
	}
	if second.ProposalID != first.ProposalID || second.Status != first.Status ||
		second.Decision != first.Decision || second.ApproverID != first.ApproverID ||
		!second.DecidedAt.Equal(first.DecidedAt) {
		t.Fatalf("replay result = %+v, want the stored decision %+v", second, first)
	}
	if storedAfter := f.readProposal(proposalID); storedAfter.Status != storedBefore.Status ||
		storedAfter.Version != storedBefore.Version {
		t.Fatalf("replay changed proposal: before %+v after %+v", storedBefore, storedAfter)
	}
	if rows := f.approvalRows(proposalID); len(rows) != 1 {
		t.Fatalf("approvals rows after replay = %d, want 1", len(rows))
	}
	if got := len(f.audits(proposalID)); got != auditsBefore {
		t.Fatalf("audit rows after replay = %d, want %d", got, auditsBefore)
	}
}

// TestDecideReplayAfterApply proves a late double-click is still idempotent
// after the executor has moved an approved proposal to applied.
func TestDecideReplayAfterApply(t *testing.T) {
	f := newFixture(t)
	proposalID, _, _ := f.pendingProposal(t)
	approverID, _ := f.seedApprover(approverSpec{})
	if _, err := f.service.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api"); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE proposals SET status = 'applied', version = version + 1
		  WHERE id = $1::uuid`, proposalID); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	auditsBefore := len(f.audits(proposalID))
	result, err := f.service.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api")
	if err != nil {
		t.Fatalf("late replayed Decide: %v", err)
	}
	if !result.Replayed || result.Status != "applied" {
		t.Fatalf("late replay = %+v, want replayed applied", result)
	}
	if got := len(f.audits(proposalID)); got != auditsBefore {
		t.Fatalf("late replay audit rows = %d, want %d", got, auditsBefore)
	}
	if rows := f.approvalRows(proposalID); len(rows) != 1 {
		t.Fatalf("late replay approvals rows = %d, want 1", len(rows))
	}
}

func TestDecideConflicts(t *testing.T) {
	tests := []struct {
		name           string
		sameApprover   bool
		secondDecision approval.Decision
	}{
		{name: "same approver different decision", sameApprover: true, secondDecision: approval.DecisionRejected},
		{name: "other approver same decision", sameApprover: false, secondDecision: approval.DecisionApproved},
		{name: "other approver different decision", sameApprover: false, secondDecision: approval.DecisionRejected},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			proposalID, _, _ := f.pendingProposal(t)
			approverID, _ := f.seedApprover(approverSpec{})
			if _, err := f.service.Decide(context.Background(), proposalID,
				approval.DecisionApproved, approverID, "api"); err != nil {
				t.Fatalf("first Decide: %v", err)
			}
			storedBefore := f.readProposal(proposalID)
			auditsBefore := len(f.audits(proposalID))

			decider := approverID
			if !tc.sameApprover {
				decider, _ = f.seedApprover(approverSpec{})
			}
			_, err := f.service.Decide(context.Background(), proposalID,
				tc.secondDecision, decider, "api")
			if !errors.Is(err, approval.ErrDecisionConflict) {
				t.Fatalf("second Decide error = %v, want ErrDecisionConflict", err)
			}
			if storedAfter := f.readProposal(proposalID); storedAfter.Status != storedBefore.Status ||
				storedAfter.Version != storedBefore.Version {
				t.Fatalf("conflict changed proposal: before %+v after %+v", storedBefore, storedAfter)
			}
			if rows := f.approvalRows(proposalID); len(rows) != 1 {
				t.Fatalf("approvals rows after conflict = %d, want 1", len(rows))
			}
			if got := len(f.audits(proposalID)); got != auditsBefore {
				t.Fatalf("audit rows after conflict = %d, want %d", got, auditsBefore)
			}
		})
	}
}

func TestDecideExpiresLateDecision(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID, _ := f.pendingProposal(t)
	approverID, _ := f.seedApprover(approverSpec{})
	before := f.readProposal(proposalID)

	// expires_at is now + TTL; advancing exactly TTL makes it due.
	f.clock.Advance(testTTL)

	_, err := f.service.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "api")
	if !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("late Decide error = %v, want ErrExpired", err)
	}
	stored := f.readProposal(proposalID)
	if stored.Status != "expired" || stored.Version != before.Version+1 {
		t.Fatalf("stored status/version = %s/%d, want expired/%d",
			stored.Status, stored.Version, before.Version+1)
	}
	if rows := f.approvalRows(proposalID); len(rows) != 0 {
		t.Fatalf("approvals rows after expiry = %d, want 0", len(rows))
	}
	if _, ok := f.auditByAction(proposalID, approval.ActionApprovalDecided); ok {
		t.Fatal("expired late decision wrote an approval.decided audit")
	}

	// The late decision adds exactly one transition audit: system:expiry.
	audits := f.audits(proposalID)
	last := audits[len(audits)-1]
	if last.Action != state.ActionProposalTransition || last.ActorType != "system" ||
		last.ActorID != "expiry" {
		t.Fatalf("expiry audit = %+v, want system:expiry proposal.transition", last)
	}
	after := decodeObject(t, last.After)
	if after["status"] != "expired" || after["reason"] != approval.ReasonExpired {
		t.Fatalf("expiry audit after = %+v, want expired/%s", after, approval.ReasonExpired)
	}
	if got := f.readCaseStatus(caseID); got != "open" {
		t.Fatalf("case status = %q, want unchanged open", got)
	}
}

func TestDecideRejectsApproverIdentities(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		active   *bool
		wantErr  error
		unknown  bool
		decision approval.Decision
		source   string
	}{
		{name: "inactive approver", role: "approver", active: boolPtr(false), wantErr: approval.ErrApproverInactive},
		{name: "viewer role", role: "viewer", wantErr: approval.ErrApproverRole},
		{name: "unknown approver", unknown: true, wantErr: approval.ErrApproverNotFound},
		{name: "invalid decision", wantErr: approval.ErrInvalidDecision, decision: "maybe"},
		{name: "empty source", wantErr: approval.ErrInvalidRequest, source: "   "},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			proposalID, _, _ := f.pendingProposal(t)
			approverID := "00000000-0000-4000-8000-0000000000aa"
			if !tc.unknown {
				approverID, _ = f.seedApprover(approverSpec{Role: tc.role, Active: tc.active})
			}
			decision := tc.decision
			if decision == "" {
				decision = approval.DecisionApproved
			}
			source := tc.source
			if source == "" {
				source = "api"
			}
			if _, err := f.service.Decide(context.Background(), proposalID,
				decision, approverID, source); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Decide error = %v, want %v", err, tc.wantErr)
			}
			assertNoDecisionWrites(t, f, proposalID, "pending_approval", 1)
		})
	}

	t.Run("agent and system actor strings", func(t *testing.T) {
		f := newFixture(t)
		proposalID, _, _ := f.pendingProposal(t)
		for _, actor := range []string{
			"agent:deepseek-v4.1-flash",
			"system:executor",
			"agent",
			"system",
		} {
			if _, err := f.service.Decide(context.Background(), proposalID,
				approval.DecisionApproved, actor, "api"); !errors.Is(err, approval.ErrAgentActor) {
				t.Fatalf("Decide(%q) error = %v, want ErrAgentActor", actor, err)
			}
		}
		assertNoDecisionWrites(t, f, proposalID, "pending_approval", 1)
	})

	t.Run("malformed ids", func(t *testing.T) {
		f := newFixture(t)
		proposalID, _, _ := f.pendingProposal(t)
		approverID, _ := f.seedApprover(approverSpec{})
		if _, err := f.service.Decide(context.Background(), "not-a-uuid",
			approval.DecisionApproved, approverID, "api"); !errors.Is(err, approval.ErrInvalidRequest) {
			t.Fatalf("malformed proposal error = %v, want ErrInvalidRequest", err)
		}
		if _, err := f.service.Decide(context.Background(), proposalID,
			approval.DecisionApproved, "not-a-uuid", "api"); !errors.Is(err, approval.ErrInvalidRequest) {
			t.Fatalf("malformed approver error = %v, want ErrInvalidRequest", err)
		}
		assertNoDecisionWrites(t, f, proposalID, "pending_approval", 1)
	})

	t.Run("proposal is not pending approval", func(t *testing.T) {
		f := newFixture(t)
		proposalID, _, _ := f.seedProposal(proposalSpec{Type: "link", Payload: linkPayload()})
		approverID, _ := f.seedApprover(approverSpec{})
		if _, err := f.service.Decide(context.Background(), proposalID,
			approval.DecisionApproved, approverID, "api"); !errors.Is(err, approval.ErrNotPending) {
			t.Fatalf("Decide error = %v, want ErrNotPending", err)
		}
		assertNoDecisionWrites(t, f, proposalID, "proposed", 0)
	})
}

func TestDecideRejectsCoMaker(t *testing.T) {
	tests := []struct {
		name       string
		entityType string
		entity     string
		useAPIKey  bool
	}{
		{name: "approver UUID on the proposal", entityType: "proposal", entity: "proposal"},
		{name: "approver API key on the case", entityType: "case", entity: "case", useAPIKey: true},
		{name: "approver UUID on the agent run", entityType: "agent_run", entity: "run"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			proposalID, caseID, runID := f.pendingProposal(t)
			approverID, apiKeyID := f.seedApprover(approverSpec{
				APIKeyID: strPtr("m4-11-co-maker-key"),
			})

			actorID := approverID
			entityID := proposalID
			switch tc.entity {
			case "case":
				entityID = caseID
			case "run":
				entityID = runID
			}
			if tc.useAPIKey {
				actorID = apiKeyID
			}
			f.appendHumanAudit(tc.entityType, entityID, actorID, tc.entityType+".maker_action")

			var err error
			if tc.useAPIKey {
				_, err = f.service.DecideByAPIKey(context.Background(), proposalID,
					approval.DecisionApproved, apiKeyID, "api")
			} else {
				_, err = f.service.Decide(context.Background(), proposalID,
					approval.DecisionApproved, approverID, "api")
			}
			if !errors.Is(err, approval.ErrCoMaker) {
				t.Fatalf("Decide error = %v, want ErrCoMaker", err)
			}

			stored := f.readProposal(proposalID)
			if stored.Status != "pending_approval" || stored.Version != 1 {
				t.Fatalf("co-maker decision changed proposal: %+v", stored)
			}
			if rows := f.approvalRows(proposalID); len(rows) != 0 {
				t.Fatalf("approvals rows = %d, want 0", len(rows))
			}
			if _, ok := f.auditByAction(proposalID, approval.ActionApprovalDecided); ok {
				t.Fatal("co-maker decision wrote an approval.decided audit")
			}
		})
	}
}

func TestDecideByAPIKeyMapsApprover(t *testing.T) {
	f := newFixture(t)
	proposalID, _, _ := f.pendingProposal(t)
	approverID, apiKeyID := f.seedApprover(approverSpec{
		Role:     "approver",
		APIKeyID: strPtr("m4-11-api-key-approver"),
	})
	if apiKeyID != "m4-11-api-key-approver" {
		t.Fatalf("seeded api key = %q", apiKeyID)
	}

	result, err := f.service.DecideByAPIKey(context.Background(), proposalID,
		approval.DecisionApproved, apiKeyID, "api")
	if err != nil {
		t.Fatalf("DecideByAPIKey: %v", err)
	}
	if result.ApproverID != approverID || result.Status != "approved" || result.Decision != approval.DecisionApproved {
		t.Fatalf("DecideByAPIKey result = %+v, want approver %s approved", result, approverID)
	}
	rows := f.approvalRows(proposalID)
	if len(rows) != 1 || rows[0].ApproverID != approverID {
		t.Fatalf("approval rows = %+v, want approver %s", rows, approverID)
	}
	decided, ok := f.auditByAction(proposalID, approval.ActionApprovalDecided)
	if !ok {
		t.Fatal("approval.decided audit is missing")
	}
	if decided.ActorType != "human" || decided.ActorID != approverID {
		t.Fatalf("decision actor = %s:%s, want human:%s", decided.ActorType, decided.ActorID, approverID)
	}
}

func TestDecideByAPIKeyTypedErrors(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		role    string
		active  *bool
		seed    bool
		wantErr error
	}{
		{name: "unknown key", apiKey: "m4-11-missing-key", wantErr: approval.ErrApproverNotFound},
		{name: "inactive approver", apiKey: "m4-11-inactive-key", active: boolPtr(false), seed: true, wantErr: approval.ErrApproverInactive},
		{name: "viewer approver", apiKey: "m4-11-viewer-key", role: "viewer", seed: true, wantErr: approval.ErrApproverRole},
		{name: "empty key", apiKey: "   ", wantErr: approval.ErrInvalidRequest},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			proposalID, _, _ := f.pendingProposal(t)
			if tc.seed {
				role := tc.role
				if role == "" {
					role = "approver"
				}
				f.seedApprover(approverSpec{
					Role:     role,
					Active:   tc.active,
					APIKeyID: strPtr(tc.apiKey),
				})
			}
			if _, err := f.service.DecideByAPIKey(context.Background(), proposalID,
				approval.DecisionApproved, tc.apiKey, "api"); !errors.Is(err, tc.wantErr) {
				t.Fatalf("DecideByAPIKey error = %v, want %v", err, tc.wantErr)
			}
			assertNoDecisionWrites(t, f, proposalID, "pending_approval", 1)
		})
	}
}

func assertDecisionResult(t *testing.T, got, want approval.DecisionResult) {
	t.Helper()
	if got.ProposalID != want.ProposalID || got.Status != want.Status ||
		got.Decision != want.Decision || got.ApproverID != want.ApproverID ||
		got.Replayed != want.Replayed || !got.DecidedAt.Equal(want.DecidedAt) {
		t.Fatalf("DecisionResult = %+v, want %+v", got, want)
	}
}

// assertNoDecisionWrites proves a rejected decision left the proposal in its
// expected status/version and wrote no approval row or approval.decided audit.
func assertNoDecisionWrites(t *testing.T, f *fixture, proposalID, wantStatus string, wantVersion int) {
	t.Helper()
	stored := f.readProposal(proposalID)
	if stored.Status != wantStatus || stored.Version != wantVersion {
		t.Fatalf("proposal changed after rejected decision: %+v, want %s/%d",
			stored, wantStatus, wantVersion)
	}
	if rows := f.approvalRows(proposalID); len(rows) != 0 {
		t.Fatalf("approvals rows = %d, want 0", len(rows))
	}
	if _, ok := f.auditByAction(proposalID, approval.ActionApprovalDecided); ok {
		t.Fatal("rejected decision wrote an approval.decided audit")
	}
}
