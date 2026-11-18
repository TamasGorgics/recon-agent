//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

type unmatchFixture struct {
	*lateMatcherFixture
	matchID        string
	ledgerID       string
	externalID     string
	sourceRecordID string
	runID          string
}

func newUnmatchFixture(t *testing.T) *unmatchFixture {
	t.Helper()
	f := newLateMatcherFixture(t)
	externalID, sourceRecordID := f.addExternal(t, "UNMATCH", -10_000, lateValueDate)
	ledgerID := f.addLedger(t, "UNMATCH", -10_000, lateValueDate)
	result := f.run(t)
	if result.Stats.Matched != 1 {
		t.Fatalf("matched = %d, want 1", result.Stats.Matched)
	}
	return &unmatchFixture{
		lateMatcherFixture: f,
		matchID:            f.matchIDForExternal(t, externalID),
		ledgerID:           ledgerID,
		externalID:         externalID,
		sourceRecordID:     sourceRecordID,
		runID:              result.RunID,
	}
}

func (f *unmatchFixture) insertLinkedBreak(t *testing.T, seq int,
	ledgerIDs []string, refs []match.RecordRef, status string) string {
	t.Helper()
	id := f.insertBreak(t, f.scopeID, seq, f.runID, ledgerIDs, refs)
	if status != "open" {
		if _, err := f.db.Pool.Exec(context.Background(),
			`UPDATE breaks SET status = $2 WHERE id = $1::uuid`, id, status); err != nil {
			t.Fatalf("set break %s status %s: %v", id, status, err)
		}
	}
	return id
}

func (f *unmatchFixture) attachBreak(t *testing.T, caseID, breakID string) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`, caseID, breakID, f.clock.Now()); err != nil {
		t.Fatalf("attach break %s to case %s: %v", breakID, caseID, err)
	}
}

func (f *unmatchFixture) seedTypedProposal(t *testing.T, caseID, typ, status string) string {
	t.Helper()
	ctx := context.Background()
	var runID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', $2)
		 RETURNING id::text`, caseID, f.clock.Now()).Scan(&runID); err != nil {
		t.Fatalf("seed agent run: %v", err)
	}
	n := fixtureSeq.Add(1)
	var id string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, version, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3, '{"marker":"keep"}'::jsonb,
		         'keep rationale', 0.9, $4, 0, $5, $6, $7)
		 RETURNING id::text`,
		caseID, runID, typ, status, fmt.Sprintf("unmatch-state-hash-%d", n),
		fmt.Sprintf("unmatch-proposal-%d", n), f.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("seed %s proposal status %s: %v", typ, status, err)
	}
	return id
}

func (f *unmatchFixture) unmatch(ctx context.Context, reason, actor string) (lifecycle.UnmatchResult, error) {
	var result lifecycle.UnmatchResult
	err := db.WithTx(ctx, f.app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = f.service.Unmatch(ctx, tx, f.matchID, reason, actor)
		return err
	})
	return result, err
}

type storedMatch struct {
	Status        string
	UnmatchedAt   *time.Time
	UnmatchedBy   *string
	UnmatchReason *string
}

func (f *unmatchFixture) matchRow(t *testing.T) storedMatch {
	t.Helper()
	var stored storedMatch
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status, unmatched_at, unmatched_by, unmatch_reason
		   FROM matches WHERE id = $1::uuid`, f.matchID).
		Scan(&stored.Status, &stored.UnmatchedAt, &stored.UnmatchedBy,
			&stored.UnmatchReason); err != nil {
		t.Fatalf("read match %s: %v", f.matchID, err)
	}
	return stored
}

func (f *unmatchFixture) legStatuses(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.db.Pool.Query(context.Background(),
		`SELECT id::text, status FROM match_legs
		  WHERE match_id = $1::uuid ORDER BY id`, f.matchID)
	if err != nil {
		t.Fatalf("query match legs: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatalf("scan match leg: %v", err)
		}
		out[id] = status
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate match legs: %v", err)
	}
	return out
}

