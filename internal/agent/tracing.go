package agent

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"

	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

const traceCostCurrency = "USD"

// caseRunFinalAttributes builds the deferred attributes for the root span.
// Outcome is absent until the loop reaches a terminal result; tokens and cost
// are always recorded once the finalizer runs, including zero values.
func caseRunFinalAttributes(result Result, usage budgetUsage) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 7)
	if result.Outcome.Valid() {
		attrs = append(attrs, attribute.String(telemetry.AttrRunOutcome, string(result.Outcome)))
	}
	if result.ProposalID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrProposalID, result.ProposalID))
	}
	attrs = append(attrs,
		attribute.Int64(telemetry.AttrTokensInput, usage.TokensIn),
		attribute.Int64(telemetry.AttrTokensOutput, usage.TokensOut),
	)
	if micro, ok := microUSD(usage.Cost); ok {
		attrs = append(attrs,
			attribute.Int64(telemetry.AttrCostMicroUSD, micro),
			attribute.String(telemetry.AttrCostCurrency, traceCostCurrency),
		)
	}
	return attrs
}

// llmCallFinalAttributes builds the success attributes for one completion.
// Only hashes, counts, model identifiers, and cost are recorded; prompts and
// messages never reach a span.
func llmCallFinalAttributes(actualModel string, response llm.Response, responseBody []byte) []attribute.KeyValue {
	inputTokens, outputTokens := traceUsageTokens(response.Usage)
	attrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrResponseModel, actualModel),
		attribute.Int64(telemetry.AttrUsageInputTokens, inputTokens),
		attribute.Int64(telemetry.AttrUsageOutputTokens, outputTokens),
		attribute.String(telemetry.AttrResponseHash, hashBytes(responseBody)),
	}
	if micro, ok := microUSD(traceUsageCost(response.Usage)); ok {
		attrs = append(attrs,
			attribute.Int64(telemetry.AttrCostMicroUSD, micro),
			attribute.String(telemetry.AttrCostCurrency, traceCostCurrency),
		)
	}
	return attrs
}

// traceUsageTokens reports the input/output token counts used on llm.call
// spans. It uses the same source order and clamping as budgetUsage.addUsage
// (PromptTokens/CompletionTokens first, then the normalized Input/Output
// fields), so the sum across llm.call spans equals case.run's
// recon.tokens.* attributes and the persisted agent_runs.tokens_* totals. In
// particular, cached prompt tokens stay included in the input count exactly as
// they are in the authoritative run accounting.
func traceUsageTokens(usage llm.Usage) (input, output int64) {
	usage, _ = sanitizeUsage(usage)
	input = int64(usage.PromptTokens)
	if input == 0 {
		input = int64(usage.InputTokens)
	}
	output = int64(usage.CompletionTokens)
	if output == 0 {
		output = int64(usage.OutputTokens)
	}
	return input, output
}

// traceUsageCost uses the same cost-source order as budget accounting:
// CostUSD, then ListCost, then ComputedCost.
func traceUsageCost(usage llm.Usage) decimal.Decimal {
	usage, _ = sanitizeUsage(usage)
	cost := usage.CostUSD
	if cost.IsZero() {
		cost = usage.ListCost
	}
	if cost.IsZero() {
		cost = usage.ComputedCost
	}
	return cost
}

// microUSD converts a non-negative USD decimal to integer micro-USD with
// half-even rounding and reports whether the value fits in int64.
func microUSD(cost decimal.Decimal) (int64, bool) {
	if cost.IsNegative() {
		return 0, false
	}
	rounded := cost.Shift(6).RoundBank(0)
	integer := rounded.BigInt()
	if !integer.IsInt64() {
		return 0, false
	}
	return integer.Int64(), true
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
