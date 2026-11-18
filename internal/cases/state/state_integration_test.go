//go:build integration

// covers: FIN-24, FIN-30, FIN-33, FIN-43
package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any test clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "cases/state: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "cases/state: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

var (
	seedBase = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	testNow  = time.Date(2026, 11, 18, 12, 0, 0, 123456000, time.UTC)
)

// specEdge is the integration test's independent copy of the 18 legal edges
// from spec 4.2, with one valid actor/reason/resolution per edge.
type specEdge struct {
	from       state.Status
	to         state.Status
	actor      string
	reason     string
	resolution string
}

var specEdges = []specEdge{
	{state.StatusOpen, state.StatusInAgent, "system:worker", "", ""},
	{state.StatusOpen, state.StatusEscalated, "system:case-lifecycle", "attempts_exhausted", ""},
	{state.StatusOpen, state.StatusResolved, "human:alice", "manual resolution", "manual"},

	{state.StatusInAgent, state.StatusOpen, "system:worker", "", ""},
	{state.StatusInAgent, state.StatusProposalPending, "system:agent-loop", "", ""},
	{state.StatusInAgent, state.StatusWaitingInfo, "system:agent-loop", "need a statement", ""},
	{state.StatusInAgent, state.StatusEscalated, "system:worker", "manual", ""},
	{state.StatusInAgent, state.StatusResolved, "system:matcher", "late match", "superseded_by_match"},

	{state.StatusProposalPending, state.StatusOpen, "system:case-lifecycle", "proposal_rejected", ""},
	{state.StatusProposalPending, state.StatusWaitingInfo, "system:agent-loop", "", ""},
	{state.StatusProposalPending, state.StatusEscalated, "system:case-lifecycle", "missing_settlement_sla", ""},
	{state.StatusProposalPending, state.StatusResolved, "human:alice", "manual resolution", "manual"},

	{state.StatusWaitingInfo, state.StatusOpen, "human:alice", "human_reply", ""},
	{state.StatusWaitingInfo, state.StatusEscalated, "system:case-lifecycle", "attempts_exhausted", ""},
	{state.StatusWaitingInfo, state.StatusResolved, "human:alice", "manual resolution", "manual"},

	{state.StatusEscalated, state.StatusOpen, "human:alice", "reopen after review", ""},
	{state.StatusEscalated, state.StatusResolved, "system:matcher", "late match", "superseded_by_match"},

	{state.StatusResolved, state.StatusOpen, "human:alice", "unmatch", ""},
}

