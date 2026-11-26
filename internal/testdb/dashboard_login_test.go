//go:build integration

package testdb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestDashboardLoginLazyProvisioning proves the dashboard login is
// provisioned only on request, is a member of recon_dashboards_ro, carries the
// read-only/timeout settings, can read every v_ops_*/v_eval_* view (the M5-04
// inventory plus the M5-05 eval views), and is torn down with the rest of the
// per-test logins (TestMain asserts no process-scoped role survives m.Run).
func TestDashboardLoginLazyProvisioning(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	if got := len(d.LoginNames()); got != len(db.Roles()) {
		t.Fatalf("LoginNames() before dashboard request = %d, want the four application logins", got)
	}

	dsn, err := d.DashboardsDSN(ctx)
	if err != nil {
		t.Fatalf("DashboardsDSN: %v", err)
	}
	if dsn == "" || dsn == d.DSN {
		t.Fatalf("DashboardsDSN() = %q, want an ephemeral DSN distinct from the maintenance DSN", dsn)
	}
	names := d.LoginNames()
	if len(names) != len(db.Roles())+1 {
		t.Fatalf("LoginNames() after dashboard request = %v, want one extra dashboard login", names)
	}

	// A second request reuses the same login/DSN rather than creating another.
	again, err := d.DashboardsDSN(ctx)
	if err != nil {
		t.Fatalf("second DashboardsDSN: %v", err)
	}
	if again != dsn {
		t.Fatalf("second DashboardsDSN() = %q, want the cached DSN %q", again, dsn)
	}
	if got := len(d.LoginNames()); got != len(db.Roles())+1 {
		t.Fatalf("LoginNames() after repeated dashboard request = %v, want one dashboard login", names)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect as dashboard login: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var currentUser, superuser string
	var member bool
	if err := conn.QueryRow(ctx,
		`SELECT current_user, current_setting('is_superuser'),
		        pg_has_role(current_user, $1::name, 'MEMBER')`,
		testdb.DashboardsRole).Scan(&currentUser, &superuser, &member); err != nil {
		t.Fatalf("inspect dashboard session: %v", err)
	}
	if superuser != "off" {
		t.Fatalf("dashboard login connects as a superuser %q", currentUser)
	}
	if !member {
		t.Fatalf("dashboard login %q is not a member of %s", currentUser, testdb.DashboardsRole)
	}
	if !strings.HasPrefix(currentUser, testdb.LoginRolePrefix()) {
		t.Fatalf("dashboard current_user = %q, want the ephemeral login prefix", currentUser)
	}
	var readOnly, timeout string
	if err := conn.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil {
		t.Fatalf("read default_transaction_read_only: %v", err)
	}
	if readOnly != "on" {
		t.Fatalf("default_transaction_read_only = %q, want on", readOnly)
	}
	if err := conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read statement_timeout: %v", err)
	}
	if timeout != "30s" {
		t.Fatalf("statement_timeout = %q, want 30s", timeout)
	}

	for _, view := range []string{
		"v_ops_match_rate", "v_ops_unmatched_value", "v_ops_break_aging",
		"v_ops_approval_queue", "v_ops_resolution_mix",
		"v_eval_accuracy", "v_eval_false_fix", "v_eval_cost_latency",
		"v_eval_override_rate", "v_eval_apply_failure_rate",
		"v_eval_escalations", "v_eval_escalation_rate",
	} {
		var count int64
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(&count); err != nil {
			t.Errorf("dashboard SELECT %s: %v", view, err)
		}
	}
	if _, err := conn.Exec(ctx, `SELECT count(*) FROM currencies`); err == nil {
		t.Fatal("dashboard login read the currencies base table, want 42501")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("dashboard base-table SELECT error = %v, want SQLSTATE 42501", err)
		}
	}
}
