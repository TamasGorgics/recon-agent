//go:build integration

// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34
package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const (
	m413MigrationVersion = 20261119120000
	m413PreviousVersion  = 20261117120000

	m413ConstraintName = "audit_log_actor_id_format"
	// m413ConstraintDef is the exact pg_get_constraintdef output for the new
	// named CHECK on PostgreSQL 16.
	m413ConstraintDef = `CHECK ((actor_id ~ '^[^[:space:][:cntrl:]:]{1,128}$'::text))`
	// m413ActorTypeCheckDef and m413ActorIDCheckDef are the m1_05 checks that
	// the migration must leave byte-for-byte unchanged.
	m413ActorTypeCheckDef = `CHECK ((actor_type = ANY (ARRAY['system'::text, 'agent'::text, 'human'::text])))`
	m413ActorIDCheckDef   = `CHECK ((actor_id <> ''::text))`
)

// TestM413ActorIDFormatConstraintDefinition proves the template database is at
// the new version and that the named CHECK has exactly the approved definition.
// The pre-existing m1_05 actor checks must be unchanged.
func TestM413ActorIDFormatConstraintDefinition(t *testing.T) {
	d := testdb.New(t)

	m413RequireConstraint(t, d, m413ConstraintName, m413ConstraintDef)
	m413RequireConstraint(t, d, "audit_log_actor_type_check", m413ActorTypeCheckDef)
	m413RequireConstraint(t, d, "audit_log_actor_id_check", m413ActorIDCheckDef)
}

// TestM413ActorIDFormatRejectsMalformedInserts drives malformed actor ids
// through the real constraint and proves valid canonical actor pairs still
// insert. Empty ids are rejected (the older non-empty check may report first).
func TestM413ActorIDFormatRejectsMalformedInserts(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	var before int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&before); err != nil {
		t.Fatalf("count audit_log before inserts: %v", err)
	}

	invalid := []struct {
		name      string
		actorType string
		actorID   string
		wantExact bool
	}{
		{"ascii space", "system", "bad id", true},
		{"leading space", "system", " bad", true},
		{"tab", "system", "bad\tid", true},
		{"newline", "system", "bad\nid", true},
		{"control", "agent", "bad\x01id", true},
		{"DEL", "agent", "bad\x7fid", true},
		{"unicode space backstop", "human", "bad\u2003id", true},
		{"colon", "system", "bad:id", true},
		{"129 runes", "human", strings.Repeat("a", 129), true},
		{"empty", "system", "", false},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			err := m413InsertRawActorRow(ctx, d, tc.actorType, tc.actorID)
			pgErr := requirePgError(t, err, sqlStateCheckViolation)
			if tc.wantExact {
				if pgErr.ConstraintName != m413ConstraintName {
					t.Fatalf("constraint = %q, want %q", pgErr.ConstraintName, m413ConstraintName)
				}
				return
			}
			// The empty string violates both the old non-empty check and the new
			// format check; PostgreSQL may report either one.
			if pgErr.ConstraintName != m413ConstraintName && pgErr.ConstraintName != "audit_log_actor_id_check" {
				t.Fatalf("constraint = %q, want one of %q or audit_log_actor_id_check",
					pgErr.ConstraintName, m413ConstraintName)
			}
		})
	}

	var after int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&after); err != nil {
		t.Fatalf("count audit_log after invalid inserts: %v", err)
	}
	if after != before {
		t.Fatalf("audit_log rows after rejected inserts = %d, want %d", after, before)
	}

	valid := []struct {
		name      string
		actorType string
		actorID   string
	}{
		{"system agent-loop", "system", "agent-loop"},
		{"human dev-admin", "human", "dev-admin"},
		{"agent model", "agent", "model"},
		{"128 runes", "system", strings.Repeat("a", 128)},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			if err := m413InsertRawActorRow(ctx, d, tc.actorType, tc.actorID); err != nil {
				t.Fatalf("insert valid actor %s:%s: %v", tc.actorType, tc.actorID, err)
			}
		})
	}
}

