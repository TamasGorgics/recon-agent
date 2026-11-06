package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const fixedNow = "2026-11-04T12:00:00Z"

func newServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func newTestAdapter(t *testing.T, server *httptest.Server, mutate func(*OpenCodeOptions)) (*OpenCode, *clock.Fake) {
	t.Helper()
	fake := clock.NewFake(mustTime(fixedNow))
	opts := OpenCodeOptions{
		BaseURL:      server.URL,
		APIKey:       "oc_sk_contract_key",
		HTTPClient:   server.Client(),
		Clock:        fake,
		UserAgent:    "recon-agent/test",
		ClientHeader: "recon-agent/test",
	}
	if mutate != nil {
		mutate(&opts)
	}
	adapter, err := NewOpenCode(opts)
	if err != nil {
		t.Fatalf("NewOpenCode: %v", err)
	}
	return adapter, fake
}

func mustTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

func testRequest() Request {
	return Request{
		Model:         "opencode-go/deepseek-v4.1-flash",
		System:        "You are a test agent.",
		Messages:      []Message{{Role: RoleUser, Content: "ping"}},
		Temperature:   temperature(0),
		MaxTokens:     32,
		SessionID:     "ses_contract_1",
		PromptVersion: "v1",
	}
}

func captureHandler(t *testing.T, response string) (http.HandlerFunc, *capturedCall) {
	t.Helper()
	captured := &capturedCall{}
	return func(w http.ResponseWriter, r *http.Request) {
		captured.mu.Lock()
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.header = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		captured.body = body
		captured.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req-contract")
		_, _ = io.WriteString(w, response)
	}, captured
}

type capturedCall struct {
	mu     sync.Mutex
	method string
	path   string
	header http.Header
	body   []byte
}

func (c *capturedCall) snapshot() (string, string, http.Header, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.path, c.header.Clone(), append([]byte(nil), c.body...)
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, body)
	}
	return decoded
}

func textResponse() string {
	return `{
		"id":"chatcmpl-text",
		"object":"chat.completion",
		"model":"deepseek-v4.1-flash",
		"choices":[{"index":0,"message":{"role":"assistant","content":"pong","reasoning_content":"thinking"},"finish_reason":"stop"}],
		"usage":{
			"prompt_tokens":38,
			"completion_tokens":16,
			"total_tokens":54,
			"cost":0,
			"opencode":{"billing_source":"go","list_cost":0.0000153,"allowance":0.00000918,
				"tokens":{"input":38,"output":16,"reasoning":12,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}
		}
	}`
}

