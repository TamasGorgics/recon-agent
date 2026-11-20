//go:build integration

// covers: FIN-30, FIN-33

package approval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval"
)

// decisionServiceWith rebuilds the fixture's approval service with the normal
// dependency set and the caller's optional hooks.
func (f *fixture) decisionServiceWith(t *testing.T, opts approval.Options) *approval.Service {
	t.Helper()
	opts.Pool = f.app
	opts.Clock = f.clock
	opts.State = f.state
	opts.Verifier = f.verifier
	opts.Limits = f.limits
	opts.Policy = approval.Policy{Engine: f.engine, Loaded: f.loaded}
	opts.TTL = testTTL
	service, err := approval.NewService(opts)
	if err != nil {
		t.Fatalf("approval.NewService: %v", err)
	}
	return service
}

// approvalUpdateRows returns only approval.update outbox rows for a proposal.
func (f *fixture) approvalUpdateRows(proposalID string) []outboxRow {
	f.t.Helper()
	var updates []outboxRow
	for _, row := range f.outboxRows(proposalID) {
		if row.Topic == approval.TopicApprovalUpdate {
			updates = append(updates, row)
		}
	}
	return updates
}

func decodeUpdatePayload(t *testing.T, row outboxRow) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("decode approval.update payload: %v", err)
	}
	return payload
}

