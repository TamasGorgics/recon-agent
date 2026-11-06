package agent

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/llm"
)

func TestBudgetLimits(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base := budgetLimits{
		MaxSteps:       2,
		MaxToolCalls:   3,
		MaxTotalTokens: 100,
		MaxCost:        decimal.RequireFromString("0.50"),
		MaxWallClock:   5 * time.Minute,
		StartedAt:      start,
	}
	cases := []struct {
		name   string
		limits *budgetLimits
		usage  budgetUsage
		now    time.Time
		want   string
	}{
		{
			name:  "within budget",
			usage: budgetUsage{Steps: 1, ToolCalls: 2, TokensIn: 40, TokensOut: 40, Cost: decimal.RequireFromString("0.10")},
			now:   start.Add(time.Minute),
			want:  "",
		},
		{
			name:  "steps at cap",
			usage: budgetUsage{Steps: 2},
			now:   start,
			want:  reasonMaxSteps,
		},
		{
			name:  "steps over cap",
			usage: budgetUsage{Steps: 3},
			now:   start,
			want:  reasonMaxSteps,
		},
		{
			name:  "tool calls at cap",
			usage: budgetUsage{ToolCalls: 3},
			now:   start,
			want:  reasonMaxToolCalls,
		},
		{
			name:  "tokens at cap",
			usage: budgetUsage{TokensIn: 60, TokensOut: 40},
			now:   start,
			want:  reasonMaxTokens,
		},
		{
			name:  "cost below cap",
			usage: budgetUsage{Cost: decimal.RequireFromString("0.49")},
			now:   start,
			want:  "",
		},
		{
			name:  "cost at cap",
			usage: budgetUsage{Cost: decimal.RequireFromString("0.50")},
			now:   start,
			want:  reasonMaxCost,
		},
		{
			name: "zero cost cap at zero usage",
			limits: func() *budgetLimits {
				limits := base
				limits.MaxCost = decimal.Zero
				return &limits
			}(),
			usage: budgetUsage{},
			now:   start,
			want:  "",
		},
		{
			name: "zero cost cap with positive usage",
			limits: func() *budgetLimits {
				limits := base
				limits.MaxCost = decimal.Zero
				return &limits
			}(),
			usage: budgetUsage{Cost: decimal.RequireFromString("0.000001")},
			now:   start,
			want:  reasonMaxCost,
		},
		{
			name:  "wall clock at cap",
			usage: budgetUsage{},
			now:   start.Add(5 * time.Minute),
			want:  reasonMaxWallClock,
		},
		{
			name:   "zero non-cost limits never exhaust",
			limits: &budgetLimits{StartedAt: start},
			usage:  budgetUsage{Steps: 100, ToolCalls: 100, TokensIn: 1_000_000},
			now:    start.Add(24 * time.Hour),
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := base
			if tc.limits != nil {
				limits = *tc.limits
			}
			got := ""
			switch {
			case limits.stepsExhausted(tc.usage):
				got = reasonMaxSteps
			case limits.toolCallsExhausted(tc.usage):
				got = reasonMaxToolCalls
			case limits.tokensExhausted(tc.usage):
				got = reasonMaxTokens
			case limits.costExhausted(tc.usage):
				got = reasonMaxCost
			case limits.wallClockExhausted(tc.now):
				got = reasonMaxWallClock
			}
			if got != tc.want {
				t.Fatalf("exhaustion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBudgetUsageAddUsage(t *testing.T) {
	cases := []struct {
		name       string
		usage      llm.Usage
		wantIn     int64
		wantOut    int64
		wantCost   string
		allowance  string
		allowanceN string
	}{
		{
			name:     "cost usd wins",
			usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 5, CostUSD: decimal.RequireFromString("0.07"), ListCost: decimal.RequireFromString("0.09"), ComputedCost: decimal.RequireFromString("0.11")},
			wantIn:   10,
			wantOut:  5,
			wantCost: "0.07",
		},
		{
			name:     "list cost fallback",
			usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 5, ListCost: decimal.RequireFromString("0.09"), ComputedCost: decimal.RequireFromString("0.11")},
			wantIn:   10,
			wantOut:  5,
			wantCost: "0.09",
		},
		{
			name:     "computed cost fallback",
			usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 5, ComputedCost: decimal.RequireFromString("0.11")},
			wantIn:   10,
			wantOut:  5,
			wantCost: "0.11",
		},
		{
			name:      "openai token fallback",
			usage:     llm.Usage{InputTokens: 7, OutputTokens: 3, CostUSD: decimal.RequireFromString("0.01")},
			wantIn:    7,
			wantOut:   3,
			wantCost:  "0.01",
			allowance: "9.99",
		},
		{
			name:     "negative usage clamps to zero",
			usage:    llm.Usage{PromptTokens: -10, CompletionTokens: -5, TotalTokens: -15, CostUSD: decimal.RequireFromString("-0.07")},
			wantIn:   0,
			wantOut:  0,
			wantCost: "0",
		},
		{
			name:     "negative prompt falls back to positive input",
			usage:    llm.Usage{PromptTokens: -10, InputTokens: 7, CompletionTokens: -5, OutputTokens: 3, CostUSD: decimal.RequireFromString("0.01")},
			wantIn:   7,
			wantOut:  3,
			wantCost: "0.01",
		},
		{
			name:     "negative cost usd falls back to list cost",
			usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 5, CostUSD: decimal.RequireFromString("-0.07"), ListCost: decimal.RequireFromString("0.09"), ComputedCost: decimal.RequireFromString("0.11")},
			wantIn:   10,
			wantOut:  5,
			wantCost: "0.09",
		},
		{
			name:     "negative list cost falls back to computed cost",
			usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 5, ListCost: decimal.RequireFromString("-0.09"), ComputedCost: decimal.RequireFromString("0.11")},
			wantIn:   10,
			wantOut:  5,
			wantCost: "0.11",
		},
		{
			name:     "all negative costs clamp to zero",
			usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 5, CostUSD: decimal.RequireFromString("-0.07"), ListCost: decimal.RequireFromString("-0.09"), ComputedCost: decimal.RequireFromString("-0.11")},
			wantIn:   10,
			wantOut:  5,
			wantCost: "0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := budgetUsage{Cost: decimal.Zero}
			usage.addUsage(tc.usage)
			if usage.TokensIn != tc.wantIn || usage.TokensOut != tc.wantOut {
				t.Fatalf("tokens = %d/%d, want %d/%d", usage.TokensIn, usage.TokensOut, tc.wantIn, tc.wantOut)
			}
			if usage.Cost.String() != tc.wantCost {
				t.Fatalf("cost = %s, want %s", usage.Cost, tc.wantCost)
			}
		})
	}
}

