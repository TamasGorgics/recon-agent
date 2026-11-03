//go:build integration

// covers: FIN-11
package db_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const (
	sqlStateInsufficientPrivilege = "42501"
	sqlStateRestrictViolation     = "23001"
	sqlStateCheckViolation        = "23514"
)

var roleTxnCounter atomic.Int64

// TestMain prepares the migration template and asserts this process leaves no
// ephemeral role logins behind. Login names are scoped by process id so a
// concurrently running package cannot be mistaken for an orphan.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "db: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "db: ensure template: %v\n", err)
		os.Exit(1)
	}

	prefix := testdb.LoginRolePrefix()
	before, err := listLoginRoles(ctx, adminDSN, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db: list login roles before run: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	after, err := listLoginRoles(ctx, adminDSN, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db: list login roles after run: %v\n", err)
		os.Exit(1)
	}
	leftovers := newRoleNames(before, after)
	if len(leftovers) > 0 {
		fmt.Fprintf(os.Stderr, "db: orphan ephemeral login roles after run: %s\n", strings.Join(leftovers, ", "))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func listLoginRoles(ctx context.Context, adminDSN, prefix string) ([]string, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	rows, err := conn.Query(ctx,
		`SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1) ORDER BY rolname`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func newRoleNames(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var added []string
	for _, name := range after {
		if _, ok := seen[name]; ok {
			continue
		}
		added = append(added, name)
	}
	return added
}

// roleFixture is the minimum chart of accounts and period a role test needs.
type roleFixture struct {
	ClearingID string
	RevenueID  string
	PeriodID   string
}

func newRoleFixture(t *testing.T, d *testdb.DB) roleFixture {
	t.Helper()
	ctx := context.Background()

	var f roleFixture
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ('role_clearing_usd', 'Role Clearing USD', 'asset', 'USD', 'active')
		 RETURNING id::text`).Scan(&f.ClearingID); err != nil {
		t.Fatalf("create clearing account: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ('role_revenue_usd', 'Role Revenue USD', 'revenue', 'USD', 'active')
		 RETURNING id::text`).Scan(&f.RevenueID); err != nil {
		t.Fatalf("create revenue account: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT id::text FROM periods WHERE status = 'open' ORDER BY start_date LIMIT 1`).Scan(&f.PeriodID); err != nil {
		t.Fatalf("read seeded open period: %v", err)
	}
	return f
}

func openRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	rp, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(rp.Close)
	return rp.Pool
}

// roleQuerier is satisfied by *pgxpool.Pool and pgx.Tx.
type roleQuerier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type roleTxnSpec struct {
	Kind     string
	Key      string
	Amount   int64
	Credit   int64
	PeriodID string
	Currency string
}

func insertRoleTxn(ctx context.Context, q roleQuerier, f roleFixture, spec roleTxnSpec) (string, error) {
	kind := spec.Kind
	if kind == "" {
		kind = "charge"
	}
	key := spec.Key
	if key == "" {
		key = fmt.Sprintf("role-test-%d", roleTxnCounter.Add(1))
	}
	periodID := spec.PeriodID
	if periodID == "" {
		periodID = f.PeriodID
	}
	currency := spec.Currency
	if currency == "" {
		currency = "USD"
	}
	credit := spec.Credit
	if credit == 0 {
		credit = spec.Amount
	}

	var id string
	if err := q.QueryRow(ctx,
		`INSERT INTO ledger_txns (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ('2026-01-15', $1, $2, $3, '', $4::uuid)
		 RETURNING id::text`,
		kind, key, "hash:"+key, periodID).Scan(&id); err != nil {
		return "", err
	}
	if _, err := q.Exec(ctx,
		`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
		 VALUES ($1::uuid, $2::uuid, 'debit', $3, $4)`,
		id, f.ClearingID, spec.Amount, currency); err != nil {
		return "", err
	}
	if _, err := q.Exec(ctx,
		`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
		 VALUES ($1::uuid, $2::uuid, 'credit', $3, $4)`,
		id, f.RevenueID, credit, currency); err != nil {
		return "", err
	}
	return id, nil
}

func postRoleTxn(ctx context.Context, pool *pgxpool.Pool, f roleFixture, spec roleTxnSpec) error {
	return db.WithTx(ctx, pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertRoleTxn(ctx, tx, f, spec)
		return err
	})
}

func requirePgError(t *testing.T, err error, codes ...string) *pgconn.PgError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a Postgres error (%s), got nil", strings.Join(codes, ", "))
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a *pgconn.PgError", err)
	}
	for _, code := range codes {
		if pgErr.Code == code {
			return pgErr
		}
	}
	t.Fatalf("SQLSTATE = %s, want one of %v (error: %v)", pgErr.Code, codes, err)
	return nil
}

