package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func TestRunnerUnknownToolRecorded(t *testing.T) {
	t.Parallel()

	runner, recorder, _, run := newTestRunner(t)
	raw, err := runner.Call(context.Background(), run, 1, "missing_tool", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if code := envelopeErrorCode(t, envelope); code != tools.CodeUnknownTool {
		t.Fatalf("error code = %q, want unknown_tool", code)
	}
	recorded := recorder.only(t)
	if recorded.ToolName != "missing_tool" {
		t.Fatalf("recorded tool name = %q, want missing_tool", recorded.ToolName)
	}
	if recorded.ErrorCode != tools.CodeUnknownTool {
		t.Fatalf("recorded error code = %q, want unknown_tool", recorded.ErrorCode)
	}
}

func TestRunnerHandlerErrorsBecomeStructuredEnvelopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		call     func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error)
		wantCode string
	}{
		{
			name: "not found",
			call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
				return tools.Result{}, tools.NotFound("case missing")
			},
			wantCode: tools.CodeNotFound,
		},
		{
			name: "invalid arguments sentinel",
			call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
				return tools.Result{}, fmt.Errorf("%w: case_id is malformed", tools.ErrInvalidArguments)
			},
			wantCode: tools.CodeInvalidArguments,
		},
		{
			name: "tool error sentinel",
			call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
				return tools.Result{}, fmt.Errorf("%w: database said no", tools.ErrTool)
			},
			wantCode: tools.CodeToolError,
		},
		{
			name: "tool failure message",
			call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
				return tools.Result{}, tools.Failure("bad tool state")
			},
			wantCode: tools.CodeToolError,
		},
		{
			name: "untyped error",
			call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
				return tools.Result{}, fmt.Errorf("pgx: connection refused secret-token")
			},
			wantCode: tools.CodeInternalError,
		},
		{
			name: "panic",
			call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
				panic("boom-secret")
			},
			wantCode: tools.CodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tool := &stubTool{name: "broken", schema: emptySchema, call: tt.call}
			runner, recorder, _, run := newTestRunner(t, tool)

			raw, err := runner.Call(context.Background(), run, 1, "broken", json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			envelope := decodeEnvelope(t, raw)
			if code := envelopeErrorCode(t, envelope); code != tt.wantCode {
				t.Fatalf("error code = %q, want %q\n%s", code, tt.wantCode, raw)
			}
			message := envelopeErrorMessage(t, envelope)
			for _, secret := range []string{"secret", "pgx", "boom", "goroutine", "stack"} {
				if strings.Contains(strings.ToLower(message), secret) {
					t.Fatalf("error message leaks %q: %q", secret, message)
				}
			}
			if got := recorder.only(t).ErrorCode; got != tt.wantCode {
				t.Fatalf("recorded error code = %q, want %q", got, tt.wantCode)
			}
			if refs := entryStrings(t, envelope["refs"]); len(refs) != 0 {
				t.Fatalf("error envelope has refs: %s", raw)
			}
		})
	}
}

func TestRunnerRecordsInvalidArgumentsWithZeroDuration(t *testing.T) {
	t.Parallel()

	runner, recorder, _, run := newTestRunner(t, newStub("echo", echoSchema))
	before := run.Clock.Now()

	raw, err := runner.Call(context.Background(), run, 1, "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if code := envelopeErrorCode(t, decodeEnvelope(t, raw)); code != tools.CodeInvalidArguments {
		t.Fatalf("error code = %q, want invalid_arguments", code)
	}
	recorded := recorder.only(t)
	if !recorded.StartedAt.Equal(before) {
		t.Fatalf("StartedAt = %v, want %v", recorded.StartedAt, before)
	}
	if recorded.DurationMS != 0 {
		t.Fatalf("DurationMS = %d, want 0 for rejected arguments", recorded.DurationMS)
	}
}

func TestRunnerRecordingFailure(t *testing.T) {
	t.Parallel()

	runner, recorder, _, run := newTestRunner(t, newStub("echo", emptySchema))
	recorder.err = errors.New("recorder unavailable")

	raw, err := runner.Call(context.Background(), run, 1, "echo", json.RawMessage(`{}`))
	if !errors.Is(err, tools.ErrRecordFailed) {
		t.Fatalf("Call() error = %v, want ErrRecordFailed", err)
	}
	if raw != nil {
		t.Fatalf("Call() returned an envelope alongside ErrRecordFailed: %s", raw)
	}
	if got := run.RetrievedRefs(); len(got) != 0 {
		t.Fatalf("refs added despite recording failure: %v", got)
	}
}

