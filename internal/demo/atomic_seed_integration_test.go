//go:build integration

package demo_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/demo"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
)

// The advisory-lock key used by every seed command. Kept in sync with
// internal/demo/demo.go's demoSeedLockClass/demoSeedLockObj.
const (
	atomicSeedLockClass = int64(0x4d3505)
	atomicSeedLockObj   = int64(505)
)

// TestM505FailedSeedDoesNotDestroyConcurrentUntaggedRow is the F-1 regression.
// Before the atomic fix the compensating rollback fell back to an unguarded
// schema reset when the guarded reset refused, destroying an untagged outbox
// row committed by another session during the failed seed. Now the seed is one
// transaction: the failure rolls itself back and the outbox row survives.
func TestM505FailedSeedDoesNotDestroyConcurrentUntaggedRow(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	forceSlowQualityInsertFailure(t, d)

	type result struct {
		summary demo.QualitySummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := demo.RunQuality(ctx, qualityOptions(d, pinnedQualityClock()))
		done <- result{summary: summary, err: err}
	}()

	// Wait until the seed holds its advisory lock and is paused inside the
	// forced eval_runs trigger, then commit the untagged row before the seed
	// fails and rolls back.
	waitForDemoSeedLock(t, d)
	waitForSeedPause(t, d)
	mustExec(t, d, `INSERT INTO outbox
	    (aggregate_type, aggregate_id, topic, payload,
	     idempotency_key, created_at, next_attempt_at)
	    VALUES ('case', 'atomic-seed-f1', 'demo.f1', '{}'::jsonb,
	            'atomic-seed-f1-outbox', now(), now())`)

	got := <-done
	if got.err == nil {
		t.Fatal("demo quality succeeded with a forced eval_runs insert failure")
	}

	var outboxRows int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE idempotency_key = 'atomic-seed-f1-outbox'`).
		Scan(&outboxRows); err != nil {
		t.Fatalf("count untagged outbox row: %v", err)
	}
	if outboxRows != 1 {
		t.Fatalf("untagged outbox row count = %d, want 1 (seed rollback destroyed it)", outboxRows)
	}

	for _, table := range []string{
		"cases", "breaks", "matches", "match_runs", "ledger_txns", "ledger_entries",
		"external_records", "settlement_batches", "audit_log", "agent_runs",
		"proposals", "approvals", "applied_proposals", "approvers", "eval_runs",
		"eval_case_results", "demo_metadata",
	} {
		var count int64
		if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s after failed seed: %v", table, err)
		}
		if count != 0 {
			t.Errorf("failed seed left %d partial rows in %s, want 0", count, table)
		}
	}

	// The guard must still refuse a destructive reset because of the untagged
	// row: the fix must not have weakened the guard to make this pass.
	if err := demo.CheckQualityResetAllowed(ctx, d.DSN); !errors.Is(err, demo.ErrQualityUntaggedData) {
		t.Fatalf("guard after failed seed = %v, want ErrQualityUntaggedData", err)
	}
}

// TestM505ConcurrentRealRowIsNotAdopted is the F-2 regression. Before the
// atomic fix markNewDemoRows UUID-diffed every M1 table in a fresh Read
// Committed transaction, so a real row committed by another session during the
// seed was marked demo-owned and a later --reset deleted it. The seed now holds
// EXCLUSIVE locks on every table it writes from before the marker snapshot
// until commit, so the concurrent insert blocks until the seed commits and can
// never be adopted (or deleted by a later --reset).
func TestM505ConcurrentRealRowIsNotAdopted(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	forceSlowMatchInsert(t, d)

	type result struct {
		summary demo.QualitySummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := demo.RunQuality(ctx, qualityOptions(d, pinnedQualityClock()))
		done <- result{summary: summary, err: err}
	}()

	waitForDemoSeedLock(t, d)
	waitForSeedPause(t, d)
	var realCaseID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, created_at)
		 VALUES (1, 424242, 'USD', 'open', now())
		 RETURNING id::text`).Scan(&realCaseID); err != nil {
		t.Fatalf("insert concurrent real case: %v", err)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("demo quality with concurrent real row: %v", got.err)
	}

	var marked bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM demo_metadata
		                 WHERE table_name = 'cases' AND row_key = $1)`, realCaseID).
		Scan(&marked); err != nil {
		t.Fatalf("check concurrent case marker: %v", err)
	}
	if marked {
		t.Fatal("concurrent real case was adopted into demo_metadata")
	}
	var seededMarkers int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM demo_metadata WHERE table_name = 'cases'`).Scan(&seededMarkers); err != nil {
		t.Fatalf("count demo case markers: %v", err)
	}
	if seededMarkers == 0 {
		t.Fatal("seed wrote no case markers; marker diff is not recording its own rows")
	}
	var stillThere int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM cases WHERE id = $1::uuid`, realCaseID).Scan(&stillThere); err != nil {
		t.Fatalf("count concurrent real case: %v", err)
	}
	if stillThere != 1 {
		t.Fatalf("concurrent real case count = %d, want 1", stillThere)
	}

	// A guard that admitted a reset over the untagged case would delete it.
	if err := demo.CheckQualityResetAllowed(ctx, d.DSN); !errors.Is(err, demo.ErrQualityUntaggedData) {
		t.Fatalf("guard after concurrent real row = %v, want ErrQualityUntaggedData", err)
	}
}

// TestM505QualityFailureRollsBackToNonDefaultM1 is the candidate-3 regression.
// Before the atomic fix the compensation ran reset+Run with the *current* flags,
// so a non-default pre-run M1 layer was replaced by the defaults. Transaction
// rollback must leave the exact pre-run rows untouched.
func TestM505QualityFailureRollsBackToNonDefaultM1(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	preOpts := demo.Options{
		DBURL:      d.DSN,
		Seed:       7,
		Days:       7,
		Difficulty: synth.DifficultyRealistic,
		Clock:      clock.NewFake(pinnedQualityClock()),
	}
	if _, err := demo.Run(ctx, preOpts); err != nil {
		t.Fatalf("seed non-default M1 pre-run layer: %v", err)
	}

	state := func() string {
		t.Helper()
		var cases, matches, breaks, auditRows, m1Markers int64
		var caseHash, matchHash string
		if err := d.Pool.QueryRow(ctx, `SELECT
		    (SELECT count(*) FROM cases),
		    (SELECT count(*) FROM matches),
		    (SELECT count(*) FROM breaks),
		    (SELECT count(*) FROM audit_log),
		    (SELECT count(*) FROM demo_metadata WHERE layer = 'm1'),
		    COALESCE((SELECT md5(string_agg(id::text, ',' ORDER BY id)) FROM cases), ''),
		    COALESCE((SELECT md5(string_agg(id::text, ',' ORDER BY id)) FROM matches), '')`).
			Scan(&cases, &matches, &breaks, &auditRows, &m1Markers, &caseHash, &matchHash); err != nil {
			t.Fatalf("snapshot pre-run M1 state: %v", err)
		}
		return strings.Join([]string{
			strconv.FormatInt(cases, 10), strconv.FormatInt(matches, 10),
			strconv.FormatInt(breaks, 10), strconv.FormatInt(auditRows, 10),
			strconv.FormatInt(m1Markers, 10), caseHash, matchHash,
		}, "|")
	}

	before := state()
	forceQualityInsertFailure(t, d)
	// RunQuality uses the default seed/days: under the old compensation this
	// would have rebuilt the M1 layer with defaults instead of seed 7 / 7 days.
	if _, err := demo.RunQuality(ctx, qualityOptions(d, pinnedQualityClock())); err == nil {
		t.Fatal("demo quality succeeded with a forced eval_runs insert failure")
	}
	after := state()
	if before != after {
		t.Fatalf("pre-run M1 state changed across failed quality seed:\nbefore=%s\nafter =%s", before, after)
	}

	for _, table := range []string{"eval_runs", "eval_case_results"} {
		var count int64
		if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("failed seed left %d rows in %s, want 0", count, table)
		}
	}
	var opsMarkers, qualityMarkers int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE layer = 'ops'),
		        count(*) FILTER (WHERE layer = 'quality')
		   FROM demo_metadata`).Scan(&opsMarkers, &qualityMarkers); err != nil {
		t.Fatalf("count residual markers: %v", err)
	}
	if opsMarkers != 0 || qualityMarkers != 0 {
		t.Fatalf("failed seed left ops/quality markers: ops=%d quality=%d", opsMarkers, qualityMarkers)
	}
}

