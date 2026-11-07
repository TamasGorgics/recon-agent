//go:build integration

package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

type m3EvalColumn struct {
	dataType  string
	nullable  string
	precision int
	scale     int
}

var m3EvalRunColumns = map[string]m3EvalColumn{
	"id":              {"uuid", "NO", 0, 0},
	"suite":           {"text", "NO", 0, 0},
	"split":           {"text", "NO", 0, 0},
	"dataset_version": {"text", "NO", 0, 0},
	"model":           {"text", "NO", 0, 0},
	"prompt_version":  {"text", "NO", 0, 0},
	"seed":            {"bigint", "NO", 0, 0},
	"git_sha":         {"text", "YES", 0, 0},
	"config":          {"jsonb", "NO", 0, 0},
	"started_at":      {"timestamp with time zone", "NO", 0, 0},
	"ended_at":        {"timestamp with time zone", "YES", 0, 0},
	"repeat_group":    {"uuid", "YES", 0, 0},
	"repeat_index":    {"integer", "YES", 0, 0},
}

var m3EvalResultColumns = map[string]m3EvalColumn{
	"id":            {"uuid", "NO", 0, 0},
	"eval_run_id":   {"uuid", "NO", 0, 0},
	"scenario_id":   {"text", "NO", 0, 0},
	"failure_mode":  {"text", "NO", 0, 0},
	"correct":       {"boolean", "NO", 0, 0},
	"false_fix":     {"boolean", "NO", 0, 0},
	"escalated":     {"boolean", "NO", 0, 0},
	"tier_expected": {"text", "YES", 0, 0},
	"tier_actual":   {"text", "YES", 0, 0},
	"confidence":    {"numeric", "YES", 5, 4},
	"cost_usd":      {"numeric", "NO", 20, 6},
	"latency_ms":    {"integer", "NO", 0, 0},
	"steps":         {"integer", "NO", 0, 0},
	"detail":        {"jsonb", "NO", 0, 0},
}

// TestM3EvalSchemaColumns asserts the M3-01 table shape, FK, unique key, and
// indexes from spec section 3.1 plus the documented repeat/repeat_index delta.
func TestM3EvalSchemaColumns(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	for _, table := range []string{"eval_runs", "eval_case_results"} {
		var exists bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("resolve table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("table %s is missing", table)
		}
	}
	assertM3EvalColumns(t, d, "eval_runs", m3EvalRunColumns)
	assertM3EvalColumns(t, d, "eval_case_results", m3EvalResultColumns)

	var fkName, deleteAction string
	if err := d.Pool.QueryRow(ctx,
		`SELECT conname, confdeltype::text
		   FROM pg_constraint
		  WHERE conrelid = 'public.eval_case_results'::regclass AND contype = 'f'`).
		Scan(&fkName, &deleteAction); err != nil {
		t.Fatalf("read eval_case_results foreign key: %v", err)
	}
	if deleteAction != "r" {
		t.Fatalf("eval_case_results FK delete action = %q, want RESTRICT (r)", deleteAction)
	}
	if fkName == "" {
		t.Fatal("eval_case_results foreign key has no name")
	}

	var uniqueName string
	if err := d.Pool.QueryRow(ctx,
		`SELECT conname
		   FROM pg_constraint
		  WHERE conrelid = 'public.eval_case_results'::regclass
		    AND contype = 'u' AND conname = 'eval_case_results_run_scenario_key'`).
		Scan(&uniqueName); err != nil {
		t.Fatalf("read eval_case_results unique constraint: %v", err)
	}

	indexes := map[string]string{}
	rows, err := d.Pool.Query(ctx,
		`SELECT indexname, indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND tablename IN ('eval_runs', 'eval_case_results')`)
	if err != nil {
		t.Fatalf("read eval indexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatalf("scan eval index: %v", err)
		}
		indexes[name] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate eval indexes: %v", err)
	}
	expected := []string{
		"eval_runs_split_started_idx",
		"eval_runs_dataset_version_idx",
		"eval_runs_model_prompt_started_idx",
		"eval_runs_repeat_group_idx",
		"eval_case_results_run_mode_idx",
		"eval_case_results_run_correct_idx",
		"eval_case_results_scenario_idx",
	}
	for _, name := range expected {
		if _, ok := indexes[name]; !ok {
			t.Fatalf("index %s is missing; have %v", name, sortedIndexNames(indexes))
		}
	}
	if def := indexes["eval_runs_repeat_group_idx"]; !strings.Contains(def, "WHERE") ||
		!strings.Contains(def, "repeat_group IS NOT NULL") {
		t.Fatalf("eval_runs_repeat_group_idx is not the partial repeat index: %s", def)
	}
	if def := indexes["eval_runs_split_started_idx"]; !strings.Contains(def, "started_at DESC") {
		t.Fatalf("eval_runs_split_started_idx does not order started_at DESC: %s", def)
	}
	if def := indexes["eval_runs_model_prompt_started_idx"]; !strings.Contains(def, "prompt_version") ||
		!strings.Contains(def, "started_at DESC") {
		t.Fatalf("eval_runs_model_prompt_started_idx is wrong: %s", def)
	}
}

