//go:build integration

// covers: FIN-13, FIN-34
package db_test

import (
	"context"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestM404PolicyDecisionAppGrant proves the additive M4-04 grant through the
// real recon_app login: risk_tier and policy_decision are writable, M4-03's
// status/version stay writable, and every other proposal column, INSERT, and
// DELETE stay denied.
func TestM404PolicyDecisionAppGrant(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	var count int
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(is_applied), false)
		   FROM goose_db_version
		  WHERE version_id = 20261112130000`).Scan(&count, &applied); err != nil {
		t.Fatalf("read M4-04 goose version: %v", err)
	}
	if count != 1 || !applied {
		t.Fatalf("M4-04 migration rows/applied = %d/%t, want 1/true", count, applied)
	}

	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	proposalID := m2InsertProposal(t, d.Pool, caseID, runID, "m4-04-app-grant", "proposed")

	// The app writes the decision columns and the M4-03 state columns in one
	// statement; both grants compose.
	if _, err := app.Exec(ctx,
		`UPDATE proposals
		    SET risk_tier = 'T1',
		        policy_decision = '{"schema_version":1,"tier":"T1","rule":"verifier_missing","reasons":["verifier_missing"],"config_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}'::jsonb,
		        status = 'pending_approval',
		        version = version + 1
		  WHERE id = $1::uuid`, proposalID); err != nil {
		t.Fatalf("app decision/state update: %v", err)
	}
	var tier, decision, status string
	var version int
	if err := app.QueryRow(ctx,
		`SELECT risk_tier, policy_decision::text, status, version
		   FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&tier, &decision, &status, &version); err != nil {
		t.Fatalf("app re-read proposal: %v", err)
	}
	if tier != "T1" || status != "pending_approval" || version != 1 {
		t.Fatalf("app-updated proposal = %s/%s/version %d, want T1/pending_approval/1", tier, status, version)
	}
	if decision == "" {
		t.Fatal("policy_decision was not written")
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
		{"verification", `UPDATE proposals SET verification = '{}'::jsonb WHERE id = $1::uuid`, []any{proposalID}},
		{"pattern_verified", `UPDATE proposals SET pattern_verified = 'fee_variance' WHERE id = $1::uuid`, []any{proposalID}},
		{"type", `UPDATE proposals SET type = 'adjustment' WHERE id = $1::uuid`, []any{proposalID}},
		{"confidence", `UPDATE proposals SET confidence = 0.5 WHERE id = $1::uuid`, []any{proposalID}},
		{"case_id", `UPDATE proposals SET case_id = $2::uuid WHERE id = $1::uuid`, []any{proposalID, secondCase}},
		{"agent_run_id", `UPDATE proposals SET agent_run_id = $2::uuid WHERE id = $1::uuid`, []any{proposalID, secondRun}},
		{"idempotency_key", `UPDATE proposals SET idempotency_key = 'm4-04-other-key' WHERE id = $1::uuid`, []any{proposalID}},
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
		         'proposed', 'state-hash', 'm4-04-app-insert', now())`,
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

	var tableUpdate bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_table_privilege($1::name, 'public.proposals', 'UPDATE')`,
		db.RoleApp).Scan(&tableUpdate); err != nil {
		t.Fatalf("read app table-level UPDATE: %v", err)
	}
	if tableUpdate {
		t.Fatal("app has table-level UPDATE on proposals, want column-level only")
	}
	for _, column := range []string{"risk_tier", "policy_decision", "status", "version"} {
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

	// The executor keeps the M4-01/M4-03 surface and gains neither policy
	// column; the read-only roles gain nothing.
	for _, role := range []string{db.RoleExecutor, db.RoleAgentRO, db.RoleAuditRO} {
		for _, column := range []string{"risk_tier", "policy_decision"} {
			var allowed bool
			if err := d.Pool.QueryRow(ctx,
				`SELECT has_column_privilege($1::name, 'public.proposals', $2::text, 'UPDATE')`,
				role, column).Scan(&allowed); err != nil {
				t.Fatalf("read %s UPDATE on proposals.%s: %v", role, column, err)
			}
			if allowed {
				t.Errorf("%s has UPDATE on proposals.%s, want denied", role, column)
			}
		}
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
	var executorTableUpdate bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_table_privilege($1::name, 'public.proposals', 'UPDATE')`,
		db.RoleExecutor).Scan(&executorTableUpdate); err != nil {
		t.Fatalf("read executor table-level UPDATE: %v", err)
	}
	if executorTableUpdate {
		t.Fatal("executor gained table-level UPDATE, want column-level only")
	}
}
