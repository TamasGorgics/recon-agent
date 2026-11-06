package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/shopspring/decimal"
)

const (
	defaultHTTPTimeout = 90 * time.Second

	// maxUsageTokens is a sanity bound far above any real context window. It
	// keeps provider-supplied token counts non-negative and prevents overflow
	// when prompt and completion counts are summed.
	maxUsageTokens int64 = 1 << 40
)

// maxUsageCost bounds a single provider-reported monetary value. Real calls are
// orders of magnitude below this; a larger value is a malformed response.
var maxUsageCost = decimal.NewFromInt(1_000_000)

var sessionPattern = regexp.MustCompile(`^ses_[A-Za-z0-9_-]+$`)

// OpenCodeOptions configures the OpenCode Console Inference adapter.
type OpenCodeOptions struct {
	BaseURL      string
	APIKey       string
	HTTPClient   *http.Client
	Clock        clock.Clock
	Logger       *slog.Logger
	Registry     *Registry
	UserAgent    string
	ClientHeader string
}

// OpenCode is a non-streaming OpenAI-compatible Chat Completions client.
type OpenCode struct {
	baseURL      string
	apiKey       string
	httpClient   *http.Client
	clock        clock.Clock
	logger       *slog.Logger
	registry     *Registry
	userAgent    string
	clientHeader string
}

// NewOpenCode builds the adapter. An empty API key or invalid base URL returns
// ErrNotConfigured without echoing the key.
func NewOpenCode(opts OpenCodeOptions) (*OpenCode, error) {
	key := strings.TrimSpace(opts.APIKey)
	if key == "" {
		return nil, fmt.Errorf("%w: OPENCODE_API_KEY is empty", ErrNotConfigured)
	}
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("%w: base URL must be an absolute http(s) URL", ErrNotConfigured)
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	timeSource := opts.Clock
	if timeSource == nil {
		timeSource = clock.Real{}
	}
	registry := opts.Registry
	if registry == nil {
		registry = DefaultRegistry()
	}
	userAgent := strings.TrimSpace(opts.UserAgent)
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	clientHeader := strings.TrimSpace(opts.ClientHeader)
	if clientHeader == "" {
		clientHeader = DefaultUserAgent
	}
	return &OpenCode{
		baseURL:      base,
		apiKey:       key,
		httpClient:   httpClient,
		clock:        timeSource,
		logger:       opts.Logger,
		registry:     registry,
		userAgent:    userAgent,
		clientHeader: clientHeader,
	}, nil
}

type wireRequest struct {
	Model       string           `json:"model"`
	Messages    []wireRequestMsg `json:"messages"`
	Tools       []wireTool       `json:"tools,omitempty"`
	ToolChoice  any              `json:"tool_choice,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"` //floatlint:allow provider temperature, not money
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Stream      bool             `json:"stream"`
}

