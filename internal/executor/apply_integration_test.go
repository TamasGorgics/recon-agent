//go:build integration

// covers: FIN-20, FIN-21, FIN-23

package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/executor"
)

// TestApplyAdjustment covers the happy path: one balanced adjustment
// transaction, one applied record, proposal/break/case resolution, the three
// expected audit entries, and one outbox row, all committed together.
func TestApplyAdjustment(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplied || result.Replayed {
		t.Fatalf("result = %+v, want applied/not replayed", result)
	}
	if result.ProposalID != fixture.proposalID {
		t.Fatalf("result proposal id = %q, want %q", result.ProposalID, fixture.proposalID)
	}
	if result.LedgerTxnID == "" {
		t.Fatal("result ledger txn id is empty")
	}
	w.assertAppliedOnce(t, fixture, result.LedgerTxnID)

	var (
		kind, valueDate, description, key string
	)
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT kind, value_date::text, description, idempotency_key
		   FROM ledger_txns WHERE id = $1::uuid`, result.LedgerTxnID).
		Scan(&kind, &valueDate, &description, &key); err != nil {
		t.Fatalf("read ledger txn: %v", err)
	}
	if kind != "adjustment" {
		t.Errorf("ledger kind = %s, want adjustment", kind)
	}
	if valueDate != "2026-02-03" {
		t.Errorf("value date = %s, want the break external value date 2026-02-03", valueDate)
	}
	if key != "proposal:"+fixture.proposalID {
		t.Errorf("idempotency key = %q, want proposal id key", key)
	}
	if want := "proposal " + fixture.proposalID + " adjustment"; description != want {
		t.Errorf("description = %q, want %q", description, want)
	}

	var entries, debits, credits int
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT count(*),
		        COALESCE(sum(CASE WHEN direction = 'debit' THEN amount_minor ELSE 0 END), 0),
		        COALESCE(sum(CASE WHEN direction = 'credit' THEN amount_minor ELSE 0 END), 0)
		   FROM ledger_entries WHERE txn_id = $1::uuid`, result.LedgerTxnID).
		Scan(&entries, &debits, &credits); err != nil {
		t.Fatalf("read ledger entries: %v", err)
	}
	if entries != 2 || debits != 500 || credits != 500 {
		t.Errorf("entries/debits/credits = %d/%d/%d, want 2/500/500", entries, debits, credits)
	}

	if got := w.oneString(t,
		`SELECT resolution_type FROM cases WHERE id = $1::uuid`, fixture.caseID); got != "adjustment" {
		t.Errorf("case resolution_type = %q, want adjustment", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM cases WHERE id = $1::uuid AND resolved_at IS NOT NULL`, fixture.caseID); got != 1 {
		t.Error("case resolved_at is not set")
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'break.resolve' AND entity_id = $1`, fixture.breakID); got != 1 {
		t.Errorf("break.resolve audit rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'case.resolve' AND entity_id = $1`, fixture.caseID); got != 1 {
		t.Errorf("case.resolve audit rows = %d, want 1", got)
	}

	var rawPayload []byte
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+fixture.proposalID).Scan(&rawPayload); err != nil {
		t.Fatalf("read outbox payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("decode outbox payload: %v", err)
	}
	if payload["proposal_id"] != fixture.proposalID || payload["type"] != "adjustment" ||
		payload["executor_run_id"] != "executor:test:run-1" || payload["ledger_txn_id"] != result.LedgerTxnID {
		t.Errorf("outbox payload = %+v", payload)
	}
}

// TestApplyWriteOff covers the write-off shape through the same adjustment
// ledger path; only the proposal type and case resolution differ.
func TestApplyWriteOff(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "write_off"})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplied || result.LedgerTxnID == "" {
		t.Fatalf("result = %+v, want applied with a ledger txn", result)
	}
	w.assertAppliedOnce(t, fixture, result.LedgerTxnID)
	if got := w.oneString(t,
		`SELECT kind FROM ledger_txns WHERE id = $1::uuid`, result.LedgerTxnID); got != "adjustment" {
		t.Errorf("ledger kind = %s, want adjustment", got)
	}
	if got := w.oneString(t,
		`SELECT resolution_type FROM cases WHERE id = $1::uuid`, fixture.caseID); got != "write_off" {
		t.Errorf("case resolution_type = %q, want write_off", got)
	}
}