func createClosedPeriod(t *testing.T, d *testdb.DB) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO periods (start_date, end_date, status, closed_at)
		 VALUES ('2019-01-01', '2019-12-31', 'closed', now())
		 RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("create closed period: %v", err)
	}
	return id
}

// TestRolesCatalog asserts the NOLOGIN group roles and the D-6 grant matrix.
func TestRolesCatalog(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	for _, role := range db.Roles() {
		var canLogin, superuser, createDB, createRole bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT rolcanlogin, rolsuper, rolcreatedb, rolcreaterole
			   FROM pg_roles WHERE rolname = $1`, role).
			Scan(&canLogin, &superuser, &createDB, &createRole); err != nil {
			t.Fatalf("role %s: %v", role, err)
		}
		if canLogin {
			t.Errorf("role %s is LOGIN, want NOLOGIN", role)
		}
		if superuser || createDB || createRole {
			t.Errorf("role %s has superuser/createdb/createrole, want none", role)
		}
	}

	type grant struct {
		role, table                                    string
		selectWant, insertWant, updateWant, deleteWant bool
	}
	grants := []grant{
		{db.RoleApp, "ledger_txns", true, true, false, false},
		{db.RoleApp, "ledger_entries", true, true, false, false},
		{db.RoleExecutor, "ledger_txns", true, true, false, false},
		{db.RoleExecutor, "ledger_entries", true, true, false, false},
		{db.RoleAgentRO, "ledger_txns", false, false, false, false},
		{db.RoleAgentRO, "ledger_entries", false, false, false, false},
		{db.RoleAuditRO, "ledger_txns", false, false, false, false},
		{db.RoleAuditRO, "ledger_entries", false, false, false, false},
		// Reference data reads are required by the M1-03 validation triggers
		// and the ledger service; they are SELECT-only.
		{db.RoleApp, "accounts", true, false, false, false},
		{db.RoleApp, "currencies", true, false, false, false},
		{db.RoleApp, "periods", true, false, false, false},
		{db.RoleExecutor, "accounts", true, false, false, false},
		{db.RoleExecutor, "currencies", true, false, false, false},
		{db.RoleExecutor, "periods", true, false, false, false},
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
	if err := d.Pool.QueryRow(ctx, `SELECT pg_get_serial_sequence('ledger_txns', 'seq')`).Scan(&sequence); err != nil {
		t.Fatalf("resolve ledger_txns identity sequence: %v", err)
	}
	if sequence == "" {
		t.Fatal("ledger_txns.seq has no identity sequence")
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

	var triggerDef string
	if err := d.Pool.QueryRow(ctx,
		`SELECT pg_get_triggerdef(tg.oid)
		   FROM pg_trigger tg
		   JOIN pg_class c ON c.oid = tg.tgrelid
		  WHERE c.relname = 'ledger_txns' AND tg.tgname = 'ledger_txns_adjustment_role'`).
		Scan(&triggerDef); err != nil {
		t.Fatalf("adjustment-role trigger is missing: %v", err)
	}
	if !strings.Contains(triggerDef, "BEFORE INSERT") {
		t.Fatalf("adjustment-role trigger definition %q is not BEFORE INSERT", triggerDef)
	}
}

// TestAppRoleCanInsertAndReadLedger proves the application role can post and
// read ledger rows, including the identity sequence (FIN-11 read/write half).
func TestAppRoleCanInsertAndReadLedger(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)
	app := openRolePool(t, d, db.RoleApp)

	if err := postRoleTxn(ctx, app, f, roleTxnSpec{Kind: "charge", Amount: 1250}); err != nil {
		t.Fatalf("app role post: %v", err)
	}

	var kind string
	var seq int64
	if err := app.QueryRow(ctx, `SELECT kind, seq FROM ledger_txns`).Scan(&kind, &seq); err != nil {
		t.Fatalf("app role select ledger_txns: %v", err)
	}
	if kind != "charge" {
		t.Fatalf("kind = %q, want charge", kind)
	}
	if seq <= 0 {
		t.Fatalf("seq = %d, want a positive identity value", seq)
	}

	var entries int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&entries); err != nil {
		t.Fatalf("app role select ledger_entries: %v", err)
	}
	if entries != 2 {
		t.Fatalf("ledger_entries count = %d, want 2", entries)
	}

	var debit int64
	if err := app.QueryRow(ctx, `SELECT amount_minor FROM ledger_entries WHERE direction = 'debit'`).Scan(&debit); err != nil {
		t.Fatalf("app role select debit entry: %v", err)
	}
	if debit != 1250 {
		t.Fatalf("debit amount = %d, want 1250", debit)
	}
}

// TestAppRoleCannotUpdateOrDeleteLedgerRows repeats the M1-03 immutability
// assertions through the application role: grant denial or the immutability
// trigger must reject every mutation (FIN-11).
func TestAppRoleCannotUpdateOrDeleteLedgerRows(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)
	app := openRolePool(t, d, db.RoleApp)

	if err := postRoleTxn(ctx, app, f, roleTxnSpec{Kind: "charge", Amount: 4321}); err != nil {
		t.Fatalf("prepare transaction: %v", err)
	}
	var txnID string
	if err := d.Pool.QueryRow(ctx, `SELECT id::text FROM ledger_txns LIMIT 1`).Scan(&txnID); err != nil {
		t.Fatalf("read transaction id: %v", err)
	}

	mutations := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "update ledger entry",
			sql:  `UPDATE ledger_entries SET amount_minor = amount_minor + 1 WHERE txn_id = $1::uuid`,
			args: []any{txnID},
		},
		{
			name: "delete ledger entry",
			sql:  `DELETE FROM ledger_entries WHERE txn_id = $1::uuid`,
			args: []any{txnID},
		},
		{
			name: "update ledger txn",
			sql:  `UPDATE ledger_txns SET description = 'tampered' WHERE id = $1::uuid`,
			args: []any{txnID},
		},
		{
			name: "delete ledger txn",
			sql:  `DELETE FROM ledger_txns WHERE id = $1::uuid`,
			args: []any{txnID},
		},
		{name: "truncate ledger entries", sql: `TRUNCATE ledger_entries`},
		{name: "truncate ledger txns", sql: `TRUNCATE ledger_txns`},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			_, err := app.Exec(ctx, mutation.sql, mutation.args...)
			// The grant is the first line of defence; the M1-03 trigger is the
			// backstop. Accept either denial, never success.
			pgErr := requirePgError(t, err, sqlStateInsufficientPrivilege, sqlStateRestrictViolation)
			if pgErr.Code == sqlStateInsufficientPrivilege &&
				!strings.Contains(strings.ToLower(pgErr.Message), "permission denied") {
				t.Fatalf("unexpected permission error: %v", err)
			}
		})
	}

	var description string
	if err := d.Pool.QueryRow(ctx, `SELECT description FROM ledger_txns WHERE id = $1::uuid`, txnID).Scan(&description); err != nil {
		t.Fatalf("read transaction after mutations: %v", err)
	}
	if description != "" {
		t.Fatalf("transaction description = %q, want unchanged empty value", description)
	}
	var amount int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT amount_minor FROM ledger_entries WHERE txn_id = $1::uuid AND direction = 'debit'`, txnID).Scan(&amount); err != nil {
		t.Fatalf("read entry after mutations: %v", err)
	}
	if amount != 4321 {
		t.Fatalf("entry amount = %d, want 4321", amount)
	}
}

