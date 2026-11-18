//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

const (
	missingCaseID = "00000000-0000-4000-8000-000000000001"
)

func TestSupersedeActiveProposalReplacement(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "proposed")
	newID := f.seedProposal(t, caseID, "expired")
	ctx := telemetry.WithTraceID(context.Background(), "trace-supersede")

	result, err := f.supersede(ctx, supersedeCall{
		caseID:        caseID,
		newProposalID: newID,
		actor:         "human:alice",
		reason:        lifecycle.ReasonNewProposal,
	})
	if err != nil {
		t.Fatalf("SupersedeActiveProposal: %v", err)
	}
	if !result.Superseded || result.ProposalID != oldID || !result.CancelInserted {
		t.Fatalf("result = %+v, want superseded old %s with a new cancel row", result, oldID)
	}

	old := f.proposal(t, oldID)
	if old.Status != "superseded" || old.Version != 1 {
		t.Fatalf("old proposal = %s/version %d, want superseded/1", old.Status, old.Version)
	}
	replacement := f.proposal(t, newID)
	if replacement.Status != "expired" || replacement.Version != 0 {
		t.Fatalf("replacement proposal = %s/version %d, want untouched expired/0",
			replacement.Status, replacement.Version)
	}
	caseRow := f.caseRow(t, caseID)
	if caseRow.Status != "proposal_pending" || caseRow.Version != 0 ||
		caseRow.ResolutionType != nil || caseRow.ResolvedAt != nil {
		t.Fatalf("case changed by supersession: %+v, want proposal_pending/0 with no resolution",
			caseRow)
	}

	outbox := f.outbox(t)
	if len(outbox) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1", len(outbox))
	}
	row := outbox[0]
	if row.AggregateType != lifecycle.AggregateApproval || row.AggregateID != oldID {
		t.Fatalf("outbox aggregate = %s/%s, want %s/%s", row.AggregateType,
			row.AggregateID, lifecycle.AggregateApproval, oldID)
	}
	if row.Topic != lifecycle.TopicApprovalCancel {
		t.Fatalf("outbox topic = %s, want %s", row.Topic, lifecycle.TopicApprovalCancel)
	}
	if want := lifecycle.CancelIdempotencyKey(oldID); row.IdempotencyKey != want {
		t.Fatalf("idempotency key = %q, want %q", row.IdempotencyKey, want)
	}
	if row.DeliveredAt != nil {
		t.Fatalf("outbox row delivered_at = %v, want undelivered", row.DeliveredAt)
	}
	payload := jsonObject(t, row.Payload)
	assertKeys(t, payload, "proposal_id", "case_id", "reason")
	if payload["proposal_id"] != oldID || payload["case_id"] != caseID ||
		payload["reason"] != lifecycle.ReasonNewProposal {
		t.Fatalf("cancel payload = %v, want old/case/reason", payload)
	}

	proposalAudits := f.audits(t, oldID)
	if len(proposalAudits) != 1 {
		t.Fatalf("proposal audits = %d, want exactly 1", len(proposalAudits))
	}
	transition := proposalAudits[0]
	if transition.Action != "proposal.transition" || transition.EntityType != "proposal" {
		t.Fatalf("proposal audit = %+v, want proposal.transition/proposal", transition)
	}
	if transition.ActorType != "system" || transition.ActorID != lifecycle.ProposalActorID {
		t.Fatalf("proposal transition actor = %s:%s, want system:%s",
			transition.ActorType, transition.ActorID, lifecycle.ProposalActorID)
	}
	before := jsonObject(t, transition.Before)
	after := jsonObject(t, transition.After)
	if before["status"] != "proposed" || before["version"] != float64(0) {
		t.Fatalf("proposal audit before = %v, want proposed/0", before)
	}
	if after["status"] != "superseded" || after["version"] != float64(1) ||
		after["reason"] != lifecycle.ReasonNewProposal {
		t.Fatalf("proposal audit after = %v, want superseded/1 with reason", after)
	}
	if transition.TraceID == nil || *transition.TraceID != "trace-supersede" {
		t.Fatalf("proposal audit trace = %v, want trace-supersede", transition.TraceID)
	}

	caseAudits := f.audits(t, caseID)
	if len(caseAudits) != 1 {
		t.Fatalf("case lifecycle audits = %d, want exactly 1", len(caseAudits))
	}
	supersedeAudit := caseAudits[0]
	if supersedeAudit.Action != lifecycle.ActionCaseProposalSuperseded ||
		supersedeAudit.EntityType != lifecycle.EntityCase {
		t.Fatalf("lifecycle audit = %+v, want %s/%s", supersedeAudit,
			lifecycle.ActionCaseProposalSuperseded, lifecycle.EntityCase)
	}
	if supersedeAudit.ActorType != "human" || supersedeAudit.ActorID != "alice" {
		t.Fatalf("lifecycle audit actor = %s:%s, want human:alice",
			supersedeAudit.ActorType, supersedeAudit.ActorID)
	}
	before = jsonObject(t, supersedeAudit.Before)
	after = jsonObject(t, supersedeAudit.After)
	assertKeys(t, before, "proposal_id", "status", "version")
	assertKeys(t, after, "proposal_id", "status", "version", "reason", "new_proposal_id")
	if before["proposal_id"] != oldID || before["status"] != "proposed" ||
		before["version"] != float64(0) {
		t.Fatalf("lifecycle audit before = %v, want old proposal proposed/0", before)
	}
	if after["proposal_id"] != oldID || after["status"] != "superseded" ||
		after["version"] != float64(1) || after["new_proposal_id"] != newID ||
		after["reason"] != lifecycle.ReasonNewProposal {
		t.Fatalf("lifecycle audit after = %v, want old proposal superseded/1 with new id", after)
	}
	if supersedeAudit.TraceID == nil || *supersedeAudit.TraceID != "trace-supersede" {
		t.Fatalf("lifecycle audit trace = %v, want trace-supersede", supersedeAudit.TraceID)
	}
}

