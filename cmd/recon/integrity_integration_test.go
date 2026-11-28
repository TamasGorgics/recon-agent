//go:build integration

// covers: FIN-52
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/integrity"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

const integrityCLIRunDate = "2026-06-15"

func newIntegrityCLIDB(t *testing.T) *testdb.DB {
	t.Helper()
	d := testdb.New(t)
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", d.DSN)
	t.Setenv("DATABASE_APP_URL", d.DSNFor(db.RoleApp))
	t.Setenv("DATABASE_AGENT_URL", "")
	return d
}

func runIntegrityCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runIntegrityTo(context.Background(), args, &out)
	return out.String(), err
}

func runIntegrityCLIWithClock(t *testing.T, clk clock.Clock, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runIntegrityToWithClock(context.Background(), args, &out, clk)
	return out.String(), err
}

func decodeIntegrityReport(t *testing.T, out string) integrity.Report {
	t.Helper()
	var report integrity.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		t.Fatalf("decode report JSON: %v\n%s", err, out)
	}
	return report
}

func integrityCLICheck(t *testing.T, report integrity.Report, name integrity.CheckName) integrity.CheckResult {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("report has no %q check", name)
	return integrity.CheckResult{}
}

func TestIntegrityCLIDefaultDateJSONAndManualTrigger(t *testing.T) {
	d := newIntegrityCLIDB(t)
	fake := clock.NewFake(time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC))

	out, err := runIntegrityCLIWithClock(t, fake, "run", "--json")
	if err != nil {
		t.Fatalf("integrity run --json: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// The stable envelope has every frozen field, and run_date is a date
	// string rather than an RFC3339 timestamp.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	for _, field := range []string{
		"run_id", "run_date", "started_at", "finished_at", "status",
		"check_count", "failure_count", "checks",
	} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("JSON envelope is missing %q: %s", field, out)
		}
	}

	report := decodeIntegrityReport(t, out)
	if report.RunID == "" || report.RunID != strings.Trim(string(raw["run_id"]), `"`) {
		t.Fatalf("run_id = %q", report.RunID)
	}
	if report.RunDate.String() != integrityCLIRunDate {
		t.Fatalf("run_date = %q, want the fake clock date %q", report.RunDate, integrityCLIRunDate)
	}
	if report.Status != integrity.StatusPassed || report.CheckCount != 5 || report.FailureCount != 0 {
		t.Fatalf("report = status %q checks %d failures %d",
			report.Status, report.CheckCount, report.FailureCount)
	}
	if len(report.Checks) != len(integrity.CheckOrder) {
		t.Fatalf("checks length = %d, want %d", len(report.Checks), len(integrity.CheckOrder))
	}
	for i, name := range integrity.CheckOrder {
		if report.Checks[i].Name != name {
			t.Fatalf("check %d = %q, want %q", i, report.Checks[i].Name, name)
		}
	}
	if report.StartedAt.IsZero() || report.FinishedAt.IsZero() ||
		report.StartedAt.Location() != time.UTC || report.FinishedAt.Location() != time.UTC {
		t.Fatalf("timestamps are not UTC: %s / %s", report.StartedAt, report.FinishedAt)
	}

	var runDate, status, triggeredBy string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT run_date::text, status, triggered_by FROM integrity_runs`).
		Scan(&runDate, &status, &triggeredBy); err != nil {
		t.Fatalf("read integrity_runs row: %v", err)
	}
	if runDate != integrityCLIRunDate || status != "passed" || triggeredBy != "manual" {
		t.Fatalf("persisted row = date %s status %s trigger %s", runDate, status, triggeredBy)
	}
}

func TestIntegrityCLIUsageErrorsAreExitTwo(t *testing.T) {
	d := newIntegrityCLIDB(t)
	cases := []struct {
		name string
		args []string
	}{
		{name: "missing command"},
		{name: "unknown command", args: []string{"verify"}},
		{name: "invalid date", args: []string{"run", "--date", "2026-6-1"}},
		{name: "impossible date", args: []string{"run", "--date", "2026-02-30"}},
		{name: "extra argument", args: []string{"run", "--date", integrityCLIRunDate, "extra"}},
		{name: "unknown flag", args: []string{"run", "--jsonl"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runIntegrityCLI(t, tc.args...)
			if err == nil {
				t.Fatalf("run %v succeeded, want a usage error", tc.args)
			}
			if code := exitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2 (%v)", code, err)
			}
			if out != "" {
				t.Fatalf("usage error wrote a report to stdout: %q", out)
			}
		})
	}
	if got := countIntegrityRows(t, d); got != 0 {
		t.Fatalf("usage errors wrote %d integrity_runs rows", got)
	}
}

func TestIntegrityCLIMissingAppURLNeverFallsBackToAdmin(t *testing.T) {
	d := newIntegrityCLIDB(t)
	t.Setenv("DATABASE_APP_URL", "")
	out, err := runIntegrityCLI(t, "run", "--date", integrityCLIRunDate)
	if err == nil {
		t.Fatal("run succeeded without DATABASE_APP_URL")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "database.app_url") {
		t.Fatalf("error = %q, want database.app_url guidance", err)
	}
	if out != "" {
		t.Fatalf("missing app URL wrote a report: %q", out)
	}
	if got := countIntegrityRows(t, d); got != 0 {
		t.Fatalf("missing app URL wrote %d rows through the admin DSN", got)
	}
}

func TestIntegrityCLIUsesAppRoleNotAdmin(t *testing.T) {
	d := newIntegrityCLIDB(t)
	ctx := context.Background()
	// The owner/maintenance pool can still insert; revoking the app role's
	// INSERT proves the CLI connected as recon_app and did not fall back.
	if _, err := d.Pool.Exec(ctx, `REVOKE INSERT ON integrity_runs FROM recon_app`); err != nil {
		t.Fatalf("revoke app insert: %v", err)
	}
	out, err := runIntegrityCLI(t, "run", "--date", integrityCLIRunDate, "--json")
	if err == nil {
		t.Fatalf("run succeeded despite the revoked app-role grant: %s", out)
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	if got := countIntegrityRows(t, d); got != 0 {
		t.Fatalf("failed app-role run wrote %d rows; the CLI used the admin role", got)
	}
}

func TestIntegrityCLIFailedRunExitsOneWithJSONAndAlert(t *testing.T) {
	d := newIntegrityCLIDB(t)
	seedZeroEntryLedgerTxn(t, d)

	out, err := runIntegrityCLI(t, "run", "--date", integrityCLIRunDate, "--json")
	if err == nil {
		t.Fatal("a completed failed run must return an exit error")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1 (%v)", code, err)
	}
	report := decodeIntegrityReport(t, out)
	if report.Status != integrity.StatusFailed || report.FailureCount != 1 {
		t.Fatalf("report = status %q failures %d, want failed/1",
			report.Status, report.FailureCount)
	}
	ledger := integrityCLICheck(t, report, integrity.CheckLedgerBalance)
	if ledger.Status != integrity.StatusFailed || ledger.ProblemCount != 1 {
		t.Fatalf("ledger_balance = status %q problems %d, want failed/1",
			ledger.Status, ledger.ProblemCount)
	}
	if len(ledger.Problems) != 1 || ledger.Problems[0].Code != integrity.CodeLedgerTxnNoEntries {
		t.Fatalf("ledger problems = %+v, want one ledger_txn_no_entries", ledger.Problems)
	}

	var status, triggeredBy string
	var failureCount int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT status, triggered_by, failure_count FROM integrity_runs`).
		Scan(&status, &triggeredBy, &failureCount); err != nil {
		t.Fatalf("read failed run row: %v", err)
	}
	if status != "failed" || triggeredBy != "manual" || failureCount != 1 {
		t.Fatalf("persisted row = status %s trigger %s failures %d", status, triggeredBy, failureCount)
	}
	var payloadText, key string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT payload::text, idempotency_key FROM outbox WHERE topic = 'integrity.failed'`).
		Scan(&payloadText, &key); err != nil {
		t.Fatalf("read integrity.failed row: %v", err)
	}
	if key != "integrity.failed:"+report.RunID {
		t.Fatalf("alert key = %q, want integrity.failed:%s", key, report.RunID)
	}
	var payload struct {
		RunID        string   `json:"run_id"`
		FailedChecks []string `json:"failed_checks"`
	}
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		t.Fatalf("decode alert payload: %v", err)
	}
	if payload.RunID != report.RunID || len(payload.FailedChecks) != 1 ||
		payload.FailedChecks[0] != string(integrity.CheckLedgerBalance) {
		t.Fatalf("alert payload = %+v", payload)
	}
	if strings.Contains(payloadText, "cli-zero-entry") {
		t.Fatalf("alert payload leaked a raw fixture value: %s", payloadText)
	}
}

func TestIntegrityCLIHumanOutputIncludesCheckCountsAndCodes(t *testing.T) {
	d := newIntegrityCLIDB(t)
	seedZeroEntryLedgerTxn(t, d)

	out, err := runIntegrityCLI(t, "run", "--date", integrityCLIRunDate)
	if err == nil || exitCode(err) != 1 {
		t.Fatalf("human failed run = %v, exit %d", err, exitCode(err))
	}
	for _, want := range []string{
		"integrity run: FAILED",
		"date=" + integrityCLIRunDate,
		"status=failed",
		"checks=5 failures=1",
		"ledger_balance failed checked=1 skipped=0 problems=1",
		"codes=ledger_txn_no_entries",
		"use --json or the daily report",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cli-zero-entry") {
		t.Fatalf("human output leaked a raw fixture value:\n%s", out)
	}
}

func TestIntegrityCLIDateScopesTieoutOnly(t *testing.T) {
	d := newIntegrityCLIDB(t)
	seedTieoutScope(t, d, "2026-06-14")

	out, err := runIntegrityCLI(t, "run", "--date", "2026-06-15", "--json")
	if err != nil {
		t.Fatalf("run with the later date: %v", err)
	}
	report := decodeIntegrityReport(t, out)
	tieout := integrityCLICheck(t, report, integrity.CheckTieout)
	if tieout.Status != integrity.StatusPassed || tieout.Checked != 1 || tieout.Skipped != 0 {
		t.Fatalf("2026-06-15 tieout = status %q checked %d skipped %d, want passed/1/0",
			tieout.Status, tieout.Checked, tieout.Skipped)
	}

	out, err = runIntegrityCLI(t, "run", "--date", "2026-06-13", "--json")
	if err != nil {
		t.Fatalf("run with the earlier date: %v", err)
	}
	report = decodeIntegrityReport(t, out)
	if report.RunDate.String() != "2026-06-13" {
		t.Fatalf("run_date = %q, want 2026-06-13", report.RunDate)
	}
	tieout = integrityCLICheck(t, report, integrity.CheckTieout)
	if tieout.Status != integrity.StatusPassed || tieout.Checked != 0 || tieout.Skipped != 1 {
		t.Fatalf("2026-06-13 tieout = status %q checked %d skipped %d, want passed/0/1",
			tieout.Status, tieout.Checked, tieout.Skipped)
	}
	// Other checks are current-state checks; the only check whose input the
	// date scopes is tieout. Ledger balance still sees the current ledger.
	ledger := integrityCLICheck(t, report, integrity.CheckLedgerBalance)
	if ledger.Status != integrity.StatusPassed || ledger.Checked != 0 {
		t.Fatalf("earlier-date ledger = status %q checked %d, want passed/0",
			ledger.Status, ledger.Checked)
	}
}

func TestIntegrityCLIHumanPassedOutput(t *testing.T) {
	newIntegrityCLIDB(t)
	out, err := runIntegrityCLI(t, "run", "--date", integrityCLIRunDate)
	if err != nil {
		t.Fatalf("human passed run: %v", err)
	}
	for _, want := range []string{
		"integrity run: PASSED",
		"date=" + integrityCLIRunDate,
		"status=passed",
		"checks=5 failures=0",
		"ledger_balance passed checked=0 skipped=0 problems=0",
		"tieout passed checked=0 skipped=0 problems=0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output is missing %q:\n%s", want, out)
		}
	}
}

func countIntegrityRows(t *testing.T, d *testdb.DB) int64 {
	t.Helper()
	var count int64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM integrity_runs`).Scan(&count); err != nil {
		t.Fatalf("count integrity_runs: %v", err)
	}
	return count
}

