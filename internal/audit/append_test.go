//go:build integration

// covers: FIN-30, FIN-31, FIN-33
package audit_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestAppendGenesis covers the first row of a chain: seq 1, a 32-byte zero
// prev_hash, typed actor columns, a stable row_hash, and a clean Verify.
//
// covers: FIN-30, FIN-31, FIN-33
func TestAppendGenesis(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	rec := appendCommitted(t, ctx, d.Pool, chainEntry(t, 1))
	if rec.Seq != 1 {
		t.Fatalf("genesis seq = %d, want 1", rec.Seq)
	}
	if len(rec.RowHash) != 32 {
		t.Fatalf("genesis row_hash length = %d, want 32", len(rec.RowHash))
	}
	if !bytes.Equal(rec.PrevHash, make([]byte, 32)) {
		t.Fatalf("genesis prev_hash = %x, want 32 zero bytes", rec.PrevHash)
	}

	stored := readStoredRow(t, ctx, d.Pool, 1)
	if stored.ActorType != "system" || stored.ActorID != "matcher" {
		t.Fatalf("actor columns = %q/%q, want system/matcher", stored.ActorType, stored.ActorID)
	}
	if stored.Action != "test.action.01" {
		t.Fatalf("action = %q", stored.Action)
	}
	if !stored.Before.Valid || !stored.After.Valid {
		t.Fatalf("before/after stored as NULL, want JSON payloads")
	}
	if !stored.TraceID.Valid || stored.TraceID.String != "trace-01" {
		t.Fatalf("trace_id = %+v, want trace-01", stored.TraceID)
	}
	if !bytes.Equal(stored.PrevHash, make([]byte, 32)) {
		t.Fatalf("stored prev_hash = %x, want zeros", stored.PrevHash)
	}
	if !bytes.Equal(stored.RowHash, rec.RowHash) {
		t.Fatalf("returned row_hash = %x, stored = %x", rec.RowHash, stored.RowHash)
	}

	// D-15: the two actor columns reconstruct the canonical string.
	actor, err := audit.ParseActor(stored.ActorType + ":" + stored.ActorID)
	if err != nil {
		t.Fatalf("reconstruct actor: %v", err)
	}
	if actor.String() != "system:matcher" {
		t.Fatalf("reconstructed actor = %q", actor.String())
	}

	report, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("Verify reported problems for a fresh chain: %+v", report.Problems)
	}
	if report.Checked != 1 || report.HeadSeq != 1 {
		t.Fatalf("Verify checked=%d head=%d, want 1/1", report.Checked, report.HeadSeq)
	}

	// Re-verifying must recompute the same row hash (no time-dependent input).
	again := readStoredRow(t, ctx, d.Pool, 1)
	if !bytes.Equal(stored.RowHash, again.RowHash) {
		t.Fatalf("row_hash changed across Verify: %x vs %x", stored.RowHash, again.RowHash)
	}
	if _, err := audit.Verify(ctx, d.Pool, 0); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
}

// TestAppendChainLinks proves consecutive rows are linked and that seq is
// contiguous.
//
// covers: FIN-31
func TestAppendChainLinks(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	var records []audit.Record
	for n := 1; n <= 3; n++ {
		records = append(records, appendCommitted(t, ctx, d.Pool, chainEntry(t, n)))
	}
	for i, rec := range records {
		if rec.Seq != int64(i+1) {
			t.Fatalf("record %d seq = %d, want %d", i, rec.Seq, i+1)
		}
	}
	for i := 1; i < len(records); i++ {
		if !bytes.Equal(records[i].PrevHash, records[i-1].RowHash) {
			t.Fatalf("row %d prev_hash = %x, want previous row_hash %x",
				records[i].Seq, records[i].PrevHash, records[i-1].RowHash)
		}
	}

	report, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("Verify problems: %+v", report.Problems)
	}
	if report.Checked != 3 || report.HeadSeq != 3 {
		t.Fatalf("Verify checked=%d head=%d, want 3/3", report.Checked, report.HeadSeq)
	}
}

