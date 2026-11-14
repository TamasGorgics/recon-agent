//go:build integration

// covers: FIN-20, FIN-23

package executor_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const m406MigrationVersion = 20261114120000

// m406ExecutorViews is exactly the ResolveState read surface this migration
// grants to recon_executor.
var m406ExecutorViews = []string{
	"v_agent_cases",
	"v_agent_case_breaks",
	"v_agent_breaks",
	"v_agent_ledger_txns",
	"v_agent_ledger_entries",
	"v_agent_external_records",
	"v_agent_settlement_batches",
	"v_agent_fx_rates",
	"v_agent_fee_schedules",
}

// m406OtherAgentViews stay agent-only; the executor must not read them.
var m406OtherAgentViews = []string{
	"v_agent_accounts",
	"v_agent_scopes",
	"v_agent_statement_balances",
	"v_agent_history",
}

func m406TablePrivilege(t *testing.T, d *testdb.DB, role, table, privilege string) bool {
	t.Helper()
	var allowed bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1::name, $2::text, $3::text)`,
		role, "public."+table, privilege).Scan(&allowed); err != nil {
		t.Fatalf("read %s %s on %s: %v", role, privilege, table, err)
	}
	return allowed
}

func m406ColumnPrivilege(t *testing.T, d *testdb.DB, role, table, column, privilege string) bool {
	t.Helper()
	var allowed bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_column_privilege($1::name, $2::text, $3::text, $4::text)`,
		role, "public."+table, column, privilege).Scan(&allowed); err != nil {
		t.Fatalf("read %s %s on %s.%s: %v", role, privilege, table, column, err)
	}
	return allowed
}

func m406RequirePrivilegeDenied(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("error = %v, want SQLSTATE 42501 permission denied", err)
	}
}

