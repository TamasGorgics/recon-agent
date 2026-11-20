//go:build integration

// covers: FIN-33
package approver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any test clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "eval/approver: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "eval/approver: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type integrationPolicyEngine struct{}

func (integrationPolicyEngine) Evaluate(policy.Proposal, policy.Verification, policy.Facts) policy.Decision {
	return policy.Decision{}
}

func (integrationPolicyEngine) ProposalTTL() time.Duration { return time.Hour }

type integrationVerifier struct{}

func (integrationVerifier) Verify(context.Context, string) (policy.Verification, error) {
	return policy.Verification{}, nil
}

type integrationLimits struct{}

func (integrationLimits) Check(context.Context, limits.Candidate) (limits.Result, error) {
	return limits.Result{}, nil
}

func newDecisionService(t *testing.T, d *testdb.DB, now time.Time) *approval.Service {
	t.Helper()
	fakeClock := clock.NewFake(now)
	app, err := db.OpenRole(context.Background(), db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(app.Close)

	stateService, err := state.NewService(app.Pool, fakeClock)
	if err != nil {
		t.Fatalf("state.NewService: %v", err)
	}
	loaded, err := policy.LoadFile("../../policy.yaml")
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	service, err := approval.NewService(approval.Options{
		Pool:     app.Pool,
		Clock:    fakeClock,
		State:    stateService,
		Verifier: integrationVerifier{},
		Limits:   integrationLimits{},
		Policy: approval.Policy{
			Engine: integrationPolicyEngine{},
			Loaded: loaded,
		},
		TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("approval.NewService: %v", err)
	}
	return service
}

func seedPendingProposal(t *testing.T, d *testdb.DB, now time.Time) (proposalID, caseID string) {
	t.Helper()
	ctx := context.Background()
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, created_at)
		 VALUES (0, 1000, 'USD', 'proposal_pending', 0, $1)
		 RETURNING id::text`, now).Scan(&caseID); err != nil {
		t.Fatalf("seed case: %v", err)
	}
	var runID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', $2)
		 RETURNING id::text`, caseID, now).Scan(&runID); err != nil {
		t.Fatalf("seed agent run: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, evidence,
		      confidence, status, version, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'seed rationale', '[]'::jsonb,
		         0.9, 'pending_approval', 0, 'state-hash',
		         'eval-approver-' || gen_random_uuid()::text, $3)
		 RETURNING id::text`, caseID, runID, now).Scan(&proposalID); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return proposalID, caseID
}

func insertApprover(t *testing.T, d *testdb.DB, id, name string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO approvers (id, display_name, role, active)
		 VALUES ($1::uuid, $2, 'approver', true)`, id, name); err != nil {
		t.Fatalf("seed approver: %v", err)
	}
}

func appendCoMakerAudit(t *testing.T, d *testdb.DB, actorID, caseID string, now time.Time) {
	t.Helper()
	err := db.WithTx(context.Background(), d.Pool, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			actor, err := audit.HumanActor(actorID)
			if err != nil {
				return err
			}
			_, err = audit.Append(ctx, tx, audit.Entry{
				Actor:      actor,
				Action:     "test.co_maker",
				EntityType: "case",
				EntityID:   caseID,
				Timestamp:  now,
			})
			return err
		})
	if err != nil {
		t.Fatalf("append co-maker audit: %v", err)
	}
}

