//go:build integration

// covers: FIN-23, FIN-24, FIN-25

package invariants_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/invariants"
)

func checkWorld(t *testing.T, w *world) invariants.Report {
	t.Helper()
	report, err := invariants.Check(context.Background(), w.db.Pool)
	if err != nil {
		t.Fatalf("invariants.Check: %v", err)
	}
	return report
}

// TestCheckValidAppliedWorld builds a real applied adjustment and link through
// the executor and requires no problems. The adjustment carries a ledger
// transaction; the link deliberately does not.
func TestCheckValidAppliedWorld(t *testing.T) {
	w := newWorld(t)
	f := w.seedAppliedWorld(t)

	if got := w.appliedLedgerTxn(t, f.adjustment.proposalID); got == "" {
		t.Fatal("adjustment applied record has no ledger transaction")
	}
	if got := w.appliedLedgerTxn(t, f.link.proposalID); got != "" {
		t.Fatalf("link applied record ledger txn = %q, want NULL", got)
	}

	report := checkWorld(t, w)
	if !report.OK() {
		t.Fatalf("valid applied world problems:\n%+v", report.Problems)
	}
}

// TestCheckValidReversalWorld posts a charge and its FIN-11 reversal through
// the real ledger service and requires no problems. A reversal is audited as
// ledger.reverse, so the checker must accept it instead of demanding
// ledger.post.
func TestCheckValidReversalWorld(t *testing.T) {
	w := newWorld(t)
	chargeID := w.postLedgerTxn(t, "invariants-reversal-charge", "2026-01-15")
	w.reverseLedgerTxn(t, chargeID)

	report := checkWorld(t, w)
	if !report.OK() {
		t.Fatalf("valid reversal world problems:\n%+v", report.Problems)
	}
}

// TestCheckDetectsMissingReversalAudit deletes only the reversal's
// ledger.reverse audit row and requires exactly the missing-audit code. The
// remaining ledger.post audit of the original charge must not satisfy the
// reversal.
func TestCheckDetectsMissingReversalAudit(t *testing.T) {
	w := newWorld(t)
	chargeID := w.postLedgerTxn(t, "invariants-reversal-charge", "2026-01-15")
	reversalID := w.reverseLedgerTxn(t, chargeID)

	if report := checkWorld(t, w); !report.OK() {
		t.Fatalf("uncorrupted reversal world problems:\n%+v", report.Problems)
	}

	w.deleteAuditRows(t, "ledger.reverse", reversalID, "")

	report := checkWorld(t, w)
	if len(report.Problems) != 1 || report.Problems[0].Code != invariants.CodeLedgerTxnMissingAudit {
		t.Fatalf("problems = %+v, want exactly one %q problem",
			report.Problems, invariants.CodeLedgerTxnMissingAudit)
	}
}

// TestCheckReadOnly proves the checker works inside a read-only transaction:
// any write statement would be rejected by PostgreSQL.
func TestCheckReadOnly(t *testing.T) {
	w := newWorld(t)
	w.seedAppliedWorld(t)

	ctx := context.Background()
	tx, err := w.db.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin read-only transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	report, err := invariants.Check(ctx, tx)
	if err != nil {
		t.Fatalf("invariants.Check in read-only transaction: %v", err)
	}
	if !report.OK() {
		t.Fatalf("valid applied world problems:\n%+v", report.Problems)
	}
}

// TestCheckDetectsCorruption corrupts one thing per subtest in a fresh
// database and requires exactly the matching stable code. Each subtest first
// proves the uncorrupted world is OK, so a failure identifies the corruption
// rather than the fixture.
func TestCheckDetectsCorruption(t *testing.T) {
	cases := []struct {
		code    string
		corrupt func(t *testing.T, w *world, f *appliedWorld)
	}{
		{
			code: invariants.CodeLedgerTxnMissingAudit,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.deleteAuditRows(t, "ledger.post", w.appliedLedgerTxn(t, f.adjustment.proposalID), "")
			},
		},
		{
			code: invariants.CodeAppliedRecordMissing,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.deleteApplied(t, f.adjustment.proposalID)
			},
		},
		{
			code: invariants.CodeAppliedRecordStatusMismatch,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.insertAppliedOn(t, f.bystander.proposalID)
			},
		},
		{
			code: invariants.CodeAppliedMissingLedger,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.setAppliedLedger(t, f.adjustment.proposalID, "")
			},
		},
		{
			code: invariants.CodeAppliedUnexpectedLedger,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				// Key the transaction to the link proposal so only the
				// type-specific NULL rule is violated, not its ownership.
				txnID := w.postLedgerTxn(t, "proposal:"+f.link.proposalID, "2026-01-16")
				w.setAppliedLedger(t, f.link.proposalID, txnID)
			},
		},
		{
			code: invariants.CodeBreakNotResolved,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.setBreakStatus(t, f.adjustment.breakID, "open")
			},
		},
		{
			code: invariants.CodeCaseNotResolved,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.setCaseStatus(t, f.adjustment.caseID, "open")
			},
		},
		{
			code: invariants.CodeProposalAppliedOutboxMissing,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.deleteOutboxRow(t, f.adjustment.proposalID)
			},
		},
		{
			code: invariants.CodeProposalAppliedOutboxDead,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.markOutboxDead(t, f.adjustment.proposalID)
			},
		},
		{
			code: invariants.CodeExecutorAppliedAuditMissing,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.deleteAuditRows(t, "executor.applied", f.adjustment.proposalID, "")
			},
		},
		{
			code: invariants.CodeTransitionAppliedAuditMissing,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.deleteAuditRows(t, "proposal.transition", f.adjustment.proposalID, "applied")
			},
		},
		{
			code: invariants.CodeBreakResolutionAuditMissing,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.deleteAuditRows(t, "break.resolve", f.adjustment.breakID, "")
			},
		},
		{
			code: invariants.CodeCaseResolutionAuditMissing,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				w.deleteAuditRows(t, "case.resolve", f.adjustment.caseID, "")
			},
		},
		{
			code: invariants.CodeAppliedLedgerWithoutProposal,
			corrupt: func(t *testing.T, w *world, f *appliedWorld) {
				t.Helper()
				// A real ledger transaction, but not the one posted for this
				// proposal (its idempotency key names no proposal).
				txnID := w.insertLedgerTxn(t, "2026-01-16")
				w.setAppliedLedger(t, f.adjustment.proposalID, txnID)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			w := newWorld(t)
			f := w.seedAppliedWorld(t)
			if report := checkWorld(t, w); !report.OK() {
				t.Fatalf("uncorrupted world problems:\n%+v", report.Problems)
			}

			tc.corrupt(t, w, f)

			report := checkWorld(t, w)
			if len(report.Problems) != 1 || report.Problems[0].Code != tc.code {
				t.Fatalf("problems = %+v, want exactly one %q problem", report.Problems, tc.code)
			}
		})
	}
}
