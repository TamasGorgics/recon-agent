//go:build integration

package demo_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/demo"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
)

// TestM505ResetGuardRejectsUntaggedRowsInAnyDroppedTable proves the reset guard
// inspects every table the destructive reset destroys, not only the eval and
// approval tables. Each subtest inserts one untagged row, proves the guard
// returns the typed error, and proves no DDL ran (the row and migration version
// survive).
type resetGuardCase struct {
	name        string
	seed        bool
	table       string
	extraTables []string
	inserter    func(t *testing.T, d *testdb.DB)
}

func (tc resetGuardCase) matches(table string) bool {
	if table == tc.table {
		return true
	}
	for _, extra := range tc.extraTables {
		if table == extra {
			return true
		}
	}
	return false
}

func TestM505ResetGuardRejectsUntaggedRowsInAnyDroppedTable(t *testing.T) {
	tests := []resetGuardCase{
		{
			name: "cases", table: "cases",
			inserter: func(t *testing.T, d *testdb.DB) {
				mustExec(t, d, `INSERT INTO cases
				    (priority, value_minor, currency, status, created_at)
				    VALUES (1, 100, 'USD', 'open', now())`)
			},
		},
		{
			name: "audit_log", table: "audit_log",
			inserter: func(t *testing.T, d *testdb.DB) {
				mustExec(t, d, `INSERT INTO audit_log
				    (seq, ts, actor_type, actor_id, action, entity_type, entity_id,
				     prev_hash, row_hash)
				    OVERRIDING SYSTEM VALUE
				    VALUES ((SELECT COALESCE(max(seq), 0) + 1 FROM audit_log),
				            now(), 'system', 'guard-test', 'guard.test', 'test',
				            'guard-test', decode(repeat('00', 32), 'hex'),
				            decode(repeat('11', 32), 'hex'))`)
			},
		},
		{
			name: "matches", seed: true, table: "matches",
			inserter: func(t *testing.T, d *testdb.DB) {
				mustExec(t, d, `INSERT INTO matches
				    (scope_id, external_record_id, rule, confidence, match_run_id,
				     status, unmatched_at, unmatched_by, unmatch_reason)
				    SELECT mr.scope_id, er.id, 'exact', 1.0, mr.id,
				           'unmatched', now(), 'guard-test', 'guard-test'
				      FROM match_runs mr CROSS JOIN external_records er
				     LIMIT 1`)
			},
		},
		{
			name: "ledger_txns", seed: true, table: "ledger_txns",
			extraTables: []string{"ledger_entries"},
			inserter: func(t *testing.T, d *testdb.DB) {
				mustExec(t, d, `WITH txn AS (
				    INSERT INTO ledger_txns
				        (value_date, kind, idempotency_key, request_hash, description, period_id)
				    VALUES (current_date, 'charge', 'untagged-guard-ledger',
				            'untagged-guard-hash', 'guard test',
				            (SELECT id FROM periods ORDER BY start_date LIMIT 1))
				    RETURNING id),
				accts AS (
				    SELECT id, row_number() OVER (ORDER BY id) AS rn
				      FROM accounts WHERE currency = 'USD' LIMIT 2)
				INSERT INTO ledger_entries
				    (txn_id, account_id, direction, amount_minor, currency)
				SELECT txn.id, accts.id,
				       CASE WHEN accts.rn = 1 THEN 'debit' ELSE 'credit' END,
				       100, 'USD'
				  FROM txn CROSS JOIN accts`)
			},
		},
		{
			name: "outbox", table: "outbox",
			inserter: func(t *testing.T, d *testdb.DB) {
				mustExec(t, d, `INSERT INTO outbox
				    (aggregate_type, aggregate_id, topic, payload,
				     idempotency_key, created_at, next_attempt_at)
				    VALUES ('case', 'guard-test', 'demo.guard',
				            '{}'::jsonb, 'untagged-guard-outbox', now(), now())`)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := testdb.New(t)
			ctx := context.Background()
			if tc.seed {
				if _, err := demo.Run(ctx, demo.Options{
					DBURL: d.DSN, Seed: 1, Days: 14,
					Difficulty: synth.DifficultyRealistic,
					Clock:      clock.NewFake(pinnedQualityClock()),
				}); err != nil {
					t.Fatalf("seed M1: %v", err)
				}
			}
			tc.inserter(t, d)

			beforeVersion := dbVersion(t, d)
			if err := demo.CheckQualityResetAllowed(ctx, d.DSN); !errors.Is(err, demo.ErrQualityUntaggedData) {
				t.Fatalf("preflight guard error = %v, want ErrQualityUntaggedData", err)
			} else {
				var typed *demo.UntaggedDataError
				if !errors.As(err, &typed) || !tc.matches(typed.Table) {
					t.Fatalf("preflight guard error = %#v, want *UntaggedDataError for %s", err, tc.table)
				}
			}

			err := demo.Reset(ctx, d.DSN)
			if !errors.Is(err, demo.ErrQualityUntaggedData) {
				t.Fatalf("reset guard error = %v, want ErrQualityUntaggedData", err)
			}
			var typed *demo.UntaggedDataError
			if !errors.As(err, &typed) || !tc.matches(typed.Table) {
				t.Fatalf("reset guard error = %#v, want *UntaggedDataError for %s", err, tc.table)
			}

			// No writes: the schema and the untagged row survive, and the
			// migration version is unchanged.
			if got := dbVersion(t, d); got != beforeVersion {
				t.Fatalf("goose version after refused reset = %d, want %d", got, beforeVersion)
			}
			var count int64
			if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM "+tc.table).Scan(&count); err != nil {
				t.Fatalf("count %s after refused reset: %v", tc.table, err)
			}
			if count == 0 {
				t.Fatalf("%s is empty after a refused reset; the row was not preserved", tc.table)
			}
		})
	}
}

