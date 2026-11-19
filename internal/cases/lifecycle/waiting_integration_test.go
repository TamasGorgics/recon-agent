//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	caselifecycle "github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// resumeWaiting runs the production service in its own transaction, the way
// M4-12's request-info callback will.
func (f *fixture) resumeWaiting(t *testing.T, caseID, actor, reason string) (casestate.Snapshot, error) {
	t.Helper()
	var snapshot casestate.Snapshot
	err := db.WithTx(context.Background(), f.app, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			snapshot, err = f.service.ResumeWaiting(ctx, tx, caseID, actor, reason)
			return err
		})
	return snapshot, err
}

func TestResumeWaitingHumanReplyReopensAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "waiting_info", "")

	snapshot, err := f.resumeWaiting(t, caseID, "human:alice", caselifecycle.ReasonHumanReply)
	if err != nil {
		t.Fatalf("ResumeWaiting: %v", err)
	}
	if snapshot.Status != casestate.StatusOpen || snapshot.Version != 1 {
		t.Fatalf("snapshot = %s/version %d, want open/1", snapshot.Status, snapshot.Version)
	}
	transitions := f.caseTransitionAudits(t, caseID)
	if len(transitions) != 1 {
		t.Fatalf("case.transition audits = %d, want 1", len(transitions))
	}
	after := jsonObject(t, transitions[0].After)
	if after["status"] != "open" || after["reason"] != caselifecycle.ReasonHumanReply {
		t.Fatalf("transition after = %+v, want open/human_reply", after)
	}
	before := jsonObject(t, transitions[0].Before)
	if before["status"] != "waiting_info" {
		t.Fatalf("transition before status = %v, want waiting_info", before["status"])
	}
	if transitions[0].ActorType != "human" || transitions[0].ActorID != "alice" {
		t.Fatalf("transition actor = %s:%s, want human:alice",
			transitions[0].ActorType, transitions[0].ActorID)
	}

	repeated, err := f.resumeWaiting(t, caseID, "human:alice", caselifecycle.ReasonHumanReply)
	if err != nil {
		t.Fatalf("repeated ResumeWaiting: %v", err)
	}
	if repeated.Status != casestate.StatusOpen || repeated.Version != 1 {
		t.Fatalf("repeated snapshot = %s/version %d, want open/1",
			repeated.Status, repeated.Version)
	}
	if got := len(f.caseTransitionAudits(t, caseID)); got != 1 {
		t.Fatalf("case.transition audits after replay = %d, want 1", got)
	}
}

func TestResumeWaitingHandlesOtherStatuses(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name       string
		status     string
		resolution string
		wantErr    bool
	}{
		{name: "open no-op", status: "open"},
		{name: "in agent", status: "in_agent", wantErr: true},
		{name: "proposal pending", status: "proposal_pending", wantErr: true},
		{name: "escalated", status: "escalated", wantErr: true},
		{name: "resolved", status: "resolved", resolution: "manual", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caseID := f.seedCase(t, tc.status, tc.resolution)
			before := f.caseRow(t, caseID)

			snapshot, err := f.resumeWaiting(t, caseID, "human:alice",
				caselifecycle.ReasonHumanReply)
			if tc.wantErr {
				if !errors.Is(err, casestate.ErrIllegalTransition) {
					t.Fatalf("error = %v, want ErrIllegalTransition", err)
				}
			} else {
				if err != nil {
					t.Fatalf("ResumeWaiting: %v", err)
				}
				if snapshot.Status != casestate.StatusOpen {
					t.Fatalf("snapshot status = %s, want open", snapshot.Status)
				}
			}
			if after := f.caseRow(t, caseID); !equalStoredCase(after, before) {
				t.Fatalf("case changed from %+v to %+v", before, after)
			}
			if got := len(f.audits(t, caseID)); got != 0 {
				t.Fatalf("case audits = %d, want 0", got)
			}
		})
	}
}

