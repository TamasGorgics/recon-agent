//go:build integration

// covers: FIN-11
package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// m2AgentViews is the complete read surface granted to recon_agent_ro. The
// view names and columns are the M2-01 contract from the run spec section 3.2.
var m2AgentViews = []string{
	"v_agent_accounts",
	"v_agent_scopes",
	"v_agent_cases",
	"v_agent_case_breaks",
	"v_agent_breaks",
	"v_agent_ledger_txns",
	"v_agent_ledger_entries",
	"v_agent_external_records",
	"v_agent_settlement_batches",
	"v_agent_statement_balances",
	"v_agent_fx_rates",
	"v_agent_fee_schedules",
	"v_agent_history",
}

// m2NewTables are the five M2-01 tables. recon_app gets the write path for the
// run/tool trace; recon_agent_ro gets INSERT on proposals plus the column-level
// SELECT on idempotency_key required by ON CONFLICT inference (Q-22).
var m2NewTables = []string{
	"agent_runs",
	"tool_calls",
	"llm_calls",
	"proposals",
	"system_switches",
}

// m2AgentDeniedTables is every M1 and M2 base table the agent role must not be
// able to read directly. The views are the only read path (spec section 3.3).
//
// proposals is deliberately absent: Q-22 grants column-level SELECT on
// idempotency_key only, which also makes count(*) succeed. TestM2AgentProposalGrants
// proves the per-column denial (payload/evidence/id/status and SELECT *).
var m2AgentDeniedTables = []string{
	"currencies",
	"fx_rates",
	"fee_schedules",
	"periods",
	"accounts",
	"ledger_txns",
	"ledger_entries",
	"audit_log",
	"audit_chain_head",
	"reconciliation_scopes",
	"settlement_batches",
	"statement_balances",
	"external_records",
	"match_runs",
	"matches",
	"match_legs",
	"breaks",
	"cases",
	"case_breaks",
	"agent_runs",
	"tool_calls",
	"llm_calls",
	"system_switches",
}

