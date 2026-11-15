package invariants_test

import (
	"context"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/invariants"
)

// TestReportOK pins the empty/non-empty report contract.
func TestReportOK(t *testing.T) {
	if !(invariants.Report{}).OK() {
		t.Error("empty report is not OK, want OK")
	}
	report := invariants.Report{Problems: []invariants.Problem{{
		Code:   invariants.CodeCaseNotResolved,
		Entity: "case",
		ID:     "00000000-0000-4000-8000-000000000000",
		Detail: "fixture",
	}}}
	if report.OK() {
		t.Error("report with one problem is OK, want not OK")
	}
}

// TestCodesStable freezes the problem codes documented in the M4-08
// specification. Renaming a constant without updating the contract fails here.
func TestCodesStable(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{invariants.CodeLedgerTxnMissingAudit, "ledger_txn_missing_audit"},
		{invariants.CodeAppliedRecordMissing, "applied_record_missing"},
		{invariants.CodeAppliedRecordStatusMismatch, "applied_record_status_mismatch"},
		{invariants.CodeAppliedMissingLedger, "applied_missing_ledger"},
		{invariants.CodeAppliedUnexpectedLedger, "applied_unexpected_ledger"},
		{invariants.CodeBreakNotResolved, "break_not_resolved"},
		{invariants.CodeCaseNotResolved, "case_not_resolved"},
		{invariants.CodeProposalAppliedOutboxMissing, "proposal_applied_outbox_missing"},
		{invariants.CodeProposalAppliedOutboxDead, "proposal_applied_outbox_dead"},
		{invariants.CodeExecutorAppliedAuditMissing, "executor_applied_audit_missing"},
		{invariants.CodeTransitionAppliedAuditMissing, "transition_applied_audit_missing"},
		{invariants.CodeBreakResolutionAuditMissing, "break_resolution_audit_missing"},
		{invariants.CodeCaseResolutionAuditMissing, "case_resolution_audit_missing"},
		{invariants.CodeAppliedLedgerWithoutProposal, "applied_ledger_without_proposal"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("code = %q, want %q", tc.got, tc.want)
		}
	}
}

// TestCheckRejectsNilQueryer proves Check reports a construction error instead
// of panicking.
func TestCheckRejectsNilQueryer(t *testing.T) {
	if _, err := invariants.Check(context.Background(), nil); err == nil {
		t.Fatal("Check(nil) returned nil error, want an error")
	}
}
