//go:build integration

// covers: FIN-13, FIN-34
package policy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestPersistDecisionWithTransition proves the M4-04 persistence composes with
// the M4-03 state machine in one caller transaction: both writes commit
// together, and a stale transition rolls the decision back with it.
func TestPersistDecisionWithTransition(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)
	ensurePolicyVersion(t, ctx, app, loadedForTest(t))
	engine := engineForTest(t)
	decision := decisionForTier(t, engine, policy.TierT1)

	service, err := state.NewService(app, clock.NewFake(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	actor, err := audit.SystemActor("policy")
	if err != nil {
		t.Fatalf("SystemActor: %v", err)
	}

	_, _, proposalID := seedProposal(t, d)
	err = db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := policy.PersistDecision(ctx, tx, proposalID, decision); err != nil {
			return err
		}
		_, err := service.Transition(ctx, tx, state.TransitionRequest{
			ProposalID:      proposalID,
			To:              state.StatusPendingApproval,
			ExpectedVersion: 0,
			Actor:           actor,
			Reason:          "policy decision recorded",
		})
		return err
	})
	if err != nil {
		t.Fatalf("decision + transition: %v", err)
	}

	stored := readProposal(t, d, proposalID)
	if stored.Tier == nil || *stored.Tier != string(policy.TierT1) {
		t.Fatalf("risk_tier = %v, want T1", stored.Tier)
	}
	if len(stored.Decision) == 0 {
		t.Fatal("policy_decision is empty")
	}
	if stored.Status != "pending_approval" || stored.Version != 1 {
		t.Fatalf("status/version = %s/%d, want pending_approval/1", stored.Status, stored.Version)
	}
	if audits := countProposalAudits(t, d, proposalID); audits != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 (the transition)", audits)
	}

	// A stale transition after PersistDecision must roll both writes back.
	_, _, rollbackID := seedProposal(t, d)
	err = db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := policy.PersistDecision(ctx, tx, rollbackID, decision); err != nil {
			return err
		}
		_, err := service.Transition(ctx, tx, state.TransitionRequest{
			ProposalID:      rollbackID,
			To:              state.StatusPendingApproval,
			ExpectedVersion: 99,
			Actor:           actor,
		})
		return err
	})
	if !errors.Is(err, state.ErrStale) {
		t.Fatalf("stale transition error = %v, want state.ErrStale", err)
	}
	rolledBack := readProposal(t, d, rollbackID)
	if rolledBack.Tier != nil || rolledBack.Decision != nil {
		t.Fatalf("decision survived a rolled-back transaction: %+v", rolledBack)
	}
	if rolledBack.Status != "proposed" || rolledBack.Version != 0 {
		t.Fatalf("status/version = %s/%d, want proposed/0", rolledBack.Status, rolledBack.Version)
	}
	if audits := countProposalAudits(t, d, rollbackID); audits != 0 {
		t.Fatalf("audit rows after rollback = %d, want 0", audits)
	}
}
