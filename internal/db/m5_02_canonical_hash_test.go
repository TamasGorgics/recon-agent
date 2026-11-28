//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const (
	m502MigrationVersion = 20261123120000
	m502PreviousVersion  = 20261122120000
	m506MigrationVersion = 20261126120000
)

// TestM502ToolResultCanonicalHashMigration proves the M5-02 schema delta: the
// nullable tool_calls.result_canonical_hash column exists at the latest
// version, a legacy row stays NULL after Up, the existing table-level grants
// cover the new column, and Down removes only the column.
func TestM502ToolResultCanonicalHashMigration(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	sqlDB, err := sql.Open("pgx", d.DSN)
	if err != nil {
		t.Fatalf("open goose database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if version := gooseVersion(t, sqlDB); version != m506MigrationVersion {
		t.Fatalf("goose version = %d, want %d", version, m506MigrationVersion)
	}
	m502RequireColumn(t, d, "text", "YES")

	// Down to the M5-02 migration, then insert a legacy tool call without
	// the column. DownTo also unwinds any newer migration (M5-04 and M5-05).
	if err := goose.DownToContext(ctx, sqlDB, ".", m502PreviousVersion); err != nil {
		t.Fatalf("goose down M5-02: %v", err)
	}
	if m502ColumnExists(t, d) {
		t.Fatal("tool_calls.result_canonical_hash still exists after Down")
	}

	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	var legacyID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO tool_calls
		     (agent_run_id, seq, tool_name, args, result, result_hash,
		      started_at, duration_ms)
		 VALUES ($1::uuid, 1, 'legacy_tool', '{}'::jsonb,
		         '{"data":{"legacy":true}}'::jsonb, 'legacy-result-hash',
		         now(), 0)
		 RETURNING id::text`, runID).Scan(&legacyID); err != nil {
		t.Fatalf("insert legacy tool call: %v", err)
	}

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("goose up M5-02: %v", err)
	}
	m502RequireColumn(t, d, "text", "YES")
	var legacyCanonicalHash *string
	if err := d.Pool.QueryRow(ctx,
		`SELECT result_canonical_hash FROM tool_calls WHERE id = $1::uuid`, legacyID).
		Scan(&legacyCanonicalHash); err != nil {
		t.Fatalf("read legacy tool call: %v", err)
	}
	if legacyCanonicalHash != nil {
		t.Fatalf("legacy result_canonical_hash = %q, want NULL", *legacyCanonicalHash)
	}

	// No backfill index was added.
	var indexes int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		  WHERE schemaname = 'public' AND tablename = 'tool_calls'
		    AND indexdef LIKE '%result_canonical_hash%'`).Scan(&indexes); err != nil {
		t.Fatalf("count canonical hash indexes: %v", err)
	}
	if indexes != 0 {
		t.Fatalf("result_canonical_hash indexes = %d, want 0", indexes)
	}

	// The existing table-level recon_app grants cover the new column; no new
	// write grant is introduced for any other runtime role.
	m502RequireTablePrivilege(t, d, db.RoleApp, "SELECT", true)
	m502RequireTablePrivilege(t, d, db.RoleApp, "INSERT", true)
	m502RequireTablePrivilege(t, d, db.RoleApp, "UPDATE", false)
	m502RequireColumnPrivilege(t, d, db.RoleApp, "SELECT", true)
	m502RequireColumnPrivilege(t, d, db.RoleApp, "UPDATE", false)
	m502RequireColumnPrivilege(t, d, db.RoleAgentRO, "SELECT", false)
	m502RequireColumnPrivilege(t, d, db.RoleExecutor, "SELECT", false)

	// recon_app can insert the new column through the existing INSERT grant.
	app := openRolePool(t, d, db.RoleApp)
	insertedID := "12345678-1234-4234-8234-123456789012"
	canonicalHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := app.Exec(ctx,
		`INSERT INTO tool_calls
		     (id, agent_run_id, seq, tool_name, args, result, result_hash,
		      result_canonical_hash, started_at, duration_ms)
		 VALUES ($1::uuid, $2::uuid, 2, 'app_tool', '{}'::jsonb,
		         '{"data":{"a":1}}'::jsonb, 'app-result-hash', $3, now(), 0)`,
		insertedID, runID, canonicalHash); err != nil {
		t.Fatalf("app insert tool call with canonical hash: %v", err)
	}
	var readCanonicalHash string
	if err := d.Pool.QueryRow(ctx,
		`SELECT result_canonical_hash FROM tool_calls WHERE id = $1::uuid`, insertedID).
		Scan(&readCanonicalHash); err != nil {
		t.Fatalf("read app-inserted tool call: %v", err)
	}
	if readCanonicalHash != canonicalHash {
		t.Fatalf("result_canonical_hash = %q, want %q", readCanonicalHash, canonicalHash)
	}

	// Down removes the column without disturbing the rows or other columns.
	if err := goose.DownToContext(ctx, sqlDB, ".", m502PreviousVersion); err != nil {
		t.Fatalf("second goose down M5-02: %v", err)
	}
	if m502ColumnExists(t, d) {
		t.Fatal("tool_calls.result_canonical_hash still exists after the second Down")
	}
	var storedResultHash string
	if err := d.Pool.QueryRow(ctx,
		`SELECT result_hash FROM tool_calls WHERE id = $1::uuid`, insertedID).
		Scan(&storedResultHash); err != nil {
		t.Fatalf("read tool call after Down: %v", err)
	}
	if storedResultHash != "app-result-hash" {
		t.Fatalf("result_hash after Down = %q", storedResultHash)
	}
}

func m502ColumnExists(t *testing.T, d *testdb.DB) bool {
	t.Helper()
	var exists bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.columns
		      WHERE table_schema = 'public' AND table_name = 'tool_calls'
		        AND column_name = 'result_canonical_hash')`).Scan(&exists); err != nil {
		t.Fatalf("check tool_calls.result_canonical_hash: %v", err)
	}
	return exists
}

func m502RequireColumn(t *testing.T, d *testdb.DB, wantType, wantNullable string) {
	t.Helper()
	var dataType, nullable string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT data_type, is_nullable
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'tool_calls'
		    AND column_name = 'result_canonical_hash'`).
		Scan(&dataType, &nullable); err != nil {
		t.Fatalf("read tool_calls.result_canonical_hash metadata: %v", err)
	}
	if dataType != wantType || nullable != wantNullable {
		t.Fatalf("tool_calls.result_canonical_hash = %s nullable=%s, want %s/%s",
			dataType, nullable, wantType, wantNullable)
	}
}

func m502RequireTablePrivilege(t *testing.T, d *testdb.DB, role, privilege string, want bool) {
	t.Helper()
	var got bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1::text, 'public.tool_calls', $2::text)`,
		role, privilege).Scan(&got); err != nil {
		t.Fatalf("read %s %s on tool_calls: %v", role, privilege, err)
	}
	if got != want {
		t.Fatalf("%s %s on tool_calls = %t, want %t", role, privilege, got, want)
	}
}

func m502RequireColumnPrivilege(t *testing.T, d *testdb.DB, role, privilege string, want bool) {
	t.Helper()
	var got bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_column_privilege($1::text, 'public.tool_calls',
		                             'result_canonical_hash', $2::text)`,
		role, privilege).Scan(&got); err != nil {
		t.Fatalf("read %s %s on tool_calls.result_canonical_hash: %v", role, privilege, err)
	}
	if got != want {
		t.Fatalf("%s %s on tool_calls.result_canonical_hash = %t, want %t",
			role, privilege, got, want)
	}
}
