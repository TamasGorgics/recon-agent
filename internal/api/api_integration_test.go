//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain keeps the migration template current before any API integration
// test clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "api: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "api: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type apiFixture struct {
	db       *testdb.DB
	app      *db.RolePool
	api      *API
	switches *SwitchWriter
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	database := testdb.New(t)
	ctx := context.Background()
	app, err := db.OpenRole(ctx, db.RoleApp, database.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(app.Close)
	store := NewPostgresStore(app.Pool)
	switchWriter := NewSwitchWriter(app.Pool, clock.NewFake(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)))
	a, err := New(Options{
		Config:     config.DefaultAPIConfig(),
		Keys:       testKeys(),
		Logger:     telemetry.NewLogger(io.Discard, slog.LevelError),
		Clock:      clock.NewFake(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)),
		Ingest:     &fakeIngest{},
		Matcher:    &fakeMatcher{},
		Cases:      &fakeCases{},
		CaseReader: store,
		CaseFile:   store,
		Proposals:  store,
		Switches:   switchWriter,
	})
	if err != nil {
		t.Fatalf("build api: %v", err)
	}
	return &apiFixture{db: database, app: app, api: a, switches: switchWriter}
}

type seededCase struct {
	caseID     string
	breakID    string
	proposalID string
	runID      string
}

