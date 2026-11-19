//go:build integration

// covers: FIN-24, FIN-30, FIN-41

package executor_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// lateMatchRig extends the executor world with the production matcher and its
// real late-match hook. The case, break, and approved proposal come from the
// executor fixtures; the match is always created by match.Engine.Run.
type lateMatchRig struct {
	*world
	engine *match.Engine
}

func (w *world) newLateMatchRig(t *testing.T) *lateMatchRig {
	t.Helper()
	return w.newLateMatchRigWithHook(t, nil)
}

// newLateMatchRigWithHook builds the same real matcher/lifecycle rig and lets a
// test wrap the production late-match hook for deterministic interleavings.
// wrap nil keeps the production hook unchanged.
func (w *world) newLateMatchRigWithHook(t *testing.T,
	wrap func(match.LateMatchHook) match.LateMatchHook) *lateMatchRig {
	t.Helper()
	ctx := context.Background()
	appPool, err := db.OpenRole(ctx, db.RoleApp, w.db.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool for lifecycle: %v", err)
	}
	t.Cleanup(appPool.Close)
	lifecycleService, err := lifecycle.NewService(lifecycle.Options{
		DB:    appPool.Pool,
		Clock: w.clock,
	})
	if err != nil {
		t.Fatalf("new lifecycle service: %v", err)
	}
	matcherActor, err := audit.SystemActor("matcher")
	if err != nil {
		t.Fatalf("parse matcher actor: %v", err)
	}
	hook := match.LateMatchHook(lifecycleService.ReconcileLateMatchHook)
	if wrap != nil {
		hook = wrap(hook)
	}
	engine, err := match.New(match.Options{
		DB:        w.db.Pool,
		Clock:     w.clock,
		Actor:     matcherActor,
		Config:    config.MatchConfig{FeeToleranceMinor: map[string]int64{"USD": 100}},
		LateMatch: hook,
	})
	if err != nil {
		t.Fatalf("new matcher: %v", err)
	}
	return &lateMatchRig{world: w, engine: engine}
}

func (r *lateMatchRig) window() match.Window {
	return match.Window{
		From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
	}
}

func (r *lateMatchRig) run(ctx context.Context) (match.RunResult, error) {
	return r.engine.Run(ctx, match.RunRequest{ScopeID: r.scopeID, Window: r.window()})
}

// lateMatchCaseSpec is the shared executor fixture shape for a case whose
// external-only break is consumed by the next matcher run. The referenced
// ledger transaction added by postReferencedLedger has the same amount and
// value date, so the real matcher creates a real match.
func lateMatchCaseSpec(proposalStatus string) caseSpec {
	return caseSpec{
		proposalStatus:    proposalStatus,
		externalValueDate: "2026-01-15",
		externalAmount:    500,
	}
}

// postReferencedLedger posts one balanced ledger transaction in the executor
// fixture scope whose scope-account line carries the external record's
// source_record_id as its external_ref. The matcher requires a non-null
// reference on ledger candidates, and the executor fixture's default ledger
// helper does not set one.
func (w *world) postReferencedLedger(t *testing.T, externalRecordID string,
	amountMinor int64, valueDate string) string {
	t.Helper()
	ctx := context.Background()
	var sourceRecordID string
	if err := w.db.Pool.QueryRow(ctx,
		`SELECT source_record_id FROM external_records WHERE id = $1::uuid`,
		externalRecordID).Scan(&sourceRecordID); err != nil {
		t.Fatalf("read external source record id: %v", err)
	}
	actor, err := audit.SystemActor("fixture")
	if err != nil {
		t.Fatalf("fixture actor: %v", err)
	}
	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin referenced ledger fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	result, err := w.ledgerSvc.Post(ctx, tx, ledger.PostRequest{
		IdempotencyKey: fmt.Sprintf("late-match-ledger-%d", fixtureSeq.Add(1)),
		Kind:           ledger.KindCharge,
		ValueDate:      mustDate(t, valueDate),
		Description:    "late-match referenced ledger fixture",
		Actor:          actor,
		Lines: []ledger.Line{
			{
				AccountID:   w.accounts["psp_clearing_usd"],
				Direction:   ledger.Debit,
				AmountMinor: amountMinor,
				Currency:    "USD",
				ExternalRef: sourceRecordID,
			},
			{
				AccountID:   w.accounts["fx_gain_loss"],
				Direction:   ledger.Credit,
				AmountMinor: amountMinor,
				Currency:    "USD",
			},
		},
	})
	if err != nil {
		t.Fatalf("post referenced ledger txn: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit referenced ledger txn: %v", err)
	}
	return result.ID
}

