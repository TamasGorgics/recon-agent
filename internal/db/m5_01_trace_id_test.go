//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const (
	m501MigrationVersion = 20261122120000
	m501PreviousVersion  = 20261120100000
)

// TestM501ProposalTraceIDMigration proves the M5-01 schema delta: the nullable
// proposals.trace_id column exists at the latest version, Down removes it, a
// legacy row inserted while it is absent stays NULL after Up, and the existing
// table-level grants still cover the new column.
func TestM501ProposalTraceIDMigration(t *testing.T) {
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

	// Down to (and including) the M5-01 migration, then insert a legacy
	// proposal row without the column.
	if err := goose.DownToContext(ctx, sqlDB, ".", m501PreviousVersion); err != nil {
		t.Fatalf("goose down M5-01: %v", err)
	}
	if m501ColumnExists(t, d) {
		t.Fatal("proposals.trace_id still exists after Down")
	}
	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	var legacyID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'legacy row',
		         0.9000, 'state-hash', 'm5-01-legacy', now())
		 RETURNING id::text`, caseID, runID).Scan(&legacyID); err != nil {
		t.Fatalf("insert legacy proposal: %v", err)
	}

	if err := goose.UpToContext(ctx, sqlDB, ".", m501MigrationVersion); err != nil {
		t.Fatalf("goose up M5-01: %v", err)
	}
	if !m501ColumnExists(t, d) {
		t.Fatal("proposals.trace_id missing after Up")
	}
	var dataType, nullable string
	if err := d.Pool.QueryRow(ctx,
		`SELECT data_type, is_nullable
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'proposals'
		    AND column_name = 'trace_id'`).Scan(&dataType, &nullable); err != nil {
		t.Fatalf("read proposals.trace_id metadata: %v", err)
	}
	if dataType != "text" || nullable != "YES" {
		t.Fatalf("proposals.trace_id = %s nullable=%s, want text/YES", dataType, nullable)
	}
	var legacyTraceID *string
	if err := d.Pool.QueryRow(ctx,
		`SELECT trace_id FROM proposals WHERE id = $1::uuid`, legacyID).
		Scan(&legacyTraceID); err != nil {
		t.Fatalf("read legacy proposal: %v", err)
	}
	if legacyTraceID != nil {
		t.Fatalf("legacy proposal trace_id = %q, want NULL", *legacyTraceID)
	}

	// The new nullable column is covered by the existing table-level grants:
	// recon_agent_ro inserts, recon_app selects, and no unintended UPDATE
	// appears on trace_id.
	m501RequireTablePrivilege(t, d, db.RoleAgentRO, "INSERT", true)
	m501RequireTablePrivilege(t, d, db.RoleApp, "SELECT", true)
	m501RequireColumnPrivilege(t, d, db.RoleExecutor, "UPDATE", false)
	m501RequireColumnPrivilege(t, d, db.RoleAgentRO, "SELECT", false)

	agent := openRolePool(t, d, db.RoleAgentRO)
	app := openRolePool(t, d, db.RoleApp)
	grantCaseID := m2CreateCase(t, d)
	grantRunID := m2CreateAgentRun(t, d, grantCaseID)
	traceID := "0123456789abcdef0123456789abcdef"
	insertedID := m2InsertProposalWithTrace(t, agent, grantCaseID, grantRunID, "m5-01-grant", traceID)
	var readTraceID *string
	if err := app.QueryRow(ctx,
		`SELECT trace_id FROM proposals WHERE id = $1::uuid`, insertedID).
		Scan(&readTraceID); err != nil {
		t.Fatalf("app read inserted trace_id: %v", err)
	}
	if readTraceID == nil || *readTraceID != traceID {
		t.Fatalf("inserted trace_id = %v, want %q", readTraceID, traceID)
	}

	// Down removes the column without disturbing other proposal columns.
	if err := goose.DownToContext(ctx, sqlDB, ".", m501PreviousVersion); err != nil {
		t.Fatalf("second goose down M5-01: %v", err)
	}
	if m501ColumnExists(t, d) {
		t.Fatal("proposals.trace_id still exists after the second Down")
	}
	var idempotencyKey string
	if err := d.Pool.QueryRow(ctx,
		`SELECT idempotency_key FROM proposals WHERE id = $1::uuid`, insertedID).
		Scan(&idempotencyKey); err != nil {
		t.Fatalf("read proposal after Down: %v", err)
	}
	if idempotencyKey != "m5-01-grant" {
		t.Fatalf("idempotency_key after Down = %q", idempotencyKey)
	}
}

func gooseVersion(t *testing.T, sqlDB *sql.DB) int64 {
	t.Helper()
	var version int64
	if err := sqlDB.QueryRow(
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version WHERE is_applied`).
		Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	return version
}

func m501ColumnExists(t *testing.T, d *testdb.DB) bool {
	t.Helper()
	var exists bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.columns
		      WHERE table_schema = 'public' AND table_name = 'proposals'
		        AND column_name = 'trace_id')`).Scan(&exists); err != nil {
		t.Fatalf("check proposals.trace_id: %v", err)
	}
	return exists
}

func m501RequireTablePrivilege(t *testing.T, d *testdb.DB, role, privilege string, want bool) {
	t.Helper()
	var got bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1::text, 'public.proposals', $2::text)`,
		role, privilege).Scan(&got); err != nil {
		t.Fatalf("read %s %s on proposals: %v", role, privilege, err)
	}
	if got != want {
		t.Fatalf("%s %s on proposals = %t, want %t", role, privilege, got, want)
	}
}

func m501RequireColumnPrivilege(t *testing.T, d *testdb.DB, role, privilege string, want bool) {
	t.Helper()
	var got bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_column_privilege($1::text, 'public.proposals', 'trace_id', $2::text)`,
		role, privilege).Scan(&got); err != nil {
		t.Fatalf("read %s %s on proposals.trace_id: %v", role, privilege, err)
	}
	if got != want {
		t.Fatalf("%s %s on proposals.trace_id = %t, want %t", role, privilege, got, want)
	}
}

func m2InsertProposalWithTrace(t *testing.T, pool *pgxpool.Pool, caseID, runID, key, traceID string) string {
	t.Helper()
	// Client-generated UUID: recon_agent_ro has INSERT but no RETURNING grant.
	const id = "12345678-1234-4234-8234-123456789012"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO proposals
		     (id, case_id, agent_run_id, type, payload, rationale, confidence,
		      state_hash, idempotency_key, created_at, trace_id)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'link', '{}'::jsonb, 'grant insert',
		         0.9000, 'state-hash', $4, now(), $5)`,
		id, caseID, runID, key, traceID); err != nil {
		t.Fatalf("agent insert proposal with trace_id: %v", err)
	}
	return id
}