func TestOpenCodeContractText(t *testing.T) {
	// covers: FIN-01
	handler, captured := captureHandler(t, textResponse())
	server := newServer(t, handler)
	adapter, _ := newTestAdapter(t, server, func(opts *OpenCodeOptions) {
		opts.BaseURL = server.URL + "/"
	})

	response, err := adapter.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	method, path, header, rawBody := captured.snapshot()
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", path)
	}
	if got, want := header.Get("Authorization"), "Bearer oc_sk_contract_key"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got, want := header.Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := header.Get("User-Agent"), "recon-agent/test"; got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
	if got, want := header.Get("x-opencode-client"), "recon-agent/test"; got != want {
		t.Errorf("x-opencode-client = %q, want %q", got, want)
	}
	if got, want := header.Get("x-opencode-session"), "ses_contract_1"; got != want {
		t.Errorf("x-opencode-session = %q, want %q", got, want)
	}

	body := decodeBody(t, rawBody)
	if got, want := body["model"], "deepseek-v4.1-flash"; got != want {
		t.Errorf("model = %v, want %v", got, want)
	}
	if stream, ok := body["stream"].(bool); !ok || stream {
		t.Errorf("stream = %v, want false", body["stream"])
	}
	if got, want := body["temperature"], json.Number("0"); got != want {
		t.Errorf("temperature = %v, want 0", got)
	}
	if got, want := body["max_tokens"], json.Number("32"); got != want {
		t.Errorf("max_tokens = %v, want 32", got)
	}
	if _, ok := body["tools"]; ok {
		t.Error("request without tools sent a tools field")
	}
	if _, ok := body["tool_choice"]; ok {
		t.Error("request without tools sent a tool_choice field")
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v, want 2", body["messages"])
	}
	first := messages[0].(map[string]any)
	second := messages[1].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are a test agent." {
		t.Errorf("first message = %#v", first)
	}
	if second["role"] != "user" || second["content"] != "ping" {
		t.Errorf("second message = %#v", second)
	}

	if response.ID != "chatcmpl-text" {
		t.Errorf("ID = %q", response.ID)
	}
	if response.Provider != ProviderOpenCodeGo {
		t.Errorf("Provider = %q", response.Provider)
	}
	if response.RequestedModel != "deepseek-v4.1-flash" || response.ActualModel != "deepseek-v4.1-flash" {
		t.Errorf("models = %q/%q", response.RequestedModel, response.ActualModel)
	}
	if response.Message.Content != "pong" || response.Message.ReasoningContent != "thinking" {
		t.Errorf("message = %#v", response.Message)
	}
	if response.StopReason != StopStop {
		t.Errorf("StopReason = %q", response.StopReason)
	}
	if response.RequestID != "req-contract" {
		t.Errorf("RequestID = %q, want req-contract", response.RequestID)
	}
	if response.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", response.Attempts)
	}
	if response.Usage.PromptTokens != 38 || response.Usage.CompletionTokens != 16 || response.Usage.TotalTokens != 54 {
		t.Errorf("prompt/completion/total = %d/%d/%d", response.Usage.PromptTokens, response.Usage.CompletionTokens, response.Usage.TotalTokens)
	}
	if response.Usage.InputTokens != 38 || response.Usage.OutputTokens != 16 || response.Usage.ReasoningTokens != 12 {
		t.Errorf("opencode tokens = %+v", response.Usage)
	}
	if !response.Usage.ListCostReported || response.Usage.ListCost.String() != "0.0000153" {
		t.Errorf("ListCost = %s reported=%v", response.Usage.ListCost, response.Usage.ListCostReported)
	}
	if !response.Usage.AllowanceReported || response.Usage.Allowance.String() != "0.00000918" {
		t.Errorf("Allowance = %s reported=%v", response.Usage.Allowance, response.Usage.AllowanceReported)
	}
	if response.Usage.CostUSD.String() != "0.0000153" || response.Usage.CostSource != CostSourceAPIListCost {
		t.Errorf("CostUSD = %s source=%s", response.Usage.CostUSD, response.Usage.CostSource)
	}
	if response.Usage.ComputedCost.String() != "0.0000153" {
		t.Errorf("ComputedCost = %s", response.Usage.ComputedCost)
	}
	if response.Usage.CostMismatch {
		t.Error("CostMismatch = true for matching costs")
	}
	if len(response.Raw) == 0 {
		t.Error("Raw response not preserved")
	}
}

