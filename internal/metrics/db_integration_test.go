//go:build integration

// covers: FIN-01, FIN-22

package metrics

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any test clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "metrics: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "metrics: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestDBGaugesAgainstDatabase seeds exact queue and outbox states in an
// isolated database, then gathers the real collector through the recon_app
// role. It proves the predicates exclude leased/resolved cases, delivered and
// dead outbox rows, that known topics without pending rows publish zero, and
// that arbitrary database topics fold into the bounded unknown series.
func TestDBGaugesAgainstDatabase(t *testing.T) {
	ctx := context.Background()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 4})

	var dbNow time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatalf("read database clock: %v", err)
	}

	// Cases: two open+unleased are the only ones counted.
	openOne := insertCase(t, d, "open", "", nil, nil)
	openTwo := insertCase(t, d, "open", "", nil, nil)
	insertCase(t, d, "in_agent", "worker-seed", timePointer(dbNow.Add(time.Hour)), nil)
	insertCase(t, d, "resolved", "", nil, timePointer(dbNow.Add(-time.Minute)))

	// Proposals: two pending_approval are counted; approved/proposed are not.
	insertProposal(t, d, openOne, "pending_approval")
	insertProposal(t, d, openTwo, "pending_approval")
	approvedCase := insertCase(t, d, "proposal_pending", "", nil, nil)
	insertProposal(t, d, approvedCase, "approved")
	proposedCase := insertCase(t, d, "proposal_pending", "", nil, nil)
	insertProposal(t, d, proposedCase, "proposed")

	// Outbox: four pending rows (two known topics, two unlisted topics), plus
	// one delivered and one dead row that must be excluded.
	insertOutbox(t, d, "approval.request", dbNow.Add(-90*time.Second), nil, false)
	insertOutbox(t, d, "approval.update", dbNow.Add(-45*time.Second), nil, false)
	insertOutbox(t, d, "proposal.applied", dbNow.Add(-300*time.Second), nil, false)
	insertOutbox(t, d, "report.post", dbNow.Add(-120*time.Second), nil, false)
	insertOutbox(t, d, "approval.request", dbNow.Add(-500*time.Second),
		timePointer(dbNow.Add(-400*time.Second)), false)
	insertOutbox(t, d, "approval.update", dbNow.Add(-600*time.Second), nil, true)

	app, err := db.OpenRole(ctx, db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(app.Close)

	m := mustNew(t, Options{
		Component:     "api",
		Queryer:       app.Pool,
		GatherTimeout: 2 * time.Second,
		Logger:        testCollectorLogger(),
	})

	views := gatherViews(t, m.Registry())
	queues := queueDepthValues(t, views)
	wantQueues := map[string]float64{
		QueueCasesOpen:        2,
		QueueApprovalsPending: 2,
		QueueOutboxPending:    4,
	}
	for queue, want := range wantQueues {
		if queues[queue] != want {
			t.Errorf("queue_depth{%s} = %v, want %v", queue, queues[queue], want)
		}
	}

	lags := outboxLagValues(t, views)
	if len(lags) != 4 {
		t.Fatalf("outbox lag series = %v, want exactly four catalogued topics", lags)
	}
	assertLagNear(t, lags, "approval.request", 90)
	assertLagNear(t, lags, "approval.update", 45)
	assertLagNear(t, lags, "approval.cancel", 0)
	assertLagNear(t, lags, "unknown", 300) // max(proposal.applied, report.post)
	if got := collectErrorCount(t, views); got != 0 {
		t.Errorf("errors{metrics,collect} = %v, want 0", got)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("registry contract after a live gather: %v", err)
	}

	// Validate gathers again (a newer database clock), so take this gather as
	// the last-value baseline before breaking the pool.
	baseline := gatherViews(t, m.Registry())
	baselineQueues := queueDepthValues(t, baseline)
	baselineLags := outboxLagValues(t, baseline)

	// A closed pool is a query failure: gauges keep their last values and the
	// bounded collect counter increments once per failed query.
	app.Close()
	after := gatherViews(t, m.Registry())
	afterQueues := queueDepthValues(t, after)
	for queue, want := range baselineQueues {
		if afterQueues[queue] != want {
			t.Errorf("queue_depth{%s} after query error = %v, want last value %v", queue, afterQueues[queue], want)
		}
	}
	afterLags := outboxLagValues(t, after)
	for topic, want := range baselineLags {
		if afterLags[topic] != want {
			t.Errorf("outbox_lag{%s} after query error = %v, want last value %v", topic, afterLags[topic], want)
		}
	}
	if got, want := collectErrorCount(t, after), 4.0; got != want {
		t.Errorf("errors{metrics,collect} after a closed pool = %v, want %v", got, want)
	}
}

func assertLagNear(t *testing.T, lags map[string]float64, topic string, want float64) {
	t.Helper()
	got, ok := lags[topic]
	if !ok {
		t.Fatalf("outbox_lag{%s} is missing from %v", topic, lags)
	}
	if got < want-5 || got > want+5 {
		t.Errorf("outbox_lag{%s} = %v, want about %v", topic, got, want)
	}
}

func insertCase(t *testing.T, d *testdb.DB, status, leasedBy string, leaseExpires, resolvedAt *time.Time) string {
	t.Helper()
	var id string
	err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO cases
		     (priority, value_minor, currency, status, created_at,
		      leased_by, lease_expires_at, resolved_at)
		 VALUES (1, 100, 'USD', $1, now() - interval '1 hour', $2, $3, $4)
		 RETURNING id::text`,
		status, nullable(leasedBy), leaseExpires, resolvedAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert %s case: %v", status, err)
	}
	return id
}

func insertProposal(t *testing.T, d *testdb.DB, caseID, status string) {
	t.Helper()
	var agentRunID string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'seed-model', 'seed-v1', now() - interval '1 hour')
		 RETURNING id::text`, caseID).Scan(&agentRunID); err != nil {
		t.Fatalf("insert agent run: %v", err)
	}
	key := fmt.Sprintf("metrics-seed-%s-%s", caseID, status)
	_, err := d.Pool.Exec(context.Background(),
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{"seeded":true}'::jsonb,
		         'metrics seed', 0.9000, $3, 'seed-state-hash', $4,
		         now() - interval '30 minutes')`,
		caseID, agentRunID, status, key)
	if err != nil {
		t.Fatalf("insert %s proposal: %v", status, err)
	}
}

func insertOutbox(t *testing.T, d *testdb.DB, topic string, createdAt time.Time, deliveredAt *time.Time, dead bool) {
	t.Helper()
	key := fmt.Sprintf("metrics-outbox-%s-%d-%t", topic, createdAt.UnixNano(), dead)
	_, err := d.Pool.Exec(context.Background(),
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at, delivered_at, dead)
		 VALUES ('proposal', 'seed', $1, '{"seeded":true}'::jsonb, $2,
		         $3, $3, $4, $5)`,
		topic, key, createdAt, deliveredAt, dead)
	if err != nil {
		t.Fatalf("insert %s outbox row: %v", topic, err)
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func timePointer(value time.Time) *time.Time { return &value }
