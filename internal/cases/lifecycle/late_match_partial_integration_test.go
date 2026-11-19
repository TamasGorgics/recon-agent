//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	caselifecycle "github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// createTwoBreakCase persists the first run's two external-only payout
// candidates through the real cases service. Both records share the fixture
// settlement batch, so CreateBreaks groups them into one case with two attached
// breaks.
func (f *lateMatcherFixture) createTwoBreakCase(t *testing.T,
	result match.RunResult) (caseID string, breaks []cases.Break) {
	t.Helper()
	candidates := make([]match.BreakCandidate, 0, 2)
	for _, candidate := range result.Breaks {
		if candidate.Side == match.SideExternalOnly {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) != 2 {
		t.Fatalf("external-only break candidates = %d, want exactly 2 (all breaks: %+v)",
			len(candidates), result.Breaks)
	}
	created, err := f.caseService.CreateBreaks(context.Background(), cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: result.RunID,
		Breaks:     candidates,
	})
	if err != nil {
		t.Fatalf("CreateBreaks: %v", err)
	}
	if len(created.Breaks) != 2 || len(created.Cases) != 1 {
		t.Fatalf("CreateBreaks created %d breaks / %d cases, want 2/1",
			len(created.Breaks), len(created.Cases))
	}
	return created.Cases[0].ID, created.Breaks
}

// breakForRecord returns the attached break carrying sourceRecordID.
func breakForRecord(t *testing.T, breaks []cases.Break, sourceRecordID string) cases.Break {
	t.Helper()
	for _, item := range breaks {
		for _, ref := range item.ExternalRefs {
			if ref.SourceRecordID == sourceRecordID {
				return item
			}
		}
	}
	t.Fatalf("no created break carries source record %q (breaks: %+v)",
		sourceRecordID, breaks)
	return cases.Break{}
}

// partialLateCase is the two-break starting point: the first break is consumed
// by the late match, the second stays attached and active.
type partialLateCase struct {
	f                *lateMatcherFixture
	matchExternalID  string
	caseID           string
	consumedBreakID  string
	remainingBreakID string
	proposalID       string
}

// setupPartialLateCase creates one case with two external-only breaks and one
// proposal in the requested status. The caller adds a ledger entry for only the
// first external record and runs the matcher; reconciliation must consume
// exactly that break.
func setupPartialLateCase(t *testing.T, proposalStatus string) *partialLateCase {
	t.Helper()
	f := newLateMatcherFixture(t)
	externalID, consumedRecordID := f.addExternal(t, "PARTIAL-A", -10_000, lateValueDate)
	_, remainingRecordID := f.addExternal(t, "PARTIAL-B", -20_000, lateValueDate)
	first := f.run(t)
	caseID, breaks := f.createTwoBreakCase(t, first)
	consumed := breakForRecord(t, breaks, consumedRecordID)
	remaining := breakForRecord(t, breaks, remainingRecordID)
	f.transitionCase(t, caseID, casestate.StatusInAgent, "system:cases", "", "")
	f.transitionCase(t, caseID, casestate.StatusProposalPending, "system:cases", "", "")
	proposalID := f.seedProposal(t, caseID, proposalStatus)
	return &partialLateCase{
		f:                f,
		matchExternalID:  externalID,
		caseID:           caseID,
		consumedBreakID:  consumed.ID,
		remainingBreakID: remaining.ID,
		proposalID:       proposalID,
	}
}