type fixture struct {
	d   *testdb.DB
	app *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testdb.New(t)
	app, err := db.NewPool(context.Background(), d.DSNFor(testdb.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(app.Close)
	return &fixture{d: d, app: app}
}

type seedValues struct {
	status       state.Status
	version      int
	attempts     int
	escalation   *string
	resolution   *string
	resolvedAt   *time.Time
	leaseOwner   *string
	leaseExpires *time.Time
}

func defaultSeed(status state.Status, version int) seedValues {
	values := seedValues{status: status, version: version, attempts: 2}
	switch status {
	case state.StatusInAgent:
		owner := "worker-1"
		expires := seedBase.Add(time.Hour)
		values.leaseOwner = &owner
		values.leaseExpires = &expires
	case state.StatusEscalated:
		reason := state.EscalationManual
		values.escalation = &reason
	case state.StatusResolved:
		at := seedBase.Add(-time.Hour)
		resolution := "manual"
		values.resolvedAt = &at
		values.resolution = &resolution
	}
	return values
}

func (f *fixture) seed(t *testing.T, values seedValues) string {
	t.Helper()
	var id string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO cases
		     (priority, value_minor, currency, status, version, attempts,
		      escalation_reason, created_at, leased_by, lease_expires_at,
		      resolved_at, resolution_type)
		 VALUES (0, 100, 'USD', $1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id::text`,
		string(values.status), values.version, values.attempts, values.escalation,
		seedBase.Add(-24*time.Hour), values.leaseOwner, values.leaseExpires,
		values.resolvedAt, values.resolution).Scan(&id); err != nil {
		t.Fatalf("seed case: %v", err)
	}
	return id
}

func transitionRequest(caseID string, to state.Status, version int, actor, reason, resolution string) state.TransitionRequest {
	return state.TransitionRequest{
		CaseID:          caseID,
		To:              to,
		ExpectedVersion: version,
		Actor:           actor,
		Reason:          reason,
		ResolutionType:  resolution,
		TraceID:         "trace-1",
		Now:             testNow,
	}
}

func (f *fixture) transitionRaw(ctx context.Context, req state.TransitionRequest) (state.Snapshot, error) {
	var snapshot state.Snapshot
	err := db.WithTx(ctx, f.app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		snapshot, err = state.Transition(ctx, tx, req)
		return err
	})
	return snapshot, err
}

func (f *fixture) transition(t *testing.T, req state.TransitionRequest) (state.Snapshot, error) {
	t.Helper()
	snapshot, err := f.transitionRaw(context.Background(), req)
	return snapshot, err
}

func (f *fixture) get(t *testing.T, caseID string) state.Snapshot {
	t.Helper()
	var (
		snapshot state.Snapshot
		status   string
	)
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT id::text, status, version, attempts, escalation_reason,
		        resolved_at, resolution_type, leased_by, lease_expires_at
		   FROM cases WHERE id = $1::uuid`, caseID).
		Scan(&snapshot.CaseID, &status, &snapshot.Version, &snapshot.Attempts,
			&snapshot.EscalationReason, &snapshot.ResolvedAt,
			&snapshot.ResolutionType, &snapshot.LeaseOwner,
			&snapshot.LeaseExpiresAt); err != nil {
		t.Fatalf("read case %s: %v", caseID, err)
	}
	snapshot.Status = state.Status(status)
	return snapshot
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

func (f *fixture) audits(t *testing.T, caseID string) []auditRow {
	t.Helper()
	rows, err := f.d.Pool.Query(context.Background(),
		`SELECT action, entity_type, entity_id, actor_type, actor_id, ts,
		        before, after, trace_id
		   FROM audit_log
		  WHERE entity_id = $1
		  ORDER BY seq`, caseID)
	if err != nil {
		t.Fatalf("query audit rows for %s: %v", caseID, err)
	}
	defer rows.Close()

	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.Action, &row.EntityType, &row.EntityID,
			&row.ActorType, &row.ActorID, &row.Timestamp, &row.Before,
			&row.After, &row.TraceID); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	return out
}

func (f *fixture) auditCount(t *testing.T, caseID string) int {
	t.Helper()
	return len(f.audits(t, caseID))
}

func requireIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
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

// TestTransitionLegalMatrixDB walks all 18 legal edges through the real
// recon_app role and asserts the snapshot, stored row, audit count, and the
// lease/escalation/resolution field rules for each edge.
func TestTransitionLegalMatrixDB(t *testing.T) {
	f := newFixture(t)
	for i, edge := range specEdges {
		index := i
		edge := edge
		t.Run(fmt.Sprintf("%02d_%s_to_%s", i+1, edge.from, edge.to), func(t *testing.T) {
			id := f.seed(t, defaultSeed(edge.from, 1))
			snapshot, err := f.transition(t, state.TransitionRequest{
				CaseID:          id,
				To:              edge.to,
				ExpectedVersion: 1,
				Actor:           edge.actor,
				Reason:          edge.reason,
				ResolutionType:  edge.resolution,
				TraceID:         "trace-1",
				Now:             testNow,
			})
			if err != nil {
				t.Fatalf("edge %d: %v", index+1, err)
			}
			if snapshot.CaseID != id || snapshot.Status != edge.to || snapshot.Version != 2 {
				t.Fatalf("snapshot = %+v, want id %s status %s version 2",
					snapshot, id, edge.to)
			}
			if snapshot.Attempts != 2 {
				t.Fatalf("attempts = %d, want 2", snapshot.Attempts)
			}

			stored := f.get(t, id)
			if stored.Status != edge.to || stored.Version != 2 || stored.Attempts != 2 {
				t.Fatalf("stored = %s/version %d/attempts %d, want %s/2/2",
					stored.Status, stored.Version, stored.Attempts, edge.to)
			}
			if edge.to != state.StatusInAgent &&
				(stored.LeaseOwner != nil || stored.LeaseExpiresAt != nil) {
				t.Fatalf("target %s retained a lease: %+v", edge.to, stored)
			}
			if edge.to == state.StatusEscalated {
				want := strings.TrimSpace(edge.reason)
				if stored.EscalationReason == nil || *stored.EscalationReason != want {
					t.Fatalf("escalation_reason = %v, want %q", stored.EscalationReason, want)
				}
			} else if stored.EscalationReason != nil {
				t.Fatalf("target %s retained escalation_reason %q", edge.to, *stored.EscalationReason)
			}
			if edge.to == state.StatusResolved {
				want := strings.TrimSpace(edge.resolution)
				if stored.ResolutionType == nil || *stored.ResolutionType != want {
					t.Fatalf("resolution_type = %v, want %q", stored.ResolutionType, want)
				}
				if stored.ResolvedAt == nil || !stored.ResolvedAt.Equal(testNow) {
					t.Fatalf("resolved_at = %v, want %v", stored.ResolvedAt, testNow)
				}
			} else if stored.ResolutionType != nil || stored.ResolvedAt != nil {
				t.Fatalf("target %s retained resolution fields: %+v", edge.to, stored)
			}

			rows := f.audits(t, id)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %d, want exactly 1", len(rows))
			}
			row := rows[0]
			if row.Action != "case.transition" || row.EntityType != "case" ||
				row.EntityID != id {
				t.Fatalf("audit row = %+v, want case.transition/case/%s", row, id)
			}
			if got := row.ActorType + ":" + row.ActorID; got != edge.actor {
				t.Fatalf("audit actor = %q, want %q", got, edge.actor)
			}
			if row.TraceID == nil || *row.TraceID != "trace-1" {
				t.Fatalf("audit trace_id = %v, want trace-1", row.TraceID)
			}
			before := jsonObject(t, row.Before)
			after := jsonObject(t, row.After)
			if before["status"] != string(edge.from) || before["version"] != float64(1) {
				t.Fatalf("audit before = %v, want status %s version 1", before, edge.from)
			}
			if after["status"] != string(edge.to) || after["version"] != float64(2) {
				t.Fatalf("audit after = %v, want status %s version 2", after, edge.to)
			}
			if edge.to == state.StatusResolved {
				if after["resolution_type"] != strings.TrimSpace(edge.resolution) {
					t.Fatalf("audit after resolution_type = %v", after["resolution_type"])
				}
				if after["resolved_at"] == nil {
					t.Fatal("audit after resolved_at is null")
				}
			}
			reason := strings.TrimSpace(edge.reason)
			if reason == "" {
				if _, ok := after["reason"]; ok {
					t.Fatalf("audit after has reason %v, want none", after["reason"])
				}
			} else if after["reason"] != reason {
				t.Fatalf("audit after reason = %v, want %q", after["reason"], reason)
			}
		})
	}
}

// TestTransitionIllegalMatrixDB walks the 12 illegal non-self pairs and the 6
// self pairs. No update, version bump, or audit row is allowed.
func TestTransitionIllegalMatrixDB(t *testing.T) {
	f := newFixture(t)
	legal := make(map[[2]state.Status]bool, len(specEdges))
	for _, edge := range specEdges {
		legal[[2]state.Status{edge.from, edge.to}] = true
	}
	for _, from := range state.AllStatuses() {
		for _, to := range state.AllStatuses() {
			if legal[[2]state.Status{from, to}] {
				continue
			}
			from, to := from, to
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				id := f.seed(t, defaultSeed(from, 1))
				resolution := ""
				if to == state.StatusResolved {
					resolution = "manual"
				}
				_, err := f.transition(t,
					transitionRequest(id, to, 1, "human:alice", "reason", resolution))
				switch {
				case from == to:
					requireIs(t, err, state.ErrAlreadyInState)
				case from == state.StatusResolved:
					requireIs(t, err, state.ErrTerminalState)
					requireIs(t, err, state.ErrIllegalTransition)
				default:
					requireIs(t, err, state.ErrIllegalTransition)
				}
				stored := f.get(t, id)
				if stored.Status != from || stored.Version != 1 || stored.Attempts != 2 {
					t.Fatalf("illegal transition changed case: %+v", stored)
				}
				if got := f.auditCount(t, id); got != 0 {
					t.Fatalf("illegal transition wrote %d audit rows, want 0", got)
				}
			})
		}
	}
}