func forceSlowQualityInsertFailure(t *testing.T, d *testdb.DB) {
	t.Helper()
	mustExec(t, d, `CREATE FUNCTION demo_quality_slow_fail() RETURNS trigger
	    LANGUAGE plpgsql AS $$
	    BEGIN
	        PERFORM pg_sleep(3);
	        RAISE EXCEPTION 'forced slow quality insert failure';
	    END
	    $$`)
	mustExec(t, d, `CREATE TRIGGER demo_quality_slow_fail
	    BEFORE INSERT ON eval_runs
	    FOR EACH ROW EXECUTE FUNCTION demo_quality_slow_fail()`)
}

func forceSlowMatchInsert(t *testing.T, d *testdb.DB) {
	t.Helper()
	// One-shot delay: the first matches row pauses the seed long enough for the
	// test to commit a real row inside the seed's transaction window.
	mustExec(t, d, `CREATE FUNCTION demo_match_slow() RETURNS trigger
	    LANGUAGE plpgsql AS $$
	    BEGIN
	        IF current_setting('demo.match_slow', true) IS DISTINCT FROM 'on' THEN
	            PERFORM set_config('demo.match_slow', 'on', false);
	            PERFORM pg_sleep(3);
	        END IF;
	        RETURN NEW;
	    END
	    $$`)
	mustExec(t, d, `CREATE TRIGGER demo_match_slow
	    BEFORE INSERT ON matches
	    FOR EACH ROW EXECUTE FUNCTION demo_match_slow()`)
}