// TestM505ResetGuardAllowsDemoOwnedRows proves a fully marked M1+ops+quality
// database resets successfully and leaves an empty migrated scratch database.
func TestM505ResetGuardAllowsDemoOwnedRows(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	runQuality(t, d, pinnedQualityClock())

	if err := demo.CheckQualityResetAllowed(ctx, d.DSN); err != nil {
		t.Fatalf("preflight guard on demo-only data: %v", err)
	}
	if err := demo.Reset(ctx, d.DSN); err != nil {
		t.Fatalf("reset demo-only data: %v", err)
	}

	for _, table := range []string{
		"cases", "case_breaks", "breaks", "matches", "match_legs", "match_runs",
		"ledger_txns", "ledger_entries", "external_records", "settlement_batches",
		"statement_balances", "reconciliation_scopes", "audit_log", "audit_chain_head",
		"agent_runs", "proposals", "approvals", "applied_proposals", "approvers",
		"eval_runs", "eval_case_results", "outbox", "approval_messages",
		"policy_config_versions", "demo_metadata",
	} {
		var count int64
		if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s after reset: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows after reset, want 0", table, count)
		}
	}
	var version int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if version != latestMigrationVersion(t) {
		t.Fatalf("goose version after reset = %d, want latest %d", version, latestMigrationVersion(t))
	}
}