func assertM3EvalColumns(t *testing.T, d *testdb.DB, table string, want map[string]m3EvalColumn) {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT column_name, data_type, is_nullable,
		        COALESCE(numeric_precision, 0), COALESCE(numeric_scale, 0)
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = $1
		  ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatalf("read %s columns: %v", table, err)
	}
	defer rows.Close()
	got := make(map[string]m3EvalColumn, len(want))
	for rows.Next() {
		var name string
		var column m3EvalColumn
		if err := rows.Scan(&name, &column.dataType, &column.nullable,
			&column.precision, &column.scale); err != nil {
			t.Fatalf("scan %s column: %v", table, err)
		}
		got[name] = column
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s columns: %v", table, err)
	}
	for name, expected := range want {
		actual, ok := got[name]
		if !ok {
			t.Fatalf("%s is missing column %s", table, name)
		}
		if actual.dataType != expected.dataType || actual.nullable != expected.nullable {
			t.Fatalf("%s.%s = %s/%s, want %s/%s", table, name,
				actual.dataType, actual.nullable, expected.dataType, expected.nullable)
		}
		if expected.dataType == "numeric" &&
			(actual.precision != expected.precision || actual.scale != expected.scale) {
			t.Fatalf("%s.%s precision/scale = %d/%d, want %d/%d", table, name,
				actual.precision, actual.scale, expected.precision, expected.scale)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s has %d columns, want %d: %v", table, len(got), len(want), sortedM3Columns(got))
	}
}

func sortedM3Columns(columns map[string]m3EvalColumn) []string {
	names := make([]string, 0, len(columns))
	for name := range columns {
		names = append(names, name)
	}
	return names
}

func sortedIndexNames(indexes map[string]string) []string {
	names := make([]string, 0, len(indexes))
	for name := range indexes {
		names = append(names, name)
	}
	return names
}

