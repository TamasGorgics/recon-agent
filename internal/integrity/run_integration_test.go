//go:build integration

// covers: FIN-52
package integrity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/integrity"
	"github.com/TamasGorgics/recon-agent/internal/metrics"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

type recordingObserver struct {
	calls       []bool
	afterCommit func(passed bool)
}

func (o *recordingObserver) IntegrityRunCompleted(passed bool) {
	o.calls = append(o.calls, passed)
	if o.afterCommit != nil {
		o.afterCommit(passed)
	}
}

type recordingTieoutObserver struct {
	computed []tieout.Result
	failed   []string
}

func (o *recordingTieoutObserver) TieoutComputed(result tieout.Result) {
	o.computed = append(o.computed, result)
}

func (o *recordingTieoutObserver) TieoutFailed(code string) {
	o.failed = append(o.failed, code)
}

// TestRunCleanFixture proves the purpose-built clean world passes all five
// checks, persists exactly one row, enqueues no alert, and notifies observers
// only after the row exists.
func TestRunCleanFixture(t *testing.T) {
	f := newFixture(t)
	observer := &recordingObserver{}
	tieoutObserver := &recordingTieoutObserver{}
	observer.afterCommit = func(bool) {
		if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 1 {
			t.Errorf("observer saw %d integrity_runs rows, want 1 committed row", got)
		}
	}

	result, err := f.run(t, integrity.Options{
		Observer:       observer,
		TieoutObserver: tieoutObserver,
	})
	if err != nil {
		t.Fatalf("integrity run: %v", err)
	}
	if result.Report.Status != integrity.StatusPassed {
		t.Fatalf("run status = %q, want passed: %+v", result.Report.Status, result.Report.Checks)
	}
	if result.Report.CheckCount != 5 || result.Report.FailureCount != 0 {
		t.Fatalf("check_count/failure_count = %d/%d, want 5/0",
			result.Report.CheckCount, result.Report.FailureCount)
	}
	if result.Report.RunDate.String() != fixtureRunDate {
		t.Fatalf("run_date = %q, want %q", result.Report.RunDate, fixtureRunDate)
	}
	if result.ID == "" || result.Report.RunID != result.ID {
		t.Fatalf("run id = %q, report run_id = %q", result.ID, result.Report.RunID)
	}
	if result.Report.FinishedAt.Before(result.Report.StartedAt) {
		t.Fatalf("finished_at %s is before started_at %s", result.Report.FinishedAt, result.Report.StartedAt)
	}
	for _, check := range result.Report.Checks {
		if check.Status != integrity.StatusPassed {
			t.Errorf("check %q status = %q, want passed: %+v", check.Name, check.Status, check.Problems)
		}
		if check.Problems == nil {
			t.Errorf("check %q problems is nil, want []", check.Name)
		}
	}
	if got := checkByName(t, result.Report, integrity.CheckLedgerBalance).Checked; got != 2 {
		t.Errorf("ledger_balance checked = %d, want 2", got)
	}
	if got := checkByName(t, result.Report, integrity.CheckValueConservation).Checked; got != 1 {
		t.Errorf("value_conservation checked = %d, want 1", got)
	}
	if got := checkByName(t, result.Report, integrity.CheckAppliedProposalTrail).Checked; got != 1 {
		t.Errorf("applied_proposal_trail checked = %d, want 1", got)
	}
	if got := checkByName(t, result.Report, integrity.CheckAuditChain).Checked; got == 0 {
		t.Error("audit_chain checked = 0, want the seeded audit rows")
	}
	tieoutCheck := checkByName(t, result.Report, integrity.CheckTieout)
	if tieoutCheck.Checked != 1 || tieoutCheck.Skipped != 0 {
		t.Errorf("tieout checked/skipped = %d/%d, want 1/0", tieoutCheck.Checked, tieoutCheck.Skipped)
	}

	if len(observer.calls) != 1 || observer.calls[0] != true {
		t.Fatalf("integrity observer calls = %v, want [true]", observer.calls)
	}
	if len(tieoutObserver.computed) != 1 {
		t.Fatalf("tieout computed observations = %d, want 1", len(tieoutObserver.computed))
	}
	if len(tieoutObserver.failed) != 0 {
		t.Fatalf("tieout failed observations = %v, want none", tieoutObserver.failed)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 1 {
		t.Fatalf("integrity_runs rows = %d, want 1", got)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM outbox WHERE topic = 'integrity.failed'`); got != 0 {
		t.Fatalf("integrity.failed outbox rows = %d, want 0 for a passing run", got)
	}
}

// TestRunFailedPersistsOneRowAndAlert proves a completed failure is (Result,
// nil), writes one failed row, and enqueues exactly one bounded alert.
func TestRunFailedPersistsOneRowAndAlert(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE statement_balances SET closing_minor = closing_minor + 100 WHERE id = $1::uuid`,
		f.statementID); err != nil {
		t.Fatalf("corrupt statement closing: %v", err)
	}
	observer := &recordingObserver{}

	result, err := f.run(t, integrity.Options{Observer: observer})
	if err != nil {
		t.Fatalf("a completed failed run must not be a Go error: %v", err)
	}
	if result.Report.Status != integrity.StatusFailed || result.Report.FailureCount != 1 {
		t.Fatalf("status/failure_count = %q/%d, want failed/1",
			result.Report.Status, result.Report.FailureCount)
	}
	problem := requireProblemCode(t, result.Report, integrity.CheckTieout, integrity.CodeTieoutUnexplained)
	if !strings.Contains(problem.Detail, "unexplained_minor=-100") {
		t.Fatalf("tieout detail = %q, want unexplained_minor=-100", problem.Detail)
	}

	var rowID string
	var status string
	var checkCount, failureCount int
	var checksText string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT id::text, status, check_count, failure_count, checks::text
		   FROM integrity_runs`).Scan(&rowID, &status, &checkCount, &failureCount, &checksText); err != nil {
		t.Fatalf("read persisted run: %v", err)
	}
	if rowID != result.ID || status != "failed" || checkCount != 5 || failureCount != 1 {
		t.Fatalf("persisted row = id %s status %s checks %d failures %d",
			rowID, status, checkCount, failureCount)
	}
	var checks []map[string]any
	if err := json.Unmarshal([]byte(checksText), &checks); err != nil {
		t.Fatalf("persisted checks are not a JSON array: %v", err)
	}
	if len(checks) != 5 {
		t.Fatalf("persisted checks length = %d, want 5", len(checks))
	}

	var alertID, alertPayload, alertKey string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT id::text, payload::text, idempotency_key
		   FROM outbox WHERE topic = 'integrity.failed'`).
		Scan(&alertID, &alertPayload, &alertKey); err != nil {
		t.Fatalf("read integrity.failed alert: %v", err)
	}
	if alertKey != "integrity.failed:"+result.ID {
		t.Fatalf("alert idempotency key = %q, want integrity.failed:%s", alertKey, result.ID)
	}
	var payload struct {
		RunID         string         `json:"run_id"`
		RunDate       string         `json:"run_date"`
		Status        string         `json:"status"`
		CheckCount    int            `json:"check_count"`
		FailureCount  int            `json:"failure_count"`
		FailedChecks  []string       `json:"failed_checks"`
		ProblemCounts map[string]int `json:"problem_counts"`
		TriggeredBy   string         `json:"triggered_by"`
	}
	if err := json.Unmarshal([]byte(alertPayload), &payload); err != nil {
		t.Fatalf("alert payload is not JSON: %v", err)
	}
	if payload.RunID != result.ID || payload.RunDate != fixtureRunDate ||
		payload.Status != "failed" || payload.CheckCount != 5 || payload.FailureCount != 1 ||
		payload.TriggeredBy != "manual" {
		t.Fatalf("alert payload = %+v", payload)
	}
	if len(payload.FailedChecks) != 1 || payload.FailedChecks[0] != "tieout" {
		t.Fatalf("alert failed_checks = %v, want [tieout]", payload.FailedChecks)
	}
	if payload.ProblemCounts["tieout"] != 1 || len(payload.ProblemCounts) != 1 {
		t.Fatalf("alert problem_counts = %v, want tieout:1", payload.ProblemCounts)
	}

	if len(observer.calls) != 1 || observer.calls[0] != false {
		t.Fatalf("integrity observer calls = %v, want [false]", observer.calls)
	}
	if count := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); count != 1 {
		t.Fatalf("integrity_runs rows = %d, want 1", count)
	}
	if count := countRows(t, f.db.Pool, `SELECT count(*) FROM outbox WHERE topic = 'integrity.failed'`); count != 1 {
		t.Fatalf("integrity.failed rows = %d, want 1", count)
	}
}