// TestM413ActorIDFormatMigrationUpDownUp applies the real goose Down and Up
// twice on a disposable database. It proves Down removes only the new CHECK,
// existing (hash-valid) rows survive Up unchanged, and the chain still
// verifies.
func TestM413ActorIDFormatMigrationUpDownUp(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	gooseDB := m413OpenGoose(t, d)

	m413RequireConstraint(t, d, m413ConstraintName, m413ConstraintDef)

	var wantActors []string
	for cycle := 0; cycle < 2; cycle++ {
		// Newer migrations (M4-12 and later) sit above M4-13; unwind down to
		// M4-13's predecessor so this test still exercises the M4-13 Down.
		if err := goose.DownToContext(ctx, gooseDB, ".", m413PreviousVersion); err != nil {
			t.Fatalf("cycle %d: apply migrations after M4-13 Down: %v", cycle, err)
		}
		m413RequireConstraintAbsent(t, d, m413ConstraintName)
		m413RequireConstraint(t, d, "audit_log_actor_type_check", m413ActorTypeCheckDef)
		m413RequireConstraint(t, d, "audit_log_actor_id_check", m413ActorIDCheckDef)
		if version := m413GooseVersion(t, d); version != m413PreviousVersion {
			t.Fatalf("cycle %d: version after Down = %d, want %d", cycle, version, m413PreviousVersion)
		}

		if cycle == 0 {
			wantActors = m413AppendExistingRows(t, d)
		}
		count, actors := m413AuditActorRows(t, d)
		if count != len(wantActors) || !slices.Equal(actors, wantActors) {
			t.Fatalf("cycle %d: rows before Up = %d %v, want %d %v", cycle, count, actors, len(wantActors), wantActors)
		}

		if err := goose.UpToContext(ctx, gooseDB, ".", m413MigrationVersion); err != nil {
			t.Fatalf("cycle %d: apply M4-13 Up: %v", cycle, err)
		}
		m413RequireConstraint(t, d, m413ConstraintName, m413ConstraintDef)
		if version := m413GooseVersion(t, d); version != m413MigrationVersion {
			t.Fatalf("cycle %d: version after Up = %d, want %d", cycle, version, m413MigrationVersion)
		}

		count, actors = m413AuditActorRows(t, d)
		if count != len(wantActors) || !slices.Equal(actors, wantActors) {
			t.Fatalf("cycle %d: rows after Up = %d %v, want %d %v", cycle, count, actors, len(wantActors), wantActors)
		}

		report, err := audit.Verify(ctx, d.Pool, 1)
		if err != nil {
			t.Fatalf("cycle %d: audit.Verify after Up: %v", cycle, err)
		}
		if !report.OK() {
			t.Fatalf("cycle %d: audit.Verify after Up found problems: %v", cycle, report.Problems)
		}
	}
}

