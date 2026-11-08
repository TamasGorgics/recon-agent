//go:build integration

package runner_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/runner"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// TestEvalRunResumeStoredProvenance covers B1: a resume restores the stored
// llm mode and cost cap, rejects an explicit conflicting --llm, and records
// the effective cap in both config.cost_cap and config.eval.max_cost_usd.
func stringValue(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

func TestEvalRunResumeStoredProvenance(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)
	ctx := context.Background()

	firstOpts := baseOptions(dir)
	firstOpts.MaxCostUSD = decimal.RequireFromString("5")
	firstOpts.MaxCostUSDSet = true
	initial, err := runner.Run(ctx, firstOpts, baseDeps(t, cfg, resultsDSN, escalationClient()))
	if err != nil {
		t.Fatalf("initial run: %v", err)
	}
	if len(initial.Runs) != 1 || initial.Runs[0].Completed != 20 {
		t.Fatalf("initial summary = %+v", initial.Runs)
	}
	runID := initial.Runs[0].RunID

	// Simulate a live run whose process crashed after the last result insert
	// but before finalization.
	if _, err := pool.Exec(ctx,
		`UPDATE eval_runs
		    SET ended_at = NULL,
		        config = jsonb_set(
		            jsonb_set(config - 'aborted' - 'abort_reason' - 'cost_spent' - 'cost_cap',
		                      '{eval,llm}', '"live"'),
		            '{eval,max_cost_usd}', '"5"')
		  WHERE id = $1::uuid`, runID); err != nil {
		t.Fatalf("prepare stored live run: %v", err)
	}

	resumeOpts := baseOptions(dir)
	resumeOpts.Resume = runID
	resumeDeps := baseDeps(t, cfg, resultsDSN, escalationClient())

	// No explicit --llm means the stored live mode is restored; without a key
	// that must fail instead of silently switching to cassette.
	t.Setenv("OPENCODE_API_KEY", "")
	_, err = runner.Run(ctx, resumeOpts, resumeDeps)
	if !runner.IsUsage(err) || !strings.Contains(err.Error(), "OPENCODE_API_KEY") {
		t.Fatalf("stored live resume without key = %v, want live key usage error", err)
	}

	// An explicit conflicting --llm is rejected, not silently overridden.
	conflictOpts := resumeOpts
	conflictOpts.LLM = runner.LLMCassette
	conflictOpts.LLMSet = true
	_, err = runner.Run(ctx, conflictOpts, resumeDeps)
	if !runner.IsUsage(err) || !strings.Contains(err.Error(), "conflicts with stored run llm") {
		t.Fatalf("stored live resume with conflicting --llm = %v, want usage conflict", err)
	}

	// With the key present and no explicit cap, the stored live mode and cap
	// are restored and the already-complete run is finalized as completed.
	t.Setenv("OPENCODE_API_KEY", "test-key-not-used")
	final, err := runner.Run(ctx, resumeOpts, resumeDeps)
	if err != nil {
		t.Fatalf("stored live resume: %v", err)
	}
	if len(final.Runs) != 1 || final.Runs[0].Aborted || final.Runs[0].Note != "already complete" {
		t.Fatalf("final summary = %+v", final.Runs)
	}

	var (
		endedSet   bool
		aborted    bool
		costCap    *string
		evalCap    *string
		resumedSet bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT ended_at IS NOT NULL,
		        (config->>'aborted')::boolean,
		        config->>'cost_cap',
		        config->'eval'->>'max_cost_usd',
		        config->>'resumed_at' IS NOT NULL
		   FROM eval_runs WHERE id = $1::uuid`, runID).
		Scan(&endedSet, &aborted, &costCap, &evalCap, &resumedSet); err != nil {
		t.Fatalf("read finalized live run: %v", err)
	}
	if !endedSet || aborted || !resumedSet {
		t.Fatalf("finalized run = ended %t aborted %t resumed %t", endedSet, aborted, resumedSet)
	}
	if costCap == nil || *costCap != "5" || evalCap == nil || *evalCap != "5" {
		t.Fatalf("effective cap provenance = cost_cap %v eval.max_cost_usd %v, want 5/5", costCap, evalCap)
	}
}

// TestEvalRunLastCaseOverrunAborts covers B2: an in-flight final case that
// pushes persisted spend over the cap still aborts the run with cost_cap.
func TestEvalRunLastCaseOverrunAborts(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)
	ctx := context.Background()

	opts := baseOptions(dir)
	opts.Concurrency = 1
	opts.MaxCostUSD = decimal.RequireFromString("0.0199")
	opts.MaxCostUSDSet = true

	summary, err := runner.Run(ctx, opts, baseDeps(t, cfg, resultsDSN, escalationClient()))
	if err != nil {
		t.Fatalf("overrun run: %v", err)
	}
	if len(summary.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(summary.Runs))
	}
	run := summary.Runs[0]
	if !run.Aborted || run.Completed != 20 || !run.SpentUSD.Equal(decimal.RequireFromString("0.02")) {
		t.Fatalf("overrun summary = %+v, want 20 completed, spent 0.02, aborted", run)
	}
	if got := countCases(t, pool, run.RunID); got != 20 {
		t.Fatalf("case rows = %d, want 20 (the last in-flight case must be recorded)", got)
	}

	var (
		endedSet    bool
		aborted     bool
		abortReason *string
		spent       *string
		capText     *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT ended_at IS NOT NULL, (config->>'aborted')::boolean,
		        config->>'abort_reason', config->>'cost_spent', config->>'cost_cap'
		   FROM eval_runs WHERE id = $1::uuid`, run.RunID).
		Scan(&endedSet, &aborted, &abortReason, &spent, &capText); err != nil {
		t.Fatalf("read overrun run: %v", err)
	}
	if !endedSet || !aborted || abortReason == nil || *abortReason != "cost_cap" {
		t.Fatalf("overrun state = ended %t aborted %t reason %v", endedSet, aborted, abortReason)
	}
	if spent == nil || *spent != "0.02" || capText == nil || *capText != "0.0199" {
		t.Fatalf("overrun spend/cap = %q/%q", stringValue(spent), stringValue(capText))
	}
	assertNoPackageSandboxes(t)
}

