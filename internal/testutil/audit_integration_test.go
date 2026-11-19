//go:build integration

// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package testutil

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain keeps the migration template current before any testutil test
// clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "testutil: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "testutil: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

var auditTestNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func testAuditEntry(action, entityType, entityID string) audit.Entry {
	actor, err := audit.SystemActor("testutil")
	if err != nil {
		panic(err)
	}
	return audit.Entry{
		Actor:      actor,
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		Timestamp:  auditTestNow,
	}
}

func mustTestActor(t *testing.T, id string) audit.Actor {
	t.Helper()
	actor, err := audit.SystemActor(id)
	if err != nil {
		t.Fatalf("system actor %q: %v", id, err)
	}
	return actor
}

func seedAudit(t *testing.T, d *testdb.DB, entries ...audit.Entry) {
	t.Helper()
	ctx := context.Background()
	for _, entry := range entries {
		AppendAudit(t, ctx, d.Pool, entry)
	}
}

// TestAuditCompletenessAgainstRealAuditLog exercises both public helpers and
// the failure paths against rows read through the real query.
func TestAuditCompletenessAgainstRealAuditLog(t *testing.T) {
	d := testdb.New(t)
	seedAudit(t, d,
		testAuditEntry("case.create", "case", "case-a"),
		testAuditEntry("case.create", "case", "case-a"),
		testAuditEntry("case.transition", "case", "case-a"),
		testAuditEntry("break.create", "break", "break-b"),
		testAuditEntry("ledger.post", "ledger_txn", "txn-outside"),
	)

	inScope := []Expectation{
		{Action: "case.create", EntityType: "case", EntityID: "case-a"},
		{Action: "case.transition", EntityType: "case", EntityID: "case-a"},
		{Action: "break.create", EntityType: "break", EntityID: "break-b"},
	}

	t.Run("all expected present with duplicate rows and expectations", func(t *testing.T) {
		expected := append([]Expectation{}, inScope...)
		expected = append(expected, inScope[0], inScope[0])
		AssertAuditCompleteness(t, d.Pool, expected)
		AssertAuditCompletenessExact(t, d.Pool, expected, "case-a", "break-b")
	})

	t.Run("exact global rejects an outside row", func(t *testing.T) {
		rec := &recordingReporter{}
		checkAuditCompleteness(rec, inScope, loadAuditTriples(t, d.Pool), nil, true)
		msg := rec.onlyFailure(t)
		if !strings.Contains(msg, "1 unexpected audit triples in scope") {
			t.Errorf("failure omits the unexpected count:\n%s", msg)
		}
		if !strings.Contains(msg, `action="ledger.post" entity_type="ledger_txn" entity_id="txn-outside"`) {
			t.Errorf("failure omits the unexpected outside triple:\n%s", msg)
		}
	})

	t.Run("exact scope includes only named entity IDs", func(t *testing.T) {
		rec := &recordingReporter{}
		checkAuditCompleteness(rec, inScope, loadAuditTriples(t, d.Pool),
			map[string]struct{}{"case-a": {}, "break-b": {}}, true)
		if len(rec.failures) != 0 {
			t.Fatalf("failures = %v, want none: txn-outside must be ignored", rec.failures)
		}
	})

	t.Run("all missing triples are listed in one failure", func(t *testing.T) {
		expected := append([]Expectation{}, inScope...)
		expected = append(expected,
			Expectation{Action: "case.transition", EntityType: "case", EntityID: "case-missing"},
			Expectation{Action: "proposal.create", EntityType: "proposal", EntityID: "proposal-missing"},
		)
		rec := &recordingReporter{}
		checkAuditCompleteness(rec, expected, loadAuditTriples(t, d.Pool), nil, false)
		msg := rec.onlyFailure(t)
		if !strings.Contains(msg, "2 of 5 expected audit triples missing") {
			t.Errorf("failure omits the missing count:\n%s", msg)
		}
		mustContainInOrder(t, msg,
			`action="case.transition" entity_type="case" entity_id="case-missing"`,
			`action="proposal.create" entity_type="proposal" entity_id="proposal-missing"`,
		)
	})
}

func TestAuditCompletenessEmptyExpectedPassesOnEmptyDB(t *testing.T) {
	d := testdb.New(t)
	AssertAuditCompleteness(t, d.Pool, nil)
	AssertAuditCompletenessExact(t, d.Pool, nil)
}

// TestAuditCompletenessReadsCallerTransaction proves the helpers read through
// the supplied transaction, including its own uncommitted rows, and never
// commit them.
func TestAuditCompletenessReadsCallerTransaction(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	entry := testAuditEntry("case.create", "case", "case-uncommitted")
	if _, err := audit.Append(ctx, tx, entry); err != nil {
		t.Fatalf("append uncommitted audit row: %v", err)
	}
	expected := []Expectation{{Action: entry.Action, EntityType: entry.EntityType, EntityID: entry.EntityID}}
	AssertAuditCompleteness(t, tx, expected)
	AssertAuditCompletenessExact(t, tx, expected, entry.EntityID)

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback transaction: %v", err)
	}
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&count); err != nil {
		t.Fatalf("count committed audit rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("committed audit rows = %d, want 0: the helper must not commit the caller's transaction", count)
	}
}

