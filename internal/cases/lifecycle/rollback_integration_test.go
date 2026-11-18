//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
)

// TestSupersedeRollbackOnOutboxFailure proves the proposal transition and its
// audit roll back with a failed approval.cancel insert. The same call succeeds
// after the grant is restored, so the transaction left no partial state.
func TestSupersedeRollbackOnOutboxFailure(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "proposed")
	ctx := context.Background()

	if _, err := f.db.Pool.Exec(ctx, `REVOKE INSERT ON outbox FROM recon_app`); err != nil {
		t.Fatalf("revoke outbox insert: %v", err)
	}
	_, err := f.supersede(ctx, supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	})
	if err == nil {
		t.Fatal("SupersedeActiveProposal succeeded with outbox INSERT revoked")
	}
	if got := f.proposal(t, oldID); got.Status != "proposed" || got.Version != 0 {
		t.Fatalf("proposal = %s/version %d after outbox failure, want proposed/0",
			got.Status, got.Version)
	}
	if got := f.caseRow(t, caseID); got.Status != "proposal_pending" || got.Version != 0 {
		t.Fatalf("case = %s/version %d after outbox failure, want unchanged", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d after outbox failure, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Fatalf("audit rows = %d after outbox failure, want 0 (proposal audit rolled back)", got)
	}

	if _, err := f.db.Pool.Exec(ctx,
		`GRANT SELECT, INSERT, UPDATE ON outbox TO recon_app`); err != nil {
		t.Fatalf("restore outbox grant: %v", err)
	}
	result, err := f.supersede(ctx, supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	})
	if err != nil {
		t.Fatalf("retry after restore: %v", err)
	}
	if !result.Superseded || !result.CancelInserted {
		t.Fatalf("retry result = %+v, want superseded with a cancel row", result)
	}
	if got := f.proposal(t, oldID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("proposal after retry = %s/version %d, want superseded/1", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 1 {
		t.Fatalf("outbox rows after retry = %d, want 1", got)
	}
	if got := len(f.audits(t, caseID)); got != 1 {
		t.Fatalf("lifecycle audits after retry = %d, want 1", got)
	}
}

// TestSupersedeRollbackOnLifecycleAuditFailure injects a failure only for the
// case.proposal_superseded audit, so the proposal authority's own audit has
// already been written when the error is raised. The whole transaction must
// still roll back.
func TestSupersedeRollbackOnLifecycleAuditFailure(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "proposed")
	ctx := context.Background()

	if _, err := f.db.Pool.Exec(ctx, `
		CREATE FUNCTION public.reject_lifecycle_audit() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.action = 'case.proposal_superseded' THEN
				RAISE EXCEPTION 'injected lifecycle audit failure';
			END IF;
			RETURN NEW;
		END
		$$`); err != nil {
		t.Fatalf("create audit failure function: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx, `
		CREATE TRIGGER reject_lifecycle_audit
		BEFORE INSERT ON audit_log
		FOR EACH ROW EXECUTE FUNCTION public.reject_lifecycle_audit()`); err != nil {
		t.Fatalf("create audit failure trigger: %v", err)
	}

	_, err := f.supersede(ctx, supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	})
	if err == nil {
		t.Fatal("SupersedeActiveProposal succeeded with the lifecycle audit rejected")
	}
	if !strings.Contains(err.Error(), "injected lifecycle audit failure") {
		t.Fatalf("error = %v, want the injected audit failure", err)
	}
	if got := f.proposal(t, oldID); got.Status != "proposed" || got.Version != 0 {
		t.Fatalf("proposal = %s/version %d after audit failure, want proposed/0",
			got.Status, got.Version)
	}
	if got := f.caseRow(t, caseID); got.Status != "proposal_pending" || got.Version != 0 {
		t.Fatalf("case = %s/version %d after audit failure, want unchanged", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d after audit failure, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
		t.Fatalf("audit rows = %d after audit failure, want 0 (proposal audit rolled back)", got)
	}

	if _, err := f.db.Pool.Exec(ctx, `DROP TRIGGER reject_lifecycle_audit ON audit_log`); err != nil {
		t.Fatalf("drop audit failure trigger: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx, `DROP FUNCTION public.reject_lifecycle_audit()`); err != nil {
		t.Fatalf("drop audit failure function: %v", err)
	}
	result, err := f.supersede(ctx, supersedeCall{
		caseID: caseID,
		actor:  "system:matcher",
		reason: lifecycle.ReasonLateMatch,
	})
	if err != nil {
		t.Fatalf("retry after dropping the failure trigger: %v", err)
	}
	if !result.Superseded || !result.CancelInserted {
		t.Fatalf("retry result = %+v, want superseded with a cancel row", result)
	}
	if got := len(f.outbox(t)); got != 1 {
		t.Fatalf("outbox rows after retry = %d, want 1", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != 2 {
		t.Fatalf("audit rows after retry = %d, want 2 (proposal + lifecycle)", got)
	}
}
