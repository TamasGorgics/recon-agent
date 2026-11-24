//go:build integration

// covers: FIN-42
package dashboards_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const (
	m504MigrationVersion = 20261124120000
	m504PreviousVersion  = 20261123120000

	sqlStateInsufficientPrivilege = "42501"
	sqlStateReadOnlyTransaction   = "25006"
)

var dashboardViews = []string{
	"v_ops_match_rate",
	"v_ops_unmatched_value",
	"v_ops_break_aging",
	"v_ops_approval_queue",
	"v_ops_resolution_mix",
}

// TestMain prepares the migrated template and fails if this process leaks an
// ephemeral login role. Dashboard logins are lazily provisioned only by tests
// that ask for them, so the same process-scoped orphan assertion used by the
// db package applies here too.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "dashboards: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "dashboards: ensure template: %v\n", err)
		os.Exit(1)
	}
	if err := requireDashboardRoleExists(ctx, adminDSN); err != nil {
		fmt.Fprintf(os.Stderr, "dashboards: %v\n", err)
		os.Exit(1)
	}

	prefix := testdb.LoginRolePrefix()
	before, err := listLoginRoles(ctx, adminDSN, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboards: list login roles before run: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	after, err := listLoginRoles(ctx, adminDSN, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboards: list login roles after run: %v\n", err)
		os.Exit(1)
	}
	orphans := newRoleNames(before, after)
	if len(orphans) > 0 {
		fmt.Fprintf(os.Stderr, "dashboards: orphan ephemeral login roles after run: %s\n", strings.Join(orphans, ", "))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func requireDashboardRoleExists(ctx context.Context, adminDSN string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect admin database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		testdb.DashboardsRole).Scan(&exists); err != nil {
		return fmt.Errorf("read %s role: %w", testdb.DashboardsRole, err)
	}
	if !exists {
		return fmt.Errorf("role %s is missing; the test template must be migrated", testdb.DashboardsRole)
	}
	return nil
}

func listLoginRoles(ctx context.Context, adminDSN, prefix string) ([]string, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	rows, err := conn.Query(ctx,
		`SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1) ORDER BY rolname`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func newRoleNames(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var added []string
	for _, name := range after {
		if _, ok := seen[name]; ok {
			continue
		}
		added = append(added, name)
	}
	return added
}

func requirePgError(t *testing.T, err error, codes ...string) *pgconn.PgError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a Postgres error (%s), got nil", strings.Join(codes, ", "))
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a *pgconn.PgError", err)
	}
	for _, code := range codes {
		if pgErr.Code == code {
			return pgErr
		}
	}
	t.Fatalf("SQLSTATE = %s, want one of %v (error: %v)", pgErr.Code, codes, err)
	return nil
}

func openDashboardConn(t *testing.T, d *testdb.DB) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	dsn, err := d.DashboardsDSN(ctx)
	if err != nil {
		t.Fatalf("provision dashboard login: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect as dashboard login: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	var currentUser string
	if err := conn.QueryRow(ctx, `SELECT current_user`).Scan(&currentUser); err != nil {
		t.Fatalf("read dashboard current_user: %v", err)
	}
	if !strings.HasPrefix(currentUser, testdb.LoginRolePrefix()) {
		t.Fatalf("dashboard session user = %q, want an ephemeral %q login", currentUser, testdb.LoginRolePrefix())
	}
	return conn
}

// TestM504RoleAndViews is T3: the migration exists at the latest version, the
// role is NOLOGIN and view-only, the five v_ops_* views are SELECTable through
// the dashboard login, every base table and v_agent_* view is denied with
// 42501, writes are denied, and the session is read-only.
func TestM504RoleAndViews(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	var version int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version != m504MigrationVersion {
		t.Fatalf("migration version = %d, want %d", version, m504MigrationVersion)
	}
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT is_applied FROM goose_db_version WHERE version_id = $1`, m504MigrationVersion).
		Scan(&applied); err != nil {
		t.Fatalf("read M5-04 goose row: %v", err)
	}
	if !applied {
		t.Fatal("M5-04 goose row is not applied")
	}

	var canLogin, superuser, createDB, createRole, replication bool
	var rolconfig []string
	if err := d.Pool.QueryRow(ctx,
		`SELECT rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolconfig
		   FROM pg_roles WHERE rolname = $1`, testdb.DashboardsRole).
		Scan(&canLogin, &superuser, &createDB, &createRole, &replication, &rolconfig); err != nil {
		t.Fatalf("read %s role: %v", testdb.DashboardsRole, err)
	}
	if canLogin {
		t.Errorf("%s is LOGIN, want NOLOGIN", testdb.DashboardsRole)
	}
	if superuser || createDB || createRole || replication {
		t.Errorf("%s has elevated attributes: super=%t createdb=%t createrole=%t replication=%t",
			testdb.DashboardsRole, superuser, createDB, createRole, replication)
	}
	requireRoleConfig(t, rolconfig)

	// No membership in any application group, and no parent role at all.
	for _, group := range db.Roles() {
		var member bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT pg_has_role($1::name, $2::name, 'MEMBER')`,
			testdb.DashboardsRole, group).Scan(&member); err != nil {
			t.Fatalf("read membership of %s in %s: %v", testdb.DashboardsRole, group, err)
		}
		if member {
			t.Errorf("%s is a member of %s, want no application-group membership", testdb.DashboardsRole, group)
		}
	}
	var parentRoles int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM pg_auth_members m
		   JOIN pg_roles r ON r.oid = m.member
		  WHERE r.rolname = $1`, testdb.DashboardsRole).Scan(&parentRoles); err != nil {
		t.Fatalf("count parent roles: %v", err)
	}
	if parentRoles != 0 {
		t.Fatalf("%s has %d parent role(s), want 0", testdb.DashboardsRole, parentRoles)
	}

	// Exactly the five v_ops_* views are granted, all SELECT, and nothing is
	// granted to PUBLIC.
	grants := roleTableGrants(t, d)
	wantGrants := make(map[string]string, len(dashboardViews))
	for _, view := range dashboardViews {
		wantGrants[view] = "SELECT"
	}
	if len(grants) != len(wantGrants) {
		t.Fatalf("role table grants = %v, want exactly the five v_ops_* views", grants)
	}
	for view, privilege := range wantGrants {
		if got, ok := grants[view]; !ok || got != privilege {
			t.Errorf("role grant on %s = %q/%t, want SELECT", view, got, ok)
		}
	}
	for _, view := range dashboardViews {
		var publicGrants int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.table_privileges
			  WHERE table_schema = 'public' AND table_name = $1 AND grantee = 'PUBLIC'`,
			view).Scan(&publicGrants); err != nil {
			t.Fatalf("count PUBLIC grants on %s: %v", view, err)
		}
		if publicGrants != 0 {
			t.Errorf("PUBLIC has %d grant(s) on %s, want 0", publicGrants, view)
		}
	}
	var routineGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.routine_privileges WHERE grantee = $1`,
		testdb.DashboardsRole).Scan(&routineGrants); err != nil {
		t.Fatalf("count routine grants: %v", err)
	}
	if routineGrants != 0 {
		t.Errorf("%s has %d routine grant(s), want 0", testdb.DashboardsRole, routineGrants)
	}

	// Owner-rights views: no security_invoker option.
	for _, view := range dashboardViews {
		var exists bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, view).Scan(&exists); err != nil {
			t.Fatalf("resolve view %s: %v", view, err)
		}
		if !exists {
			t.Errorf("view %s is missing", view)
		}
		var reloptions []string
		if err := d.Pool.QueryRow(ctx,
			`SELECT reloptions FROM pg_class c
			   JOIN pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = 'public' AND c.relname = $1`, view).Scan(&reloptions); err != nil {
			t.Fatalf("read view options for %s: %v", view, err)
		}
		for _, option := range reloptions {
			if strings.HasPrefix(option, "security_invoker=") {
				t.Errorf("%s has %q, want the default owner-rights view", view, option)
			}
		}
	}

	// Schema usage only; no CREATE. has_schema_privilege('USAGE') is true even
	// without this grant because PUBLIC retains USAGE on schema public, so the
	// M5-04 grant is asserted through the direct ACL entry.
	if !roleHasDirectSchemaUsage(t, d) {
		t.Errorf("%s has no direct USAGE grant on schema public", testdb.DashboardsRole)
	}
	var schemaCreate bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_schema_privilege($1::text, 'public', 'CREATE')`,
		testdb.DashboardsRole).Scan(&schemaCreate); err != nil {
		t.Fatalf("read schema CREATE privilege: %v", err)
	}
	if schemaCreate {
		t.Errorf("%s has CREATE on schema public, want denied", testdb.DashboardsRole)
	}

	// No base-table or v_agent_* privilege, and no sequence privilege.
	deniedTables := []string{
		"matches", "breaks", "cases", "proposals", "approvals",
		"applied_proposals", "ledger_txns", "external_records", "v_agent_cases",
	}
	for _, table := range deniedTables {
		var selectAllowed, insertAllowed, updateAllowed, deleteAllowed bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT has_table_privilege($1::text, $2::text, 'SELECT'),
			        has_table_privilege($1::text, $2::text, 'INSERT'),
			        has_table_privilege($1::text, $2::text, 'UPDATE'),
			        has_table_privilege($1::text, $2::text, 'DELETE')`,
			testdb.DashboardsRole, "public."+table).
			Scan(&selectAllowed, &insertAllowed, &updateAllowed, &deleteAllowed); err != nil {
			t.Fatalf("read privileges on %s: %v", table, err)
		}
		if selectAllowed || insertAllowed || updateAllowed || deleteAllowed {
			t.Errorf("%s privileges on %s = SELECT %t INSERT %t UPDATE %t DELETE %t, want all denied",
				testdb.DashboardsRole, table, selectAllowed, insertAllowed, updateAllowed, deleteAllowed)
		}
	}
	var sequence string
	if err := d.Pool.QueryRow(ctx, `SELECT pg_get_serial_sequence('ledger_txns', 'seq')`).Scan(&sequence); err != nil {
		t.Fatalf("resolve ledger_txns identity sequence: %v", err)
	}
	var sequenceUsage, sequenceSelect bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_sequence_privilege($1::text, $2::text, 'USAGE'),
		        has_sequence_privilege($1::text, $2::text, 'SELECT')`,
		testdb.DashboardsRole, sequence).Scan(&sequenceUsage, &sequenceSelect); err != nil {
		t.Fatalf("read sequence privileges: %v", err)
	}
	if sequenceUsage || sequenceSelect {
		t.Errorf("%s sequence privileges USAGE=%t SELECT=%t, want denied", testdb.DashboardsRole, sequenceUsage, sequenceSelect)
	}

	// The real dashboard login can read every view and is read-only.
	conn := openDashboardConn(t, d)
	for _, view := range dashboardViews {
		var count int64
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(&count); err != nil {
			t.Fatalf("dashboard SELECT %s: %v", view, err)
		}
	}
	var readOnly, timeout string
	if err := conn.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil {
		t.Fatalf("read default_transaction_read_only: %v", err)
	}
	if readOnly != "on" {
		t.Fatalf("default_transaction_read_only = %q, want on", readOnly)
	}
	if err := conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read statement_timeout: %v", err)
	}
	if timeout != "30s" {
		t.Fatalf("statement_timeout = %q, want 30s", timeout)
	}

	for _, table := range deniedTables {
		if _, err := conn.Exec(ctx, `SELECT count(*) FROM `+table); err != nil {
			requirePgError(t, err, sqlStateInsufficientPrivilege)
		} else {
			t.Errorf("dashboard SELECT %s succeeded, want 42501", table)
		}
	}

	// The session default blocks writes before the privilege check; the same
	// INSERT inside an explicit read-write transaction reaches the grant and is
	// denied with 42501, proving there is no write privilege either.
	if _, err := conn.Exec(ctx,
		`INSERT INTO currencies (code, exponent) VALUES ('ZZZ', 2)`); err != nil {
		requirePgError(t, err, sqlStateReadOnlyTransaction)
	} else {
		t.Fatal("dashboard INSERT in the default read-only session succeeded, want 25006")
	}

	deniedWrites := []struct {
		name string
		sql  string
	}{
		{name: "insert currency", sql: `INSERT INTO currencies (code, exponent) VALUES ('ZZZ', 2)`},
		{name: "update break", sql: `UPDATE breaks SET status = 'resolved'`},
		{name: "delete break", sql: `DELETE FROM breaks`},
		{name: "truncate breaks", sql: `TRUNCATE breaks`},
		{name: "create table", sql: `CREATE TABLE dashboard_escape (id INT)`},
	}
	for _, write := range deniedWrites {
		t.Run(write.name, func(t *testing.T) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("begin read-write transaction: %v", err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(ctx, `SET TRANSACTION READ WRITE`); err != nil {
				t.Fatalf("set transaction read write: %v", err)
			}
			_, err = tx.Exec(ctx, write.sql)
			requirePgError(t, err, sqlStateInsufficientPrivilege)
		})
	}
}

