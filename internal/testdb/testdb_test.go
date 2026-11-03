//go:build integration

package testdb_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

// TestMain enforces the ADV-01 lifecycle: the template exists before the
// baseline is recorded, and no new recon_test_% database created by this test
// process may survive m.Run. Clone names embed the process id, so databases
// created concurrently by other package test binaries are not this process's
// orphans; the template itself is explicitly excluded from the assertion.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "testdb: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	dbPrefix := testdb.DatabasePrefix()

	created, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN})
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: ensure template: %v\n", err)
		os.Exit(1)
	}
	if created {
		fmt.Printf("testdb: created template %q and applied migrations\n", testdb.DefaultTemplate)
	} else {
		fmt.Printf("testdb: reusing template %q\n", testdb.DefaultTemplate)
	}

	before, err := listTestDatabases(ctx, adminDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: list databases before run: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("testdb: baseline recon_test_%% databases (orphan scope prefix %q): %s\n", dbPrefix, strings.Join(before, ", "))

	rolePrefix := testdb.LoginRolePrefix()
	beforeRoles, err := listLoginRoles(ctx, adminDSN, rolePrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: list login roles before run: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("testdb: baseline login roles with prefix %q: %s\n", rolePrefix, strings.Join(beforeRoles, ", "))

	code := m.Run()

	after, err := listTestDatabases(ctx, adminDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: list databases after run: %v\n", err)
		os.Exit(1)
	}
	orphans := newDatabases(before, after, dbPrefix)
	if len(orphans) > 0 {
		fmt.Fprintf(os.Stderr, "testdb: orphan databases after run (process prefix %q): %s\n", dbPrefix, strings.Join(orphans, ", "))
		if code == 0 {
			code = 1
		}
	} else {
		fmt.Printf("testdb: no new databases with process prefix %q after run (template %q excluded)\n", dbPrefix, testdb.DefaultTemplate)
	}

	afterRoles, err := listLoginRoles(ctx, adminDSN, rolePrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: list login roles after run: %v\n", err)
		os.Exit(1)
	}
	roleOrphans := newRoleNames(beforeRoles, afterRoles)
	if len(roleOrphans) > 0 {
		fmt.Fprintf(os.Stderr, "testdb: orphan login roles after run: %s\n", strings.Join(roleOrphans, ", "))
		if code == 0 {
			code = 1
		}
	} else {
		fmt.Printf("testdb: no new login roles with prefix %q after run\n", rolePrefix)
	}

	os.Exit(code)
}

func TestIsolatedDatabase(t *testing.T) {
	d := testdb.New(t)

	if d.Name == "" || d.DSN == "" || d.Pool == nil {
		t.Fatalf("New() returned an incomplete DB: %+v", d)
	}
	if d.Name == testdb.DefaultTemplate {
		t.Fatalf("New() returned the template database %q", d.Name)
	}
	if !strings.HasPrefix(d.Name, "recon_test_") {
		t.Fatalf("database name = %q, want recon_test_ prefix", d.Name)
	}

	var one int
	if err := d.Pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query isolated database: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 = %d, want 1", one)
	}

	var version int64
	if err := d.Pool.QueryRow(context.Background(), "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatalf("template migration missing from clone: %v", err)
	}
	if want := currentMigrationVersion(t); version != want {
		t.Fatalf("migration version = %d, want %d", version, want)
	}

	if got := d.DSNFor(""); got != d.DSN {
		t.Fatalf("DSNFor(\"\") = %q, want %q", got, d.DSN)
	}
	for _, role := range []string{testdb.RoleApp, testdb.RoleExecutor, testdb.RoleAgentRO, testdb.RoleAuditRO} {
		dsn := d.DSNFor(role)
		if dsn == "" || dsn == d.DSN {
			t.Fatalf("DSNFor(%q) = %q, want an ephemeral role DSN distinct from the maintenance DSN", role, dsn)
		}
	}
	if got, want := d.DSNFor("app"), d.DSNFor(testdb.RoleApp); got != want {
		t.Fatalf("DSNFor(\"app\") = %q, want the alias of %s (%q)", got, testdb.RoleApp, want)
	}
	if got := len(d.LoginNames()); got != 4 {
		t.Fatalf("LoginNames() returned %d logins, want one per role", got)
	}
}