// TestTransitionStaleCAS pins the expected-version CAS: a mismatch is ErrStale
// before the write, and a replayed old version loses after a successful one.
func TestTransitionStaleCAS(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusOpen, 5))

	for _, expected := range []int{4, 6, 1} {
		_, err := f.transition(t, transitionRequest(id, state.StatusInAgent, expected, "system:worker", "", ""))
		requireIs(t, err, state.ErrStale)
		stored := f.get(t, id)
		if stored.Status != state.StatusOpen || stored.Version != 5 {
			t.Fatalf("stale %d changed case: %s/version %d", expected, stored.Status, stored.Version)
		}
		if got := f.auditCount(t, id); got != 0 {
			t.Fatalf("stale %d wrote %d audit rows", expected, got)
		}
	}

	snapshot, err := f.transition(t, transitionRequest(id, state.StatusInAgent, 5, "system:worker", "", ""))
	if err != nil {
		t.Fatalf("valid transition: %v", err)
	}
	if snapshot.Version != 6 || snapshot.Status != state.StatusInAgent {
		t.Fatalf("snapshot = %s/version %d, want in_agent/6", snapshot.Status, snapshot.Version)
	}
	if _, err := f.transition(t, transitionRequest(id, state.StatusProposalPending, 5, "system:agent-loop", "", "")); !errors.Is(err, state.ErrStale) {
		t.Fatalf("replay error = %v, want ErrStale", err)
	}
	if got := f.auditCount(t, id); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}

