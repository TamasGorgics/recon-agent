package tools_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// Stable IDs used by unit tests. They are syntactically valid UUIDs so the
// same values work with the integration recorder.
const (
	testRunID  = "11111111-1111-4111-8111-111111111111"
	testCaseID = "22222222-2222-4222-8222-222222222222"
	testTxnID  = "33333333-3333-4333-8333-333333333333"
	testBatch  = "44444444-4444-4444-8444-444444444444"
	testSched  = "55555555-5555-4555-8555-555555555555"
)

// fakeRecorder captures tool calls without touching Postgres.
type fakeRecorder struct {
	mu    sync.Mutex
	calls []tools.ToolCall
	err   error
}

func (f *fakeRecorder) RecordToolCall(_ context.Context, call tools.ToolCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakeRecorder) all() []tools.ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tools.ToolCall(nil), f.calls...)
}

func (f *fakeRecorder) only(t *testing.T) tools.ToolCall {
	t.Helper()
	calls := f.all()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(calls))
	}
	return calls[0]
}

// stubTool is a configurable Tool for registry, validation and runner tests.
type stubTool struct {
	name   string
	schema string
	call   func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error)
}

func (s *stubTool) Name() string                 { return s.name }
func (s *stubTool) Description() string          { return "stub tool " + s.name }
func (s *stubTool) InputSchema() json.RawMessage { return json.RawMessage(s.schema) }

func (s *stubTool) Call(ctx context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error) {
	if s.call == nil {
		return tools.Result{Data: map[string]any{}}, nil
	}
	return s.call(ctx, run, args)
}

// emptySchema is a valid no-argument tool schema.
const emptySchema = `{
	"type": "object",
	"additionalProperties": false
}`

// echoSchema is the shared validation fixture. It exercises nested objects,
// required fields, patterns, enums and an integer amount.
const echoSchema = `{
	"type": "object",
	"additionalProperties": false,
	"required": ["case_id", "amount_minor", "currency", "date", "mode", "nested"],
	"properties": {
		"case_id": {"type": "string", "pattern": "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"},
		"amount_minor": {"type": "integer"},
		"currency": {"type": "string", "pattern": "^[A-Z]{3}$"},
		"date": {"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$"},
		"mode": {"enum": ["a", "b"]},
		"nested": {
			"type": "object",
			"additionalProperties": false,
			"required": ["id"],
			"properties": {"id": {"type": "string"}}
		}
	}
}`

func newStub(name, schema string) *stubTool {
	return &stubTool{
		name:   name,
		schema: schema,
		call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
			return tools.Result{Data: map[string]any{"ok": true}}, nil
		},
	}
}

// newTestRunner builds a registry with the supplied tools, a fake recorder, a
// frozen fake clock and a run context.
func newTestRunner(t *testing.T, toolList ...tools.Tool) (*tools.Runner, *fakeRecorder, *clock.Fake, *tools.RunContext) {
	t.Helper()
	registry := tools.NewRegistry()
	for _, tool := range toolList {
		if err := registry.Register(tool); err != nil {
			t.Fatalf("register %T: %v", tool, err)
		}
	}
	recorder := &fakeRecorder{}
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	run := tools.NewRunContext(testRunID, testCaseID, nil, fake)
	return tools.NewRunner(registry, recorder, fake), recorder, fake, run
}

// decodeEnvelope unmarshals a runner result and asserts the three keys that
// every envelope must always carry.
func decodeEnvelope(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, raw)
	}
	for _, key := range []string{"refs", "untrusted_fields", "truncated"} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("envelope missing %q: %s", key, raw)
		}
	}
	return envelope
}

func envelopeErrorCode(t *testing.T, envelope map[string]any) string {
	t.Helper()
	errObject, ok := envelope["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObject["code"].(string)
	return code
}

func envelopeErrorMessage(t *testing.T, envelope map[string]any) string {
	t.Helper()
	errObject, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no error object: %v", envelope)
	}
	message, _ := errObject["message"].(string)
	return message
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func entryStrings(t *testing.T, value any) []string {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("value is %T, want a JSON array", value)
	}
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("array item is %T, want string", item)
		}
		values = append(values, text)
	}
	return values
}
