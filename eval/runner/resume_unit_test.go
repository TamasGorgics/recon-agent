package runner

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

func resumeFixtureRow() runRow {
	return runRow{
		Suite:         "agent",
		Split:         dataset.Dev,
		Model:         config.DefaultLLMModel,
		PromptVersion: config.DefaultAgentPromptVersion,
		Seed:          1,
		LLM:           LLMCassette,
		LLMKnown:      true,
		MaxCostUSD:    decimal.RequireFromString("5"),
		CapKnown:      true,
	}
}

func TestParseStoredRunSettings(t *testing.T) {
	llmMode, maxCost, llmKnown, capKnown, err := parseStoredRunSettings([]byte(
		`{"eval":{"llm":"LIVE","max_cost_usd":"5.25"},"pricing":{"schema":"llm-pricing/v1"}}`))
	if err != nil {
		t.Fatalf("parseStoredRunSettings: %v", err)
	}
	if llmMode != LLMLive || !llmKnown || !capKnown || !maxCost.Equal(decimal.RequireFromString("5.25")) {
		t.Fatalf("settings = llm %q known %t cap %s known %t", llmMode, llmKnown, maxCost, capKnown)
	}

	_, _, llmKnown, capKnown, err = parseStoredRunSettings([]byte(`{"pricing":{"schema":"x"}}`))
	if err != nil {
		t.Fatalf("missing eval settings: %v", err)
	}
	if llmKnown || capKnown {
		t.Fatalf("missing settings reported known: llm=%t cap=%t", llmKnown, capKnown)
	}

	for _, raw := range []string{
		`{"eval":{"max_cost_usd":""}}`,
		`{"eval":{"max_cost_usd":"not-a-decimal"}}`,
		`{"eval":{"max_cost_usd":"-1"}}`,
		`not-json`,
	} {
		if _, _, _, _, err := parseStoredRunSettings([]byte(raw)); err == nil {
			t.Fatalf("parseStoredRunSettings(%q) succeeded, want error", raw)
		}
	}
}

func TestApplyResumeOptionsRestoresStoredLiveModeAndCap(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	opts := Options{Resume: "11111111-1111-4111-8111-111111111111"}
	ApplyDefaults(&opts)
	stored := resumeFixtureRow()
	stored.LLM = LLMLive

	// No explicit --llm means the stored live mode is restored, so the live
	// key guard must fail rather than silently switching to cassette.
	err := applyResumeOptions(&opts, stored)
	if !IsUsage(err) || !strings.Contains(err.Error(), "OPENCODE_API_KEY") {
		t.Fatalf("stored live resume without key = %v, want live key usage error", err)
	}

	t.Setenv("OPENCODE_API_KEY", "test-key-not-used")
	opts = Options{Resume: "11111111-1111-4111-8111-111111111111"}
	ApplyDefaults(&opts)
	if err := applyResumeOptions(&opts, stored); err != nil {
		t.Fatalf("stored live resume with key: %v", err)
	}
	if opts.LLM != LLMLive {
		t.Fatalf("restored llm = %q, want live", opts.LLM)
	}
	if !opts.MaxCostUSD.Equal(stored.MaxCostUSD) {
		t.Fatalf("restored cap = %s, want %s", opts.MaxCostUSD, stored.MaxCostUSD)
	}
}

func TestApplyResumeOptionsRejectsConflictingLLM(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "test-key-not-used")
	stored := resumeFixtureRow()
	stored.LLM = LLMLive
	opts := Options{
		Resume: "11111111-1111-4111-8111-111111111111",
		LLM:    LLMCassette,
		LLMSet: true,
		Repeat: 1,
		Suite:  "agent",
		Split:  dataset.Dev,
		Seed:   ptrInt64(1),
		Model:  config.DefaultLLMModel,
		Prompt: config.DefaultAgentPromptVersion,
	}
	if err := applyResumeOptions(&opts, stored); !IsUsage(err) ||
		!strings.Contains(err.Error(), "conflicts with stored run llm") {
		t.Fatalf("llm conflict = %v, want usage conflict", err)
	}
}

func TestApplyResumeOptionsAllowsHigherExplicitCap(t *testing.T) {
	opts := Options{
		Resume:        "11111111-1111-4111-8111-111111111111",
		MaxCostUSD:    decimal.RequireFromString("10"),
		MaxCostUSDSet: true,
	}
	ApplyDefaults(&opts)
	stored := resumeFixtureRow()
	if err := applyResumeOptions(&opts, stored); err != nil {
		t.Fatalf("applyResumeOptions: %v", err)
	}
	if !opts.MaxCostUSD.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("explicit cap = %s, want 10", opts.MaxCostUSD)
	}
}

func TestApplyResumeOptionsRequiresCapProvenance(t *testing.T) {
	opts := Options{Resume: "11111111-1111-4111-8111-111111111111"}
	ApplyDefaults(&opts)
	stored := resumeFixtureRow()
	stored.CapKnown = false
	stored.MaxCostUSD = decimal.Zero
	if err := applyResumeOptions(&opts, stored); !IsUsage(err) ||
		!strings.Contains(err.Error(), "max_cost_usd") {
		t.Fatalf("missing cap provenance = %v, want usage error", err)
	}

	opts = Options{
		Resume:        "11111111-1111-4111-8111-111111111111",
		MaxCostUSDSet: true,
	}
	ApplyDefaults(&opts)
	if err := applyResumeOptions(&opts, stored); err != nil {
		t.Fatalf("explicit unlimited cap: %v", err)
	}
	if !opts.MaxCostUSD.IsZero() {
		t.Fatalf("explicit unlimited cap = %s, want 0", opts.MaxCostUSD)
	}
}