// TestRunLockLoser proves the advisory lock loser writes nothing, alerts
// nothing, and observes nothing; the winner's transaction-scoped lock releases.
func TestRunLockLoser(t *testing.T) {
	f := newFixture(t)
	app := f.appPool(t)
	ctx := context.Background()
	lockTx, err := app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	var locked bool
	if err := lockTx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`,
		integrity.AdvisoryLockKey).Scan(&locked); err != nil {
		t.Fatalf("acquire fixture lock: %v", err)
	}
	if !locked {
		t.Fatal("fixture failed to acquire the integrity lock")
	}

	observer := &recordingObserver{}
	if _, err := f.run(t, integrity.Options{Observer: observer}); !errors.Is(err, integrity.ErrAlreadyRunning) {
		t.Fatalf("lock loser error = %v, want ErrAlreadyRunning", err)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 0 {
		t.Fatalf("lock loser wrote %d integrity_runs rows, want 0", got)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM outbox WHERE topic = 'integrity.failed'`); got != 0 {
		t.Fatalf("lock loser wrote %d alerts, want 0", got)
	}
	if len(observer.calls) != 0 {
		t.Fatalf("lock loser observer calls = %v, want none", observer.calls)
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release fixture lock: %v", err)
	}
	if result := f.mustRun(t, integrity.Options{}); result.Report.Status != integrity.StatusPassed {
		t.Fatalf("run after lock release status = %q, want passed", result.Report.Status)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 1 {
		t.Fatalf("integrity_runs rows after lock release = %d, want 1", got)
	}
}