func (f *unmatchFixture) breakStatuses(t *testing.T, ids []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, id := range ids {
		out[id] = f.breakStatus(t, id)
	}
	return out
}

func (f *unmatchFixture) proposalRows(t *testing.T, ids []string) map[string]storedProposal {
	t.Helper()
	out := map[string]storedProposal{}
	for _, id := range ids {
		out[id] = f.proposal(t, id)
	}
	return out
}

func (f *unmatchFixture) caseLease(t *testing.T, id string) (*string, *time.Time) {
	t.Helper()
	var (
		leasedBy  *string
		expiresAt *time.Time
	)
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT leased_by, lease_expires_at FROM cases WHERE id = $1::uuid`, id).
		Scan(&leasedBy, &expiresAt); err != nil {
		t.Fatalf("read case lease for %s: %v", id, err)
	}
	return leasedBy, expiresAt
}

type unmatchSnapshot struct {
	match     storedMatch
	legs      map[string]string
	breaks    map[string]string
	cases     map[string]storedCase
	proposals map[string]storedProposal
	audits    int
	outbox    int
}

func (f *unmatchFixture) snapshot(t *testing.T, breakIDs, caseIDs, proposalIDs []string) unmatchSnapshot {
	t.Helper()
	cases := map[string]storedCase{}
	for _, id := range caseIDs {
		cases[id] = f.caseRow(t, id)
	}
	return unmatchSnapshot{
		match:     f.matchRow(t),
		legs:      f.legStatuses(t),
		breaks:    f.breakStatuses(t, breakIDs),
		cases:     cases,
		proposals: f.proposalRows(t, proposalIDs),
		audits:    f.count(t, `SELECT count(*) FROM audit_log`),
		outbox:    f.count(t, `SELECT count(*) FROM outbox`),
	}
}

func (s unmatchSnapshot) assertEqual(t *testing.T, other unmatchSnapshot, context string) {
	t.Helper()
	if !equalStoredMatch(s.match, other.match) {
		t.Fatalf("%s: match changed: before %+v, after %+v", context, s.match, other.match)
	}
	assertStringMapsEqual(t, context+" legs", s.legs, other.legs)
	assertStringMapsEqual(t, context+" breaks", s.breaks, other.breaks)
	if len(s.cases) != len(other.cases) {
		t.Fatalf("%s: case count changed: before %d, after %d", context,
			len(s.cases), len(other.cases))
	}
	for id, before := range s.cases {
		if !equalStoredCase(before, other.cases[id]) {
			t.Fatalf("%s: case %s changed: before %+v, after %+v", context, id,
				before, other.cases[id])
		}
	}
	if len(s.proposals) != len(other.proposals) {
		t.Fatalf("%s: proposal count changed: before %d, after %d", context,
			len(s.proposals), len(other.proposals))
	}
	for id, before := range s.proposals {
		if before != other.proposals[id] {
			t.Fatalf("%s: proposal %s changed: before %+v, after %+v", context, id,
				before, other.proposals[id])
		}
	}
	if s.audits != other.audits {
		t.Fatalf("%s: audit rows changed: before %d, after %d", context,
			s.audits, other.audits)
	}
	if s.outbox != other.outbox {
		t.Fatalf("%s: outbox rows changed: before %d, after %d", context,
			s.outbox, other.outbox)
	}
}

func equalStoredMatch(a, b storedMatch) bool {
	return a.Status == b.Status && equalTimePtr(a.UnmatchedAt, b.UnmatchedAt) &&
		equalStringPtr(a.UnmatchedBy, b.UnmatchedBy) &&
		equalStringPtr(a.UnmatchReason, b.UnmatchReason)
}

func assertStringMapsEqual(t *testing.T, label string, a, b map[string]string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s changed size: before %v, after %v", label, a, b)
	}
	for id, value := range a {
		if b[id] != value {
			t.Fatalf("%s changed for %s: before %q, after %q", label, id, value, b[id])
		}
	}
}

type unmatchSuccess struct {
	f             *unmatchFixture
	resolvedCase  string
	pendingCase   string
	escalatedCase string
	proposalID    string
	breaks        []string
	changedBreaks []string
}

func setupUnmatchSuccess(t *testing.T) *unmatchSuccess {
	t.Helper()
	f := newUnmatchFixture(t)
	resolvedCase := f.seedCase(t, "resolved", "manual")
	pendingCase := f.seedCase(t, "in_agent", "")
	escalatedCase := f.seedCase(t, "escalated", "")

	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE cases SET leased_by = 'system:agent-loop', lease_expires_at = $2
		  WHERE id = $1::uuid`, pendingCase, f.clock.Now().Add(time.Minute)); err != nil {
		t.Fatalf("seed in_agent lease: %v", err)
	}
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE cases SET escalation_reason = 'manual' WHERE id = $1::uuid`,
		escalatedCase); err != nil {
		t.Fatalf("seed escalation reason: %v", err)
	}

	ref := []match.RecordRef{{Source: f.source, SourceRecordID: f.sourceRecordID}}
	break1 := f.insertLinkedBreak(t, 1, []string{f.ledgerID}, nil, "in_case")
	break2 := f.insertLinkedBreak(t, 2, nil, ref, "resolved")
	break3 := f.insertLinkedBreak(t, 3, []string{f.ledgerID}, nil, "superseded")
	break4 := f.insertLinkedBreak(t, 4, []string{f.ledgerID}, nil, "void")
	break5 := f.insertLinkedBreak(t, 5, nil, ref, "open")
	f.attachBreak(t, resolvedCase, break1)
	f.attachBreak(t, pendingCase, break2)
	f.attachBreak(t, escalatedCase, break3)
	f.attachBreak(t, resolvedCase, break4)
	proposalID := f.seedProposal(t, pendingCase, "proposed")

	return &unmatchSuccess{
		f: f, resolvedCase: resolvedCase, pendingCase: pendingCase,
		escalatedCase: escalatedCase, proposalID: proposalID,
		breaks:        []string{break1, break2, break3, break4, break5},
		changedBreaks: []string{break1, break2, break3},
	}
}

func TestUnmatchSuccessAcrossBreaksCasesAndProposals(t *testing.T) {
	s := setupUnmatchSuccess(t)
	f := s.f
	beforeLegs := f.legStatuses(t)
	if len(beforeLegs) != 1 {
		t.Fatalf("match legs = %v, want exactly 1", beforeLegs)
	}

	result, err := f.unmatch(context.Background(), "  operator requested  ", "human:alice")
	if err != nil {
		t.Fatalf("Unmatch: %v", err)
	}
	if result.MatchID != f.matchID {
		t.Fatalf("result match id = %s, want %s", result.MatchID, f.matchID)
	}
	if result.BreaksReopened != 3 || result.CasesReopened != 3 ||
		result.ProposalsCancelled != 1 {
		t.Fatalf("result counts = breaks %d/cases %d/proposals %d, want 3/3/1",
			result.BreaksReopened, result.CasesReopened, result.ProposalsCancelled)
	}
	if !result.UnmatchedAt.Equal(f.clock.Now()) {
		t.Fatalf("unmatched_at = %v, want injected clock %v",
			result.UnmatchedAt, f.clock.Now())
	}

	matchRow := f.matchRow(t)
	if matchRow.Status != "unmatched" || matchRow.UnmatchedBy == nil ||
		*matchRow.UnmatchedBy != "human:alice" || matchRow.UnmatchReason == nil ||
		*matchRow.UnmatchReason != "operator requested" {
		t.Fatalf("match row = %+v, want unmatched by human:alice with trimmed reason", matchRow)
	}
	if matchRow.UnmatchedAt == nil || !matchRow.UnmatchedAt.Equal(f.clock.Now()) {
		t.Fatalf("match unmatched_at = %v, want injected clock", matchRow.UnmatchedAt)
	}

	legs := f.legStatuses(t)
	for id, status := range legs {
		if status != "unmatched" {
			t.Fatalf("match leg %s status = %s, want trigger-synced unmatched", id, status)
		}
	}

	breaks := f.breakStatuses(t, s.breaks)
	for _, id := range s.changedBreaks {
		if breaks[id] != "open" {
			t.Fatalf("break %s status = %s, want open", id, breaks[id])
		}
		if audits := f.audits(t, id); len(audits) != 1 ||
			audits[0].Action != lifecycle.ActionBreakUnmatched {
			t.Fatalf("break %s audits = %+v, want exactly one break.unmatched", id, audits)
		}
	}
	if breaks[s.breaks[3]] != "void" || breaks[s.breaks[4]] != "open" {
		t.Fatalf("void/open breaks changed: %v", breaks)
	}
	if audits := f.audits(t, s.breaks[3]); len(audits) != 0 {
		t.Fatalf("void break audits = %+v, want none", audits)
	}
	if audits := f.audits(t, s.breaks[4]); len(audits) != 0 {
		t.Fatalf("already-open break audits = %+v, want none", audits)
	}

	for _, caseID := range []string{s.resolvedCase, s.pendingCase, s.escalatedCase} {
		row := f.caseRow(t, caseID)
		if row.Status != "open" || row.Version != 1 ||
			row.ResolutionType != nil || row.ResolvedAt != nil ||
			row.EscalationReason != nil {
			t.Fatalf("case %s = %+v, want open/version 1 with cleared fields", caseID, row)
		}
		leasedBy, expiresAt := f.caseLease(t, caseID)
		if leasedBy != nil || expiresAt != nil {
			t.Fatalf("case %s lease = %v/%v, want cleared", caseID, leasedBy, expiresAt)
		}
	}

	proposal := f.proposal(t, s.proposalID)
	if proposal.Status != "superseded" || proposal.Version != 1 {
		t.Fatalf("proposal = %s/version %d, want superseded/1",
			proposal.Status, proposal.Version)
	}
	outbox := f.outbox(t)
	if len(outbox) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1", len(outbox))
	}
	cancel := outbox[0]
	if cancel.AggregateType != lifecycle.AggregateApproval ||
		cancel.AggregateID != s.proposalID ||
		cancel.Topic != lifecycle.TopicApprovalCancel ||
		cancel.IdempotencyKey != lifecycle.CancelIdempotencyKey(s.proposalID) {
		t.Fatalf("cancel row = %+v, want approval.cancel for %s", cancel, s.proposalID)
	}
	payload := jsonObject(t, cancel.Payload)
	assertKeys(t, payload, "proposal_id", "case_id", "reason")
	if payload["proposal_id"] != s.proposalID || payload["case_id"] != s.pendingCase ||
		payload["reason"] != lifecycle.ReasonCaseReopened {
		t.Fatalf("cancel payload = %v, want case_reopened for the pending case", payload)
	}

	resolvedCaseAudits := f.audits(t, s.resolvedCase)
	if len(resolvedCaseAudits) != 1 ||
		resolvedCaseAudits[0].Action != "case.transition" {
		t.Fatalf("resolved case audits = %+v, want one case.transition", resolvedCaseAudits)
	}
	caseAudits := f.audits(t, s.pendingCase)
	if len(caseAudits) != 2 {
		t.Fatalf("pending case audits = %+v, want supersede + transition", caseAudits)
	}
	if caseAudits[0].Action != lifecycle.ActionCaseProposalSuperseded ||
		caseAudits[1].Action != "case.transition" {
		t.Fatalf("pending case audit order = %s then %s, want supersede then transition",
			caseAudits[0].Action, caseAudits[1].Action)
	}
	for _, row := range caseAudits {
		if row.ActorType != "human" || row.ActorID != "alice" {
			t.Fatalf("case audit actor = %s:%s, want human:alice",
				row.ActorType, row.ActorID)
		}
	}
	if audits := f.audits(t, s.proposalID); len(audits) != 1 ||
		audits[0].Action != "proposal.transition" {
		t.Fatalf("proposal audits = %+v, want one proposal.transition", audits)
	}

	matchAudits := f.audits(t, f.matchID)
	if len(matchAudits) != 1 {
		t.Fatalf("match audits = %+v, want exactly one", matchAudits)
	}
	matchAudit := matchAudits[0]
	if matchAudit.Action != lifecycle.ActionMatchUnmatch ||
		matchAudit.EntityType != lifecycle.EntityMatch ||
		matchAudit.ActorType != "human" || matchAudit.ActorID != "alice" {
		t.Fatalf("match audit = %+v, want match.unmatch by human:alice", matchAudit)
	}
	before := jsonObject(t, matchAudit.Before)
	if before["status"] != "active" {
		t.Fatalf("match audit before = %v, want active", before)
	}
	after := jsonObject(t, matchAudit.After)
	assertKeys(t, after, "status", "match_id", "reason", "actor",
		"breaks_reopened", "cases_reopened", "proposals_cancelled", "unmatched_at")
	if after["status"] != "unmatched" || after["match_id"] != f.matchID ||
		after["reason"] != "operator requested" || after["actor"] != "human:alice" ||
		after["breaks_reopened"] != float64(3) ||
		after["cases_reopened"] != float64(3) ||
		after["proposals_cancelled"] != float64(1) {
		t.Fatalf("match audit after = %v, want the success summary", after)
	}

	// Repeating the call is a typed conflict with no second audit or outbox row.
	beforeReplay := f.snapshot(t, s.breaks,
		[]string{s.resolvedCase, s.pendingCase, s.escalatedCase},
		[]string{s.proposalID})
	_, err = f.unmatch(context.Background(), "operator requested", "human:alice")
	if !errors.Is(err, lifecycle.ErrMatchNotActive) {
		t.Fatalf("repeated Unmatch error = %v, want ErrMatchNotActive", err)
	}
	afterReplay := f.snapshot(t, s.breaks,
		[]string{s.resolvedCase, s.pendingCase, s.escalatedCase},
		[]string{s.proposalID})
	beforeReplay.assertEqual(t, afterReplay, "repeated unmatch")
	if got := f.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'match.unmatch'`); got != 1 {
		t.Fatalf("match.unmatch audits = %d, want 1", got)
	}
}