// TestM3EvalSchemaConstraints exercises the checks and constraints behaviorally
// so an unnamed check cannot silently disappear.
func TestM3EvalSchemaConstraints(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	counter := 0
	nextScenario := func() string {
		counter++
		return fmt.Sprintf("m3-scenario-%03d", counter)
	}

	runID := m3InsertEvalRun(t, d)

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated)
		 VALUES ($1::uuid, $2, 'fee_variance', true, false, false)`,
		runID, nextScenario()); err != nil {
		t.Fatalf("insert valid eval result: %v", err)
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated)
		 VALUES ($1::uuid, $2, 'fee_variance', true, false, false)`,
		runID, "m3-scenario-001"); err == nil {
		t.Fatal("duplicate (eval_run_id, scenario_id) insert succeeded")
	} else if pgErr := requirePgError(t, err, "23505"); pgErr.ConstraintName != "eval_case_results_run_scenario_key" {
		t.Fatalf("duplicate constraint = %q, want eval_case_results_run_scenario_key", pgErr.ConstraintName)
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated)
		 VALUES ('00000000-0000-4000-8000-000000000000'::uuid, $1, 'fee_variance', true, false, false)`,
		nextScenario()); err == nil {
		t.Fatal("eval result with a missing run succeeded")
	} else {
		requirePgError(t, err, "23503")
	}

	checkCases := []struct {
		name string
		sql  string
		args []any
	}{
		{"empty suite", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('', 'test', 'v1', 'scripted', 'v1', 2, now())`, []any{}},
		{"unknown split", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('suite', 'holdout', 'v1', 'scripted', 'v1', 2, now())`, []any{}},
		{"empty dataset version", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('suite', 'test', '', 'scripted', 'v1', 2, now())`, []any{}},
		{"empty model", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('suite', 'test', 'v1', '', 'v1', 2, now())`, []any{}},
		{"empty prompt version", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('suite', 'test', 'v1', 'scripted', '', 2, now())`, []any{}},
		{"ended before started", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at, ended_at)
		 VALUES ('suite', 'test', 'v1', 'scripted', 'v1', 2, now(), now() - interval '1 hour')`, []any{}},
		{"repeat index without group", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at, repeat_index)
		 VALUES ('suite', 'test', 'v1', 'scripted', 'v1', 2, now(), 0)`, []any{}},
		{"negative repeat index", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at, repeat_group, repeat_index)
		 VALUES ('suite', 'test', 'v1', 'scripted', 'v1', 2, now(), gen_random_uuid(), -1)`, []any{}},
		{"config not object", `INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at, config)
		 VALUES ('suite', 'test', 'v1', 'scripted', 'v1', 2, now(), '[]'::jsonb)`, []any{}},
		{"empty scenario", `INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated)
		 VALUES ($1::uuid, '', 'fee_variance', true, false, false)`, []any{runID}},
		{"empty failure mode", `INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated)
		 VALUES ($1::uuid, $2, '', true, false, false)`, nil},
		{"invalid tier", `INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated, tier_expected)
		 VALUES ($1::uuid, $2, 'fee_variance', true, false, false, 'T3')`, nil},
		{"confidence above one", `INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated, confidence)
		 VALUES ($1::uuid, $2, 'fee_variance', true, false, false, 1.5)`, nil},
		{"negative cost", `INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated, cost_usd)
		 VALUES ($1::uuid, $2, 'fee_variance', true, false, false, -1)`, nil},
		{"negative latency", `INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated, latency_ms)
		 VALUES ($1::uuid, $2, 'fee_variance', true, false, false, -1)`, nil},
		{"negative steps", `INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated, steps)
		 VALUES ($1::uuid, $2, 'fee_variance', true, false, false, -1)`, nil},
		{"detail not object", `INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated, detail)
		 VALUES ($1::uuid, $2, 'fee_variance', true, false, false, '[]'::jsonb)`, nil},
	}
	for _, tc := range checkCases {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if args == nil {
				args = []any{runID, nextScenario()}
			}
			_, err := d.Pool.Exec(ctx, tc.sql, args...)
			if err == nil {
				t.Fatalf("%s insert succeeded, want a check violation", tc.name)
			}
			requirePgError(t, err, "23514")
		})
	}

	// Positive coherence: a repeat group with an index is valid.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_runs
		     (suite, split, dataset_version, model, prompt_version, seed, started_at,
		      repeat_group, repeat_index, git_sha, config)
		 VALUES ('suite', 'test', 'v1', 'scripted', 'v1', 2, now(),
		         gen_random_uuid(), 1, 'abc123', '{"concurrency": 4}'::jsonb)`); err != nil {
		t.Fatalf("valid repeat run insert: %v", err)
	}
}

func m3InsertEvalRun(t *testing.T, d *testdb.DB) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('m3-01-suite', 'test', 'dataset-v1', 'scripted', 'v1', 2, now())
		 RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("insert eval run: %v", err)
	}
	return id
}