// TestAppendRequiresCallerTransaction covers the contract that audit rows join
// the caller's transaction. A nil tx is an explicit runtime error.
//
// covers: FIN-30
func TestAppendRequiresCallerTransaction(t *testing.T) {
	ctx := context.Background()
	entry := chainEntry(t, 1)

	if _, err := audit.Append(ctx, nil, entry); !errors.Is(err, audit.ErrNoTransaction) {
		t.Fatalf("Append(nil tx) error = %v, want ErrNoTransaction", err)
	}
	var tx pgx.Tx
	if _, err := audit.Append(ctx, tx, entry); !errors.Is(err, audit.ErrNoTransaction) {
		t.Fatalf("Append(typed nil tx) error = %v, want ErrNoTransaction", err)
	}
}

// TestAppendValidatesEntry rejects incomplete entries before touching the
// transaction.
//
// covers: FIN-30
func TestAppendValidatesEntry(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(*audit.Entry)
	}{
		{"zero actor", func(e *audit.Entry) { e.Actor = audit.Actor{} }},
		{"empty action", func(e *audit.Entry) { e.Action = "" }},
		{"empty entity type", func(e *audit.Entry) { e.EntityType = "" }},
		{"empty entity id", func(e *audit.Entry) { e.EntityID = "" }},
		{"zero timestamp", func(e *audit.Entry) { e.Timestamp = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := chainEntry(t, 1)
			tc.mutate(&entry)
			if _, err := audit.Append(ctx, nil, entry); !errors.Is(err, audit.ErrInvalidEntry) {
				t.Fatalf("Append error = %v, want ErrInvalidEntry", err)
			}
		})
	}
}

// TestAppendRollbackRemovesAuditRow proves the audit row participates in the
// caller's transaction: a rollback removes it and does not leave a seq gap.
//
// covers: FIN-30, FIN-31
func TestAppendRollbackRemovesAuditRow(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := audit.Append(ctx, tx, chainEntry(t, 1)); err != nil {
		t.Fatalf("append in rolled-back tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := countAuditRows(t, ctx, d.Pool, "audit_log"); got != 0 {
		t.Fatalf("audit_log rows after rollback = %d, want 0", got)
	}
	if got := countAuditRows(t, ctx, d.Pool, "audit_chain_head"); got != 0 {
		t.Fatalf("audit_chain_head rows after rollback = %d, want 0", got)
	}

	first := appendCommitted(t, ctx, d.Pool, chainEntry(t, 1))
	if first.Seq != 1 {
		t.Fatalf("seq after a rolled-back append = %d, want 1 (no identity gap)", first.Seq)
	}

	// A rollback after a committed row must also leave the next seq contiguous.
	second := appendCommitted(t, ctx, d.Pool, chainEntry(t, 2))
	tx2, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second: %v", err)
	}
	if _, err := audit.Append(ctx, tx2, chainEntry(t, 3)); err != nil {
		t.Fatalf("append rolled back row: %v", err)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("rollback second: %v", err)
	}
	third := appendCommitted(t, ctx, d.Pool, chainEntry(t, 4))
	if third.Seq != second.Seq+1 {
		t.Fatalf("seq after rollback = %d, want %d", third.Seq, second.Seq+1)
	}
	if !bytes.Equal(third.PrevHash, second.RowHash) {
		t.Fatalf("chain did not re-link after rollback: prev=%x want=%x", third.PrevHash, second.RowHash)
	}

	report, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("Verify problems after rollback: %+v", report.Problems)
	}
}

