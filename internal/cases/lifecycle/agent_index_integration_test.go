//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestAgentSecondProposalStillHitsOneActiveIndex proves M4-10 does not widen
// agent proposal writes. The agent role can only INSERT, and an insert for a
// case that already has an active proposal still fails the partial unique
// index. It also cannot UPDATE the active proposal to supersede it.
func TestAgentSecondProposalStillHitsOneActiveIndex(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "proposed")
	ctx := context.Background()

	var runID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', $2)
		 RETURNING id::text`, caseID, f.clock.Now()).Scan(&runID); err != nil {
		t.Fatalf("seed second agent run: %v", err)
	}

	agent, err := db.OpenRole(ctx, testdb.RoleAgentRO, f.db.DSNFor(testdb.RoleAgentRO))
	if err != nil {
		t.Fatalf("open recon_agent_ro pool: %v", err)
	}
	t.Cleanup(agent.Close)

	_, err = agent.Pool.Exec(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'agent replacement',
		         0.9, 'agent-state-hash', $3, $4)`,
		caseID, runID, fmt.Sprintf("agent-second-%d", fixtureSeq.Add(1)), f.clock.Now())
	if err == nil {
		t.Fatal("agent second active proposal succeeded, want one-active index conflict")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("agent insert error = %v, want a PgError", err)
	}
	if pgErr.Code != "23505" || pgErr.ConstraintName != "proposals_one_active_per_case" {
		t.Fatalf("agent insert error = code %s constraint %s, want 23505/proposals_one_active_per_case",
			pgErr.Code, pgErr.ConstraintName)
	}
	if got := f.activeProposalCount(t, caseID); got != 1 {
		t.Fatalf("active proposals = %d, want 1", got)
	}

	_, err = agent.Pool.Exec(ctx,
		`UPDATE proposals SET status = 'superseded' WHERE id = $1::uuid`, oldID)
	if err == nil {
		t.Fatal("agent role updated a proposal, want permission denied")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("agent update error = %v, want SQLSTATE 42501", err)
	}
	if got := f.proposal(t, oldID); got.Status != "proposed" || got.Version != 0 {
		t.Fatalf("proposal = %s/version %d after agent update attempt, want untouched",
			got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d, want 0", got)
	}
}

// TestLifecycleServiceUsesApplicationRole is a small guard that the service
// was constructed with the app pool and that the cancel row is readable as
// recon_app; the integration fixture already runs every mutating call through
// that role.
func TestLifecycleServiceUsesApplicationRole(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	f.seedProposal(t, caseID, "proposed")
	if _, err := f.supersede(context.Background(), supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	}); err != nil {
		t.Fatalf("supersede as recon_app: %v", err)
	}
	if got := len(f.outbox(t)); got != 1 {
		t.Fatalf("outbox rows = %d, want 1", got)
	}
}