// TestDecideInfoRequestedUsesLifecycleHook proves the info-requested path
// rejects the proposal with the constant transition reason, calls the dedicated
// lifecycle hook instead of OnProposalClosed in the same transaction, moves the
// case to waiting_info without an attempt, and enqueues exactly one update.
func TestDecideInfoRequestedUsesLifecycleHook(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID := f.pendingProposalInCaseStatus(t, "proposal_pending")
	approverID, _ := f.seedApprover(approverSpec{})
	auditsBefore := len(f.audits(proposalID))
	beforeCase := f.readCase(caseID)

	closedCalls := 0
	hookCalls := 0
	lifecycleSvc := f.lifecycleService(t)
	service := f.decisionServiceWith(t, approval.Options{
		OnProposalClosed: func(context.Context, pgx.Tx, string, string, string) error {
			closedCalls++
			return nil
		},
		InfoRequestedHook: func(ctx context.Context, tx pgx.Tx, id string) error {
			hookCalls++
			return lifecycleSvc.RequestInfo(ctx, tx, id)
		},
	})

	opts := approval.DecisionOptions{InfoRequested: true, Reason: "  need the bank statement  "}
	result, err := service.DecideWithOptions(context.Background(), proposalID,
		approval.DecisionRejected, approverID, "slack", opts)
	if err != nil {
		t.Fatalf("DecideWithOptions: %v", err)
	}
	if result.Status != "rejected" || result.Decision != approval.DecisionRejected || result.Replayed {
		t.Fatalf("result = %+v, want a fresh rejected decision", result)
	}
	if closedCalls != 0 {
		t.Fatalf("OnProposalClosed calls = %d, want 0 for info-requested", closedCalls)
	}
	if hookCalls != 1 {
		t.Fatalf("InfoRequestedHook calls = %d, want 1", hookCalls)
	}

	stored := f.readProposal(proposalID)
	if stored.Status != "rejected" || stored.Version != 2 {
		t.Fatalf("proposal = %s/%d, want rejected/2", stored.Status, stored.Version)
	}
	rows := f.approvalRows(proposalID)
	if len(rows) != 1 || rows[0].Decision != "rejected" || rows[0].Channel != "slack" {
		t.Fatalf("approvals = %+v, want one slack rejection", rows)
	}

	audits := f.audits(proposalID)
	if len(audits) != auditsBefore+2 {
		t.Fatalf("proposal audits = %d, want %d", len(audits), auditsBefore+2)
	}
	transitionAfter := decodeObject(t, audits[auditsBefore].After)
	if transitionAfter["status"] != "rejected" || transitionAfter["reason"] != approval.ReasonInfoRequested {
		t.Fatalf("proposal transition after = %+v, want rejected/%s",
			transitionAfter, approval.ReasonInfoRequested)
	}
	decidedAfter := decodeObject(t, audits[auditsBefore+1].After)
	if decidedAfter["decision"] != "rejected" || decidedAfter["info_requested"] != true ||
		decidedAfter["reason"] != "need the bank statement" {
		t.Fatalf("approval.decided after = %+v, want rejected/info_requested/reason", decidedAfter)
	}

	afterCase := f.readCase(caseID)
	if afterCase.Status != "waiting_info" || afterCase.Attempts != beforeCase.Attempts ||
		afterCase.Version != beforeCase.Version+1 {
		t.Fatalf("case = %s/attempts %d/version %d, want waiting_info/%d/%d",
			afterCase.Status, afterCase.Attempts, afterCase.Version,
			beforeCase.Attempts, beforeCase.Version+1)
	}
	caseAudits := f.caseAudits(caseID)
	if len(caseAudits) != 1 {
		t.Fatalf("case audits = %d, want 1", len(caseAudits))
	}
	if caseAudits[0].Action != "case.transition" || caseAudits[0].ActorType != "system" ||
		caseAudits[0].ActorID != "case-lifecycle" {
		t.Fatalf("case audit = %+v, want system:case-lifecycle case.transition", caseAudits[0])
	}
	caseAfter := decodeObject(t, caseAudits[0].After)
	if caseAfter["status"] != "waiting_info" || caseAfter["reason"] != "info_requested" {
		t.Fatalf("case transition after = %+v, want waiting_info/info_requested", caseAfter)
	}

	updates := f.approvalUpdateRows(proposalID)
	if len(updates) != 1 {
		t.Fatalf("approval.update rows = %d, want 1: %+v", len(updates), updates)
	}
	update := updates[0]
	if update.AggregateType != "proposal" || update.AggregateID != proposalID ||
		update.IdempotencyKey != approval.TopicApprovalUpdate+":"+proposalID {
		t.Fatalf("approval.update row = %+v", update)
	}
	payload := decodeUpdatePayload(t, update)
	if payload["info_requested"] != true || payload["reason"] != "need the bank statement" ||
		payload["decision"] != "rejected" || payload["status"] != "rejected" {
		t.Fatalf("approval.update payload = %+v", payload)
	}

	// Replaying the same options decision calls no hook and enqueues no update.
	proposalAuditsBefore := len(f.audits(proposalID))
	caseAuditsBefore := len(f.caseAudits(caseID))
	replay, err := service.DecideWithOptions(context.Background(), proposalID,
		approval.DecisionRejected, approverID, "slack", opts)
	if err != nil {
		t.Fatalf("replayed DecideWithOptions: %v", err)
	}
	if !replay.Replayed {
		t.Fatalf("replayed result = %+v, want Replayed", replay)
	}
	if hookCalls != 1 {
		t.Fatalf("InfoRequestedHook calls after replay = %d, want 1", hookCalls)
	}
	if got := len(f.approvalUpdateRows(proposalID)); got != 1 {
		t.Fatalf("approval.update rows after replay = %d, want 1", got)
	}
	if got := len(f.audits(proposalID)); got != proposalAuditsBefore {
		t.Fatalf("proposal audits after replay = %d, want %d", got, proposalAuditsBefore)
	}
	if got := len(f.caseAudits(caseID)); got != caseAuditsBefore {
		t.Fatalf("case audits after replay = %d, want %d", got, caseAuditsBefore)
	}
}

// TestDecideInfoRequestedHookFailureRollsBack proves the hook shares the
// decision transaction: a hook error removes the proposal transition, approval
// row, audit, and update row.
func TestDecideInfoRequestedHookFailureRollsBack(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID := f.pendingProposalInCaseStatus(t, "proposal_pending")
	approverID, _ := f.seedApprover(approverSpec{})
	beforeProposal := f.readProposal(proposalID)
	beforeCase := f.readCase(caseID)
	auditsBefore := len(f.audits(proposalID))

	boom := errors.New("injected info-requested hook failure")
	service := f.decisionServiceWith(t, approval.Options{
		InfoRequestedHook: func(context.Context, pgx.Tx, string) error { return boom },
	})
	_, err := service.DecideWithOptions(context.Background(), proposalID,
		approval.DecisionRejected, approverID, "slack",
		approval.DecisionOptions{InfoRequested: true})
	if !errors.Is(err, boom) {
		t.Fatalf("DecideWithOptions error = %v, want the hook error", err)
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
		t.Fatal("approval.decided audit survived the hook rollback")
	}
	if got := len(f.audits(proposalID)); got != auditsBefore {
		t.Fatalf("proposal audits = %d, want %d after rollback", got, auditsBefore)
	}
	if got := f.readCase(caseID); got != beforeCase {
		t.Fatalf("case changed from %+v to %+v, want rollback", beforeCase, got)
	}
	if got := len(f.caseAudits(caseID)); got != 0 {
		t.Fatalf("case audits = %d, want 0 after rollback", got)
	}
	if got := len(f.approvalUpdateRows(proposalID)); got != 0 {
		t.Fatalf("approval.update rows = %d, want 0 after rollback", got)
	}
}