func TestSyntheticApproverSeedPassesDecideAndFourEyes(t *testing.T) {
	d := testdb.New(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	service := newDecisionService(t, d, now)
	ctx := context.Background()

	// Seeding is idempotent and writes the fixed synthetic identity with no
	// channel identifiers.
	if err := EnsureSyntheticApprover(ctx, d.Pool, SyntheticApproverID); err != nil {
		t.Fatalf("first EnsureSyntheticApprover: %v", err)
	}
	if err := EnsureSyntheticApprover(ctx, d.Pool, SyntheticApproverID); err != nil {
		t.Fatalf("second EnsureSyntheticApprover: %v", err)
	}
	var count int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM approvers WHERE id = $1::uuid`, SyntheticApproverID).
		Scan(&count); err != nil {
		t.Fatalf("count synthetic approver: %v", err)
	}
	if count != 1 {
		t.Fatalf("synthetic approver rows = %d, want 1", count)
	}
	var (
		display      string
		role         string
		active       bool
		slackUserID  *string
		ticketUserID *string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT display_name, role, active, slack_user_id, ticket_user_id
		   FROM approvers WHERE id = $1::uuid`, SyntheticApproverID).
		Scan(&display, &role, &active, &slackUserID, &ticketUserID); err != nil {
		t.Fatalf("read synthetic approver: %v", err)
	}
	if display != SyntheticApproverDisplayName || role != SyntheticApproverRole || !active {
		t.Fatalf("synthetic approver = %q/%q/%v, want %q/%q/true",
			display, role, active, SyntheticApproverDisplayName, SyntheticApproverRole)
	}
	if slackUserID != nil || ticketUserID != nil {
		t.Fatalf("synthetic approver channel ids = %v/%v, want NULL/NULL", slackUserID, ticketUserID)
	}

	// The synthetic identity passes Service.Decide's four-eyes check and is
	// recorded as the e2e source with a human audit actor.
	proposalID, _ := seedPendingProposal(t, d, now)
	result, err := service.Decide(ctx, proposalID, approval.DecisionApproved,
		SyntheticApproverID, SyntheticSource)
	if err != nil {
		t.Fatalf("Decide with synthetic approver: %v", err)
	}
	if result.Decision != approval.DecisionApproved ||
		result.ApproverID != SyntheticApproverID ||
		result.Status != statusApproved ||
		result.Replayed {
		t.Fatalf("decision result = %+v, want a fresh approved synthetic decision", result)
	}

	var (
		channel    string
		decision   string
		approverID string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT channel, decision, approver_id::text
		   FROM approvals WHERE proposal_id = $1::uuid`, proposalID).
		Scan(&channel, &decision, &approverID); err != nil {
		t.Fatalf("read approval row: %v", err)
	}
	if channel != SyntheticSource || decision != string(approval.DecisionApproved) ||
		approverID != SyntheticApproverID {
		t.Fatalf("approval row = %q/%q/%s, want e2e/approved/%s",
			channel, decision, approverID, SyntheticApproverID)
	}

	var decidedAudits int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		  WHERE entity_id = $1 AND action = $2 AND actor_type = 'human' AND actor_id = $3`,
		proposalID, approval.ActionApprovalDecided, SyntheticApproverID).
		Scan(&decidedAudits); err != nil {
		t.Fatalf("count decision audits: %v", err)
	}
	if decidedAudits != 1 {
		t.Fatalf("decision audit rows = %d, want 1", decidedAudits)
	}

	// Repeating the same decision is a service-level replay: nothing new is
	// written and the stored decision time is preserved.
	replayed, err := service.Decide(ctx, proposalID, approval.DecisionApproved,
		SyntheticApproverID, SyntheticSource)
	if err != nil {
		t.Fatalf("replayed Decide: %v", err)
	}
	if !replayed.Replayed || !replayed.DecidedAt.Equal(result.DecidedAt) {
		t.Fatalf("replayed result = %+v, want Replayed with the stored time %v",
			replayed, result.DecidedAt)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`, proposalID).
		Scan(&count); err != nil {
		t.Fatalf("count approvals after replay: %v", err)
	}
	if count != 1 {
		t.Fatalf("approval rows after replay = %d, want 1", count)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		  WHERE entity_id = $1 AND action = $2`, proposalID, approval.ActionApprovalDecided).
		Scan(&decidedAudits); err != nil {
		t.Fatalf("count decision audits after replay: %v", err)
	}
	if decidedAudits != 1 {
		t.Fatalf("decision audit rows after replay = %d, want 1", decidedAudits)
	}

	// A human co-maker UUID is rejected by four-eyes.
	coMakerID := "55555555-5555-4555-8555-555555555555"
	insertApprover(t, d, coMakerID, "Co maker")
	coProposalID, coCaseID := seedPendingProposal(t, d, now)
	appendCoMakerAudit(t, d, coMakerID, coCaseID, now)
	if _, err := service.Decide(ctx, coProposalID, approval.DecisionApproved,
		coMakerID, SyntheticSource); !errors.Is(err, approval.ErrCoMaker) {
		t.Fatalf("Decide with co-maker = %v, want approval.ErrCoMaker", err)
	}

	// Agent and system actor strings never reach the database as a human
	// approver (FIN-33).
	if _, err := service.Decide(ctx, coProposalID, approval.DecisionApproved,
		"agent:"+SyntheticApproverID, SyntheticSource); !errors.Is(err, approval.ErrAgentActor) {
		t.Fatalf("Decide with agent actor = %v, want approval.ErrAgentActor", err)
	}
	if _, err := service.Decide(ctx, coProposalID, approval.DecisionApproved,
		"system:"+SyntheticApproverID, SyntheticSource); !errors.Is(err, approval.ErrAgentActor) {
		t.Fatalf("Decide with system actor = %v, want approval.ErrAgentActor", err)
	}
}