// TestRunCancel writes no row, no alert, and no observation.
func TestRunCancel(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observer := &recordingObserver{}
	_, err := integrity.Run(ctx, f.appPool(t), integrity.Options{
		Clock:    f.clock,
		RunDate:  integrity.NewDate(mustDate(t, fixtureRunDate)),
		Observer: observer,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run error = %v, want context.Canceled", err)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 0 {
		t.Fatalf("canceled run wrote %d rows, want 0", got)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM outbox WHERE topic = 'integrity.failed'`); got != 0 {
		t.Fatalf("canceled run wrote %d alerts, want 0", got)
	}
	if len(observer.calls) != 0 {
		t.Fatalf("canceled run observer calls = %v, want none", observer.calls)
	}
}

// TestRunCheckErrorSavepoint fails one check with a real SQL permission error
// and proves the savepoint keeps the transaction usable so the failed row is
// still persisted and the other checks still run.
func TestRunCheckErrorSavepoint(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.db.Pool.Exec(ctx, `REVOKE SELECT ON ledger_txns FROM recon_app`); err != nil {
		t.Fatalf("revoke ledger read: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.db.Pool.Exec(context.Background(), `GRANT SELECT, INSERT ON ledger_txns TO recon_app`)
	})

	result, err := f.run(t, integrity.Options{})
	if err != nil {
		t.Fatalf("savepoint run must complete with a failed report, got error: %v", err)
	}
	if result.Report.Status != integrity.StatusFailed {
		t.Fatalf("status = %q, want failed", result.Report.Status)
	}
	ledger := checkByName(t, result.Report, integrity.CheckLedgerBalance)
	if ledger.Status != integrity.StatusFailed {
		t.Fatalf("ledger_balance status = %q, want failed", ledger.Status)
	}
	if len(ledger.Problems) != 1 || ledger.Problems[0].Code != integrity.CodeCheckError {
		t.Fatalf("ledger_balance problems = %+v, want one check_error", ledger.Problems)
	}
	if ledger.Problems[0].Detail != "query_failed" {
		t.Fatalf("check_error detail = %q, want query_failed", ledger.Problems[0].Detail)
	}
	// Checks after the failing check still produced real results.
	if tieoutCheck := checkByName(t, result.Report, integrity.CheckTieout); tieoutCheck.Checked != 1 {
		t.Fatalf("tieout checked = %d, want 1 (other checks continued)", tieoutCheck.Checked)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 1 {
		t.Fatalf("savepoint run wrote %d rows, want 1", got)
	}
}

// TestRunReportCap persists the true pre-cap problem_count with exactly 100
// retained problems and truncated=true.
func TestRunReportCap(t *testing.T) {
	f := newFixture(t)
	f.tamper(t, func(ctx context.Context, tx pgx.Tx) {
		for i := 0; i < 150; i++ {
			key := fmt.Sprintf("cap-entry-%d", i)
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_txns
				     (value_date, kind, idempotency_key, request_hash, description, period_id)
				 VALUES ($1::date, 'charge', $2, 'fixture-hash', '', $3::uuid)`,
				fixtureValueDay, key, f.periodID); err != nil {
				t.Fatalf("insert zero-entry txn %d: %v", i, err)
			}
		}
	})

	result := f.mustRun(t, integrity.Options{})
	ledger := checkByName(t, result.Report, integrity.CheckLedgerBalance)
	if ledger.ProblemCount != 150 {
		t.Fatalf("ledger problem_count = %d, want 150 (pre-cap)", ledger.ProblemCount)
	}
	if len(ledger.Problems) != integrity.MaxPersistedProblems {
		t.Fatalf("ledger retained problems = %d, want %d", len(ledger.Problems), integrity.MaxPersistedProblems)
	}
	if !ledger.Truncated {
		t.Fatal("ledger truncated = false, want true")
	}
	var persistedCount, persistedLen int
	var truncated bool
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT (checks->0->>'problem_count')::int,
		        jsonb_array_length(checks->0->'problems'),
		        (checks->0->>'truncated')::boolean
		   FROM integrity_runs`).Scan(&persistedCount, &persistedLen, &truncated); err != nil {
		t.Fatalf("read persisted capped check: %v", err)
	}
	if persistedCount != 150 || persistedLen != integrity.MaxPersistedProblems || !truncated {
		t.Fatalf("persisted cap = count %d len %d truncated %t", persistedCount, persistedLen, truncated)
	}
}

// TestRunSentinelRedaction proves a raw external memo sentinel never reaches
// the report, the persisted checks JSON, or the outbox payload.
func TestRunSentinelRedaction(t *testing.T) {
	f := newFixture(t)
	if got := countRows(t, f.db.Pool,
		`SELECT count(*) FROM external_records WHERE raw->>'memo' = $1`, sentinelMemo); got != 1 {
		t.Fatalf("fixture sentinel rows = %d, want 1", got)
	}
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE statement_balances SET closing_minor = closing_minor + 100 WHERE id = $1::uuid`,
		f.statementID); err != nil {
		t.Fatalf("corrupt statement closing: %v", err)
	}

	result := f.mustRun(t, integrity.Options{})
	if result.Report.Status != integrity.StatusFailed {
		t.Fatalf("status = %q, want failed", result.Report.Status)
	}
	reportJSON, err := json.Marshal(result.Report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if strings.Contains(string(reportJSON), sentinelMemo) {
		t.Fatal("report JSON contains the raw memo sentinel")
	}
	var checksText, payloadText string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT checks::text FROM integrity_runs WHERE id = $1::uuid`, result.ID).
		Scan(&checksText); err != nil {
		t.Fatalf("read persisted checks: %v", err)
	}
	if strings.Contains(checksText, sentinelMemo) {
		t.Fatal("persisted checks JSON contains the raw memo sentinel")
	}
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT payload::text FROM outbox WHERE topic = 'integrity.failed'`).
		Scan(&payloadText); err != nil {
		t.Fatalf("read alert payload: %v", err)
	}
	if strings.Contains(payloadText, sentinelMemo) {
		t.Fatal("outbox payload contains the raw memo sentinel")
	}
}