// TestDecideInfoRequestedWithoutHookFailsClosed proves a nil InfoRequestedHook
// rejects the decision before any write and never falls back to
// OnProposalClosed.
func TestDecideInfoRequestedWithoutHookFailsClosed(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID := f.pendingProposalInCaseStatus(t, "proposal_pending")
	approverID, _ := f.seedApprover(approverSpec{})
	beforeProposal := f.readProposal(proposalID)
	beforeCase := f.readCase(caseID)
	auditsBefore := len(f.audits(proposalID))

	closedCalls := 0
	service := f.decisionServiceWith(t, approval.Options{
		OnProposalClosed: func(context.Context, pgx.Tx, string, string, string) error {
			closedCalls++
			return nil
		},
	})
	_, err := service.DecideWithOptions(context.Background(), proposalID,
		approval.DecisionRejected, approverID, "slack",
		approval.DecisionOptions{InfoRequested: true})
	if !errors.Is(err, approval.ErrInfoRequestedUnsupported) {
		t.Fatalf("DecideWithOptions error = %v, want ErrInfoRequestedUnsupported", err)
	}
	if closedCalls != 0 {
		t.Fatalf("OnProposalClosed calls = %d, want 0", closedCalls)
	}

	afterProposal := f.readProposal(proposalID)
	if afterProposal.Status != beforeProposal.Status || afterProposal.Version != beforeProposal.Version {
		t.Fatalf("proposal changed: %+v -> %+v", beforeProposal, afterProposal)
	}
	if rows := f.approvalRows(proposalID); len(rows) != 0 {
		t.Fatalf("approvals rows = %d, want 0", len(rows))
	}
	if _, ok := f.auditByAction(proposalID, approval.ActionApprovalDecided); ok {
		t.Fatal("nil-hook info request wrote an approval.decided audit")
	}
	if got := len(f.audits(proposalID)); got != auditsBefore {
		t.Fatalf("proposal audits = %d, want %d", got, auditsBefore)
	}
	if got := f.readCase(caseID); got != beforeCase {
		t.Fatalf("case changed: %+v -> %+v", beforeCase, got)
	}
	if got := len(f.approvalUpdateRows(proposalID)); got != 0 {
		t.Fatalf("approval.update rows = %d, want 0", got)
	}
}

// TestDecideWithOptionsReasonValidation proves the reason bound and character
// rules run before any write, and that InfoRequested requires a rejection.
func TestDecideWithOptionsReasonValidation(t *testing.T) {
	tests := []struct {
		name     string
		decision approval.Decision
		opts     approval.DecisionOptions
	}{
		{
			name:     "over 2000 runes",
			decision: approval.DecisionRejected,
			opts:     approval.DecisionOptions{Reason: strings.Repeat("a", 2001)},
		},
		{
			name:     "control character",
			decision: approval.DecisionRejected,
			opts:     approval.DecisionOptions{Reason: "needs\x00info"},
		},
		{
			name:     "format character",
			decision: approval.DecisionRejected,
			opts:     approval.DecisionOptions{Reason: "needs\u200einfo"},
		},
		{
			name:     "info requested on approval",
			decision: approval.DecisionApproved,
			opts:     approval.DecisionOptions{InfoRequested: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			proposalID, _, _ := f.pendingProposal(t)
			approverID, _ := f.seedApprover(approverSpec{})
			_, err := f.service.DecideWithOptions(context.Background(), proposalID,
				tc.decision, approverID, "slack", tc.opts)
			if !errors.Is(err, approval.ErrInvalidRequest) {
				t.Fatalf("DecideWithOptions error = %v, want ErrInvalidRequest", err)
			}
			assertNoDecisionWrites(t, f, proposalID, "pending_approval", 1)
			if got := len(f.approvalUpdateRows(proposalID)); got != 0 {
				t.Fatalf("approval.update rows = %d, want 0", got)
			}
		})
	}
}

