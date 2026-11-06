package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

type countingClient struct {
	mu    sync.Mutex
	calls int
	fn    func(Request) (Response, error)
}

func (c *countingClient) Complete(_ context.Context, req Request) (Response, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.fn(req)
}

func (c *countingClient) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func cassetteRequest() Request {
	return Request{
		Model:         "opencode-go/deepseek-v4.1-flash",
		System:        "You are a test agent.",
		Messages:      []Message{{Role: RoleUser, Content: "ping"}},
		Tools:         []ToolDef{{Name: "get_ping", Description: "ping", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
		ToolChoice:    &ToolChoice{Mode: ToolChoiceTool, Name: "get_ping"},
		Temperature:   temperature(0),
		MaxTokens:     32,
		SessionID:     "ses_cassette_secret",
		PromptVersion: "v1",
	}
}

func cassetteResponse() Response {
	return Response{
		ID:             "chatcmpl-cassette",
		Provider:       ProviderOpenCodeGo,
		RequestedModel: "deepseek-v4.1-flash",
		ActualModel:    "deepseek-v4.1-flash",
		Message:        Message{Role: RoleAssistant, Content: "pong", ReasoningContent: "thinking"},
		StopReason:     StopStop,
		Usage: Usage{
			PromptTokens:     38,
			CompletionTokens: 16,
			TotalTokens:      54,
			ListCost:         decimal.RequireFromString("0.0000153"),
			ListCostReported: true,
			ComputedCost:     decimal.RequireFromString("0.0000153"),
			CostUSD:          decimal.RequireFromString("0.0000153"),
			CostSource:       CostSourceAPIListCost,
		},
		Raw:       json.RawMessage(`{"id":"chatcmpl-cassette","choices":[]}`),
		RequestID: "req-1",
		Latency:   5 * time.Millisecond,
		Attempts:  1,
	}
}

func recordCassette(t *testing.T, dir string, update bool) *Cassette {
	t.Helper()
	inner := &countingClient{fn: func(Request) (Response, error) { return cassetteResponse(), nil }}
	cassette, err := NewCassette(inner, CassetteOptions{
		Mode:     CassetteRecord,
		Dir:      dir,
		Update:   update,
		Provider: ProviderOpenCodeGo,
		BaseURL:  DefaultBaseURL,
		Now:      func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewCassette: %v", err)
	}
	return cassette
}

func TestCassetteRecordReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	req := cassetteRequest()
	inner := &countingClient{fn: func(Request) (Response, error) { return cassetteResponse(), nil }}
	recorder, err := NewCassette(inner, CassetteOptions{
		Mode:     CassetteRecord,
		Dir:      dir,
		Provider: ProviderOpenCodeGo,
		BaseURL:  DefaultBaseURL,
		Now:      func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewCassette(record): %v", err)
	}
	recorded, err := recorder.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("record Complete: %v", err)
	}
	if inner.Calls() != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.Calls())
	}

	path, err := recorder.path(req, mustCassetteKey(t, recorder, req))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cassette: %v", err)
	}
	for _, forbidden := range []string{"ses_cassette_secret", "Authorization", "User-Agent", "x-opencode-session", "oc_sk_"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("cassette contains %q:\n%s", forbidden, data)
		}
	}

	replay, err := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: dir, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
	if err != nil {
		t.Fatalf("NewCassette(replay): %v", err)
	}
	replayed, err := replay.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("replay Complete: %v", err)
	}
	if !reflect.DeepEqual(recorded, replayed) {
		t.Fatalf("replay differs\nrecorded: %#v\nreplayed: %#v", recorded, replayed)
	}
}

func mustCassetteKey(t *testing.T, cassette *Cassette, req Request) string {
	t.Helper()
	key, err := cassette.key(req)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return key
}

func TestCassetteReplayMissIsHardError(t *testing.T) {
	dir := t.TempDir()
	req := cassetteRequest()
	recordCassette(t, dir, false)
	replay, err := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	missing := req
	missing.PromptVersion = "v2"
	_, err = replay.Complete(context.Background(), missing)
	if !errors.Is(err, ErrCassetteMiss) {
		t.Fatalf("replay error = %v, want ErrCassetteMiss", err)
	}
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindCassetteMiss || pe.Retryable {
		t.Fatalf("replay error = %+v, want cassette_miss non-retryable", pe)
	}
	if strings.Contains(err.Error(), req.SessionID) {
		t.Fatal("replay error leaked the session ID")
	}
}

func TestCassetteReplayCorruptAndKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	req := cassetteRequest()
	recorder := recordCassette(t, dir, false)
	if _, err := recorder.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	replay, _ := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: dir, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
	key := mustCassetteKey(t, replay, req)
	path, err := replay.path(req, key)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Complete(context.Background(), req); !errors.Is(err, ErrCassetteMiss) {
		t.Fatalf("corrupt replay error = %v, want ErrCassetteMiss", err)
	}

	file := map[string]any{}
	data, _ := json.Marshal(cassetteResponse())
	file["schema"] = 1
	file["key"] = "deadbeef"
	file["response"] = json.RawMessage(data)
	updated, _ := json.Marshal(file)
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Complete(context.Background(), req); !errors.Is(err, ErrCassetteMiss) {
		t.Fatalf("key mismatch error = %v, want ErrCassetteMiss", err)
	}
}

func TestCassetteReplayEmptySession(t *testing.T) {
	dir := t.TempDir()
	req := cassetteRequest()
	recorder := recordCassette(t, dir, false)
	if _, err := recorder.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	replay, _ := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: dir, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
	req.SessionID = ""
	if _, err := replay.Complete(context.Background(), req); err != nil {
		t.Fatalf("replay without session: %v", err)
	}
}

func TestCassettePassthroughCallsInnerAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	inner := &countingClient{fn: func(Request) (Response, error) { return cassetteResponse(), nil }}
	cassette, err := NewCassette(inner, CassetteOptions{Mode: CassettePassthrough, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cassette.Complete(context.Background(), cassetteRequest()); err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	if inner.Calls() != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.Calls())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("passthrough wrote %d entries", len(entries))
	}
}

func TestCassetteRecordRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	req := cassetteRequest()
	first := recordCassette(t, dir, false)
	if _, err := first.Complete(context.Background(), req); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if _, err := first.Complete(context.Background(), req); err == nil {
		t.Fatal("record overwrote an existing cassette without Update")
	}
	updatedInner := &countingClient{fn: func(Request) (Response, error) {
		response := cassetteResponse()
		response.ID = "replacement"
		return response, nil
	}}
	updated, err := NewCassette(updatedInner, CassetteOptions{Mode: CassetteRecord, Dir: dir, Update: true, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updated.Complete(context.Background(), req); err != nil {
		t.Fatalf("Update record: %v", err)
	}
	replay, _ := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: dir, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
	got, err := replay.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("replay updated: %v", err)
	}
	if got.ID != "replacement" {
		t.Fatalf("replayed ID = %q, want replacement", got.ID)
	}
}

func TestCassetteKeySensitivity(t *testing.T) {
	cassette, err := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	base := cassetteRequest()
	baseKey := mustCassetteKey(t, cassette, base)

	same := base
	same.SessionID = "ses_different"
	if got := mustCassetteKey(t, cassette, same); got != baseKey {
		t.Error("key changed with SessionID")
	}
	noSession := base
	noSession.SessionID = ""
	if got := mustCassetteKey(t, cassette, noSession); got != baseKey {
		t.Error("key changed with empty SessionID")
	}

	tests := []struct {
		name string
		edit func(*Request)
	}{
		{"model", func(r *Request) { r.Model = "glm-5.3-flash" }},
		{"prompt version", func(r *Request) { r.PromptVersion = "v2" }},
		{"system", func(r *Request) { r.System = "different" }},
		{"messages", func(r *Request) { r.Messages[0].Content = "different" }},
		{"tools", func(r *Request) { r.Tools[0].Description = "different" }},
		{"tool choice", func(r *Request) { r.ToolChoice = &ToolChoice{Mode: ToolChoiceAuto} }},
		{"temperature", func(r *Request) { r.Temperature = nil }},
		{"max tokens", func(r *Request) { r.MaxTokens = 64 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			changed.Messages = append([]Message(nil), base.Messages...)
			changed.Tools = append([]ToolDef(nil), base.Tools...)
			tt.edit(&changed)
			if got := mustCassetteKey(t, cassette, changed); got == baseKey {
				t.Fatalf("key did not change for %s", tt.name)
			}
		})
	}

	otherBase, _ := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: t.TempDir(), BaseURL: "https://example.test/v1"})
	if got := mustCassetteKey(t, otherBase, base); got == baseKey {
		t.Fatal("key did not change with base URL")
	}
}