func requireRoleConfig(t *testing.T, config []string) {
	t.Helper()
	seen := make(map[string]bool, len(config))
	for _, setting := range config {
		seen[setting] = true
	}
	for _, want := range []string{
		"default_transaction_read_only=on",
		"statement_timeout=30s",
	} {
		if !seen[want] {
			t.Errorf("rolconfig = %v, missing %q", config, want)
		}
	}
}

// roleHasDirectSchemaUsage reports whether recon_dashboards_ro has its own
// USAGE entry on schema public. has_schema_privilege cannot be used for this:
// PUBLIC has USAGE on schema public by default, so it stays true after the
// direct M5-04 grant is revoked.
func roleHasDirectSchemaUsage(t *testing.T, d *testdb.DB) bool {
	t.Helper()
	var has bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1
		       FROM pg_namespace n
		       CROSS JOIN LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) AS acl
		       JOIN pg_roles r ON r.oid = acl.grantee
		      WHERE n.nspname = 'public'
		        AND r.rolname = $1
		        AND acl.privilege_type = 'USAGE')`,
		testdb.DashboardsRole).Scan(&has); err != nil {
		t.Fatalf("read direct schema USAGE grant: %v", err)
	}
	return has
}

func roleTableGrants(t *testing.T, d *testdb.DB) map[string]string {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT table_name, privilege_type
		   FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND grantee = $1
		  ORDER BY table_name, privilege_type`, testdb.DashboardsRole)
	if err != nil {
		t.Fatalf("list role table grants: %v", err)
	}
	defer rows.Close()

	grants := make(map[string]string)
	for rows.Next() {
		var table, privilege string
		if err := rows.Scan(&table, &privilege); err != nil {
			t.Fatalf("scan role table grant: %v", err)
		}
		if previous, ok := grants[table]; ok {
			grants[table] = previous + "," + privilege
			continue
		}
		grants[table] = privilege
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list role table grants: %v", err)
	}
	return grants
}