// TestRunReportShape pins the persisted checks wire shape: an array in frozen
// order, snake_case fields only, [] problems, and a consistent problem_count.
func TestRunReportShape(t *testing.T) {
	f := newFixture(t)
	f.setStatementClosing(t, f.statementID, 10100)
	result := f.mustRun(t, integrity.Options{})

	var checksText string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT checks::text FROM integrity_runs WHERE id = $1::uuid`, result.ID).
		Scan(&checksText); err != nil {
		t.Fatalf("read persisted checks: %v", err)
	}
	var checks []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(checksText), &checks); err != nil {
		t.Fatalf("persisted checks are not a JSON array: %v", err)
	}
	if len(checks) != len(integrity.CheckOrder) {
		t.Fatalf("persisted checks length = %d, want %d", len(checks), len(integrity.CheckOrder))
	}
	checkKeys := []string{"name", "status", "checked", "skipped", "problem_count", "truncated", "problems"}
	problemKeys := []string{"code", "entity", "id", "detail"}
	for i, check := range checks {
		if len(check) != len(checkKeys) {
			t.Fatalf("check %d fields = %v, want exactly %v", i, check, checkKeys)
		}
		for _, key := range checkKeys {
			if _, ok := check[key]; !ok {
				t.Fatalf("check %d is missing %q: %s", i, key, checksText)
			}
		}
		var name string
		if err := json.Unmarshal(check["name"], &name); err != nil {
			t.Fatalf("decode check %d name: %v", i, err)
		}
		if name != string(integrity.CheckOrder[i]) {
			t.Fatalf("check %d = %q, want %q", i, name, integrity.CheckOrder[i])
		}
		var problemCount int
		if err := json.Unmarshal(check["problem_count"], &problemCount); err != nil {
			t.Fatalf("decode check %d problem_count: %v", i, err)
		}
		var problems []map[string]json.RawMessage
		if err := json.Unmarshal(check["problems"], &problems); err != nil {
			t.Fatalf("decode check %d problems: %v", i, err)
		}
		if problems == nil {
			t.Fatalf("check %d problems is null, want []", i)
		}
		if problemCount != len(problems) {
			t.Fatalf("check %d problem_count = %d, want len(problems) %d",
				i, problemCount, len(problems))
		}
		for j, problem := range problems {
			if len(problem) != len(problemKeys) {
				t.Fatalf("check %d problem %d fields = %v, want %v", i, j, problem, problemKeys)
			}
			for _, key := range problemKeys {
				if _, ok := problem[key]; !ok {
					t.Fatalf("check %d problem %d is missing %q", i, j, key)
				}
			}
		}
	}
}

// TestRunCancelLeavesGaugeAtSeed proves a canceled run writes no row and never
// reaches the post-commit observer, so the real gauge stays at its seed 0.
func TestRunCancelLeavesGaugeAtSeed(t *testing.T) {
	f := newFixture(t)
	registry, err := metrics.New(metrics.Options{Component: "worker"})
	if err != nil {
		t.Fatalf("build metrics: %v", err)
	}
	observer := metrics.NewIntegrityObserver(registry)
	if got := gatheredGauge(t, registry, metrics.MetricIntegrityJobStatus,
		map[string]string{"job": metrics.JobIntegrity}); got != 0 {
		t.Fatalf("seed integrity gauge = %v, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = integrity.Run(ctx, f.appPool(t), integrity.Options{
		Clock:    f.clock,
		RunDate:  integrity.NewDate(mustDate(t, fixtureRunDate)),
		Observer: observer,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run error = %v, want context.Canceled", err)
	}
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM integrity_runs`); got != 0 {
		t.Fatalf("canceled run wrote %d rows, want 0", got)
	}
	if got := gatheredGauge(t, registry, metrics.MetricIntegrityJobStatus,
		map[string]string{"job": metrics.JobIntegrity}); got != 0 {
		t.Fatalf("canceled run set the integrity gauge = %v, want 0", got)
	}
}