// TestEvalRunExactlyAtCapCompletes covers the B2 boundary: spending exactly
// the cap after every case completed is allowed to stay completed.
func TestEvalRunExactlyAtCapCompletes(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	_, resultsDSN := newResultsPool(t)

	opts := baseOptions(dir)
	opts.Concurrency = 1
	opts.MaxCostUSD = decimal.RequireFromString("0.02")
	opts.MaxCostUSDSet = true

	summary, err := runner.Run(context.Background(), opts,
		baseDeps(t, cfg, resultsDSN, escalationClient()))
	if err != nil {
		t.Fatalf("exactly-at-cap run: %v", err)
	}
	if len(summary.Runs) != 1 || summary.Runs[0].Aborted ||
		summary.Runs[0].Completed != 20 {
		t.Fatalf("exactly-at-cap summary = %+v, want 20 completed and not aborted", summary.Runs)
	}
	assertNoPackageSandboxes(t)
}

// TestEvalRunCompletedResumeFinalizes covers B3: a crash between the last
// result insert and the finalize update is repairable by resuming the
// complete run.
func TestEvalRunCompletedResumeFinalizes(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)
	ctx := context.Background()

	firstOpts := baseOptions(dir)
	firstOpts.MaxCostUSD = decimal.RequireFromString("1")
	firstOpts.MaxCostUSDSet = true
	initial, err := runner.Run(ctx, firstOpts, baseDeps(t, cfg, resultsDSN, escalationClient()))
	if err != nil {
		t.Fatalf("initial run: %v", err)
	}
	runID := initial.Runs[0].RunID

	if _, err := pool.Exec(ctx,
		`UPDATE eval_runs
		    SET ended_at = NULL,
		        config = config - 'aborted' - 'abort_reason' - 'cost_spent' - 'cost_cap'
		  WHERE id = $1::uuid`, runID); err != nil {
		t.Fatalf("simulate crash before finalize: %v", err)
	}

	resumeOpts := baseOptions(dir)
	resumeOpts.Resume = runID
	var executed atomic.Int64
	resumeDeps := baseDeps(t, cfg, resultsDSN,
		func(ref sandbox.CaseRef, truth dataset.ScenarioTruth) (llm.Client, error) {
			executed.Add(1)
			return escalationClient()(ref, truth)
		})

	summary, err := runner.Run(ctx, resumeOpts, resumeDeps)
	if err != nil {
		t.Fatalf("completed resume: %v", err)
	}
	if len(summary.Runs) != 1 || summary.Runs[0].Note != "already complete" ||
		summary.Runs[0].Aborted || summary.Runs[0].Completed != 20 {
		t.Fatalf("completed resume summary = %+v", summary.Runs)
	}
	if got := executed.Load(); got != 0 {
		t.Fatalf("completed resume executed %d scenarios, want 0", got)
	}

	var (
		endedSet bool
		aborted  bool
		spent    *string
		capText  *string
		evalCap  *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT ended_at IS NOT NULL, (config->>'aborted')::boolean,
		        config->>'cost_spent', config->>'cost_cap',
		        config->'eval'->>'max_cost_usd'
		   FROM eval_runs WHERE id = $1::uuid`, runID).
		Scan(&endedSet, &aborted, &spent, &capText, &evalCap); err != nil {
		t.Fatalf("read repaired run: %v", err)
	}
	if !endedSet || aborted {
		t.Fatalf("repaired run = ended %t aborted %t", endedSet, aborted)
	}
	if spent == nil || *spent != "0.02" || capText == nil || *capText != "1" ||
		evalCap == nil || *evalCap != "1" {
		t.Fatalf("repaired run spend/cap = %v/%v eval %v", spent, capText, evalCap)
	}
	if got := countCases(t, pool, runID); got != 20 {
		t.Fatalf("case rows after repair = %d, want 20", got)
	}
	assertNoPackageSandboxes(t)
}

// TestEvalRunConcurrentResumeLock covers the concurrent-resume contract: one
// resume acquires the run lock, the other fails with ErrRunLocked before
// executing, every pending scenario runs once, and spend is not duplicated.
func TestEvalRunConcurrentResumeLock(t *testing.T) {
	ds, cfg := miniAcceptanceDataset(t)
	dir := writeManifest(t, ds)
	pool, resultsDSN := newResultsPool(t)
	ctx := context.Background()

	firstOpts := baseOptions(dir)
	firstOpts.Concurrency = 1
	firstOpts.MaxCostUSD = decimal.RequireFromString("0.005")
	firstOpts.MaxCostUSDSet = true
	first, err := runner.Run(ctx, firstOpts, baseDeps(t, cfg, resultsDSN, escalationClient()))
	if err != nil {
		t.Fatalf("initial partial run: %v", err)
	}
	runID := first.Runs[0].RunID
	completedBefore := countCases(t, pool, runID)
	if completedBefore <= 0 || completedBefore >= 20 {
		t.Fatalf("partial run completed %d cases, want 1..19", completedBefore)
	}
	pending := 20 - completedBefore

	// The lock holder blocks on its first client construction until the losing
	// resume has reported ErrRunLocked, making the contention deterministic.
	release := make(chan struct{})
	var calls atomic.Int64
	inner := escalationClient()
	counting := func(ref sandbox.CaseRef, truth dataset.ScenarioTruth) (llm.Client, error) {
		calls.Add(1)
		<-release
		return inner(ref, truth)
	}
	deps := baseDeps(t, cfg, resultsDSN, counting)

	resumeOpts := baseOptions(dir)
	resumeOpts.Concurrency = 4
	resumeOpts.MaxCostUSD = decimal.RequireFromString("1")
	resumeOpts.MaxCostUSDSet = true
	resumeOpts.Resume = runID

	type outcome struct {
		summary runner.Summary
		err     error
	}
	outcomes := make(chan outcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			summary, err := runner.Run(ctx, resumeOpts, deps)
			outcomes <- outcome{summary: summary, err: err}
		}()
	}
	close(start)

	firstOutcome := <-outcomes
	close(release)
	secondOutcome := <-outcomes
	wg.Wait()

	var succeeded, locked int
	for _, result := range []outcome{firstOutcome, secondOutcome} {
		switch {
		case result.err == nil:
			succeeded++
			if len(result.summary.Runs) != 1 || result.summary.Runs[0].Completed != 20 ||
				result.summary.Runs[0].Aborted {
				t.Fatalf("concurrent resume summary = %+v", result.summary.Runs)
			}
		case errors.Is(result.err, runner.ErrRunLocked):
			locked++
		default:
			t.Fatalf("concurrent resume error = %v, want nil or ErrRunLocked", result.err)
		}
	}
	if succeeded != 1 || locked != 1 {
		t.Fatalf("concurrent resume outcomes = %d succeeded, %d locked; want 1/1", succeeded, locked)
	}
	if got := calls.Load(); got != int64(pending) {
		t.Fatalf("client executions = %d, want exactly the %d pending scenarios", got, pending)
	}
	if got := countCases(t, pool, runID); got != 20 {
		t.Fatalf("case rows = %d, want 20", got)
	}

	var (
		rows int
		sum  string
	)
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(cost_usd), 0)::text
		   FROM eval_case_results WHERE eval_run_id = $1::uuid`, runID).
		Scan(&rows, &sum); err != nil {
		t.Fatalf("read concurrent resume rows: %v", err)
	}
	if rows != 20 || sum != "0.020000" {
		t.Fatalf("concurrent resume rows/cost = %d/%s, want 20/0.020000", rows, sum)
	}
	assertNoPackageSandboxes(t)
}