// TestDecideWithOptionsReasonSemantics covers the two additive reason paths:
// a rejection reason becomes the transition reason, while an approval reason is
// audit-only and the approved edge takes no reason.
func TestDecideWithOptionsReasonSemantics(t *testing.T) {
	t.Run("rejection reason becomes transition reason", func(t *testing.T) {
		f := newFixture(t)
		proposalID, _, _ := f.pendingProposal(t)
		approverID, _ := f.seedApprover(approverSpec{})
		auditsBefore := len(f.audits(proposalID))

		_, err := f.service.DecideWithOptions(context.Background(), proposalID,
			approval.DecisionRejected, approverID, "api",
			approval.DecisionOptions{Reason: "  fee disputed  "})
		if err != nil {
			t.Fatalf("DecideWithOptions: %v", err)
		}
		audits := f.audits(proposalID)
		transitionAfter := decodeObject(t, audits[auditsBefore].After)
		if transitionAfter["reason"] != "fee disputed" {
			t.Fatalf("transition reason = %v, want the trimmed reason", transitionAfter["reason"])
		}
		decidedAfter := decodeObject(t, audits[auditsBefore+1].After)
		if decidedAfter["reason"] != "fee disputed" {
			t.Fatalf("decided audit reason = %v, want fee disputed", decidedAfter["reason"])
		}
		updates := f.approvalUpdateRows(proposalID)
		if len(updates) != 1 || decodeUpdatePayload(t, updates[0])["reason"] != "fee disputed" {
			t.Fatalf("approval.update rows = %+v, want one with reason", updates)
		}
	})

	t.Run("approval reason is audit only", func(t *testing.T) {
		f := newFixture(t)
		proposalID, _, _ := f.pendingProposal(t)
		approverID, _ := f.seedApprover(approverSpec{})
		auditsBefore := len(f.audits(proposalID))

		_, err := f.service.DecideWithOptions(context.Background(), proposalID,
			approval.DecisionApproved, approverID, "api",
			approval.DecisionOptions{Reason: "checked with ops"})
		if err != nil {
			t.Fatalf("DecideWithOptions: %v", err)
		}
		audits := f.audits(proposalID)
		transitionAfter := decodeObject(t, audits[auditsBefore].After)
		if _, present := transitionAfter["reason"]; present {
			t.Fatalf("approved transition has reason %v, want absent", transitionAfter["reason"])
		}
		decidedAfter := decodeObject(t, audits[auditsBefore+1].After)
		if decidedAfter["reason"] != "checked with ops" {
			t.Fatalf("decided audit reason = %v, want checked with ops", decidedAfter["reason"])
		}
		if _, present := decidedAfter["info_requested"]; present {
			t.Fatalf("plain approval has info_requested: %+v", decidedAfter)
		}
	})
}

