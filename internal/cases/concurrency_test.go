//go:build integration

// covers: FIN-24, FIN-30

package cases_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
)

// TestConcurrentLease is the FIN-24 exclusivity test: twenty workers racing one
// queue under -race must receive twenty distinct cases, and each lease must
// leave exactly one version increment and one audit row.
func TestConcurrentLease(t *testing.T) {
	f := newFixture(t)
	const workers = 20
	for i := 0; i < workers; i++ {
		seedOneCase(t, f, fmt.Sprintf("concurrent-%02d", i), int64(1000+i))
	}

	type leaseResult struct {
		worker int
		c      cases.Case
		err    error
	}
	results := make(chan leaseResult, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(worker int) {
			defer wg.Done()
			leased, err := f.service.Lease(context.Background(), fmt.Sprintf("worker-%02d", worker), time.Minute)
			results <- leaseResult{worker: worker, c: leased, err: err}
		}(i)
	}
	wg.Wait()
	close(results)

	seen := make(map[string]int, workers)
	for result := range results {
		if result.err != nil {
			t.Fatalf("worker %d Lease: %v", result.worker, result.err)
		}
		if result.c.ID == "" {
			t.Fatalf("worker %d got an empty case id", result.worker)
		}
		if other, duplicate := seen[result.c.ID]; duplicate {
			t.Fatalf("case %s leased by workers %d and %d", result.c.ID, other, result.worker)
		}
		seen[result.c.ID] = result.worker
		if result.c.Status != cases.StatusInAgent {
			t.Fatalf("case %s status = %s, want in_agent", result.c.ID, result.c.Status)
		}
		if result.c.LeasedBy == nil || *result.c.LeasedBy != fmt.Sprintf("worker-%02d", result.worker) {
			t.Fatalf("case %s leased_by = %v, want worker-%02d", result.c.ID, result.c.LeasedBy, result.worker)
		}
		if result.c.Version != 1 {
			t.Fatalf("case %s version = %d, want 1", result.c.ID, result.c.Version)
		}
		if result.c.Attempts != 0 {
			t.Fatalf("case %s attempts = %d, want 0", result.c.ID, result.c.Attempts)
		}
	}
	if got, want := len(seen), workers; got != want {
		t.Fatalf("distinct leased cases = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM cases WHERE status = 'in_agent' AND leased_by IS NOT NULL`), workers; got != want {
		t.Fatalf("in_agent cases = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM audit_log WHERE action = $1`, cases.ActionCaseLease), workers; got != want {
		t.Fatalf("lease audit rows = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM cases WHERE attempts <> 0 OR version <> 1`), 0; got != want {
		t.Fatalf("cases with unexpected attempts/version = %d, want %d", got, want)
	}
}