// TestUnbalancedTransactionFailsAtCommitAppRole repeats the M1-03 FIN-10 check
// as the application role: the deferred balance trigger must run with only the
// role's grants.
func TestUnbalancedTransactionFailsAtCommitAppRole(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)
	app := openRolePool(t, d, db.RoleApp)

	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := insertRoleTxn(ctx, tx, f, roleTxnSpec{Kind: "charge", Amount: 1000, Credit: 999})
	if err != nil {
		t.Fatalf("inserting an unbalanced transaction should succeed before commit: %v", err)
	}

	err = tx.Commit(ctx)
	pgErr := requirePgError(t, err, sqlStateCheckViolation)
	if !strings.Contains(strings.ToLower(pgErr.Message), "unbalanced") {
		t.Fatalf("error %q does not mention unbalanced", pgErr.Message)
	}

	var exists bool
	if err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_txns WHERE id = $1::uuid)`, id).Scan(&exists); err != nil {
		t.Fatalf("check rollback: %v", err)
	}
	if exists {
		t.Fatal("unbalanced transaction persisted after a failed commit")
	}
}

// TestEntryValidationAppRole repeats the M1-03 closed-period, wrong-currency,
// and inactive-account checks through the application role.
func TestEntryValidationAppRole(t *testing.T) {
	t.Run("closed period", func(t *testing.T) {
		ctx := context.Background()
		d := testdb.New(t)
		f := newRoleFixture(t, d)
		app := openRolePool(t, d, db.RoleApp)
		closedPeriod := createClosedPeriod(t, d)

		err := postRoleTxn(ctx, app, f, roleTxnSpec{
			Kind:     "charge",
			Amount:   100,
			PeriodID: closedPeriod,
		})
		pgErr := requirePgError(t, err, sqlStateCheckViolation)
		if !strings.Contains(strings.ToLower(pgErr.Message), "closed") {
			t.Fatalf("error %q does not mention closed", pgErr.Message)
		}
	})

	t.Run("wrong currency", func(t *testing.T) {
		ctx := context.Background()
		d := testdb.New(t)
		f := newRoleFixture(t, d)
		app := openRolePool(t, d, db.RoleApp)

		err := postRoleTxn(ctx, app, f, roleTxnSpec{Kind: "charge", Amount: 100, Currency: "EUR"})
		requirePgError(t, err, sqlStateCheckViolation)
	})

	t.Run("inactive account", func(t *testing.T) {
		ctx := context.Background()
		d := testdb.New(t)
		f := newRoleFixture(t, d)
		app := openRolePool(t, d, db.RoleApp)

		var inactiveID string
		if err := d.Pool.QueryRow(ctx,
			`INSERT INTO accounts (code, name, type, currency, status)
			 VALUES ('role_inactive_usd', 'Role Inactive USD', 'asset', 'USD', 'inactive')
			 RETURNING id::text`).Scan(&inactiveID); err != nil {
			t.Fatalf("create inactive account: %v", err)
		}

		err := db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var id string
			if err := tx.QueryRow(ctx,
				`INSERT INTO ledger_txns (value_date, kind, idempotency_key, request_hash, description, period_id)
				 VALUES ('2026-01-15', 'charge', 'inactive-account', 'hash:inactive', '', $1::uuid)
				 RETURNING id::text`, f.PeriodID).Scan(&id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
				 VALUES ($1::uuid, $2::uuid, 'debit', 100, 'USD')`, id, inactiveID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
				 VALUES ($1::uuid, $2::uuid, 'credit', 100, 'USD')`, id, f.RevenueID)
			return err
		})
		pgErr := requirePgError(t, err, sqlStateCheckViolation)
		if !strings.Contains(strings.ToLower(pgErr.Message), "cannot be posted") {
			t.Fatalf("error %q does not mention inactive posting", pgErr.Message)
		}
	})
}

// TestAgentRoleCannotTouchLedger enforces spec section 3.2 invariant 4: the
// agent runtime role has no ledger privileges at all.
func TestAgentRoleCannotTouchLedger(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)
	agent := openRolePool(t, d, db.RoleAgentRO)

	denied := []struct {
		name string
		sql  string
		args []any
	}{
		{name: "select ledger_txns", sql: `SELECT count(*) FROM ledger_txns`},
		{name: "select ledger_entries", sql: `SELECT count(*) FROM ledger_entries`},
		{
			name: "insert ledger_txns",
			sql: `INSERT INTO ledger_txns (value_date, kind, idempotency_key, request_hash, period_id)
			      VALUES ('2026-01-15', 'charge', 'agent-insert', 'hash', $1::uuid)`,
			args: []any{f.PeriodID},
		},
		{
			name: "insert ledger_entries",
			sql: `INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
			      VALUES (gen_random_uuid(), $1::uuid, 'debit', 100, 'USD')`,
			args: []any{f.ClearingID},
		},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			_, err := agent.Exec(ctx, tc.sql, tc.args...)
			requirePgError(t, err, sqlStateInsufficientPrivilege)
		})
	}
}

// TestAuditRoleCannotReadLedger pins that recon_audit_ro is for the audit log
// only; M1-05 adds its audit_log SELECT grant.
func TestAuditRoleCannotReadLedger(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)
	audit := openRolePool(t, d, db.RoleAuditRO)

	denied := []struct {
		name string
		sql  string
		args []any
	}{
		{name: "select ledger_txns", sql: `SELECT count(*) FROM ledger_txns`},
		{name: "select ledger_entries", sql: `SELECT count(*) FROM ledger_entries`},
		{
			name: "insert ledger_txns",
			sql: `INSERT INTO ledger_txns (value_date, kind, idempotency_key, request_hash, period_id)
			      VALUES ('2026-01-15', 'charge', 'audit-insert', 'hash', $1::uuid)`,
			args: []any{f.PeriodID},
		},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			_, err := audit.Exec(ctx, tc.sql, tc.args...)
			requirePgError(t, err, sqlStateInsufficientPrivilege)
		})
	}
}

// TestAdjustmentRequiresExecutorRole covers the adjustment-only trigger: the
// application role cannot insert kind='adjustment', the executor role can.
func TestAdjustmentRequiresExecutorRole(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)
	app := openRolePool(t, d, db.RoleApp)
	executor := openRolePool(t, d, db.RoleExecutor)

	t.Run("application role is rejected", func(t *testing.T) {
		err := postRoleTxn(ctx, app, f, roleTxnSpec{Kind: "adjustment", Amount: 500})
		pgErr := requirePgError(t, err, sqlStateInsufficientPrivilege)
		if !strings.Contains(strings.ToLower(pgErr.Message), "recon_executor") {
			t.Fatalf("error %q does not name recon_executor", pgErr.Message)
		}

		var count int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger_txns WHERE kind = 'adjustment'`).Scan(&count); err != nil {
			t.Fatalf("count adjustments: %v", err)
		}
		if count != 0 {
			t.Fatalf("adjustment rows after rejected insert = %d, want 0", count)
		}
	})

	t.Run("executor role is accepted", func(t *testing.T) {
		if err := postRoleTxn(ctx, executor, f, roleTxnSpec{Kind: "adjustment", Amount: 700}); err != nil {
			t.Fatalf("executor adjustment post: %v", err)
		}

		var count int
		if err := executor.QueryRow(ctx, `SELECT count(*) FROM ledger_txns WHERE kind = 'adjustment'`).Scan(&count); err != nil {
			t.Fatalf("count adjustments as executor: %v", err)
		}
		if count != 1 {
			t.Fatalf("adjustment rows = %d, want 1", count)
		}
	})

	t.Run("executor role still posts normal transactions", func(t *testing.T) {
		if err := postRoleTxn(ctx, executor, f, roleTxnSpec{Kind: "charge", Amount: 300}); err != nil {
			t.Fatalf("executor charge post: %v", err)
		}
	})
}