// TestDecideEnqueuesApprovalUpdate proves every fresh decision enqueues exactly
// one payload-contract-compliant update and a replay enqueues none.
func TestDecideEnqueuesApprovalUpdate(t *testing.T) {
	tests := []struct {
		name     string
		decision approval.Decision
		info     bool
	}{
		{name: "approve", decision: approval.DecisionApproved},
		{name: "reject", decision: approval.DecisionRejected},
		{name: "request info", decision: approval.DecisionRejected, info: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var (
				proposalID, caseID string
				service            *approval.Service
				opts               approval.DecisionOptions
			)
			if tc.info {
				proposalID, caseID = f.pendingProposalInCaseStatus(t, "proposal_pending")
				service = f.decisionServiceWith(t, approval.Options{
					InfoRequestedHook: f.lifecycleService(t).RequestInfo,
				})
				opts.InfoRequested = true
			} else {
				proposalID, caseID, _ = f.pendingProposal(t)
				service = f.service
			}
			approverID, _ := f.seedApprover(approverSpec{})

			var (
				result approval.DecisionResult
				err    error
			)
			if tc.info {
				result, err = service.DecideWithOptions(context.Background(), proposalID,
					tc.decision, approverID, "slack", opts)
			} else {
				result, err = service.Decide(context.Background(), proposalID,
					tc.decision, approverID, "slack")
			}
			if err != nil {
				t.Fatalf("decision: %v", err)
			}
			if result.Replayed {
				t.Fatal("first decision reported Replayed")
			}
			if got := f.readCase(caseID); tc.info && got.Status != "waiting_info" {
				t.Fatalf("case status = %q, want waiting_info", got.Status)
			}

			updates := f.approvalUpdateRows(proposalID)
			if len(updates) != 1 {
				t.Fatalf("approval.update rows = %d, want 1: %+v", len(updates), updates)
			}
			row := updates[0]
			if row.AggregateType != "proposal" || row.AggregateID != proposalID ||
				row.Topic != approval.TopicApprovalUpdate ||
				row.IdempotencyKey != approval.TopicApprovalUpdate+":"+proposalID {
				t.Fatalf("approval.update row = %+v", row)
			}
			payload := decodeUpdatePayload(t, row)
			if len(payload) != 8 {
				t.Fatalf("approval.update payload keys = %v, want exactly 8 (reason omitted)", payload)
			}
			want := map[string]any{
				"proposal_id":    proposalID,
				"case_id":        caseID,
				"decision":       string(tc.decision),
				"status":         string(tc.decision),
				"source":         "slack",
				"approver_id":    approverID,
				"info_requested": tc.info,
			}
			for key, value := range want {
				if payload[key] != value {
					t.Fatalf("payload[%s] = %v, want %v", key, payload[key], value)
				}
			}
			decidedAt, ok := payload["decided_at"].(string)
			if !ok {
				t.Fatalf("payload decided_at = %v", payload["decided_at"])
			}
			parsed, err := time.Parse(time.RFC3339Nano, decidedAt)
			if err != nil {
				t.Fatalf("decided_at %q: %v", decidedAt, err)
			}
			if !parsed.Equal(f.testNowUTC) {
				t.Fatalf("decided_at = %v, want %v", parsed, f.testNowUTC)
			}

			// Same-approver same-decision replay: no hook, audit, or update.
			proposalAuditsBefore := len(f.audits(proposalID))
			var replayed approval.DecisionResult
			if tc.info {
				replayed, err = service.DecideWithOptions(context.Background(), proposalID,
					tc.decision, approverID, "slack", opts)
			} else {
				replayed, err = service.Decide(context.Background(), proposalID,
					tc.decision, approverID, "slack")
			}
			if err != nil {
				t.Fatalf("replayed decision: %v", err)
			}
			if !replayed.Replayed {
				t.Fatalf("replayed result = %+v, want Replayed", replayed)
			}
			if got := len(f.approvalUpdateRows(proposalID)); got != 1 {
				t.Fatalf("approval.update rows after replay = %d, want 1", got)
			}
			if got := len(f.audits(proposalID)); got != proposalAuditsBefore {
				t.Fatalf("proposal audits after replay = %d, want %d", got, proposalAuditsBefore)
			}
		})
	}
}

// TestDecideApprovalUpdateEnqueueConflictRollsBack simulates a failed enqueue
// with a pre-existing idempotency key: the whole decision must roll back.
func TestDecideApprovalUpdateEnqueueConflictRollsBack(t *testing.T) {
	f := newFixture(t)
	proposalID, caseID, _ := f.pendingProposal(t)
	approverID, _ := f.seedApprover(approverSpec{})
	beforeProposal := f.readProposal(proposalID)
	beforeCase := f.readCase(caseID)
	key := approval.TopicApprovalUpdate + ":" + proposalID
	if _, err := f.d.Pool.Exec(context.Background(),
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES ('proposal', $1, $2, '{"seed":true}'::jsonb, $3,
		         CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		proposalID, approval.TopicApprovalUpdate, key); err != nil {
		t.Fatalf("pre-insert approval.update key: %v", err)
	}

	_, err := f.service.Decide(context.Background(), proposalID,
		approval.DecisionApproved, approverID, "slack")
	if err == nil {
		t.Fatal("Decide succeeded despite a duplicate approval.update key")
	}
	if !errors.Is(err, approval.ErrDecisionConflict) {
		t.Fatalf("Decide error = %v, want ErrDecisionConflict", err)
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
		t.Fatal("approval.decided audit survived the enqueue rollback")
	}
	if got := f.readCase(caseID); got != beforeCase {
		t.Fatalf("case changed: %+v -> %+v", beforeCase, got)
	}
	rows := f.approvalUpdateRows(proposalID)
	if len(rows) != 1 || rows[0].IdempotencyKey != key {
		t.Fatalf("approval.update rows = %+v, want only the pre-existing key", rows)
	}
}
