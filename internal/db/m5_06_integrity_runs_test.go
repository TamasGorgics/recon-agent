//go:build integration

// covers: FIN-52
package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const m506PreviousVersion = 20261125130000

const m506ChecksJSON = `[
  {"name":"ledger_balance","status":"passed","checked":0,"skipped":0,"problem_count":0,"truncated":false,"problems":[]},
  {"name":"value_conservation","status":"passed","checked":0,"skipped":0,"problem_count":0,"truncated":false,"problems":[]},
  {"name":"applied_proposal_trail","status":"passed","checked":0,"skipped":0,"problem_count":0,"truncated":false,"problems":[]},
  {"name":"audit_chain","status":"passed","checked":0,"skipped":0,"problem_count":0,"truncated":false,"problems":[]},
  {"name":"tieout","status":"passed","checked":0,"skipped":0,"problem_count":0,"truncated":false,"problems":[]}
]`

// TestM506IntegrityRunsMigration pins the M5-06 table shape, constraints, and
// index, and proves a second goose Up is a no-op.
func TestM506IntegrityRunsMigration(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	if version := m506GooseVersion(t, d); version != m506MigrationVersion {
		t.Fatalf("goose version = %d, want %d", version, m506MigrationVersion)
	}

	wantColumns := map[string]m4ColumnSpec{
		"id":            {"uuid", "NO", "gen_random_uuid()"},
		"run_date":      {"date", "NO", ""},
		"started_at":    {"timestamp with time zone", "NO", ""},
		"finished_at":   {"timestamp with time zone", "NO", ""},
		"status":        {"text", "NO", ""},
		"check_count":   {"integer", "NO", ""},
		"failure_count": {"integer", "NO", ""},
		"checks":        {"jsonb", "NO", ""},
		"triggered_by":  {"text", "NO", ""},
	}
	columns := m4TableColumns(t, d, "integrity_runs")
	if len(columns) != len(wantColumns) {
		t.Fatalf("integrity_runs has %d columns, want %d: %v",
			len(columns), len(wantColumns), m4ColumnNames(columns))
	}
	for name, want := range wantColumns {
		got, ok := columns[name]
		if !ok {
			t.Fatalf("integrity_runs.%s is missing; got %v", name, m4ColumnNames(columns))
		}
		if got != want {
			t.Errorf("integrity_runs.%s = %+v, want %+v", name, got, want)
		}
	}

	constraints := m4TableConstraints(t, d, "integrity_runs")
	if len(constraints) != 8 {
		t.Fatalf("integrity_runs has %d constraints, want 8: %v", len(constraints), constraints)
	}
	m4RequireConstraint(t, constraints, "integrity_runs_pkey", "p", "PRIMARY KEY (id)")
	m4RequireConstraint(t, constraints, "integrity_runs_status_check", "c",
		"'passed'", "'failed'")
	m4RequireConstraint(t, constraints, "integrity_runs_check_count_check", "c",
		"check_count > 0")
	m4RequireConstraint(t, constraints, "integrity_runs_check", "c",
		"failure_count >= 0", "failure_count <= check_count")
	m4RequireConstraint(t, constraints, "integrity_runs_check1", "c",
		"jsonb_typeof(checks)", "jsonb_array_length(checks)", "check_count")
	m4RequireConstraint(t, constraints, "integrity_runs_triggered_by_check", "c",
		"'schedule'", "'manual'")
	m4RequireConstraint(t, constraints, "integrity_runs_check2", "c",
		"finished_at >= started_at")
	m4RequireConstraint(t, constraints, "integrity_runs_check3", "c",
		"status = 'passed'", "failure_count = 0", "status = 'failed'", "failure_count > 0")

	var indexDef string
	if err := d.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND tablename = 'integrity_runs'
		    AND indexname = 'integrity_runs_run_date_started_idx'`).Scan(&indexDef); err != nil {
		t.Fatalf("read integrity_runs index: %v", err)
	}
	if !strings.Contains(indexDef, "(run_date, started_at DESC)") {
		t.Fatalf("index definition = %q, want (run_date, started_at DESC)", indexDef)
	}

	// There is deliberately no UNIQUE(run_date): manual reruns and scheduler
	// restarts append a new completed attempt for the same date.
	var uniqueOnDate int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		  WHERE conrelid = to_regclass('public.integrity_runs')
		    AND contype = 'u'`).Scan(&uniqueOnDate); err != nil {
		t.Fatalf("count unique constraints: %v", err)
	}
	if uniqueOnDate != 0 {
		t.Fatalf("integrity_runs has %d unique constraints, want 0", uniqueOnDate)
	}

	// Insert two completed rows for the same date through the maintenance role:
	// the append-only history contract allows this.
	first := m506InsertRun(t, d, "2026-11-26", "passed", 0)
	second := m506InsertRun(t, d, "2026-11-26", "failed", 1)
	if first == second {
		t.Fatal("two same-date inserts returned the same id")
	}

	// A second Up at the current version is a no-op.
	sqlDB := m506OpenGoose(t, d)
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("second goose up: %v", err)
	}
	if version := m506GooseVersion(t, d); version != m506MigrationVersion {
		t.Fatalf("version after second up = %d, want %d", version, m506MigrationVersion)
	}
	var applied int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM goose_db_version WHERE version_id = $1 AND is_applied`,
		m506MigrationVersion).Scan(&applied); err != nil {
		t.Fatalf("count M5-06 goose rows: %v", err)
	}
	if applied != 1 {
		t.Fatalf("M5-06 applied goose rows = %d, want 1", applied)
	}
}

// TestM506IntegrityRunsConstraints drives the checks: status, counts, array
// length, the status/failure_count pairing, and finished_at >= started_at.
func TestM506IntegrityRunsConstraints(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	for _, test := range []struct {
		name       string
		statement  string
		args       []any
		constraint string
		alsoOK     []string
	}{
		{
			name: "unknown status",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now(), 'running', 5, 0, $1::jsonb, 'schedule')`,
			args:       []any{m506ChecksJSON},
			constraint: "integrity_runs_status_check",
			// The status/failure_count pairing check also references status;
			// PostgreSQL may report either violated check first.
			alsoOK: []string{"integrity_runs_check3"},
		},
		{
			name: "zero check count",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now(), 'passed', 0, 0, '[]'::jsonb, 'schedule')`,
			constraint: "integrity_runs_check_count_check",
		},
		{
			name: "failure count exceeds check count",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now(), 'failed', 5, 6, $1::jsonb, 'schedule')`,
			args:       []any{m506ChecksJSON},
			constraint: "integrity_runs_check",
		},
		{
			name: "checks is not an array",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now(), 'passed', 1, 0, '{}'::jsonb, 'schedule')`,
			constraint: "integrity_runs_check1",
		},
		{
			name: "checks length differs from check_count",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now(), 'passed', 5, 0, '[{}]'::jsonb, 'schedule')`,
			constraint: "integrity_runs_check1",
		},
		{
			name: "unknown trigger",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now(), 'passed', 5, 0, $1::jsonb, 'worker')`,
			args:       []any{m506ChecksJSON},
			constraint: "integrity_runs_triggered_by_check",
		},
		{
			name: "finished before started",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now() - interval '1 second', 'passed', 5, 0, $1::jsonb, 'schedule')`,
			args:       []any{m506ChecksJSON},
			constraint: "integrity_runs_check2",
		},
		{
			name: "passed with failures",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now(), 'passed', 5, 1, $1::jsonb, 'schedule')`,
			args:       []any{m506ChecksJSON},
			constraint: "integrity_runs_check3",
		},
		{
			name: "failed without failures",
			statement: `INSERT INTO integrity_runs
			    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
			    VALUES ('2026-11-26', now(), now(), 'failed', 5, 0, $1::jsonb, 'schedule')`,
			args:       []any{m506ChecksJSON},
			constraint: "integrity_runs_check3",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := d.Pool.Exec(ctx, test.statement, test.args...)
			if err == nil {
				t.Fatal("invalid row insert succeeded")
			}
			pgErr := requirePgError(t, err, sqlStateCheckViolation)
			allowed := pgErr.ConstraintName == test.constraint
			for _, name := range test.alsoOK {
				if pgErr.ConstraintName == name {
					allowed = true
				}
			}
			if !allowed {
				t.Fatalf("constraint = %q, want %q (also allowed: %v)",
					pgErr.ConstraintName, test.constraint, test.alsoOK)
			}
		})
	}
}

// TestM506IntegrityRunsGrants proves the append-only privilege matrix: recon_app
// may SELECT/INSERT, app UPDATE/DELETE are denied, PUBLIC and every other role
// have nothing, and the real recon_app login exercises the granted statements.
func TestM506IntegrityRunsGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	grants := m506TableGrants(t, d)
	if len(grants) != 2 {
		t.Fatalf("integrity_runs grants = %v, want exactly recon_app INSERT/SELECT", grants)
	}
	if got := m506TablePrivileges(t, d, db.RoleApp); got != "INSERT,SELECT" {
		t.Fatalf("recon_app privileges = %q, want INSERT,SELECT", got)
	}
	for _, privilege := range []string{"UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
		if m506RolePrivilege(t, d, db.RoleApp, privilege) {
			t.Errorf("recon_app has %s on integrity_runs, want denied", privilege)
		}
	}
	for _, role := range []string{db.RoleExecutor, db.RoleAgentRO, db.RoleAuditRO} {
		if got := m506TablePrivileges(t, d, role); got != "" {
			t.Errorf("%s privileges = %q, want none", role, got)
		}
	}
	var dashboardExists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		testdb.DashboardsRole).Scan(&dashboardExists); err != nil {
		t.Fatalf("check dashboard role: %v", err)
	}
	if dashboardExists && m506RolePrivilege(t, d, testdb.DashboardsRole, "SELECT") {
		t.Errorf("dashboard role has SELECT on integrity_runs, want denied until M5-07")
	}

	var publicGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.table_privileges
		  WHERE table_schema = 'public' AND table_name = 'integrity_runs'
		    AND grantee = 'PUBLIC'`).Scan(&publicGrants); err != nil {
		t.Fatalf("read PUBLIC grants: %v", err)
	}
	if publicGrants != 0 {
		t.Fatalf("PUBLIC holds %d integrity_runs grants, want 0", publicGrants)
	}
	var publicColumnGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_column_grants
		  WHERE table_schema = 'public' AND table_name = 'integrity_runs'
		    AND grantee = 'PUBLIC'`).Scan(&publicColumnGrants); err != nil {
		t.Fatalf("read PUBLIC column grants: %v", err)
	}
	if publicColumnGrants != 0 {
		t.Fatalf("PUBLIC holds %d integrity_runs column grants, want 0", publicColumnGrants)
	}

	app := openRolePool(t, d, db.RoleApp)
	var insertedID string
	if err := app.QueryRow(ctx,
		`INSERT INTO integrity_runs
		    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
		 VALUES ('2026-11-26', now(), now(), 'passed', 5, 0, $1::jsonb, 'manual')
		 RETURNING id::text`, m506ChecksJSON).Scan(&insertedID); err != nil {
		t.Fatalf("app insert integrity_runs: %v", err)
	}
	var status string
	if err := app.QueryRow(ctx,
		`SELECT status FROM integrity_runs WHERE id = $1::uuid`, insertedID).Scan(&status); err != nil {
		t.Fatalf("app select integrity_runs: %v", err)
	}
	if status != "passed" {
		t.Fatalf("app-read status = %q, want passed", status)
	}
	if _, err := app.Exec(ctx,
		`UPDATE integrity_runs SET status = 'failed' WHERE id = $1::uuid`, insertedID); err == nil {
		t.Fatal("app UPDATE integrity_runs succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := app.Exec(ctx,
		`DELETE FROM integrity_runs WHERE id = $1::uuid`, insertedID); err == nil {
		t.Fatal("app DELETE integrity_runs succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
}

// TestM506IntegrityRunsDown applies the real goose Down and re-Up on a
// disposable clone: the table and its grants disappear and are restored.
func TestM506IntegrityRunsDown(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	sqlDB := m506OpenGoose(t, d)

	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("apply M5-06 Down: %v", err)
	}
	if version := m506GooseVersion(t, d); version != m506PreviousVersion {
		t.Fatalf("version after Down = %d, want %d", version, m506PreviousVersion)
	}
	var exists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT to_regclass('public.integrity_runs') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("resolve integrity_runs after Down: %v", err)
	}
	if exists {
		t.Fatal("integrity_runs still exists after Down")
	}
	var grants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND table_name = 'integrity_runs'`).Scan(&grants); err != nil {
		t.Fatalf("read grants after Down: %v", err)
	}
	if grants != 0 {
		t.Fatalf("integrity_runs grants after Down = %d, want 0", grants)
	}

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("re-apply M5-06 Up: %v", err)
	}
	if version := m506GooseVersion(t, d); version != m506MigrationVersion {
		t.Fatalf("version after re-Up = %d, want %d", version, m506MigrationVersion)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT to_regclass('public.integrity_runs') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("resolve integrity_runs after re-Up: %v", err)
	}
	if !exists {
		t.Fatal("integrity_runs was not recreated by Up")
	}
	if got := m506TablePrivileges(t, d, db.RoleApp); got != "INSERT,SELECT" {
		t.Fatalf("recon_app privileges after re-Up = %q, want INSERT,SELECT", got)
	}
}

