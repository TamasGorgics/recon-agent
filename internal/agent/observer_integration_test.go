//go:build integration

package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

type observedAgentRun struct {
	outcome string
	model   string
	costUSD float64
}

type recordingRunObserver struct {
	runs     []observedAgentRun
	failures []string
}

func (o *recordingRunObserver) AgentRun(outcome, model string, costUSD float64) {
	o.runs = append(o.runs, observedAgentRun{outcome: outcome, model: model, costUSD: costUSD})
}

func (o *recordingRunObserver) AgentFailed(code string) { o.failures = append(o.failures, code) }

// TestAgentStoreObserverIntegration proves the run store observes exactly one
// outcome/cost after a one-row finish and emits only bounded failure codes for
// validation and not-found paths (never a run observation).
func TestAgentStoreObserverIntegration(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openAgentRolePool(t, d, db.RoleApp)

	caseID := insertAgentCase(t, d, "open", nil, nil)
	runID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	observer := &recordingRunObserver{}
	store := agent.NewDBStore(app, agent.WithObserver(observer))

	startedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := store.CreateRun(ctx, agent.RunRecord{
		ID: runID, CaseID: caseID, Model: "test-model", PromptVersion: "v1", StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := store.FinishRun(ctx, agent.RunFinish{
		RunID: runID, EndedAt: startedAt.Add(2 * time.Second), TokensIn: 10, TokensOut: 5,
		CostUSD: decimal.RequireFromString("0.25"), Outcome: agent.OutcomeProposal,
		Model: "test-model-2026",
	}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if len(observer.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(observer.runs))
	}
	if got := observer.runs[0]; got.outcome != "proposal" || got.model != "test-model-2026" ||
		got.costUSD != 0.25 {
		t.Fatalf("run observation = %+v, want proposal/test-model-2026/0.25", got)
	}
	if len(observer.failures) != 0 {
		t.Fatalf("failures = %v, want none", observer.failures)
	}

	// Invalid outcome is rejected before the write; only a bounded code.
	if err := store.FinishRun(ctx, agent.RunFinish{
		RunID: runID, EndedAt: startedAt, Outcome: agent.Outcome("bogus"),
	}); err == nil {
		t.Fatal("FinishRun with invalid outcome succeeded")
	}
	// Negative cost is rejected before the write.
	if err := store.FinishRun(ctx, agent.RunFinish{
		RunID: runID, EndedAt: startedAt, CostUSD: decimal.RequireFromString("-1"),
		Outcome: agent.OutcomeProposal,
	}); err == nil {
		t.Fatal("FinishRun with negative cost succeeded")
	}
	// A zero-row update is not a completed run.
	err := store.FinishRun(ctx, agent.RunFinish{
		RunID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", EndedAt: startedAt,
		Outcome: agent.OutcomeProposal, Model: "test-model",
	})
	if !errors.Is(err, agent.ErrRunNotFound) {
		t.Fatalf("missing run error = %v, want ErrRunNotFound", err)
	}

	if len(observer.runs) != 1 {
		t.Fatalf("runs after rejected finalizes = %d, want 1", len(observer.runs))
	}
	want := []string{agent.ErrorCodeInvalidOutput, agent.ErrorCodeInvalidRequest, agent.ErrorCodeNotFound}
	if len(observer.failures) != len(want) {
		t.Fatalf("failures = %v, want %v", observer.failures, want)
	}
	for i := range want {
		if observer.failures[i] != want[i] {
			t.Fatalf("failures = %v, want %v", observer.failures, want)
		}
	}
}