// TestAppendTimestampUTC covers FIN-40 for audit rows: timestamps are stored in
// UTC, and hashing truncates to Postgres microsecond precision so Verify
// recomputes the same hash.
//
// covers: FIN-30, FIN-31
func TestAppendTimestampUTC(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	zone := time.FixedZone("EST", -5*60*60)
	entry := chainEntry(t, 5)
	entry.Timestamp = time.Date(2026, 11, 4, 7, 30, 0, 987654321, zone)
	rec := appendCommitted(t, ctx, d.Pool, entry)

	stored := readStoredRow(t, ctx, d.Pool, rec.Seq)
	if !stored.TS.Equal(entry.Timestamp.Truncate(time.Microsecond)) {
		t.Fatalf("stored ts instant = %v, want %v", stored.TS, entry.Timestamp.Truncate(time.Microsecond))
	}
	if got := stored.TS.UTC().Format(time.RFC3339Nano); got != "2026-11-04T12:30:00.987654Z" {
		t.Fatalf("stored ts in UTC = %q, want 2026-11-04T12:30:00.987654Z", got)
	}
	if stored.TS.Nanosecond() != 987654000 {
		t.Fatalf("stored ts nanoseconds = %d, want 987654000 (microsecond precision)", stored.TS.Nanosecond())
	}
	if !rec.Timestamp.Equal(stored.TS) {
		t.Fatalf("returned timestamp %v does not equal stored %v", rec.Timestamp, stored.TS)
	}

	report, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("Verify problems for non-UTC input: %+v", report.Problems)
	}
}

