//go:build integration

// covers: FIN-13, FIN-34
package db_test

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const (
	m405MigrationVersion          = 20261113090000
	m405ReadGrantMigrationVersion = 20261113091000
)

// m405VerifierViews is the exact read surface the deterministic verifier loads
// its sources through.
var m405VerifierViews = []string{
	"v_agent_case_breaks",
	"v_agent_breaks",
	"v_agent_external_records",
	"v_agent_ledger_txns",
	"v_agent_ledger_entries",
	"v_agent_fee_schedules",
	"v_agent_fx_rates",
}

// m405RemainingAgentViews stay agent-only. Widening recon_app's read surface to
// any of them would exceed the verifier's need.
var m405RemainingAgentViews = []string{
	"v_agent_accounts",
	"v_agent_scopes",
	"v_agent_cases",
	"v_agent_settlement_batches",
	"v_agent_statement_balances",
	"v_agent_history",
}

func m405ColumnUpdate(t *testing.T, d *testdb.DB, role, column string) bool {
	t.Helper()
	var allowed bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_column_privilege($1::name, 'public.proposals', $2::text, 'UPDATE')`,
		role, column).Scan(&allowed); err != nil {
		t.Fatalf("read %s UPDATE on proposals.%s: %v", role, column, err)
	}
	return allowed
}

func m405TablePrivilege(t *testing.T, d *testdb.DB, role, privilege string) bool {
	t.Helper()
	var allowed bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1::name, 'public.proposals', $2::text)`,
		role, privilege).Scan(&allowed); err != nil {
		t.Fatalf("read %s %s on proposals: %v", role, privilege, err)
	}
	return allowed
}

