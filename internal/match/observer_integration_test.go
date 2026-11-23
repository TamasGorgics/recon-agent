//go:build integration

package match_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// recordingMatchObserver records every observer callback in order.
type recordingMatchObserver struct {
	runs     []match.RunResult
	sources  []string
	failures []string
}

func (o *recordingMatchObserver) MatchRun(source string, stats match.RunStats) {
	o.sources = append(o.sources, source)
	o.runs = append(o.runs, match.RunResult{Stats: stats})
}

func (o *recordingMatchObserver) MatchFailed(code string) { o.failures = append(o.failures, code) }

func newObservedEngine(t *testing.T, f *fixture, observer match.Observer) *match.Engine {
	t.Helper()
	engine, err := match.New(match.Options{
		DB:       f.db.Pool,
		Clock:    testClock(t),
		Actor:    matcherActor(t),
		Config:   config.MatchConfig{FeeToleranceMinor: map[string]int64{"USD": usdTolerance}},
		Observer: observer,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine
}

// TestEngineObserverRecordsCommittedRun proves one successful run emits exactly
// one observation with the scope source, per-rule accepted matches, and break
// sides, and that a later failed run emits only a bounded failure code.
func TestEngineObserverRecordsCommittedRun(t *testing.T) {
	f := newFixture(t, "psp_a")
	f.addExternal(t, "OBS-OK", 5_000, "2026-06-10")
	f.addLedger(t, "OBS-OK", 5_000, "2026-06-10")
	f.addExternal(t, "OBS-ONLY", -700, "2026-06-11")

	observer := &recordingMatchObserver{}
	engine := newObservedEngine(t, f, observer)
	result := runEngine(t, engine, f.scopeID, "2026-06-01", "2026-06-30")

	if len(observer.runs) != 1 || len(observer.sources) != 1 {
		t.Fatalf("runs = %d sources = %d, want 1/1", len(observer.runs), len(observer.sources))
	}
	if observer.sources[0] != "psp_a" {
		t.Fatalf("observed source = %q, want psp_a", observer.sources[0])
	}
	if got, want := observer.runs[0].Stats.Matched, 1; got != want {
		t.Fatalf("observed matched = %d, want %d", got, want)
	}
	if got, want := observer.runs[0].Stats.ByPass["exact"], 1; got != want {
		t.Fatalf("observed exact matches = %d, want %d", got, want)
	}
	if got, want := observer.runs[0].Stats.BreaksBySide["external_only"], 1; got != want {
		t.Fatalf("observed external_only breaks = %d, want %d", got, want)
	}
	if observer.runs[0].Stats.Matched != result.Stats.Matched {
		t.Fatalf("observed stats differ from result stats")
	}

	_, err := engine.Run(context.Background(), match.RunRequest{
		ScopeID: "00000000-0000-4000-8000-000000000000",
		Window:  match.Window{From: mustDate(t, "2026-06-01"), To: mustDate(t, "2026-06-30")},
	})
	if !errors.Is(err, match.ErrScopeNotFound) {
		t.Fatalf("unknown scope error = %v, want ErrScopeNotFound", err)
	}
	if len(observer.runs) != 1 {
		t.Fatalf("runs after failed run = %d, want 1", len(observer.runs))
	}
	if len(observer.failures) != 1 || observer.failures[0] != match.ErrorCodeNotFound {
		t.Fatalf("failures = %v, want [not_found]", observer.failures)
	}
}

// TestEngineObserverRejectsInvalidRequest keeps caller mistakes bounded and
// proves no run observation accompanies them.
func TestEngineObserverRejectsInvalidRequest(t *testing.T) {
	f := newFixture(t, "psp_a")
	observer := &recordingMatchObserver{}
	engine := newObservedEngine(t, f, observer)

	if _, err := engine.Run(context.Background(), match.RunRequest{}); !errors.Is(err, match.ErrInvalidRequest) {
		t.Fatalf("empty request error = %v, want ErrInvalidRequest", err)
	}
	if len(observer.runs) != 0 {
		t.Fatalf("runs = %d, want 0", len(observer.runs))
	}
	if len(observer.failures) != 1 || observer.failures[0] != match.ErrorCodeInvalidRequest {
		t.Fatalf("failures = %v, want [invalid_request]", observer.failures)
	}
}