func TestSanitizeUsage(t *testing.T) {
	negative := llm.Usage{
		PromptTokens:       -1,
		CompletionTokens:   -2,
		TotalTokens:        -3,
		ReasoningTokens:    -4,
		CachedTokens:       -5,
		InputTokens:        -6,
		OutputTokens:       -7,
		CacheReadTokens:    -8,
		CacheWrite5mTokens: -9,
		CacheWrite1hTokens: -10,
		APICost:            decimal.RequireFromString("-0.01"),
		ListCost:           decimal.RequireFromString("-0.02"),
		ComputedCost:       decimal.RequireFromString("-0.03"),
		CostUSD:            decimal.RequireFromString("-0.04"),
		Allowance:          decimal.RequireFromString("-0.05"),
	}
	sanitized, fields := sanitizeUsage(negative)
	wantFields := []string{
		"prompt_tokens", "completion_tokens", "total_tokens", "reasoning_tokens",
		"cached_tokens", "input_tokens", "output_tokens", "cache_read_tokens",
		"cache_write_5m_tokens", "cache_write_1h_tokens", "api_cost", "list_cost",
		"computed_cost", "cost_usd", "allowance",
	}
	if !slices.Equal(fields, wantFields) {
		t.Fatalf("clamped fields = %v, want %v", fields, wantFields)
	}
	if sanitized.PromptTokens != 0 || sanitized.CompletionTokens != 0 ||
		sanitized.TotalTokens != 0 || sanitized.ReasoningTokens != 0 ||
		sanitized.CachedTokens != 0 || sanitized.InputTokens != 0 ||
		sanitized.OutputTokens != 0 || sanitized.CacheReadTokens != 0 ||
		sanitized.CacheWrite5mTokens != 0 || sanitized.CacheWrite1hTokens != 0 {
		t.Fatalf("token counts were not clamped: %+v", sanitized)
	}
	for name, amount := range map[string]decimal.Decimal{
		"api_cost": sanitized.APICost, "list_cost": sanitized.ListCost,
		"computed_cost": sanitized.ComputedCost, "cost_usd": sanitized.CostUSD,
		"allowance": sanitized.Allowance,
	} {
		if amount.IsNegative() || !amount.IsZero() {
			t.Fatalf("%s = %s, want zero", name, amount)
		}
	}

	positive := llm.Usage{PromptTokens: 10, CompletionTokens: 5, CostUSD: decimal.RequireFromString("0.01")}
	unchanged, fields := sanitizeUsage(positive)
	if len(fields) != 0 || !reflect.DeepEqual(unchanged, positive) {
		t.Fatalf("positive usage changed: %+v fields=%v", unchanged, fields)
	}
}

func TestBudgetUsageNonNegative(t *testing.T) {
	usage := budgetUsage{
		TokensIn: -1, TokensOut: -2, Cost: decimal.RequireFromString("-0.01"),
	}
	got := usage.nonNegative()
	if got.TokensIn != 0 || got.TokensOut != 0 || !got.Cost.IsZero() {
		t.Fatalf("nonNegative = %+v, want zero usage", got)
	}
}

func TestNormalizedFlags(t *testing.T) {
	got := normalizedFlags([]string{" b ", "a", "", "a", "b", "  "})
	want := []string{"a", "b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("normalizedFlags = %v, want %v", got, want)
	}
	if empty := normalizedFlags(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("normalizedFlags(nil) = %#v, want non-nil empty", empty)
	}
}

func TestClampDuration(t *testing.T) {
	if got := clampDuration(-time.Second); got != 0 {
		t.Fatalf("negative duration = %d, want 0", got)
	}
	if got := clampDuration(1500 * time.Microsecond); got != 1 {
		t.Fatalf("1500us = %d, want 1", got)
	}
}