// TestTransitionRowLock proves Transition takes FOR UPDATE: a second
// transaction blocks until the holder releases the row.
func TestTransitionRowLock(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusOpen, 1))
	ctx := context.Background()

	holder, err := f.app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx,
		`SELECT id FROM cases WHERE id = $1::uuid FOR UPDATE`, id); err != nil {
		t.Fatalf("lock case: %v", err)
	}

	results := make(chan error, 1)
	go func() {
		runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := f.transitionRaw(runCtx,
			transitionRequest(id, state.StatusInAgent, 1, "system:worker", "", ""))
		results <- err
	}()

	select {
	case err := <-results:
		t.Fatalf("transition returned while the row was locked: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := holder.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("rollback holder: %v", err)
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("transition after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transition did not finish after the lock was released")
	}
	if stored := f.get(t, id); stored.Status != state.StatusInAgent || stored.Version != 2 {
		t.Fatalf("stored = %s/version %d, want in_agent/2", stored.Status, stored.Version)
	}
	if got := f.auditCount(t, id); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}

// TestTransitionConcurrentSameVersion proves the row lock and CAS admit exactly
// one of two same-version transitions and write one audit row. The two requests
// use different legal targets so the loser reports ErrStale; with an identical
// target the documented precedence (already-in-state before stale) would report
// ErrAlreadyInState instead.
func TestTransitionConcurrentSameVersion(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusOpen, 1))

	requests := []state.TransitionRequest{
		transitionRequest(id, state.StatusInAgent, 1, "system:worker", "", ""),
		transitionRequest(id, state.StatusEscalated, 1, "system:case-lifecycle", "manual", ""),
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := range requests {
		req := requests[i]
		go func() {
			<-start
			_, err := f.transitionRaw(context.Background(), req)
			results <- err
		}()
	}
	close(start)

	successes, stales := 0, 0
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, state.ErrStale):
			stales++
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if successes != 1 || stales != 1 {
		t.Fatalf("successes=%d stales=%d, want 1/1", successes, stales)
	}
	stored := f.get(t, id)
	if stored.Version != 2 {
		t.Fatalf("version = %d, want 2", stored.Version)
	}
	if stored.Status != state.StatusInAgent && stored.Status != state.StatusEscalated {
		t.Fatalf("status = %s, want in_agent or escalated", stored.Status)
	}
	if got := f.auditCount(t, id); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}

