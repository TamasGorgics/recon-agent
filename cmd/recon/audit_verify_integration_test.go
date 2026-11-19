//go:build integration

// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

const auditVerifySeedRows = 4

// auditVerifyEntry returns a deterministic entry whose every hashed field
// differs by n.
func auditVerifyEntry(t *testing.T, n int) audit.Entry {
	t.Helper()
	actor, err := audit.SystemActor("matcher")
	if err != nil {
		t.Fatalf("build system actor: %v", err)
	}
	return audit.Entry{
		Actor:      actor,
		Action:     fmt.Sprintf("test.verify.%02d", n),
		EntityType: "audit_verify_test",
		EntityID:   fmt.Sprintf("entity-%02d", n),
		Before:     map[string]any{"n": n},
		After:      map[string]any{"n": n, "state": "verified"},
		Timestamp: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC).
			Add(time.Duration(n) * time.Second),
	}
}

// newAuditVerifyDB clones a fresh database, points the CLI at its app-role
// login, and optionally seeds a committed audit chain of rows.
func newAuditVerifyDB(t *testing.T, rows int) *testdb.DB {
	t.Helper()
	d := testdb.New(t)
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", d.DSN)
	t.Setenv("DATABASE_APP_URL", d.DSNFor(db.RoleApp))
	t.Setenv("DATABASE_AGENT_URL", d.DSNFor(db.RoleAgentRO))
	for n := 1; n <= rows; n++ {
		testutil.AppendAudit(t, context.Background(), d.Pool, auditVerifyEntry(t, n))
	}
	return d
}

// runAuditVerifyCLI invokes the command through the same dispatch the binary
// uses and returns what it wrote to stdout.
func runAuditVerifyCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runAuditTo(context.Background(),
		append([]string{"verify"}, args...), &out)
	return out.String(), err
}

func auditRowsAndHead(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (int64, int64) {
	t.Helper()
	var rows, head int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&rows); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM audit_chain_head`).Scan(&head); err != nil {
		t.Fatalf("read chain head: %v", err)
	}
	return rows, head
}

func TestAuditVerifyCLIIntact(t *testing.T) {
	ctx := context.Background()
	d := newAuditVerifyDB(t, auditVerifySeedRows)

	out, err := runAuditVerifyCLI(t, "--json")
	if err != nil {
		t.Fatalf("audit verify intact: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	want := `{"from":1,"head_seq":4,"checked":4,"findings":[]}` + "\n"
	if out != want {
		t.Fatalf("JSON = %s, want %s", out, want)
	}

	out, err = runAuditVerifyCLI(t)
	if err != nil {
		t.Fatalf("audit verify intact human: %v", err)
	}
	wantHuman := "audit verify: OK from=1 head=4 checked=4 findings=0\n"
	if out != wantHuman {
		t.Fatalf("human output = %q, want %q", out, wantHuman)
	}

	// Verification is read-only: the chain must be byte-for-byte unchanged.
	if rows, head := auditRowsAndHead(t, ctx, d.Pool); rows != 4 || head != 4 {
		t.Fatalf("audit state after verify = rows %d head %d, want 4/4", rows, head)
	}
}

func TestAuditVerifyCLIFromSemantics(t *testing.T) {
	newAuditVerifyDB(t, auditVerifySeedRows)

	cases := []struct {
		name     string
		args     []string
		wantJSON string
	}{
		{
			name:     "from zero clamps to one",
			args:     []string{"--json", "--from", "0"},
			wantJSON: `{"from":1,"head_seq":4,"checked":4,"findings":[]}`,
		},
		{
			name:     "negative from clamps to one",
			args:     []string{"--json", "--from", "-9"},
			wantJSON: `{"from":1,"head_seq":4,"checked":4,"findings":[]}`,
		},
		{
			name:     "incremental from",
			args:     []string{"--json", "--from", "2"},
			wantJSON: `{"from":2,"head_seq":4,"checked":3,"findings":[]}`,
		},
		{
			name:     "head plus one is an empty check",
			args:     []string{"--json", "--from", "5"},
			wantJSON: `{"from":5,"head_seq":4,"checked":0,"findings":[]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runAuditVerifyCLI(t, tc.args...)
			if err != nil {
				t.Fatalf("audit verify %v: %v", tc.args, err)
			}
			if strings.TrimSpace(out) != tc.wantJSON {
				t.Fatalf("JSON = %s, want %s", strings.TrimSpace(out), tc.wantJSON)
			}
		})
	}

	t.Run("beyond tail is exit two with no output", func(t *testing.T) {
		out, err := runAuditVerifyCLI(t, "--json", "--from", "6")
		if err == nil {
			t.Fatal("audit verify beyond tail = nil, want an error")
		}
		if code := exitCode(err); code != 2 {
			t.Fatalf("exit code = %d, want 2 (%v)", code, err)
		}
		if !errors.Is(err, audit.ErrFromSeqBeyondTail) {
			t.Fatalf("error = %v, want ErrFromSeqBeyondTail", err)
		}
		if !strings.Contains(err.Error(), "beyond the chain tail") {
			t.Fatalf("error = %q, want the beyond-tail message", err)
		}
		if out != "" {
			t.Fatalf("beyond-tail wrote to stdout: %q", out)
		}
	})
}