// TestApplyReplay proves FIN-23 through the service: a second Apply returns a
// replay and writes no second ledger transaction, applied row, or outbox row.
func TestApplyReplay(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})

	first, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if second.Status != executor.StatusReplayed || !second.Replayed {
		t.Fatalf("second result = %+v, want replayed", second)
	}
	if second.LedgerTxnID != first.LedgerTxnID {
		t.Errorf("replay ledger txn = %q, want %q", second.LedgerTxnID, first.LedgerTxnID)
	}
	if got := w.count(t,
		`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`,
		"proposal:"+fixture.proposalID); got != 1 {
		t.Errorf("ledger txns = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, fixture.proposalID); got != 1 {
		t.Errorf("applied rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+fixture.proposalID); got != 1 {
		t.Errorf("outbox rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log WHERE action = 'proposal.transition' AND entity_id = $1`,
		fixture.proposalID); got != 1 {
		t.Errorf("proposal.transition audit rows = %d, want 1", got)
	}
}

// TestApplyStaleStateHash changes a break after proposal insertion. The
// re-validation hash no longer matches, so Apply records state_changed in a
// second transaction and leaves every apply side effect unwritten.
func TestApplyStaleStateHash(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE breaks SET delta_minor = delta_minor + 1 WHERE id = $1::uuid`, fixture.breakID); err != nil {
		t.Fatalf("mutate break: %v", err)
	}

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureStateChanged {
		t.Fatalf("result = %+v, want apply_failed/state_changed", result)
	}
	if result.Replayed {
		t.Error("failed apply reported Replayed")
	}
	w.assertNoApplySideEffects(t, fixture)
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "apply_failed" {
		t.Errorf("proposal status = %s, want apply_failed", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'proposal.transition' AND entity_id = $1
		    AND after->>'reason' = 'state_changed'`, fixture.proposalID); got != 1 {
		t.Errorf("state_changed transition audit rows = %d, want 1", got)
	}
}

// TestApplyBreakResolved resolves a break after proposal insertion and expects
// break_resolved with no apply writes.
func TestApplyBreakResolved(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE breaks SET status = 'resolved' WHERE id = $1::uuid`, fixture.breakID); err != nil {
		t.Fatalf("resolve break: %v", err)
	}

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureBreakResolved {
		t.Fatalf("result = %+v, want apply_failed/break_resolved", result)
	}
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, fixture.proposalID); got != 0 {
		t.Errorf("applied rows = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`,
		"proposal:"+fixture.proposalID); got != 0 {
		t.Errorf("ledger txns = %d, want 0", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+fixture.proposalID); got != 0 {
		t.Errorf("outbox rows = %d, want 0", got)
	}
	if got := w.status(t, fixture.caseID); got != "proposal_pending" {
		t.Errorf("case status = %s, want proposal_pending", got)
	}
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "apply_failed" {
		t.Errorf("proposal status = %s, want apply_failed", got)
	}
}

// TestApplyCaseResolved resolves the case after proposal insertion and expects
// break_resolved with no apply writes.
func TestApplyCaseResolved(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE cases SET status = 'resolved', resolved_at = $2, resolution_type = 'superseded_by_match'
		  WHERE id = $1::uuid`, fixture.caseID, w.clock.Now()); err != nil {
		t.Fatalf("resolve case: %v", err)
	}

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureBreakResolved {
		t.Fatalf("result = %+v, want apply_failed/break_resolved", result)
	}
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`, fixture.proposalID); got != 0 {
		t.Errorf("applied rows = %d, want 0", got)
	}
	if got := w.breakStatus(t, fixture.breakID); got != "in_case" {
		t.Errorf("break status = %s, want in_case", got)
	}
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "apply_failed" {
		t.Errorf("proposal status = %s, want apply_failed", got)
	}
}

// TestApplyExpired refuses an approved proposal whose expires_at is at or
// before the clock, recording expired and writing no apply side effect.
func TestApplyExpired(t *testing.T) {
	w := newWorld(t)
	expired := mustTime(t, testNow).Add(-time.Hour)
	fixture := w.newCase(t, caseSpec{expiresAt: &expired})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureExpired {
		t.Fatalf("result = %+v, want apply_failed/expired", result)
	}
	w.assertNoApplySideEffects(t, fixture)
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "apply_failed" {
		t.Errorf("proposal status = %s, want apply_failed", got)
	}
}