func TestSupersedePendingApprovalCancelOnly(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "pending_approval")

	result, err := f.supersede(context.Background(), supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	})
	if err != nil {
		t.Fatalf("SupersedeActiveProposal: %v", err)
	}
	if !result.Superseded || result.ProposalID != oldID || !result.CancelInserted {
		t.Fatalf("result = %+v, want superseded old %s with a new cancel row", result, oldID)
	}
	if got := f.proposal(t, oldID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("proposal = %s/version %d, want superseded/1", got.Status, got.Version)
	}
	if got := f.caseRow(t, caseID); got.Status != "proposal_pending" || got.Version != 0 {
		t.Fatalf("case = %s/version %d, want unchanged proposal_pending/0", got.Status, got.Version)
	}

	outbox := f.outbox(t)
	if len(outbox) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(outbox))
	}
	payload := jsonObject(t, outbox[0].Payload)
	assertKeys(t, payload, "proposal_id", "case_id", "reason")
	if payload["proposal_id"] != oldID || payload["case_id"] != caseID ||
		payload["reason"] != lifecycle.ReasonLateMatch {
		t.Fatalf("cancel payload = %v, want old/case/late-match", payload)
	}

	caseAudits := f.audits(t, caseID)
	if len(caseAudits) != 1 || caseAudits[0].Action != lifecycle.ActionCaseProposalSuperseded {
		t.Fatalf("lifecycle audits = %+v, want exactly one supersede row", caseAudits)
	}
	if caseAudits[0].ActorType != "system" || caseAudits[0].ActorID != "matcher" {
		t.Fatalf("lifecycle actor = %s:%s, want system:matcher",
			caseAudits[0].ActorType, caseAudits[0].ActorID)
	}
	after := jsonObject(t, caseAudits[0].After)
	if after["new_proposal_id"] != nil {
		t.Fatalf("cancel-only lifecycle new_proposal_id = %v, want null", after["new_proposal_id"])
	}
	if caseAudits[0].TraceID != nil {
		t.Fatalf("lifecycle trace = %v, want NULL", *caseAudits[0].TraceID)
	}

	proposalAudits := f.audits(t, oldID)
	if len(proposalAudits) != 1 {
		t.Fatalf("proposal audits = %d, want 1", len(proposalAudits))
	}
	proposalAfter := jsonObject(t, proposalAudits[0].After)
	if proposalAfter["reason"] != lifecycle.ReasonLateMatch {
		t.Fatalf("proposal transition reason = %v, want %q",
			proposalAfter["reason"], lifecycle.ReasonLateMatch)
	}
}