// TestEphemeralLoginPools covers per-test provisioning: one random LOGIN per
// role, IN ROLE its group, usable through db.OpenRole, and never the
// maintenance/superuser DSN.
func TestEphemeralLoginPools(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	names := d.LoginNames()
	if len(names) != len(db.Roles()) {
		t.Fatalf("LoginNames() = %v, want one login per role", names)
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, testdb.LoginRolePrefix()) {
			t.Errorf("ephemeral login %q does not use prefix %q", name, testdb.LoginRolePrefix())
		}
		if seen[name] {
			t.Errorf("duplicate ephemeral login %q", name)
		}
		seen[name] = true
	}

	dsns := make(map[string]string, len(db.Roles()))
	for _, role := range db.Roles() {
		dsn := d.DSNFor(role)
		if dsn == "" || dsn == d.DSN {
			t.Fatalf("DSNFor(%s) = %q, want an ephemeral DSN distinct from the maintenance DSN", role, dsn)
		}
		if other, ok := dsns[dsn]; ok {
			t.Fatalf("roles %s and %s share DSN %q", other, role, dsn)
		}
		dsns[dsn] = role

		rp, err := db.OpenRole(ctx, role, dsn)
		if err != nil {
			t.Fatalf("db.OpenRole(%s): %v", role, err)
		}
		t.Cleanup(rp.Close)

		var currentUser, superuser string
		var member bool
		if err := rp.Pool.QueryRow(ctx,
			`SELECT current_user, current_setting('is_superuser'), pg_has_role(current_user, $1::name, 'MEMBER')`,
			role).Scan(&currentUser, &superuser, &member); err != nil {
			t.Fatalf("inspect %s session: %v", role, err)
		}
		if superuser != "off" {
			t.Fatalf("%s pool connects as a superuser", role)
		}
		if !member {
			t.Fatalf("%s pool session %q is not a member of %s", role, currentUser, role)
		}
		if !strings.HasPrefix(currentUser, testdb.LoginRolePrefix()) {
			t.Fatalf("%s pool current_user = %q, want an ephemeral login", role, currentUser)
		}
	}

	if got := d.DSNFor(""); got != d.DSN {
		t.Fatalf("DSNFor(\"\") = %q, want the maintenance DSN %q", got, d.DSN)
	}
	for alias, role := range map[string]string{
		"app":      db.RoleApp,
		"executor": db.RoleExecutor,
		"agent_ro": db.RoleAgentRO,
		"audit_ro": db.RoleAuditRO,
	} {
		if got, want := d.DSNFor(alias), d.DSNFor(role); got != want {
			t.Errorf("DSNFor(%q) = %q, want alias of %s (%q)", alias, got, role, want)
		}
	}

	if _, err := db.OpenRole(ctx, db.RoleApp, d.DSN); !errors.Is(err, db.ErrSuperuserRolePool) {
		t.Fatalf("OpenRole with the maintenance DSN: err = %v, want ErrSuperuserRolePool", err)
	}
	if _, err := db.OpenRole(ctx, db.RoleExecutor, d.DSNFor(db.RoleApp)); !errors.Is(err, db.ErrRoleNotMember) {
		t.Fatalf("OpenRole(app DSN) as executor: err = %v, want ErrRoleNotMember", err)
	}
	if _, err := db.OpenRole(ctx, "recon_missing", d.DSNFor(db.RoleApp)); !errors.Is(err, db.ErrUnknownRole) {
		t.Fatalf("OpenRole(unknown role): err = %v, want ErrUnknownRole", err)
	}
}

// TestDSNForUnknownRolePanics documents the fail-closed contract: a misspelled
// role must not silently fall back to the maintenance DSN.
func TestDSNForUnknownRolePanics(t *testing.T) {
	d := testdb.New(t)
	defer func() {
		if recover() == nil {
			t.Fatal("DSNFor(unknown role) did not panic")
		}
	}()
	_ = d.DSNFor("recon_missing")
}
