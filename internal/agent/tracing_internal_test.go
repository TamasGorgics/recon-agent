package agent

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// TestTraceUsageTokensMatchesBudgetAccounting pins the token source order used
// by llm.call spans to the authoritative run accounting. A provider response
// can carry both raw prompt/completion counts and normalized input/output
// counts (where the normalized input excludes cached tokens); the span must
// agree with case.run and agent_runs, not silently use a different total.
func TestTraceUsageTokensMatchesBudgetAccounting(t *testing.T) {
	tests := []struct {
		name  string
		usage llm.Usage
	}{
		{
			name: "provider prompt and completion with cached input",
			usage: llm.Usage{
				PromptTokens:     100,
				CompletionTokens: 20,
				CachedTokens:     40,
				InputTokens:      60,
				OutputTokens:     20,
				TotalTokens:      120,
			},
		},
		{
			name:  "normalized input and output only",
			usage: llm.Usage{InputTokens: 60, OutputTokens: 20},
		},
		{
			name:  "prompt and completion only",
			usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20},
		},
		{
			name:  "negative provider fields are clamped",
			usage: llm.Usage{PromptTokens: -5, CompletionTokens: -2, InputTokens: 60, OutputTokens: 20},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var accumulated budgetUsage
			accumulated.addUsage(test.usage)
			input, output := traceUsageTokens(test.usage)
			if input != accumulated.TokensIn || output != accumulated.TokensOut {
				t.Fatalf("traceUsageTokens = %d/%d, budget accounting = %d/%d",
					input, output, accumulated.TokensIn, accumulated.TokensOut)
			}
		})
	}
}