// TestApplyDeterministicLedgerError restricts the adjustment allowlist so the
// ledger rejects the stored payload; the executor maps that deterministic
// error to state_changed instead of retrying forever.
func TestApplyDeterministicLedgerError(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	restricted := w.withAdjustmentAccounts(t, "fees_expense")

	result, err := restricted.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureStateChanged {
		t.Fatalf("result = %+v, want apply_failed/state_changed", result)
	}
	w.assertNoApplySideEffects(t, fixture)
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "apply_failed" {
		t.Errorf("proposal status = %s, want apply_failed", got)
	}
}

// TestApplyValueDatePreference pins the value-date order: break external
// value_date, then earliest cited ledger_txn evidence value_date, then the
// clock date.
func TestApplyValueDatePreference(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec caseSpec
		want string
	}{
		{
			name: "external_before_ledger",
			spec: caseSpec{withExternal: true, withLedger: true, externalValueDate: "2026-02-03", ledgerValueDate: "2026-01-15"},
			want: "2026-02-03",
		},
		{
			name: "ledger_fallback",
			spec: caseSpec{withLedger: true, ledgerValueDate: "2026-01-15"},
			want: "2026-01-15",
		},
		{
			// The break is external-only and names no ledger transactions;
			// the cited evidence ledger ref supplies the fallback date.
			name: "cited_ledger_fallback",
			spec: caseSpec{externalRefMissing: true, citedLedger: true, ledgerValueDate: "2026-01-15"},
			want: "2026-01-15",
		},
		{
			name: "clock_fallback",
			spec: caseSpec{externalRefMissing: true},
			want: "2026-06-15",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			fixture := w.newCase(t, tc.spec)
			result, err := w.service.Apply(context.Background(), fixture.proposalID)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if result.Status != executor.StatusApplied {
				t.Fatalf("result = %+v, want applied", result)
			}
			if got := w.oneString(t,
				`SELECT value_date::text FROM ledger_txns WHERE id = $1::uuid`, result.LedgerTxnID); got != tc.want {
				t.Errorf("value date = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestApplyInvalidInputs checks the typed pre-condition errors never change
// state.
func TestApplyInvalidInputs(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if _, err := w.service.Apply(ctx, "not-a-uuid"); !errors.Is(err, executor.ErrInvalidProposalID) {
		t.Fatalf("invalid id error = %v, want ErrInvalidProposalID", err)
	}
	if _, err := w.service.Apply(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, executor.ErrProposalNotFound) {
		t.Fatalf("unknown proposal error = %v, want ErrProposalNotFound", err)
	}

	fixture := w.newCase(t, caseSpec{proposalStatus: "pending_approval"})
	if _, err := w.service.Apply(ctx, fixture.proposalID); !errors.Is(err, executor.ErrNotApplicable) {
		t.Fatalf("pending proposal error = %v, want ErrNotApplicable", err)
	}
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "pending_approval" {
		t.Errorf("proposal status = %s, want unchanged pending_approval", got)
	}
}

// TestNewServiceOptions pins validation and the copied defaults.
func TestNewServiceOptions(t *testing.T) {
	w := newWorld(t)
	base := w.options()
	for _, tc := range []struct {
		name   string
		mutate func(*executor.Options)
	}{
		{"nil_pool", func(o *executor.Options) { o.Pool = nil }},
		{"nil_ledger", func(o *executor.Options) { o.Ledger = nil }},
		{"nil_state", func(o *executor.Options) { o.State = nil }},
		{"nil_clock", func(o *executor.Options) { o.Clock = nil }},
		{"empty_run_id", func(o *executor.Options) { o.ExecutorRunID = "   " }},
		{"negative_batch", func(o *executor.Options) { o.BatchSize = -1 }},
		{"negative_poll", func(o *executor.Options) { o.PollInterval = -time.Second }},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			tc.mutate(&opts)
			if _, err := executor.NewService(opts); !errors.Is(err, executor.ErrInvalidOptions) {
				t.Fatalf("NewService error = %v, want ErrInvalidOptions", err)
			}
		})
	}

	opts := base
	opts.BatchSize = 0
	opts.PollInterval = 0
	opts.Sleep = nil
	svc, err := executor.NewService(opts)
	if err != nil {
		t.Fatalf("NewService defaults: %v", err)
	}
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
}