func TestCassetteToolChoiceDefaultMatchesAuto(t *testing.T) {
	cassette, _ := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: t.TempDir()})
	withNil := cassetteRequest()
	withNil.ToolChoice = nil
	withAuto := cassetteRequest()
	withAuto.ToolChoice = &ToolChoice{Mode: ToolChoiceAuto}
	if mustCassetteKey(t, cassette, withNil) != mustCassetteKey(t, cassette, withAuto) {
		t.Fatal("nil tool choice with tools differs from auto")
	}
}

func TestCassetteRejectsUnsafePaths(t *testing.T) {
	req := cassetteRequest()
	if _, err := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: t.TempDir(), Provider: ".."}); err == nil {
		t.Fatal("NewCassette accepted an unsafe provider component")
	}
	cassette, _ := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: t.TempDir()})
	for _, version := range []string{"../escape", "a/b", "..", "."} {
		changed := req
		changed.PromptVersion = version
		if _, err := cassette.key(changed); err != nil {
			continue
		}
		if _, err := cassette.path(changed, mustCassetteKey(t, cassette, changed)); err == nil {
			t.Errorf("path accepted prompt version %q", version)
		}
	}
	unsafeModel := req
	unsafeModel.Model = "../evil"
	if _, err := cassette.key(unsafeModel); err != nil {
		return
	}
	if _, err := cassette.path(unsafeModel, mustCassetteKey(t, cassette, unsafeModel)); err == nil {
		t.Error("path accepted a traversal model")
	}
}

func TestCassetteRejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	inner := &countingClient{fn: func(Request) (Response, error) {
		response := cassetteResponse()
		response.Message.Content = strings.Repeat("x", maxCassetteBytes)
		return response, nil
	}}
	cassette, _ := NewCassette(inner, CassetteOptions{Mode: CassetteRecord, Dir: dir})
	if _, err := cassette.Complete(context.Background(), cassetteRequest()); err == nil {
		t.Fatal("record accepted an oversized cassette")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize error = %v, want size diagnostic", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("oversized record wrote %d entries", len(entries))
	}
}

func TestCassetteRedactsSecretsInFile(t *testing.T) {
	dir := t.TempDir()
	key := "oc_sk_synthetic_secret"
	inner := &countingClient{fn: func(Request) (Response, error) {
		response := cassetteResponse()
		response.Message.Content = "leaked " + key + " and ses_abc123"
		return response, nil
	}}
	cassette, _ := NewCassette(inner, CassetteOptions{Mode: CassetteRecord, Dir: dir, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
	req := cassetteRequest()
	if _, err := cassette.Complete(context.Background(), req); err != nil {
		t.Fatalf("record: %v", err)
	}
	path, _ := cassette.path(req, mustCassetteKey(t, cassette, req))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), key) || strings.Contains(string(data), "ses_abc123") {
		t.Fatalf("cassette retained a secret:\n%s", data)
	}
	var file cassetteFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("cassette is corrupt: %v", err)
	}
	if len(file.Redactions) == 0 {
		t.Fatal("redactions list is empty")
	}
}

func TestCassetteRedactionPreservesJSONAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	req := cassetteRequest()
	req.System = "x-opencode-session: ses_request_secret"
	req.Messages[0].Content = "authorization:request-secret"
	response := cassetteResponse()
	response.Message.Content = "authorization:sekrit-token"
	response.Message.ReasoningContent = "x-opencode-session: ses_reasoning_secret"
	response.Raw = json.RawMessage(`{"message":"authorization:sekrit-token","value":"Bearer oc_sk_synthetic_raw"}`)
	response.Usage.Raw = json.RawMessage(`{"authorization":"sekrit-token"}`)

	inner := &countingClient{fn: func(Request) (Response, error) { return response, nil }}
	recorder, err := NewCassette(inner, CassetteOptions{
		Mode:     CassetteRecord,
		Dir:      dir,
		Provider: ProviderOpenCodeGo,
		BaseURL:  DefaultBaseURL,
		Now:      func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewCassette(record): %v", err)
	}
	recorded, err := recorder.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("record Complete: %v", err)
	}

	path, err := recorder.path(req, mustCassetteKey(t, recorder, req))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cassette: %v", err)
	}
	var file cassetteFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("cassette is invalid JSON after redaction: %v\n%s", err, data)
	}
	for _, secret := range []string{
		"sekrit-token",
		"request-secret",
		"ses_request_secret",
		"ses_reasoning_secret",
		"oc_sk_synthetic_raw",
	} {
		if strings.Contains(string(data), secret) {
			t.Errorf("cassette retained %q:\n%s", secret, data)
		}
	}

	replay, err := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: dir, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
	if err != nil {
		t.Fatalf("NewCassette(replay): %v", err)
	}
	replayed, err := replay.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("replay Complete: %v", err)
	}
	if !reflect.DeepEqual(recorded, replayed) {
		t.Fatalf("redacted replay differs\nrecorded: %#v\nreplayed: %#v", recorded, replayed)
	}
}

func TestCassetteRedactionPunctuationRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		forbidden string
	}{
		{"authorization before quote", "authorization:sekrit-token", "sekrit-token"},
		{"authorization before comma", "authorization:sekrit-token, rest", "sekrit-token"},
		{"authorization before brace", "authorization:sekrit-token} rest", "sekrit-token"},
		{"authorization before newline", "authorization:sekrit-token\nnext", "sekrit-token"},
		{"session before comma", "x-opencode-session: ses_secret123, rest", "ses_secret123"},
		{"session before brace", "x-opencode-session: ses_secret123} rest", "ses_secret123"},
		{"bearer before quote", "Bearer abc.def-123", "abc.def-123"},
		{"bearer before comma", "Bearer abc.def-123, rest", "abc.def-123"},
		{"api key before brace", "Bearer oc_sk_synthetic_bearer} rest", "oc_sk_synthetic_bearer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			req := cassetteRequest()
			response := cassetteResponse()
			response.Message.Content = tt.content
			inner := &countingClient{fn: func(Request) (Response, error) { return response, nil }}
			recorder, err := NewCassette(inner, CassetteOptions{
				Mode:     CassetteRecord,
				Dir:      dir,
				Provider: ProviderOpenCodeGo,
				BaseURL:  DefaultBaseURL,
				Now:      func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
			})
			if err != nil {
				t.Fatalf("NewCassette(record): %v", err)
			}
			recorded, err := recorder.Complete(context.Background(), req)
			if err != nil {
				t.Fatalf("record Complete: %v", err)
			}
			path, err := recorder.path(req, mustCassetteKey(t, recorder, req))
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read cassette: %v", err)
			}
			var file cassetteFile
			if err := json.Unmarshal(data, &file); err != nil {
				t.Fatalf("cassette is invalid JSON: %v\n%s", err, data)
			}
			if strings.Contains(string(data), tt.forbidden) {
				t.Fatalf("cassette retained %q:\n%s", tt.forbidden, data)
			}
			replay, err := NewCassette(nil, CassetteOptions{Mode: CassetteReplay, Dir: dir, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
			if err != nil {
				t.Fatalf("NewCassette(replay): %v", err)
			}
			replayed, err := replay.Complete(context.Background(), req)
			if err != nil {
				t.Fatalf("replay Complete: %v", err)
			}
			if !reflect.DeepEqual(recorded, replayed) {
				t.Fatalf("redacted replay differs\nrecorded: %#v\nreplayed: %#v", recorded, replayed)
			}
		})
	}
}

func TestCassetteCleanFileHasEmptyRedactions(t *testing.T) {
	dir := t.TempDir()
	req := cassetteRequest()
	cassette := recordCassette(t, dir, false)
	if _, err := cassette.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	path, _ := cassette.path(req, mustCassetteKey(t, cassette, req))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file cassetteFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Redactions) != 0 {
		t.Fatalf("clean cassette redactions = %v, want empty", file.Redactions)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("cassette has no trailing newline")
	}
	if filepath.Base(path) != file.Key+".json" {
		t.Fatalf("cassette file name %q does not match key", filepath.Base(path))
	}
}

func TestCassetteReplayDoesNotCallInner(t *testing.T) {
	dir := t.TempDir()
	req := cassetteRequest()
	recorder := recordCassette(t, dir, false)
	if _, err := recorder.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	inner := &countingClient{fn: func(Request) (Response, error) { t.Fatal("replay called the inner client"); return Response{}, nil }}
	replay, _ := NewCassette(inner, CassetteOptions{Mode: CassetteReplay, Dir: dir, Provider: ProviderOpenCodeGo, BaseURL: DefaultBaseURL})
	if _, err := replay.Complete(context.Background(), req); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if inner.Calls() != 0 {
		t.Fatal("replay called the inner client")
	}
}