func m506InsertRun(t *testing.T, d *testdb.DB, runDate, status string, failureCount int) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO integrity_runs
		    (run_date, started_at, finished_at, status, check_count, failure_count, checks, triggered_by)
		 VALUES ($1::date, now(), now(), $2, 5, $3, $4::jsonb, 'schedule')
		 RETURNING id::text`, runDate, status, failureCount, m506ChecksJSON).Scan(&id); err != nil {
		t.Fatalf("insert integrity run: %v", err)
	}
	return id
}

func m506OpenGoose(t *testing.T, d *testdb.DB) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("pgx", d.DSN)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping scratch database: %v", err)
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("configure goose: %v", err)
	}
	return sqlDB
}

func m506GooseVersion(t *testing.T, d *testdb.DB) int64 {
	t.Helper()
	var version int64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	return version
}

// m506TableGrants returns every direct table grant on integrity_runs except
// the migration owner's own implicit privileges, as "grantee:PRIVILEGE".
func m506TableGrants(t *testing.T, d *testdb.DB) []string {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT grantee, privilege_type
		   FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND table_name = 'integrity_runs'
		    AND grantee <> current_user
		  ORDER BY grantee, privilege_type`)
	if err != nil {
		t.Fatalf("read integrity_runs grants: %v", err)
	}
	defer rows.Close()
	var grants []string
	for rows.Next() {
		var grantee, privilege string
		if err := rows.Scan(&grantee, &privilege); err != nil {
			t.Fatalf("scan integrity_runs grant: %v", err)
		}
		grants = append(grants, grantee+":"+privilege)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate integrity_runs grants: %v", err)
	}
	return grants
}

// m506TablePrivileges returns the granted table privileges for role on
// integrity_runs in a stable order, excluding column privileges.
func m506TablePrivileges(t *testing.T, d *testdb.DB, role string) string {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT privilege_type
		   FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND table_name = 'integrity_runs'
		    AND grantee = $1
		  ORDER BY privilege_type`, role)
	if err != nil {
		t.Fatalf("read %s integrity_runs grants: %v", role, err)
	}
	defer rows.Close()
	var privileges []string
	for rows.Next() {
		var privilege string
		if err := rows.Scan(&privilege); err != nil {
			t.Fatalf("scan %s grant: %v", role, err)
		}
		privileges = append(privileges, privilege)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s grants: %v", role, err)
	}
	return strings.Join(privileges, ",")
}

// m506RolePrivilege answers has_table_privilege for integrity_runs, so
// inherited grants are caught as well as direct grants.
func m506RolePrivilege(t *testing.T, d *testdb.DB, role, privilege string) bool {
	t.Helper()
	var allowed bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1::name, 'public.integrity_runs', $2::text)`,
		role, privilege).Scan(&allowed); err != nil {
		t.Fatalf("read %s %s on integrity_runs: %v", role, privilege, err)
	}
	return allowed
}