func m405ViewPrivilege(t *testing.T, d *testdb.DB, role, view, privilege string) bool {
	t.Helper()
	var allowed bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1::name, $2::text, $3::text)`,
		role, "public."+view, privilege).Scan(&allowed); err != nil {
		t.Fatalf("read %s %s on %s: %v", role, privilege, view, err)
	}
	return allowed
}

// TestM405VerifierAppGrant proves the additive M4-05 grant through the real
// recon_app login: verification and pattern_verified become writable while
// every column owned by another task stays exactly as its migration left it.
func TestM405VerifierAppGrant(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	var count int
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(is_applied), false)
		   FROM goose_db_version
		  WHERE version_id = $1`, m405MigrationVersion).Scan(&count, &applied); err != nil {
		t.Fatalf("read M4-05 goose version: %v", err)
	}
	if count != 1 || !applied {
		t.Fatalf("M4-05 migration rows/applied = %d/%t, want 1/true", count, applied)
	}

	migration, err := fs.ReadFile(migrations.FS, "20261113090000_m4_05_verifier_grant.sql")
	if err != nil {
		t.Fatalf("read M4-05 migration: %v", err)
	}
	for _, want := range []string{
		"GRANT UPDATE (verification, pattern_verified) ON proposals TO recon_app;",
		"REVOKE UPDATE (verification, pattern_verified) ON proposals FROM recon_app;",
		"M4-03",
		"M4-04",
		"M4-11",
	} {
		if !strings.Contains(string(migration), want) {
			t.Errorf("M4-05 migration missing %q", want)
		}
	}

	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	proposalID := m2InsertProposal(t, d.Pool, caseID, runID, "m4-05-app-grant", "proposed")

	if _, err := app.Exec(ctx,
		`UPDATE proposals
		    SET verification = '{"schema_version":1,"reason":"no_pattern"}'::jsonb,
		        pattern_verified = 'fee_variance'
		  WHERE id = $1::uuid`, proposalID); err != nil {
		t.Fatalf("app verifier update: %v", err)
	}
	var verification, pattern string
	if err := app.QueryRow(ctx,
		`SELECT verification::text, pattern_verified FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&verification, &pattern); err != nil {
		t.Fatalf("app re-read verifier columns: %v", err)
	}
	if !strings.Contains(verification, `"schema_version"`) || pattern != "fee_variance" {
		t.Fatalf("app-updated verification/pattern = %q/%q", verification, pattern)
	}

	secondCase := m2CreateCase(t, d)
	secondRun := m2CreateAgentRun(t, d, secondCase)
	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{"payload", `UPDATE proposals SET payload = '{}'::jsonb WHERE id = $1::uuid`, []any{proposalID}},
		{"rationale", `UPDATE proposals SET rationale = 'tampered' WHERE id = $1::uuid`, []any{proposalID}},
		{"evidence", `UPDATE proposals SET evidence = '[]'::jsonb WHERE id = $1::uuid`, []any{proposalID}},
		{"state_hash", `UPDATE proposals SET state_hash = 'tampered' WHERE id = $1::uuid`, []any{proposalID}},
		{"expires_at", `UPDATE proposals SET expires_at = now() WHERE id = $1::uuid`, []any{proposalID}},
		{"type", `UPDATE proposals SET type = 'adjustment' WHERE id = $1::uuid`, []any{proposalID}},
		{"confidence", `UPDATE proposals SET confidence = 0.5 WHERE id = $1::uuid`, []any{proposalID}},
		{"case_id", `UPDATE proposals SET case_id = $2::uuid WHERE id = $1::uuid`, []any{proposalID, secondCase}},
		{"agent_run_id", `UPDATE proposals SET agent_run_id = $2::uuid WHERE id = $1::uuid`, []any{proposalID, secondRun}},
		{"idempotency_key", `UPDATE proposals SET idempotency_key = 'm4-05-other-key' WHERE id = $1::uuid`, []any{proposalID}},
		{"created_at", `UPDATE proposals SET created_at = now() WHERE id = $1::uuid`, []any{proposalID}},
		{"supersedes_proposal_id", `UPDATE proposals SET supersedes_proposal_id = NULL WHERE id = $1::uuid`, []any{proposalID}},
	} {
		tc := tc
		t.Run("deny_update_"+tc.name, func(t *testing.T) {
			if _, err := app.Exec(ctx, tc.sql, tc.args...); err == nil {
				t.Fatalf("app updated proposals.%s, want permission denied", tc.name)
			} else {
				requirePgError(t, err, sqlStateInsufficientPrivilege)
			}
		})
	}

	if _, err := app.Exec(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'app insert', 0.5,
		         'proposed', 'state-hash', 'm4-05-app-insert', now())`,
		secondCase, secondRun); err == nil {
		t.Fatal("app INSERT into proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := app.Exec(ctx,
		`DELETE FROM proposals WHERE id = $1::uuid`, proposalID); err == nil {
		t.Fatal("app DELETE from proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	if m405TablePrivilege(t, d, db.RoleApp, "UPDATE") {
		t.Fatal("app has table-level UPDATE on proposals, want column-level only")
	}
	// verification/pattern_verified are M4-05; status/version remain M4-03 and
	// risk_tier/policy_decision remain M4-04.
	for _, column := range []string{
		"verification", "pattern_verified", "status", "version",
		"risk_tier", "policy_decision",
	} {
		if !m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Errorf("app lacks UPDATE on proposals.%s, want granted", column)
		}
	}
	for _, column := range []string{
		"payload", "rationale", "evidence", "state_hash", "expires_at",
		"type", "confidence", "case_id", "agent_run_id", "idempotency_key",
		"created_at", "supersedes_proposal_id",
	} {
		if m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Errorf("app has UPDATE on proposals.%s, want denied", column)
		}
	}

	// Executor unchanged: M4-01's SELECT + status/version only.
	for _, column := range []string{"verification", "pattern_verified", "risk_tier", "policy_decision"} {
		if m405ColumnUpdate(t, d, db.RoleExecutor, column) {
			t.Errorf("executor has UPDATE on proposals.%s, want denied", column)
		}
	}
	for _, column := range []string{"status", "version"} {
		if !m405ColumnUpdate(t, d, db.RoleExecutor, column) {
			t.Errorf("executor lost UPDATE on proposals.%s", column)
		}
	}
	if !m405TablePrivilege(t, d, db.RoleExecutor, "SELECT") {
		t.Error("executor lost SELECT on proposals")
	}
	if m405TablePrivilege(t, d, db.RoleExecutor, "UPDATE") {
		t.Error("executor gained table-level UPDATE on proposals")
	}

	// Agent unchanged: table INSERT and idempotency-key SELECT only.
	for _, column := range []string{"verification", "pattern_verified", "status", "version"} {
		if m405ColumnUpdate(t, d, db.RoleAgentRO, column) {
			t.Errorf("agent has UPDATE on proposals.%s, want denied", column)
		}
	}
	if m405TablePrivilege(t, d, db.RoleAgentRO, "UPDATE") {
		t.Error("agent gained table-level UPDATE on proposals")
	}
	if !m405TablePrivilege(t, d, db.RoleAgentRO, "INSERT") {
		t.Error("agent lost INSERT on proposals")
	}
	if m405TablePrivilege(t, d, db.RoleAgentRO, "DELETE") {
		t.Error("agent gained DELETE on proposals")
	}

	// Audit role unchanged: no proposal privileges at all.
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		if m405TablePrivilege(t, d, db.RoleAuditRO, privilege) {
			t.Errorf("audit gained %s on proposals", privilege)
		}
	}
	for _, column := range []string{"verification", "pattern_verified", "status", "version"} {
		if m405ColumnUpdate(t, d, db.RoleAuditRO, column) {
			t.Errorf("audit has UPDATE on proposals.%s, want denied", column)
		}
	}
}