// seedCases inserts one full case and one empty, newer case directly through
// the maintenance pool. Read routes use the recon_app pool.
func seedCases(t *testing.T, f *apiFixture) (full seededCase, emptyID string) {
	t.Helper()
	ctx := context.Background()
	var accountID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency)
		 VALUES ($1, 'API test account', 'asset', 'USD') RETURNING id::text`,
		"ACC-API-"+t.Name()).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	var scopeID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD') RETURNING id::text`, accountID).Scan(&scopeID); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	detected := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	var runID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'config-hash') RETURNING id::text`,
		scopeID, detected).Scan(&runID); err != nil {
		t.Fatalf("seed match run: %v", err)
	}
	createdFull := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	var breakID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO breaks (signature, scope_id, match_run_id, side, ledger_txn_ids,
		                     external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, 'external_only', '{}'::uuid[],
		         '[{"source":"psp_a","source_record_id":"rec-api-1"}]'::jsonb,
		         1234, 'USD', 'in_case', $4) RETURNING id::text`,
		strings.Repeat("a", 64), scopeID, runID, createdFull).Scan(&breakID); err != nil {
		t.Fatalf("seed break: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES (2, 1234, 'USD', 'open', 1, 0, $1) RETURNING id::text`,
		createdFull).Scan(&full.caseID); err != nil {
		t.Fatalf("seed full case: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`, full.caseID, breakID, createdFull); err != nil {
		t.Fatalf("seed case break: %v", err)
	}
	full.breakID = breakID
	runStarted := createdFull.Add(-time.Minute)
	runEnded := createdFull
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at, ended_at,
		                         tokens_in, tokens_out, cost_usd, trace_id, outcome)
		 VALUES ($1::uuid, 'deepseek-v4.1-flash', 'v1', $2, $3, 1200, 300, 0.000420, 'trace-api-1', 'proposal')
		 RETURNING id::text`, full.caseID, runStarted, runEnded).Scan(&full.runID); err != nil {
		t.Fatalf("seed agent run: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO proposals (case_id, agent_run_id, type, payload, rationale, evidence,
		                        confidence, pattern_claimed, pattern_verified, risk_tier,
		                        status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'adjustment', '{}'::jsonb, 'fee variance adjustment',
		         '["ledger_txn:1"]'::jsonb, 0.87, 'fee_variance', 'fee_variance', 'T1',
		         'pending_approval', 'state-hash', 'idem-api-1', $3)
		 RETURNING id::text`, full.caseID, full.runID, createdFull).Scan(&full.proposalID); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	createdEmpty := createdFull.Add(time.Hour)
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES (1, 100, 'USD', 'open', 0, 0, $1) RETURNING id::text`, createdEmpty).Scan(&emptyID); err != nil {
		t.Fatalf("seed empty case: %v", err)
	}
	return full, emptyID
}

func (f *apiFixture) get(t *testing.T, target string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testViewerKey)
	recorder := newTestRecorder()
	f.api.Handler().ServeHTTP(recorder, request)
	return recorder.Result()
}

func TestAPICasesListAndDetailIntegration(t *testing.T) {
	f := newAPIFixture(t)
	full, emptyID := seedCases(t, f)

	response := f.get(t, RouteCases+"?limit=1")
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", response.StatusCode)
	}
	var first CaseList
	if err := json.NewDecoder(response.Body).Decode(&first); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(first.Items) != 1 || first.Items[0].ID != emptyID {
		t.Fatalf("first page = %+v, want the newer empty case", first.Items)
	}
	if first.NextCursor == "" {
		t.Fatal("next_cursor is empty with a second page remaining")
	}
	if first.Items[0].BreakIDs == nil {
		t.Fatal("break_ids must be [] not null")
	}

	response = f.get(t, RouteCases+"?limit=1&cursor="+first.NextCursor)
	defer func() { _ = response.Body.Close() }()
	var second CaseList
	if err := json.NewDecoder(response.Body).Decode(&second); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].ID != full.caseID {
		t.Fatalf("second page = %+v, want the full case", second.Items)
	}
	if len(second.Items[0].BreakIDs) != 1 || second.Items[0].BreakIDs[0] != full.breakID {
		t.Fatalf("break_ids = %v", second.Items[0].BreakIDs)
	}

	response = f.get(t, "/v1/cases/"+full.caseID)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d", response.StatusCode)
	}
	var document CaseFile
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if document.SchemaVersion != 1 || document.Case.ID != full.caseID {
		t.Fatalf("document = %+v", document)
	}
	if len(document.Breaks) != 1 || document.Breaks[0].ID != full.breakID {
		t.Fatalf("breaks = %+v", document.Breaks)
	}
	if len(document.Proposals) != 1 || document.Proposals[0].ID != full.proposalID {
		t.Fatalf("proposals = %+v", document.Proposals)
	}
	if document.Proposals[0].Rationale != "fee variance adjustment" {
		t.Fatalf("rationale = %q", document.Proposals[0].Rationale)
	}
	if len(document.AgentRuns) != 1 || document.AgentRuns[0].CostUSD != "0.000420" {
		t.Fatalf("agent runs = %+v", document.AgentRuns)
	}
	if document.AgentRuns[0].Outcome == nil || *document.AgentRuns[0].Outcome != "proposal" {
		t.Fatalf("outcome = %v", document.AgentRuns[0].Outcome)
	}

	response = f.get(t, "/v1/cases/"+emptyID)
	defer func() { _ = response.Body.Close() }()
	var empty CaseFile
	if err := json.NewDecoder(response.Body).Decode(&empty); err != nil {
		t.Fatalf("decode empty detail: %v", err)
	}
	if empty.Breaks == nil || empty.Proposals == nil || empty.AgentRuns == nil {
		t.Fatalf("empty sections must be [] not null: %+v", empty)
	}
}

// covers: FIN-30
func TestAPISwitchAuditIntegration(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()

	var current bool
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT enabled FROM system_switches WHERE name = 'agent_enabled'`).Scan(&current); err != nil {
		t.Fatalf("read seeded switch: %v", err)
	}
	want := !current
	body := fmt.Sprintf(`{"name":"agent_enabled","enabled":%t,"reason":"integrated operator pause"}`, want)
	request, err := http.NewRequest(http.MethodPost, RouteSwitches, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testAdminKey)
	request.Header.Set("Content-Type", "application/json")
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	request.Header.Set("Traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	recorder := newTestRecorder()
	f.api.Handler().ServeHTTP(recorder, request)
	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("switch status = %d", response.StatusCode)
	}
	var result SwitchResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode switch: %v", err)
	}
	if !result.Changed || result.Enabled != want || result.UpdatedBy != "human:dev-admin" {
		t.Fatalf("result = %+v", result)
	}

	var (
		enabled   bool
		updatedBy string
		updatedAt time.Time
	)
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT enabled, updated_by, updated_at FROM system_switches WHERE name = 'agent_enabled'`).
		Scan(&enabled, &updatedBy, &updatedAt); err != nil {
		t.Fatalf("read updated switch: %v", err)
	}
	if enabled != want || updatedBy != "human:dev-admin" {
		t.Fatalf("row = enabled:%v updated_by:%q", enabled, updatedBy)
	}

	countAudit := func() int {
		t.Helper()
		var count int
		if err := f.db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_id = $2`,
			ActionSwitchSet, SwitchAgent).Scan(&count); err != nil {
			t.Fatalf("count audit: %v", err)
		}
		return count
	}
	if got := countAudit(); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
	var (
		actorType string
		actorID   string
		trace     *string
	)
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT actor_type, actor_id, trace_id FROM audit_log
		  WHERE action = $1 AND entity_id = $2`, ActionSwitchSet, SwitchAgent).
		Scan(&actorType, &actorID, &trace); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if actorType != "human" || actorID != "dev-admin" {
		t.Fatalf("audit actor = %s:%s, want human:dev-admin", actorType, actorID)
	}
	if trace == nil || *trace != traceID {
		t.Fatalf("audit trace_id = %v, want %q", trace, traceID)
	}

	// Same value again: changed=false, no second audit row, timestamp unchanged.
	request, err = http.NewRequest(http.MethodPost, RouteSwitches, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new second request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testAdminKey)
	request.Header.Set("Content-Type", "application/json")
	recorder = newTestRecorder()
	f.api.Handler().ServeHTTP(recorder, request)
	response = recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("no-op status = %d", response.StatusCode)
	}
	var noOp SwitchResult
	if err := json.NewDecoder(response.Body).Decode(&noOp); err != nil {
		t.Fatalf("decode no-op: %v", err)
	}
	if noOp.Changed {
		t.Fatal("no-op reported changed=true")
	}
	if got := countAudit(); got != 1 {
		t.Fatalf("audit rows after no-op = %d, want 1", got)
	}
	var updatedAtAfter time.Time
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT updated_at FROM system_switches WHERE name = 'agent_enabled'`).Scan(&updatedAtAfter); err != nil {
		t.Fatalf("read no-op timestamp: %v", err)
	}
	if !updatedAtAfter.Equal(updatedAt) {
		t.Fatalf("updated_at changed on no-op: %v -> %v", updatedAt, updatedAtAfter)
	}
}

// TestAPIRoleMatrixIntegrationSmoke proves the real recon_app pool can serve
// the read routes the role matrix exposes.
func TestAPIRoleMatrixIntegrationSmoke(t *testing.T) {
	f := newAPIFixture(t)
	seedCases(t, f)
	for _, target := range []string{RouteCases, RouteProposals} {
		response := f.get(t, target)
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("%s status = %d", target, response.StatusCode)
		}
		response.Body.Close()
	}
}
