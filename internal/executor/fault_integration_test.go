//go:build integration && faultinject

// covers: FIN-20, FIN-21, FIN-23, FIN-24, FIN-25

package executor_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/fault"
	"github.com/TamasGorgics/recon-agent/internal/invariants"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
)

const (
	// faultSubprocessMarker selects the helper test in the re-executed binary.
	faultSubprocessMarker = "RECON_FAULT_SUBPROCESS"
	// faultChildDSNEnv, faultChildProposalEnv, and faultChildNowEnv pass the
	// isolated database and fixture into the helper.
	faultChildDSNEnv      = "RECON_TEST_DSN"
	faultChildProposalEnv = "RECON_TEST_PROPOSAL_ID"
	faultChildNowEnv      = "RECON_TEST_NOW"
	// faultChildExitCode is the os.Exit status the faultinject package uses.
	// The subprocess test asserts this exact value.
	faultChildExitCode = 86
	// faultChildTimeout bounds one crash subprocess run so a hang cannot stall
	// the parent test suite.
	faultChildTimeout = 2 * time.Minute
)

// TestExecutorFaultSubprocess is the re-executed helper. It constructs the
// same executor dependency set as the world fixture and calls Apply for the
// proposal named by the RECON_TEST_* environment. A matching failpoint exits
// the process with status 86 before this function can return.
func TestExecutorFaultSubprocess(t *testing.T) {
	if os.Getenv(faultSubprocessMarker) != "1" {
		t.Skip("subprocess helper: set RECON_FAULT_SUBPROCESS=1")
	}

	dsn := os.Getenv(faultChildDSNEnv)
	proposalID := os.Getenv(faultChildProposalEnv)
	nowRaw := os.Getenv(faultChildNowEnv)
	if dsn == "" || proposalID == "" || nowRaw == "" {
		t.Fatalf("%s, %s, and %s are required", faultChildDSNEnv, faultChildProposalEnv, faultChildNowEnv)
	}
	now, err := time.Parse(time.RFC3339, nowRaw)
	if err != nil {
		t.Fatalf("parse %s %q: %v", faultChildNowEnv, nowRaw, err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open executor pool: %v", err)
	}
	defer pool.Close()

	clk := clock.NewFake(now.UTC())
	ledgerSvc := ledger.NewService(clk, config.LedgerConfig{
		AdjustmentAccounts: []string{"fees_expense", "suspense", "fx_gain_loss", "rounding"},
	})
	stateSvc, err := state.NewService(pool, clk)
	if err != nil {
		t.Fatalf("new state service: %v", err)
	}
	svc, err := executor.NewService(executor.Options{
		Pool:          pool,
		Ledger:        ledgerSvc,
		State:         stateSvc,
		Clock:         clk,
		ExecutorRunID: "executor:fault:child",
		BatchSize:     4,
		PollInterval:  time.Millisecond,
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new executor service: %v", err)
	}

	result, err := svc.Apply(ctx, proposalID)
	if err != nil {
		t.Fatalf("child Apply returned a non-fault error: %v", err)
	}
	t.Fatalf("child Apply returned without hitting a failpoint: %+v", result)
}

// TestExecutorFaultPoints kills the executor at every failpoint and asserts
// the rolled-back transaction left no partial state, then retries and asserts
// exactly-once apply. An adjustment writes a ledger transaction and resolves a
// break and case, so every one of the ten points fires for this proposal type;
// a non-firing point makes the child exit without 86 and fails the subtest.
func TestExecutorFaultPoints(t *testing.T) {
	// The parent must never trip a failpoint itself; only the child env sets it.
	t.Setenv("RECON_FAULT", "")

	points := []string{
		fault.ExecutorAfterProposalLock,
		fault.ExecutorAfterRevalidation,
		fault.ExecutorAfterLedgerInsert,
		fault.ExecutorAfterAppliedInsert,
		fault.ExecutorAfterBreakResolve,
		fault.ExecutorAfterCaseResolve,
		fault.ExecutorAfterTransition,
		fault.ExecutorAfterAudit,
		fault.ExecutorAfterOutboxInsert,
		fault.ExecutorBeforeCommit,
	}

	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			w := newWorld(t)
			fixture := w.newCase(t, caseSpec{})
			ctx := context.Background()

			childCtx, cancel := context.WithTimeout(ctx, faultChildTimeout)
			defer cancel()
			cmd := exec.CommandContext(childCtx, os.Args[0],
				"-test.run=^TestExecutorFaultSubprocess$", "-test.v")
			cmd.Env = faultChildEnvironment(
				w.db.DSNFor(db.RoleExecutor), fixture.proposalID, testNow, point)
			output, err := cmd.CombinedOutput()
			if childCtx.Err() != nil {
				t.Fatalf("subprocess at %s timed out after %s: %v; output:\n%s",
					point, faultChildTimeout, err, output)
			}
			if err == nil {
				t.Fatalf("subprocess at %s exited 0, want %d; output:\n%s",
					point, faultChildExitCode, output)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("subprocess at %s error = %v, want *exec.ExitError; output:\n%s",
					point, err, output)
			}
			if code := exitErr.ExitCode(); code != faultChildExitCode {
				t.Fatalf("subprocess at %s exit code = %d, want %d; output:\n%s",
					point, code, faultChildExitCode, output)
			}

			assertFaultCrashState(t, w, fixture)
			assertFaultInvariantsOK(t, w, "after crash at "+point)

			result, err := w.service.Apply(ctx, fixture.proposalID)
			if err != nil {
				t.Fatalf("retry Apply after crash at %s: %v", point, err)
			}
			if result.Status != executor.StatusApplied || result.Replayed {
				t.Fatalf("retry result = %+v, want applied/not replayed", result)
			}
			w.assertAppliedOnce(t, fixture, result.LedgerTxnID)
			assertFaultAppliedOnce(t, w, fixture, result.LedgerTxnID)
			assertFaultInvariantsOK(t, w, "after retry at "+point)

			replay, err := w.service.Apply(ctx, fixture.proposalID)
			if err != nil {
				t.Fatalf("second Apply after retry at %s: %v", point, err)
			}
			if replay.Status != executor.StatusReplayed || !replay.Replayed {
				t.Fatalf("second Apply result = %+v, want replayed", replay)
			}
			if replay.LedgerTxnID != result.LedgerTxnID {
				t.Fatalf("second Apply ledger txn = %q, want %q", replay.LedgerTxnID, result.LedgerTxnID)
			}
			assertFaultAppliedOnce(t, w, fixture, result.LedgerTxnID)
			assertFaultInvariantsOK(t, w, "after replay at "+point)
		})
	}
}

