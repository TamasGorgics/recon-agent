//go:build integration

// covers: FIN-22, FIN-23
package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const (
	m412MigrationVersion = 20261120100000
	m412PreviousVersion  = 20261119120000
)

// TestM412ApprovalMessagesMigrationVersion proves the M4-12 migration was
// applied exactly once by goose.
func TestM412ApprovalMessagesMigrationVersion(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	var count int
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(is_applied), false)
		   FROM goose_db_version WHERE version_id = $1`, m412MigrationVersion).
		Scan(&count, &applied); err != nil {
		t.Fatalf("read M4-12 goose version: %v", err)
	}
	if count != 1 || !applied {
		t.Fatalf("M4-12 migration rows/applied = %d/%t, want 1/true", count, applied)
	}
}

// TestM412ApprovalMessagesSchema pins the exact table shape from spec section
// 4.1: six columns, the (proposal_id, channel) primary key, the proposal FK
// with CASCADE, and named non-empty/state checks.
func TestM412ApprovalMessagesSchema(t *testing.T) {
	d := testdb.New(t)

	wantColumns := map[string]m4ColumnSpec{
		"proposal_id":  {"uuid", "NO", ""},
		"channel":      {"text", "NO", ""},
		"external_ref": {"text", "NO", ""},
		"state":        {"text", "NO", ""},
		"created_at":   {"timestamp with time zone", "NO", "now()"},
		"updated_at":   {"timestamp with time zone", "NO", "now()"},
	}
	columns := m4TableColumns(t, d, "approval_messages")
	if len(columns) != len(wantColumns) {
		t.Fatalf("approval_messages has %d columns, want %d: got %v",
			len(columns), len(wantColumns), m4ColumnNames(columns))
	}
	for name, want := range wantColumns {
		got, ok := columns[name]
		if !ok {
			t.Fatalf("approval_messages.%s is missing; got columns %v",
				name, m4ColumnNames(columns))
		}
		if got != want {
			t.Errorf("approval_messages.%s = %+v, want %+v", name, got, want)
		}
	}

	constraints := m4TableConstraints(t, d, "approval_messages")
	if len(constraints) != 5 {
		t.Fatalf("approval_messages has %d constraints, want 5: %v",
			len(constraints), constraints)
	}
	m4RequireConstraint(t, constraints, "approval_messages_pkey", "p",
		"PRIMARY KEY (proposal_id, channel)")
	m4RequireConstraint(t, constraints, "approval_messages_channel_check", "c",
		"channel <> ''")
	m4RequireConstraint(t, constraints, "approval_messages_external_ref_check", "c",
		"external_ref <> ''")
	m4RequireConstraint(t, constraints, "approval_messages_state_check", "c",
		"state = ANY", "'posted'", "'updated'", "'cancelled'")

	refTable, deleteAction := m4ForeignKey(t, d, "approval_messages",
		"approval_messages_proposal_id_fkey")
	if refTable != "proposals" || deleteAction != "c" {
		t.Fatalf("proposal FK references %q with delete action %q, want proposals/c",
			refTable, deleteAction)
	}
}

// TestM412ApprovalMessagesConstraintsAndCascade drives the real constraints:
// a valid row inserts, duplicates and empty values fail with the named
// constraint, an unknown proposal fails the FK, and deleting the proposal
// cascades the message rows.
func TestM412ApprovalMessagesConstraintsAndCascade(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	proposalID := m4ApprovedProposal(t, d, "m4-12-approval-messages")

	var createdAt, updatedAt string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
		 VALUES ($1::uuid, 'slack', 'C0123456789:1732041600.123456', 'posted')
		 RETURNING created_at::text, updated_at::text`, proposalID).
		Scan(&createdAt, &updatedAt); err != nil {
		t.Fatalf("insert valid approval message: %v", err)
	}
	if createdAt == "" || createdAt != updatedAt {
		t.Fatalf("database clock defaults = %q/%q, want equal non-empty values",
			createdAt, updatedAt)
	}

	duplicate := func() error {
		_, err := d.Pool.Exec(ctx,
			`INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
			 VALUES ($1::uuid, 'slack', 'C9999999999:9999999999.999999', 'posted')`,
			proposalID)
		return err
	}
	if err := duplicate(); err == nil {
		t.Fatal("duplicate (proposal_id, channel) accepted")
	} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "approval_messages_pkey" {
		t.Fatalf("duplicate constraint = %q, want approval_messages_pkey", pgErr.ConstraintName)
	}

	for _, test := range []struct {
		name       string
		statement  string
		args       []any
		constraint string
		code       string
	}{
		{
			name: "empty channel",
			statement: `INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
			            VALUES ($1::uuid, '', 'C0123456789:1732041600.123456', 'posted')`,
			args:       []any{proposalID},
			constraint: "approval_messages_channel_check",
			code:       sqlStateCheckViolation,
		},
		{
			name: "empty external ref",
			statement: `INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
			            VALUES ($1::uuid, 'ticket', '', 'posted')`,
			args:       []any{proposalID},
			constraint: "approval_messages_external_ref_check",
			code:       sqlStateCheckViolation,
		},
		{
			name: "unknown state",
			statement: `INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
			            VALUES ($1::uuid, 'ticket', 'ticket:42', 'posting')`,
			args:       []any{proposalID},
			constraint: "approval_messages_state_check",
			code:       sqlStateCheckViolation,
		},
		{
			name: "missing proposal",
			statement: `INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
			            VALUES ($1::uuid, 'ticket', 'ticket:42', 'posted')`,
			args:       []any{m4MissingUUID},
			constraint: "approval_messages_proposal_id_fkey",
			code:       sqlStateForeignKey,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := d.Pool.Exec(ctx, test.statement, test.args...)
			if err == nil {
				t.Fatal("invalid insert succeeded")
			}
			pgErr := requirePgError(t, err, test.code)
			if pgErr.ConstraintName != test.constraint {
				t.Fatalf("constraint = %q, want %q", pgErr.ConstraintName, test.constraint)
			}
		})
	}

	// One row per channel is allowed for the same proposal.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
		 VALUES ($1::uuid, 'ticket', 'ticket:42', 'posted')`, proposalID); err != nil {
		t.Fatalf("insert second channel: %v", err)
	}

	if _, err := d.Pool.Exec(ctx,
		`DELETE FROM proposals WHERE id = $1::uuid`, proposalID); err != nil {
		t.Fatalf("delete proposal: %v", err)
	}
	var remaining int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM approval_messages WHERE proposal_id = $1::uuid`,
		proposalID).Scan(&remaining); err != nil {
		t.Fatalf("count cascaded rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("approval_messages rows after proposal delete = %d, want 0", remaining)
	}
}