// TestM505ResetGuardConcurrentInsertAborts proves the guard and drop are one
// locked operation: an untagged row committed while the reset waits for the
// table lock is seen by the guard, so the reset aborts instead of destroying it.
func TestM505ResetGuardConcurrentInsertAborts(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if _, err := demo.Run(ctx, demo.Options{
		DBURL: d.DSN, Seed: 1, Days: 14,
		Difficulty: synth.DifficultyRealistic,
		Clock:      clock.NewFake(pinnedQualityClock()),
	}); err != nil {
		t.Fatalf("seed M1: %v", err)
	}

	conn, err := pgx.Connect(ctx, d.DSN)
	if err != nil {
		t.Fatalf("open guard connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(ctx)) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin guard transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE cases IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock cases: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cases
	    (priority, value_minor, currency, status, created_at)
	    VALUES (1, 100, 'USD', 'open', now())`); err != nil {
		t.Fatalf("insert concurrent untagged case: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- demo.Reset(ctx, d.DSN) }()
	waitForPendingResetLock(t, d, "cases")

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent insert: %v", err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, demo.ErrQualityUntaggedData) {
			t.Fatalf("concurrent reset error = %v, want ErrQualityUntaggedData", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("reset did not finish after the concurrent insert committed")
	}

	var count int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM cases WHERE created_at > now() - interval '1 minute'`).Scan(&count); err != nil {
		t.Fatalf("count concurrent case: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent untagged case count = %d, want 1 (not destroyed)", count)
	}
	var demoCases int64
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM cases`).Scan(&demoCases); err != nil {
		t.Fatalf("count cases: %v", err)
	}
	if demoCases <= 1 {
		t.Fatalf("demo rows were dropped by the refused reset: cases = %d", demoCases)
	}
}

// TestM505ResetGuardRefusesProduction proves the production environment guard
// still wins over the data guard and makes no connection-level writes.
func TestM505ResetGuardRefusesProduction(t *testing.T) {
	t.Setenv("RECON_ENV", "production")
	d := testdb.New(t)
	before := dbVersion(t, d)
	if err := demo.Reset(context.Background(), d.DSN); !errors.Is(err, demo.ErrResetProduction) {
		t.Fatalf("reset in production error = %v, want ErrResetProduction", err)
	}
	if got := dbVersion(t, d); got != before {
		t.Fatalf("production reset changed goose version %d -> %d", before, got)
	}
}

// TestM505DemoQualitySeedAllOrNothing forces a quality insert failure after the
// M1+ops layers succeeded. Compensation must leave no partial superset.
func TestM505DemoQualitySeedAllOrNothing(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	forceQualityInsertFailure(t, d)

	if _, err := demo.RunQuality(ctx, qualityOptions(d, pinnedQualityClock())); err == nil {
		t.Fatal("demo quality succeeded with a forced eval_runs insert failure")
	}

	for _, table := range []string{
		"cases", "breaks", "matches", "ledger_txns", "ledger_entries",
		"external_records", "settlement_batches", "audit_log",
		"agent_runs", "proposals", "approvals", "applied_proposals", "approvers",
		"eval_runs", "eval_case_results", "demo_metadata",
	} {
		var count int64
		if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s after failed seed: %v", table, err)
		}
		if count != 0 {
			t.Errorf("failed seed left %d partial rows in %s, want 0", count, table)
		}
	}
}

// TestM505DemoQualitySeedFailureRestoresM1PreRun proves the compensating
// rollback restores a pre-existing M1-only demo layer instead of leaving a
// partial superset or an empty scratch database.
func TestM505DemoQualitySeedFailureRestoresM1PreRun(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	opts := demo.Options{
		DBURL: d.DSN, Seed: 1, Days: 14,
		Difficulty: synth.DifficultyRealistic,
		Clock:      clock.NewFake(pinnedQualityClock()),
	}
	if _, err := demo.Run(ctx, opts); err != nil {
		t.Fatalf("seed M1 pre-run layer: %v", err)
	}
	var m1Cases int64
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM cases`).Scan(&m1Cases); err != nil {
		t.Fatalf("count pre-run cases: %v", err)
	}
	if m1Cases == 0 {
		t.Fatal("pre-run M1 seed wrote no cases")
	}

	forceQualityInsertFailure(t, d)
	if _, err := demo.RunQuality(ctx, opts); err == nil {
		t.Fatal("demo quality succeeded with a forced eval_runs insert failure")
	}

	var cases, matches int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM cases), (SELECT count(*) FROM matches)`).
		Scan(&cases, &matches); err != nil {
		t.Fatalf("count restored M1 rows: %v", err)
	}
	if cases == 0 || matches == 0 {
		t.Fatalf("pre-run M1 layer not restored: cases=%d matches=%d", cases, matches)
	}
	var evalRuns, qualityRows, opsRows int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM eval_runs),
		        (SELECT count(*) FROM demo_metadata WHERE layer = 'quality'),
		        (SELECT count(*) FROM demo_metadata WHERE layer = 'ops')`).
		Scan(&evalRuns, &qualityRows, &opsRows); err != nil {
		t.Fatalf("count residual quality/ops rows: %v", err)
	}
	if evalRuns != 0 || qualityRows != 0 || opsRows != 0 {
		t.Fatalf("failed seed left quality/ops rows: eval_runs=%d quality_markers=%d ops_markers=%d",
			evalRuns, qualityRows, opsRows)
	}
}