// TestM413ActorIDFormatMigrationRejectsInvalidExistingRows proves the
// migration fails closed on pre-existing violations: it reports the count and
// constraint name only, adds no partial constraint, records no version, and
// never rewrites the offending rows.
func TestM413ActorIDFormatMigrationRejectsInvalidExistingRows(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	gooseDB := m413OpenGoose(t, d)

	if err := goose.DownToContext(ctx, gooseDB, ".", m413PreviousVersion); err != nil {
		t.Fatalf("apply migrations after M4-13 Down before seeding invalid rows: %v", err)
	}

	const (
		firstInvalid  = "pre-existing bad id"
		secondInvalid = "pre-existing:bad"
	)
	for _, actorID := range []string{firstInvalid, secondInvalid} {
		if err := m413InsertRawActorRow(ctx, d, "system", actorID); err != nil {
			t.Fatalf("insert pre-existing invalid actor %q with the constraint absent: %v", actorID, err)
		}
	}

	err := goose.UpToContext(ctx, gooseDB, ".", m413MigrationVersion)
	if err == nil {
		t.Fatal("M4-13 Up succeeded with invalid pre-existing actor rows, want fail-closed error")
	}
	for _, want := range []string{m413ConstraintName, "2 existing"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Up error %q does not contain %q", err, want)
		}
	}
	for _, leak := range []string{firstInvalid, secondInvalid} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("Up error leaked the offending actor id %q: %v", leak, err)
		}
	}

	m413RequireConstraintAbsent(t, d, m413ConstraintName)
	if version := m413GooseVersion(t, d); version != m413PreviousVersion {
		t.Fatalf("version after failed Up = %d, want %d", version, m413PreviousVersion)
	}

	var invalidRows int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE actor_id IN ($1, $2)`,
		firstInvalid, secondInvalid).Scan(&invalidRows); err != nil {
		t.Fatalf("count pre-existing invalid rows: %v", err)
	}
	if invalidRows != 2 {
		t.Fatalf("invalid rows after failed Up = %d, want 2 (no rewrite/removal)", invalidRows)
	}
}

func m413OpenGoose(t *testing.T, d *testdb.DB) *sql.DB {
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

func m413Constraint(t *testing.T, d *testdb.DB, name string) (contype, definition string, ok bool) {
	t.Helper()
	err := d.Pool.QueryRow(context.Background(),
		`SELECT contype::text, pg_get_constraintdef(oid)
		   FROM pg_constraint
		  WHERE conrelid = 'public.audit_log'::regclass AND conname = $1`, name).
		Scan(&contype, &definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("read constraint %s: %v", name, err)
	}
	return contype, definition, true
}

func m413RequireConstraint(t *testing.T, d *testdb.DB, name, wantDefinition string) {
	t.Helper()
	contype, definition, ok := m413Constraint(t, d, name)
	if !ok {
		t.Fatalf("constraint %s is missing", name)
	}
	if contype != "c" {
		t.Fatalf("constraint %s contype = %q, want c", name, contype)
	}
	if definition != wantDefinition {
		t.Fatalf("constraint %s definition = %q, want %q", name, definition, wantDefinition)
	}
}

func m413RequireConstraintAbsent(t *testing.T, d *testdb.DB, name string) {
	t.Helper()
	if _, definition, ok := m413Constraint(t, d, name); ok {
		t.Fatalf("constraint %s still exists with definition %q", name, definition)
	}
}

func m413GooseVersion(t *testing.T, d *testdb.DB) int64 {
	t.Helper()
	var version int64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	return version
}

func m413InsertRawActorRow(ctx context.Context, d *testdb.DB, actorType, actorID string) error {
	_, err := d.Pool.Exec(ctx,
		`INSERT INTO audit_log
		     (ts, actor_type, actor_id, action, entity_type, entity_id, prev_hash, row_hash)
		 VALUES (now(), $1, $2, 'm4-13.actor-probe', 'm4-13', 'probe',
		         decode(repeat('00', 32), 'hex'), decode(repeat('11', 32), 'hex'))`,
		actorType, actorID)
	return err
}

// m413AppendExistingRows writes three hash-valid rows while the new constraint
// is absent, using the real audit writer and canonical actor strings.
func m413AppendExistingRows(t *testing.T, d *testdb.DB) []string {
	t.Helper()
	ctx := context.Background()
	actors := []string{"system:agent-loop", "human:dev-admin", "agent:model"}
	ts := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	err := db.WithTx(ctx, d.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for _, canonical := range actors {
			actor, err := audit.ParseActor(canonical)
			if err != nil {
				return fmt.Errorf("parse actor %q: %w", canonical, err)
			}
			if _, err := audit.Append(ctx, tx, audit.Entry{
				Actor:      actor,
				Action:     "m4-13.existing",
				EntityType: "actor-probe",
				EntityID:   string(actor.Type()) + "/" + actor.ID(),
				Timestamp:  ts,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("append hash-valid existing rows: %v", err)
	}
	return actors
}

func m413AuditActorRows(t *testing.T, d *testdb.DB) (int, []string) {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT actor_type || ':' || actor_id FROM audit_log ORDER BY seq`)
	if err != nil {
		t.Fatalf("read audit actor rows: %v", err)
	}
	defer rows.Close()

	var actors []string
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			t.Fatalf("scan audit actor row: %v", err)
		}
		actors = append(actors, actor)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit actor rows: %v", err)
	}
	return len(actors), actors
}