// TestM412ApprovalMessagesGrants proves the privilege matrix: recon_app has
// SELECT/INSERT/UPDATE and no DELETE; executor, agent-read, and audit-read
// roles have nothing; PUBLIC has nothing. The real recon_app login exercises
// the granted statements.
func TestM412ApprovalMessagesGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	proposalID := m4ApprovedProposal(t, d, "m4-12-message-grants")

	if got := m412TablePrivileges(t, d, db.RoleApp); got != "INSERT,SELECT,UPDATE" {
		t.Fatalf("recon_app privileges = %q, want INSERT,SELECT,UPDATE", got)
	}
	for _, privilege := range []string{"DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
		if m412RolePrivilege(t, d, db.RoleApp, privilege) {
			t.Errorf("recon_app has %s on approval_messages, want denied", privilege)
		}
	}
	for _, role := range []string{db.RoleExecutor, db.RoleAgentRO, db.RoleAuditRO} {
		if got := m412TablePrivileges(t, d, role); got != "" {
			t.Errorf("%s privileges = %q, want none", role, got)
		}
		for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			if m412RolePrivilege(t, d, role, privilege) {
				t.Errorf("%s has %s on approval_messages, want denied", role, privilege)
			}
		}
	}

	var publicGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND table_name = 'approval_messages'
		    AND grantee = 'PUBLIC'`).Scan(&publicGrants); err != nil {
		t.Fatalf("read PUBLIC grants: %v", err)
	}
	if publicGrants != 0 {
		t.Fatalf("PUBLIC holds %d approval_messages grants, want 0", publicGrants)
	}
	var publicColumnGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_column_grants
		  WHERE table_schema = 'public' AND table_name = 'approval_messages'
		    AND grantee = 'PUBLIC'`).Scan(&publicColumnGrants); err != nil {
		t.Fatalf("read PUBLIC column grants: %v", err)
	}
	if publicColumnGrants != 0 {
		t.Fatalf("PUBLIC holds %d approval_messages column grants, want 0", publicColumnGrants)
	}

	app := openRolePool(t, d, db.RoleApp)
	if _, err := app.Exec(ctx,
		`INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
		 VALUES ($1::uuid, 'slack', 'C0123456789:1732041600.123456', 'posted')`,
		proposalID); err != nil {
		t.Fatalf("app insert approval_messages: %v", err)
	}
	if _, err := app.Exec(ctx,
		`UPDATE approval_messages SET state = 'updated', updated_at = now()
		  WHERE proposal_id = $1::uuid AND channel = 'slack'`, proposalID); err != nil {
		t.Fatalf("app update approval_messages: %v", err)
	}
	var state string
	if err := app.QueryRow(ctx,
		`SELECT state FROM approval_messages
		  WHERE proposal_id = $1::uuid AND channel = 'slack'`, proposalID).Scan(&state); err != nil {
		t.Fatalf("app select approval_messages: %v", err)
	}
	if state != "updated" {
		t.Fatalf("app-read state = %q, want updated", state)
	}
	if _, err := app.Exec(ctx,
		`DELETE FROM approval_messages WHERE proposal_id = $1::uuid`, proposalID); err == nil {
		t.Fatal("app DELETE approval_messages succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
}

// TestM412ApprovalMessagesMigrationDown applies the real goose Down and then
// Up on a disposable clone. Down removes only the M4-12 table and its grants;
// earlier migrations' objects remain, and Up restores the table and version.
func TestM412ApprovalMessagesMigrationDown(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	gooseDB := m412OpenGoose(t, d)

	// Later migrations (for example M5-01) must be unwound first so this test
	// still exercises M4-12's own Down rather than the newest migration's.
	if err := goose.DownToContext(ctx, gooseDB, ".", m412MigrationVersion); err != nil {
		t.Fatalf("unwind migrations newer than M4-12: %v", err)
	}
	if err := goose.DownContext(ctx, gooseDB, "."); err != nil {
		t.Fatalf("apply M4-12 Down: %v", err)
	}
	if version := m412GooseVersion(t, d); version != m412PreviousVersion {
		t.Fatalf("version after Down = %d, want %d", version, m412PreviousVersion)
	}
	var exists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT to_regclass('public.approval_messages') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("resolve approval_messages after Down: %v", err)
	}
	if exists {
		t.Fatal("approval_messages still exists after Down")
	}
	for _, table := range []string{"proposals", "approvals", "approvers", "outbox"} {
		if err := d.Pool.QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("resolve %s after Down: %v", table, err)
		}
		if !exists {
			t.Errorf("%s was dropped by the M4-12 Down", table)
		}
	}
	var grants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND table_name = 'approval_messages'
		    AND grantee = 'recon_app'`).Scan(&grants); err != nil {
		t.Fatalf("read app grants after Down: %v", err)
	}
	if grants != 0 {
		t.Fatalf("approval_messages grants after Down = %d, want 0", grants)
	}

	if err := goose.UpContext(ctx, gooseDB, "."); err != nil {
		t.Fatalf("re-apply M4-12 Up: %v", err)
	}
	// goose.Up migrates to the latest embedded version, not just M4-12.
	if version := m412GooseVersion(t, d); version != m505MigrationVersion {
		t.Fatalf("version after re-Up = %d, want latest %d", version, m505MigrationVersion)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT to_regclass('public.approval_messages') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("resolve approval_messages after re-Up: %v", err)
	}
	if !exists {
		t.Fatal("approval_messages was not recreated by Up")
	}
	if got := m412TablePrivileges(t, d, db.RoleApp); got != "INSERT,SELECT,UPDATE" {
		t.Fatalf("recon_app privileges after re-Up = %q, want INSERT,SELECT,UPDATE", got)
	}
}

func m412OpenGoose(t *testing.T, d *testdb.DB) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("pgx", d.DSN)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping scratch database: %v", err)
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("configure goose: %v", err)
	}
	return sqlDB
}

func m412GooseVersion(t *testing.T, d *testdb.DB) int64 {
	t.Helper()
	var version int64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	return version
}

// m412RolePrivilege answers has_table_privilege for approval_messages, so
// inherited grants are caught as well as direct grants.
func m412RolePrivilege(t *testing.T, d *testdb.DB, role, privilege string) bool {
	t.Helper()
	var allowed bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1::name, 'public.approval_messages', $2::text)`,
		role, privilege).Scan(&allowed); err != nil {
		t.Fatalf("read %s %s on approval_messages: %v", role, privilege, err)
	}
	return allowed
}

// m412TablePrivileges returns the granted table privileges for role in a
// stable order, excluding column privileges.
func m412TablePrivileges(t *testing.T, d *testdb.DB, role string) string {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT privilege_type
		   FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND table_name = 'approval_messages'
		    AND grantee = $1
		  ORDER BY privilege_type`, role)
	if err != nil {
		t.Fatalf("read %s approval_messages grants: %v", role, err)
	}
	defer rows.Close()

	var privileges []string
	for rows.Next() {
		var privilege string
		if err := rows.Scan(&privilege); err != nil {
			t.Fatalf("scan %s grant: %v", role, err)
		}
		privileges = append(privileges, privilege)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s grants: %v", role, err)
	}
	return strings.Join(privileges, ",")
}