func waitForDemoSeedLock(t *testing.T, d *testdb.DB) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var held int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks
			  WHERE locktype = 'advisory'
			    AND classid::bigint = $1
			    AND objid::bigint = $2
			    AND granted`, atomicSeedLockClass, atomicSeedLockObj).Scan(&held); err != nil {
			t.Fatalf("inspect demo seed advisory lock: %v", err)
		}
		if held > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("seed never acquired the demo advisory lock")
}

func waitForSeedPause(t *testing.T, d *testdb.DB) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var paused int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database()
			    AND wait_event = 'PgSleep'`).Scan(&paused); err != nil {
			t.Fatalf("inspect paused seed backend: %v", err)
		}
		if paused > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("seed never paused inside the injected trigger")
}

// TestM505EvalWriterBlocksWhileSeedHoldsLocks is the F-3 regression for the
// blocked-writer side of the serialization. The seed pauses inside its first
// eval_runs insert while holding EXCLUSIVE locks on every seed table. A
// concurrent real eval_runs insert must block (it times out) instead of
// committing alongside the synthetic rows. Once the seed finishes, the writer
// completes unmarked and the reset guard refuses the mixed dataset.
func TestM505EvalWriterBlocksWhileSeedHoldsLocks(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	forceSlowQualityInsertPause(t, d)

	type result struct {
		summary demo.QualitySummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := demo.RunQuality(ctx, qualityOptions(d, pinnedQualityClock()))
		done <- result{summary: summary, err: err}
	}()

	waitForDemoSeedLock(t, d)
	waitForSeedPause(t, d)

	// The seed holds LOCK TABLE ... IN EXCLUSIVE MODE on eval_runs, so this
	// writer must wait. A short deadline proves it cannot commit while the
	// seed is running.
	insertCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	_, err := d.Pool.Exec(insertCtx,
		`INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('agent', 'test', 'production-baseline', 'real-model', 'v9', 1, now())`)
	if err == nil {
		t.Fatal("concurrent eval_runs insert committed while the seed held its table locks")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent eval_runs insert error = %v, want context deadline (blocked by seed lock)", err)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("demo quality while a writer was blocked: %v", got.err)
	}

	// The writer releases after the seed commits; the real row must not be
	// adopted as demo evidence and the guard must refuse the mixed dataset.
	var realID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('agent', 'test', 'production-baseline', 'real-model', 'v9', 1, now())
		 RETURNING id::text`).Scan(&realID); err != nil {
		t.Fatalf("insert real eval run after the seed finished: %v", err)
	}
	var marked bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM demo_metadata
		                 WHERE table_name = 'eval_runs' AND row_key = $1)`, realID).
		Scan(&marked); err != nil {
		t.Fatalf("check real eval run marker: %v", err)
	}
	if marked {
		t.Fatal("real eval run was adopted into demo_metadata")
	}
	var evalRuns int64
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM eval_runs`).Scan(&evalRuns); err != nil {
		t.Fatalf("count eval runs: %v", err)
	}
	if evalRuns != 14 {
		t.Fatalf("eval_runs count = %d, want 14 (13 synthetic + 1 real)", evalRuns)
	}
	if err := demo.CheckQualityResetAllowed(ctx, d.DSN); !errors.Is(err, demo.ErrQualityUntaggedData) {
		t.Fatalf("guard after mixed eval rows = %v, want ErrQualityUntaggedData", err)
	}
}

// TestM505CommittedEvalRowFailsClosedBeforeLock is the F-3 regression for the
// commit-before-lock side. A real eval_runs row stays uncommitted while the
// seed starts, so the seed blocks on LOCK TABLE eval_runs. The row commits
// before the lock is granted; the post-lock READ COMMITTED presence check is
// authoritative and the seed fails closed with ErrQualityDataPresent and zero
// writes.
func TestM505CommittedEvalRowFailsClosedBeforeLock(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	side, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin side transaction: %v", err)
	}
	defer func() { _ = side.Rollback(ctx) }()
	if _, err := side.Exec(ctx,
		`INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('agent', 'test', 'production-baseline', 'real-model', 'v9', 1, now())`); err != nil {
		t.Fatalf("insert uncommitted real eval run: %v", err)
	}

	type result struct {
		summary demo.QualitySummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := demo.RunQuality(ctx, qualityOptions(d, pinnedQualityClock()))
		done <- result{summary: summary, err: err}
	}()

	waitForDemoSeedLock(t, d)
	waitForSeedTableLockWait(t, d, "eval_runs")
	if err := side.Commit(ctx); err != nil {
		t.Fatalf("commit real eval run: %v", err)
	}

	got := <-done
	if !errors.Is(got.err, demo.ErrQualityDataPresent) {
		t.Fatalf("seed error = %v, want ErrQualityDataPresent", got.err)
	}

	var evalRuns, evalResults, markers, cases, agentRuns int64
	if err := d.Pool.QueryRow(ctx, `SELECT
	    (SELECT count(*) FROM eval_runs),
	    (SELECT count(*) FROM eval_case_results),
	    (SELECT count(*) FROM demo_metadata),
	    (SELECT count(*) FROM cases),
	    (SELECT count(*) FROM agent_runs)`).
		Scan(&evalRuns, &evalResults, &markers, &cases, &agentRuns); err != nil {
		t.Fatalf("count rows after fail-closed seed: %v", err)
	}
	if evalRuns != 1 {
		t.Errorf("eval_runs count = %d, want 1 (only the real row)", evalRuns)
	}
	if evalResults != 0 || markers != 0 || cases != 0 || agentRuns != 0 {
		t.Errorf("fail-closed seed wrote synthetic rows: eval_case_results=%d demo_metadata=%d cases=%d agent_runs=%d, want 0",
			evalResults, markers, cases, agentRuns)
	}
}

// forceSlowQualityInsertPause pauses the seed inside its first synthetic
// eval_runs insert. The one-shot per-session setting and the model predicate
// keep the test's own real-model insert fast.
func forceSlowQualityInsertPause(t *testing.T, d *testdb.DB) {
	t.Helper()
	mustExec(t, d, `CREATE FUNCTION demo_quality_slow_ok() RETURNS trigger
	    LANGUAGE plpgsql AS $$
	    BEGIN
	        IF NEW.model LIKE 'demo-model-%'
	           AND current_setting('demo.quality_slow', true) IS DISTINCT FROM 'on' THEN
	            PERFORM set_config('demo.quality_slow', 'on', false);
	            PERFORM pg_sleep(3);
	        END IF;
	        RETURN NEW;
	    END
	    $$`)
	mustExec(t, d, `CREATE TRIGGER demo_quality_slow_ok
	    BEFORE INSERT ON eval_runs
	    FOR EACH ROW EXECUTE FUNCTION demo_quality_slow_ok()`)
}

// waitForSeedTableLockWait waits until the seed backend is queued on the
// EXCLUSIVE table lock for table.
func waitForSeedTableLockWait(t *testing.T, d *testdb.DB, table string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks
			  WHERE relation = $1::regclass
			    AND NOT granted`, "public."+table).Scan(&waiting); err != nil {
			t.Fatalf("inspect waiting relation lock on %s: %v", table, err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("seed never waited on the %s table lock", table)
}