// TestAuditRoleMatrix covers the D-6 grant matrix and the database-enforced
// append-only rule for audit rows (FIN-31): app/executor INSERT+SELECT,
// audit_ro SELECT, agent_ro no INSERT, and no role may UPDATE/DELETE.
//
// covers: FIN-31
func TestAuditRoleMatrix(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	app := openRolePool(t, d, db.RoleApp)
	executor := openRolePool(t, d, db.RoleExecutor)
	auditRO := openRolePool(t, d, db.RoleAuditRO)
	agentRO := openRolePool(t, d, db.RoleAgentRO)

	// recon_app and recon_executor can append and read.
	appRecord := appendCommitted(t, ctx, app, chainEntry(t, 1))
	execRecord := appendCommitted(t, ctx, executor, chainEntry(t, 2))
	if appRecord.Seq != 1 || execRecord.Seq != 2 {
		t.Fatalf("app/executor seqs = %d/%d, want 1/2", appRecord.Seq, execRecord.Seq)
	}
	for name, pool := range map[string]interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}{"app": app, "executor": executor, "audit_ro": auditRO} {
		if got := countAuditRows(t, ctx, pool, "audit_log"); got != 2 {
			t.Fatalf("%s SELECT count = %d, want 2", name, got)
		}
	}

	// recon_audit_ro can verify the chain; it has SELECT on both chain tables.
	if report, err := audit.Verify(ctx, auditRO, 0); err != nil {
		t.Fatalf("audit_ro Verify: %v", err)
	} else if !report.OK() {
		t.Fatalf("audit_ro Verify problems: %+v", report.Problems)
	}

	// recon_audit_ro and recon_agent_ro cannot append.
	if _, err := auditRO.Exec(ctx, `INSERT INTO audit_log (action) VALUES ('nope')`); err == nil {
		t.Fatal("audit_ro INSERT succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := agentRO.Exec(ctx, `INSERT INTO audit_log (action) VALUES ('nope')`); err == nil {
		t.Fatal("agent_ro INSERT succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	// Neither posting role can UPDATE or DELETE; the grant itself denies it.
	for name, pool := range map[string]interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}{
		"app":      app,
		"executor": executor,
	} {
		if _, err := pool.Exec(ctx, `UPDATE audit_log SET action = 'tampered' WHERE seq = 1`); err == nil {
			t.Fatalf("%s UPDATE succeeded, want permission denied", name)
		} else {
			requirePgError(t, err, sqlStateInsufficientPrivilege)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM audit_log WHERE seq = 1`); err == nil {
			t.Fatalf("%s DELETE succeeded, want permission denied", name)
		} else {
			requirePgError(t, err, sqlStateInsufficientPrivilege)
		}
	}

	// Even the maintenance/superuser role cannot mutate or truncate audit rows:
	// triggers, not grants, are the backstop (FIN-31).
	if _, err := d.Pool.Exec(ctx, `UPDATE audit_log SET action = 'tampered' WHERE seq = 1`); err == nil {
		t.Fatal("superuser UPDATE succeeded, want trigger rejection")
	} else {
		requirePgError(t, err, sqlStateRestrictViolation)
	}
	if _, err := d.Pool.Exec(ctx, `DELETE FROM audit_log WHERE seq = 1`); err == nil {
		t.Fatal("superuser DELETE succeeded, want trigger rejection")
	} else {
		requirePgError(t, err, sqlStateRestrictViolation)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE audit_log`); err == nil {
		t.Fatal("superuser TRUNCATE succeeded, want trigger rejection")
	} else {
		requirePgError(t, err, sqlStateRestrictViolation)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE audit_chain_head`); err == nil {
		t.Fatal("superuser TRUNCATE audit_chain_head succeeded, want trigger rejection")
	} else {
		requirePgError(t, err, sqlStateRestrictViolation)
	}
}

// TestAuditGrantsCatalog asserts the migration grants directly, so a grant
// regression fails even if a behavioural test happens to be skipped.
//
// covers: FIN-31
func TestAuditGrantsCatalog(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	type grant struct {
		role, table                                    string
		selectWant, insertWant, updateWant, deleteWant bool
	}
	grants := []grant{
		{db.RoleApp, "audit_log", true, true, false, false},
		{db.RoleExecutor, "audit_log", true, true, false, false},
		{db.RoleAuditRO, "audit_log", true, false, false, false},
		{db.RoleAgentRO, "audit_log", false, false, false, false},
		{db.RoleApp, "audit_chain_head", true, true, false, false},
		{db.RoleExecutor, "audit_chain_head", true, true, false, false},
		{db.RoleAuditRO, "audit_chain_head", true, false, false, false},
		{db.RoleAgentRO, "audit_chain_head", false, false, false, false},
	}
	for _, g := range grants {
		t.Run(g.role+"/"+g.table, func(t *testing.T) {
			var canSelect, canInsert, canUpdate, canDelete bool
			if err := d.Pool.QueryRow(ctx,
				`SELECT has_table_privilege($1::text, $2::text, 'SELECT'),
				        has_table_privilege($1::text, $2::text, 'INSERT'),
				        has_table_privilege($1::text, $2::text, 'UPDATE'),
				        has_table_privilege($1::text, $2::text, 'DELETE')`,
				g.role, g.table).Scan(&canSelect, &canInsert, &canUpdate, &canDelete); err != nil {
				t.Fatalf("read privileges: %v", err)
			}
			if canSelect != g.selectWant || canInsert != g.insertWant ||
				canUpdate != g.updateWant || canDelete != g.deleteWant {
				t.Fatalf("%s on %s = SELECT %t INSERT %t UPDATE %t DELETE %t, want %t/%t/%t/%t",
					g.role, g.table, canSelect, canInsert, canUpdate, canDelete,
					g.selectWant, g.insertWant, g.updateWant, g.deleteWant)
			}
		})
	}

	var sequence string
	if err := d.Pool.QueryRow(ctx, `SELECT pg_get_serial_sequence('audit_log', 'seq')`).Scan(&sequence); err != nil {
		t.Fatalf("resolve audit_log identity sequence: %v", err)
	}
	if sequence == "" {
		t.Fatal("audit_log.seq has no identity sequence")
	}
	for _, role := range db.Roles() {
		want := role == db.RoleApp || role == db.RoleExecutor
		var usage, selectPriv bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT has_sequence_privilege($1::text, $2::text, 'USAGE'),
			        has_sequence_privilege($1::text, $2::text, 'SELECT')`,
			role, sequence).Scan(&usage, &selectPriv); err != nil {
			t.Fatalf("read sequence privileges for %s: %v", role, err)
		}
		if usage != want || selectPriv != want {
			t.Errorf("%s sequence privileges USAGE=%t SELECT=%t, want %t/%t", role, usage, selectPriv, want, want)
		}
	}
}