// TestLateMatchPartialConsumptionReopensCase covers spec 5.2's partial branch
// with the real matcher: one of two attached breaks is consumed, the active
// proposed/pending_approval proposal is superseded and cancelled, and the case
// reopens to open with reason partial_late_match. A replay writes no duplicate.
func TestLateMatchPartialConsumptionReopensCase(t *testing.T) {
	for _, proposalStatus := range []string{"proposed", "pending_approval"} {
		proposalStatus := proposalStatus
		t.Run(proposalStatus, func(t *testing.T) {
			s := setupPartialLateCase(t, proposalStatus)
			f := s.f
			f.addLedger(t, "PARTIAL-A", -10_000, lateValueDate)
			beforeCase := f.caseRow(t, s.caseID)

			second := f.run(t)
			if second.Stats.Matched != 1 {
				t.Fatalf("second run matched = %d, want 1 (stats: %+v)", second.Stats.Matched, second.Stats)
			}
			matchID := f.matchIDForExternal(t, s.matchExternalID)

			if got := f.breakStatus(t, s.consumedBreakID); got != "superseded" {
				t.Fatalf("consumed break status = %s, want superseded", got)
			}
			if got := f.breakStatus(t, s.remainingBreakID); got != "in_case" {
				t.Fatalf("remaining break status = %s, want untouched in_case", got)
			}
			if got := f.count(t,
				`SELECT count(*) FROM case_breaks WHERE case_id = $1::uuid`, s.caseID); got != 2 {
				t.Fatalf("attached breaks = %d, want the remaining break still attached (2)", got)
			}

			if got := f.proposal(t, s.proposalID); got.Status != "superseded" || got.Version != 1 {
				t.Fatalf("proposal = %s/version %d, want superseded/1", got.Status, got.Version)
			}
			outbox := f.outbox(t)
			if len(outbox) != 1 {
				t.Fatalf("outbox rows = %d, want exactly 1 approval.cancel", len(outbox))
			}
			if outbox[0].Topic != caselifecycle.TopicApprovalCancel ||
				outbox[0].AggregateID != s.proposalID ||
				outbox[0].IdempotencyKey != caselifecycle.CancelIdempotencyKey(s.proposalID) {
				t.Fatalf("cancel row = %+v, want one approval.cancel for %s", outbox[0], s.proposalID)
			}

			afterCase := f.caseRow(t, s.caseID)
			if afterCase.Status != "open" {
				t.Fatalf("case status = %s, want open (partial late match must never resolve)", afterCase.Status)
			}
			if afterCase.Version != beforeCase.Version+1 {
				t.Fatalf("case version = %d, want exactly one bump from %d",
					afterCase.Version, beforeCase.Version)
			}
			if afterCase.ResolutionType != nil || afterCase.ResolvedAt != nil {
				t.Fatalf("reopened case resolution = %v/%v, want cleared", afterCase.ResolutionType,
					afterCase.ResolvedAt)
			}

			var transitions, reopens, supersedeAudits int
			for _, row := range f.audits(t, s.caseID) {
				switch row.Action {
				case casestate.ActionCaseTransition:
					transitions++
					after := jsonObject(t, row.After)
					if after["status"] != "open" {
						continue
					}
					reopens++
					if row.ActorType != "system" || row.ActorID != "matcher" {
						t.Fatalf("partial transition actor = %s:%s, want system:matcher",
							row.ActorType, row.ActorID)
					}
					if after["reason"] != casestate.ReasonPartialLateMatch {
						t.Fatalf("partial transition after = %v, want open with reason %s",
							after, casestate.ReasonPartialLateMatch)
					}
					if after["resolution_type"] != nil {
						t.Fatalf("partial transition resolution_type = %v, want null", after["resolution_type"])
					}
				case caselifecycle.ActionCaseProposalSuperseded:
					supersedeAudits++
				}
			}
			if transitions != 3 { // in_agent, proposal_pending, partial reopen
				t.Fatalf("case.transition audits = %d, want 3 (two setup plus the partial reopen)",
					transitions)
			}
			if reopens != 1 {
				t.Fatalf("partial reopen audits = %d, want exactly 1", reopens)
			}
			if supersedeAudits != 1 {
				t.Fatalf("case.proposal_superseded audits = %d, want exactly 1", supersedeAudits)
			}

			var breakSuperseded int
			for _, row := range f.audits(t, s.consumedBreakID) {
				if row.Action != caselifecycle.ActionBreakSuperseded {
					continue
				}
				breakSuperseded++
				after := jsonObject(t, row.After)
				if after["status"] != "superseded" || after["match_id"] != matchID ||
					after["case_id"] != s.caseID {
					t.Fatalf("consumed break audit after = %v, want superseded with match/case", after)
				}
			}
			if breakSuperseded != 1 {
				t.Fatalf("break.superseded audits = %d, want exactly 1", breakSuperseded)
			}
			if got := f.audits(t, s.remainingBreakID); len(got) != 2 {
				// break.create and break.in_case from case creation; no reconciler write.
				t.Fatalf("remaining break audits = %d, want the two creation rows and no supersede",
					len(got))
			}

			// Replay the same active match: the case is already open, there is no
			// active proposal, and the remaining break is still active, so the call is
			// a state no-op that writes nothing.
			beforeAudits := f.count(t, `SELECT count(*) FROM audit_log`)
			beforeOutbox := len(f.outbox(t))
			result, err := f.reconcile(context.Background(), matchID, "system:matcher")
			if err != nil {
				t.Fatalf("replay ReconcileLateMatch: %v", err)
			}
			if len(result.Cases) != 1 || result.Cases[0].Status != "open" ||
				result.Cases[0].Reopened || result.Cases[0].Resolved {
				t.Fatalf("replay result = %+v, want one already-open, not reopened/resolved case",
					result.Cases)
			}
			if got := f.count(t, `SELECT count(*) FROM audit_log`); got != beforeAudits {
				t.Fatalf("audit rows after replay = %d, want %d", got, beforeAudits)
			}
			if got := len(f.outbox(t)); got != beforeOutbox {
				t.Fatalf("outbox rows after replay = %d, want %d", got, beforeOutbox)
			}
			replayCase := f.caseRow(t, s.caseID)
			if replayCase.Status != afterCase.Status || replayCase.Version != afterCase.Version {
				t.Fatalf("case after replay = %s/version %d, want %s/version %d",
					replayCase.Status, replayCase.Version, afterCase.Status, afterCase.Version)
			}
			if got := f.proposal(t, s.proposalID); got.Status != "superseded" || got.Version != 1 {
				t.Fatalf("proposal after replay = %s/version %d, want superseded/1", got.Status, got.Version)
			}
			if got := f.breakStatus(t, s.consumedBreakID); got != "superseded" {
				t.Fatalf("consumed break after replay = %s, want superseded", got)
			}
			if got := f.breakStatus(t, s.remainingBreakID); got != "in_case" {
				t.Fatalf("remaining break after replay = %s, want in_case", got)
			}
		})
	}
}

