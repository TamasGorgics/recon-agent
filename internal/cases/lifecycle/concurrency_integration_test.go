//go:build integration

// covers: FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"sync"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
)

type concurrentResult struct {
	result lifecycle.SupersedeResult
	err    error
}

// TestSupersedeConcurrentCallsProduceOneCancel starts two identical
// supersessions on the same case. The case FOR UPDATE lock serializes them:
// one transitions the proposal, the other observes no active proposal and
// becomes a no-op. Exactly one proposal transition, one cancel row, and one
// lifecycle audit survive.
func TestSupersedeConcurrentCallsProduceOneCancel(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	oldID := f.seedProposal(t, caseID, "proposed")

	start := make(chan struct{})
	results := make(chan concurrentResult, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := f.supersede(context.Background(), supersedeCall{
				caseID: caseID,
				actor:  "system:matcher",
				reason: lifecycle.ReasonLateMatch,
			})
			results <- concurrentResult{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var superseded, noops int
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent SupersedeActiveProposal: %v", got.err)
		}
		if got.result.Superseded {
			superseded++
			if got.result.ProposalID != oldID || !got.result.CancelInserted {
				t.Fatalf("winner result = %+v, want old %s with a cancel row",
					got.result, oldID)
			}
		} else {
			noops++
		}
	}
	if superseded != 1 || noops != 1 {
		t.Fatalf("results = %d superseded/%d no-op, want 1/1", superseded, noops)
	}

	if got := f.proposal(t, oldID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("proposal = %s/version %d, want superseded/1", got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 1 {
		t.Fatalf("outbox rows = %d, want 1", got)
	}
	if got := len(f.audits(t, oldID)); got != 1 {
		t.Fatalf("proposal transition audits = %d, want 1", got)
	}
	if got := len(f.audits(t, caseID)); got != 1 {
		t.Fatalf("lifecycle audits = %d, want 1", got)
	}
}