// TestExecutorReadViewGrant proves the additive migration through the real
// recon_executor login: the nine ResolveState views are readable, the four
// other agent views stay closed, and the Down section revokes exactly the nine.
func TestExecutorReadViewGrant(t *testing.T) {
	ctx := context.Background()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 4})
	for _, view := range m406ExecutorViews {
		if !m406TablePrivilege(t, d, db.RoleExecutor, view, "SELECT") {
			t.Errorf("executor lacks SELECT on %s", view)
		}
	}
	for _, view := range m406OtherAgentViews {
		if m406TablePrivilege(t, d, db.RoleExecutor, view, "SELECT") {
			t.Errorf("executor gained SELECT on %s, want agent-only", view)
		}
	}

	var count int
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(is_applied), false)
		   FROM goose_db_version WHERE version_id = $1`, m406MigrationVersion).
		Scan(&count, &applied); err != nil {
		t.Fatalf("read M4-06 goose version: %v", err)
	}
	if count == 0 || !applied {
		t.Fatalf("M4-06 migration rows/applied = %d/%t, want applied", count, applied)
	}

	migration, err := fs.ReadFile(migrations.FS, "20261114120000_m4_06_executor_read_views.sql")
	if err != nil {
		t.Fatalf("read M4-06 migration: %v", err)
	}
	for _, view := range m406ExecutorViews {
		if !strings.Contains(string(migration), view) {
			t.Errorf("M4-06 migration missing %s", view)
		}
	}
	for _, want := range []string{"GRANT SELECT ON", "TO recon_executor;", "REVOKE SELECT ON", "FROM recon_executor;"} {
		if !strings.Contains(string(migration), want) {
			t.Errorf("M4-06 migration missing %q", want)
		}
	}
}

// TestExecutorApplyGrant proves the apply write matrix through the real
// recon_executor login: apply tables are writable, proposal columns other than
// status/version stay unwritable, and app/agent/audit surfaces did not widen.
func TestExecutorApplyGrant(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})

	for _, tc := range []struct{ table, privilege string }{
		{"ledger_txns", "INSERT"},
		{"ledger_entries", "INSERT"},
		{"applied_proposals", "INSERT"},
		{"outbox", "INSERT"},
		{"audit_log", "INSERT"},
		{"audit_chain_head", "INSERT"},
		{"breaks", "UPDATE"},
		{"cases", "UPDATE"},
		{"proposals", "SELECT"},
	} {
		if !m406TablePrivilege(t, w.db, db.RoleExecutor, tc.table, tc.privilege) {
			t.Errorf("executor lacks %s on %s", tc.privilege, tc.table)
		}
	}
	if m406TablePrivilege(t, w.db, db.RoleExecutor, "proposals", "UPDATE") {
		t.Error("executor has table-level UPDATE on proposals, want column-level only")
	}
	for _, column := range []string{"status", "version"} {
		if !m406ColumnPrivilege(t, w.db, db.RoleExecutor, "proposals", column, "UPDATE") {
			t.Errorf("executor lacks UPDATE on proposals.%s", column)
		}
	}
	for _, column := range []string{"payload", "rationale", "evidence", "state_hash", "expires_at"} {
		if m406ColumnPrivilege(t, w.db, db.RoleExecutor, "proposals", column, "UPDATE") {
			t.Errorf("executor has UPDATE on proposals.%s, want denied", column)
		}
	}
	if m406TablePrivilege(t, w.db, db.RoleExecutor, "outbox", "SELECT") {
		t.Error("executor has table-level SELECT on outbox, want column-level only")
	}
	if m406TablePrivilege(t, w.db, db.RoleExecutor, "outbox", "UPDATE") {
		t.Error("executor has UPDATE on outbox, want INSERT only")
	}
	if m406TablePrivilege(t, w.db, db.RoleExecutor, "applied_proposals", "DELETE") {
		t.Error("executor has DELETE on applied_proposals")
	}

	// The real executor session can read each ResolveState view and use the
	// outbox conflict arbiter without RETURNING.
	for _, view := range m406ExecutorViews {
		var n int
		if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(&n); err != nil {
			t.Errorf("executor SELECT %s: %v", view, err)
		}
	}
	if err := w.pool.QueryRow(ctx, `SELECT id FROM outbox LIMIT 1`).Scan(new(string)); err == nil {
		t.Error("executor table-level SELECT on outbox succeeded, want permission denied")
	} else {
		m406RequirePrivilegeDenied(t, err)
	}
	key := "m4-06-grant-outbox-" + fixture.proposalID
	for i := 0; i < 2; i++ {
		if _, err := w.pool.Exec(ctx,
			`INSERT INTO outbox
			     (id, aggregate_type, aggregate_id, topic, payload, idempotency_key,
			      created_at, next_attempt_at)
			 VALUES (gen_random_uuid(), 'proposal', 'grant', 'proposal.applied',
			         '{}'::jsonb, $1, now(), now())
			 ON CONFLICT (idempotency_key) DO NOTHING`, key); err != nil {
			t.Fatalf("executor outbox insert %d: %v", i, err)
		}
	}
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO outbox
		     (id, aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES (gen_random_uuid(), 'proposal', 'grant', 'proposal.applied',
		         '{}'::jsonb, 'm4-06-grant-returning', now(), now())
		 RETURNING id`); err == nil {
		t.Error("executor INSERT ... RETURNING on outbox succeeded, want permission denied")
	} else {
		m406RequirePrivilegeDenied(t, err)
	}

	for _, column := range []string{"payload", "rationale", "evidence", "state_hash", "expires_at"} {
		if _, err := w.pool.Exec(ctx,
			`UPDATE proposals SET `+column+` = `+proposalColumnValue(column)+` WHERE id = $1::uuid`,
			fixture.proposalID); err == nil {
			t.Errorf("executor updated proposals.%s, want permission denied", column)
		} else {
			m406RequirePrivilegeDenied(t, err)
		}
	}
	if _, err := w.pool.Exec(ctx,
		`UPDATE proposals SET status = 'pending_approval', version = version + 1
		  WHERE id = $1::uuid`, fixture.proposalID); err != nil {
		t.Fatalf("executor status/version update: %v", err)
	}
	if _, err := w.pool.Exec(ctx,
		`DELETE FROM proposals WHERE id = $1::uuid`, fixture.proposalID); err == nil {
		t.Error("executor DELETE on proposals succeeded, want permission denied")
	} else {
		m406RequirePrivilegeDenied(t, err)
	}

	// app, agent, and audit surfaces stay exactly as their own migrations left
	// them: no executor view grant leaked a write privilege.
	if !m406TablePrivilege(t, w.db, db.RoleApp, "proposals", "SELECT") {
		t.Error("app lost SELECT on proposals")
	}
	for _, column := range []string{"status", "version", "verification", "pattern_verified", "risk_tier", "policy_decision"} {
		if !m406ColumnPrivilege(t, w.db, db.RoleApp, "proposals", column, "UPDATE") {
			t.Errorf("app lost UPDATE on proposals.%s", column)
		}
	}
	for _, column := range []string{"payload", "rationale", "evidence", "state_hash", "expires_at"} {
		if m406ColumnPrivilege(t, w.db, db.RoleApp, "proposals", column, "UPDATE") {
			t.Errorf("app gained UPDATE on proposals.%s", column)
		}
	}
	if !m406TablePrivilege(t, w.db, db.RoleAgentRO, "proposals", "INSERT") {
		t.Error("agent lost INSERT on proposals")
	}
	if m406TablePrivilege(t, w.db, db.RoleAgentRO, "proposals", "SELECT") {
		t.Error("agent gained table-level SELECT on proposals")
	}
	if !m406TablePrivilege(t, w.db, db.RoleAuditRO, "audit_log", "SELECT") {
		t.Error("audit lost SELECT on audit_log")
	}
	if m406TablePrivilege(t, w.db, db.RoleAuditRO, "audit_log", "INSERT") {
		t.Error("audit gained INSERT on audit_log")
	}
	if m406TablePrivilege(t, w.db, db.RoleExecutor, "proposals", "DELETE") {
		t.Error("executor has DELETE on proposals")
	}
}