// TestAuditCompletenessHelperPerformsNoWrites runs both assertions inside a
// read-only transaction. Any write attempted by the helper would fail with
// SQLSTATE 25006 and fail the test; the row counts also stay unchanged.
func TestAuditCompletenessHelperPerformsNoWrites(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	seedAudit(t, d, testAuditEntry("case.create", "case", "case-read-only"))

	beforeAudit := countRows(t, d, `SELECT count(*) FROM audit_log`)
	beforeHead := countRows(t, d, `SELECT count(*) FROM audit_chain_head`)

	tx, err := d.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin read-only transaction: %v", err)
	}
	expected := []Expectation{{Action: "case.create", EntityType: "case", EntityID: "case-read-only"}}
	AssertAuditCompleteness(t, tx, expected)
	AssertAuditCompletenessExact(t, tx, expected, "case-read-only")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback read-only transaction: %v", err)
	}

	if got := countRows(t, d, `SELECT count(*) FROM audit_log`); got != beforeAudit {
		t.Errorf("audit_log rows = %d after the helpers, want unchanged %d", got, beforeAudit)
	}
	if got := countRows(t, d, `SELECT count(*) FROM audit_chain_head`); got != beforeHead {
		t.Errorf("audit_chain_head rows = %d after the helpers, want unchanged %d", got, beforeHead)
	}
}

// installAuditInsertFailure installs a temporary BEFORE INSERT trigger on
// audit_log that raises for every row. The returned cleanup drops the trigger
// and its function.
func installAuditInsertFailure(t *testing.T, d *testdb.DB) func() {
	t.Helper()
	ctx := context.Background()
	const (
		function = "m4_13_audit_insert_failure_fn"
		trigger  = "m4_13_audit_insert_failure"
	)
	if _, err := d.Pool.Exec(ctx,
		`CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   RAISE EXCEPTION 'm4-13 injected audit insert failure';
		 END;
		 $$`); err != nil {
		t.Fatalf("create audit failure function: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`CREATE TRIGGER `+trigger+` BEFORE INSERT ON audit_log
		 FOR EACH ROW EXECUTE FUNCTION `+function+`()`); err != nil {
		_, _ = d.Pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+function+`()`)
		t.Fatalf("create audit failure trigger: %v", err)
	}
	return func() {
		ctx := context.Background()
		if _, err := d.Pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+trigger+` ON audit_log`); err != nil {
			t.Errorf("drop audit failure trigger: %v", err)
		}
		if _, err := d.Pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+function+`()`); err != nil {
			t.Errorf("drop audit failure function: %v", err)
		}
	}
}

// TestAuditRollbackOnAuditInsertTrigger is the same-transaction proof: a real
// cases.Service.Transition updates the cases row and appends its audit row in
// one transaction; the injected audit_log BEFORE INSERT trigger aborts the
// append. The transaction rolls back, so neither the case update nor the audit
// row survives on a fresh connection.
func TestAuditRollbackOnAuditInsertTrigger(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app, err := db.NewPool(ctx, d.DSNFor(testdb.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(app.Close)

	var caseID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES (2, 500, 'USD', 'open', 0, 0, $1) RETURNING id::text`,
		auditTestNow).Scan(&caseID); err != nil {
		t.Fatalf("seed case row: %v", err)
	}

	service, err := cases.NewService(app, clock.NewFake(auditTestNow), mustTestActor(t, "worker"))
	if err != nil {
		t.Fatalf("new case service: %v", err)
	}

	drop := installAuditInsertFailure(t, d)
	t.Cleanup(drop)

	_, err = service.Transition(ctx, cases.TransitionRequest{
		CaseID:          caseID,
		ExpectedVersion: 0,
		Status:          cases.StatusInAgent,
	})
	if err == nil {
		t.Fatal("Transition succeeded despite the injected audit insert failure")
	}
	if !strings.Contains(err.Error(), "m4-13 injected audit insert failure") {
		t.Fatalf("Transition error = %v, want the injected trigger error", err)
	}

	// A fresh connection proves the rollback, not just the aborted session's
	// view: the domain row is untouched and no audit row or chain head exists.
	conn, err := pgx.Connect(ctx, d.DSN)
	if err != nil {
		t.Fatalf("open fresh connection: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	var (
		status            string
		version, attempts int
	)
	if err := conn.QueryRow(ctx,
		`SELECT status, version, attempts FROM cases WHERE id = $1::uuid`, caseID).
		Scan(&status, &version, &attempts); err != nil {
		t.Fatalf("read case after rollback: %v", err)
	}
	if status != "open" || version != 0 || attempts != 0 {
		t.Errorf("case after rollback = {status:%s version:%d attempts:%d}, want {open 0 0}",
			status, version, attempts)
	}
	var auditRows, headRows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&auditRows); err != nil {
		t.Fatalf("count audit rows on fresh connection: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit_chain_head`).Scan(&headRows); err != nil {
		t.Fatalf("count chain head rows on fresh connection: %v", err)
	}
	if auditRows != 0 || headRows != 0 {
		t.Errorf("after rollback audit_log=%d audit_chain_head=%d, want 0/0", auditRows, headRows)
	}

	// Dropping the trigger makes the same mutation succeed, proving the failure
	// came from the injected trigger and not from an invalid fixture.
	drop()
	got, err := service.Transition(ctx, cases.TransitionRequest{
		CaseID:          caseID,
		ExpectedVersion: 0,
		Status:          cases.StatusInAgent,
	})
	if err != nil {
		t.Fatalf("Transition after dropping the trigger: %v", err)
	}
	if got.Status != cases.StatusInAgent || got.Version != 1 {
		t.Fatalf("transitioned case = {status:%s version:%d}, want {in_agent 1}", got.Status, got.Version)
	}
	expected := []Expectation{{
		Action:     cases.ActionCaseTransition,
		EntityType: cases.EntityCase,
		EntityID:   caseID,
	}}
	AssertAuditCompleteness(t, d.Pool, expected)
	AssertAuditCompletenessExact(t, d.Pool, expected, caseID)
}

func countRows(t *testing.T, d *testdb.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := d.Pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return count
}