// faultChildEnvironment inherits the parent environment except for the fault
// and child-fixture variables, then sets the values for one crash run.
func faultChildEnvironment(dsn, proposalID, now, point string) []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "RECON_FAULT", faultSubprocessMarker,
			faultChildDSNEnv, faultChildProposalEnv, faultChildNowEnv:
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"RECON_FAULT="+point+":exit",
		faultSubprocessMarker+"=1",
		faultChildDSNEnv+"="+dsn,
		faultChildProposalEnv+"="+proposalID,
		faultChildNowEnv+"="+now,
	)
}

// assertFaultCrashState asserts the crash rolled back before any committed
// side effect: approved proposal, unresolved break/case, and no ledger,
// applied, outbox, or audit rows.
func assertFaultCrashState(t *testing.T, w *world, fixture *caseFixture) {
	t.Helper()
	w.assertNoApplySideEffects(t, fixture)

	var (
		status  string
		version int
	)
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT status, version FROM proposals WHERE id = $1::uuid`,
		fixture.proposalID).Scan(&status, &version); err != nil {
		t.Fatalf("read proposal after crash: %v", err)
	}
	if status != "approved" {
		t.Errorf("proposal status after crash = %s, want approved", status)
	}
	if version != 0 {
		t.Errorf("proposal version after crash = %d, want 0", version)
	}

	if got := w.count(t,
		`SELECT count(*) FROM ledger_entries le
		   JOIN ledger_txns lt ON lt.id = le.txn_id
		  WHERE lt.idempotency_key = $1`,
		"proposal:"+fixture.proposalID); got != 0 {
		t.Errorf("ledger entries for proposal after crash = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'proposal.transition' AND entity_id = $1`,
		fixture.proposalID); got != 0 {
		t.Errorf("proposal.transition audits after crash = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'break.resolve' AND entity_id = $1`,
		fixture.breakID); got != 0 {
		t.Errorf("break.resolve audits after crash = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'case.resolve' AND entity_id = $1`,
		fixture.caseID); got != 0 {
		t.Errorf("case.resolve audits after crash = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'case.transition' AND entity_id = $1`,
		fixture.caseID); got != 0 {
		t.Errorf("case.transition audits after crash = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM cases
		  WHERE id = $1::uuid AND resolution_type IS NULL AND resolved_at IS NULL`,
		fixture.caseID); got != 1 {
		t.Error("case is resolved after crash")
	}
}

// assertFaultInvariantsOK fails when the reusable checker reports a problem or
// a database error.
func assertFaultInvariantsOK(t *testing.T, w *world, when string) {
	t.Helper()
	report, err := invariants.Check(context.Background(), w.db.Pool)
	if err != nil {
		t.Fatalf("invariants.Check %s: %v", when, err)
	}
	if !report.OK() {
		t.Fatalf("invariants.Check %s: %+v", when, report.Problems)
	}
}

// assertFaultAppliedOnce asserts the retried apply wrote exactly one ledger
// transaction (with its two lines), one applied record, and one outbox row.
func assertFaultAppliedOnce(t *testing.T, w *world, fixture *caseFixture, ledgerTxnID string) {
	t.Helper()
	if ledgerTxnID == "" {
		t.Fatal("applied ledger txn id is empty")
	}
	if got := w.count(t,
		`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`,
		"proposal:"+fixture.proposalID); got != 1 {
		t.Errorf("ledger transactions for proposal = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM ledger_entries WHERE txn_id = $1::uuid`,
		ledgerTxnID); got != 2 {
		t.Errorf("ledger entries for proposal = %d, want 2", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`,
		fixture.proposalID); got != 1 {
		t.Errorf("applied_proposals rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE ledger_txn_id = $1::uuid`,
		ledgerTxnID); got != 1 {
		t.Errorf("applied rows referencing the ledger transaction = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+fixture.proposalID); got != 1 {
		t.Errorf("proposal.applied outbox rows = %d, want 1", got)
	}
}