// TestM3EvalGrants asserts the exact M3-01 privilege matrix: recon_app records
// runs and results, while the agent/audit/executor roles cannot read either
// table. It runs through the real ephemeral logins, not the owner pool.
func TestM3EvalGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	type matrixRow struct {
		role                                                   string
		runsSelect, runsInsert, runsUpdate, runsDelete         bool
		resultSelect, resultInsert, resultUpdate, resultDelete bool
	}
	matrix := []matrixRow{
		{
			role:       db.RoleApp,
			runsSelect: true, runsInsert: true, runsUpdate: true, runsDelete: false,
			resultSelect: true, resultInsert: true, resultUpdate: false, resultDelete: false,
		},
		{role: db.RoleAgentRO},
		{role: db.RoleAuditRO},
		{role: db.RoleExecutor},
	}
	for _, row := range matrix {
		t.Run(row.role, func(t *testing.T) {
			assertEvalPrivilege(t, d, row.role, "eval_runs",
				row.runsSelect, row.runsInsert, row.runsUpdate, row.runsDelete)
			assertEvalPrivilege(t, d, row.role, "eval_case_results",
				row.resultSelect, row.resultInsert, row.resultUpdate, row.resultDelete)
		})
	}

	var publicGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_table_grants
		  WHERE table_schema = 'public'
		    AND table_name IN ('eval_runs', 'eval_case_results')
		    AND grantee = 'PUBLIC'`).Scan(&publicGrants); err != nil {
		t.Fatalf("read PUBLIC grants: %v", err)
	}
	if publicGrants != 0 {
		t.Fatalf("PUBLIC holds %d eval table grants, want 0", publicGrants)
	}

	app := openRolePool(t, d, db.RoleApp)
	var runID string
	if err := app.QueryRow(ctx,
		`INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('m3-01-suite', 'test', 'dataset-v1', 'scripted', 'v1', 2, now())
		 RETURNING id::text`).Scan(&runID); err != nil {
		t.Fatalf("app insert eval_runs: %v", err)
	}
	if _, err := app.Exec(ctx,
		`INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated, cost_usd, latency_ms, steps)
		 VALUES ($1::uuid, 'grant-scenario', 'fee_variance', true, false, false, 0.000001, 12, 3)`,
		runID); err != nil {
		t.Fatalf("app insert eval_case_results: %v", err)
	}
	if _, err := app.Exec(ctx,
		`UPDATE eval_runs SET ended_at = now() WHERE id = $1::uuid`, runID); err != nil {
		t.Fatalf("app update eval_runs: %v", err)
	}
	if _, err := app.Exec(ctx,
		`INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated)
		 VALUES ($1::uuid, 'grant-scenario', 'fee_variance', true, false, false)`,
		runID); err == nil {
		t.Fatal("app duplicate scenario insert succeeded")
	} else if pgErr := requirePgError(t, err, "23505"); pgErr.ConstraintName != "eval_case_results_run_scenario_key" {
		t.Fatalf("app duplicate constraint = %q", pgErr.ConstraintName)
	}
	if _, err := app.Exec(ctx,
		`UPDATE eval_case_results SET correct = false WHERE eval_run_id = $1::uuid`, runID); err == nil {
		t.Fatal("app UPDATE eval_case_results succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := app.Exec(ctx,
		`DELETE FROM eval_runs WHERE id = $1::uuid`, runID); err == nil {
		t.Fatal("app DELETE eval_runs succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := app.Exec(ctx, `DELETE FROM eval_case_results`); err == nil {
		t.Fatal("app DELETE eval_case_results succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	for _, role := range []string{db.RoleAgentRO, db.RoleAuditRO, db.RoleExecutor} {
		t.Run("denied/"+role, func(t *testing.T) {
			rolePool := openRolePool(t, d, role)
			for _, table := range []string{"eval_runs", "eval_case_results"} {
				if _, err := rolePool.Exec(ctx, `SELECT count(*) FROM `+table); err == nil {
					t.Fatalf("%s read %s, want permission denied", role, table)
				} else {
					requirePgError(t, err, sqlStateInsufficientPrivilege)
				}
			}
		})
	}
	if _, err := app.Exec(ctx,
		`INSERT INTO eval_case_results
		     (eval_run_id, scenario_id, failure_mode, correct, false_fix, escalated)
		 VALUES ($1::uuid, 'no-run', 'fee_variance', true, false, false)`,
		"00000000-0000-4000-8000-000000000000"); err == nil {
		t.Fatal("app inserted a result for a missing run")
	} else {
		requirePgError(t, err, "23503")
	}
}

func assertEvalPrivilege(t *testing.T, d *testdb.DB, role, table string,
	selectWant, insertWant, updateWant, deleteWant bool) {
	t.Helper()
	var canSelect, canInsert, canUpdate, canDelete bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1::text, $2::text, 'SELECT'),
		        has_table_privilege($1::text, $2::text, 'INSERT'),
		        has_table_privilege($1::text, $2::text, 'UPDATE'),
		        has_table_privilege($1::text, $2::text, 'DELETE')`,
		role, table).Scan(&canSelect, &canInsert, &canUpdate, &canDelete); err != nil {
		t.Fatalf("read %s privileges on %s: %v", role, table, err)
	}
	if canSelect != selectWant || canInsert != insertWant ||
		canUpdate != updateWant || canDelete != deleteWant {
		t.Fatalf("%s on %s = SELECT %t INSERT %t UPDATE %t DELETE %t, want %t/%t/%t/%t",
			role, table, canSelect, canInsert, canUpdate, canDelete,
			selectWant, insertWant, updateWant, deleteWant)
	}
}