func TestM2AgentSchemaObjects(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	for _, table := range m2NewTables {
		t.Run("table/"+table, func(t *testing.T) {
			var exists bool
			if err := d.Pool.QueryRow(ctx,
				`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
				t.Fatalf("resolve table %s: %v", table, err)
			}
			if !exists {
				t.Fatalf("table %s is missing", table)
			}
		})
	}

	for _, view := range m2AgentViews {
		t.Run("view/"+view, func(t *testing.T) {
			var exists bool
			if err := d.Pool.QueryRow(ctx,
				`SELECT to_regclass('public.' || $1) IS NOT NULL`, view).Scan(&exists); err != nil {
				t.Fatalf("resolve view %s: %v", view, err)
			}
			if !exists {
				t.Fatalf("view %s is missing", view)
			}
		})
	}

	var partialIndex string
	if err := d.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND indexname = 'proposals_one_active_per_case'`).
		Scan(&partialIndex); err != nil {
		t.Fatalf("read proposals_one_active_per_case definition: %v", err)
	}
	if !strings.Contains(partialIndex, "UNIQUE INDEX") || !strings.Contains(partialIndex, "WHERE") {
		t.Fatalf("proposals_one_active_per_case is not a partial unique index: %s", partialIndex)
	}
	for _, status := range []string{"proposed", "pending_approval", "approved", "auto_approved"} {
		if !strings.Contains(partialIndex, status) {
			t.Fatalf("proposals_one_active_per_case predicate does not contain %q: %s", status, partialIndex)
		}
	}
	for _, status := range []string{"applied", "apply_failed", "rejected", "expired", "superseded"} {
		if strings.Contains(partialIndex, status) {
			t.Fatalf("proposals_one_active_per_case predicate contains inactive status %q: %s", status, partialIndex)
		}
	}

	var searchIndex string
	if err := d.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND indexname = 'external_records_agent_search_idx'`).
		Scan(&searchIndex); err != nil {
		t.Fatalf("read external_records_agent_search_idx definition: %v", err)
	}
	for _, column := range []string{"currency", "amount_minor", "value_date"} {
		if !strings.Contains(searchIndex, column) {
			t.Fatalf("external_records_agent_search_idx does not contain %q: %s", column, searchIndex)
		}
	}

	// recon_agent_ro may read every view. The test database has no synthetic
	// world, so count(*) is a permission check, not a data assertion.
	agent := openRolePool(t, d, db.RoleAgentRO)
	for _, view := range m2AgentViews {
		t.Run("agent-select/"+view, func(t *testing.T) {
			var count int
			if err := agent.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(&count); err != nil {
				t.Fatalf("agent role select %s: %v", view, err)
			}
		})
	}
}

// TestM2AgentCannotReadBaseTables proves the owner-rights view boundary:
// recon_agent_ro reads the v_agent_* views but has no table-level base-table
// SELECT grant, including the audit tables and its own agent trace tables
// (spec section 3.3; spec section 3.2 invariant 4 / FIN-11 for the ledger half).
// proposals.idempotency_key is the one column-level exception (Q-22); its
// denial matrix lives in TestM2AgentProposalGrants.
func TestM2AgentCannotReadBaseTables(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	agent := openRolePool(t, d, db.RoleAgentRO)

	for _, table := range m2AgentDeniedTables {
		t.Run(table, func(t *testing.T) {
			_, err := agent.Exec(ctx, `SELECT count(*) FROM `+table)
			requirePgError(t, err, sqlStateInsufficientPrivilege)
		})
	}
}

// TestM2AgentProposalGrants proves Q-B and Q-22: the agent inserts
// client-generated proposals without RETURNING, may use
// ON CONFLICT (idempotency_key) DO NOTHING because it has the minimal
// column-level SELECT on idempotency_key needed for conflict inference, and
// still cannot read payload/evidence or any other proposal column, cannot use
// RETURNING, and cannot UPDATE/DELETE proposals or UPDATE system_switches.
func TestM2AgentProposalGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	agent := openRolePool(t, d, db.RoleAgentRO)

	// The conflict-target grant is column-level only. If it ever becomes a
	// table-level SELECT, payload/evidence would leak; pin the exact shape.
	var tableSelect, keySelect, payloadSelect bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT has_table_privilege($1::text, 'public.proposals', 'SELECT'),
		        has_column_privilege($1::text, 'public.proposals', 'idempotency_key', 'SELECT'),
		        has_column_privilege($1::text, 'public.proposals', 'payload', 'SELECT')`,
		db.RoleAgentRO).Scan(&tableSelect, &keySelect, &payloadSelect); err != nil {
		t.Fatalf("read agent proposal column privileges: %v", err)
	}
	if tableSelect {
		t.Fatal("recon_agent_ro has table-level SELECT on proposals, want none")
	}
	if !keySelect {
		t.Fatal("recon_agent_ro cannot SELECT idempotency_key, want the Q-22 conflict-inference grant")
	}
	if payloadSelect {
		t.Fatal("recon_agent_ro can SELECT payload, want column-level idempotency_key only")
	}

	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)

	const insertOnConflict = `
		INSERT INTO proposals
		    (case_id, agent_run_id, type, payload, rationale, confidence,
		     state_hash, idempotency_key, created_at)
		VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'evidence matches',
		        0.9000, 'state-hash', $3, now())
		ON CONFLICT (idempotency_key) DO NOTHING`

	tag, err := agent.Exec(ctx, insertOnConflict, caseID, runID, "agent-conflict-key")
	if err != nil {
		t.Fatalf("agent insert ON CONFLICT (idempotency_key) DO NOTHING: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("agent first insert rows affected = %d, want 1", tag.RowsAffected())
	}

	// Reading only the granted column proves the grant is usable, not a payload
	// read path.
	var gotKey string
	if err := agent.QueryRow(ctx,
		`SELECT idempotency_key FROM proposals WHERE idempotency_key = $1`,
		"agent-conflict-key").Scan(&gotKey); err != nil {
		t.Fatalf("agent SELECT idempotency_key: %v", err)
	}
	if gotKey != "agent-conflict-key" {
		t.Fatalf("agent SELECT idempotency_key = %q, want agent-conflict-key", gotKey)
	}

	var proposalID string
	if err := d.Pool.QueryRow(ctx,
		`SELECT id::text FROM proposals WHERE idempotency_key = 'agent-conflict-key'`).
		Scan(&proposalID); err != nil {
		t.Fatalf("read proposal inserted by agent: %v", err)
	}

	// A duplicate key in a fresh case conflicts only on idempotency_key; the
	// agent's ON CONFLICT is a zero-row no-op, not a 42501 and not a 23505.
	caseID2 := m2CreateCase(t, d)
	runID2 := m2CreateAgentRun(t, d, caseID2)
	tag, err = agent.Exec(ctx, insertOnConflict, caseID2, runID2, "agent-conflict-key")
	if err != nil {
		t.Fatalf("agent duplicate idempotency key insert: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("agent duplicate idempotency key rows affected = %d, want 0", tag.RowsAffected())
	}
	var keyCount int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM proposals WHERE idempotency_key = 'agent-conflict-key'`).
		Scan(&keyCount); err != nil {
		t.Fatalf("count duplicate idempotency key: %v", err)
	}
	if keyCount != 1 {
		t.Fatalf("idempotency key rows after duplicate = %d, want 1", keyCount)
	}

	// A second active proposal uses a different idempotency key, so the
	// partial unique index is not the conflict arbiter and 23505 must reach the
	// agent even with ON CONFLICT (idempotency_key) DO NOTHING.
	_, err = agent.Exec(ctx, insertOnConflict, caseID, runID, "agent-second-active")
	pgErr := requirePgError(t, err, "23505")
	if pgErr.ConstraintName != "proposals_one_active_per_case" {
		t.Fatalf("constraint = %q, want proposals_one_active_per_case", pgErr.ConstraintName)
	}

	// The agent has no SELECT on any other proposal column.
	for _, column := range []string{"id", "payload", "evidence", "status", "rationale"} {
		t.Run("agent-select-"+column, func(t *testing.T) {
			var value any
			err := agent.QueryRow(ctx,
				`SELECT `+column+` FROM proposals WHERE idempotency_key = 'agent-conflict-key'`).
				Scan(&value)
			requirePgError(t, err, sqlStateInsufficientPrivilege)
		})
	}
	_, err = agent.Exec(ctx, `SELECT * FROM proposals`)
	requirePgError(t, err, sqlStateInsufficientPrivilege)

	// RETURNING needs SELECT on the returned column, which the agent role does
	// not have. A fresh case avoids a partial-index conflict masking the grant
	// error.
	caseID3 := m2CreateCase(t, d)
	runID3 := m2CreateAgentRun(t, d, caseID3)
	_, err = agent.Exec(ctx,
		strings.TrimSpace(insertOnConflict)+` RETURNING id`,
		caseID3, runID3, "agent-returning")
	requirePgError(t, err, sqlStateInsufficientPrivilege)

	if _, err := agent.Exec(ctx,
		`UPDATE proposals SET rationale = 'tampered' WHERE id = $1::uuid`, proposalID); err == nil {
		t.Fatal("agent UPDATE proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := agent.Exec(ctx,
		`DELETE FROM proposals WHERE id = $1::uuid`, proposalID); err == nil {
		t.Fatal("agent DELETE proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := agent.Exec(ctx,
		`UPDATE system_switches SET enabled = false WHERE name = 'agent_enabled'`); err == nil {
		t.Fatal("agent UPDATE system_switches succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
}

// TestM2AppRolePrivileges asserts the recon_app grant matrix: it records the
// agent run/tool/LLM trace, reads proposals, and reads/updates switches, but
// cannot mutate the trace after the fact or update proposals.
func TestM2AppRolePrivileges(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	type privilege struct {
		table                                          string
		selectWant, insertWant, updateWant, deleteWant bool
	}
	matrix := []privilege{
		{"agent_runs", true, true, true, false},
		{"tool_calls", true, true, false, false},
		{"llm_calls", true, true, false, false},
		{"proposals", true, false, false, false},
		{"system_switches", true, false, true, false},
	}
	for _, row := range matrix {
		t.Run(row.table, func(t *testing.T) {
			var canSelect, canInsert, canUpdate, canDelete bool
			if err := d.Pool.QueryRow(ctx,
				`SELECT has_table_privilege($1::text, $2::text, 'SELECT'),
				        has_table_privilege($1::text, $2::text, 'INSERT'),
				        has_table_privilege($1::text, $2::text, 'UPDATE'),
				        has_table_privilege($1::text, $2::text, 'DELETE')`,
				db.RoleApp, row.table).Scan(&canSelect, &canInsert, &canUpdate, &canDelete); err != nil {
				t.Fatalf("read privileges: %v", err)
			}
			if canSelect != row.selectWant || canInsert != row.insertWant ||
				canUpdate != row.updateWant || canDelete != row.deleteWant {
				t.Fatalf("%s privileges = SELECT %t INSERT %t UPDATE %t DELETE %t, want %t/%t/%t/%t",
					row.table, canSelect, canInsert, canUpdate, canDelete,
					row.selectWant, row.insertWant, row.updateWant, row.deleteWant)
			}
		})
	}

	caseID := m2CreateCase(t, d)

	var runID string
	if err := app.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		t.Fatalf("app insert agent_runs: %v", err)
	}
	if _, err := app.Exec(ctx,
		`UPDATE agent_runs SET ended_at = now(), outcome = 'escalated' WHERE id = $1::uuid`,
		runID); err != nil {
		t.Fatalf("app update agent_runs: %v", err)
	}
	var outcome string
	if err := app.QueryRow(ctx,
		`SELECT outcome FROM agent_runs WHERE id = $1::uuid`, runID).Scan(&outcome); err != nil {
		t.Fatalf("app select agent_runs: %v", err)
	}
	if outcome != "escalated" {
		t.Fatalf("agent_runs.outcome = %q, want escalated", outcome)
	}

	var toolCallID string
	if err := app.QueryRow(ctx,
		`INSERT INTO tool_calls
		     (agent_run_id, seq, tool_name, args, result, result_hash, started_at, duration_ms)
		 VALUES ($1::uuid, 1, 'get_case', '{}'::jsonb, '{"data":{}}'::jsonb,
		         'hash-tool', now(), 3)
		 RETURNING id::text`, runID).Scan(&toolCallID); err != nil {
		t.Fatalf("app insert tool_calls: %v", err)
	}
	var llmCallID string
	if err := app.QueryRow(ctx,
		`INSERT INTO llm_calls
		     (agent_run_id, seq, model, request_hash, response, usage, started_at, duration_ms)
		 VALUES ($1::uuid, 1, 'scripted', 'hash-request', '{"stop_reason":"end_turn"}'::jsonb,
		         '{}'::jsonb, now(), 5)
		 RETURNING id::text`, runID).Scan(&llmCallID); err != nil {
		t.Fatalf("app insert llm_calls: %v", err)
	}

	proposalID := m2InsertProposal(t, d.Pool, caseID, runID, "app-read-proposal", "proposed")
	var count int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE id = $1::uuid`, proposalID).Scan(&count); err != nil {
		t.Fatalf("app select proposals: %v", err)
	}
	if count != 1 {
		t.Fatalf("app proposals count = %d, want 1", count)
	}

	if _, err := app.Exec(ctx,
		`UPDATE proposals SET rationale = 'tampered' WHERE id = $1::uuid`, proposalID); err == nil {
		t.Fatal("app UPDATE proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	denied := []struct {
		name string
		sql  string
		args []any
	}{
		{name: "update tool_calls", sql: `UPDATE tool_calls SET tool_name = 'tampered' WHERE id = $1::uuid`, args: []any{toolCallID}},
		{name: "delete tool_calls", sql: `DELETE FROM tool_calls WHERE id = $1::uuid`, args: []any{toolCallID}},
		{name: "update llm_calls", sql: `UPDATE llm_calls SET model = 'tampered' WHERE id = $1::uuid`, args: []any{llmCallID}},
		{name: "delete llm_calls", sql: `DELETE FROM llm_calls WHERE id = $1::uuid`, args: []any{llmCallID}},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := app.Exec(ctx, tc.sql, tc.args...); err == nil {
				t.Fatalf("%s succeeded, want permission denied", tc.name)
			} else {
				requirePgError(t, err, sqlStateInsufficientPrivilege)
			}
		})
	}

	if _, err := app.Exec(ctx,
		`UPDATE system_switches SET reason = 'app-test', updated_by = 'system:test'
		  WHERE name = 'agent_enabled'`); err != nil {
		t.Fatalf("app update system_switches: %v", err)
	}
	var enabled bool
	if err := app.QueryRow(ctx,
		`SELECT enabled FROM system_switches WHERE name = 'agent_enabled'`).Scan(&enabled); err != nil {
		t.Fatalf("app select system_switches: %v", err)
	}
	if !enabled {
		t.Fatal("system_switches.agent_enabled = false, want true")
	}
}

// TestM2SwitchSeeds pins the exact two switch rows and proves the seed is
// idempotent under ON CONFLICT (name) DO NOTHING.
func TestM2SwitchSeeds(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	type seed struct {
		enabled bool
		reason  string
	}
	seeds := map[string]seed{
		"agent_enabled":      {enabled: true, reason: "seed"},
		"auto_apply_enabled": {enabled: false, reason: "seed"},
	}

	rows, err := d.Pool.Query(ctx,
		`SELECT name, enabled, reason, updated_by FROM system_switches ORDER BY name`)
	if err != nil {
		t.Fatalf("select system_switches: %v", err)
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var name, reason, updatedBy string
		var enabled bool
		if err := rows.Scan(&name, &enabled, &reason, &updatedBy); err != nil {
			t.Fatalf("scan system_switches: %v", err)
		}
		want, ok := seeds[name]
		if !ok {
			t.Fatalf("unexpected switch seed %q", name)
		}
		if enabled != want.enabled || reason != want.reason {
			t.Fatalf("%s = enabled %t reason %q, want %t/%q", name, enabled, reason, want.enabled, want.reason)
		}
		if updatedBy != "system:migration" {
			t.Fatalf("%s.updated_by = %q, want system:migration", name, updatedBy)
		}
		seen[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate system_switches: %v", err)
	}
	if len(seen) != len(seeds) {
		t.Fatalf("switch seeds = %v, want exactly %v", seen, seeds)
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO system_switches (name, enabled, reason, updated_by, updated_at)
		 VALUES ('agent_enabled', true, 'seed', 'system:migration', now())
		 ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("re-run agent_enabled seed: %v", err)
	}
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM system_switches`).Scan(&count); err != nil {
		t.Fatalf("count system_switches: %v", err)
	}
	if count != 2 {
		t.Fatalf("system_switches count after re-seed = %d, want 2", count)
	}
}

// TestM2ApproversAbsent proves M2-01 does not create the M4 approvers table and
// the agent role has no privilege if a later migration does create it.
func TestM2ApproversAbsent(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	var exists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT to_regclass('public.approvers') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("resolve approvers: %v", err)
	}
	var grants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_table_grants
		  WHERE grantee = $1 AND table_schema = 'public' AND table_name = 'approvers'`,
		db.RoleAgentRO).Scan(&grants); err != nil {
		t.Fatalf("count approvers grants: %v", err)
	}
	if grants != 0 {
		t.Fatalf("recon_agent_ro has %d recorded approvers grant(s), want 0", grants)
	}
	if !exists {
		return
	}
	// Future-proof: M4-01 may create approvers. The M2-01 contract is "no agent
	// privilege", not "table absent forever".
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		var allowed bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT has_table_privilege($1::text, 'public.approvers', $2::text)`,
			db.RoleAgentRO, privilege).Scan(&allowed); err != nil {
			t.Fatalf("read approvers %s privilege: %v", privilege, err)
		}
		if allowed {
			t.Fatalf("recon_agent_ro has %s on approvers, want none", privilege)
		}
	}
}

// TestM2ProposalActiveIndex proves spec section 3.2 invariant 7: one active
// proposal per case, a superseded/terminal proposal allows a new active one,
// and a duplicate idempotency key with ON CONFLICT DO NOTHING is a no-op (Q-B).
//
// covers: spec §3.2 invariant 7 (no FIN ID)
func TestM2ProposalActiveIndex(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	firstID := m2InsertProposal(t, d.Pool, caseID, runID, "active-key-1", "proposed")

	_, err := d.Pool.Exec(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'second', 0.5000,
		         'proposed', 'state-hash-2', 'active-key-2', now())`,
		caseID, runID)
	pgErr := requirePgError(t, err, "23505")
	if pgErr.ConstraintName != "proposals_one_active_per_case" {
		t.Fatalf("constraint = %q, want proposals_one_active_per_case", pgErr.ConstraintName)
	}

	if _, err := d.Pool.Exec(ctx,
		`UPDATE proposals SET status = 'superseded' WHERE id = $1::uuid`, firstID); err != nil {
		t.Fatalf("supersede first proposal: %v", err)
	}
	m2InsertProposal(t, d.Pool, caseID, runID, "active-key-3", "pending_approval")

	// A duplicate idempotency key in a different case conflicts only on the
	// idempotency_key unique constraint. ON CONFLICT DO NOTHING must report
	// zero rows and leave the case free of a second active proposal. Run this
	// path as the real agent login: the owner pool hid PostgreSQL's
	// conflict-target SELECT requirement (Q-22).
	caseID2 := m2CreateCase(t, d)
	runID2 := m2CreateAgentRun(t, d, caseID2)
	agent := openRolePool(t, d, db.RoleAgentRO)
	tag, err := agent.Exec(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'duplicate key', 0.5000,
		         'proposed', 'state-hash-4', 'active-key-1', now())
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		caseID2, runID2)
	if err != nil {
		t.Fatalf("duplicate idempotency key insert: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("duplicate idempotency key rows affected = %d, want 0", tag.RowsAffected())
	}
}

