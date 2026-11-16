//go:build integration

package limits_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "limits: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "limits: ensure template: %v\n", err)
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

var fixtureSeq atomic.Int64

// testCandidateID is a canonical lower-case UUID for candidates that are not
// persisted. Passing it exercises ProposalID validation while the exclusion is
// a no-op because no such row exists.
const testCandidateID = "11111111-1111-4111-8111-111111111111"

type fixture struct {
	d          *testdb.DB
	pool       *pgxpool.Pool
	service    *limits.Service
	clock      *clock.Fake
	scopeID    string
	matchRunID string
	batchID    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	database := testdb.New(t)
	app := openRolePool(t, database, db.RoleApp)
	clk := clock.NewFake(testNow)
	svc, err := limits.NewService(limits.Options{
		Pool:   app,
		Clock:  clk,
		Config: config.DefaultAutoApplyLimitsConfig(),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f := &fixture{d: database, pool: app, service: svc, clock: clk}
	f.seedWorld(t)
	return f
}

func (f *fixture) seedWorld(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var accountID string
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency)
		 VALUES ($1, 'limits test account', 'asset', 'USD')
		 RETURNING id::text`, "ACC-LIMITS-"+t.Name()).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD') RETURNING id::text`, accountID).Scan(&f.scopeID); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'limits-config-hash')
		 RETURNING id::text`, f.scopeID, testNow).Scan(&f.matchRunID); err != nil {
		t.Fatalf("seed match run: %v", err)
	}
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ('psp_a', 0, 'USD', $1, 0, 0)
		 RETURNING id::text`, "limits-file-"+t.Name()).Scan(&f.batchID); err != nil {
		t.Fatalf("seed settlement batch: %v", err)
	}
}

func (f *fixture) setSwitch(t *testing.T, enabled bool, reason string) {
	t.Helper()
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE system_switches
		    SET enabled = $1, reason = $2, updated_by = 'test:fixture', updated_at = $3
		  WHERE name = 'auto_apply_enabled'`, enabled, reason, testNow); err != nil {
		t.Fatalf("set auto-apply switch: %v", err)
	}
}

type proposalSpec struct {
	proposalType string
	payload      string
	currency     string
	tier         string
	status       string
	createdAt    time.Time
	appliedAt    *time.Time
}

func (f *fixture) seedProposal(t *testing.T, spec proposalSpec) (proposalID string) {
	t.Helper()
	if spec.proposalType == "" {
		spec.proposalType = "adjustment"
	}
	if spec.payload == "" {
		spec.payload = `{}`
	}
	if spec.currency == "" {
		spec.currency = "USD"
	}
	if spec.tier == "" {
		spec.tier = "T0"
	}
	if spec.status == "" {
		spec.status = "auto_approved"
	}
	if spec.createdAt.IsZero() {
		spec.createdAt = testNow.Add(-time.Hour)
	}
	ctx := context.Background()
	var caseID string
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES (0, 100, $1, 'open', 0, 0, $2)
		 RETURNING id::text`, spec.currency, spec.createdAt).Scan(&caseID); err != nil {
		t.Fatalf("seed case: %v", err)
	}
	var runID string
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', $2)
		 RETURNING id::text`, caseID, spec.createdAt).Scan(&runID); err != nil {
		t.Fatalf("seed agent run: %v", err)
	}
	key := fmt.Sprintf("limits-%d", fixtureSeq.Add(1))
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      risk_tier, status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, 'seed', 0.9000,
		         $5, $6, 'state-hash', $7, $8)
		 RETURNING id::text`,
		caseID, runID, spec.proposalType, spec.payload, spec.tier, spec.status, key, spec.createdAt).
		Scan(&proposalID); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	if spec.appliedAt != nil {
		if _, err := f.d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, $2, 'limits-test-executor')`, proposalID, *spec.appliedAt); err != nil {
			t.Fatalf("seed applied proposal: %v", err)
		}
	}
	return proposalID
}

// setProposalStatus simulates M4-11 persisting a Check result before it checks
// the next candidate.
func (f *fixture) setProposalStatus(t *testing.T, proposalID, status string) {
	t.Helper()
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE proposals SET status = $2 WHERE id = $1::uuid`, proposalID, status); err != nil {
		t.Fatalf("set proposal status: %v", err)
	}
}

func lineJSON(amount int64, currency, direction string) string {
	return fmt.Sprintf(`{"amount_minor":%d,"currency":%q,"direction":%q}`, amount, currency, direction)
}