func TestUnmatchResolvedCaseWithActiveProposal(t *testing.T) {
	f := newUnmatchFixture(t)
	caseID := f.seedCase(t, "resolved", "manual")
	proposalID := f.seedProposal(t, caseID, "proposed")
	breakID := f.insertLinkedBreak(t, 1, []string{f.ledgerID}, nil, "in_case")
	f.attachBreak(t, caseID, breakID)

	result, err := f.unmatch(context.Background(), "reopen for review", "human:alice")
	if err != nil {
		t.Fatalf("Unmatch: %v", err)
	}
	if result.ProposalsCancelled != 1 || result.CasesReopened != 1 ||
		result.BreaksReopened != 1 {
		t.Fatalf("result = %+v, want 1/1/1", result)
	}
	if got := f.proposal(t, proposalID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("resolved case proposal = %s/version %d, want superseded/1",
			got.Status, got.Version)
	}
	if got := f.caseRow(t, caseID); got.Status != "open" || got.Version != 1 ||
		got.ResolutionType != nil || got.ResolvedAt != nil {
		t.Fatalf("case = %+v, want open/version 1 with cleared resolution", got)
	}
	if outbox := f.outbox(t); len(outbox) != 1 || outbox[0].AggregateID != proposalID {
		t.Fatalf("outbox = %+v, want one cancel row for %s", outbox, proposalID)
	}
	caseAudits := f.audits(t, caseID)
	if len(caseAudits) != 2 ||
		caseAudits[0].Action != lifecycle.ActionCaseProposalSuperseded ||
		caseAudits[1].Action != "case.transition" {
		t.Fatalf("case audits = %+v, want supersede then transition", caseAudits)
	}
}