// TestTransitionAuditPayload pins the exact before/after shape, actor, trace,
// timestamp, and the no-audit-on-error/no-op rule.
func TestTransitionAuditPayload(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusInAgent, 4))

	req := state.TransitionRequest{
		CaseID:          strings.ToUpper(id),
		To:              state.StatusProposalPending,
		ExpectedVersion: 4,
		Actor:           "system:agent-loop",
		Reason:          "  proposal pending  ",
		TraceID:         "trace-42",
		Now:             testNow,
	}
	if _, err := f.transition(t, req); err != nil {
		t.Fatalf("transition: %v", err)
	}
	rows := f.audits(t, id)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.EntityID != id {
		t.Fatalf("audit entity id = %q, want canonical %q", row.EntityID, id)
	}
	if row.ActorType != "system" || row.ActorID != "agent-loop" {
		t.Fatalf("actor = %s:%s, want system:agent-loop", row.ActorType, row.ActorID)
	}
	if row.TraceID == nil || *row.TraceID != "trace-42" {
		t.Fatalf("trace_id = %v, want trace-42", row.TraceID)
	}
	if !row.Timestamp.Equal(testNow) {
		t.Fatalf("audit ts = %v, want %v", row.Timestamp, testNow)
	}
	before := jsonObject(t, row.Before)
	after := jsonObject(t, row.After)
	wantBefore := []string{"escalation_reason", "resolution_type", "resolved_at", "status", "version"}
	if got := objectKeys(before); strings.Join(got, ",") != strings.Join(wantBefore, ",") {
		t.Fatalf("before keys = %v, want %v", got, wantBefore)
	}
	wantAfter := append(append([]string{}, wantBefore...), "reason")
	sort.Strings(wantAfter)
	if got := objectKeys(after); strings.Join(got, ",") != strings.Join(wantAfter, ",") {
		t.Fatalf("after keys = %v, want %v", got, wantAfter)
	}
	if before["status"] != "in_agent" || before["version"] != float64(4) {
		t.Fatalf("before = %v, want in_agent/4", before)
	}
	if before["escalation_reason"] != nil || before["resolution_type"] != nil ||
		before["resolved_at"] != nil {
		t.Fatalf("before carried resolution/escalation fields: %v", before)
	}
	if after["status"] != "proposal_pending" || after["version"] != float64(5) {
		t.Fatalf("after = %v, want proposal_pending/5", after)
	}
	if after["escalation_reason"] != nil || after["resolution_type"] != nil ||
		after["resolved_at"] != nil {
		t.Fatalf("after carried resolution/escalation fields: %v", after)
	}
	if after["reason"] != "proposal pending" {
		t.Fatalf("after reason = %v, want proposal pending", after["reason"])
	}

	// A stale request and a self-transition must not add audit rows.
	if _, err := f.transition(t, state.TransitionRequest{
		CaseID:          id,
		To:              state.StatusOpen,
		ExpectedVersion: 4,
		Actor:           "human:alice",
		Reason:          "reopen",
		Now:             testNow,
	}); !errors.Is(err, state.ErrStale) {
		t.Fatalf("stale error = %v", err)
	}
	if _, err := f.transition(t, state.TransitionRequest{
		CaseID:          id,
		To:              state.StatusProposalPending,
		ExpectedVersion: 5,
		Actor:           "system:agent-loop",
		Now:             testNow,
	}); !errors.Is(err, state.ErrAlreadyInState) {
		t.Fatalf("self error = %v", err)
	}
	if got := f.auditCount(t, id); got != 1 {
		t.Fatalf("audit rows after errors = %d, want 1", got)
	}

	// A second success appends a second audit with resolution fields.
	resolveNow := testNow.Add(time.Second)
	if _, err := f.transition(t, state.TransitionRequest{
		CaseID:          id,
		To:              state.StatusResolved,
		ExpectedVersion: 5,
		Actor:           "human:alice",
		Reason:          "manual resolution",
		ResolutionType:  "manual",
		Now:             resolveNow,
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rows = f.audits(t, id)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	if rows[1].TraceID != nil {
		t.Fatalf("resolve trace_id = %v, want nil", rows[1].TraceID)
	}
	if rows[1].ActorType != "human" || rows[1].ActorID != "alice" {
		t.Fatalf("resolve actor = %s:%s, want human:alice", rows[1].ActorType, rows[1].ActorID)
	}
	after = jsonObject(t, rows[1].After)
	if after["resolution_type"] != "manual" {
		t.Fatalf("resolve after resolution_type = %v", after["resolution_type"])
	}
	if after["resolved_at"] == nil {
		t.Fatal("resolve after resolved_at is null")
	}
	if after["reason"] != "manual resolution" {
		t.Fatalf("resolve after reason = %v", after["reason"])
	}
}

// TestTransitionAuditFailureRollsBack revokes INSERT on audit_log from the
// application role; the update, lease change, and audit all roll back.
func TestTransitionAuditFailureRollsBack(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusInAgent, 1))
	before := f.get(t, id)

	if _, err := f.d.Pool.Exec(context.Background(),
		`REVOKE INSERT ON audit_log FROM recon_app`); err != nil {
		t.Fatalf("revoke audit insert: %v", err)
	}
	_, err := f.transition(t,
		transitionRequest(id, state.StatusProposalPending, 1, "system:agent-loop", "", ""))
	if err == nil {
		t.Fatal("transition with a revoked audit insert succeeded")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Fatalf("error = %v, want an audit failure", err)
	}
	stored := f.get(t, id)
	if stored.Status != before.Status || stored.Version != before.Version {
		t.Fatalf("audit failure changed case: %+v, want %+v", stored, before)
	}
	if (stored.LeaseOwner == nil) != (before.LeaseOwner == nil) ||
		(stored.LeaseExpiresAt == nil) != (before.LeaseExpiresAt == nil) {
		t.Fatalf("audit failure changed lease: %+v, want %+v", stored, before)
	}
	if got := f.auditCount(t, id); got != 0 {
		t.Fatalf("audit rows = %d, want 0", got)
	}
}

