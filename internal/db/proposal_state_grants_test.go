//go:build integration

// covers: FIN-24
package db_test

import (
	"context"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestProposalStateAppGrant proves the M4-03 grant through the real ephemeral
// recon_app login: it can lock a proposal and update only status/version, while
// rationale, payload, state_hash, expires_at, INSERT, and DELETE stay denied.
func TestProposalStateAppGrant(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	var count int
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(is_applied), false)
		   FROM goose_db_version
		  WHERE version_id = 20261112120000`).Scan(&count, &applied); err != nil {
		t.Fatalf("read M4-03 goose version: %v", err)
	}
	if count != 1 || !applied {
		t.Fatalf("M4-03 migration rows/applied = %d/%t, want 1/true", count, applied)
	}

	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	proposalID := m2InsertProposal(t, d.Pool, caseID, runID, "m4-03-app-grant", "proposed")

	// SELECT ... FOR UPDATE needs SELECT plus UPDATE on at least one column.
	var locked string
	if err := app.QueryRow(ctx,
		`SELECT id::text FROM proposals WHERE id = $1::uuid FOR UPDATE`,
		proposalID).Scan(&locked); err != nil {
		t.Fatalf("app SELECT ... FOR UPDATE: %v", err)
	}
	if locked != proposalID {
		t.Fatalf("locked proposal = %q, want %q", locked, proposalID)
	}

	if _, err := app.Exec(ctx,
		`UPDATE proposals SET status = 'approved', version = version + 1
		  WHERE id = $1::uuid`, proposalID); err != nil {
		t.Fatalf("app status/version update: %v", err)
	}
	var status string
	var version int
	if err := app.QueryRow(ctx,
		`SELECT status, version FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&status, &version); err != nil {
		t.Fatalf("app re-read proposal: %v", err)
	}
	if status != "approved" || version != 1 {
		t.Fatalf("app-updated proposal = %s/version %d, want approved/1", status, version)
	}

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"rationale", `UPDATE proposals SET rationale = 'tampered' WHERE id = $1::uuid`},
		{"payload", `UPDATE proposals SET payload = '{}'::jsonb WHERE id = $1::uuid`},
		{"state_hash", `UPDATE proposals SET state_hash = 'tampered' WHERE id = $1::uuid`},
		{"expires_at", `UPDATE proposals SET expires_at = now() WHERE id = $1::uuid`},
	} {
		tc := tc
		t.Run("deny_update_"+tc.name, func(t *testing.T) {
			if _, err := app.Exec(ctx, tc.sql, proposalID); err == nil {
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
		         'proposed', 'state-hash', 'm4-03-app-insert', now())`,
		caseID, runID); err == nil {
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

	// Table-level UPDATE stays false; only the two state columns are granted.
	var tableUpdate bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_table_privilege($1::name, 'public.proposals', 'UPDATE')`,
		db.RoleApp).Scan(&tableUpdate); err != nil {
		t.Fatalf("read app table-level UPDATE: %v", err)
	}
	if tableUpdate {
		t.Fatal("app has table-level UPDATE on proposals, want column-level only")
	}
	for _, column := range []string{"status", "version"} {
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
	for _, column := range []string{"payload", "rationale", "evidence", "expires_at"} {
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

	// The app keeps SELECT, and the executor's existing proposal surface is
	// unchanged.
	var appSelect, executorSelect, executorTableUpdate bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_table_privilege($1::name, 'public.proposals', 'SELECT')`,
		db.RoleApp).Scan(&appSelect); err != nil {
		t.Fatalf("read app SELECT: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_table_privilege($1::name, 'public.proposals', 'SELECT')`,
		db.RoleExecutor).Scan(&executorSelect); err != nil {
		t.Fatalf("read executor SELECT: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_table_privilege($1::name, 'public.proposals', 'UPDATE')`,
		db.RoleExecutor).Scan(&executorTableUpdate); err != nil {
		t.Fatalf("read executor table-level UPDATE: %v", err)
	}
	if !appSelect || !executorSelect {
		t.Fatalf("proposal SELECT grants = app %t executor %t, want true/true",
			appSelect, executorSelect)
	}
	if executorTableUpdate {
		t.Fatal("executor gained table-level UPDATE, want column-level only")
	}
	for _, column := range []string{"status", "version"} {
		var allowed bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT has_column_privilege($1::name, 'public.proposals', $2::text, 'UPDATE')`,
			db.RoleExecutor, column).Scan(&allowed); err != nil {
			t.Fatalf("read executor UPDATE on proposals.%s: %v", column, err)
		}
		if !allowed {
			t.Errorf("executor lost UPDATE on proposals.%s", column)
		}
	}
}
