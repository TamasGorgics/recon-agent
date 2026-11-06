package llm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

// Client is the provider-neutral LLM boundary. Complete performs exactly one
// logical completion; retries and fallbacks belong to a wrapper.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Role is a chat message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one provider-neutral chat message.
type Message struct {
	Role             Role       `json:"role"`
	Content          string     `json:"content"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

// ToolDef describes one tool exposed to the model.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is a model-requested tool invocation. Arguments stay raw in the
// provider layer; the tool layer validates them.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolChoiceMode enumerates the supported tool-choice modes.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool"
)

// ToolChoice constrains tool selection for a request.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
}

// Request is one provider-neutral completion request.
type Request struct {
	Model         string
	System        string
	Messages      []Message
	Tools         []ToolDef
	ToolChoice    *ToolChoice
	Temperature   *float64 //floatlint:allow provider temperature, not money
	MaxTokens     int
	SessionID     string
	PromptVersion string
}

// StopReason is the normalized reason a completion stopped.
type StopReason string

const (
	StopStop          StopReason = "stop"
	StopToolCalls     StopReason = "tool_calls"
	StopLength        StopReason = "length"
	StopContentFilter StopReason = "content_filter"
	StopUnknown       StopReason = "unknown"
)

// CostSource identifies where CostUSD came from.
type CostSource string

const (
	CostSourceNone         CostSource = ""
	CostSourceAPIListCost  CostSource = "opencode_list_cost"
	CostSourcePricingTable CostSource = "pricing_table"
)

// Usage is the normalized token and cost accounting for one response. All
// monetary values are exact decimals; no field is a float.
type Usage struct {
	PromptTokens       int             `json:"prompt_tokens"`
	CompletionTokens   int             `json:"completion_tokens"`
	TotalTokens        int             `json:"total_tokens"`
	ReasoningTokens    int             `json:"reasoning_tokens"`
	CachedTokens       int             `json:"cached_tokens"`
	InputTokens        int             `json:"input_tokens"`
	OutputTokens       int             `json:"output_tokens"`
	CacheReadTokens    int             `json:"cache_read_tokens"`
	CacheWrite5mTokens int             `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens int             `json:"cache_write_1h_tokens"`
	APICost            decimal.Decimal `json:"api_cost"`
	ListCost           decimal.Decimal `json:"list_cost"`
	ListCostReported   bool            `json:"list_cost_reported"`
	ComputedCost       decimal.Decimal `json:"computed_cost"`
	CostUSD            decimal.Decimal `json:"cost_usd"`
	Allowance          decimal.Decimal `json:"allowance"`
	AllowanceReported  bool            `json:"allowance_reported"`
	BillingSource      string          `json:"billing_source,omitempty"`
	CostSource         CostSource      `json:"cost_source,omitempty"`
	CostMismatch       bool            `json:"cost_mismatch,omitempty"`
	Raw                json.RawMessage `json:"raw,omitempty"`
}

// Response is one provider-neutral completion response.
type Response struct {
	ID             string          `json:"id"`
	Provider       string          `json:"provider"`
	RequestedModel string          `json:"requested_model"`
	ActualModel    string          `json:"actual_model"`
	Message        Message         `json:"message"`
	StopReason     StopReason      `json:"stop_reason"`
	Usage          Usage           `json:"usage"`
	Raw            json.RawMessage `json:"raw,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	Latency        time.Duration   `json:"latency_ns"`
	Attempts       int             `json:"attempts"`
	FallbackUsed   bool            `json:"fallback_used,omitempty"`
}
