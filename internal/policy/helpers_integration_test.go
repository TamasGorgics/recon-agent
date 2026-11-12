//go:build integration

package policy_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any test clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "policy: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "policy: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func openRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	rp, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(rp.Close)
	return rp.Pool
}

var policyProposalCounter atomic.Int64

// seedProposal inserts one proposed proposal with a distinct case and agent
// run. It mirrors the M2-01 fixture rows used by the db grant tests.
func seedProposal(t *testing.T, d *testdb.DB) (caseID, runID, proposalID string) {
	t.Helper()
	ctx := context.Background()
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, created_at)
		 VALUES (0, 100, 'USD', now())
		 RETURNING id::text`).Scan(&caseID); err != nil {
		t.Fatalf("create case: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		t.Fatalf("create agent run: %v", err)
	}
	key := fmt.Sprintf("m4-04-policy-%d", policyProposalCounter.Add(1))
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, state_hash, idempotency_key, created_at, expires_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'seed', 0.9000,
		         'proposed', 'state-hash', $3, now(), '2027-01-01T00:00:00Z')
		 RETURNING id::text`,
		caseID, runID, key).Scan(&proposalID); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}
	return caseID, runID, proposalID
}

type storedProposal struct {
	Tier       *string
	Decision   []byte
	Status     string
	Version    int
	ExpiresAt  *time.Time
	CaseID     string
	Rationale  string
	StateHash  string
	Confidence string
}

func readProposal(t *testing.T, d *testdb.DB, proposalID string) storedProposal {
	t.Helper()
	var p storedProposal
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT risk_tier, policy_decision, status, version, expires_at,
		        case_id::text, rationale, state_hash, confidence::text
		   FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&p.Tier, &p.Decision, &p.Status, &p.Version, &p.ExpiresAt,
			&p.CaseID, &p.Rationale, &p.StateHash, &p.Confidence); err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	return p
}

func countProposalAudits(t *testing.T, d *testdb.DB, proposalID string) int {
	t.Helper()
	var count int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log
		  WHERE entity_type = 'proposal' AND entity_id = $1`, proposalID).Scan(&count); err != nil {
		t.Fatalf("count proposal audits: %v", err)
	}
	return count
}
