//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const testNow = "2026-11-18T12:00:00Z"

var fixtureSeq atomic.Int64

type fixture struct {
	db      *testdb.DB
	app     *pgxpool.Pool
	service *lifecycle.Service
	clock   *clock.Fake
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 20})
	ctx := context.Background()
	app, err := db.OpenRole(ctx, testdb.RoleApp, d.DSNFor(testdb.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(app.Close)

	clk := clock.NewFake(mustTime(t, testNow))
	service, err := lifecycle.NewService(lifecycle.Options{DB: app.Pool, Clock: clk})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &fixture{db: d, app: app.Pool, service: service, clock: clk}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

// seedCase inserts one case in status. A resolved case gets resolved_at and an
// optional resolution_type so the FIN-24 schema checks hold.
func (f *fixture) seedCase(t *testing.T, status, resolutionType string) string {
	t.Helper()
	var (
		id  string
		err error
	)
	if status == "resolved" {
		var resolution any
		if resolutionType != "" {
			resolution = resolutionType
		}
		err = f.db.Pool.QueryRow(context.Background(),
			`INSERT INTO cases
			     (priority, value_minor, currency, status, version, created_at,
			      resolved_at, resolution_type)
			 VALUES (0, 100, 'USD', 'resolved', 0, $1, $1, $2)
			 RETURNING id::text`,
			f.clock.Now(), resolution).Scan(&id)
	} else {
		err = f.db.Pool.QueryRow(context.Background(),
			`INSERT INTO cases
			     (priority, value_minor, currency, status, version, created_at)
			 VALUES (0, 100, 'USD', $1, 0, $2)
			 RETURNING id::text`,
			status, f.clock.Now()).Scan(&id)
	}
	if err != nil {
		t.Fatalf("seed case status %s: %v", status, err)
	}
	return id
}

// seedProposal inserts one agent run and proposal for caseID. One run per
// proposal keeps proposals_one_proposal_per_run satisfied; the caller is
// responsible for the one-active-per-case index.
func (f *fixture) seedProposal(t *testing.T, caseID, status string) string {
	t.Helper()
	ctx := context.Background()
	var runID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', $2)
		 RETURNING id::text`, caseID, f.clock.Now()).Scan(&runID); err != nil {
		t.Fatalf("seed agent run: %v", err)
	}
	n := fixtureSeq.Add(1)
	var id string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, version, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{"marker":"keep"}'::jsonb,
		         'keep rationale', 0.9, $3, 0, $4, $5, $6)
		 RETURNING id::text`,
		caseID, runID, status, fmt.Sprintf("state-hash-%d", n),
		fmt.Sprintf("lifecycle-seed-%d", n), f.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("seed proposal status %s: %v", status, err)
	}
	return id
}

type supersedeCall struct {
	caseID        string
	newProposalID string
	actor         string
	reason        string
}

func (f *fixture) supersede(ctx context.Context, call supersedeCall) (lifecycle.SupersedeResult, error) {
	var result lifecycle.SupersedeResult
	err := db.WithTx(ctx, f.app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = f.service.SupersedeActiveProposal(ctx, tx, call.caseID,
			call.newProposalID, call.actor, call.reason)
		return err
	})
	return result, err
}

type storedProposal struct {
	Status  string
	Version int
}

func (f *fixture) proposal(t *testing.T, id string) storedProposal {
	t.Helper()
	var stored storedProposal
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status, version FROM proposals WHERE id = $1::uuid`, id).
		Scan(&stored.Status, &stored.Version); err != nil {
		t.Fatalf("read proposal %s: %v", id, err)
	}
	return stored
}

func (f *fixture) activeProposalCount(t *testing.T, caseID string) int {
	t.Helper()
	return f.count(t,
		`SELECT count(*) FROM proposals
		  WHERE case_id = $1::uuid
		    AND status IN ('proposed', 'pending_approval', 'approved', 'auto_approved')`,
		caseID)
}

type storedCase struct {
	Status           string
	Version          int
	Attempts         int
	EscalationReason *string
	ResolutionType   *string
	ResolvedAt       *time.Time
}

func (f *fixture) caseRow(t *testing.T, id string) storedCase {
	t.Helper()
	var stored storedCase
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status, version, attempts, escalation_reason, resolution_type, resolved_at
		   FROM cases WHERE id = $1::uuid`, id).
		Scan(&stored.Status, &stored.Version, &stored.Attempts,
			&stored.EscalationReason, &stored.ResolutionType,
			&stored.ResolvedAt); err != nil {
		t.Fatalf("read case %s: %v", id, err)
	}
	return stored
}

// equalStoredCase compares case rows by value. The pointer fields must not be
// compared by address.
func equalStoredCase(a, b storedCase) bool {
	return a.Status == b.Status && a.Version == b.Version &&
		a.Attempts == b.Attempts &&
		equalStringPtr(a.EscalationReason, b.EscalationReason) &&
		equalStringPtr(a.ResolutionType, b.ResolutionType) &&
		equalTimePtr(a.ResolvedAt, b.ResolvedAt)
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

type outboxRow struct {
	AggregateType  string
	AggregateID    string
	Topic          string
	IdempotencyKey string
	Payload        []byte
	DeliveredAt    *time.Time
}

func (f *fixture) outbox(t *testing.T) []outboxRow {
	t.Helper()
	rows, err := f.db.Pool.Query(context.Background(),
		`SELECT aggregate_type, aggregate_id, topic, idempotency_key,
		        payload::text, delivered_at
		   FROM outbox ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.AggregateType, &row.AggregateID, &row.Topic,
			&row.IdempotencyKey, &row.Payload, &row.DeliveredAt); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

type auditRow struct {
	Action     string
	EntityType string
	EntityID   string
	ActorType  string
	ActorID    string
	Before     []byte
	After      []byte
	TraceID    *string
}

func (f *fixture) audits(t *testing.T, entityID string) []auditRow {
	t.Helper()
	rows, err := f.db.Pool.Query(context.Background(),
		`SELECT action, entity_type, entity_id, actor_type, actor_id,
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
			&row.ActorType, &row.ActorID, &row.Before, &row.After,
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

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
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

func assertKeys(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	if len(object) != len(want) {
		t.Fatalf("JSON keys = %v, want exactly %v", object, want)
	}
	for _, key := range want {
		if _, ok := object[key]; !ok {
			t.Fatalf("JSON keys = %v, missing %q", object, key)
		}
	}
}
