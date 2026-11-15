// covers: FIN-25

package fault_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/fault"
)

// TestFaultFailpointConstants pins the ten executor failpoint names from the
// M4-08 contract; a mapped value also collapses duplicate spellings, so the
// length check proves all names stay distinct.
func TestFaultFailpointConstants(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		fault.ExecutorAfterProposalLock:  "executor.after_proposal_lock",
		fault.ExecutorAfterRevalidation:  "executor.after_revalidation",
		fault.ExecutorAfterLedgerInsert:  "executor.after_ledger_insert",
		fault.ExecutorAfterAppliedInsert: "executor.after_applied_insert",
		fault.ExecutorAfterBreakResolve:  "executor.after_break_resolve",
		fault.ExecutorAfterCaseResolve:   "executor.after_case_resolve",
		fault.ExecutorAfterTransition:    "executor.after_transition",
		fault.ExecutorAfterAudit:         "executor.after_audit",
		fault.ExecutorAfterOutboxInsert:  "executor.after_outbox_insert",
		fault.ExecutorBeforeCommit:       "executor.before_commit",
	}
	if len(names) != 10 {
		t.Fatalf("got %d distinct failpoints, want 10", len(names))
	}
	for got, want := range names {
		if got != want {
			t.Errorf("failpoint = %q, want %q", got, want)
		}
	}
}