func TestUnmatchRefusesAppliedAdjustmentBeforeAnyWrite(t *testing.T) {
	for _, typ := range []string{"adjustment", "write_off"} {
		t.Run(typ, func(t *testing.T) {
			s := setupUnmatchSuccess(t)
			f := s.f
			appliedProposal := f.seedTypedProposal(t, s.resolvedCase, typ, "applied")
			if _, err := f.db.Pool.Exec(context.Background(),
				`INSERT INTO applied_proposals
				     (proposal_id, ledger_txn_id, applied_at, executor_run_id)
				 VALUES ($1::uuid, $2::uuid, $3, 'executor-unmatch-test')`,
				appliedProposal, f.ledgerID, f.clock.Now()); err != nil {
				t.Fatalf("insert applied_proposals: %v", err)
			}

			caseIDs := []string{s.resolvedCase, s.pendingCase, s.escalatedCase}
			proposalIDs := []string{s.proposalID, appliedProposal}
			before := f.snapshot(t, s.breaks, caseIDs, proposalIDs)

			_, err := f.unmatch(context.Background(), "operator requested", "human:alice")
			if !errors.Is(err, lifecycle.ErrAdjustmentApplied) {
				t.Fatalf("Unmatch error = %v, want ErrAdjustmentApplied", err)
			}
			message := err.Error()
			for _, want := range []string{
				f.ledgerID,
				"recon reverse --txn " + f.ledgerID,
				"future",
				"not implemented in this release",
				typ,
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("refusal message %q does not contain %q", message, want)
				}
			}

			after := f.snapshot(t, s.breaks, caseIDs, proposalIDs)
			before.assertEqual(t, after, "applied adjustment refusal")
		})
	}
}