// TestTransitionCallerRollback proves the caller's transaction owns commit:
// returning an error after a successful Transition discards both writes.
func TestTransitionCallerRollback(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusOpen, 1))
	abort := errors.New("caller abort")

	err := db.WithTx(context.Background(), f.app, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := state.Transition(ctx, tx,
				transitionRequest(id, state.StatusInAgent, 1, "system:worker", "", "")); err != nil {
				return err
			}
			return abort
		})
	if !errors.Is(err, abort) {
		t.Fatalf("error = %v, want caller abort", err)
	}
	stored := f.get(t, id)
	if stored.Status != state.StatusOpen || stored.Version != 1 {
		t.Fatalf("caller rollback changed case: %+v", stored)
	}
	if got := f.auditCount(t, id); got != 0 {
		t.Fatalf("caller rollback left %d audit rows", got)
	}
}

// TestTransitionFieldRules covers the lease, escalation, resolution, attempts,
// and clock rules that are not exercised by a single matrix edge.
func TestTransitionFieldRules(t *testing.T) {
	t.Run("in_agent to proposal_pending clears lease", func(t *testing.T) {
		f := newFixture(t)
		id := f.seed(t, defaultSeed(state.StatusInAgent, 1))
		if _, err := f.transition(t, transitionRequest(id, state.StatusProposalPending, 1,
			"system:agent-loop", "", "")); err != nil {
			t.Fatalf("transition: %v", err)
		}
		stored := f.get(t, id)
		if stored.LeaseOwner != nil || stored.LeaseExpiresAt != nil {
			t.Fatalf("lease retained: %+v", stored)
		}
		if stored.Attempts != 2 {
			t.Fatalf("attempts = %d, want 2", stored.Attempts)
		}
	})

	t.Run("in_agent to escalated clears lease and sets reason", func(t *testing.T) {
		f := newFixture(t)
		id := f.seed(t, defaultSeed(state.StatusInAgent, 1))
		if _, err := f.transition(t, transitionRequest(id, state.StatusEscalated, 1,
			"system:worker", "attempts_exhausted", "")); err != nil {
			t.Fatalf("transition: %v", err)
		}
		stored := f.get(t, id)
		if stored.LeaseOwner != nil || stored.LeaseExpiresAt != nil {
			t.Fatalf("lease retained: %+v", stored)
		}
		if stored.EscalationReason == nil || *stored.EscalationReason != "attempts_exhausted" {
			t.Fatalf("escalation_reason = %v", stored.EscalationReason)
		}
	})

	t.Run("escalated to resolved clears escalation and sets resolution", func(t *testing.T) {
		f := newFixture(t)
		id := f.seed(t, defaultSeed(state.StatusEscalated, 1))
		if _, err := f.transition(t, transitionRequest(id, state.StatusResolved, 1,
			"system:matcher", "late match", "superseded_by_match")); err != nil {
			t.Fatalf("transition: %v", err)
		}
		stored := f.get(t, id)
		if stored.EscalationReason != nil {
			t.Fatalf("escalation_reason = %q, want nil", *stored.EscalationReason)
		}
		if stored.ResolutionType == nil || *stored.ResolutionType != "superseded_by_match" {
			t.Fatalf("resolution_type = %v", stored.ResolutionType)
		}
		if stored.ResolvedAt == nil || !stored.ResolvedAt.Equal(testNow) {
			t.Fatalf("resolved_at = %v, want %v", stored.ResolvedAt, testNow)
		}
	})

	t.Run("escalated to open clears escalation", func(t *testing.T) {
		f := newFixture(t)
		id := f.seed(t, defaultSeed(state.StatusEscalated, 1))
		if _, err := f.transition(t, transitionRequest(id, state.StatusOpen, 1,
			"human:alice", "reviewed", "")); err != nil {
			t.Fatalf("transition: %v", err)
		}
		stored := f.get(t, id)
		if stored.EscalationReason != nil {
			t.Fatalf("escalation_reason = %q, want nil", *stored.EscalationReason)
		}
	})

	t.Run("resolved to open clears resolution", func(t *testing.T) {
		f := newFixture(t)
		id := f.seed(t, defaultSeed(state.StatusResolved, 1))
		if _, err := f.transition(t, transitionRequest(id, state.StatusOpen, 1,
			"human:alice", "reopen", "")); err != nil {
			t.Fatalf("transition: %v", err)
		}
		stored := f.get(t, id)
		if stored.EscalationReason != nil || stored.ResolutionType != nil || stored.ResolvedAt != nil {
			t.Fatalf("resolved -> open retained fields: %+v", stored)
		}
		if stored.Attempts != 2 {
			t.Fatalf("attempts = %d, want 2", stored.Attempts)
		}
	})

	t.Run("resolved sets fresh resolved_at and overwrites stale fields", func(t *testing.T) {
		f := newFixture(t)
		values := defaultSeed(state.StatusOpen, 1)
		oldAt := seedBase.Add(-48 * time.Hour)
		oldResolution := "manual"
		values.resolvedAt = &oldAt
		values.resolution = &oldResolution
		id := f.seed(t, values)
		if _, err := f.transition(t, transitionRequest(id, state.StatusResolved, 1,
			"human:alice", "manual resolution", "manual")); err != nil {
			t.Fatalf("transition: %v", err)
		}
		stored := f.get(t, id)
		if stored.ResolvedAt == nil || !stored.ResolvedAt.Equal(testNow) {
			t.Fatalf("resolved_at = %v, want %v", stored.ResolvedAt, testNow)
		}
	})

	t.Run("target in_agent retains lease", func(t *testing.T) {
		f := newFixture(t)
		f.dropCaseChecks(t, func(definition string) bool {
			return strings.Contains(definition, "leased_by")
		})
		values := defaultSeed(state.StatusOpen, 1)
		owner := "pre-leased"
		expires := seedBase.Add(2 * time.Hour)
		values.leaseOwner = &owner
		values.leaseExpires = &expires
		id := f.seed(t, values)

		snapshot, err := f.transition(t, transitionRequest(id, state.StatusInAgent, 1,
			"system:worker", "", ""))
		if err != nil {
			t.Fatalf("transition: %v", err)
		}
		if snapshot.LeaseOwner == nil || *snapshot.LeaseOwner != owner {
			t.Fatalf("snapshot lease_owner = %v, want %q", snapshot.LeaseOwner, owner)
		}
		stored := f.get(t, id)
		if stored.LeaseOwner == nil || *stored.LeaseOwner != owner {
			t.Fatalf("lease_owner = %v, want %q", stored.LeaseOwner, owner)
		}
		if stored.LeaseExpiresAt == nil || !stored.LeaseExpiresAt.Equal(expires) {
			t.Fatalf("lease_expires_at = %v, want %v", stored.LeaseExpiresAt, expires)
		}
	})

	t.Run("zero Now falls back to the wall clock", func(t *testing.T) {
		f := newFixture(t)
		id := f.seed(t, defaultSeed(state.StatusOpen, 1))
		snapshot, err := f.transition(t, state.TransitionRequest{
			CaseID:          id,
			To:              state.StatusInAgent,
			ExpectedVersion: 1,
			Actor:           "system:worker",
		})
		if err != nil {
			t.Fatalf("transition: %v", err)
		}
		if snapshot.UpdatedAt.IsZero() {
			t.Fatal("UpdatedAt is zero with a zero Now")
		}
		rows := f.audits(t, id)
		if len(rows) != 1 {
			t.Fatalf("audit rows = %d, want 1", len(rows))
		}
		if !rows[0].Timestamp.Equal(snapshot.UpdatedAt) {
			t.Fatalf("audit ts = %v, want %v", rows[0].Timestamp, snapshot.UpdatedAt)
		}
	})
}

