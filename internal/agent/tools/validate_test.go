package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func TestArgumentValidation(t *testing.T) {
	t.Parallel()

	valid := `{"case_id":"` + testCaseID + `","amount_minor":12,"currency":"USD","date":"2026-01-02","mode":"a","nested":{"id":"x"}}`

	tests := []struct {
		name      string
		args      json.RawMessage
		wantCode  string
		wantValid bool
	}{
		{name: "valid", args: json.RawMessage(valid), wantValid: true},
		{name: "unknown top-level field", args: json.RawMessage(strings.Replace(valid, `"nested"`, `"extra":1,"nested"`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "unknown nested field", args: json.RawMessage(strings.Replace(valid, `"id":"x"`, `"id":"x","other":true`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "missing required", args: json.RawMessage(`{"case_id":"` + testCaseID + `"}`), wantCode: tools.CodeInvalidArguments},
		{name: "wrong amount type", args: json.RawMessage(strings.Replace(valid, `"amount_minor":12`, `"amount_minor":"12"`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "wrong currency type", args: json.RawMessage(strings.Replace(valid, `"currency":"USD"`, `"currency":12`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "bad currency pattern", args: json.RawMessage(strings.Replace(valid, `"currency":"USD"`, `"currency":"usd"`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "bad date pattern", args: json.RawMessage(strings.Replace(valid, `"date":"2026-01-02"`, `"date":"2026-1-2"`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "bad uuid pattern", args: json.RawMessage(strings.Replace(valid, `"case_id":"`+testCaseID+`"`, `"case_id":"not-a-uuid"`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "bad enum", args: json.RawMessage(strings.Replace(valid, `"mode":"a"`, `"mode":"c"`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "nested object wrong type", args: json.RawMessage(strings.Replace(valid, `"nested":{"id":"x"}`, `"nested":"x"`, 1)), wantCode: tools.CodeInvalidArguments},
		{name: "null args", args: json.RawMessage(`null`), wantCode: tools.CodeInvalidArguments},
		{name: "empty args", args: nil, wantCode: tools.CodeInvalidArguments},
		{name: "syntactically invalid JSON", args: json.RawMessage(`{"amount_minor":`), wantCode: tools.CodeInvalidArguments},
		{name: "trailing data", args: json.RawMessage(valid + `{}`), wantCode: tools.CodeInvalidArguments},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runner, recorder, _, run := newTestRunner(t, newStub("echo", echoSchema))

			raw, err := runner.Call(context.Background(), run, 1, "echo", tt.args)
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			envelope := decodeEnvelope(t, raw)
			gotCode := envelopeErrorCode(t, envelope)

			if tt.wantValid {
				if gotCode != "" {
					t.Fatalf("valid args rejected: %s", raw)
				}
				if _, ok := envelope["data"].(map[string]any); !ok {
					t.Fatalf("success envelope has no data object: %s", raw)
				}
				return
			}
			if gotCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q\n%s", gotCode, tt.wantCode, raw)
			}
			recorded := recorder.only(t)
			if recorded.ErrorCode != tt.wantCode {
				t.Fatalf("recorded error code = %q, want %q", recorded.ErrorCode, tt.wantCode)
			}
		})
	}
}

// covers: FIN-05
func TestAmountMinorRejectsFractionalAndNonIntegerJSON(t *testing.T) {
	t.Parallel()

	base := `"case_id":"` + testCaseID + `","currency":"USD","date":"2026-01-02","mode":"a","nested":{"id":"x"}`

	rejected := []struct {
		name  string
		value string
	}{
		{name: "fraction", value: `12.5`},
		{name: "string", value: `"12"`},
		{name: "trailing zero", value: `12.0`},
		{name: "exponent", value: `1e3`},
		{name: "null", value: `null`},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runner, recorder, _, run := newTestRunner(t, newStub("echo", echoSchema))
			args := json.RawMessage(`{` + base + `,"amount_minor":` + tt.value + `}`)

			raw, err := runner.Call(context.Background(), run, 1, "echo", args)
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			envelope := decodeEnvelope(t, raw)
			if code := envelopeErrorCode(t, envelope); code != tools.CodeInvalidArguments {
				t.Fatalf("amount_minor=%s: error code = %q, want invalid_arguments\n%s", tt.value, code, raw)
			}
			if got := recorder.only(t).ErrorCode; got != tools.CodeInvalidArguments {
				t.Fatalf("recorded error code = %q, want invalid_arguments", got)
			}
		})
	}

	t.Run("integer accepted", func(t *testing.T) {
		t.Parallel()
		runner, _, _, run := newTestRunner(t, newStub("echo", echoSchema))
		args := json.RawMessage(`{` + base + `,"amount_minor":12}`)

		raw, err := runner.Call(context.Background(), run, 1, "echo", args)
		if err != nil {
			t.Fatalf("Call() error = %v", err)
		}
		if code := envelopeErrorCode(t, decodeEnvelope(t, raw)); code != "" {
			t.Fatalf("integer amount rejected: %s", raw)
		}
	})
}

// covers: FIN-05
func TestExplicitIntegerFieldRejectsNonCanonicalNumbers(t *testing.T) {
	t.Parallel()

	schema := `{
		"type":"object",
		"additionalProperties":false,
		"required":["amount"],
		"properties":{"amount":{"type":"integer"}}
	}`

	for _, value := range []string{`1e3`, `12.0`} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			runner, _, _, run := newTestRunner(t, newStub("integer_field", schema))
			raw, err := runner.Call(context.Background(), run, 1, "integer_field", json.RawMessage(`{"amount":`+value+`}`))
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			if code := envelopeErrorCode(t, decodeEnvelope(t, raw)); code != tools.CodeInvalidArguments {
				t.Fatalf("amount=%s: error code = %q, want invalid_arguments", value, code)
			}
		})
	}

	runner, _, _, run := newTestRunner(t, newStub("integer_field", schema))
	raw, err := runner.Call(context.Background(), run, 1, "integer_field", json.RawMessage(`{"amount":1000}`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if code := envelopeErrorCode(t, decodeEnvelope(t, raw)); code != "" {
		t.Fatalf("integer value rejected: %s", raw)
	}
}

func TestInvalidJSONArgsRecordedWithSentinel(t *testing.T) {
	t.Parallel()

	runner, recorder, _, run := newTestRunner(t, newStub("echo", echoSchema))
	raw, err := runner.Call(context.Background(), run, 1, "echo", json.RawMessage(`{"nope":`))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if code := envelopeErrorCode(t, decodeEnvelope(t, raw)); code != tools.CodeInvalidArguments {
		t.Fatalf("error code = %q, want invalid_arguments", code)
	}

	recorded := recorder.only(t)
	var sentinel map[string]any
	if err := json.Unmarshal(recorded.Args, &sentinel); err != nil {
		t.Fatalf("recorded args are not JSON: %v", err)
	}
	if sentinel["_invalid_json"] != true {
		t.Fatalf("recorded args = %s, want the _invalid_json sentinel", recorded.Args)
	}
}

func TestNilArgsAllowedForNoArgumentTool(t *testing.T) {
	t.Parallel()

	runner, _, _, run := newTestRunner(t, newStub("noargs", emptySchema))
	raw, err := runner.Call(context.Background(), run, 1, "noargs", nil)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if code := envelopeErrorCode(t, decodeEnvelope(t, raw)); code != "" {
		t.Fatalf("no-argument tool rejected nil args: %s", raw)
	}
}