func TestUnmatchValidationErrorsWriteNothing(t *testing.T) {
	f := newUnmatchFixture(t)
	breakID := f.insertLinkedBreak(t, 1, []string{f.ledgerID}, nil, "in_case")
	caseID := f.seedCase(t, "proposal_pending", "")
	f.attachBreak(t, caseID, breakID)
	proposalID := f.seedProposal(t, caseID, "proposed")

	before := f.snapshot(t, []string{breakID}, []string{caseID}, []string{proposalID})

	tests := []struct {
		name    string
		matchID string
		reason  string
		actor   string
		wantErr error
	}{
		{name: "missing match", matchID: "00000000-0000-4000-8000-000000000099",
			reason: "operator requested", actor: "human:alice", wantErr: lifecycle.ErrMatchNotFound},
		{name: "malformed match", matchID: "not-a-uuid",
			reason: "operator requested", actor: "human:alice", wantErr: lifecycle.ErrInvalidRequest},
		{name: "system actor", matchID: f.matchID,
			reason: "operator requested", actor: "system:reconciler", wantErr: lifecycle.ErrActorNotAllowed},
		{name: "agent actor", matchID: f.matchID,
			reason: "operator requested", actor: "agent:model-v1", wantErr: lifecycle.ErrActorNotAllowed},
		{name: "empty actor", matchID: f.matchID,
			reason: "operator requested", actor: "", wantErr: lifecycle.ErrInvalidActor},
		{name: "blank reason", matchID: f.matchID,
			reason: "   ", actor: "human:alice", wantErr: lifecycle.ErrInvalidReason},
		{name: "overlong reason", matchID: f.matchID,
			reason: strings.Repeat("a", 501), actor: "human:alice", wantErr: lifecycle.ErrInvalidReason},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.matchID == f.matchID {
				_, err = f.unmatch(context.Background(), tc.reason, tc.actor)
			} else {
				_, err = f.unmatchMatchID(context.Background(), tc.matchID, tc.reason, tc.actor)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Unmatch error = %v, want %v", err, tc.wantErr)
			}
			after := f.snapshot(t, []string{breakID}, []string{caseID}, []string{proposalID})
			before.assertEqual(t, after, tc.name)
		})
	}

	if got := f.matchRow(t); got.Status != "active" || got.UnmatchedAt != nil {
		t.Fatalf("match changed by validation errors: %+v", got)
	}
}