func TestAuditVerifyCLIEmptyChain(t *testing.T) {
	newAuditVerifyDB(t, 0)

	out, err := runAuditVerifyCLI(t, "--json")
	if err != nil {
		t.Fatalf("audit verify empty chain: %v", err)
	}
	want := `{"from":1,"head_seq":0,"checked":0,"findings":[]}` + "\n"
	if out != want {
		t.Fatalf("empty-chain JSON = %s, want %s", out, want)
	}

	out, err = runAuditVerifyCLI(t)
	if err != nil {
		t.Fatalf("audit verify empty chain human: %v", err)
	}
	wantHuman := "audit verify: OK from=1 head=0 checked=0 findings=0\n"
	if out != wantHuman {
		t.Fatalf("empty-chain human output = %q, want %q", out, wantHuman)
	}

	out, err = runAuditVerifyCLI(t, "--json", "--from", "2")
	if err == nil {
		t.Fatal("audit verify beyond an empty chain = nil, want an error")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	if !errors.Is(err, audit.ErrFromSeqBeyondTail) {
		t.Fatalf("error = %v, want ErrFromSeqBeyondTail", err)
	}
	if out != "" {
		t.Fatalf("beyond-empty-tail wrote to stdout: %q", out)
	}
}

func TestAuditVerifyCLIQueryErrorIsExitTwo(t *testing.T) {
	ctx := context.Background()
	d := newAuditVerifyDB(t, 2)
	if _, err := d.Pool.Exec(ctx,
		`REVOKE SELECT ON audit_chain_head, audit_log FROM recon_app`); err != nil {
		t.Fatalf("revoke audit reads: %v", err)
	}

	out, err := runAuditVerifyCLI(t, "--json")
	if err == nil {
		t.Fatal("audit verify without SELECT = nil, want an error")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("error = %v, want a 42501 permission error from the verification query", err)
	}
	if !strings.Contains(err.Error(), "audit verify") {
		t.Fatalf("error = %q, want it to name the command", err)
	}
	if out != "" {
		t.Fatalf("query error wrote to stdout: %q", out)
	}
}

// TestAuditVerifyTransactionIsReadOnly proves the helper the CLI uses opens a
// READ ONLY transaction: a write in it fails with SQLSTATE 25006 and leaves no
// rows behind. Combined with audit.Verify issuing only SELECTs, this is the
// spec 3.5 read-only guarantee.
func TestAuditVerifyTransactionIsReadOnly(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app, err := db.OpenRole(ctx, db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	defer app.Close()

	writeErr := withAuditVerifyTx(ctx, app.Pool,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO audit_log
				     (seq, ts, actor_type, actor_id, action, entity_type,
				      entity_id, prev_hash, row_hash)
				 OVERRIDING SYSTEM VALUE
				 VALUES (1, now(), 'system', 'matcher', 'test.write',
				         'audit_verify_test', 'read-only-probe',
				         decode(repeat('00', 32), 'hex'),
				         decode(repeat('00', 32), 'hex'))`)
			return err
		})
	if writeErr == nil {
		t.Fatal("write inside the verification transaction succeeded, want a read-only error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(writeErr, &pgErr) {
		t.Fatalf("write error %v is not a *pgconn.PgError", writeErr)
	}
	if pgErr.Code != "25006" {
		t.Fatalf("SQLSTATE = %s, want 25006 (read-only transaction): %v",
			pgErr.Code, writeErr)
	}
	if !strings.Contains(strings.ToLower(pgErr.Message), "read-only") {
		t.Fatalf("error message = %q, want a read-only transaction error", pgErr.Message)
	}

	if rows, head := auditRowsAndHead(t, ctx, d.Pool); rows != 0 || head != 0 {
		t.Fatalf("audit state after rejected write = rows %d head %d, want 0/0",
			rows, head)
	}
}

// appendAfterHeadRead wraps the verification transaction and commits one new
// audit row right after Verify reads the chain head (the second QueryRow is the
// audit_log bounds query). With REPEATABLE READ the snapshot is already fixed,
// so the concurrent append must be invisible.
type appendAfterHeadRead struct {
	tx       pgx.Tx
	calls    int
	appended bool
	append   func()
}

func (q *appendAfterHeadRead) QueryRow(ctx context.Context, sql string,
	args ...any) pgx.Row {
	q.calls++
	if q.calls > 1 && !q.appended {
		q.appended = true
		q.append()
	}
	return q.tx.QueryRow(ctx, sql, args...)
}

func (q *appendAfterHeadRead) Query(ctx context.Context, sql string,
	args ...any) (pgx.Rows, error) {
	return q.tx.Query(ctx, sql, args...)
}

// TestAuditVerifySnapshotHidesConcurrentAppend covers spec 3.7: head and rows
// share one REPEATABLE READ snapshot, so an append that commits while the
// verification is running cannot fake a truncated tail or a head mismatch.
func TestAuditVerifySnapshotHidesConcurrentAppend(t *testing.T) {
	ctx := context.Background()
	d := newAuditVerifyDB(t, 3)
	app, err := db.OpenRole(ctx, db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	defer app.Close()

	var report audit.Report
	err = withAuditVerifyTx(ctx, app.Pool, func(ctx context.Context, tx pgx.Tx) error {
		querier := &appendAfterHeadRead{tx: tx, append: func() {
			testutil.AppendAudit(t, context.Background(), d.Pool, auditVerifyEntry(t, 4))
		}}
		var verifyErr error
		report, verifyErr = audit.Verify(ctx, querier, 1)
		return verifyErr
	})
	if err != nil {
		t.Fatalf("verify with a concurrent append: %v", err)
	}
	if !report.OK() || report.HeadSeq != 3 || report.Checked != 3 {
		t.Fatalf("report = %+v, want intact head 3 checked 3 from the pre-append snapshot",
			report)
	}
	if rows, head := auditRowsAndHead(t, ctx, d.Pool); rows != 4 || head != 4 {
		t.Fatalf("committed append state = rows %d head %d, want 4/4", rows, head)
	}
}

func auditLogUserTriggers(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT tgname FROM pg_trigger
		  WHERE tgrelid = 'audit_log'::regclass AND NOT tgisinternal
		  ORDER BY tgname`)
	if err != nil {
		t.Fatalf("discover audit_log triggers: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect audit_log triggers: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("audit_log has no user triggers to disable")
	}
	return names
}

// setAuditLogTriggers disables or enables the discovered triggers by name.
// Names are never hardcoded; DISABLE TRIGGER USER would also work, but naming
// each trigger proves the test knows exactly what it silenced.
func setAuditLogTriggers(ctx context.Context, tx pgx.Tx, action string,
	names []string) error {
	for _, name := range names {
		stmt := "ALTER TABLE audit_log " + action + " TRIGGER " +
			pgx.Identifier{name}.Sanitize()
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s trigger %s: %w", action, name, err)
		}
	}
	return nil
}

// assertAuditLogTriggersEnabled checks pg_trigger directly: every non-internal
// audit_log trigger is back to its normal enabled state ('O').
func assertAuditLogTriggersEnabled(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, want []string) {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT tgname, tgenabled FROM pg_trigger
		  WHERE tgrelid = 'audit_log'::regclass AND NOT tgisinternal
		  ORDER BY tgname`)
	if err != nil {
		t.Fatalf("read audit_log trigger states: %v", err)
	}
	type triggerState struct {
		Name    string `db:"tgname"`
		Enabled string `db:"tgenabled"`
	}
	states, err := pgx.CollectRows(rows, pgx.RowToStructByName[triggerState])
	if err != nil {
		t.Fatalf("collect audit_log trigger states: %v", err)
	}
	if len(states) != len(want) {
		t.Fatalf("trigger count after restore = %d, want %d", len(states), len(want))
	}
	for _, state := range states {
		if state.Enabled != "O" {
			t.Fatalf("trigger %s tgenabled = %q, want 'O'", state.Name, state.Enabled)
		}
	}
}

// tamperAuditLog commits one mutation with the append-only triggers disabled,
// then re-enables every discovered trigger and asserts it is back on.
func tamperAuditLog(t *testing.T, pool *pgxpool.Pool,
	mutate func(context.Context, pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	triggers := auditLogUserTriggers(t, ctx, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tamper transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := setAuditLogTriggers(ctx, tx, "DISABLE", triggers); err != nil {
		t.Fatalf("disable audit triggers: %v", err)
	}
	if err := mutate(ctx, tx); err != nil {
		t.Fatalf("tamper audit_log: %v", err)
	}
	if err := setAuditLogTriggers(ctx, tx, "ENABLE", triggers); err != nil {
		t.Fatalf("re-enable audit triggers: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tamper transaction: %v", err)
	}
	assertAuditLogTriggersEnabled(t, ctx, pool, triggers)
}

func TestAuditVerifyCLITamper(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(context.Context, pgx.Tx) error
		wantKinds []string
	}{
		{
			name: "payload update",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`UPDATE audit_log SET after = '{"tampered":true}'::jsonb WHERE seq = 2`)
				return err
			},
			wantKinds: []string{"hash_mismatch"},
		},
		{
			name: "middle row deleted",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE seq = 2`)
				return err
			},
			wantKinds: []string{"gap", "broken_link"},
		},
		{
			name: "tail row deleted with head mirror intact",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE seq = 4`)
				return err
			},
			wantKinds: []string{"truncated_tail"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := newAuditVerifyDB(t, auditVerifySeedRows)
			present := auditLogUserTriggers(t, context.Background(), d.Pool)
			if len(present) == 0 {
				t.Fatal("no audit_log triggers discovered")
			}
			tamperAuditLog(t, d.Pool, tc.mutate)

			out, err := runAuditVerifyCLI(t, "--json")
			if err == nil {
				t.Fatal("audit verify tampered chain = nil, want an exit-1 findings error")
			}
			if code := exitCode(err); code != 1 {
				t.Fatalf("exit code = %d, want 1 (%v)", code, err)
			}

			var result auditVerifyJSON
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("parse tampered JSON %q: %v", out, err)
			}
			if result.From != 1 || result.HeadSeq != auditVerifySeedRows {
				t.Fatalf("from/head_seq = %d/%d, want 1/%d",
					result.From, result.HeadSeq, auditVerifySeedRows)
			}
			if len(result.Findings) == 0 {
				t.Fatalf("no findings for tampered chain: %s", out)
			}
			kinds := make(map[string]bool, len(result.Findings))
			for i, finding := range result.Findings {
				kinds[finding.Kind] = true
				if finding.Seq < 1 {
					t.Errorf("finding %d has seq %d, want >= 1", i, finding.Seq)
				}
				if strings.TrimSpace(finding.Detail) == "" {
					t.Errorf("finding %d (%s) has an empty detail", i, finding.Kind)
				}
				if i > 0 {
					prev := result.Findings[i-1]
					if prev.Seq > finding.Seq ||
						(prev.Seq == finding.Seq && prev.Kind > finding.Kind) {
						t.Errorf("findings not sorted at %d: %+v then %+v",
							i, prev, finding)
					}
				}
			}
			for _, want := range tc.wantKinds {
				if !kinds[want] {
					t.Errorf("findings missing kind %q: %s", want, out)
				}
			}
			for kind := range kinds {
				known := kind == "hash_mismatch" || kind == "broken_link" ||
					kind == "gap" || kind == "truncated_tail" ||
					kind == "head_mismatch"
				if !known {
					t.Errorf("unknown finding kind %q", kind)
				}
			}

			// The tamper must be committed, so the read-only CLI saw it, and
			// the triggers must be restored for the next reader/writer.
			assertAuditLogTriggersEnabled(t, context.Background(), d.Pool, present)
		})
	}
}