func TestIngestTouchedWaitingCaseResumesInAttachTransaction(t *testing.T) {
	f := newLateMatcherFixture(t)
	f.addExternal(t, "WAIT-INGEST", -10_000, lateValueDate)
	first := f.run(t)
	if len(first.Breaks) != 1 {
		t.Fatalf("matcher breaks = %d, want 1", len(first.Breaks))
	}
	created, err := f.caseService.CreateBreaks(context.Background(), cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: first.RunID,
		Breaks:     first.Breaks,
	})
	if err != nil {
		t.Fatalf("CreateBreaks: %v", err)
	}
	if created.CreatedBreaks != 1 || len(created.Cases) != 1 {
		t.Fatalf("created %d breaks / %d cases, want 1/1",
			created.CreatedBreaks, len(created.Cases))
	}
	caseID := created.Cases[0].ID
	f.transitionCase(t, caseID, casestate.StatusInAgent, "system:cases", "", "")
	f.transitionCase(t, caseID, casestate.StatusWaitingInfo, "system:cases", "", "")
	waiting := f.caseRow(t, caseID)
	if waiting.Status != "waiting_info" {
		t.Fatalf("case status = %q, want waiting_info", waiting.Status)
	}

	caseActor, err := audit.SystemActor("cases")
	if err != nil {
		t.Fatalf("parse cases actor: %v", err)
	}
	hooked, err := cases.NewService(f.app, f.clock, caseActor,
		cases.WithCaseTouchedHook(f.service.OnIngestTouchedCase))
	if err != nil {
		t.Fatalf("new hooked case service: %v", err)
	}
	// The same ingest data re-runs: the break signature already exists and is
	// attached, so the pass touches the waiting case in its transaction.
	second, err := hooked.CreateBreaks(context.Background(), cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: first.RunID,
		Breaks:     first.Breaks,
	})
	if err != nil {
		t.Fatalf("hooked CreateBreaks: %v", err)
	}
	if second.CreatedBreaks != 0 || len(second.Cases) != 0 {
		t.Fatalf("second pass created %d breaks / %d cases, want 0/0",
			second.CreatedBreaks, len(second.Cases))
	}

	resumed := f.caseRow(t, caseID)
	if resumed.Status != "open" || resumed.Version != waiting.Version+1 {
		t.Fatalf("case = %s/version %d, want open/%d",
			resumed.Status, resumed.Version, waiting.Version+1)
	}
	if resumed.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 (waiting resume is not a retry)", resumed.Attempts)
	}
	transitions := f.caseTransitionAudits(t, caseID)
	if len(transitions) != 3 {
		t.Fatalf("case.transition audits = %d, want 3 (in_agent, waiting_info, ingest resume)",
			len(transitions))
	}
	resumeAudit := transitions[len(transitions)-1]
	after := jsonObject(t, resumeAudit.After)
	if after["status"] != "open" || after["reason"] != caselifecycle.ReasonNewIngest {
		t.Fatalf("resume audit after = %+v, want open/new_ingest", after)
	}
	if resumeAudit.ActorType != "system" || resumeAudit.ActorID != "ingest" {
		t.Fatalf("resume actor = %s:%s, want system:ingest",
			resumeAudit.ActorType, resumeAudit.ActorID)
	}

	// A third pass sees open: no new transition and no duplicate audit.
	if _, err := hooked.CreateBreaks(context.Background(), cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: first.RunID,
		Breaks:     first.Breaks,
	}); err != nil {
		t.Fatalf("third CreateBreaks: %v", err)
	}
	if got := f.caseRow(t, caseID); !equalStoredCase(got, resumed) {
		t.Fatalf("third pass changed case: %+v -> %+v", resumed, got)
	}
	if got := len(f.caseTransitionAudits(t, caseID)); got != 3 {
		t.Fatalf("case.transition audits after third pass = %d, want 3", got)
	}
}

func TestIngestTouchHookFailureRollsBackBreakCreation(t *testing.T) {
	f := newLateMatcherFixture(t)
	f.addExternal(t, "WAIT-FAIL", -20_000, lateValueDate)
	first := f.run(t)
	if len(first.Breaks) != 1 {
		t.Fatalf("matcher breaks = %d, want 1", len(first.Breaks))
	}
	caseActor, err := audit.SystemActor("cases")
	if err != nil {
		t.Fatalf("parse cases actor: %v", err)
	}
	boom := errors.New("injected touch failure")
	hooked, err := cases.NewService(f.app, f.clock, caseActor,
		cases.WithCaseTouchedHook(func(context.Context, pgx.Tx, string) error {
			return boom
		}))
	if err != nil {
		t.Fatalf("new hooked case service: %v", err)
	}
	_, err = hooked.CreateBreaks(context.Background(), cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: first.RunID,
		Breaks:     first.Breaks,
	})
	if !errors.Is(err, boom) {
		t.Fatalf("CreateBreaks error = %v, want the hook error", err)
	}
	signature := first.Breaks[0].Signature()
	if got := f.count(t, `SELECT count(*) FROM breaks WHERE signature = $1`, signature); got != 0 {
		t.Fatalf("breaks persisted after hook failure = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM cases`); got != 0 {
		t.Fatalf("cases persisted after hook failure = %d, want 0", got)
	}
}