// TestTransitionInvalidStoredStatus drops the status CHECK in this isolated
// database, stores an out-of-set value, and proves the authority fails closed
// without a write or audit row.
func TestTransitionInvalidStoredStatus(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusOpen, 1))
	f.dropCaseChecks(t, func(definition string) bool {
		return strings.Contains(definition, "proposal_pending")
	})
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE cases SET status = 'agent_failed' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("store invalid status: %v", err)
	}
	_, err := f.transition(t, transitionRequest(id, state.StatusOpen, 1,
		"human:alice", "reopen", ""))
	requireIs(t, err, state.ErrInvalidStatus)
	if got := f.auditCount(t, id); got != 0 {
		t.Fatalf("invalid stored status wrote %d audit rows", got)
	}
}

// TestTransitionCaseNotFound proves a valid request for an absent case is the
// typed not-found error and writes nothing.
func TestTransitionCaseNotFound(t *testing.T) {
	f := newFixture(t)
	_, err := f.transition(t, transitionRequest("99999999-9999-4999-8999-999999999999",
		state.StatusInAgent, 1, "system:worker", "", ""))
	requireIs(t, err, state.ErrCaseNotFound)
}

// TestTransitionAcceptsVersionZero pins the wave 1b-i correction: CreateCase
// inserts version 0, so a freshly created case must be able to take its first
// transition without a synthetic bump. Only negative versions are invalid.
func TestTransitionAcceptsVersionZero(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, defaultSeed(state.StatusOpen, 0))
	snapshot, err := f.transition(t, transitionRequest(id, state.StatusInAgent, 0,
		"system:worker", "", ""))
	if err != nil {
		t.Fatalf("version-0 transition: %v", err)
	}
	if snapshot.Status != state.StatusInAgent || snapshot.Version != 1 {
		t.Fatalf("snapshot = %s/version %d, want in_agent/1",
			snapshot.Status, snapshot.Version)
	}
	stored := f.get(t, id)
	if stored.Status != state.StatusInAgent || stored.Version != 1 {
		t.Fatalf("stored = %s/version %d, want in_agent/1", stored.Status, stored.Version)
	}
	rows := f.audits(t, id)
	if len(rows) != 1 || rows[0].Action != state.ActionCaseTransition {
		t.Fatalf("audits = %+v, want exactly one case.transition", rows)
	}
}

// dropCaseChecks drops the CHECK constraints on cases whose definition matches
// predicate. It is used only to exercise defensive branches (invalid stored
// status) and an otherwise unreachable lease combination; each fixture has an
// isolated database.
func (f *fixture) dropCaseChecks(t *testing.T, predicate func(string) bool) {
	t.Helper()
	rows, err := f.d.Pool.Query(context.Background(),
		`SELECT conname, pg_get_constraintdef(oid)
		   FROM pg_constraint
		  WHERE conrelid = 'cases'::regclass AND contype = 'c'`)
	if err != nil {
		t.Fatalf("list case checks: %v", err)
	}
	var names []string
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			rows.Close()
			t.Fatalf("scan case check: %v", err)
		}
		if predicate(definition) {
			names = append(names, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read case checks: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no case CHECK matched the predicate")
	}
	for _, name := range names {
		if _, err := f.d.Pool.Exec(context.Background(),
			`ALTER TABLE cases DROP CONSTRAINT `+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Fatalf("drop case check %s: %v", name, err)
		}
	}
}
