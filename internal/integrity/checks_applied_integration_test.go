//go:build integration

// covers: FIN-52
package integrity_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/integrity"
	"github.com/TamasGorgics/recon-agent/internal/invariants"
)

// TestAppliedProposalTrail proves internal/invariants.Check is called unchanged
// and its stable codes are mapped one-for-one under the applied check.
func TestAppliedProposalTrail(t *testing.T) {
	t.Run("clean applied adjustment passes", func(t *testing.T) {
		f := newFixture(t)
		result := f.mustRun(t, integrity.Options{})
		check := checkByName(t, result.Report, integrity.CheckAppliedProposalTrail)
		if check.Status != integrity.StatusPassed || check.Checked != 1 || check.ProblemCount != 0 {
			t.Fatalf("applied check status/checked/problems = %q/%d/%d, want passed/1/0",
				check.Status, check.Checked, check.ProblemCount)
		}
	})

	t.Run("missing ledger maps applied_missing_ledger", func(t *testing.T) {
		f := newFixture(t)
		f.clearAppliedLedger(t)
		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckAppliedProposalTrail,
			invariants.CodeAppliedMissingLedger)
		if problem.ID != f.proposalID {
			t.Fatalf("applied_missing_ledger id = %q, want proposal %q", problem.ID, f.proposalID)
		}
		if checkByName(t, result.Report, integrity.CheckAppliedProposalTrail).Checked != 1 {
			t.Fatal("applied check checked != 1")
		}
	})

	t.Run("missing executor audit maps executor_applied_audit_missing", func(t *testing.T) {
		f := newFixture(t)
		f.deleteExecutorAudit(t)
		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckAppliedProposalTrail,
			invariants.CodeExecutorAppliedAuditMissing)
		if problem.ID != f.proposalID {
			t.Fatalf("executor_applied_audit_missing id = %q, want proposal %q", problem.ID, f.proposalID)
		}
		// The same deleted audit row legitimately fails the audit chain too.
		if auditCheck := checkByName(t, result.Report, integrity.CheckAuditChain); auditCheck.Status != integrity.StatusFailed {
			t.Fatal("audit_chain should also report the deleted audit row")
		}
	})

	t.Run("invariant code strings are unchanged", func(t *testing.T) {
		// The mapping is one-for-one with the frozen internal/invariants
		// contract; a rename or renumbering here would silently change the
		// FIN-52 alert contract.
		if invariants.CodeAppliedMissingLedger != "applied_missing_ledger" {
			t.Fatalf("applied ledger code = %q, want applied_missing_ledger",
				invariants.CodeAppliedMissingLedger)
		}
		if invariants.CodeExecutorAppliedAuditMissing != "executor_applied_audit_missing" {
			t.Fatalf("executor audit code = %q, want executor_applied_audit_missing",
				invariants.CodeExecutorAppliedAuditMissing)
		}
	})
}
