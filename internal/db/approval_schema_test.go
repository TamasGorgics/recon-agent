//go:build integration

// covers: FIN-11
package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const (
	sqlStateNotNullViolation = "23502"
	sqlStateForeignKey       = "23503"
	sqlStateUniqueViolation  = "23505"
	m4MissingUUID            = "00000000-0000-4000-8000-000000000000"
)

// m4Tables is the exact M4-01 table set. No other table may share an M4-01
// prefix (approv/applied/outbox/policy).
var m4Tables = []string{
	"approvers",
	"approvals",
	"applied_proposals",
	"outbox",
	"policy_config_versions",
}

type m4ColumnSpec struct {
	dataType string
	nullable string
	def      string // exact column_default; "" means no default
}

type m4Constraint struct {
	contype string
	def     string
}

// TestM401MigrationVersion proves the M4-01 goose version was applied exactly
// once. Re-running make migrate must not add a second row; goose's version
// table is the idempotency mechanism (ADR 0002).
func TestM401MigrationVersion(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	var count int
	var applied bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(is_applied), false)
		   FROM goose_db_version
		  WHERE version_id = 20261110120000`).Scan(&count, &applied); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if count != 1 {
		t.Fatalf("goose rows for 20261110120000 = %d, want exactly 1", count)
	}
	if !applied {
		t.Fatal("goose row for 20261110120000 is not applied")
	}
}

// TestM401Schema pins the five tables, their exact columns/types/nullability/
// defaults, primary keys, FKs with RESTRICT, named uniques, semantic checks,
// and the two indexes (spec section 4).
func TestM401Schema(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	wantColumns := map[string]map[string]m4ColumnSpec{
		"approvers": {
			"id":             {"uuid", "NO", "gen_random_uuid()"},
			"display_name":   {"text", "NO", ""},
			"role":           {"text", "NO", ""},
			"slack_user_id":  {"text", "YES", ""},
			"ticket_user_id": {"text", "YES", ""},
			"active":         {"boolean", "NO", "true"},
		},
		"approvals": {
			"id":              {"uuid", "NO", "gen_random_uuid()"},
			"proposal_id":     {"uuid", "NO", ""},
			"channel":         {"text", "NO", ""},
			"external_ref":    {"text", "YES", ""},
			"approver_id":     {"uuid", "NO", ""},
			"decision":        {"text", "NO", ""},
			"decided_at":      {"timestamp with time zone", "NO", ""},
			"signature_valid": {"boolean", "NO", "false"},
		},
		"applied_proposals": {
			"proposal_id":     {"uuid", "NO", ""},
			"ledger_txn_id":   {"uuid", "YES", ""},
			"applied_at":      {"timestamp with time zone", "NO", ""},
			"executor_run_id": {"text", "NO", ""},
		},
		"outbox": {
			"id":              {"uuid", "NO", "gen_random_uuid()"},
			"aggregate_type":  {"text", "NO", ""},
			"aggregate_id":    {"text", "NO", ""},
			"topic":           {"text", "NO", ""},
			"payload":         {"jsonb", "NO", ""},
			"idempotency_key": {"text", "NO", ""},
			"created_at":      {"timestamp with time zone", "NO", ""},
			"delivered_at":    {"timestamp with time zone", "YES", ""},
			"attempts":        {"integer", "NO", "0"},
			"next_attempt_at": {"timestamp with time zone", "NO", ""},
			"dead":            {"boolean", "NO", "false"},
		},
		"policy_config_versions": {
			"hash":      {"text", "NO", ""},
			"yaml":      {"text", "NO", ""},
			"loaded_at": {"timestamp with time zone", "NO", ""},
		},
	}

	for table, columns := range wantColumns {
		t.Run("table/"+table, func(t *testing.T) {
			var exists bool
			if err := d.Pool.QueryRow(ctx,
				`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
				t.Fatalf("resolve table %s: %v", table, err)
			}
			if !exists {
				t.Fatalf("table %s is missing", table)
			}
			got := m4TableColumns(t, d, table)
			if len(got) != len(columns) {
				t.Fatalf("%s has %d columns, want %d: got %v", table, len(got), len(columns), m4ColumnNames(got))
			}
			for name, want := range columns {
				have, ok := got[name]
				if !ok {
					t.Fatalf("%s.%s is missing; got columns %v", table, name, m4ColumnNames(got))
				}
				if have.dataType != want.dataType {
					t.Errorf("%s.%s data_type = %q, want %q", table, name, have.dataType, want.dataType)
				}
				if have.nullable != want.nullable {
					t.Errorf("%s.%s is_nullable = %q, want %q", table, name, have.nullable, want.nullable)
				}
				if have.def != want.def {
					t.Errorf("%s.%s column_default = %q, want %q", table, name, have.def, want.def)
				}
			}
		})
	}

	// No extra M4-01-prefixed table (the rest of the schema is owned by M1-M3).
	rows, err := d.Pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		    AND (table_name LIKE 'approv%' OR table_name LIKE 'applied%'
		         OR table_name LIKE 'outbox%' OR table_name LIKE 'policy%')`)
	if err != nil {
		t.Fatalf("list M4-01 tables: %v", err)
	}
	defer rows.Close()
	gotTables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		gotTables[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	if len(gotTables) != len(m4Tables) {
		t.Fatalf("M4-01 tables = %v, want exactly %v", gotTables, m4Tables)
	}
	for _, table := range m4Tables {
		if !gotTables[table] {
			t.Fatalf("M4-01 table %s is missing from %v", table, gotTables)
		}
	}

	// Primary keys.
	for table, wantName := range map[string]string{
		"approvers":              "approvers_pkey",
		"approvals":              "approvals_pkey",
		"applied_proposals":      "applied_proposals_pkey",
		"outbox":                 "outbox_pkey",
		"policy_config_versions": "policy_config_versions_pkey",
	} {
		t.Run("pk/"+table, func(t *testing.T) {
			cons := m4TableConstraints(t, d, table)
			m4RequireConstraint(t, cons, wantName, "p", "")
		})
	}

	// FKs: names, target tables, and ON DELETE RESTRICT.
	type fkWant struct {
		table, name, refTable string
	}
	for _, fk := range []fkWant{
		{"approvals", "approvals_proposal_id_fkey", "proposals"},
		{"approvals", "approvals_approver_id_fkey", "approvers"},
		{"applied_proposals", "applied_proposals_proposal_id_fkey", "proposals"},
		{"applied_proposals", "applied_proposals_ledger_txn_fkey", "ledger_txns"},
	} {
		t.Run("fk/"+fk.table+"/"+fk.name, func(t *testing.T) {
			refTable, deleteAction := m4ForeignKey(t, d, fk.table, fk.name)
			if refTable != fk.refTable {
				t.Fatalf("%s references %s, want %s", fk.name, refTable, fk.refTable)
			}
			if deleteAction != "r" {
				t.Fatalf("%s confdeltype = %q, want 'r' (RESTRICT)", fk.name, deleteAction)
			}
		})
	}

	// Named unique constraints frozen by the spec.
	for _, unique := range []struct {
		table, name string
		columns     []string
	}{
		{"approvers", "approvers_slack_user_id_key", []string{"slack_user_id"}},
		{"approvers", "approvers_ticket_user_id_key", []string{"ticket_user_id"}},
		{"approvals", "approvals_proposal_approver_key", []string{"proposal_id", "approver_id"}},
		{"applied_proposals", "applied_proposals_ledger_txn_key", []string{"ledger_txn_id"}},
		{"outbox", "outbox_idempotency_key_key", []string{"idempotency_key"}},
	} {
		t.Run("unique/"+unique.name, func(t *testing.T) {
			cons := m4TableConstraints(t, d, unique.table)
			got, ok := cons[unique.name]
			if !ok {
				t.Fatalf("%s is missing (constraints: %v)", unique.name, cons)
			}
			if got.contype != "u" {
				t.Fatalf("%s contype = %q, want unique", unique.name, got.contype)
			}
			for _, column := range unique.columns {
				if !strings.Contains(got.def, column) {
					t.Fatalf("%s definition %q does not contain %s", unique.name, got.def, column)
				}
			}
		})
	}

	// Semantic checks. PostgreSQL auto-names anonymous checks, so assert the
	// condition rather than an invented name.
	m4RequireCheck(t, d, "approvers", "display_name", "<> ''")
	m4RequireCheck(t, d, "approvers", "role", "'viewer'", "'approver'", "'admin'")
	m4RequireCheck(t, d, "approvers", "slack_user_id", "<> ''")
	m4RequireCheck(t, d, "approvers", "ticket_user_id", "<> ''")
	m4RequireCheck(t, d, "approvals", "decision", "'approved'", "'rejected'")
	m4RequireCheck(t, d, "approvals", "channel", "<> ''")
	m4RequireCheck(t, d, "approvals", "external_ref", "<> ''")
	m4RequireCheck(t, d, "applied_proposals", "executor_run_id", "<> ''")
	m4RequireCheck(t, d, "outbox", "aggregate_type", "<> ''")
	m4RequireCheck(t, d, "outbox", "aggregate_id", "<> ''")
	m4RequireCheck(t, d, "outbox", "topic", "<> ''")
	m4RequireCheck(t, d, "outbox", "attempts", ">= 0")
	m4RequireCheck(t, d, "outbox", "dead", "delivered_at IS NULL")
	m4RequireCheck(t, d, "policy_config_versions", "hash", "[0-9a-f]{64}")
	m4RequireCheck(t, d, "policy_config_versions", "yaml", "<> ''")

	// Approvals read-path index.
	var proposalIndex string
	if err := d.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND indexname = 'approvals_proposal_id_idx'`).
		Scan(&proposalIndex); err != nil {
		t.Fatalf("read approvals_proposal_id_idx: %v", err)
	}
	if !strings.Contains(proposalIndex, "(proposal_id)") || strings.Contains(proposalIndex, "WHERE") {
		t.Fatalf("approvals_proposal_id_idx = %q, want a non-partial (proposal_id) index", proposalIndex)
	}

	// Outbox due-claim partial index. Assert the semantic predicate, not one
	// exact deparse spelling of `dead = false`.
	var outboxIndex string
	if err := d.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND indexname = 'outbox_undelivered_idx'`).
		Scan(&outboxIndex); err != nil {
		t.Fatalf("read outbox_undelivered_idx: %v", err)
	}
	for _, want := range []string{"next_attempt_at", "id", "delivered_at IS NULL", "WHERE"} {
		if !strings.Contains(outboxIndex, want) {
			t.Fatalf("outbox_undelivered_idx = %q, missing %q", outboxIndex, want)
		}
	}
	if !strings.Contains(outboxIndex, "dead = false") && !strings.Contains(outboxIndex, "NOT dead") {
		t.Fatalf("outbox_undelivered_idx = %q, missing the dead predicate", outboxIndex)
	}
}

// TestM401Constraints covers the negative DDL checks, FK actions, and the
// SQLSTATE contract (spec AC-3).
func TestM401Constraints(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)

	t.Run("approvers", func(t *testing.T) {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvers (display_name, role) VALUES ('', 'approver')`); err == nil {
			t.Fatal("empty display_name accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvers (display_name, role) VALUES ('Bad Role', 'owner')`); err == nil {
			t.Fatal("invalid role accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvers (display_name, role, slack_user_id) VALUES ('Empty Slack', 'approver', '')`); err == nil {
			t.Fatal("empty slack_user_id accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvers (display_name, role, ticket_user_id) VALUES ('Empty Ticket', 'approver', '')`); err == nil {
			t.Fatal("empty ticket_user_id accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}

		slack := "U-DUP"
		m4CreateApprover(t, d, "First", &slack, nil)
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvers (display_name, role, slack_user_id) VALUES ('Second', 'approver', $1)`,
			slack); err == nil {
			t.Fatal("duplicate slack_user_id accepted")
		} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "approvers_slack_user_id_key" {
			t.Fatalf("duplicate slack constraint = %q, want approvers_slack_user_id_key", pgErr.ConstraintName)
		}

		ticket := "T-DUP"
		m4CreateApprover(t, d, "Third", nil, &ticket)
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvers (display_name, role, ticket_user_id) VALUES ('Fourth', 'approver', $1)`,
			ticket); err == nil {
			t.Fatal("duplicate ticket_user_id accepted")
		} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "approvers_ticket_user_id_key" {
			t.Fatalf("duplicate ticket constraint = %q, want approvers_ticket_user_id_key", pgErr.ConstraintName)
		}

		// NULLs are distinct: multiple approvers without external identity.
		m4CreateApprover(t, d, "No Slack A", nil, nil)
		m4CreateApprover(t, d, "No Slack B", nil, nil)
	})

	t.Run("approvals", func(t *testing.T) {
		approverID := m4CreateApprover(t, d, "Decision Maker", nil, nil)
		proposalID := m4ApprovedProposal(t, d, "m4-approval-proposal")

		insert := func(proposal, approver, channel, decision, externalRef string) error {
			_, err := d.Pool.Exec(ctx,
				`INSERT INTO approvals
				     (proposal_id, channel, external_ref, approver_id, decision, decided_at)
				 VALUES ($1::uuid, $2, NULLIF($3, ''), $4::uuid, $5, now())`,
				proposal, channel, externalRef, approver, decision)
			return err
		}

		if err := insert(proposalID, approverID, "api", "maybe", ""); err == nil {
			t.Fatal("invalid decision accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}
		if err := insert(proposalID, approverID, "", "approved", ""); err == nil {
			t.Fatal("empty channel accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}
		if err := insert(proposalID, approverID, "api", "approved", " "); err != nil {
			t.Fatalf("non-empty external_ref rejected: %v", err)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvals (proposal_id, channel, external_ref, approver_id, decision, decided_at)
			 VALUES ($1::uuid, 'api', '', $2::uuid, 'approved', now())`,
			proposalID, approverID); err == nil {
			t.Fatal("empty external_ref accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}

		// NULL proposal/approver.
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvals (proposal_id, channel, approver_id, decision, decided_at)
			 VALUES (NULL, 'api', $1::uuid, 'approved', now())`, approverID); err == nil {
			t.Fatal("NULL proposal_id accepted")
		} else {
			requirePgError(t, err, sqlStateNotNullViolation)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvals (proposal_id, channel, approver_id, decision, decided_at)
			 VALUES ($1::uuid, 'api', NULL, 'approved', now())`, proposalID); err == nil {
			t.Fatal("NULL approver_id accepted")
		} else {
			requirePgError(t, err, sqlStateNotNullViolation)
		}

		// Duplicate (proposal_id, approver_id).
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvals (proposal_id, channel, approver_id, decision, decided_at)
			 VALUES ($1::uuid, 'slack', $2::uuid, 'rejected', now())`,
			proposalID, approverID); err == nil {
			t.Fatal("duplicate (proposal_id, approver_id) accepted")
		} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "approvals_proposal_approver_key" {
			t.Fatalf("duplicate approval constraint = %q, want approvals_proposal_approver_key", pgErr.ConstraintName)
		}

		// Invalid FKs.
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvals (proposal_id, channel, approver_id, decision, decided_at)
			 VALUES ($1::uuid, 'api', $2::uuid, 'approved', now())`,
			m4MissingUUID, approverID); err == nil {
			t.Fatal("missing proposal FK accepted")
		} else {
			requirePgError(t, err, sqlStateForeignKey)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO approvals (proposal_id, channel, approver_id, decision, decided_at)
			 VALUES ($1::uuid, 'api', $2::uuid, 'approved', now())`,
			proposalID, m4MissingUUID); err == nil {
			t.Fatal("missing approver FK accepted")
		} else {
			requirePgError(t, err, sqlStateForeignKey)
		}

		// RESTRICT: the approval trail blocks deletes.
		if _, err := d.Pool.Exec(ctx,
			`DELETE FROM proposals WHERE id = $1::uuid`, proposalID); err == nil {
			t.Fatal("referenced proposal was deleted")
		} else {
			requirePgError(t, err, sqlStateForeignKey)
		}
		if _, err := d.Pool.Exec(ctx,
			`DELETE FROM approvers WHERE id = $1::uuid`, approverID); err == nil {
			t.Fatal("referenced approver was deleted")
		} else {
			requirePgError(t, err, sqlStateForeignKey)
		}
	})

	t.Run("applied_proposals", func(t *testing.T) {
		proposalID := m4ApprovedProposal(t, d, "m4-applied-proposal")
		ledgerTxnID := m4LedgerTxn(t, d, f, "m4-applied-ledger")

		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, $2::uuid, now(), 'run-1')`,
			proposalID, ledgerTxnID); err != nil {
			t.Fatalf("first applied row: %v", err)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, NULL, now(), 'run-2')`, proposalID); err == nil {
			t.Fatal("second applied row for the same proposal accepted")
		} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "applied_proposals_pkey" {
			t.Fatalf("second applied row constraint = %q, want applied_proposals_pkey", pgErr.ConstraintName)
		}

		otherProposal := m4ApprovedProposal(t, d, "m4-applied-proposal-2")
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, $2::uuid, now(), 'run-3')`,
			otherProposal, ledgerTxnID); err == nil {
			t.Fatal("second applied row for the same ledger txn accepted")
		} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "applied_proposals_ledger_txn_key" {
			t.Fatalf("duplicate ledger constraint = %q, want applied_proposals_ledger_txn_key", pgErr.ConstraintName)
		}

		// Multiple link/duplicate applied rows with NULL ledger_txn_id succeed.
		for _, key := range []string{"m4-applied-null-1", "m4-applied-null-2"} {
			p := m4ApprovedProposal(t, d, key)
			if _, err := d.Pool.Exec(ctx,
				`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
				 VALUES ($1::uuid, NULL, now(), 'run-null')`, p); err != nil {
				t.Fatalf("NULL ledger applied row %s: %v", key, err)
			}
		}

		// Invalid FKs.
		unusedLedger := m4LedgerTxn(t, d, f, "m4-applied-ledger-unused")
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, $2::uuid, now(), 'run-bad')`,
			m4MissingUUID, unusedLedger); err == nil {
			t.Fatal("missing proposal FK accepted")
		} else {
			requirePgError(t, err, sqlStateForeignKey)
		}
		badProposal := m4ApprovedProposal(t, d, "m4-applied-bad-ledger")
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, $2::uuid, now(), 'run-bad')`,
			badProposal, m4MissingUUID); err == nil {
			t.Fatal("missing ledger FK accepted")
		} else {
			requirePgError(t, err, sqlStateForeignKey)
		}

		// NOT NULL and non-empty checks.
		nullProposal := m4ApprovedProposal(t, d, "m4-applied-null-time")
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, NULL, NULL, 'run-1')`, nullProposal); err == nil {
			t.Fatal("NULL applied_at accepted")
		} else {
			requirePgError(t, err, sqlStateNotNullViolation)
		}
		nullRun := m4ApprovedProposal(t, d, "m4-applied-null-run")
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, NULL, now(), NULL)`, nullRun); err == nil {
			t.Fatal("NULL executor_run_id accepted")
		} else {
			requirePgError(t, err, sqlStateNotNullViolation)
		}
		emptyRun := m4ApprovedProposal(t, d, "m4-applied-empty-run")
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, NULL, now(), '')`, emptyRun); err == nil {
			t.Fatal("empty executor_run_id accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}
	})

	t.Run("outbox", func(t *testing.T) {
		insert := func(aggregateType, aggregateID, topic, key string, attempts int) error {
			_, err := d.Pool.Exec(ctx,
				`INSERT INTO outbox
				     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
				      created_at, attempts, next_attempt_at)
				 VALUES ($1, $2, $3, '{}'::jsonb, $4, now(), $5, now())`,
				aggregateType, aggregateID, topic, key, attempts)
			return err
		}

		if err := insert("proposal", "p-1", "approval.request", "m4-outbox-1", 0); err != nil {
			t.Fatalf("first outbox row: %v", err)
		}
		if err := insert("proposal", "p-2", "approval.request", "m4-outbox-1", 0); err == nil {
			t.Fatal("duplicate idempotency_key accepted")
		} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "outbox_idempotency_key_key" {
			t.Fatalf("duplicate outbox constraint = %q, want outbox_idempotency_key_key", pgErr.ConstraintName)
		}

		for _, tc := range []struct {
			name          string
			aggregateType string
			aggregateID   string
			topic         string
			key           string
			attempts      int
		}{
			{"empty aggregate_type", "", "p", "t", "m4-outbox-empty-type", 0},
			{"empty aggregate_id", "proposal", "", "t", "m4-outbox-empty-id", 0},
			{"empty topic", "proposal", "p", "", "m4-outbox-empty-topic", 0},
			{"empty key", "proposal", "p", "t", "", 0},
			{"negative attempts", "proposal", "p", "t", "m4-outbox-negative", -1},
		} {
			if err := insert(tc.aggregateType, tc.aggregateID, tc.topic, tc.key, tc.attempts); err == nil {
				t.Fatalf("%s accepted", tc.name)
			} else {
				requirePgError(t, err, sqlStateCheckViolation)
			}
		}

		// dead=true cannot coexist with delivered_at.
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO outbox
			     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
			      created_at, delivered_at, next_attempt_at, dead)
			 VALUES ('proposal', 'p-dead', 't', '{}'::jsonb, 'm4-outbox-dead', now(), now(), now(), true)`); err == nil {
			t.Fatal("dead=true with delivered_at accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}

		// NULL required timestamps.
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO outbox
			     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
			      created_at, next_attempt_at)
			 VALUES ('proposal', 'p-null-created', 't', '{}'::jsonb, 'm4-outbox-null-created', NULL, now())`); err == nil {
			t.Fatal("NULL created_at accepted")
		} else {
			requirePgError(t, err, sqlStateNotNullViolation)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO outbox
			     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
			      created_at, next_attempt_at)
			 VALUES ('proposal', 'p-null-next', 't', '{}'::jsonb, 'm4-outbox-null-next', now(), NULL)`); err == nil {
			t.Fatal("NULL next_attempt_at accepted")
		} else {
			requirePgError(t, err, sqlStateNotNullViolation)
		}
	})

	t.Run("policy_config_versions", func(t *testing.T) {
		validHash := strings.Repeat("a", 64)
		insert := func(hash, yaml string) error {
			_, err := d.Pool.Exec(ctx,
				`INSERT INTO policy_config_versions (hash, yaml, loaded_at)
				 VALUES ($1, $2, now())`, hash, yaml)
			return err
		}
		if err := insert(validHash, "tiers: {}\n"); err != nil {
			t.Fatalf("first policy config: %v", err)
		}
		if err := insert(validHash, "tiers: {}\n"); err == nil {
			t.Fatal("duplicate hash accepted")
		} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "policy_config_versions_pkey" {
			t.Fatalf("duplicate policy hash constraint = %q, want policy_config_versions_pkey", pgErr.ConstraintName)
		}
		for _, hash := range []string{"abc", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
			if err := insert(hash, "yaml: true\n"); err == nil {
				t.Fatalf("bad hash %q accepted", hash)
			} else {
				requirePgError(t, err, sqlStateCheckViolation)
			}
		}
		if err := insert(strings.Repeat("b", 64), ""); err == nil {
			t.Fatal("empty yaml accepted")
		} else {
			requirePgError(t, err, sqlStateCheckViolation)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO policy_config_versions (hash, yaml, loaded_at)
			 VALUES ($1, 'yaml: true', NULL)`, strings.Repeat("c", 64)); err == nil {
			t.Fatal("NULL loaded_at accepted")
		} else {
			requirePgError(t, err, sqlStateNotNullViolation)
		}
	})
}