type wireRequestMsg struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Name       string            `json:"name,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolCalls  []wireRequestCall `json:"tool_calls,omitempty"`
}

type wireRequestCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function wireRequestFunc `json:"function"`
}

type wireRequestFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

type wireToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireToolChoiceFunc struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// Complete performs one non-streaming Chat Completions request.
func (o *OpenCode) Complete(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, newRequestError(ProviderOpenCodeGo, APIID(req.Model), err.Error())
	}
	if !sessionPattern.MatchString(req.SessionID) {
		return Response{}, newRequestError(ProviderOpenCodeGo, APIID(req.Model), "x-opencode-session must match ^ses_[A-Za-z0-9_-]+$")
	}
	model, err := o.registry.Get(req.Model)
	if err != nil {
		return Response{}, newUnknownModelError(req.Model)
	}

	payload, err := o.buildBody(req)
	if err != nil {
		return Response{}, newRequestError(ProviderOpenCodeGo, model.ID, err.Error())
	}

	start := o.clock.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, newRequestError(ProviderOpenCodeGo, model.ID, "build request: "+err.Error())
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", o.userAgent)
	httpReq.Header.Set("x-opencode-client", o.clientHeader)
	httpReq.Header.Set("x-opencode-session", req.SessionID)

	resp, err := o.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, o.transportError(ctx, model.ID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, canceledError(model.ID, ctxErr)
		}
		return Response{}, &ProviderError{
			Provider:  ProviderOpenCodeGo,
			Model:     model.ID,
			Kind:      KindUnavailable,
			Retryable: true,
			Message:   Redact("read response: " + readErr.Error()),
			Attempts:  1,
			Cause:     readErr,
		}
	}

	requestID := firstNonEmpty(
		resp.Header.Get("x-request-id"),
		resp.Header.Get("request-id"),
		resp.Header.Get("x-opencode-request-id"),
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		providerErr := classifyProviderError(ProviderOpenCodeGo, model.ID, resp.StatusCode, body)
		if retryAfter, ok := parseRetryAfterHeader(resp.Header.Get("Retry-After"), o.clock.Now()); ok {
			providerErr.RetryAfter = retryAfter
		}
		if requestID != "" {
			providerErr.RequestID = requestID
		}
		o.logFailure(model.ID, resp.StatusCode, providerErr)
		return Response{}, providerErr
	}

	response, err := o.parseResponse(model, req, start, body, requestID)
	if err != nil {
		providerErr := newInvalidResponseError(ProviderOpenCodeGo, model.ID, err.Error(), body)
		providerErr.StatusCode = resp.StatusCode
		if requestID != "" {
			providerErr.RequestID = requestID
		}
		o.logFailure(model.ID, resp.StatusCode, providerErr)
		return Response{}, providerErr
	}
	o.logSuccess(response)
	return response, nil
}

func (o *OpenCode) buildBody(req Request) ([]byte, error) {
	messages := make([]wireRequestMsg, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.System) != "" {
		messages = append(messages, wireRequestMsg{Role: string(RoleSystem), Content: req.System})
	}
	for _, message := range req.Messages {
		wire := wireRequestMsg{
			Role:       string(message.Role),
			Content:    message.Content,
			Name:       message.Name,
			ToolCallID: message.ToolCallID,
		}
		for _, call := range message.ToolCalls {
			args := string(call.Arguments)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			wire.ToolCalls = append(wire.ToolCalls, wireRequestCall{
				ID:   call.ID,
				Type: "function",
				Function: wireRequestFunc{
					Name:      call.Name,
					Arguments: args,
				},
			})
		}
		messages = append(messages, wire)
	}

	wire := wireRequest{
		Model:       APIID(req.Model),
		Messages:    messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		// stream is always false for Complete and is serialized explicitly.
		Stream: false,
	}
	for _, tool := range req.Tools {
		wire.Tools = append(wire.Tools, wireTool{
			Type:     "function",
			Function: wireToolDef(tool),
		})
	}
	if choice := effectiveToolChoice(req); choice != nil {
		switch choice.Mode {
		case ToolChoiceAuto:
			wire.ToolChoice = string(ToolChoiceAuto)
		case ToolChoiceNone:
			wire.ToolChoice = string(ToolChoiceNone)
		case ToolChoiceRequired:
			wire.ToolChoice = string(ToolChoiceRequired)
		case ToolChoiceTool:
			object := wireToolChoiceFunc{Type: "function"}
			object.Function.Name = choice.Name
			wire.ToolChoice = object
		}
	}
	return json.Marshal(wire)
}

func effectiveToolChoice(req Request) *ToolChoice {
	if len(req.Tools) == 0 {
		return nil
	}
	if req.ToolChoice != nil {
		return req.ToolChoice
	}
	return &ToolChoice{Mode: ToolChoiceAuto}
}

type wireResponse struct {
	ID        string       `json:"id"`
	Model     string       `json:"model"`
	Choices   []wireChoice `json:"choices"`
	Usage     *wireUsage   `json:"usage"`
	RequestID string       `json:"request_id"`
}

type wireChoice struct {
	Message      wireResponseMsg `json:"message"`
	FinishReason *string         `json:"finish_reason"`
}

type wireResponseMsg struct {
	Role             string             `json:"role"`
	Content          json.RawMessage    `json:"content"`
	ReasoningContent json.RawMessage    `json:"reasoning_content"`
	ToolCalls        []wireResponseCall `json:"tool_calls"`
}

type wireResponseCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function wireResponseFunction `json:"function"`
}

type wireResponseFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireUsage struct {
	PromptTokens        *int            `json:"prompt_tokens"`
	CompletionTokens    *int            `json:"completion_tokens"`
	TotalTokens         *int            `json:"total_tokens"`
	Cost                json.RawMessage `json:"cost"`
	PromptTokensDetails struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OpenCode *wireOpenCodeUsage `json:"opencode"`
}

type wireOpenCodeUsage struct {
	BillingSource string          `json:"billing_source"`
	ListCost      json.RawMessage `json:"list_cost"`
	Allowance     json.RawMessage `json:"allowance"`
	Tokens        *wireTokenUsage `json:"tokens"`
}

type wireTokenUsage struct {
	Input        *int `json:"input"`
	Output       *int `json:"output"`
	Reasoning    *int `json:"reasoning"`
	CacheRead    *int `json:"cache_read"`
	CacheWrite5m *int `json:"cache_write_5m"`
	CacheWrite1h *int `json:"cache_write_1h"`
}

func (o *OpenCode) parseResponse(model *Model, req Request, start time.Time, body []byte, headerRequestID string) (Response, error) {
	var wire wireResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	if len(wire.Choices) == 0 {
		return Response{}, errors.New("response has no choices")
	}
	choice := wire.Choices[0]

	content, err := rawString(wire.Choices[0].Message.Content)
	if err != nil {
		return Response{}, fmt.Errorf("content: %w", err)
	}
	reasoning, err := rawString(wire.Choices[0].Message.ReasoningContent)
	if err != nil {
		return Response{}, fmt.Errorf("reasoning_content: %w", err)
	}

	message := Message{
		Role:             RoleAssistant,
		Content:          content,
		ReasoningContent: reasoning,
	}
	for _, call := range choice.Message.ToolCalls {
		args, err := rawString(call.Function.Arguments)
		if err != nil {
			return Response{}, fmt.Errorf("tool call %q arguments: %w", call.ID, err)
		}
		message.ToolCalls = append(message.ToolCalls, ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: json.RawMessage(args),
		})
	}

	var rawUsage json.RawMessage
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err == nil {
		rawUsage = append(json.RawMessage(nil), envelope["usage"]...)
	}

	usage, err := o.mapUsage(model, wire.Usage, start)
	if err != nil {
		return Response{}, err
	}
	usage.Raw = rawUsage

	actualModel := strings.TrimSpace(wire.Model)
	if actualModel == "" {
		actualModel = model.ID
	}
	requestedModel := APIID(req.Model)
	response := Response{
		ID:             wire.ID,
		Provider:       ProviderOpenCodeGo,
		RequestedModel: requestedModel,
		ActualModel:    actualModel,
		Message:        message,
		StopReason:     mapStopReason(choice.FinishReason),
		Usage:          usage,
		Raw:            append(json.RawMessage(nil), body...),
		RequestID:      firstNonEmpty(headerRequestID, wire.RequestID),
		Latency:        o.clock.Since(start),
		Attempts:       1,
		FallbackUsed:   actualModel != requestedModel,
	}
	return response, nil
}

func (o *OpenCode) mapUsage(model *Model, wire *wireUsage, at time.Time) (Usage, error) {
	usage := Usage{}
	if wire == nil {
		tokens := Tokens{}
		usage.ComputedCost = model.ComputeCost(tokens, at)
		usage.CostUSD = usage.ComputedCost
		usage.CostSource = CostSourcePricingTable
		return usage, nil
	}
	for _, field := range []struct {
		name  string
		value *int
	}{
		{"usage.prompt_tokens", wire.PromptTokens},
		{"usage.completion_tokens", wire.CompletionTokens},
		{"usage.total_tokens", wire.TotalTokens},
		{"usage.prompt_tokens_details.cached_tokens", wire.PromptTokensDetails.CachedTokens},
		{"usage.completion_tokens_details.reasoning_tokens", wire.CompletionTokensDetails.ReasoningTokens},
	} {
		if err := validateTokenCount(field.name, field.value); err != nil {
			return Usage{}, err
		}
	}
	if wire.OpenCode != nil && wire.OpenCode.Tokens != nil {
		tokens := wire.OpenCode.Tokens
		for _, field := range []struct {
			name  string
			value *int
		}{
			{"usage.opencode.tokens.input", tokens.Input},
			{"usage.opencode.tokens.output", tokens.Output},
			{"usage.opencode.tokens.reasoning", tokens.Reasoning},
			{"usage.opencode.tokens.cache_read", tokens.CacheRead},
			{"usage.opencode.tokens.cache_write_5m", tokens.CacheWrite5m},
			{"usage.opencode.tokens.cache_write_1h", tokens.CacheWrite1h},
		} {
			if err := validateTokenCount(field.name, field.value); err != nil {
				return Usage{}, err
			}
		}
	}

	usage.PromptTokens = intValue(wire.PromptTokens)
	usage.CompletionTokens = intValue(wire.CompletionTokens)
	if wire.TotalTokens != nil {
		total := int64(usage.PromptTokens) + int64(usage.CompletionTokens)
		if int64(*wire.TotalTokens) != total {
			return Usage{}, fmt.Errorf("usage.total_tokens %d does not equal prompt+completion %d", *wire.TotalTokens, total)
		}
		usage.TotalTokens = *wire.TotalTokens
	} else {
		usage.TotalTokens = int(int64(usage.PromptTokens) + int64(usage.CompletionTokens))
	}
	usage.CachedTokens = intValue(wire.PromptTokensDetails.CachedTokens)
	usage.ReasoningTokens = intValue(wire.CompletionTokensDetails.ReasoningTokens)

	if wire.OpenCode != nil && wire.OpenCode.Tokens != nil {
		tokens := wire.OpenCode.Tokens
		usage.InputTokens = intValue(tokens.Input)
		usage.OutputTokens = intValue(tokens.Output)
		usage.ReasoningTokens = intValue(tokens.Reasoning)
		usage.CacheReadTokens = intValue(tokens.CacheRead)
		usage.CacheWrite5mTokens = intValue(tokens.CacheWrite5m)
		usage.CacheWrite1hTokens = intValue(tokens.CacheWrite1h)
		if usage.CachedTokens == 0 {
			usage.CachedTokens = usage.CacheReadTokens
		}
	} else {
		usage.CacheReadTokens = usage.CachedTokens
		usage.InputTokens = maxInt(usage.PromptTokens-usage.CacheReadTokens, 0)
		usage.OutputTokens = usage.CompletionTokens
	}

	apiCost, _, err := parseDecimalJSON(wire.Cost)
	if err != nil {
		return Usage{}, fmt.Errorf("usage.cost: %w", err)
	}
	usage.APICost = apiCost

	if wire.OpenCode != nil {
		usage.BillingSource = wire.OpenCode.BillingSource
		listCost, reported, err := parseDecimalJSON(wire.OpenCode.ListCost)
		if err != nil {
			return Usage{}, fmt.Errorf("usage.opencode.list_cost: %w", err)
		}
		usage.ListCost = listCost
		usage.ListCostReported = reported
		allowance, allowanceReported, err := parseDecimalJSON(wire.OpenCode.Allowance)
		if err != nil {
			return Usage{}, fmt.Errorf("usage.opencode.allowance: %w", err)
		}
		usage.Allowance = allowance
		usage.AllowanceReported = allowanceReported
	}

	tokens := Tokens{
		Input:        usage.InputTokens,
		Output:       usage.OutputTokens,
		CacheRead:    usage.CacheReadTokens,
		CacheWrite5m: usage.CacheWrite5mTokens,
		CacheWrite1h: usage.CacheWrite1hTokens,
	}
	usage.ComputedCost = model.ComputeCost(tokens, at)
	if usage.ListCostReported {
		usage.CostUSD = usage.ListCost
		usage.CostSource = CostSourceAPIListCost
		diff := usage.ComputedCost.Sub(usage.ListCost).Abs()
		threshold := decimal.New(1, -6)
		if percentage := usage.ListCost.Mul(decimal.New(5, -3)); percentage.GreaterThan(threshold) {
			threshold = percentage
		}
		usage.CostMismatch = diff.GreaterThan(threshold)
	} else {
		usage.CostUSD = usage.ComputedCost
		usage.CostSource = CostSourcePricingTable
	}
	for _, amount := range []struct {
		name  string
		value decimal.Decimal
	}{
		{"usage.cost", usage.APICost},
		{"usage.opencode.list_cost", usage.ListCost},
		{"usage.opencode.allowance", usage.Allowance},
		{"usage.computed_cost", usage.ComputedCost},
		{"usage.cost_usd", usage.CostUSD},
	} {
		if err := validateUsageCost(amount.name, amount.value); err != nil {
			return Usage{}, err
		}
	}
	return usage, nil
}

func validateTokenCount(name string, value *int) error {
	if value == nil {
		return nil
	}
	if *value < 0 {
		return fmt.Errorf("%s is negative: %d", name, *value)
	}
	if int64(*value) > maxUsageTokens {
		return fmt.Errorf("%s exceeds maximum %d", name, maxUsageTokens)
	}
	return nil
}

func validateUsageCost(name string, value decimal.Decimal) error {
	if value.IsNegative() {
		return fmt.Errorf("%s is negative: %s", name, value)
	}
	if value.GreaterThan(maxUsageCost) {
		return fmt.Errorf("%s exceeds maximum %s", name, maxUsageCost)
	}
	return nil
}

func (o *OpenCode) transportError(ctx context.Context, model string, err error) *ProviderError {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return canceledError(model, ctxErr)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &ProviderError{
			Provider:  ProviderOpenCodeGo,
			Model:     model,
			Kind:      KindTimeout,
			Retryable: true,
			Message:   Redact("request timeout: " + err.Error()),
			Attempts:  1,
			Cause:     err,
		}
	}
	return &ProviderError{
		Provider:  ProviderOpenCodeGo,
		Model:     model,
		Kind:      KindUnavailable,
		Retryable: true,
		Message:   Redact("request failed: " + err.Error()),
		Attempts:  1,
		Cause:     err,
	}
}

func canceledError(model string, cause error) *ProviderError {
	return &ProviderError{
		Provider:  ProviderOpenCodeGo,
		Model:     model,
		Kind:      KindCanceled,
		Retryable: false,
		Message:   Redact(cause.Error()),
		Attempts:  1,
		Cause:     cause,
	}
}

func (o *OpenCode) logSuccess(response Response) {
	if o.logger == nil {
		return
	}
	o.logger.Info("llm completion",
		slog.String("provider", Redact(response.Provider)),
		slog.String("model", Redact(response.ActualModel)),
		slog.Int("prompt_tokens", response.Usage.PromptTokens),
		slog.Int("completion_tokens", response.Usage.CompletionTokens),
		slog.Int("total_tokens", response.Usage.TotalTokens),
		slog.String("cost_usd", response.Usage.CostUSD.String()),
		slog.String("request_id", Redact(response.RequestID)),
		slog.Int64("duration_ms", response.Latency.Milliseconds()),
	)
}

func (o *OpenCode) logFailure(model string, status int, err *ProviderError) {
	if o.logger == nil {
		return
	}
	o.logger.Warn("llm request failed",
		slog.String("provider", ProviderOpenCodeGo),
		slog.String("model", Redact(model)),
		slog.Int("status", status),
		slog.String("kind", string(err.Kind)),
		slog.Bool("retryable", err.Retryable),
		slog.String("request_id", Redact(err.RequestID)),
	)
}

func rawString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func parseDecimalJSON(raw json.RawMessage) (decimal.Decimal, bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return decimal.Zero, false, nil
	}
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, `"`) {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return decimal.Zero, false, err
		}
		text = value
	}
	value, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Zero, false, err
	}
	return value, true, nil
}

func mapStopReason(reason *string) StopReason {
	if reason == nil {
		return StopUnknown
	}
	switch strings.TrimSpace(*reason) {
	case string(StopStop):
		return StopStop
	case string(StopToolCalls):
		return StopToolCalls
	case string(StopLength):
		return StopLength
	case string(StopContentFilter):
		return StopContentFilter
	default:
		return StopUnknown
	}
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
