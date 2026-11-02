//go:build integration

package testdb_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain enforces the ADV-01 lifecycle: the template exists before the
// baseline is recorded, and no new recon_test_% database may survive m.Run.
// The template itself is explicitly excluded from the orphan assertion.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "testdb: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}

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
	fmt.Printf("testdb: baseline recon_test_%% databases: %s\n", strings.Join(before, ", "))

	code := m.Run()

	after, err := listTestDatabases(ctx, adminDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: list databases after run: %v\n", err)
		os.Exit(1)
	}
	orphans := newDatabases(before, after, testdb.DefaultTemplate)
	if len(orphans) > 0 {
		fmt.Fprintf(os.Stderr, "testdb: orphan databases after run: %s\n", strings.Join(orphans, ", "))
		if code == 0 {
			code = 1
		}
	} else {
		fmt.Printf("testdb: no new recon_test_%% databases after run (template %q excluded)\n", testdb.DefaultTemplate)
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
	if version != 1 {
		t.Fatalf("migration version = %d, want 1", version)
	}

	if got := d.DSNFor(""); got != d.DSN {
		t.Fatalf("DSNFor(\"\") = %q, want %q", got, d.DSN)
	}
	if got := d.DSNFor("app"); got != d.DSN {
		t.Fatalf("DSNFor(\"app\") = %q, want the default DSN %q at M0", got, d.DSN)
	}
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

func newDatabases(before, after []string, exclude string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var added []string
	for _, name := range after {
		if name == exclude {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		added = append(added, name)
	}
	return added
}
