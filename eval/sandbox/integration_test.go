//go:build integration

package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
)

func integrationAdminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	return dsn
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := NewManager(Options{
		AdminDSN: integrationAdminDSN(t),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

func queryInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var value int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return value
}

func databaseExists(t *testing.T, adminDSN, name string) bool {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(context.Background())) }()
	var exists bool
	if err := conn.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("check database %q: %v", name, err)
	}
	return exists
}

func roleExists(t *testing.T, adminDSN, name string) bool {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(context.Background())) }()
	var exists bool
	if err := conn.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("check role %q: %v", name, err)
	}
	return exists
}

func execAdmin(t *testing.T, adminDSN, sql string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(context.Background())) }()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

type evalTruthColumn struct {
	dataType string
	nullable string
}

var evalTruthColumns = map[string]map[string]evalTruthColumn{
	"sandbox_meta": {
		"id":                   {"integer", "NO"},
		"eval_dataset_version": {"text", "NO"},
		"split":                {"text", "NO"},
		"seed":                 {"bigint", "NO"},
		"world_clock":          {"timestamp with time zone", "NO"},
		"loaded_at":            {"timestamp with time zone", "NO"},
	},
	"scenarios": {
		"scenario_id":          {"text", "NO"},
		"split":                {"text", "NO"},
		"mode":                 {"text", "NO"},
		"difficulty":           {"text", "NO"},
		"break_signature":      {"text", "NO"},
		"truth_schema_version": {"text", "NO"},
		"truth":                {"jsonb", "NO"},
		"loaded_at":            {"timestamp with time zone", "NO"},
	},
	"scenario_cases": {
		"scenario_id":   {"text", "NO"},
		"case_id":       {"uuid", "NO"},
		"break_id":      {"uuid", "YES"},
		"synthetic":     {"boolean", "NO"},
		"scenario_time": {"timestamp with time zone", "NO"},
		"created_at":    {"timestamp with time zone", "NO"},
	},
}

func TestEvalTruthMigrationSchemaAndGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	for table, wantColumns := range evalTruthColumns {
		rows, err := d.Pool.Query(ctx,
			`SELECT column_name, data_type, is_nullable
			   FROM information_schema.columns
			  WHERE table_schema = 'eval_truth' AND table_name = $1
			  ORDER BY ordinal_position`, table)
		if err != nil {
			t.Fatalf("read %s columns: %v", table, err)
		}
		got := make(map[string]evalTruthColumn)
		for rows.Next() {
			var name string
			var column evalTruthColumn
			if err := rows.Scan(&name, &column.dataType, &column.nullable); err != nil {
				rows.Close()
				t.Fatalf("scan %s column: %v", table, err)
			}
			got[name] = column
		}
		rows.Close()
		for name, want := range wantColumns {
			column, ok := got[name]
			if !ok {
				t.Fatalf("%s.%s is missing", table, name)
			}
			if column != want {
				t.Fatalf("%s.%s = %+v, want %+v", table, name, column, want)
			}
		}
	}

	for table, constraintType := range map[string]string{
		"sandbox_meta":   "p",
		"scenarios":      "p",
		"scenario_cases": "p",
	} {
		var count int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_constraint
			  WHERE conrelid = $1::regclass AND contype = $2`,
			"eval_truth."+table, constraintType).Scan(&count); err != nil {
			t.Fatalf("read %s primary key: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("%s primary key count = %d, want 1", table, count)
		}
	}

	var deleteAction, foreignKey string
	if err := d.Pool.QueryRow(ctx,
		`SELECT conname, confdeltype::text
		   FROM pg_constraint
		  WHERE conrelid = 'eval_truth.scenario_cases'::regclass AND contype = 'f'`).
		Scan(&foreignKey, &deleteAction); err != nil {
		t.Fatalf("read scenario_cases foreign key: %v", err)
	}
	if deleteAction != "c" {
		t.Fatalf("scenario_cases FK delete action = %q, want CASCADE (c)", deleteAction)
	}
	if foreignKey == "" {
		t.Fatal("scenario_cases foreign key has no name")
	}

	var indexExists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM pg_indexes
		     WHERE schemaname = 'eval_truth'
		       AND tablename = 'scenario_cases'
		       AND indexname = 'eval_truth_scenario_cases_case_idx')`).Scan(&indexExists); err != nil {
		t.Fatalf("read scenario_cases index: %v", err)
	}
	if !indexExists {
		t.Fatal("eval_truth_scenario_cases_case_idx is missing")
	}

	var publicGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM pg_namespace n,
		        aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) AS privilege
		  WHERE n.nspname = 'eval_truth' AND privilege.grantee = 0`).Scan(&publicGrants); err != nil {
		t.Fatalf("read eval_truth schema ACL: %v", err)
	}
	if publicGrants != 0 {
		t.Fatalf("eval_truth schema grants %d privileges to PUBLIC, want 0", publicGrants)
	}

	for _, role := range db.Roles() {
		var schemaUsage, tableSelect bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT has_schema_privilege($1, 'eval_truth', 'USAGE'),
			        has_table_privilege($1, 'eval_truth.scenarios', 'SELECT')`,
			role).Scan(&schemaUsage, &tableSelect); err != nil {
			t.Fatalf("read %s privileges: %v", role, err)
		}
		if schemaUsage || tableSelect {
			t.Fatalf("%s has eval_truth privileges (schema=%t table=%t)",
				role, schemaUsage, tableSelect)
		}
	}

	if count := queryInt(t, d.Pool, `SELECT count(*) FROM eval_truth.scenarios`); count != 0 {
		t.Fatalf("admin read returned %d scenarios, want 0", count)
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_truth.sandbox_meta (id, eval_dataset_version, split, seed, world_clock, loaded_at)
		 VALUES (1, 'v1', 'dev', 1, $1, $1)`, now); err != nil {
		t.Fatalf("insert sandbox_meta: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_truth.sandbox_meta (id, eval_dataset_version, split, seed, world_clock, loaded_at)
		 VALUES (1, 'v2', 'dev', 1, $1, $1)`, now); !isSQLState(err, "23505", "23514") {
		t.Fatalf("second sandbox_meta error = %v, want unique/check violation", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_truth.sandbox_meta (id, eval_dataset_version, split, seed, world_clock, loaded_at)
		 VALUES (2, 'v2', 'dev', 1, $1, $1)`, now); !isSQLState(err, "23514", "23505") {
		t.Fatalf("sandbox_meta id=2 error = %v, want check violation", err)
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_truth.scenarios
		     (scenario_id, split, mode, difficulty, break_signature, truth_schema_version, truth, loaded_at)
		 VALUES ('s1', 'dev', 'clean', 'easy', '', 'v1', '{}'::jsonb, $1)`, now); err != nil {
		t.Fatalf("insert scenario: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_truth.scenarios
		     (scenario_id, split, mode, difficulty, break_signature, truth_schema_version, truth, loaded_at)
		 VALUES ('s2', 'bogus', 'clean', 'easy', '', 'v1', '{}'::jsonb, $1)`, now); !isSQLState(err, "23514") {
		t.Fatalf("invalid split error = %v, want check violation", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_truth.scenarios
		     (scenario_id, split, mode, difficulty, break_signature, truth_schema_version, truth, loaded_at)
		 VALUES ('s3', 'dev', 'clean', 'easy', '', 'v1', '[]'::jsonb, $1)`, now); !isSQLState(err, "23514") {
		t.Fatalf("non-object truth error = %v, want check violation", err)
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_truth.scenario_cases (scenario_id, case_id, synthetic, scenario_time, created_at)
		 VALUES ('missing', gen_random_uuid(), false, $1, $1)`, now); !isSQLState(err, "23503") {
		t.Fatalf("missing scenario FK error = %v, want foreign key violation", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_truth.scenario_cases (scenario_id, case_id, synthetic, scenario_time, created_at)
		 VALUES ('s1', gen_random_uuid(), false, $1, $1)`, now); err != nil {
		t.Fatalf("insert scenario case: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `DELETE FROM eval_truth.scenarios WHERE scenario_id = 's1'`); err != nil {
		t.Fatalf("delete scenario: %v", err)
	}
	if count := queryInt(t, d.Pool,
		`SELECT count(*) FROM eval_truth.scenario_cases WHERE scenario_id = 's1'`); count != 0 {
		t.Fatalf("cascade left %d scenario_cases rows, want 0", count)
	}
}

func TestAgentRoleCannotReadEvalTruth(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	agent, err := db.OpenRole(ctx, db.RoleAgentRO, d.DSNFor(db.RoleAgentRO))
	if err != nil {
		t.Fatalf("open agent role pool: %v", err)
	}
	defer agent.Close()

	var count int
	err = agent.Pool.QueryRow(ctx, `SELECT count(*) FROM eval_truth.scenarios`).Scan(&count)
	if err == nil {
		t.Fatalf("recon_agent_ro read eval_truth.scenarios successfully (count=%d)", count)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("agent read error = %T %v, want a Postgres error", err, err)
	}
	if pgErr.Code != "42501" && pgErr.Code != "3F000" {
		t.Fatalf("agent read SQLSTATE = %s, want 42501 or 3F000 (%v)", pgErr.Code, pgErr)
	}

	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM eval_truth.scenarios`).Scan(&count); err != nil {
		t.Fatalf("admin read eval_truth.scenarios: %v", err)
	}
}

func TestSandboxCloneLifecycleAndRoleBoundaries(t *testing.T) {
	ctx := context.Background()
	ds, cfg := miniDataset(t, 7101)
	adminDSN := integrationAdminDSN(t)
	manager := newManager(t)

	sandbox, err := manager.Clone(ctx, ds, LoadOptions{Config: cfg})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	name := sandbox.Name
	logins := make([]string, 0, len(sandbox.logins))
	for _, login := range sandbox.logins {
		logins = append(logins, login.Username)
	}
	sort.Strings(logins)
	if len(logins) != 4 {
		t.Fatalf("provisioned %d logins, want 4: %v", len(logins), logins)
	}
	for _, login := range logins {
		if !strings.HasPrefix(login, loginPrefix) {
			t.Fatalf("login %q does not use the eval login prefix", login)
		}
	}
	if !strings.HasPrefix(name, databasePrefix) || strings.HasPrefix(name, "recon_test_") {
		t.Fatalf("clone name %q is not an eval-only name", name)
	}

	for role, pool := range map[string]*pgxpool.Pool{db.RoleApp: sandbox.App, db.RoleAgentRO: sandbox.Agent} {
		var currentUser, superuser string
		if err := pool.QueryRow(ctx,
			`SELECT current_user, current_setting('is_superuser')`).
			Scan(&currentUser, &superuser); err != nil {
			t.Fatalf("%s pool identity: %v", role, err)
		}
		if superuser != "off" {
			t.Fatalf("%s pool is a superuser", role)
		}
		if !strings.HasPrefix(currentUser, loginPrefix) {
			t.Fatalf("%s pool current_user %q is not an ephemeral eval login", role, currentUser)
		}
	}

	if err := sandbox.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sandbox.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if databaseExists(t, adminDSN, name) {
		t.Fatalf("clone %q survived Close", name)
	}
	for _, login := range logins {
		if roleExists(t, adminDSN, login) {
			t.Fatalf("login %q survived Close", login)
		}
	}

	// A partial load failure must still drop the clone and every login.
	beforeDatabases := evalDatabases(t, adminDSN)
	beforeLogins := evalLogins(t, adminDSN)
	broken := *ds
	broken.Files = []synth.File{{Path: "ledger.json", Data: []byte("{not json")}}
	if _, err := manager.Clone(ctx, &broken, LoadOptions{Config: cfg}); err == nil {
		t.Fatal("Clone succeeded with a corrupt world")
	}
	afterDatabases := evalDatabases(t, adminDSN)
	afterLogins := evalLogins(t, adminDSN)
	if got := newNames(beforeDatabases, afterDatabases); len(got) != 0 {
		t.Fatalf("partial load left databases: %v", got)
	}
	if got := newNames(beforeLogins, afterLogins); len(got) != 0 {
		t.Fatalf("partial load left logins: %v", got)
	}
}

func evalDatabases(t *testing.T, adminDSN string) []string {
	t.Helper()
	names, err := listDatabases(context.Background(), adminDSN, databasePrefix)
	if err != nil {
		t.Fatalf("list eval databases: %v", err)
	}
	return names
}

func evalLogins(t *testing.T, adminDSN string) []string {
	t.Helper()
	names, err := listLogins(context.Background(), adminDSN, loginPrefix)
	if err != nil {
		t.Fatalf("list eval logins: %v", err)
	}
	return names
}

func isSQLState(err error, codes ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	for _, code := range codes {
		if pgErr.Code == code {
			return true
		}
	}
	return false
}
