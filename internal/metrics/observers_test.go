package metrics

import (
	"math"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestMatchObserverRecordsRunStats proves the matcher adapter maps RunStats to
// the frozen source/rule/side labels, ignores non-positive counts, and folds
// unknown rule, side, and source values to unknown.
func TestMatchObserverRecordsRunStats(t *testing.T) {
	m := mustNew(t, Options{Component: "api"})
	observer := NewMatchObserver(m)
	observer.MatchRun("psp_a", match.RunStats{
		ByPass:       map[string]int{"exact": 2, "fuzzy": 1, "not_a_rule": 3, "zero": 0, "negative": -1},
		BreaksBySide: map[string]int{"ledger_only": 1, "not_a_side": 2, "zero": 0},
	})

	views := gatherViews(t, m.Registry())
	runs := mustSeries(t, mustFamily(t, views, MetricMatchRunsTotal), map[string]string{"source": "psp_a"})
	if got, want := counterValue(t, runs), float64(1); got != want {
		t.Fatalf("match runs = %v, want %v", got, want)
	}
	matches := mustFamily(t, views, MetricMatchesTotal)
	for rule, want := range map[string]float64{"exact": 2, "fuzzy": 1, "unknown": 3} {
		series := mustSeries(t, matches, map[string]string{"source": "psp_a", "rule": rule})
		if got := counterValue(t, series); got != want {
			t.Fatalf("matches{rule=%s} = %v, want %v", rule, got, want)
		}
	}
	breaks := mustFamily(t, views, MetricBreaksTotal)
	for side, want := range map[string]float64{"ledger_only": 1, "unknown": 2} {
		series := mustSeries(t, breaks, map[string]string{"source": "psp_a", "side": side})
		if got := counterValue(t, series); got != want {
			t.Fatalf("breaks{side=%s} = %v, want %v", side, got, want)
		}
	}

	// An ID-like source can never become a series.
	observer.MatchRun("018f3b8e-5c00-7a3e-9a3e-123456789abc", match.RunStats{})
	views = gatherViews(t, m.Registry())
	series := mustSeries(t, mustFamily(t, views, MetricMatchRunsTotal), map[string]string{"source": unknownLabelValue})
	if got := counterValue(t, series); got != 1 {
		t.Fatalf("unknown source runs = %v, want 1", got)
	}
}

func TestMatchObserverRecordsBoundedFailure(t *testing.T) {
	m := mustNew(t, Options{Component: "api"})
	NewMatchObserver(m).MatchFailed("not_found")
	NewMatchObserver(m).MatchFailed("free text error: scope 123")
	views := gatherViews(t, m.Registry())
	family := mustFamily(t, views, MetricErrorsTotal)
	for code, want := range map[string]float64{"not_found": 1, unknownLabelValue: 1} {
		series := mustSeries(t, family, map[string]string{"component": "matcher", "code": code})
		if got := counterValue(t, series); got != want {
			t.Fatalf("errors{code=%s} = %v, want %v", code, got, want)
		}
	}
}

func TestCasesObserverRecordsAgeAndFailure(t *testing.T) {
	m := mustNew(t, Options{Component: "worker"})
	observer := NewCasesObserver(m, "worker")
	observer.CaseAge("open", 42)
	observer.CaseAge("resolved", 3600)
	observer.CaseAge("not_a_status", 7)
	observer.CaseFailed("conflict")
	observer.CaseFailed("free text")

	views := gatherViews(t, m.Registry())
	family := mustFamily(t, views, MetricCaseAgeSeconds)
	for status, want := range map[string]float64{"open": 42, "resolved": 3600, unknownLabelValue: 7} {
		histogram := histogramValue(t, mustSeries(t, family, map[string]string{"status": status}))
		if histogram.count != 1 || math.Abs(histogram.sum-want) > 1e-9 {
			t.Fatalf("case age{status=%s} = count %d sum %v, want 1/%v", status, histogram.count, histogram.sum, want)
		}
	}
	errorsFamily := mustFamily(t, views, MetricErrorsTotal)
	for code, want := range map[string]float64{"conflict": 1, unknownLabelValue: 1} {
		series := mustSeries(t, errorsFamily, map[string]string{"component": "worker", "code": code})
		if got := counterValue(t, series); got != want {
			t.Fatalf("errors{code=%s} = %v, want %v", code, got, want)
		}
	}
}

func TestAgentObserverRecordsOutcomeCostAndFailure(t *testing.T) {
	m := mustNew(t, Options{Component: "worker"})
	observer := NewAgentObserver(m)
	observer.AgentRun("proposal", "gpt-4o-mini", 0.25)
	observer.AgentRun("proposal", "gpt-4o-mini", 0.25)
	observer.AgentRun("not_an_outcome", "../free text", 0.5)
	observer.AgentFailed("internal")

	views := gatherViews(t, m.Registry())
	runs := mustFamily(t, views, MetricAgentRunsTotal)
	series := mustSeries(t, runs, map[string]string{"outcome": "proposal", "model": "gpt-4o-mini"})
	if got, want := counterValue(t, series), float64(2); got != want {
		t.Fatalf("agent runs = %v, want %v", got, want)
	}
	folded := mustSeries(t, runs, map[string]string{"outcome": unknownLabelValue, "model": unknownLabelValue})
	if got, want := counterValue(t, folded), float64(1); got != want {
		t.Fatalf("unknown agent runs = %v, want %v", got, want)
	}
	cost := mustFamily(t, views, MetricAgentCostUSDTotal)
	got := counterValue(t, mustSeries(t, cost, map[string]string{"model": "gpt-4o-mini"}))
	if math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("agent cost = %v, want 0.5", got)
	}
	errorsFamily := mustFamily(t, views, MetricErrorsTotal)
	errSeries := mustSeries(t, errorsFamily, map[string]string{"component": "agent", "code": "internal"})
	if got, want := counterValue(t, errSeries), float64(1); got != want {
		t.Fatalf("agent errors = %v, want %v", got, want)
	}
}

func TestWorkerObserverFoldsRawTextToUnknown(t *testing.T) {
	m := mustNew(t, Options{Component: "worker"})
	observer := NewWorkerObserver(m)
	observer.WorkerFailed("internal")
	observer.WorkerFailed("worker: run failed: provider timeout 123")

	views := gatherViews(t, m.Registry())
	family := mustFamily(t, views, MetricErrorsTotal)
	for code, want := range map[string]float64{"internal": 1, unknownLabelValue: 1} {
		series := mustSeries(t, family, map[string]string{"component": "worker", "code": code})
		if got := counterValue(t, series); got != want {
			t.Fatalf("errors{code=%s} = %v, want %v", code, got, want)
		}
	}
}

// TestObserversNilSafe proves a nil registry or nil adapter is a no-op rather
// than a panic; binaries may wire an observer before metrics initialization in
// tests.
func TestObserversNilSafe(t *testing.T) {
	(*MatchObserver)(nil).MatchRun("psp_a", match.RunStats{})
	(*MatchObserver)(nil).MatchFailed("internal")
	(*CasesObserver)(nil).CaseAge("open", 1)
	(*CasesObserver)(nil).CaseFailed("internal")
	(*AgentObserver)(nil).AgentRun("proposal", "m", 1)
	(*AgentObserver)(nil).AgentFailed("internal")
	(*WorkerObserver)(nil).WorkerFailed("internal")
	NewMatchObserver(nil).MatchRun("psp_a", match.RunStats{})
	NewCasesObserver(nil, "api").CaseFailed("internal")
}