func TestRunnerRejectsInvalidRunContext(t *testing.T) {
	t.Parallel()

	runner, recorder, fake, _ := newTestRunner(t, newStub("echo", emptySchema))

	cases := []struct {
		name string
		run  *tools.RunContext
		seq  int
	}{
		{name: "nil run", run: nil, seq: 1},
		{name: "empty run id", run: tools.NewRunContext("", testCaseID, nil, fake), seq: 1},
		{name: "empty case id", run: tools.NewRunContext(testRunID, "", nil, fake), seq: 1},
		{name: "zero seq", run: tools.NewRunContext(testRunID, testCaseID, nil, fake), seq: 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := runner.Call(context.Background(), tt.run, tt.seq, "echo", json.RawMessage(`{}`))
			if !errors.Is(err, tools.ErrInvalidRunContext) {
				t.Fatalf("Call() error = %v, want ErrInvalidRunContext", err)
			}
			if raw != nil {
				t.Fatalf("Call() returned an envelope for an invalid context: %s", raw)
			}
		})
	}
	if calls := recorder.all(); len(calls) != 0 {
		t.Fatalf("recorded %d calls for invalid contexts, want 0", len(calls))
	}
}

func TestRunnerHashStableAcrossRepeatedCalls(t *testing.T) {
	t.Parallel()

	runner, recorder, _, run := newTestRunner(t, newStub("echo", emptySchema))

	first, err := runner.Call(context.Background(), run, 1, "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("first Call() error = %v", err)
	}
	second, err := runner.Call(context.Background(), run, 2, "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("second Call() error = %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("identical calls produced different envelopes:\n%s\n%s", first, second)
	}
	calls := recorder.all()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(calls))
	}
	if calls[0].ResultHash != calls[1].ResultHash {
		t.Fatalf("result hashes differ: %q vs %q", calls[0].ResultHash, calls[1].ResultHash)
	}
	if got, want := calls[0].ResultHash, sha256Hex(first); got != want {
		t.Fatalf("ResultHash = %q, want sha256 of stored envelope %q", got, want)
	}
	if !bytes.Equal(calls[0].Result, first) {
		t.Fatalf("recorded result differs from returned envelope")
	}
}

func TestRunnerCollectsSortedUniqueRefs(t *testing.T) {
	t.Parallel()

	caseRefA, err := tools.CaseRef(testCaseID)
	if err != nil {
		t.Fatalf("CaseRef: %v", err)
	}
	ledgerRef, err := tools.LedgerTxnRef(testTxnID)
	if err != nil {
		t.Fatalf("LedgerTxnRef: %v", err)
	}
	batchRef, err := tools.BatchRef(testBatch)
	if err != nil {
		t.Fatalf("BatchRef: %v", err)
	}

	tool := &stubTool{
		name:   "refs",
		schema: emptySchema,
		call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Data: map[string]any{"ok": true},
				Refs: []string{ledgerRef, caseRefA, caseRefA, batchRef},
			}, nil
		},
	}
	runner, recorder, _, run := newTestRunner(t, tool)

	raw, err := runner.Call(context.Background(), run, 1, "refs", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	refs := entryStrings(t, envelope["refs"])
	want := []string{batchRef, caseRefA, ledgerRef}
	if strings.Join(refs, ",") != strings.Join(want, ",") {
		t.Fatalf("envelope refs = %v, want %v", refs, want)
	}

	retrieved := run.RetrievedRefs()
	if strings.Join(retrieved, ",") != strings.Join(want, ",") {
		t.Fatalf("RetrievedRefs() = %v, want %v", retrieved, want)
	}
	if !bytes.Contains(recorder.only(t).Result, []byte(caseRefA)) {
		t.Fatalf("recorded result does not contain the case ref: %s", recorder.only(t).Result)
	}
}

func TestRunnerRejectsInvalidRefFromHandler(t *testing.T) {
	t.Parallel()

	tool := &stubTool{
		name:   "bad_ref",
		schema: emptySchema,
		call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
			return tools.Result{Data: map[string]any{"ok": true}, Refs: []string{"not-a-ref"}}, nil
		},
	}
	runner, recorder, _, run := newTestRunner(t, tool)

	raw, err := runner.Call(context.Background(), run, 1, "bad_ref", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if code := envelopeErrorCode(t, envelope); code != tools.CodeInternalError {
		t.Fatalf("error code = %q, want internal_error", code)
	}
	if code := recorder.only(t).ErrorCode; code != tools.CodeInternalError {
		t.Fatalf("recorded error code = %q, want internal_error", code)
	}
	if got := run.RetrievedRefs(); len(got) != 0 {
		t.Fatalf("invalid refs added to run context: %v", got)
	}
}