// TestLateMatchApprovedProposalLeftForExecutorRecordssBreakResolved covers the
// approved/auto-approved late-match acceptance: reconciliation resolves the
// case superseded_by_match and deliberately leaves the proposal untouched; the
// existing executor apply path then records apply_failed(break_resolved) with
// no ledger/applied writes and no case write.
func TestLateMatchApprovedProposalLeftForExecutorRecordsBreakResolved(t *testing.T) {
	for _, proposalStatus := range []string{"approved", "auto_approved"} {
		proposalStatus := proposalStatus
		t.Run(proposalStatus, func(t *testing.T) {
			w := newWorld(t)
			fixture := w.newCase(t, lateMatchCaseSpec(proposalStatus))
			w.postReferencedLedger(t, fixture.externalRecordID, 500, "2026-01-15")
			rig := w.newLateMatchRig(t)
			ctx := context.Background()

			runResult, err := rig.run(ctx)
			if err != nil {
				t.Fatalf("run matcher: %v", err)
			}
			if runResult.Stats.Matched != 1 {
				t.Fatalf("matched = %d, want 1 (stats: %+v)", runResult.Stats.Matched, runResult.Stats)
			}

			assertProposalRow(t, w, fixture.proposalID, proposalStatus, 0)
			if got := w.breakStatus(t, fixture.breakID); got != "superseded" {
				t.Fatalf("break status = %s, want superseded", got)
			}
			if got := w.status(t, fixture.caseID); got != "resolved" {
				t.Fatalf("case status = %s, want resolved", got)
			}
			if got := w.oneString(t,
				`SELECT resolution_type FROM cases WHERE id = $1::uuid`, fixture.caseID); got !=
				casestate.ResolutionSupersededByMatch {
				t.Fatalf("case resolution_type = %q, want %s", got,
					casestate.ResolutionSupersededByMatch)
			}
			if got := w.count(t,
				`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_id = $2`,
				lifecycle.ActionCaseProposalSuperseded, fixture.caseID); got != 0 {
				t.Fatalf("case.proposal_superseded audits = %d, want 0 for an approved proposal", got)
			}
			if got := w.count(t,
				`SELECT count(*) FROM outbox WHERE topic = $1`, lifecycle.TopicApprovalCancel); got != 0 {
				t.Fatalf("approval.cancel rows = %d, want 0 for an approved proposal", got)
			}
			if got := w.count(t,
				`SELECT count(*) FROM audit_log WHERE action = 'case.transition' AND entity_id = $1`,
				fixture.caseID); got != 1 {
				t.Fatalf("case.transition audits = %d, want exactly one late-match transition", got)
			}
			caseBeforeApply := w.status(t, fixture.caseID)

			applied, err := w.service.Apply(ctx, fixture.proposalID)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if applied.Status != executor.StatusApplyFailed ||
				applied.FailureReason != executor.FailureBreakResolved {
				t.Fatalf("apply result = %+v, want apply_failed/break_resolved", applied)
			}
			if got := w.count(t,
				`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`,
				fixture.proposalID); got != 0 {
				t.Errorf("applied_proposals rows = %d, want 0", got)
			}
			if got := w.count(t,
				`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`,
				"proposal:"+fixture.proposalID); got != 0 {
				t.Errorf("ledger txns = %d, want 0", got)
			}
			if got := w.count(t,
				`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
				"proposal.applied:"+fixture.proposalID); got != 0 {
				t.Errorf("proposal.applied outbox rows = %d, want 0", got)
			}
			assertProposalRow(t, w, fixture.proposalID, "apply_failed", 1)
			var reason string
			if err := w.db.Pool.QueryRow(ctx,
				`SELECT COALESCE(after->>'reason', '') FROM audit_log
				  WHERE action = 'proposal.transition' AND entity_id = $1
				  ORDER BY seq DESC LIMIT 1`, fixture.proposalID).Scan(&reason); err != nil {
				t.Fatalf("read apply_failed reason: %v", err)
			}
			if reason != executor.FailureBreakResolved {
				t.Fatalf("apply_failed reason = %q, want %q", reason, executor.FailureBreakResolved)
			}

			if got := w.status(t, fixture.caseID); got != caseBeforeApply {
				t.Fatalf("case status after apply = %s, want unchanged %s", got, caseBeforeApply)
			}
			if got := w.oneString(t,
				`SELECT resolution_type FROM cases WHERE id = $1::uuid`, fixture.caseID); got !=
				casestate.ResolutionSupersededByMatch {
				t.Fatalf("case resolution_type after apply = %q, want %s", got,
					casestate.ResolutionSupersededByMatch)
			}
			if got := w.count(t,
				`SELECT count(*) FROM audit_log WHERE action = 'case.transition' AND entity_id = $1`,
				fixture.caseID); got != 1 {
				t.Fatalf("case.transition audits after apply = %d, want still 1 (no executor case write)", got)
			}
			if got := w.count(t,
				`SELECT count(*) FROM audit_log WHERE action = 'executor.applied' AND entity_id = $1`,
				fixture.proposalID); got != 0 {
				t.Fatalf("executor.applied audits = %d, want 0", got)
			}

			// A repeated apply sees apply_failed, a non-applicable status, and
			// writes no second verdict or side effect.
			beforeAudits := w.count(t, `SELECT count(*) FROM audit_log`)
			if _, err := w.service.Apply(ctx, fixture.proposalID); !errors.Is(err, executor.ErrNotApplicable) {
				t.Fatalf("second Apply error = %v, want ErrNotApplicable", err)
			}
			if got := w.count(t, `SELECT count(*) FROM audit_log`); got != beforeAudits {
				t.Fatalf("audit rows after repeated apply = %d, want %d", got, beforeAudits)
			}
			if got := w.count(t,
				`SELECT count(*) FROM audit_log WHERE action = 'proposal.transition' AND entity_id = $1`,
				fixture.proposalID); got != 1 {
				// The SQL seed writes no proposal transition; apply_failed is
				// the only verdict.
				t.Fatalf("proposal.transition audits = %d, want exactly 1", got)
			}
		})
	}
}

// assertProposalRow reads one proposal's status and version.
func assertProposalRow(t *testing.T, w *world, proposalID, wantStatus string, wantVersion int) {
	t.Helper()
	var (
		status  string
		version int
	)
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT status, version FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&status, &version); err != nil {
		t.Fatalf("read proposal %s: %v", proposalID, err)
	}
	if status != wantStatus || version != wantVersion {
		t.Fatalf("proposal = %s/version %d, want %s/version %d",
			status, version, wantStatus, wantVersion)
	}
}

// TestConcurrentApplyAndLateMatchKeepsInvariants runs a deterministic
// overlapping interleaving of the approved-proposal executor apply and the real
// matcher's late-match hook:
//
//  1. the matcher creates the match and its legs, then pauses inside the
//     production LateMatch hook, with its transaction still open;
//  2. the executor apply runs to completion while that match transaction is
//     open (the matcher holds no case/break lock yet);
//  3. the hook is released and reconciles against the executor-resolved case.
//
// The invariant set is asserted after both complete: the case is resolved, the
// break is terminal, the proposal has exactly one applied verdict, the approved
// proposal is not cancelled, and the case/break/proposal/match audits are not
// duplicated or lost.
//
// The opposite ordering (late match resolves the case first, then Apply records
// apply_failed(break_resolved)) is covered deterministically by
// TestLateMatchApprovedProposalLeftForExecutorRecordsBreakResolved.
//
// Both operations use the existing db.WithTx deadlock/serialization retry; this
// test adds no retry of its own.
func TestConcurrencyApplyVsLateMatchKeepsInvariants(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	fixture := w.newCase(t, lateMatchCaseSpec("approved"))
	w.postReferencedLedger(t, fixture.externalRecordID, 500, "2026-01-15")

	matchInserted := make(chan struct{})
	releaseReconcile := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseReconcile) }) }
	t.Cleanup(release)

	rig := w.newLateMatchRigWithHook(t, func(next match.LateMatchHook) match.LateMatchHook {
		return func(ctx context.Context, tx pgx.Tx, matchID, actor string) error {
			close(matchInserted)
			<-releaseReconcile
			return next(ctx, tx, matchID, actor)
		}
	})

	runErr := make(chan error, 1)
	go func() {
		_, err := rig.run(ctx)
		runErr <- err
	}()
	select {
	case <-matchInserted:
	case err := <-runErr:
		t.Fatalf("matcher finished before the late-match hook: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("matcher did not reach the late-match hook")
	}

	// The match transaction is open but paused before any case/break write.
	// The executor apply must win cleanly.
	applied, err := w.service.Apply(ctx, fixture.proposalID)
	if err != nil {
		t.Fatalf("concurrent Apply: %v", err)
	}
	if applied.Status != executor.StatusApplied || applied.Replayed {
		t.Fatalf("apply result = %+v, want applied/not replayed", applied)
	}
	release()
	if err := <-runErr; err != nil {
		t.Fatalf("matcher run after the executor apply: %v", err)
	}

	if got := w.status(t, fixture.caseID); got != "resolved" {
		t.Fatalf("case status = %s, want resolved", got)
	}
	if got := w.oneString(t,
		`SELECT resolution_type FROM cases WHERE id = $1::uuid`, fixture.caseID); got != "adjustment" {
		t.Fatalf("case resolution_type = %q, want adjustment (the executor won)", got)
	}
	if got := w.breakStatus(t, fixture.breakID); got != "resolved" {
		t.Fatalf("break status = %s, want resolved", got)
	}
	assertProposalRow(t, w, fixture.proposalID, "applied", 1)
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'proposal.transition' AND entity_id = $1`,
		fixture.proposalID); got != 1 {
		t.Fatalf("proposal.transition audits = %d, want exactly 1 verdict", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`,
		fixture.proposalID); got != 1 {
		t.Fatalf("applied_proposals rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+fixture.proposalID); got != 1 {
		t.Fatalf("proposal.applied outbox rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM outbox WHERE topic = $1`, lifecycle.TopicApprovalCancel); got != 0 {
		t.Fatalf("approval.cancel rows = %d, want 0 (approved proposals are never cancelled)", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_id = $2`,
		lifecycle.ActionCaseProposalSuperseded, fixture.caseID); got != 0 {
		t.Fatalf("case.proposal_superseded audits = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'case.transition' AND entity_id = $1`,
		fixture.caseID); got != 1 {
		t.Fatalf("case.transition audits = %d, want exactly 1 (no duplicate/lost transition)", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'break.resolve' AND entity_id = $1`,
		fixture.breakID); got != 1 {
		t.Fatalf("break.resolve audits = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_id = $2`,
		lifecycle.ActionBreakSuperseded, fixture.breakID); got != 0 {
		t.Fatalf("break.superseded audits = %d, want 0 after an executor win", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_id = $2`,
		lifecycle.ActionCaseLateMatchSkipped, fixture.caseID); got != 1 {
		t.Fatalf("case.late_match_skipped audits = %d, want exactly 1 after an executor win", got)
	}

	// The matcher created exactly one active match and one match.run audit; an
	// aborted/retried transaction must leave no duplicate or missing verdict.
	var sourceRecordID string
	if err := w.db.Pool.QueryRow(ctx,
		`SELECT source_record_id FROM external_records WHERE id = $1::uuid`,
		fixture.externalRecordID).Scan(&sourceRecordID); err != nil {
		t.Fatalf("read external source record id: %v", err)
	}
	if got := w.count(t,
		`SELECT count(*) FROM matches m
		  JOIN external_records er ON er.id = m.external_record_id
		  WHERE er.source_record_id = $1 AND m.status = 'active'`, sourceRecordID); got != 1 {
		t.Fatalf("active matches for the external record = %d, want exactly 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'match.run'`); got != 1 {
		t.Fatalf("match.run audits = %d, want exactly 1", got)
	}
}
