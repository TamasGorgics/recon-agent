package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

func TestParseOptionsDefaults(t *testing.T) {
	opts, err := ParseOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	ApplyDefaults(&opts)
	if err := Validate(opts); err != nil {
		t.Fatalf("Validate defaults: %v", err)
	}
	if opts.Suite != DefaultSuite || opts.Split != dataset.Dev || opts.Repeat != 1 ||
		opts.Concurrency != 4 || opts.LLM != LLMCassette {
		t.Fatalf("defaults = %+v", opts)
	}
	if opts.Model != config.DefaultLLMModel || opts.Prompt != config.DefaultAgentPromptVersion {
		t.Fatalf("model/prompt defaults = %q/%q", opts.Model, opts.Prompt)
	}
	if opts.Seed != nil || !opts.MaxCostUSD.IsZero() || opts.Resume != "" {
		t.Fatalf("seed/cap/resume = %v/%s/%q", opts.Seed, opts.MaxCostUSD, opts.Resume)
	}
}

func TestParseOptionsExplicit(t *testing.T) {
	opts, err := ParseOptions([]string{
		"--suite", "agent",
		"--split", "test",
		"--model", "opencode-go/deepseek-v4.1-flash",
		"--prompt", "v1",
		"--seed", "7",
		"--repeat", "2",
		"--concurrency", "8",
		"--llm", "cassette",
		"--max-cost-usd", "1.50",
		"--dataset-dir", "/tmp/ds",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	ApplyDefaults(&opts)
	if err := Validate(opts); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if opts.Split != dataset.Test || opts.Seed == nil || *opts.Seed != 7 {
		t.Fatalf("split/seed = %s/%v", opts.Split, opts.Seed)
	}
	if opts.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", opts.Model)
	}
	if opts.Repeat != 2 || opts.Concurrency != 8 {
		t.Fatalf("repeat/concurrency = %d/%d", opts.Repeat, opts.Concurrency)
	}
	if !opts.MaxCostUSD.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("max cost = %s", opts.MaxCostUSD)
	}
	if !opts.SplitSet || !opts.ModelSet || !opts.PromptSet || !opts.RepeatSet || !opts.ConcurrencySet {
		t.Fatalf("set flags not recorded: %+v", opts)
	}
}

func TestValidateExplicitZeroRepeatAndConcurrency(t *testing.T) {
	for _, args := range [][]string{
		{"--repeat", "0"},
		{"--concurrency", "0"},
		{"--suite", ""},
	} {
		opts, err := ParseOptions(args, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("ParseOptions(%v): %v", args, err)
		}
		ApplyDefaults(&opts)
		if err := Validate(opts); !IsUsage(err) {
			t.Fatalf("%v: Validate = %v, want usage error", args, err)
		}
	}
}

func TestParseOptionsFlagErrors(t *testing.T) {
	if _, err := ParseOptions([]string{"--nope"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown flag: want error")
	}
	if _, err := ParseOptions([]string{"extra"}, &bytes.Buffer{}); !IsUsage(err) {
		t.Fatalf("positional arg error = %v, want usage", err)
	}
	_, err := ParseOptions([]string{"-h"}, &bytes.Buffer{})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h error = %v, want flag.ErrHelp", err)
	}
}

func TestApplyResumeOptionsUsesStoredValues(t *testing.T) {
	opts := Options{Resume: "11111111-1111-4111-8111-111111111111"}
	ApplyDefaults(&opts)
	if err := Validate(opts); err != nil {
		t.Fatalf("Validate base: %v", err)
	}
	stored := runRow{
		Suite:         "agent",
		Split:         dataset.Test,
		Model:         "deepseek-v4.1-flash",
		PromptVersion: "v1",
		Seed:          2,
		LLM:           LLMCassette,
		LLMKnown:      true,
		MaxCostUSD:    decimal.RequireFromString("5"),
		CapKnown:      true,
	}
	if err := applyResumeOptions(&opts, stored); err != nil {
		t.Fatalf("applyResumeOptions: %v", err)
	}
	if opts.Split != dataset.Test || opts.Seed == nil || *opts.Seed != 2 {
		t.Fatalf("stored values not applied: %+v", opts)
	}
	if opts.LLM != LLMCassette || !opts.MaxCostUSD.Equal(stored.MaxCostUSD) {
		t.Fatalf("stored llm/cap not applied: llm=%q cap=%s", opts.LLM, opts.MaxCostUSD)
	}
}

func TestApplyResumeOptionsRejectsConflicts(t *testing.T) {
	opts := Options{
		Resume:   "11111111-1111-4111-8111-111111111111",
		Split:    dataset.Dev,
		SplitSet: true,
		Seed:     ptrInt64(1),
	}
	ApplyDefaults(&opts)
	stored := runRow{Suite: "agent", Split: dataset.Test, Model: "deepseek-v4.1-flash",
		PromptVersion: "v1", Seed: 2, LLM: LLMCassette, LLMKnown: true,
		MaxCostUSD: decimal.RequireFromString("5"), CapKnown: true}
	if err := applyResumeOptions(&opts, stored); !IsUsage(err) {
		t.Fatalf("split conflict = %v, want usage", err)
	}
}

func ptrInt64(value int64) *int64 { return &value }

func TestCapLabel(t *testing.T) {
	if got := capLabel(decimal.Zero); got != "unlimited" {
		t.Fatalf("cap label zero = %q", got)
	}
	if got := capLabel(decimal.RequireFromString("1.50")); got != "1.5" {
		t.Fatalf("cap label = %q", got)
	}
}