func TestSupersedeNoActiveProposalIsNoop(t *testing.T) {
	t.Run("approved proposal is left for the executor", func(t *testing.T) {
		f := newFixture(t)
		caseID := f.seedCase(t, "proposal_pending", "")
		approvedID := f.seedProposal(t, caseID, "approved")

		result, err := f.supersede(context.Background(), supersedeCall{
			caseID: caseID,
			actor:  "system:matcher",
			reason: lifecycle.ReasonLateMatch,
		})
		if err != nil {
			t.Fatalf("SupersedeActiveProposal: %v", err)
		}
		if result.Superseded || result.ProposalID != "" || result.CancelInserted {
			t.Fatalf("result = %+v, want an empty no-op", result)
		}
		if got := f.proposal(t, approvedID); got.Status != "approved" || got.Version != 0 {
			t.Fatalf("approved proposal = %s/version %d, want untouched", got.Status, got.Version)
		}
		if got := len(f.outbox(t)); got != 0 {
			t.Fatalf("outbox rows = %d, want 0", got)
		}
		if got := len(f.audits(t, caseID)); got != 0 {
			t.Fatalf("case audits = %d, want 0", got)
		}
		if got := len(f.audits(t, approvedID)); got != 0 {
			t.Fatalf("proposal audits = %d, want 0", got)
		}
	})

	t.Run("no proposal at all", func(t *testing.T) {
		f := newFixture(t)
		caseID := f.seedCase(t, "proposal_pending", "")

		result, err := f.supersede(context.Background(), supersedeCall{
			caseID: caseID,
			actor:  "system:matcher",
			reason: lifecycle.ReasonLateMatch,
		})
		if err != nil {
			t.Fatalf("SupersedeActiveProposal: %v", err)
		}
		if result.Superseded || result.ProposalID != "" || result.CancelInserted {
			t.Fatalf("result = %+v, want an empty no-op", result)
		}
		if got := len(f.outbox(t)); got != 0 {
			t.Fatalf("outbox rows = %d, want 0", got)
		}
		if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
			t.Fatalf("audit rows = %d, want 0", got)
		}
	})
}