// covers: FIN-23
// TestM401AppliedProposalsOnce proves the exactly-once application record as
// the real recon_executor login: one applied row per proposal, one row per
// non-null ledger transaction, and many NULL-ledger link/duplicate rows.
func TestM401AppliedProposalsOnce(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)
	executor := openRolePool(t, d, db.RoleExecutor)

	proposal1 := m4ApprovedProposal(t, d, "m4-once-1")
	proposal2 := m4ApprovedProposal(t, d, "m4-once-2")
	ledger1 := m4LedgerTxn(t, d, f, "m4-once-ledger-1")
	ledger2 := m4LedgerTxn(t, d, f, "m4-once-ledger-2")

	insert := func(proposalID string, ledgerTxnID *string) error {
		_, err := executor.Exec(ctx,
			`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, $2::uuid, now(), 'm4-once-run')`,
			proposalID, ledgerTxnID)
		return err
	}

	if err := insert(proposal1, &ledger1); err != nil {
		t.Fatalf("first applied row as executor: %v", err)
	}
	if err := insert(proposal1, &ledger2); err == nil {
		t.Fatal("second applied row for the same proposal accepted")
	} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "applied_proposals_pkey" {
		t.Fatalf("same-proposal constraint = %q, want applied_proposals_pkey", pgErr.ConstraintName)
	}
	if err := insert(proposal2, &ledger1); err == nil {
		t.Fatal("second applied row for the same ledger txn accepted")
	} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "applied_proposals_ledger_txn_key" {
		t.Fatalf("same-ledger constraint = %q, want applied_proposals_ledger_txn_key", pgErr.ConstraintName)
	}

	for _, key := range []string{"m4-once-null-1", "m4-once-null-2"} {
		p := m4ApprovedProposal(t, d, key)
		if err := insert(p, nil); err != nil {
			t.Fatalf("NULL-ledger applied row %s: %v", key, err)
		}
	}

	if err := insert(m4MissingUUID, &ledger2); err == nil {
		t.Fatal("applied row for a missing proposal accepted")
	} else {
		requirePgError(t, err, sqlStateForeignKey)
	}
	missingLedger := m4MissingUUID
	if err := insert(m4ApprovedProposal(t, d, "m4-once-missing-ledger"), &missingLedger); err == nil {
		t.Fatal("applied row for a missing ledger txn accepted")
	} else {
		requirePgError(t, err, sqlStateForeignKey)
	}
}