func TestRolePoolSize(t *testing.T) {
	cases := map[int]int32{-1: 4, 1: 4, 2: 4, 4: 6, 14: 16, 32: 16}
	for concurrency, want := range cases {
		if got := rolePoolSize(concurrency); got != want {
			t.Fatalf("rolePoolSize(%d) = %d, want %d", concurrency, got, want)
		}
	}
}

func TestValidateLiveGuardsNeverLeakKey(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	opts := Options{}
	ApplyDefaults(&opts)
	opts.LLM = LLMLive
	opts.MaxCostUSD = decimal.RequireFromString("1")
	if err := Validate(opts); !IsUsage(err) {
		t.Fatalf("live without key = %v, want usage", err)
	}

	const secret = "oc_sk_eval_secret_value"
	t.Setenv("OPENCODE_API_KEY", secret)
	opts.MaxCostUSD = decimal.Zero
	err := Validate(opts)
	if !IsUsage(err) {
		t.Fatalf("live zero cap = %v, want usage", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("validation error leaked OPENCODE_API_KEY")
	}

	opts.MaxCostUSD = decimal.RequireFromString("1")
	if err := Validate(opts); err != nil {
		t.Fatalf("live with key and cap: %v", err)
	}
}

func TestSuiteSelectionMatrix(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantSuite string
		wantE2E   bool
		wantUsage bool
	}{
		{name: "default agent", wantSuite: SuiteAgent},
		{name: "agent explicit", args: []string{"--suite", "agent"}, wantSuite: SuiteAgent},
		{name: "e2e flag", args: []string{"--e2e"}, wantSuite: SuiteE2E, wantE2E: true},
		{name: "suite e2e", args: []string{"--suite", "e2e"}, wantSuite: SuiteE2E, wantE2E: true},
		{name: "both selectors agree", args: []string{"--e2e", "--suite", "e2e"},
			wantSuite: SuiteE2E, wantE2E: true},
		{name: "e2e plus agent suite", args: []string{"--e2e", "--suite", "agent"},
			wantSuite: SuiteE2E, wantE2E: true},
		{name: "incompatible suite", args: []string{"--e2e", "--suite", "matcher"}, wantUsage: true},
		{name: "e2e false with suite e2e", args: []string{"--e2e=false", "--suite", "e2e"}, wantUsage: true},
		{name: "repeat with e2e flag", args: []string{"--e2e", "--repeat", "2"}, wantUsage: true},
		{name: "repeat with suite e2e", args: []string{"--suite", "e2e", "--repeat", "2"}, wantUsage: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := ParseOptions(tc.args, &bytes.Buffer{})
			if err != nil {
				t.Fatalf("ParseOptions(%v): %v", tc.args, err)
			}
			ApplyDefaults(&opts)
			if err := Validate(opts); tc.wantUsage {
				if !IsUsage(err) {
					t.Fatalf("Validate(%v) = %v, want usage error", tc.args, err)
				}
				return
			} else if err != nil {
				t.Fatalf("Validate(%v): %v", tc.args, err)
			}
			if err := SelectSuite(&opts); err != nil {
				t.Fatalf("SelectSuite(%v): %v", tc.args, err)
			}
			if opts.Suite != tc.wantSuite || opts.E2E != tc.wantE2E {
				t.Fatalf("suite/e2e = %q/%t, want %q/%t", opts.Suite, opts.E2E,
					tc.wantSuite, tc.wantE2E)
			}
		})
	}
}

func TestBuildRunConfigE2EMarker(t *testing.T) {
	base := Options{
		Suite:       SuiteE2E,
		Split:       dataset.Dev,
		Repeat:      1,
		Concurrency: 4,
		LLM:         LLMCassette,
		E2E:         true,
	}
	raw, err := buildRunConfig(base, 0)
	if err != nil {
		t.Fatalf("buildRunConfig e2e: %v", err)
	}
	var e2eDocument map[string]any
	if err := json.Unmarshal(raw, &e2eDocument); err != nil {
		t.Fatalf("unmarshal e2e config: %v", err)
	}
	if flagValue, ok := e2eDocument["e2e"].(bool); !ok || !flagValue {
		t.Fatalf("e2e config marker = %#v, want true", e2eDocument["e2e"])
	}
	evalSection, _ := e2eDocument["eval"].(map[string]any)
	if evalSection["suite"] != SuiteE2E {
		t.Fatalf("e2e config suite = %#v, want %q", evalSection["suite"], SuiteE2E)
	}

	// A programmatic Suite=e2e without E2E still records the canonical marker.
	derived := base
	derived.E2E = false
	raw, err = buildRunConfig(derived, 0)
	if err != nil {
		t.Fatalf("buildRunConfig derived: %v", err)
	}
	var derivedDocument map[string]any
	if err := json.Unmarshal(raw, &derivedDocument); err != nil {
		t.Fatalf("unmarshal derived config: %v", err)
	}
	if flagValue, ok := derivedDocument["e2e"].(bool); !ok || !flagValue {
		t.Fatalf("derived e2e config marker = %#v, want true", derivedDocument["e2e"])
	}

	agent := base
	agent.Suite = SuiteAgent
	agent.E2E = false
	raw, err = buildRunConfig(agent, 0)
	if err != nil {
		t.Fatalf("buildRunConfig agent: %v", err)
	}
	var agentDocument map[string]any
	if err := json.Unmarshal(raw, &agentDocument); err != nil {
		t.Fatalf("unmarshal agent config: %v", err)
	}
	if _, present := agentDocument["e2e"]; present {
		t.Fatalf("agent config unexpectedly carries e2e: %#v", agentDocument["e2e"])
	}
}
