//go:build integration

// covers: FIN-23, FIN-33
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
	m411MigrationVersion = 20261117120000
	m406MigrationVersion = 20261114120000
)

// TestM411ApprovalMigration proves the approved M4-11 migration through the
// real recon_app login: approvers.api_key_id is a nullable unique non-empty
// text column, and recon_app can set proposals.expires_at without gaining any
// other proposal column.
func TestM411ApprovalMigration(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	var count int
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(is_applied), false)
		   FROM goose_db_version WHERE version_id = $1`, m411MigrationVersion).
		Scan(&count, &applied); err != nil {
		t.Fatalf("read M4-11 goose version: %v", err)
	}
	if count != 1 || !applied {
		t.Fatalf("M4-11 migration rows/applied = %d/%t, want 1/true", count, applied)
	}

	// Column shape is exactly one nullable text column with no default.
	columns := m4TableColumns(t, d, "approvers")
	apiKey, ok := columns["api_key_id"]
	if !ok {
		t.Fatalf("approvers.api_key_id is missing; columns = %v", m4ColumnNames(columns))
	}
	if apiKey.dataType != "text" || apiKey.nullable != "YES" || apiKey.def != "" {
		t.Fatalf("approvers.api_key_id = %+v, want nullable text without default", apiKey)
	}

	// Unique constraint and the non-empty check.
	cons := m4TableConstraints(t, d, "approvers")
	if _, ok := cons["approvers_api_key_id_key"]; !ok {
		t.Fatalf("approvers_api_key_id_key is missing; constraints = %v", cons)
	}
	m4RequireCheck(t, d, "approvers", "api_key_id", "<> ''")

	// The real app login can set expires_at.
	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	proposalID := m2InsertProposal(t, d.Pool, caseID, runID, "m4-11-app-expiry", "proposed")
	if _, err := app.Exec(ctx,
		`UPDATE proposals SET expires_at = '2026-11-19T12:00:00Z' WHERE id = $1::uuid`,
		proposalID); err != nil {
		t.Fatalf("app UPDATE proposals.expires_at: %v", err)
	}
	var expiresAt string
	if err := d.Pool.QueryRow(ctx,
		`SELECT expires_at::text FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&expiresAt); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if expiresAt == "" {
		t.Fatal("expires_at was not written")
	}

	// No table-level UPDATE, and exactly expires_at joins the existing columns.
	var tableUpdate bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_table_privilege($1::name, 'public.proposals', 'UPDATE')`,
		db.RoleApp).Scan(&tableUpdate); err != nil {
		t.Fatalf("read app table-level UPDATE: %v", err)
	}
	if tableUpdate {
		t.Fatal("app has table-level UPDATE on proposals, want column-level only")
	}
	for _, column := range []string{
		"expires_at", "status", "version", "risk_tier", "policy_decision",
		"verification", "pattern_verified",
	} {
		var allowed bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT has_column_privilege($1::name, 'public.proposals', $2::text, 'UPDATE')`,
			db.RoleApp, column).Scan(&allowed); err != nil {
			t.Fatalf("read app UPDATE on proposals.%s: %v", column, err)
		}
		if !allowed {
			t.Errorf("app lacks UPDATE on proposals.%s, want granted", column)
		}
	}
	for _, column := range []string{
		"payload", "rationale", "evidence", "state_hash", "type", "confidence",
		"case_id", "agent_run_id", "idempotency_key", "created_at",
		"supersedes_proposal_id",
	} {
		var allowed bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT has_column_privilege($1::name, 'public.proposals', $2::text, 'UPDATE')`,
			db.RoleApp, column).Scan(&allowed); err != nil {
			t.Fatalf("read app UPDATE on proposals.%s: %v", column, err)
		}
		if allowed {
			t.Errorf("app has UPDATE on proposals.%s, want denied", column)
		}
	}

	// api_key_id is unique when present, and the empty string is rejected.
	var firstID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO approvers (display_name, role, api_key_id)
		 VALUES ('API Key One', 'approver', 'key-1')
		 RETURNING id::text`).Scan(&firstID); err != nil {
		t.Fatalf("insert approver with api_key_id: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO approvers (display_name, role, api_key_id)
		 VALUES ('API Key Two', 'approver', 'key-1')`); err == nil {
		t.Fatal("duplicate api_key_id accepted")
	} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "approvers_api_key_id_key" {
		t.Fatalf("duplicate api_key_id constraint = %q, want approvers_api_key_id_key", pgErr.ConstraintName)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO approvers (display_name, role, api_key_id)
		 VALUES ('Empty Key', 'approver', '')`); err == nil {
		t.Fatal("empty api_key_id accepted")
	} else {
		requirePgError(t, err, sqlStateCheckViolation)
	}
	// NULL keys are distinct, so multiple approvers can exist without one.
	for _, name := range []string{"No Key A", "No Key B"} {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvers (display_name, role) VALUES ($1, 'approver')`, name); err != nil {
			t.Fatalf("insert approver %q without api_key_id: %v", name, err)
		}
	}
	// The app keeps SELECT on approvers.
	if _, err := app.Exec(ctx, `SELECT count(*) FROM approvers`); err != nil {
		t.Fatalf("app SELECT approvers: %v", err)
	}
}

// TestM411ApprovalMigrationDown applies the embedded Down migration on an
// isolated clone and proves it reverses exactly its own changes: the grant and
// the column, while every earlier proposal grant and the approvers table remain.
func TestM411ApprovalMigrationDown(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	if !m405ColumnUpdate(t, d, db.RoleApp, "expires_at") {
		t.Fatal("app lacks pre-Down UPDATE on proposals.expires_at")
	}

	sqlDB, err := sql.Open("pgx", d.DSN)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("configure goose: %v", err)
	}
	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("apply M4-11 Down: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close scratch database: %v", err)
	}

	var version int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read version after M4-11 Down: %v", err)
	}
	if version != m406MigrationVersion {
		t.Fatalf("version after M4-11 Down = %d, want %d", version, m406MigrationVersion)
	}

	var exists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_schema = 'public' AND table_name = 'approvers'
		                   AND column_name = 'api_key_id')`).Scan(&exists); err != nil {
		t.Fatalf("read api_key_id column: %v", err)
	}
	if exists {
		t.Fatal("approvers.api_key_id still exists after Down")
	}

	for _, column := range []string{"expires_at"} {
		if m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Errorf("app still has UPDATE on proposals.%s after Down", column)
		}
	}
	for _, column := range []string{
		"status", "version", "risk_tier", "policy_decision",
		"verification", "pattern_verified",
	} {
		if !m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Errorf("M4-11 Down revoked proposals.%s, which belongs to an earlier migration", column)
		}
	}
	if m405TablePrivilege(t, d, db.RoleApp, "UPDATE") {
		t.Error("app has table-level UPDATE after M4-11 Down")
	}
	var approversExists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT to_regclass('public.approvers') IS NOT NULL`).Scan(&approversExists); err != nil {
		t.Fatalf("resolve approvers table: %v", err)
	}
	if !approversExists {
		t.Fatal("approvers table was dropped by the M4-11 Down")
	}
}