// TestM2ExecutorAuditNoGrants pins that M2-01 adds no privileges to the
// executor or audit roles (spec section 3.3).
func TestM2ExecutorAuditNoGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	objects := append(append([]string{}, m2NewTables...), m2AgentViews...)
	for _, role := range []string{db.RoleExecutor, db.RoleAuditRO} {
		for _, object := range objects {
			t.Run(role+"/"+object, func(t *testing.T) {
				for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
					var allowed bool
					if err := d.Pool.QueryRow(ctx,
						`SELECT has_table_privilege($1::text, $2::text, $3::text)`,
						role, object, privilege).Scan(&allowed); err != nil {
						t.Fatalf("read %s %s privilege: %v", role, object, err)
					}
					if allowed {
						t.Fatalf("%s has %s on %s, want none", role, privilege, object)
					}
				}
			})
		}
	}
}

// TestM2InjectionFlagColumns pins the M2-07 schema delta (Q-23): both trace
// tables carry a non-null JSONB array defaulting to [], the array check rejects
// objects, the app role can write the trace columns, and the agent role gains
// no read access.
func TestM2InjectionFlagColumns(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	for _, table := range []string{"agent_runs", "tool_calls"} {
		t.Run(table, func(t *testing.T) {
			var dataType, nullable string
			var columnDefault *string
			if err := d.Pool.QueryRow(ctx,
				`SELECT data_type, is_nullable, column_default
				   FROM information_schema.columns
				  WHERE table_schema = 'public' AND table_name = $1
				    AND column_name = 'injection_flags'`, table).
				Scan(&dataType, &nullable, &columnDefault); err != nil {
				t.Fatalf("read %s.injection_flags definition: %v", table, err)
			}
			if dataType != "jsonb" {
				t.Fatalf("%s.injection_flags data_type = %q, want jsonb", table, dataType)
			}
			if nullable != "NO" {
				t.Fatalf("%s.injection_flags is_nullable = %q, want NO", table, nullable)
			}
			if columnDefault == nil || !strings.Contains(*columnDefault, "'[]'") {
				t.Fatalf("%s.injection_flags default = %v, want '[]'::jsonb", table, columnDefault)
			}

			var agentSelect bool
			if err := d.Pool.QueryRow(ctx,
				`SELECT has_column_privilege($1::text, 'public.' || $2::text,
				                              'injection_flags', 'SELECT')`,
				db.RoleAgentRO, table).Scan(&agentSelect); err != nil {
				t.Fatalf("read agent column privilege on %s: %v", table, err)
			}
			if agentSelect {
				t.Fatalf("recon_agent_ro can SELECT %s.injection_flags, want no access", table)
			}
		})
	}

	caseID := m2CreateCase(t, d)
	app := openRolePool(t, d, db.RoleApp)

	var runID, runFlags string
	if err := app.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text, injection_flags::text`, caseID).Scan(&runID, &runFlags); err != nil {
		t.Fatalf("app insert agent_runs: %v", err)
	}
	if runFlags != "[]" {
		t.Fatalf("agent_runs.injection_flags default = %q, want []", runFlags)
	}

	if _, err := app.Exec(ctx,
		`UPDATE agent_runs SET injection_flags = '{"instruction_override":true}'::jsonb
		  WHERE id = $1::uuid`, runID); err == nil {
		t.Fatal("agent_runs.injection_flags accepted a JSON object, want array check")
	} else {
		requirePgError(t, err, "23514")
	}

	if _, err := app.Exec(ctx,
		`UPDATE agent_runs SET injection_flags = '["instruction_override"]'::jsonb
		  WHERE id = $1::uuid`, runID); err != nil {
		t.Fatalf("app update agent_runs.injection_flags: %v", err)
	}
	if err := app.QueryRow(ctx,
		`SELECT injection_flags::text FROM agent_runs WHERE id = $1::uuid`, runID).
		Scan(&runFlags); err != nil {
		t.Fatalf("read updated run flags: %v", err)
	}
	if runFlags != `["instruction_override"]` {
		t.Fatalf("agent_runs.injection_flags = %q", runFlags)
	}

	var defaultFlags string
	if err := app.QueryRow(ctx,
		`INSERT INTO tool_calls
		     (agent_run_id, seq, tool_name, args, result, result_hash, started_at, duration_ms)
		 VALUES ($1::uuid, 1, 'get_case', '{}'::jsonb, '{"data":{}}'::jsonb,
		         'hash-default', now(), 1)
		 RETURNING injection_flags::text`, runID).Scan(&defaultFlags); err != nil {
		t.Fatalf("app insert tool_calls with default flags: %v", err)
	}
	if defaultFlags != "[]" {
		t.Fatalf("tool_calls.injection_flags default = %q, want []", defaultFlags)
	}

	var explicitFlags string
	if err := app.QueryRow(ctx,
		`INSERT INTO tool_calls
		     (agent_run_id, seq, tool_name, args, result, result_hash,
		      injection_flags, started_at, duration_ms)
		 VALUES ($1::uuid, 2, 'lookup_external_record', '{}'::jsonb,
		         '{"data":{}}'::jsonb, 'hash-explicit', '["url"]'::jsonb, now(), 1)
		 RETURNING injection_flags::text`, runID).Scan(&explicitFlags); err != nil {
		t.Fatalf("app insert tool_calls with explicit flags: %v", err)
	}
	if explicitFlags != `["url"]` {
		t.Fatalf("tool_calls.injection_flags = %q, want [\"url\"]", explicitFlags)
	}

	if _, err := app.Exec(ctx,
		`INSERT INTO tool_calls
		     (agent_run_id, seq, tool_name, args, result, result_hash,
		      injection_flags, started_at, duration_ms)
		 VALUES ($1::uuid, 3, 'bad_flags', '{}'::jsonb, '{"data":{}}'::jsonb,
		         'hash-bad', '{"x":1}'::jsonb, now(), 1)`, runID); err == nil {
		t.Fatal("tool_calls.injection_flags accepted a JSON object, want array check")
	} else {
		requirePgError(t, err, "23514")
	}
}

func m2CreateCase(t *testing.T, d *testdb.DB) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO cases (priority, value_minor, currency, created_at)
		 VALUES (0, 100, 'USD', now())
		 RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("create case: %v", err)
	}
	return id
}

func m2CreateAgentRun(t *testing.T, d *testdb.DB, caseID string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&id); err != nil {
		t.Fatalf("create agent run: %v", err)
	}
	return id
}

func m2InsertProposal(t *testing.T, pool *pgxpool.Pool, caseID, runID, key, status string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'evidence matches',
		         0.9000, $3, 'state-hash', $4, now())
		 RETURNING id::text`,
		caseID, runID, status, key).Scan(&id); err != nil {
		t.Fatalf("insert proposal %q: %v", key, err)
	}
	return id
}
