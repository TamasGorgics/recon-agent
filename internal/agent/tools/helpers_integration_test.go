//go:build integration

package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// integrationWorld is the minimum database world the M2-04 integration tests
// need: a case, an agent run, and pools for the agent and app roles.
type integrationWorld struct {
	DB     *testdb.DB
	CaseID string
	RunID  string
	Agent  *pgxpool.Pool
	App    *pgxpool.Pool
}

func newIntegrationWorld(t *testing.T) *integrationWorld {
	t.Helper()
	ctx := context.Background()
	d := testdb.New(t)

	var caseID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, created_at)
		 VALUES (0, 100, 'USD', now())
		 RETURNING id::text`).Scan(&caseID); err != nil {
		t.Fatalf("create case: %v", err)
	}

	app := openToolRolePool(t, d, db.RoleApp)
	agent := openToolRolePool(t, d, db.RoleAgentRO)

	var runID string
	if err := app.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		t.Fatalf("create agent run: %v", err)
	}

	return &integrationWorld{DB: d, CaseID: caseID, RunID: runID, Agent: agent, App: app}
}

func openToolRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	rp, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(rp.Close)
	return rp.Pool
}

func requirePgErrorCode(t *testing.T, err error, code string) *pgconn.PgError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected Postgres error %s, got nil", code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a *pgconn.PgError", err)
	}
	if pgErr.Code != code {
		t.Fatalf("SQLSTATE = %s, want %s (error: %v)", pgErr.Code, code, err)
	}
	return pgErr
}

// integrationInfoTool proves the handler's queryer is the agent role.
type integrationInfoTool struct{}

func (integrationInfoTool) Name() string                 { return "integration_info" }
func (integrationInfoTool) Description() string          { return "report the tool-session role" }
func (integrationInfoTool) InputSchema() json.RawMessage { return json.RawMessage(emptySchema) }

func (integrationInfoTool) Call(ctx context.Context, run *tools.RunContext, _ json.RawMessage) (tools.Result, error) {
	var user, superuser string
	var member bool
	if err := run.Agent.QueryRow(ctx,
		`SELECT current_user, current_setting('is_superuser'), pg_has_role(current_user, 'recon_agent_ro', 'MEMBER')`).
		Scan(&user, &superuser, &member); err != nil {
		return tools.Result{}, err
	}

	var auditDenied bool
	var ignored int
	err := run.Agent.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&ignored)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			auditDenied = true
		} else {
			return tools.Result{}, err
		}
	}
	return tools.Result{Data: map[string]any{
		"user":         user,
		"superuser":    superuser,
		"member":       member,
		"audit_denied": auditDenied,
	}}, nil
}