// TestRunTieoutObserverProducesGauge proves the real tie-out adapter receives
// a fixture computation and records the signed unexplained amount.
func TestRunTieoutObserverProducesGauge(t *testing.T) {
	f := newFixture(t)
	f.setStatementClosing(t, f.statementID, 10100)
	registry, err := metrics.New(metrics.Options{Component: "worker"})
	if err != nil {
		t.Fatalf("build metrics: %v", err)
	}
	result := f.mustRun(t, integrity.Options{
		Observer:       metrics.NewIntegrityObserver(registry),
		TieoutObserver: metrics.NewTieoutObserver(registry),
	})
	if result.Report.Status != integrity.StatusFailed {
		t.Fatalf("status = %q, want failed", result.Report.Status)
	}
	if got := gatheredGauge(t, registry, metrics.MetricTieoutUnexplainedMinor,
		map[string]string{"currency": "USD", "source": "psp_a"}); got != -100 {
		t.Fatalf("tie-out gauge = %v, want -100", got)
	}
	if got := gatheredGauge(t, registry, metrics.MetricIntegrityJobStatus,
		map[string]string{"job": metrics.JobIntegrity}); got != 2 {
		t.Fatalf("integrity gauge = %v, want 2 (failed)", got)
	}
}

// gatheredGauge returns the gauge value whose labels contain every wanted pair.
func gatheredGauge(t *testing.T, registry *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := registry.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := true
			for key, want := range labels {
				found := false
				for _, label := range metric.GetLabel() {
					if label.GetName() == key && label.GetValue() == want {
						found = true
						break
					}
				}
				if !found {
					matched = false
					break
				}
			}
			if matched {
				return metric.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return 0
}

// TestEmptyDatabasePasses pins the vacuous-pass contract: no data means no
// detected inconsistency, not evidence of coverage.
func TestEmptyDatabasePasses(t *testing.T) {
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 4})
	clk := clock.NewFake(mustTime(t, fixtureNow))
	rolePool, err := db.OpenRole(context.Background(), db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(rolePool.Close)

	result, err := integrity.Run(context.Background(), rolePool.Pool, integrity.Options{
		Clock:   clk,
		RunDate: integrity.NewDate(mustDate(t, fixtureRunDate)),
	})
	if err != nil {
		t.Fatalf("empty run: %v", err)
	}
	if result.Report.Status != integrity.StatusPassed || result.Report.FailureCount != 0 {
		t.Fatalf("empty run status/failure_count = %q/%d, want passed/0",
			result.Report.Status, result.Report.FailureCount)
	}
	for _, check := range result.Report.Checks {
		if check.Status != integrity.StatusPassed {
			t.Errorf("empty check %q status = %q", check.Name, check.Status)
		}
	}
	if auditCheck := checkByName(t, result.Report, integrity.CheckAuditChain); auditCheck.Checked != 0 {
		t.Errorf("empty audit_chain checked = %d, want 0", auditCheck.Checked)
	}
	tieoutCheck := checkByName(t, result.Report, integrity.CheckTieout)
	if tieoutCheck.Checked != 0 || tieoutCheck.Skipped != 0 {
		t.Errorf("empty tieout checked/skipped = %d/%d, want 0/0", tieoutCheck.Checked, tieoutCheck.Skipped)
	}
	if got := countRows(t, d.Pool, `SELECT count(*) FROM outbox WHERE topic = 'integrity.failed'`); got != 0 {
		t.Fatalf("empty run wrote %d alerts, want 0", got)
	}
}