// proposalColumnValue returns a SQL literal for the denied column updates.
func proposalColumnValue(column string) string {
	switch column {
	case "payload", "evidence":
		return "'[]'::jsonb"
	case "expires_at":
		return "now()"
	default:
		return "'tampered'"
	}
}

// TestExecutorReadGrantMigrationDown exercises the migration Down/Up pair on a
// clone: Down revokes exactly the nine reads and Up restores them.
func TestExecutorReadGrantMigrationDown(t *testing.T) {
	ctx := context.Background()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 4})

	sqlDB, err := sql.Open("pgx", d.DSN)
	if err != nil {
		t.Fatalf("open goose sql database: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("configure goose dialect: %v", err)
	}
	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("migrate M4-06 down: %v", err)
	}
	for _, view := range m406ExecutorViews {
		if m406TablePrivilege(t, d, db.RoleExecutor, view, "SELECT") {
			t.Errorf("executor still has SELECT on %s after Down", view)
		}
	}
	if !m406TablePrivilege(t, d, db.RoleExecutor, "ledger_txns", "INSERT") {
		t.Error("Down revoked an unrelated executor base-table grant")
	}
	if !m406TablePrivilege(t, d, db.RoleAgentRO, "v_agent_cases", "SELECT") {
		t.Error("Down revoked recon_agent_ro's M2 view grant")
	}
	if !m406TablePrivilege(t, d, db.RoleApp, "v_agent_fx_rates", "SELECT") {
		t.Error("Down revoked recon_app's M4-05 verifier view grant")
	}

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("migrate M4-06 up: %v", err)
	}
	for _, view := range m406ExecutorViews {
		if !m406TablePrivilege(t, d, db.RoleExecutor, view, "SELECT") {
			t.Errorf("executor lacks SELECT on %s after Up", view)
		}
	}
}