// TestM401ExecutorGrants proves the exact executor privilege surface through
// the real ephemeral login: proposal lock/state update, applied insert, outbox
// enqueue with ON CONFLICT inference, and the existing adjustment path.
func TestM401ExecutorGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	f := newRoleFixture(t, d)
	executor := openRolePool(t, d, db.RoleExecutor)

	proposalID := m4ApprovedProposal(t, d, "m4-executor-proposal")
	ledgerTxnID := m4LedgerTxn(t, d, f, "m4-executor-ledger")

	// SELECT ... FOR UPDATE requires SELECT plus UPDATE on at least one column.
	var locked string
	if err := executor.QueryRow(ctx,
		`SELECT id::text FROM proposals WHERE id = $1::uuid FOR UPDATE`, proposalID).Scan(&locked); err != nil {
		t.Fatalf("executor SELECT ... FOR UPDATE: %v", err)
	}
	if locked != proposalID {
		t.Fatalf("locked proposal = %q, want %q", locked, proposalID)
	}

	if _, err := executor.Exec(ctx,
		`UPDATE proposals SET status = 'applied', version = version + 1 WHERE id = $1::uuid`,
		proposalID); err != nil {
		t.Fatalf("executor status/version update: %v", err)
	}

	if _, err := executor.Exec(ctx,
		`UPDATE proposals SET payload = '{}'::jsonb WHERE id = $1::uuid`, proposalID); err == nil {
		t.Fatal("executor updated proposals.payload, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	if _, err := executor.Exec(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'executor insert', 0.5,
		         'proposed', 'state-hash', 'm4-executor-insert', now())`,
		caseID, runID); err == nil {
		t.Fatal("executor INSERT into proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := executor.Exec(ctx,
		`DELETE FROM proposals WHERE id = $1::uuid`, proposalID); err == nil {
		t.Fatal("executor DELETE from proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	// Spec AC-8: pin the exact column-level surface as the real executor login.
	// Table-level UPDATE stays false so the M2 table-level assertion is not
	// mistaken for a missing executor grant.
	var tableUpdate bool
	if err := executor.QueryRow(ctx,
		`SELECT has_table_privilege(current_user, 'public.proposals', 'UPDATE')`).
		Scan(&tableUpdate); err != nil {
		t.Fatalf("read executor table-level UPDATE on proposals: %v", err)
	}
	if tableUpdate {
		t.Fatal("executor has table-level UPDATE on proposals, want column-level only")
	}
	for _, column := range []string{"status", "version"} {
		var allowed bool
		if err := executor.QueryRow(ctx,
			`SELECT has_column_privilege(current_user, 'public.proposals', $1::text, 'UPDATE')`,
			column).Scan(&allowed); err != nil {
			t.Fatalf("read executor UPDATE on proposals.%s: %v", column, err)
		}
		if !allowed {
			t.Errorf("executor lacks UPDATE on proposals.%s, want granted", column)
		}
	}
	for _, column := range []string{"payload", "rationale", "evidence"} {
		var allowed bool
		if err := executor.QueryRow(ctx,
			`SELECT has_column_privilege(current_user, 'public.proposals', $1::text, 'UPDATE')`,
			column).Scan(&allowed); err != nil {
			t.Fatalf("read executor UPDATE on proposals.%s: %v", column, err)
		}
		if allowed {
			t.Errorf("executor has UPDATE on proposals.%s, want denied", column)
		}
	}

	// Applied row insert and second-row conflict.
	appliedProposal := m4ApprovedProposal(t, d, "m4-executor-applied")
	secondLedger := m4LedgerTxn(t, d, f, "m4-executor-ledger-2")
	if _, err := executor.Exec(ctx,
		`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, $2::uuid, now(), 'executor-run')`,
		appliedProposal, ledgerTxnID); err != nil {
		t.Fatalf("executor applied insert: %v", err)
	}
	if _, err := executor.Exec(ctx,
		`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, $2::uuid, now(), 'executor-run-2')`,
		appliedProposal, secondLedger); err == nil {
		t.Fatal("executor second applied row accepted")
	} else if pgErr := requirePgError(t, err, sqlStateUniqueViolation); pgErr.ConstraintName != "applied_proposals_pkey" {
		t.Fatalf("second applied constraint = %q, want applied_proposals_pkey", pgErr.ConstraintName)
	}

	// Outbox enqueue with conflict inference. The executor has INSERT plus
	// column-level SELECT(idempotency_key); RETURNING and SELECT * must fail.
	outboxKey := "m4-executor-outbox"
	enqueue := func() (pgconn.CommandTag, error) {
		return executor.Exec(ctx,
			`INSERT INTO outbox
			     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
			      created_at, next_attempt_at)
			 VALUES ('proposal', $1, 'approval.request', '{}'::jsonb, $2, now(), now())
			 ON CONFLICT (idempotency_key) DO NOTHING`, proposalID, outboxKey)
	}
	tag, err := enqueue()
	if err != nil {
		t.Fatalf("executor outbox enqueue: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("first outbox enqueue rows affected = %d, want 1", tag.RowsAffected())
	}
	tag, err = enqueue()
	if err != nil {
		t.Fatalf("duplicate outbox enqueue: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("duplicate outbox enqueue rows affected = %d, want 0", tag.RowsAffected())
	}

	if _, err := executor.Exec(ctx, `SELECT * FROM outbox`); err == nil {
		t.Fatal("executor SELECT * FROM outbox succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := executor.Exec(ctx,
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES ('proposal', 'returning', 'approval.request', '{}'::jsonb,
		         'm4-executor-returning', now(), now())
		 RETURNING id`); err == nil {
		t.Fatal("executor INSERT ... RETURNING id succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := executor.Exec(ctx,
		`SELECT idempotency_key FROM outbox WHERE idempotency_key = $1`, outboxKey); err != nil {
		t.Fatalf("executor SELECT idempotency_key: %v", err)
	}

	// No executor privilege on the other new tables.
	for _, table := range []string{"approvers", "approvals", "policy_config_versions"} {
		for _, statement := range []string{
			`SELECT count(*) FROM ` + table,
			`INSERT INTO ` + table + ` DEFAULT VALUES`,
		} {
			if _, err := executor.Exec(ctx, statement); err == nil {
				t.Fatalf("executor %q succeeded, want permission denied", statement)
			} else {
				requirePgError(t, err, sqlStateInsufficientPrivilege)
			}
		}
	}

	// Existing adjustment posting path still works for the executor.
	if err := postRoleTxn(ctx, executor, f, roleTxnSpec{Kind: "adjustment", Amount: 500}); err != nil {
		t.Fatalf("executor adjustment post: %v", err)
	}
}