// TestEnsureTemplateRecreatesOnVersionMismatch proves D-3: a template whose
// goose version is older, newer, or missing relative to migrations.FS is
// dropped (WITH FORCE) and rebuilt from the current migration set.
func TestEnsureTemplateRecreatesOnVersionMismatch(t *testing.T) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}

	// Deliberately avoid the recon_test_% namespace: another package's TestMain
	// may run concurrently and would treat a new recon_test_% database as an
	// orphan. This template is dropped by cleanup below.
	template := fmt.Sprintf("recon_tmpl_version_check_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), adminDSN)
		if err != nil {
			t.Errorf("connect to drop template %q: %v", template, err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+template+`" WITH (FORCE)`); err != nil {
			t.Errorf("drop template %q: %v", template, err)
		}
	})

	created, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN, Template: template})
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if !created {
		t.Fatal("first EnsureTemplate call did not create the template")
	}

	tamperCases := []struct {
		name string
		sql  string
	}{
		{"older migration version", `DELETE FROM goose_db_version WHERE version_id <> 0`},
		{"newer migration version", `INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999999999999, true)`},
		{"missing goose version table", `DROP TABLE goose_db_version`},
	}

	for _, tc := range tamperCases {
		t.Run(tc.name, func(t *testing.T) {
			conn := connectTemplate(t, adminDSN, template)
			if _, err := conn.Exec(ctx, `CREATE TABLE stale_marker (id INT)`); err != nil {
				_ = conn.Close(ctx)
				t.Fatalf("create stale marker: %v", err)
			}
			if _, err := conn.Exec(ctx, tc.sql); err != nil {
				_ = conn.Close(ctx)
				t.Fatalf("tamper template: %v", err)
			}
			if err := conn.Close(ctx); err != nil {
				t.Fatalf("close template connection before drop: %v", err)
			}

			created, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN, Template: template})
			if err != nil {
				t.Fatalf("recreate stale template: %v", err)
			}
			if !created {
				t.Fatal("stale template was reused instead of recreated")
			}

			conn = connectTemplate(t, adminDSN, template)
			defer func() { _ = conn.Close(context.Background()) }()

			var version int64
			if err := conn.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil {
				t.Fatalf("read recreated template version: %v", err)
			}
			if want := currentMigrationVersion(t); version != want {
				t.Fatalf("recreated template version = %d, want %d", version, want)
			}

			var marker, currencies string
			if err := conn.QueryRow(ctx,
				`SELECT COALESCE(to_regclass('public.stale_marker')::text, ''), COALESCE(to_regclass('public.currencies')::text, '')`).
				Scan(&marker, &currencies); err != nil {
				t.Fatalf("inspect recreated template: %v", err)
			}
			if marker != "" {
				t.Fatalf("stale marker %q survived template recreation", marker)
			}
			if currencies == "" {
				t.Fatal("currencies table missing after recreation")
			}
		})
	}

	created, err = testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN, Template: template})
	if err != nil {
		t.Fatalf("reuse current template: %v", err)
	}
	if created {
		t.Fatal("template at the current migration version was recreated instead of reused")
	}
}

// currentMigrationVersion returns the highest migration version embedded in
// migrations.FS, matching what testdb.EnsureTemplate compares against.
func currentMigrationVersion(t *testing.T) int64 {
	t.Helper()
	goose.SetBaseFS(migrations.FS)
	found, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		t.Fatalf("collect migrations: %v", err)
	}
	var maxVersion int64
	for _, migration := range found {
		if migration.Version > maxVersion {
			maxVersion = migration.Version
		}
	}
	if maxVersion == 0 {
		t.Fatal("no migrations embedded in migrations.FS")
	}
	return maxVersion
}

func connectTemplate(t *testing.T, adminDSN, template string) *pgx.Conn {
	t.Helper()
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("admin DSN must be a postgres:// URL, got scheme %q", u.Scheme)
	}
	u.Path = "/" + template
	u.RawPath = ""
	conn, err := pgx.Connect(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect to template %q: %v", template, err)
	}
	return conn
}

func listTestDatabases(ctx context.Context, adminDSN string) ([]string, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	rows, err := conn.Query(ctx,
		`SELECT datname FROM pg_database WHERE starts_with(datname, 'recon_test_') ORDER BY datname`)
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

// TestLoginCleanup proves the per-test cleanup drops every ephemeral LOGIN
// role. The post-cleanup assertion is registered before testdb.New so LIFO
// ordering runs it after the database cleanup.
func TestLoginCleanup(t *testing.T) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	prefix := testdb.LoginRolePrefix()

	t.Cleanup(func() {
		names, err := listLoginRoles(context.Background(), adminDSN, prefix)
		if err != nil {
			t.Errorf("list login roles after cleanup: %v", err)
			return
		}
		if len(names) != 0 {
			t.Errorf("ephemeral login roles survived cleanup: %s", strings.Join(names, ", "))
		}
	})

	d := testdb.New(t)
	names := d.LoginNames()
	if len(names) != 4 {
		t.Fatalf("LoginNames() = %v, want one login per role", names)
	}

	live, err := listLoginRoles(ctx, adminDSN, prefix)
	if err != nil {
		t.Fatalf("list live login roles: %v", err)
	}
	if len(live) != len(names) {
		t.Fatalf("live login roles = %v, want %v", live, names)
	}
	for _, name := range names {
		found := false
		for _, liveName := range live {
			if liveName == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("login role %q is not present in pg_roles", name)
		}
	}
}
