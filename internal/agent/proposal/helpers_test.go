package proposal

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const (
	unitRunID    = "11111111-1111-4111-8111-111111111111"
	unitCaseID   = "22222222-2222-4222-8222-222222222222"
	otherCaseID  = "99999999-9999-4999-8999-999999999999"
	unitTxnID    = "33333333-3333-4333-8333-333333333333"
	unitBatchID  = "44444444-4444-4444-8444-444444444444"
	unitSchedID  = "55555555-5555-4555-8555-555555555555"
	unitRecordID = "66666666-6666-4666-8666-666666666666"
)

type execCall struct {
	SQL  string
	Args []any
}

type fakeExecer struct {
	mu    sync.Mutex
	calls []execCall
	tag   pgconn.CommandTag
	err   error
}

func (f *fakeExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, execCall{SQL: sql, Args: append([]any(nil), args...)})
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	return f.tag, nil
}

func (f *fakeExecer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeRecorder struct {
	mu    sync.Mutex
	calls []tools.ToolCall
}

func (f *fakeRecorder) RecordToolCall(_ context.Context, call tools.ToolCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return nil
}

func (f *fakeRecorder) all() []tools.ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tools.ToolCall(nil), f.calls...)
}

type fakeEvidenceReader struct {
	refs []string
	err  error
}

func (f *fakeEvidenceReader) RunRefs(context.Context, string) ([]string, error) {
	return f.refs, f.err
}

type stubReadTool struct {
	name string
	refs []string
}

func (s stubReadTool) Name() string        { return s.name }
func (s stubReadTool) Description() string { return "stub read tool" }
func (s stubReadTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}

func (s stubReadTool) Call(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
	return tools.Result{Data: map[string]any{"ok": true}, Refs: s.refs}, nil
}

func newUnitDeps(execer *fakeExecer, evidence *fakeEvidenceReader) Deps {
	return Deps{
		AgentExec: execer,
		Clock:     clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
		Evidence:  evidence,
	}
}

func newUnitRunner(t *testing.T, deps Deps) (*tools.Runner, *fakeRecorder, *tools.RunContext) {
	t.Helper()
	registry := tools.NewRegistry()
	if err := Register(registry, deps); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	recorder := &fakeRecorder{}
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	run := tools.NewRunContext(unitRunID, unitCaseID, nil, fake)
	return tools.NewRunner(registry, recorder, fake), recorder, run
}

func decodeEnvelope(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, raw)
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

func envelopeData(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no data object: %v", envelope)
	}
	return data
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return raw
}