func TestOpenCodeToolCallRequestAndResponse(t *testing.T) {
	handler, captured := captureHandler(t, `{
		"id":"chatcmpl-tool","model":"deepseek-v4.1-flash",
		"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[
			{"id":"call-1","type":"function","function":{"name":"get_ping","arguments":"{\"count\":2}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":10,"completion_tokens":4}
	}`)
	server := newServer(t, handler)
	adapter, _ := newTestAdapter(t, server, nil)

	req := Request{
		Model:  "deepseek-v4.1-flash",
		System: "tools available",
		Messages: []Message{
			{Role: RoleAssistant, Content: "", ReasoningContent: "hidden reasoning", ToolCalls: []ToolCall{{ID: "call-0", Name: "get_ping", Arguments: nil}}},
			{Role: RoleTool, Content: "ok", ToolCallID: "call-0"},
		},
		Tools:         []ToolDef{{Name: "get_ping", Description: "ping", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:    &ToolChoice{Mode: ToolChoiceTool, Name: "get_ping"},
		MaxTokens:     0,
		SessionID:     "ses_tool_1",
		PromptVersion: "v1",
	}
	response, err := adapter.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, _, _, rawBody := captured.snapshot()
	body := decodeBody(t, rawBody)
	if _, ok := body["temperature"]; ok {
		t.Error("nil temperature was serialized")
	}
	if _, ok := body["max_tokens"]; ok {
		t.Error("zero max_tokens was serialized")
	}
	choice, ok := body["tool_choice"].(map[string]any)
	if !ok || choice["type"] != "function" {
		t.Fatalf("tool_choice = %#v", body["tool_choice"])
	}
	fn := choice["function"].(map[string]any)
	if fn["name"] != "get_ping" {
		t.Fatalf("tool_choice.function = %#v", fn)
	}
	tools := body["tools"].([]any)
	toolFn := tools[0].(map[string]any)["function"].(map[string]any)
	if toolFn["name"] != "get_ping" || toolFn["parameters"] == nil {
		t.Fatalf("tools = %#v", tools)
	}
	messages := body["messages"].([]any)
	assistant := messages[1].(map[string]any)
	if _, ok := assistant["reasoning_content"]; ok {
		t.Error("reasoning_content was sent outbound")
	}
	assistantCalls := assistant["tool_calls"].([]any)
	arguments := assistantCalls[0].(map[string]any)["function"].(map[string]any)["arguments"]
	if arguments != "{}" {
		t.Errorf("empty arguments = %v, want {}", arguments)
	}
	toolMessage := messages[2].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call-0" {
		t.Errorf("tool message = %#v", toolMessage)
	}

	if response.StopReason != StopToolCalls {
		t.Errorf("StopReason = %q, want tool_calls", response.StopReason)
	}
	if response.Message.Content != "" || response.Message.ReasoningContent != "" {
		t.Errorf("message = %#v", response.Message)
	}
	if len(response.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", response.Message.ToolCalls)
	}
	call := response.Message.ToolCalls[0]
	if call.ID != "call-1" || call.Name != "get_ping" || string(call.Arguments) != `{"count":2}` {
		t.Errorf("tool call = %#v", call)
	}
}

func TestOpenCodeToolChoiceDefaultsToAuto(t *testing.T) {
	handler, captured := captureHandler(t, textResponse())
	server := newServer(t, handler)
	adapter, _ := newTestAdapter(t, server, nil)
	req := testRequest()
	req.Tools = []ToolDef{{Name: "get_ping", Parameters: json.RawMessage(`{"type":"object"}`)}}
	req.ToolChoice = nil
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, _, _, rawBody := captured.snapshot()
	if got := decodeBody(t, rawBody)["tool_choice"]; got != "auto" {
		t.Fatalf("tool_choice = %#v, want auto", got)
	}
}

func TestOpenCodeResponseShapes(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		wantText  string
		wantStop  StopReason
		wantError bool
	}{
		{"null content", `{"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":null,"reasoning_content":null},"finish_reason":"stop"}]}`, "", StopStop, false},
		{"empty content", `{"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":""},"finish_reason":"stop"}]}`, "", StopStop, false},
		{"absent content", `{"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{},"finish_reason":null}]}`, "", StopUnknown, false},
		{"unknown finish", `{"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":"x"},"finish_reason":"weird"}]}`, "x", StopUnknown, false},
		{"multiple choices", `{"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":"first"},"finish_reason":"stop"},{"message":{"content":"second"},"finish_reason":"length"}]}`, "first", StopStop, false},
		{"empty choices", `{"id":"1","model":"deepseek-v4.1-flash","choices":[]}`, "", "", true},
		{"number content", `{"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":42},"finish_reason":"stop"}]}`, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tt.response) })
			adapter, _ := newTestAdapter(t, server, nil)
			response, err := adapter.Complete(context.Background(), testRequest())
			if tt.wantError {
				if err == nil {
					t.Fatal("Complete accepted an invalid response")
				}
				pe, ok := AsProviderError(err)
				if !ok || pe.Kind != KindInvalidResponse || pe.Retryable {
					t.Fatalf("error = %+v, want invalid_response non-retryable", pe)
				}
				return
			}
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if response.Message.Content != tt.wantText {
				t.Errorf("content = %q, want %q", response.Message.Content, tt.wantText)
			}
			if response.StopReason != tt.wantStop {
				t.Errorf("stop = %q, want %q", response.StopReason, tt.wantStop)
			}
		})
	}
}