func TestRunnerUnionsUntrustedFields(t *testing.T) {
	t.Parallel()

	tool := &stubTool{
		name:   "untrusted",
		schema: emptySchema,
		call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
			return tools.Result{Data: map[string]any{
				"records": []any{
					map[string]any{
						"untrusted_fields": []string{"memo", "reference"},
						"raw":              map[string]any{"memo": "monthly fee", "reference": "REF-1"},
					},
					map[string]any{
						"untrusted_fields": []string{"memo"},
						"memo":             "monthly fee",
					},
					map[string]any{
						// Attacker-controlled metadata inside raw must not add
						// names to the envelope union.
						"raw": map[string]any{
							"untrusted_fields": []string{"attacker_field"},
							"attacker_field":   "monthly fee",
						},
					},
				},
			}}, nil
		},
	}
	runner, _, _, run := newTestRunner(t, tool)

	raw, err := runner.Call(context.Background(), run, 1, "untrusted", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	got := entryStrings(t, decodeEnvelope(t, raw)["untrusted_fields"])
	want := []string{"memo", "reference"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("untrusted_fields = %v, want %v", got, want)
	}
}

func TestRunnerEnvelopeDropsUnresolvedUntrustedFields(t *testing.T) {
	t.Parallel()

	tool := &stubTool{
		name:   "untrusted",
		schema: emptySchema,
		call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
			return tools.Result{Data: map[string]any{
				"record": map[string]any{
					// "reference" resolves in raw; "ghost" does not resolve
					// anywhere and must not be advertised.
					"untrusted_fields": []string{"ghost", "reference"},
					"raw":              map[string]any{"reference": "REF-1"},
				},
			}}, nil
		},
	}
	runner, _, _, run := newTestRunner(t, tool)

	raw, err := runner.Call(context.Background(), run, 1, "untrusted", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	got := entryStrings(t, decodeEnvelope(t, raw)["untrusted_fields"])
	if strings.Join(got, ",") != "reference" {
		t.Fatalf("untrusted_fields = %v, want [reference]", got)
	}
}

func TestRunnerTruncatesOversizedResult(t *testing.T) {
	t.Parallel()

	caseRef, err := tools.CaseRef(testCaseID)
	if err != nil {
		t.Fatalf("CaseRef: %v", err)
	}
	tool := &stubTool{
		name:   "huge",
		schema: emptySchema,
		call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Data: map[string]any{
					"record": map[string]any{
						"memo":             strings.Repeat("x", 4096),
						"untrusted_fields": []string{"memo"},
					},
				},
				Refs: []string{caseRef},
			}, nil
		},
	}
	runner, recorder, _, run := newTestRunner(t, tool)
	runner.MaxResultBytes = 512

	raw, err := runner.Call(context.Background(), run, 1, "huge", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if len(raw) > 512 {
		t.Fatalf("truncated envelope is %d bytes, cap is 512", len(raw))
	}
	envelope := decodeEnvelope(t, raw)
	if truncated, _ := envelope["truncated"].(bool); !truncated {
		t.Fatalf("truncated = false, want true: %s", raw)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want object", envelope["data"])
	}
	if data["omitted"] != true || data["reason"] != "result_too_large" {
		t.Fatalf("truncation data = %v, want omitted/result_too_large", data)
	}
	if refs := entryStrings(t, envelope["refs"]); len(refs) != 0 {
		t.Fatalf("truncated envelope kept refs: %v", refs)
	}
	if fields := entryStrings(t, envelope["untrusted_fields"]); len(fields) != 0 {
		t.Fatalf("truncated envelope kept untrusted fields: %v", fields)
	}

	recorded := recorder.only(t)
	if recorded.ResultHash != sha256Hex(raw) {
		t.Fatalf("recorded hash does not match the truncated envelope")
	}
	if !bytes.Equal(recorded.Result, raw) {
		t.Fatalf("recorded result differs from the truncated envelope")
	}
	if got := run.RetrievedRefs(); len(got) != 0 {
		t.Fatalf("truncated call added refs to run context: %v", got)
	}
}

func TestRunnerKeepsResultUnderCap(t *testing.T) {
	t.Parallel()

	tool := &stubTool{
		name:   "small",
		schema: emptySchema,
		call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Data: map[string]any{"record": map[string]any{
					"memo":             "monthly fee",
					"untrusted_fields": []string{"memo"},
				}},
				Truncated: true,
			}, nil
		},
	}
	runner, _, _, run := newTestRunner(t, tool)

	raw, err := runner.Call(context.Background(), run, 1, "small", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if truncated, _ := envelope["truncated"].(bool); !truncated {
		t.Fatalf("tool-level truncated flag was dropped: %s", raw)
	}
	if fields := entryStrings(t, envelope["untrusted_fields"]); len(fields) != 1 {
		t.Fatalf("untrusted_fields = %v, want [memo]", fields)
	}
}