// TestM401AppGrants proves the application role surface: approvals are
// append-only, outbox is the worker path, policy config is content-addressed,
// applied rows and approvers are read-only, and proposals stay SELECT-only.
func TestM401AppGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	approverID := m4CreateApprover(t, d, "App Approver", nil, nil)
	proposalID := m4ApprovedProposal(t, d, "m4-app-proposal")

	for _, table := range []string{"approvers", "applied_proposals", "policy_config_versions", "proposals"} {
		if _, err := app.Exec(ctx, `SELECT count(*) FROM `+table); err != nil {
			t.Fatalf("app SELECT %s: %v", table, err)
		}
	}

	var approvalID string
	if err := app.QueryRow(ctx,
		`INSERT INTO approvals (proposal_id, channel, approver_id, decision, decided_at)
		 VALUES ($1::uuid, 'api', $2::uuid, 'approved', now())
		 RETURNING id::text`, proposalID, approverID).Scan(&approvalID); err != nil {
		t.Fatalf("app insert approval: %v", err)
	}
	if _, err := app.Exec(ctx,
		`UPDATE approvals SET decision = 'rejected' WHERE id = $1::uuid`, approvalID); err == nil {
		t.Fatal("app UPDATE approvals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := app.Exec(ctx,
		`DELETE FROM approvals WHERE id = $1::uuid`, approvalID); err == nil {
		t.Fatal("app DELETE approvals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	if _, err := app.Exec(ctx,
		`INSERT INTO applied_proposals (proposal_id, ledger_txn_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, NULL, now(), 'app-run')`, proposalID); err == nil {
		t.Fatal("app INSERT applied_proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	if _, err := app.Exec(ctx,
		`INSERT INTO approvers (display_name, role) VALUES ('App Written', 'approver')`); err == nil {
		t.Fatal("app INSERT approvers succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	policyHash := strings.Repeat("d", 64)
	if _, err := app.Exec(ctx,
		`INSERT INTO policy_config_versions (hash, yaml, loaded_at) VALUES ($1, 'tiers: {}', now())`,
		policyHash); err != nil {
		t.Fatalf("app insert policy config: %v", err)
	}
	var policyYAML string
	if err := app.QueryRow(ctx,
		`SELECT yaml FROM policy_config_versions WHERE hash = $1`, policyHash).Scan(&policyYAML); err != nil {
		t.Fatalf("app select policy config: %v", err)
	}
	if policyYAML != "tiers: {}" {
		t.Fatalf("policy yaml = %q", policyYAML)
	}
	if _, err := app.Exec(ctx,
		`UPDATE policy_config_versions SET yaml = 'changed' WHERE hash = $1`, policyHash); err == nil {
		t.Fatal("app UPDATE policy config succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
	if _, err := app.Exec(ctx,
		`DELETE FROM policy_config_versions WHERE hash = $1`, policyHash); err == nil {
		t.Fatal("app DELETE policy config succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	outboxKey := "m4-app-outbox"
	var outboxID string
	if err := app.QueryRow(ctx,
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES ('proposal', $1, 'approval.request', '{}'::jsonb, $2, now(), now())
		 RETURNING id::text`, proposalID, outboxKey).Scan(&outboxID); err != nil {
		t.Fatalf("app insert outbox: %v", err)
	}
	if _, err := app.Exec(ctx,
		`UPDATE outbox
		    SET delivered_at = now(), attempts = attempts + 1, next_attempt_at = now(), dead = false
		  WHERE id = $1::uuid`, outboxID); err != nil {
		t.Fatalf("app update outbox delivery columns: %v", err)
	}
	if _, err := app.Exec(ctx,
		`UPDATE outbox SET delivered_at = NULL, dead = true WHERE id = $1::uuid`, outboxID); err != nil {
		t.Fatalf("app mark outbox dead: %v", err)
	}
	if _, err := app.Exec(ctx,
		`DELETE FROM outbox WHERE id = $1::uuid`, outboxID); err == nil {
		t.Fatal("app DELETE outbox succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}

	if _, err := app.Exec(ctx,
		`UPDATE proposals SET status = 'rejected' WHERE id = $1::uuid`, proposalID); err == nil {
		t.Fatal("app UPDATE proposals succeeded, want permission denied")
	} else {
		requirePgError(t, err, sqlStateInsufficientPrivilege)
	}
}

// TestM401AgentAuditNoNewGrants proves the agent and audit roles gain nothing
// on any new table, at table or column level, and that PUBLIC has no grants.
func TestM401AgentAuditNoNewGrants(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	for _, role := range []string{db.RoleAgentRO, db.RoleAuditRO} {
		t.Run(role, func(t *testing.T) {
			for _, table := range m4Tables {
				for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
					var allowed bool
					if err := d.Pool.QueryRow(ctx,
						`SELECT has_table_privilege($1::text, $2::text, $3::text)`,
						role, table, privilege).Scan(&allowed); err != nil {
						t.Fatalf("read %s %s %s privilege: %v", role, table, privilege, err)
					}
					if allowed {
						t.Fatalf("%s has %s on %s, want none", role, privilege, table)
					}
				}
			}

			for _, column := range []struct{ table, name string }{
				{"outbox", "idempotency_key"},
				{"proposals", "status"},
				{"proposals", "version"},
			} {
				var allowed bool
				if err := d.Pool.QueryRow(ctx,
					`SELECT has_column_privilege($1::text, $2::text, $3::text, 'SELECT')`,
					role, column.table, column.name).Scan(&allowed); err != nil {
					t.Fatalf("read %s %s.%s column privilege: %v", role, column.table, column.name, err)
				}
				if allowed {
					t.Fatalf("%s can SELECT %s.%s, want none", role, column.table, column.name)
				}
			}

			// Representative real login: every new table read is denied.
			rolePool := openRolePool(t, d, role)
			for _, table := range m4Tables {
				if _, err := rolePool.Exec(ctx, `SELECT count(*) FROM `+table); err == nil {
					t.Fatalf("%s read %s, want permission denied", role, table)
				} else {
					requirePgError(t, err, sqlStateInsufficientPrivilege)
				}
			}
		})
	}

	var tableGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND grantee = 'PUBLIC'
		    AND table_name = ANY($1::text[])`, m4Tables).Scan(&tableGrants); err != nil {
		t.Fatalf("read PUBLIC table grants: %v", err)
	}
	if tableGrants != 0 {
		t.Fatalf("PUBLIC holds %d new-table grants, want 0", tableGrants)
	}
	var columnGrants int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_column_grants
		  WHERE table_schema = 'public' AND grantee = 'PUBLIC'
		    AND table_name = ANY($1::text[])`, m4Tables).Scan(&columnGrants); err != nil {
		t.Fatalf("read PUBLIC column grants: %v", err)
	}
	if columnGrants != 0 {
		t.Fatalf("PUBLIC holds %d new-table column grants, want 0", columnGrants)
	}
}

// covers: FIN-22
// TestM401OutboxStructural proves the structural prerequisite for
// same-transaction outbox writes: the full table, the due-claim partial index,
// and the executor's ability to update a proposal state and enqueue an outbox
// row in one transaction. At-least-once delivery semantics are M4-07; this
// test only proves the schema and insert capability.
func TestM401OutboxStructural(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	executor := openRolePool(t, d, db.RoleExecutor)

	proposalID := m4ApprovedProposal(t, d, "m4-outbox-structural")

	var indexDef string
	if err := d.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND indexname = 'outbox_undelivered_idx'`).
		Scan(&indexDef); err != nil {
		t.Fatalf("read outbox_undelivered_idx: %v", err)
	}
	for _, want := range []string{"next_attempt_at", "delivered_at IS NULL"} {
		if !strings.Contains(indexDef, want) {
			t.Fatalf("outbox_undelivered_idx = %q, missing %q", indexDef, want)
		}
	}

	outboxKey := "m4-outbox-structural-key"
	if err := db.WithTx(ctx, executor, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE proposals SET status = 'applied', version = version + 1 WHERE id = $1::uuid`,
			proposalID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO outbox
			     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
			      created_at, next_attempt_at)
			 VALUES ('proposal', $1, 'approval.request', '{}'::jsonb, $2, now(), now())`,
			proposalID, outboxKey)
		return err
	}); err != nil {
		t.Fatalf("executor same-transaction apply + outbox: %v", err)
	}

	var status string
	if err := d.Pool.QueryRow(ctx,
		`SELECT status FROM proposals WHERE id = $1::uuid`, proposalID).Scan(&status); err != nil {
		t.Fatalf("read applied proposal: %v", err)
	}
	if status != "applied" {
		t.Fatalf("proposal status = %q, want applied", status)
	}
	var count int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`, outboxKey).Scan(&count); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("outbox rows = %d, want 1", count)
	}
}

func m4TableColumns(t *testing.T, d *testdb.DB, table string) map[string]m4ColumnSpec {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT column_name, data_type, is_nullable, COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("read %s columns: %v", table, err)
	}
	defer rows.Close()
	columns := map[string]m4ColumnSpec{}
	for rows.Next() {
		var name string
		var spec m4ColumnSpec
		if err := rows.Scan(&name, &spec.dataType, &spec.nullable, &spec.def); err != nil {
			t.Fatalf("scan %s column: %v", table, err)
		}
		columns[name] = spec
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s columns: %v", table, err)
	}
	return columns
}

func m4ColumnNames(columns map[string]m4ColumnSpec) []string {
	names := make([]string, 0, len(columns))
	for name := range columns {
		names = append(names, name)
	}
	return names
}

func m4TableConstraints(t *testing.T, d *testdb.DB, table string) map[string]m4Constraint {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT conname, contype::text, pg_get_constraintdef(oid)
		   FROM pg_constraint
		  WHERE conrelid = to_regclass('public.' || $1)`, table)
	if err != nil {
		t.Fatalf("read %s constraints: %v", table, err)
	}
	defer rows.Close()
	constraints := map[string]m4Constraint{}
	for rows.Next() {
		var name string
		var c m4Constraint
		if err := rows.Scan(&name, &c.contype, &c.def); err != nil {
			t.Fatalf("scan %s constraint: %v", table, err)
		}
		constraints[name] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s constraints: %v", table, err)
	}
	return constraints
}

func m4RequireConstraint(t *testing.T, constraints map[string]m4Constraint, name, wantType string, fragments ...string) {
	t.Helper()
	c, ok := constraints[name]
	if !ok {
		t.Fatalf("constraint %s is missing (have %v)", name, constraints)
	}
	if c.contype != wantType {
		t.Fatalf("constraint %s contype = %q, want %q", name, c.contype, wantType)
	}
	for _, fragment := range fragments {
		if !strings.Contains(c.def, fragment) {
			t.Fatalf("constraint %s definition %q does not contain %q", name, c.def, fragment)
		}
	}
}

func m4RequireCheck(t *testing.T, d *testdb.DB, table string, fragments ...string) {
	t.Helper()
	for _, c := range m4TableConstraints(t, d, table) {
		if c.contype != "c" {
			continue
		}
		ok := true
		for _, fragment := range fragments {
			if !strings.Contains(c.def, fragment) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("%s has no check constraint containing %v", table, fragments)
}

func m4ForeignKey(t *testing.T, d *testdb.DB, table, name string) (refTable, deleteAction string) {
	t.Helper()
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT confrelid::regclass::text, confdeltype::text
		   FROM pg_constraint
		  WHERE conrelid = to_regclass('public.' || $1)
		    AND conname = $2 AND contype = 'f'`, table, name).Scan(&refTable, &deleteAction); err != nil {
		t.Fatalf("read %s.%s foreign key: %v", table, name, err)
	}
	return refTable, deleteAction
}

func m4CreateApprover(t *testing.T, d *testdb.DB, displayName string, slack, ticket *string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO approvers (display_name, role, slack_user_id, ticket_user_id)
		 VALUES ($1, 'approver', $2, $3)
		 RETURNING id::text`, displayName, slack, ticket).Scan(&id); err != nil {
		t.Fatalf("create approver %q: %v", displayName, err)
	}
	return id
}

func m4ApprovedProposal(t *testing.T, d *testdb.DB, key string) string {
	t.Helper()
	caseID := m2CreateCase(t, d)
	runID := m2CreateAgentRun(t, d, caseID)
	return m2InsertProposal(t, d.Pool, caseID, runID, key, "approved")
}

func m4LedgerTxn(t *testing.T, d *testdb.DB, f roleFixture, key string) string {
	t.Helper()
	var id string
	err := db.WithTx(context.Background(), d.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = insertRoleTxn(ctx, tx, f, roleTxnSpec{Key: key, Amount: 100})
		return err
	})
	if err != nil {
		t.Fatalf("insert ledger txn %q: %v", key, err)
	}
	return id
}
