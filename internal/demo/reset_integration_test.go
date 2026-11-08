//go:build integration

package demo_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/demo"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

// TestMain prepares the migration template cloned by the reset regression
// test. The template already contains eval_truth, which is the schema that
// must not survive a reset.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "demo: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "demo: ensure test template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestResetTwiceDropsEvalTruth is the regression for the M3-02 reset
// collision: DROP SCHEMA public CASCADE left eval_truth behind, so the next
// migration run failed with SQLSTATE 42P07 ("sandbox_meta already exists")
// and left the database unmigrated. Two consecutive resets must both reach
// the latest migration and leave a newly created, empty eval_truth.
func TestResetTwiceDropsEvalTruth(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()

	if !relationExists(t, db, "eval_truth.scenarios") {
		t.Fatal("template clone is missing eval_truth.scenarios; the reset regression needs a pre-existing eval schema")
	}

	latest := latestMigrationVersion(t)
	for run := 1; run <= 2; run++ {
		if err := demo.Reset(ctx, db.DSN); err != nil {
			t.Fatalf("Reset run %d: %v", run, err)
		}

		if !relationExists(t, db, "eval_truth.scenarios") {
			t.Fatalf("reset run %d: eval_truth.scenarios was not recreated", run)
		}
		for _, table := range []string{"sandbox_meta", "scenarios", "scenario_cases"} {
			if got := queryInt(t, db, "SELECT count(*) FROM eval_truth."+table); got != 0 {
				t.Fatalf("reset run %d: eval_truth.%s has %d rows, want 0", run, table, got)
			}
		}
		if got := gooseVersion(t, db); got != latest {
			t.Fatalf("reset run %d: goose version = %d, want latest %d", run, got, latest)
		}
	}
}

func relationExists(t *testing.T, db *testdb.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("check relation %q: %v", name, err)
	}
	return exists
}

func queryInt(t *testing.T, db *testdb.DB, query string) int64 {
	t.Helper()
	var got int64
	if err := db.Pool.QueryRow(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return got
}

func gooseVersion(t *testing.T, db *testdb.DB) int64 {
	t.Helper()
	return queryInt(t, db,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version WHERE is_applied`)
}

// latestMigrationVersion derives the newest embedded migration version from
// the migration filenames, so the assertion follows new migrations without a
// hard-coded version.
func latestMigrationVersion(t *testing.T) int64 {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var latest int64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("migration %q has no version prefix", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %q has invalid version prefix: %v", name, err)
		}
		if version > latest {
			latest = version
		}
	}
	if latest == 0 {
		t.Fatal("no embedded migrations found")
	}
	return latest
}
