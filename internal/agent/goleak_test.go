package agent_test

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// TestNoGoroutineLeaks exercises a successful run, a persisted provider error,
// and a canceled context so the loop cannot leak a goroutine on any terminal
// path.
func TestNoGoroutineLeaks(t *testing.T) {
	defer goleak.VerifyNone(t)

	t.Run("success", func(t *testing.T) {
		h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
			{Response: toolResponse("call_1", "escalate", `{}`)},
		}})
		if _, err := h.runner.Run(context.Background(), testCaseID); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	t.Run("provider error", func(t *testing.T) {
		h := newHarness(t, runnerOptions{steps: []llm.ScriptedStep{
			{Err: &llm.ProviderError{Provider: llm.ProviderOpenCodeGo, Kind: llm.KindUnavailable}},
		}})
		if _, err := h.runner.Run(context.Background(), testCaseID); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h := newHarness(t, runnerOptions{})
		if _, err := h.runner.Run(ctx, testCaseID); err == nil {
			t.Fatal("Run with a canceled context returned nil error")
		}
	})
}