// TestM504MigrationDown applies the real goose Down on an isolated clone. The
// role is cluster-wide, so the down migration revokes this database's
// privileges, drops the views, and drops the role only when no other database
// still holds a grant (the test template and the dev database normally do).
// The test proves the safe retained-role branch and that Up restores everything.
func TestM504MigrationDown(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	sqlDB, err := sql.Open("pgx", d.DSN)
	if err != nil {
		t.Fatalf("open goose database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("configure goose: %v", err)
	}
	if version := gooseVersion(t, sqlDB); version != m504MigrationVersion {
		t.Fatalf("goose version = %d, want %d", version, m504MigrationVersion)
	}

	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("goose down M5-04: %v", err)
	}
	if version := gooseVersion(t, sqlDB); version != m504PreviousVersion {
		t.Fatalf("version after down = %d, want %d", version, m504PreviousVersion)
	}
	for _, view := range dashboardViews {
		var exists bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, view).Scan(&exists); err != nil {
			t.Fatalf("resolve view %s after down: %v", view, err)
		}
		if exists {
			t.Errorf("view %s still exists after down", view)
		}
	}
	var roleExists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		testdb.DashboardsRole).Scan(&roleExists); err != nil {
		t.Fatalf("check dashboard role after down: %v", err)
	}
	if !roleExists {
		t.Fatal("dashboard role was dropped in a test cluster where the template still references it; the guarded drop failed")
	}
	var canLogin bool
	var config []string
	if err := d.Pool.QueryRow(ctx,
		`SELECT rolcanlogin, rolconfig FROM pg_roles WHERE rolname = $1`,
		testdb.DashboardsRole).Scan(&canLogin, &config); err != nil {
		t.Fatalf("read retained role after down: %v", err)
	}
	if canLogin {
		t.Error("retained dashboard role is LOGIN, want NOLOGIN")
	}
	requireRoleConfig(t, config)
	if got := roleTableGrants(t, d); len(got) != 0 {
		t.Errorf("retained role still has view grants in this database: %v", got)
	}
	if roleHasDirectSchemaUsage(t, d) {
		t.Error("retained role still has a direct USAGE grant on schema public after down")
	}

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("goose up M5-04 after down: %v", err)
	}
	if version := gooseVersion(t, sqlDB); version != m504MigrationVersion {
		t.Fatalf("version after re-up = %d, want %d", version, m504MigrationVersion)
	}
	for _, view := range dashboardViews {
		var exists bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, view).Scan(&exists); err != nil {
			t.Fatalf("resolve view %s after re-up: %v", view, err)
		}
		if !exists {
			t.Errorf("view %s is missing after re-up", view)
		}
	}
	if got := roleTableGrants(t, d); len(got) != len(dashboardViews) {
		t.Fatalf("grants after re-up = %v, want the five v_ops_* views", got)
	}
	if !roleHasDirectSchemaUsage(t, d) {
		t.Error("dashboard role lost its direct schema USAGE grant after re-up")
	}

	// A goose Up at the current version is a no-op.
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("idempotent goose up: %v", err)
	}
	if version := gooseVersion(t, sqlDB); version != m504MigrationVersion {
		t.Fatalf("version after idempotent up = %d, want %d", version, m504MigrationVersion)
	}
}

func gooseVersion(t *testing.T, sqlDB *sql.DB) int64 {
	t.Helper()
	var version int64
	if err := sqlDB.QueryRow(`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	return version
}