// unmatchMatchID is the validation-matrix escape hatch for a different match
// id while keeping the fixture helper's transaction wiring.
func (f *unmatchFixture) unmatchMatchID(ctx context.Context, matchID, reason, actor string) (lifecycle.UnmatchResult, error) {
	var result lifecycle.UnmatchResult
	err := db.WithTx(ctx, f.app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = f.service.Unmatch(ctx, tx, matchID, reason, actor)
		return err
	})
	return result, err
}

func TestUnmatchRollbackOnInjectedMidOperationFailure(t *testing.T) {
	s := setupUnmatchSuccess(t)
	f := s.f
	failCase := s.resolvedCase
	for _, id := range []string{s.pendingCase, s.escalatedCase} {
		if id > failCase {
			failCase = id
		}
	}
	triggerName := f.installCaseFailureTrigger(t, failCase)

	before := f.snapshot(t, s.breaks,
		[]string{s.resolvedCase, s.pendingCase, s.escalatedCase},
		[]string{s.proposalID})

	_, err := f.unmatch(context.Background(), "operator requested", "human:alice")
	if err == nil || !strings.Contains(err.Error(), "injected unmatch failure") {
		t.Fatalf("Unmatch error = %v, want the injected failure", err)
	}
	after := f.snapshot(t, s.breaks,
		[]string{s.resolvedCase, s.pendingCase, s.escalatedCase},
		[]string{s.proposalID})
	before.assertEqual(t, after, "rolled-back unmatch")

	f.dropCaseFailureTrigger(t, triggerName)
	result, err := f.unmatch(context.Background(), "operator requested", "human:alice")
	if err != nil {
		t.Fatalf("retry after removing the fault: %v", err)
	}
	if result.BreaksReopened != 3 || result.CasesReopened != 3 ||
		result.ProposalsCancelled != 1 {
		t.Fatalf("retry result = %+v, want 3/3/1", result)
	}
}

