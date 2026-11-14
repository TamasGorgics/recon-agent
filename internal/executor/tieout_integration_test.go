//go:build integration

// covers: FIN-20, FIN-21, FIN-23, FIN-54

package executor_test

import (
	"context"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// serviceWithLedgerAllowlist builds an executor whose adjustment allowlist is
// exactly codes, which lets the tie-out test post through the scope's clearing
// account.
func (w *world) serviceWithLedgerAllowlist(t *testing.T, codes ...string) *executor.Service {
	t.Helper()
	opts := w.options()
	opts.Ledger = ledger.NewService(w.clock, config.LedgerConfig{AdjustmentAccounts: codes})
	svc, err := executor.NewService(opts)
	if err != nil {
		t.Fatalf("new executor service with ledger allowlist: %v", err)
	}
	return svc
}

// TestAppliedAdjustmentTieout pins the M1-15 invariant: an applied adjustment
// adds an open ledger item and its clearing-account balance in equal measure,
// so the scope/date unexplained difference is unchanged (zero before and
// after when the statement and external open item already agree).
func TestAppliedAdjustmentTieout(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	asOf := mustDate(t, "2026-02-03")
	if _, err := w.db.Pool.Exec(ctx,
		`INSERT INTO statement_balances
		     (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
		 VALUES ($1::uuid, $2::date, 0, 1234, 'USD', $3::uuid)`,
		w.scopeID, asOf, w.batchID); err != nil {
		t.Fatalf("insert statement balance: %v", err)
	}

	fixture := w.newCase(t, caseSpec{lines: []testLine{
		{AccountCode: "psp_clearing_usd", Direction: "debit", AmountMinor: 500, Currency: "USD"},
		{AccountCode: "fees_expense", Direction: "credit", AmountMinor: 500, Currency: "USD"},
	}})

	before, err := tieout.Compute(ctx, w.db.Pool, w.scopeID, asOf)
	if err != nil {
		t.Fatalf("tieout before apply: %v", err)
	}
	if !before.Unexplained.IsZero() {
		t.Fatalf("tieout before apply unexplained = %s, want zero: %+v", before.Unexplained, before)
	}

	svc := w.serviceWithLedgerAllowlist(t, "psp_clearing_usd", "fees_expense")
	result, err := svc.Apply(ctx, fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplied {
		t.Fatalf("result = %+v, want applied", result)
	}

	after, err := tieout.Compute(ctx, w.db.Pool, w.scopeID, asOf)
	if err != nil {
		t.Fatalf("tieout after apply: %v", err)
	}
	if !after.Unexplained.IsZero() {
		t.Fatalf("tieout after apply unexplained = %s, want zero: %+v", after.Unexplained, after)
	}
	delta, err := after.Unexplained.Sub(before.Unexplained)
	if err != nil {
		t.Fatalf("compute unexplained delta: %v", err)
	}
	if !delta.IsZero() {
		t.Errorf("unexplained delta = %s, want zero", delta)
	}
	if got := after.LedgerBalance.AmountMinor() - before.LedgerBalance.AmountMinor(); got != 500 {
		t.Errorf("ledger balance delta = %d, want 500", got)
	}
	if got := after.LedgerOnly.AmountMinor() - before.LedgerOnly.AmountMinor(); got != 500 {
		t.Errorf("ledger-only delta = %d, want 500", got)
	}
	if after.ExternalOnly.AmountMinor() != before.ExternalOnly.AmountMinor() {
		t.Errorf("external-only changed from %d to %d, want unchanged",
			before.ExternalOnly.AmountMinor(), after.ExternalOnly.AmountMinor())
	}
}