// TestLateMatchPartialConsumptionLeavesApprovedProposalIntact covers the
// approved-proposal partial branch: the consumed break is superseded and the
// case reopens for the remaining work, but the approved proposal is left
// untouched and no approval.cancel row is written.
func TestLateMatchPartialConsumptionLeavesApprovedProposalIntact(t *testing.T) {
	s := setupPartialLateCase(t, "approved")
	f := s.f
	f.addLedger(t, "PARTIAL-A", -10_000, lateValueDate)
	beforeCase := f.caseRow(t, s.caseID)

	second := f.run(t)
	if second.Stats.Matched != 1 {
		t.Fatalf("second run matched = %d, want 1 (stats: %+v)", second.Stats.Matched, second.Stats)
	}
	if got := f.breakStatus(t, s.consumedBreakID); got != "superseded" {
		t.Fatalf("consumed break status = %s, want superseded", got)
	}
	if got := f.breakStatus(t, s.remainingBreakID); got != "in_case" {
		t.Fatalf("remaining break status = %s, want in_case", got)
	}
	afterCase := f.caseRow(t, s.caseID)
	if afterCase.Status != "open" {
		t.Fatalf("case status = %s, want open", afterCase.Status)
	}
	if afterCase.Version != beforeCase.Version+1 {
		t.Fatalf("case version = %d, want exactly one bump from %d",
			afterCase.Version, beforeCase.Version)
	}
	if got := f.proposal(t, s.proposalID); got.Status != "approved" || got.Version != 0 {
		t.Fatalf("approved proposal = %s/version %d, want approved/0 (untouched)", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d, want 0 (approved proposals are never cancelled)", got)
	}
	if got := f.count(t,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_id = $2`,
		caselifecycle.ActionCaseProposalSuperseded, s.caseID); got != 0 {
		t.Fatalf("case.proposal_superseded audits = %d, want 0", got)
	}
	var reopens int
	for _, row := range f.audits(t, s.caseID) {
		if row.Action != casestate.ActionCaseTransition {
			continue
		}
		after := jsonObject(t, row.After)
		if after["status"] != "open" {
			continue
		}
		reopens++
		if after["reason"] != casestate.ReasonPartialLateMatch {
			t.Fatalf("partial reopen reason = %v, want %s", after["reason"],
				casestate.ReasonPartialLateMatch)
		}
	}
	if reopens != 1 {
		t.Fatalf("partial reopen audits = %d, want exactly 1", reopens)
	}
}

// TestLateMatchPartialEscalatedCaseUsesExactReason pins the escalated -> open
// edge: the system reason must be exactly partial_late_match, and the reopened
// case clears its escalation state while the active proposal is cancelled.
func TestLateMatchPartialEscalatedCaseUsesExactReason(t *testing.T) {
	s := setupPartialLateCase(t, "proposed")
	f := s.f
	f.transitionCase(t, s.caseID, casestate.StatusEscalated, "system:cases",
		casestate.EscalationManual, "")
	beforeCase := f.caseRow(t, s.caseID)
	if beforeCase.EscalationReason == nil ||
		*beforeCase.EscalationReason != casestate.EscalationManual {
		t.Fatalf("setup escalation_reason = %v, want %s", beforeCase.EscalationReason,
			casestate.EscalationManual)
	}

	f.addLedger(t, "PARTIAL-A", -10_000, lateValueDate)
	second := f.run(t)
	if second.Stats.Matched != 1 {
		t.Fatalf("second run matched = %d, want 1 (stats: %+v)", second.Stats.Matched, second.Stats)
	}

	afterCase := f.caseRow(t, s.caseID)
	if afterCase.Status != "open" {
		t.Fatalf("case status = %s, want open", afterCase.Status)
	}
	if afterCase.Version != beforeCase.Version+1 {
		t.Fatalf("case version = %d, want exactly one bump from %d",
			afterCase.Version, beforeCase.Version)
	}
	if afterCase.EscalationReason != nil {
		t.Fatalf("escalation_reason after reopen = %v, want cleared", afterCase.EscalationReason)
	}
	if afterCase.ResolutionType != nil || afterCase.ResolvedAt != nil {
		t.Fatalf("reopened resolution = %v/%v, want cleared", afterCase.ResolutionType,
			afterCase.ResolvedAt)
	}
	if got := f.proposal(t, s.proposalID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("proposal = %s/version %d, want superseded/1", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1 approval.cancel", got)
	}

	var reopenAudits int
	for _, row := range f.audits(t, s.caseID) {
		if row.Action != casestate.ActionCaseTransition {
			continue
		}
		after := jsonObject(t, row.After)
		if after["status"] != "open" {
			continue
		}
		reopenAudits++
		before := jsonObject(t, row.Before)
		if before["status"] != "escalated" || before["escalation_reason"] != casestate.EscalationManual {
			t.Fatalf("reopen before = %v, want escalated/manual", before)
		}
		if after["reason"] != casestate.ReasonPartialLateMatch {
			t.Fatalf("reopen reason = %v, want exactly %s", after["reason"],
				casestate.ReasonPartialLateMatch)
		}
		if after["escalation_reason"] != nil {
			t.Fatalf("reopen after escalation_reason = %v, want null", after["escalation_reason"])
		}
	}
	if reopenAudits != 1 {
		t.Fatalf("escalated -> open audits = %d, want exactly 1", reopenAudits)
	}
}