func (f *unmatchFixture) installCaseFailureTrigger(t *testing.T, caseID string) string {
	t.Helper()
	name := fmt.Sprintf("unmatch_fail_%d", fixtureSeq.Add(1))
	function := fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    IF OLD.id = '%s'::uuid AND NEW.status = 'open' AND OLD.status <> 'open' THEN
        RAISE EXCEPTION 'injected unmatch failure';
    END IF;
    RETURN NEW;
END;
$fn$`, name, caseID)
	if _, err := f.db.Pool.Exec(context.Background(), function); err != nil {
		t.Fatalf("install fault function: %v", err)
	}
	trigger := fmt.Sprintf(
		`CREATE TRIGGER %s BEFORE UPDATE ON cases
		     FOR EACH ROW EXECUTE FUNCTION %s()`, name, name)
	if _, err := f.db.Pool.Exec(context.Background(), trigger); err != nil {
		t.Fatalf("install fault trigger: %v", err)
	}
	return name
}

func (f *unmatchFixture) dropCaseFailureTrigger(t *testing.T, name string) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON cases`, name)); err != nil {
		t.Fatalf("drop fault trigger: %v", err)
	}
	if _, err := f.db.Pool.Exec(context.Background(),
		fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name)); err != nil {
		t.Fatalf("drop fault function: %v", err)
	}
}