func TestOpenCodeUsageWithoutOpenCodeBlock(t *testing.T) {
	// covers: FIN-01
	response := `{
		"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":5}}
	}`
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, response) })
	adapter, _ := newTestAdapter(t, server, nil)
	got, err := adapter.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage.InputTokens != 60 || got.Usage.CacheReadTokens != 40 || got.Usage.CachedTokens != 40 {
		t.Errorf("derived tokens = %+v", got.Usage)
	}
	if got.Usage.OutputTokens != 20 || got.Usage.ReasoningTokens != 5 {
		t.Errorf("output/reasoning = %d/%d", got.Usage.OutputTokens, got.Usage.ReasoningTokens)
	}
	if got.Usage.TotalTokens != 120 {
		t.Errorf("TotalTokens = %d, want 120", got.Usage.TotalTokens)
	}
	if got.Usage.ListCostReported || got.Usage.CostSource != CostSourcePricingTable {
		t.Errorf("cost source = %q reported=%v", got.Usage.CostSource, got.Usage.ListCostReported)
	}
	if got.Usage.ComputedCost.String() != "0.00002112" {
		t.Errorf("ComputedCost = %s, want 0.00002112", got.Usage.ComputedCost)
	}
	if got.Usage.CostUSD.String() != "0.00002112" {
		t.Errorf("CostUSD = %s", got.Usage.CostUSD)
	}
}

func TestOpenCodeDecimalCostsAndMismatch(t *testing.T) {
	// covers: FIN-01
	response := `{
		"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":28,"completion_tokens":24,"cost":"0",
			"opencode":{"billing_source":"go","list_cost":"0.000005","allowance":"0.000003",
				"tokens":{"input":28,"output":24,"reasoning":0,"cache_read":256,"cache_write_5m":0,"cache_write_1h":0}}}
	}`
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, response) })
	adapter, _ := newTestAdapter(t, server, nil)
	got, err := adapter.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage.ListCost.String() != "0.000005" || got.Usage.Allowance.String() != "0.000003" {
		t.Errorf("costs = %s/%s", got.Usage.ListCost, got.Usage.Allowance)
	}
	if got.Usage.APICost.String() != "0" {
		t.Errorf("APICost = %s", got.Usage.APICost)
	}
	if !got.Usage.CostMismatch {
		t.Error("CostMismatch = false, want true")
	}
	if got.Usage.ComputedCost.String() != "0.000019368" {
		t.Errorf("ComputedCost = %s", got.Usage.ComputedCost)
	}
	if got.Usage.CostUSD.String() != "0.000005" {
		t.Errorf("CostUSD = %s, want API list cost", got.Usage.CostUSD)
	}
}