// seedZeroEntryLedgerTxn inserts a transaction with no entries under owner
// replica mode and its expected ledger.post audit row. The deferred balance
// trigger rejects this on the normal path, so the seed is the frozen corruption
// path; the audit row keeps applied_proposal_trail green so the test asserts
// only the ledger_balance failure.
func seedZeroEntryLedgerTxn(t *testing.T, d *testdb.DB) {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	var periodID string
	if err := tx.QueryRow(ctx,
		`SELECT id::text FROM periods ORDER BY start_date LIMIT 1`).Scan(&periodID); err != nil {
		t.Fatalf("read period: %v", err)
	}
	var txnID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO ledger_txns
		    (value_date, kind, idempotency_key, request_hash, description, period_id)
		VALUES ('2026-06-01', 'charge', 'cli-zero-entry', 'cli-fixture-hash', 'cli fixture', $1::uuid)
		RETURNING id::text`,
		periodID).Scan(&txnID); err != nil {
		t.Fatalf("insert zero-entry txn: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = origin`); err != nil {
		t.Fatalf("restore triggers: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}

	actor, err := audit.SystemActor("cli-fixture")
	if err != nil {
		t.Fatalf("fixture actor: %v", err)
	}
	testutil.AppendAudit(t, ctx, d.Pool, audit.Entry{
		Actor:      actor,
		Action:     "ledger.post",
		EntityType: "ledger_txn",
		EntityID:   txnID,
		Timestamp:  time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	})
}

// seedTieoutScope inserts an asset clearing account, a scope, and a statement
// with zero opening/closing at asOf. With no ledger activity the scope ties.
func seedTieoutScope(t *testing.T, d *testdb.DB, asOf string) {
	t.Helper()
	ctx := context.Background()
	var accountID string
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO accounts (code, name, type, currency, status)
		VALUES ('cli_clearing_usd', 'CLI clearing', 'asset', 'USD', 'active')
		RETURNING id::text`).Scan(&accountID); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	var scopeID string
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		VALUES ($1::uuid, 'cli_source', 'USD') RETURNING id::text`, accountID).
		Scan(&scopeID); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	var batchID string
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO settlement_batches
		    (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		VALUES ('cli_source', 0, 'USD', 'cli-statement-hash', 0, 0)
		RETURNING id::text`).Scan(&batchID); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
		INSERT INTO statement_balances
		    (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
		VALUES ($1::uuid, $2::date, 0, 0, 'USD', $3::uuid)`,
		scopeID, asOf, batchID); err != nil {
		t.Fatalf("insert statement: %v", err)
	}
}
