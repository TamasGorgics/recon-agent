//go:build integration

// covers: FIN-20, FIN-21, FIN-23

package executor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// installTrigger creates a per-test BEFORE trigger function and trigger. The
// returned cleanup drops both so a test can retry the apply on a clean schema.
func (w *world) installTrigger(t *testing.T, triggerName, table, event, body string) func() {
	t.Helper()
	ctx := context.Background()
	function := triggerName + "_fn"
	if _, err := w.db.Pool.Exec(ctx,
		`CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   `+body+`
		   RETURN NEW;
		 END;
		 $$`); err != nil {
		t.Fatalf("create trigger function %s: %v", function, err)
	}
	if _, err := w.db.Pool.Exec(ctx,
		`CREATE TRIGGER `+triggerName+` BEFORE `+event+` ON `+table+
			` FOR EACH ROW EXECUTE FUNCTION `+function+`()`); err != nil {
		t.Fatalf("create trigger %s: %v", triggerName, err)
	}
	return func() {
		ctx := context.Background()
		if _, err := w.db.Pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+triggerName+` ON `+table); err != nil {
			t.Errorf("drop trigger %s: %v", triggerName, err)
		}
		if _, err := w.db.Pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+function+`()`); err != nil {
			t.Errorf("drop trigger function %s: %v", function, err)
		}
	}
}

// TestApplyRollbackOnAuditFailure injects a failure into the executor.applied
// audit append and proves the whole apply transaction rolled back: no ledger
// transaction, applied row, outbox row, or break/case resolution survives.
func TestApplyRollbackOnAuditFailure(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	drop := w.installTrigger(t, "test_fail_executor_audit", "audit_log", "INSERT",
		`IF NEW.action = 'executor.applied' THEN RAISE EXCEPTION 'injected audit failure'; END IF;`)

	if _, err := w.service.Apply(context.Background(), fixture.proposalID); err == nil {
		t.Fatal("Apply succeeded despite the injected audit failure")
	}
	w.assertNoApplySideEffects(t, fixture)
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "approved" {
		t.Errorf("proposal status = %s, want approved after rollback", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'proposal.transition' AND entity_id = $1`,
		fixture.proposalID); got != 0 {
		t.Errorf("proposal.transition audit rows = %d, want 0 after rollback", got)
	}

	drop()
	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply after dropping the trigger: %v", err)
	}
	if result.Status != "applied" {
		t.Fatalf("retry result = %+v, want applied", result)
	}
}

// TestApplyRollbackOnOutboxFailure injects a failure into the outbox insert
// and proves the ledger, applied, audit, and resolution writes roll back.
func TestApplyRollbackOnOutboxFailure(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	drop := w.installTrigger(t, "test_fail_outbox", "outbox", "INSERT",
		`IF NEW.idempotency_key LIKE 'proposal.applied:%' THEN RAISE EXCEPTION 'injected outbox failure'; END IF;`)

	if _, err := w.service.Apply(context.Background(), fixture.proposalID); err == nil {
		t.Fatal("Apply succeeded despite the injected outbox failure")
	}
	w.assertNoApplySideEffects(t, fixture)
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'executor.applied' AND entity_id = $1`,
		fixture.proposalID); got != 0 {
		t.Errorf("executor.applied audit rows = %d, want 0 after rollback", got)
	}
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "approved" {
		t.Errorf("proposal status = %s, want approved after rollback", got)
	}

	drop()
	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply after dropping the trigger: %v", err)
	}
	if result.Status != "applied" {
		t.Fatalf("retry result = %+v, want applied", result)
	}
}

// TestApplyInfrastructureFailureLeavesApproved revokes the executor's SELECT on
// v_agent_cases and proves a permission error while re-loading the proposal
// state is returned to the caller and leaves the proposal approved. It must not
// be recorded as apply_failed or write any apply side effect.
func TestApplyInfrastructureFailureLeavesApproved(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})

	if _, err := w.db.Pool.Exec(ctx,
		`REVOKE SELECT ON v_agent_cases FROM `+db.RoleExecutor); err != nil {
		t.Fatalf("revoke executor SELECT on v_agent_cases: %v", err)
	}
	t.Cleanup(func() {
		if _, err := w.db.Pool.Exec(context.Background(),
			`GRANT SELECT ON v_agent_cases TO `+db.RoleExecutor); err != nil {
			t.Errorf("restore executor SELECT on v_agent_cases: %v", err)
		}
	})

	_, err := w.service.Apply(ctx, fixture.proposalID)
	if err == nil {
		t.Fatal("Apply succeeded despite the revoked SELECT; want an infrastructure error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("Apply error = %v, want SQLSTATE 42501 permission denied", err)
	}
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "approved" {
		t.Errorf("proposal status = %s, want approved (not apply_failed)", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'proposal.transition' AND entity_id = $1`,
		fixture.proposalID); got != 0 {
		t.Errorf("proposal.transition audit rows = %d, want 0", got)
	}
	w.assertNoApplySideEffects(t, fixture)
}

// TestApplyRollbackOnTransitionFailure injects a failure into the proposal
// applied update and proves no earlier write survives.
func TestApplyRollbackOnTransitionFailure(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	drop := w.installTrigger(t, "test_fail_proposal_applied", "proposals", "UPDATE",
		`IF NEW.status = 'applied' THEN RAISE EXCEPTION 'injected transition failure'; END IF;`)

	if _, err := w.service.Apply(context.Background(), fixture.proposalID); err == nil {
		t.Fatal("Apply succeeded despite the injected transition failure")
	}
	w.assertNoApplySideEffects(t, fixture)
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'proposal.transition' AND entity_id = $1`,
		fixture.proposalID); got != 0 {
		t.Errorf("proposal.transition audit rows = %d, want 0 after rollback", got)
	}

	drop()
	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply after dropping the trigger: %v", err)
	}
	if result.Status != "applied" {
		t.Fatalf("retry result = %+v, want applied", result)
	}
}