func TestOpenCodeRequestIDFallsBackToBody(t *testing.T) {
	response := `{"id":"1","request_id":"body-request","model":"deepseek-v4.1-flash","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, response) })
	adapter, _ := newTestAdapter(t, server, nil)
	got, err := adapter.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "body-request" {
		t.Fatalf("RequestID = %q", got.RequestID)
	}
}

func TestOpenCodeHTTPErrors(t *testing.T) {
	fake := clock.NewFake(mustTime(fixedNow))
	tests := []struct {
		name      string
		status    int
		body      string
		header    string
		kind      ErrorKind
		retryable bool
		retry     time.Duration
	}{
		{"400 unknown model", 400, `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`, "", KindModelNotFound, false, 0},
		{"400 missing session", 400, `{"type":"error","error":{"type":"MissingSessionID","message":"missing"}}`, "", KindInvalidRequest, false, 0},
		{"401 auth", 401, `{"error":{"type":"authentication_error","message":"Go requires a credential"}}`, "", KindAuth, false, 0},
		{"429 header", 429, `{"type":"RateLimit.Exceeded","message":"slow"}`, "3", KindRateLimit, true, 3 * time.Second},
		{"429 body", 429, `{"type":"RateLimit.Exceeded","message":"slow","retryAfter":7}`, "", KindRateLimit, true, 7 * time.Second},
		{"429 header wins", 429, `{"type":"RateLimit.Exceeded","message":"slow","retryAfter":7}`, "3", KindRateLimit, true, 3 * time.Second},
		{"429 http date", 429, `{}`, fake.Now().Add(30 * time.Second).Format(http.TimeFormat), KindRateLimit, true, 30 * time.Second},
		{"500 typed", 500, `{"type":"Router.Unavailable","message":"down","retryAfter":5}`, "", KindUnavailable, true, 5 * time.Second},
		{"500 generic", 500, `{"error":{"type":"server_error","message":"boom"}}`, "", KindServer, true, 0},
		{"500 downgrade", 500, `{"type":"Router.Unavailable","message":"down","shouldRetry":false}`, "", KindUnavailable, false, 0},
		{"400 budget", 400, `{"type":"Account.GoUsageLimit","message":"limit","retryAfter":60}`, "", KindBudget, false, 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.Header().Set("x-request-id", "req-error")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			adapter, _ := newTestAdapter(t, server, func(opts *OpenCodeOptions) { opts.Clock = fake })
			_, err := adapter.Complete(context.Background(), testRequest())
			if err == nil {
				t.Fatal("Complete accepted an error response")
			}
			pe, ok := AsProviderError(err)
			if !ok {
				t.Fatalf("error is not a ProviderError: %v", err)
			}
			if pe.Kind != tt.kind || pe.Retryable != tt.retryable {
				t.Errorf("kind/retryable = %q/%v, want %q/%v", pe.Kind, pe.Retryable, tt.kind, tt.retryable)
			}
			if pe.RetryAfter != tt.retry {
				t.Errorf("RetryAfter = %s, want %s", pe.RetryAfter, tt.retry)
			}
			if pe.RequestID != "req-error" {
				t.Errorf("RequestID = %q", pe.RequestID)
			}
			if pe.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", pe.StatusCode, tt.status)
			}
		})
	}
}

func TestOpenCodeMalformedSuccessBody(t *testing.T) {
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{`) })
	adapter, _ := newTestAdapter(t, server, nil)
	_, err := adapter.Complete(context.Background(), testRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindInvalidResponse || pe.Retryable {
		t.Fatalf("error = %+v, want invalid_response non-retryable", pe)
	}
}

func TestOpenCodeUnknownModelBeforeHTTP(t *testing.T) {
	var calls int
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, textResponse())
	})
	adapter, _ := newTestAdapter(t, server, nil)
	req := testRequest()
	req.Model = "does-not-exist-model"
	_, err := adapter.Complete(context.Background(), req)
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("error = %v, want ErrUnknownModel", err)
	}
	pe, _ := AsProviderError(err)
	if pe.Kind != KindModelNotFound || pe.Retryable {
		t.Fatalf("kind = %q retryable=%v", pe.Kind, pe.Retryable)
	}
	if calls != 0 {
		t.Fatalf("server calls = %d, want 0", calls)
	}
}

func TestOpenCodeSessionValidationBeforeHTTP(t *testing.T) {
	var calls int
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, textResponse())
	})
	adapter, _ := newTestAdapter(t, server, nil)
	for _, session := range []string{"", "missing-prefix", "ses_", "ses_bad space"} {
		req := testRequest()
		req.SessionID = session
		_, err := adapter.Complete(context.Background(), req)
		pe, ok := AsProviderError(err)
		if !ok || pe.Kind != KindInvalidRequest {
			t.Fatalf("session %q error = %+v, want invalid_request", session, pe)
		}
	}
	if calls != 0 {
		t.Fatalf("server calls = %d, want 0", calls)
	}
}