// TestM505ResetGuardAllowsIntegrityRuns proves the named operational-evidence
// exception: a valid integrity_runs row does not block the guard or reset, and
// the reset drops the run history. No wildcard or demoRowKeySQL key is used.
// covers: FIN-52
func TestM505ResetGuardAllowsIntegrityRuns(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	mustExec(t, d, `INSERT INTO integrity_runs
	    (run_date, started_at, finished_at, status, check_count,
	     failure_count, checks, triggered_by)
	    VALUES (current_date, now(), now(), 'passed', 5, 0,
	            '[{},{},{},{},{}]'::jsonb, 'manual')`)

	if err := demo.CheckQualityResetAllowed(ctx, d.DSN); err != nil {
		t.Fatalf("guard blocked a valid integrity_runs row: %v", err)
	}
	if err := demo.Reset(ctx, d.DSN); err != nil {
		t.Fatalf("reset with a valid integrity_runs row: %v", err)
	}

	var rows int64
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM integrity_runs`).Scan(&rows); err != nil {
		t.Fatalf("count integrity_runs after reset: %v", err)
	}
	if rows != 0 {
		t.Fatalf("integrity_runs rows after reset = %d, want the history dropped", rows)
	}
	if got := dbVersion(t, d); got != latestMigrationVersion(t) {
		t.Fatalf("goose version after reset = %d, want latest %d", got, latestMigrationVersion(t))
	}
}

// TestM505ResetGuardRejectsUnknownPublicTableRows proves the exception is
// narrow: a new public table with a row still aborts with the typed error and
// no DDL runs.
func TestM505ResetGuardRejectsUnknownPublicTableRows(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	mustExec(t, d, `CREATE TABLE guard_unknown_public (id INT PRIMARY KEY)`)
	mustExec(t, d, `INSERT INTO guard_unknown_public (id) VALUES (1)`)
	beforeVersion := dbVersion(t, d)

	for _, reset := range []func() error{
		func() error { return demo.CheckQualityResetAllowed(ctx, d.DSN) },
		func() error { return demo.Reset(ctx, d.DSN) },
	} {
		err := reset()
		if !errors.Is(err, demo.ErrQualityUntaggedData) {
			t.Fatalf("guard error = %v, want ErrQualityUntaggedData", err)
		}
		var typed *demo.UntaggedDataError
		if !errors.As(err, &typed) || typed.Table != "guard_unknown_public" || typed.Rows != 1 {
			t.Fatalf("guard error = %#v, want *UntaggedDataError for guard_unknown_public", err)
		}
	}
	var rows int64
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM guard_unknown_public`).Scan(&rows); err != nil {
		t.Fatalf("unknown table did not survive the refused reset: %v", err)
	}
	if rows != 1 {
		t.Fatalf("unknown table rows after refused reset = %d, want 1", rows)
	}
	if got := dbVersion(t, d); got != beforeVersion {
		t.Fatalf("goose version after refused reset = %d, want %d", got, beforeVersion)
	}
}

// TestM505ResetGuardStillRejectsIntegrityFailedOutboxRow pins AQ-3b: the
// integrity_runs exception does not extend to outbox, so a failed demo run's
// alert row still blocks reset until it is handled.
func TestM505ResetGuardStillRejectsIntegrityFailedOutboxRow(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	mustExec(t, d, `INSERT INTO integrity_runs
	    (run_date, started_at, finished_at, status, check_count,
	     failure_count, checks, triggered_by)
	    VALUES (current_date, now(), now(), 'failed', 5, 1,
	            '[{},{},{},{},{}]'::jsonb, 'manual')`)
	mustExec(t, d, `INSERT INTO outbox
	    (aggregate_type, aggregate_id, topic, payload, idempotency_key,
	     created_at, next_attempt_at)
	    VALUES ('integrity_run', 'guard-test', 'integrity.failed',
	            '{}'::jsonb, 'integrity.failed:guard-test', now(), now())`)

	err := demo.CheckQualityResetAllowed(ctx, d.DSN)
	if !errors.Is(err, demo.ErrQualityUntaggedData) {
		t.Fatalf("guard error = %v, want the outbox row to still block reset", err)
	}
	var typed *demo.UntaggedDataError
	if !errors.As(err, &typed) || typed.Table != "outbox" {
		t.Fatalf("guard error = %#v, want *UntaggedDataError for outbox", err)
	}
}

func mustExec(t *testing.T, d *testdb.DB, query string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), query); err != nil {
		t.Fatalf("exec %q: %v", strings.SplitN(query, "\n", 2)[0], err)
	}
}

func dbVersion(t *testing.T, d *testdb.DB) int64 {
	t.Helper()
	var version int64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	return version
}

func forceQualityInsertFailure(t *testing.T, d *testdb.DB) {
	t.Helper()
	mustExec(t, d, `CREATE FUNCTION demo_quality_fail() RETURNS trigger
	    LANGUAGE plpgsql AS $$
	    BEGIN
	        RAISE EXCEPTION 'forced quality insert failure';
	    END
	    $$`)
	mustExec(t, d, `CREATE TRIGGER demo_quality_fail
	    BEFORE INSERT ON eval_runs
	    FOR EACH ROW EXECUTE FUNCTION demo_quality_fail()`)
}

func waitForPendingResetLock(t *testing.T, d *testdb.DB, table string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var pending int
		err := d.Pool.QueryRow(ctx,
			`SELECT count(*)
			   FROM pg_locks l
			   JOIN pg_class c ON c.oid = l.relation
			  WHERE c.relname = $1
			    AND l.mode = 'AccessExclusiveLock'
			    AND NOT l.granted`, table).Scan(&pending)
		if err != nil {
			t.Fatalf("inspect pending reset lock: %v", err)
		}
		if pending > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("reset never blocked on the %s table lock", table))
}
