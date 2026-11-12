//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

var seedCounter atomic.Int64

type fixture struct {
	d       *testdb.DB
	app     *pgxpool.Pool
	service *state.Service
	clock   *clock.Fake
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)
	fake := clock.NewFake(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
	service, err := state.NewService(app, fake)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &fixture{d: d, app: app, service: service, clock: fake}
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

type seedValues struct {
	status     state.Status
	version    int
	stateHash  string
	payload    string
	rationale  string
	expiresAt  *time.Time
	supersedes *string
}

type seedOption func(*seedValues)

func withExpiry(at time.Time) seedOption {
	return func(v *seedValues) { v.expiresAt = &at }
}

func withVersion(version int) seedOption {
	return func(v *seedValues) { v.version = version }
}

func withSupersedes(id string) seedOption {
	return func(v *seedValues) { v.supersedes = &id }
}

// seed creates one case, agent run, and proposal. One case per proposal keeps
// the partial one-active-proposal-per-case index satisfied.
func (f *fixture) seed(t *testing.T, status state.Status, opts ...seedOption) string {
	t.Helper()
	ctx := context.Background()
	values := seedValues{
		status:    status,
		stateHash: "state-hash-keep",
		payload:   `{"marker":"keep"}`,
		rationale: "keep rationale",
	}
	for _, opt := range opts {
		opt(&values)
	}

	var caseID string
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, created_at)
		 VALUES (0, 100, 'USD', now())
		 RETURNING id::text`).Scan(&caseID); err != nil {
		t.Fatalf("seed case: %v", err)
	}
	var runID string
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		t.Fatalf("seed agent run: %v", err)
	}
	key := fmt.Sprintf("m4-03-seed-%d", seedCounter.Add(1))
	var id string
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, version, state_hash, idempotency_key,
		      supersedes_proposal_id, created_at, expires_at)
		 VALUES ($1::uuid, $2::uuid, 'link', $3::jsonb, $4, 0.9,
		         $5, $6, $7, $8, $9::uuid, now(), $10)
		 RETURNING id::text`,
		caseID, runID, values.payload, values.rationale, string(values.status),
		values.version, values.stateHash, key, values.supersedes, values.expiresAt).
		Scan(&id); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return id
}

type storedProposal struct {
	Status     state.Status
	Version    int
	StateHash  string
	Payload    map[string]any
	Rationale  string
	ExpiresAt  *time.Time
	Supersedes *string
}

func (f *fixture) get(t *testing.T, id string) storedProposal {
	t.Helper()
	var (
		stored  storedProposal
		status  string
		payload []byte
	)
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT status, version, state_hash, payload, rationale,
		        expires_at, supersedes_proposal_id::text
		   FROM proposals WHERE id = $1::uuid`, id).
		Scan(&status, &stored.Version, &stored.StateHash, &payload,
			&stored.Rationale, &stored.ExpiresAt, &stored.Supersedes); err != nil {
		t.Fatalf("read proposal %s: %v", id, err)
	}
	stored.Status = state.Status(status)
	if err := json.Unmarshal(payload, &stored.Payload); err != nil {
		t.Fatalf("decode proposal payload: %v", err)
	}
	return stored
}

type auditRow struct {
	Action     string
	EntityType string
	EntityID   string
	ActorType  string
	ActorID    string
	Timestamp  time.Time
	Before     []byte
	After      []byte
	TraceID    *string
}

func (f *fixture) audits(t *testing.T, entityID string) []auditRow {
	t.Helper()
	rows, err := f.d.Pool.Query(context.Background(),
		`SELECT action, entity_type, entity_id, actor_type, actor_id, ts,
		        before, after, trace_id
		   FROM audit_log
		  WHERE entity_id = $1
		  ORDER BY seq`, entityID)
	if err != nil {
		t.Fatalf("query audit rows for %s: %v", entityID, err)
	}
	defer rows.Close()

	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.Action, &row.EntityType, &row.EntityID,
			&row.ActorType, &row.ActorID, &row.Timestamp, &row.Before, &row.After,
			&row.TraceID); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	return out
}

func (f *fixture) auditCount(t *testing.T, entityID string) int {
	t.Helper()
	return len(f.audits(t, entityID))
}

func (f *fixture) transition(req state.TransitionRequest) (state.Proposal, error) {
	var got state.Proposal
	err := db.WithTx(context.Background(), f.app, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = f.service.Transition(ctx, tx, req)
			return err
		})
	return got, err
}

func systemActor(t *testing.T, id string) audit.Actor {
	t.Helper()
	actor, err := audit.SystemActor(id)
	if err != nil {
		t.Fatalf("SystemActor(%q): %v", id, err)
	}
	return actor
}

func humanActor(t *testing.T, id string) audit.Actor {
	t.Helper()
	actor, err := audit.HumanActor(id)
	if err != nil {
		t.Fatalf("HumanActor(%q): %v", id, err)
	}
	return actor
}

func agentActor(t *testing.T, id string) audit.Actor {
	t.Helper()
	actor, err := audit.AgentActor(id)
	if err != nil {
		t.Fatalf("AgentActor(%q): %v", id, err)
	}
	return actor
}

func legalReason(edge specEdge) string {
	if !edge.reasonRequired {
		return ""
	}
	if len(edge.reasonEnum) > 0 {
		return edge.reasonEnum[0]
	}
	return "required reason"
}

func jsonObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("expected a JSON object, got NULL")
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode JSON object %s: %v", raw, err)
	}
	return object
}

func objectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