func TestSupersedeActiveProposalEqualToNewProposalIsNoop(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	activeID := f.seedProposal(t, caseID, "proposed")

	result, err := f.supersede(context.Background(), supersedeCall{
		caseID:        caseID,
		newProposalID: activeID,
		actor:         "human:alice",
		reason:        lifecycle.ReasonNewProposal,
	})
	if err != nil {
		t.Fatalf("SupersedeActiveProposal: %v", err)
	}
	if result.Superseded || result.ProposalID != activeID || result.CancelInserted {
		t.Fatalf("result = %+v, want no-op naming the active proposal", result)
	}
	if got := f.proposal(t, activeID); got.Status != "proposed" || got.Version != 0 {
		t.Fatalf("proposal = %s/version %d, want untouched proposed/0", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Fatalf("audit rows = %d, want 0", got)
	}
}

// TestSupersedeOutboxConflictIsSuccess pre-inserts the same approval.cancel
// idempotency key. The outbox insert reports zero rows, which is success: the
// proposal still transitions and the lifecycle audit is still written, but the
// duplicate row is not inserted.
func TestSupersedeOutboxConflictIsSuccess(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "proposed")
	ctx := context.Background()

	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES ('approval', $1, 'approval.cancel',
		         ('{"proposal_id":"' || $1 || '","case_id":"' || $2 ||
		          '","reason":"pre-existing"}')::jsonb,
		         $3, $4, $4)`,
		oldID, caseID, lifecycle.CancelIdempotencyKey(oldID), f.clock.Now()); err != nil {
		t.Fatalf("pre-insert cancel row: %v", err)
	}

	result, err := f.supersede(ctx, supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	})
	if err != nil {
		t.Fatalf("SupersedeActiveProposal: %v", err)
	}
	if !result.Superseded || result.ProposalID != oldID || result.CancelInserted {
		t.Fatalf("result = %+v, want superseded with a duplicate-insert success", result)
	}
	if got := f.proposal(t, oldID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("proposal = %s/version %d, want superseded/1", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 1 {
		t.Fatalf("outbox rows = %d, want the one pre-existing row", got)
	}
	if got := len(f.audits(t, caseID)); got != 1 {
		t.Fatalf("lifecycle audits = %d, want 1", got)
	}
}

func TestSupersedeRepeatedCallIsIdempotent(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "proposed")
	call := supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	}

	first, err := f.supersede(context.Background(), call)
	if err != nil {
		t.Fatalf("first SupersedeActiveProposal: %v", err)
	}
	if !first.Superseded {
		t.Fatalf("first result = %+v, want superseded", first)
	}

	second, err := f.supersede(context.Background(), call)
	if err != nil {
		t.Fatalf("second SupersedeActiveProposal: %v", err)
	}
	if second.Superseded || second.ProposalID != "" || second.CancelInserted {
		t.Fatalf("second result = %+v, want an empty no-op", second)
	}

	if got := f.proposal(t, oldID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("proposal = %s/version %d after replay, want superseded/1", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 1 {
		t.Fatalf("outbox rows = %d after replay, want 1", got)
	}
	if got := len(f.audits(t, oldID)); got != 1 {
		t.Fatalf("proposal audits = %d after replay, want 1", got)
	}
	if got := len(f.audits(t, caseID)); got != 1 {
		t.Fatalf("lifecycle audits = %d after replay, want 1", got)
	}
}

func TestSupersedeRejectsAgentActorWithNoWrites(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "proposed")

	_, err := f.supersede(context.Background(), supersedeCall{
		caseID: caseID,
		actor:  "agent:model-v1",
		reason: lifecycle.ReasonLateMatch,
	})
	if !errors.Is(err, lifecycle.ErrActorNotAllowed) {
		t.Fatalf("agent actor error = %v, want ErrActorNotAllowed", err)
	}
	if got := f.proposal(t, oldID); got.Status != "proposed" || got.Version != 0 {
		t.Fatalf("proposal = %s/version %d, want untouched", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Fatalf("audit rows = %d, want 0", got)
	}
}

func TestSupersedeMissingCaseIsTypedError(t *testing.T) {
	f := newFixture(t)
	_, err := f.supersede(context.Background(), supersedeCall{
		caseID: missingCaseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	})
	if !errors.Is(err, lifecycle.ErrCaseNotFound) {
		t.Fatalf("missing case error = %v, want ErrCaseNotFound", err)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Fatalf("audit rows = %d, want 0", got)
	}
}

func TestSupersedeResolvedCaseIsTypedError(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "resolved", "superseded_by_match")
	oldID := f.seedProposal(t, caseID, "proposed")

	_, err := f.supersede(context.Background(), supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	})
	if !errors.Is(err, lifecycle.ErrTerminalState) {
		t.Fatalf("resolved case error = %v, want ErrTerminalState", err)
	}
	if got := f.proposal(t, oldID); got.Status != "proposed" || got.Version != 0 {
		t.Fatalf("proposal = %s/version %d, want untouched", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Fatalf("audit rows = %d, want 0", got)
	}
}