// TestM405VerifierReadViewsGrant proves the additive M4-05 read grant under the
// real recon_app login: all seven verifier views are readable, none is writable,
// and the executor/agent/audit matrix is unchanged.
func TestM405VerifierReadViewsGrant(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	var count int
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(is_applied), false)
		   FROM goose_db_version
		  WHERE version_id = $1`, m405ReadGrantMigrationVersion).Scan(&count, &applied); err != nil {
		t.Fatalf("read M4-05 read-grant goose version: %v", err)
	}
	if count != 1 || !applied {
		t.Fatalf("M4-05 read-grant migration rows/applied = %d/%t, want 1/true", count, applied)
	}

	migration, err := fs.ReadFile(migrations.FS, "20261113091000_m4_05_verifier_read_grant.sql")
	if err != nil {
		t.Fatalf("read M4-05 read-grant migration: %v", err)
	}
	checks := append([]string{
		"GRANT SELECT ON",
		"TO recon_app;",
		"REVOKE SELECT ON",
		"FROM recon_app;",
		"read surface, not a write expansion",
	}, m405VerifierViews...)
	for _, want := range checks {
		if !strings.Contains(string(migration), want) {
			t.Errorf("M4-05 read-grant migration missing %q", want)
		}
	}

	for _, view := range m405VerifierViews {
		view := view
		t.Run("app_select_"+view, func(t *testing.T) {
			if !m405ViewPrivilege(t, d, db.RoleApp, view, "SELECT") {
				t.Errorf("app lacks SELECT on %s", view)
			}
			var rows int
			if err := app.QueryRow(ctx, "SELECT count(*) FROM "+view).Scan(&rows); err != nil {
				t.Errorf("app SELECT count(*) FROM %s: %v", view, err)
			}
		})
		t.Run("app_no_write_"+view, func(t *testing.T) {
			for _, privilege := range []string{"INSERT", "UPDATE", "DELETE"} {
				if m405ViewPrivilege(t, d, db.RoleApp, view, privilege) {
					t.Errorf("app has %s on %s, want denied", privilege, view)
				}
			}
		})
	}

	// Exactly the seven verifier views: the rest of the M2 agent surface stays
	// agent-only.
	for _, view := range m405RemainingAgentViews {
		if m405ViewPrivilege(t, d, db.RoleApp, view, "SELECT") {
			t.Errorf("app gained SELECT on %s, which is outside the verifier read surface", view)
		}
	}

	// Executor and audit keep no view privileges; the agent role keeps its M2
	// SELECT-only grant.
	for _, view := range m405VerifierViews {
		for _, role := range []string{db.RoleExecutor, db.RoleAuditRO} {
			for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
				if m405ViewPrivilege(t, d, role, view, privilege) {
					t.Errorf("%s gained %s on %s", role, privilege, view)
				}
			}
		}
		if !m405ViewPrivilege(t, d, db.RoleAgentRO, view, "SELECT") {
			t.Errorf("agent lost SELECT on %s", view)
		}
		for _, privilege := range []string{"INSERT", "UPDATE", "DELETE"} {
			if m405ViewPrivilege(t, d, db.RoleAgentRO, view, privilege) {
				t.Errorf("agent gained %s on %s", privilege, view)
			}
		}
	}
}

// TestM405VerifierGrantDownReversesOnlyOwnGrants applies the embedded Down
// migrations on an isolated scratch clone and proves each one revokes exactly
// its own grants: the read migration only the seven view SELECTs, then the
// column migration only verification/pattern_verified.
func TestM405VerifierGrantDownReversesOnlyOwnGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	for _, column := range []string{"verification", "pattern_verified"} {
		if !m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Fatalf("app lacks pre-Down UPDATE on proposals.%s", column)
		}
	}
	for _, view := range m405VerifierViews {
		if !m405ViewPrivilege(t, d, db.RoleApp, view, "SELECT") {
			t.Fatalf("app lacks pre-Down SELECT on %s", view)
		}
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

	// Down 1: the M4-05 read grant (20261113091000) only.
	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("apply M4-05 read-grant Down: %v", err)
	}

	var version int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read version after read-grant Down: %v", err)
	}
	if version != m405MigrationVersion {
		t.Fatalf("version after read-grant Down = %d, want %d", version, m405MigrationVersion)
	}
	for _, view := range m405VerifierViews {
		if m405ViewPrivilege(t, d, db.RoleApp, view, "SELECT") {
			t.Errorf("app still has SELECT on %s after read-grant Down", view)
		}
		if !m405ViewPrivilege(t, d, db.RoleAgentRO, view, "SELECT") {
			t.Errorf("read-grant Down revoked agent SELECT on %s", view)
		}
	}
	for _, column := range []string{
		"verification", "pattern_verified", "status", "version",
		"risk_tier", "policy_decision",
	} {
		if !m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Errorf("read-grant Down revoked proposals.%s, which belongs to an earlier migration", column)
		}
	}

	// Down 2: the M4-05 column grant (20261113090000) only.
	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("apply M4-05 column-grant Down: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close scratch database: %v", err)
	}

	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read version after column-grant Down: %v", err)
	}
	if version != 20261112130000 {
		t.Fatalf("version after column-grant Down = %d, want 20261112130000", version)
	}

	for _, column := range []string{"verification", "pattern_verified"} {
		if m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Errorf("app still has UPDATE on proposals.%s after Down", column)
		}
	}
	for _, column := range []string{"status", "version", "risk_tier", "policy_decision"} {
		if !m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Errorf("Down revoked proposals.%s, which belongs to an earlier migration", column)
		}
	}
	for _, column := range []string{"payload", "rationale", "evidence", "state_hash", "expires_at"} {
		if m405ColumnUpdate(t, d, db.RoleApp, column) {
			t.Errorf("app has UPDATE on proposals.%s after Down, want denied", column)
		}
	}
	if m405TablePrivilege(t, d, db.RoleApp, "UPDATE") {
		t.Error("app has table-level UPDATE after Down")
	}
	for _, view := range m405VerifierViews {
		if m405ViewPrivilege(t, d, db.RoleApp, view, "SELECT") {
			t.Errorf("app regained SELECT on %s after the column-grant Down", view)
		}
	}
}
