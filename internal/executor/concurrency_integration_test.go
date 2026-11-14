//go:build integration

// covers: FIN-20, FIN-21, FIN-23

package executor_test

import (
	"context"
	"sync"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/executor"
)

// TestConcurrentApplyExactlyOneWinner races two Apply calls on one proposal.
// The serializable transaction and applied_proposals primary key must produce
// exactly one ledger transaction, one applied row, one outbox row, and one
// transition; the loser replays.
func TestConcurrentApplyExactlyOneWinner(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})

	start := make(chan struct{})
	results := make([]executor.Result, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = w.service.Apply(context.Background(), fixture.proposalID)
		}()
	}
	close(start)
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("Apply %d error = %v, want nil", i, errs[i])
		}
	}

	applied, replayed := 0, 0
	var ledgerTxnID string
	for _, result := range results {
		switch {
		case result.Status == executor.StatusApplied && !result.Replayed:
			applied++
			ledgerTxnID = result.LedgerTxnID
		case result.Status == executor.StatusReplayed && result.Replayed:
			replayed++
		default:
			t.Fatalf("unexpected concurrent result %+v", result)
		}
	}
	if applied != 1 || replayed != 1 {
		t.Fatalf("applied/replayed = %d/%d, want 1/1", applied, replayed)
	}
	w.assertAppliedOnce(t, fixture, ledgerTxnID)
	if got := w.count(t,
		`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`,
		"proposal:"+fixture.proposalID); got != 1 {
		t.Errorf("ledger txns = %d, want 1", got)
	}
}