func linesJSON(lines ...string) string {
	return `{"lines":[` + joinStrings(lines) + `]}`
}

func joinStrings(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += ","
		}
		out += value
	}
	return out
}

func (f *fixture) seedBreaks(t *testing.T, count int, detectedAt time.Time, status string) {
	t.Helper()
	if status == "" {
		status = "open"
	}
	ctx := context.Background()
	for i := 0; i < count; i++ {
		signature := fmt.Sprintf("%064x", fixtureSeq.Add(1))
		if _, err := f.d.Pool.Exec(ctx,
			`INSERT INTO breaks
			     (signature, scope_id, match_run_id, side, ledger_txn_ids,
			      external_refs, delta_minor, currency, status, detected_at)
			 VALUES ($1, $2::uuid, $3::uuid, 'external_only', '{}'::uuid[],
			         '[{"source":"psp_a","source_record_id":"limits-break"}]'::jsonb,
			         1, 'USD', $4, $5)`,
			signature, f.scopeID, f.matchRunID, status, detectedAt); err != nil {
			t.Fatalf("seed break %d: %v", i, err)
		}
	}
}

func (f *fixture) seedMatches(t *testing.T, count int, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		seq := fixtureSeq.Add(1)
		var externalID string
		if err := f.d.Pool.QueryRow(ctx,
			`INSERT INTO external_records
			     (source, source_record_id, kind, amount_minor, currency,
			      occurred_at, value_date, batch_id, raw, raw_hash)
			 VALUES ('psp_a', $1, 'charge', 100, 'USD', $2, $3, $4::uuid, '{}'::jsonb, $5)
			 RETURNING id::text`,
			fmt.Sprintf("limits-ext-%d", seq), createdAt,
			createdAt.Format("2006-01-02"), f.batchID,
			fmt.Sprintf("limits-raw-hash-%d", seq)).Scan(&externalID); err != nil {
			t.Fatalf("seed external record %d: %v", i, err)
		}
		if _, err := f.d.Pool.Exec(ctx,
			`INSERT INTO matches
			     (scope_id, external_record_id, rule, confidence, match_run_id, status, created_at)
			 VALUES ($1::uuid, $2::uuid, 'fixture', 1.0000, $3::uuid, 'active', $4)`,
			f.scopeID, externalID, f.matchRunID, createdAt); err != nil {
			t.Fatalf("seed match %d: %v", i, err)
		}
	}
}

type switchRow struct {
	Enabled   bool
	Reason    string
	UpdatedBy string
	UpdatedAt time.Time
}

func (f *fixture) readSwitch(t *testing.T) switchRow {
	t.Helper()
	var row switchRow
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT enabled, reason, updated_by, updated_at
		   FROM system_switches WHERE name = 'auto_apply_enabled'`).
		Scan(&row.Enabled, &row.Reason, &row.UpdatedBy, &row.UpdatedAt); err != nil {
		t.Fatalf("read switch: %v", err)
	}
	return row
}

func (f *fixture) auditCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log
		  WHERE action = 'switch.set' AND entity_type = 'system_switch'
		    AND entity_id = 'auto_apply_enabled'`).Scan(&count); err != nil {
		t.Fatalf("count switch audits: %v", err)
	}
	return count
}

type auditRow struct {
	ActorType  string
	ActorID    string
	Action     string
	EntityType string
	EntityID   string
	Before     map[string]any
	After      map[string]any
}

func (f *fixture) readAudit(t *testing.T) auditRow {
	t.Helper()
	var (
		row           auditRow
		before, after []byte
	)
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT actor_type, actor_id, action, entity_type, entity_id, before, after
		   FROM audit_log
		  WHERE action = 'switch.set' AND entity_type = 'system_switch'
		    AND entity_id = 'auto_apply_enabled'
		  ORDER BY seq DESC LIMIT 1`).
		Scan(&row.ActorType, &row.ActorID, &row.Action, &row.EntityType, &row.EntityID,
			&before, &after); err != nil {
		t.Fatalf("read switch audit: %v", err)
	}
	if err := json.Unmarshal(before, &row.Before); err != nil {
		t.Fatalf("decode audit before: %v", err)
	}
	if err := json.Unmarshal(after, &row.After); err != nil {
		t.Fatalf("decode audit after: %v", err)
	}
	return row
}
