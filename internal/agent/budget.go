package agent

import (
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// Budget exhaustion reasons. They are internal classifications; the persisted
// outcome is always budget_exhausted.
const (
	reasonMaxSteps     = "max_steps"
	reasonMaxToolCalls = "max_tool_calls"
	reasonMaxTokens    = "max_total_tokens"
	reasonMaxCost      = "max_cost_usd"
	reasonMaxWallClock = "max_wall_clock"
)

// budgetLimits is the per-run bounded-loop contract. MaxSteps, MaxToolCalls,
// MaxTotalTokens and MaxWallClock treat zero as no limit; MaxCost treats zero
// as a zero-dollar budget and is exhausted by any positive cost.
type budgetLimits struct {
	MaxSteps       int
	MaxToolCalls   int
	MaxTotalTokens int64
	MaxCost        decimal.Decimal
	MaxWallClock   time.Duration
	StartedAt      time.Time
}

// budgetUsage is the accumulated accounting for one run.
type budgetUsage struct {
	Steps     int
	ToolCalls int
	TokensIn  int64
	TokensOut int64
	Cost      decimal.Decimal
}

func (u budgetUsage) totalTokens() int64 { return u.TokensIn + u.TokensOut }

func (l budgetLimits) stepsExhausted(u budgetUsage) bool {
	return l.MaxSteps > 0 && u.Steps >= l.MaxSteps
}

func (l budgetLimits) toolCallsExhausted(u budgetUsage) bool {
	return l.MaxToolCalls > 0 && u.ToolCalls >= l.MaxToolCalls
}

func (l budgetLimits) tokensExhausted(u budgetUsage) bool {
	return l.MaxTotalTokens > 0 && u.totalTokens() >= l.MaxTotalTokens
}

// costExhausted treats a zero cap as a zero-dollar budget: any positive spend
// exhausts it. Positive caps keep the at-cap contract used by the other limits.
func (l budgetLimits) costExhausted(u budgetUsage) bool {
	if l.MaxCost.IsZero() {
		return u.Cost.IsPositive()
	}
	return u.Cost.GreaterThanOrEqual(l.MaxCost)
}

func (l budgetLimits) wallClockExhausted(now time.Time) bool {
	return l.MaxWallClock > 0 && now.Sub(l.StartedAt) >= l.MaxWallClock
}

// addUsage adds one response's tokens and cost. Negative provider usage is
// clamped to zero before accounting so a malformed completion can never make
// the run un-finalizable. The cost source order is CostUSD, then ListCost,
// then ComputedCost; the allowance is never budgeted.
func (u *budgetUsage) addUsage(usage llm.Usage) {
	usage, _ = sanitizeUsage(usage)
	tokensIn := int64(usage.PromptTokens)
	if tokensIn == 0 {
		tokensIn = int64(usage.InputTokens)
	}
	tokensOut := int64(usage.CompletionTokens)
	if tokensOut == 0 {
		tokensOut = int64(usage.OutputTokens)
	}
	u.TokensIn += tokensIn
	u.TokensOut += tokensOut

	cost := usage.CostUSD
	if cost.IsZero() {
		cost = usage.ListCost
	}
	if cost.IsZero() {
		cost = usage.ComputedCost
	}
	u.Cost = u.Cost.Add(cost)
}

// sanitizeUsage returns a copy of usage with every negative token count and
// monetary amount clamped to zero, plus the names of the clamped fields. Field
// names are safe to log; values never are.
func sanitizeUsage(usage llm.Usage) (llm.Usage, []string) {
	var clamped []string
	clampInt := func(name string, value *int) {
		if *value < 0 {
			*value = 0
			clamped = append(clamped, name)
		}
	}
	clampDecimal := func(name string, value *decimal.Decimal) {
		if value.IsNegative() {
			*value = decimal.Zero
			clamped = append(clamped, name)
		}
	}

	clampInt("prompt_tokens", &usage.PromptTokens)
	clampInt("completion_tokens", &usage.CompletionTokens)
	clampInt("total_tokens", &usage.TotalTokens)
	clampInt("reasoning_tokens", &usage.ReasoningTokens)
	clampInt("cached_tokens", &usage.CachedTokens)
	clampInt("input_tokens", &usage.InputTokens)
	clampInt("output_tokens", &usage.OutputTokens)
	clampInt("cache_read_tokens", &usage.CacheReadTokens)
	clampInt("cache_write_5m_tokens", &usage.CacheWrite5mTokens)
	clampInt("cache_write_1h_tokens", &usage.CacheWrite1hTokens)
	clampDecimal("api_cost", &usage.APICost)
	clampDecimal("list_cost", &usage.ListCost)
	clampDecimal("computed_cost", &usage.ComputedCost)
	clampDecimal("cost_usd", &usage.CostUSD)
	clampDecimal("allowance", &usage.Allowance)
	return usage, clamped
}

// nonNegative returns a copy of the accumulated usage with any negative
// aggregate clamped to zero. It is the final guard before persistence.
func (u budgetUsage) nonNegative() budgetUsage {
	if u.TokensIn < 0 {
		u.TokensIn = 0
	}
	if u.TokensOut < 0 {
		u.TokensOut = 0
	}
	if u.Cost.IsNegative() {
		u.Cost = decimal.Zero
	}
	return u
}

// normalizedFlags trims, drops empty strings, de-duplicates and sorts flag
// codes. The result is never nil.
func normalizedFlags(flags []string) []string {
	seen := make(map[string]struct{}, len(flags))
	out := make([]string, 0, len(flags))
	for _, flag := range flags {
		flag = strings.TrimSpace(flag)
		if flag == "" {
			continue
		}
		if _, ok := seen[flag]; ok {
			continue
		}
		seen[flag] = struct{}{}
		out = append(out, flag)
	}
	sort.Strings(out)
	return out
}