func TestOpenCodeTransportError(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed with oc_sk_synthetic_key")
	})
	adapter, err := NewOpenCode(OpenCodeOptions{
		BaseURL:    DefaultBaseURL,
		APIKey:     "oc_sk_synthetic_key",
		HTTPClient: &http.Client{Transport: transport},
		Clock:      clock.NewFake(mustTime(fixedNow)),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Complete(context.Background(), testRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindUnavailable || !pe.Retryable {
		t.Fatalf("error = %+v, want unavailable retryable", pe)
	}
	if strings.Contains(err.Error(), "oc_sk_synthetic_key") {
		t.Fatal("transport error leaked the API key")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOpenCodeCanceledContext(t *testing.T) {
	server := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	})
	adapter, _ := newTestAdapter(t, server, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := adapter.Complete(ctx, testRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindCanceled || pe.Retryable {
		t.Fatalf("error = %+v, want canceled non-retryable", pe)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false: %v", err)
	}
}

func TestOpenCodeDeadline(t *testing.T) {
	server := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	})
	adapter, _ := newTestAdapter(t, server, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := adapter.Complete(ctx, testRequest())
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindCanceled {
		t.Fatalf("error = %+v, want canceled", pe)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false: %v", err)
	}
}

func TestOpenCodeRedactsErrorsAndLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	key := "oc_sk_synthetic_never_log"
	session := "ses_never_log"
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"bad Bearer `+key+` x-opencode-session: `+session+`"}}`)
	})
	adapter, _ := newTestAdapter(t, server, func(opts *OpenCodeOptions) {
		opts.APIKey = key
		opts.Logger = logger
	})
	req := testRequest()
	req.SessionID = session
	_, err := adapter.Complete(context.Background(), req)
	if err == nil {
		t.Fatal("Complete succeeded")
	}
	for _, forbidden := range []string{key, session} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("error leaked %q: %v", forbidden, err)
		}
		if strings.Contains(logs.String(), forbidden) {
			t.Errorf("logs leaked %q: %s", forbidden, logs.String())
		}
	}
	pe, _ := AsProviderError(err)
	if strings.Contains(string(pe.Body), key) || strings.Contains(string(pe.Body), session) {
		t.Fatal("error body leaked a secret")
	}
}

func TestOpenCodeSuccessLogRedactsSecretShapedModel(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	response := `{
		"id":"chatcmpl-secret-model","model":"oc_sk_live_deadbeef",
		"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]
	}`
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, response) })
	adapter, _ := newTestAdapter(t, server, func(opts *OpenCodeOptions) { opts.Logger = logger })
	if _, err := adapter.Complete(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "oc_sk_live_deadbeef") {
		t.Fatalf("logs leaked a secret-shaped model: %s", logs.String())
	}
}

func TestOpenCodeSuccessLogHasNoPayloadOrSession(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, textResponse()) })
	adapter, _ := newTestAdapter(t, server, func(opts *OpenCodeOptions) { opts.Logger = logger })
	req := testRequest()
	req.Messages[0].Content = "highly sensitive memo text"
	req.SessionID = "ses_sensitive_session"
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"highly sensitive memo text", "ses_sensitive_session"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Errorf("logs leaked %q: %s", forbidden, logs.String())
		}
	}
}

func TestNewOpenCodeConfigurationErrors(t *testing.T) {
	if _, err := NewOpenCode(OpenCodeOptions{APIKey: ""}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty key error = %v, want ErrNotConfigured", err)
	}
	if _, err := NewOpenCode(OpenCodeOptions{APIKey: "key", BaseURL: "://bad"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("bad base URL error = %v, want ErrNotConfigured", err)
	}
	adapter, err := NewOpenCode(OpenCodeOptions{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.baseURL != DefaultBaseURL {
		t.Fatalf("default base URL = %q, want %q", adapter.baseURL, DefaultBaseURL)
	}
}
