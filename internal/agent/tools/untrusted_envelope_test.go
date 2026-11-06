package tools_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

type recordingWrapper struct {
	fields []string
}

func (w *recordingWrapper) Wrap(field, value string) string {
	w.fields = append(w.fields, field)
	return "<<" + field + ":" + value + ">>"
}

func decodeWrappedEnvelope(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return value
}

func TestWrapUntrustedEnvelope(t *testing.T) {
	envelope := json.RawMessage(`{
		"data": {
			"records": [
				{
					"id": "ext-1",
					"memo": "memo text",
					"amount_minor": 1250,
					"untrusted_fields": ["memo", "amount_minor", "missing"],
					"raw": {"memo": "raw memo", "untrusted_fields": ["memo"]}
				},
				{
					"id": "ext-2",
					"memo": "second memo",
					"untrusted_fields": ["memo"]
				}
			],
			"nested": {"memo": "not declared here"},
			"untrusted_fields": ["memo"]
		},
		"refs": ["external_record:psp_a:ext-1"],
		"truncated": true,
		"untrusted_fields": ["memo"]
	}`)
	wrapper := &recordingWrapper{}
	wrapped, err := tools.WrapUntrustedEnvelope(envelope, wrapper)
	if err != nil {
		t.Fatalf("WrapUntrustedEnvelope: %v", err)
	}
	root := decodeWrappedEnvelope(t, wrapped)

	if root["truncated"] != true {
		t.Fatalf("truncated = %v, want true", root["truncated"])
	}
	refs, ok := root["refs"].([]any)
	if !ok || len(refs) != 1 || refs[0] != "external_record:psp_a:ext-1" {
		t.Fatalf("refs = %#v", root["refs"])
	}
	// The top-level declaration resolves only against the top-level object,
	// which has no memo field, so the nested memo must stay untouched.
	data := root["data"].(map[string]any)
	nested := data["nested"].(map[string]any)
	if nested["memo"] != "not declared here" {
		t.Fatalf("object-local rule violated: nested memo = %v", nested["memo"])
	}

	records := data["records"].([]any)
	first := records[0].(map[string]any)
	if first["memo"] != "<<memo:memo text>>" {
		t.Fatalf("direct memo = %v", first["memo"])
	}
	if first["amount_minor"] != float64(1250) {
		t.Fatalf("non-string value was rewritten: %v", first["amount_minor"])
	}
	raw := first["raw"].(map[string]any)
	if raw["memo"] != "<<memo:raw memo>>" {
		t.Fatalf("raw memo = %v", raw["memo"])
	}
	second := records[1].(map[string]any)
	if second["memo"] != "<<memo:second memo>>" {
		t.Fatalf("array element memo = %v", second["memo"])
	}

	if len(wrapper.fields) != 3 {
		t.Fatalf("wrapped fields = %v, want memo, memo (raw), memo (second record)", wrapper.fields)
	}
}

func TestWrapUntrustedEnvelopeIgnoresAttackerRawMetadata(t *testing.T) {
	envelope := json.RawMessage(`{
		"data": {
			"record": {
				"memo": "memo text",
				"raw": {"memo": "attacker payload", "untrusted_fields": ["memo"]}
			}
		},
		"refs": [],
		"truncated": false
	}`)
	wrapper := &recordingWrapper{}
	wrapped, err := tools.WrapUntrustedEnvelope(envelope, wrapper)
	if err != nil {
		t.Fatalf("WrapUntrustedEnvelope: %v", err)
	}
	if len(wrapper.fields) != 0 {
		t.Fatalf("attacker-supplied raw.untrusted_fields was honoured: %v", wrapper.fields)
	}
	root := decodeWrappedEnvelope(t, wrapped)
	record := root["data"].(map[string]any)["record"].(map[string]any)
	if record["memo"] != "memo text" {
		t.Fatalf("memo was wrapped without a declaration: %v", record["memo"])
	}
	if record["raw"].(map[string]any)["memo"] != "attacker payload" {
		t.Fatalf("raw memo was wrapped without a declaration")
	}
}

func TestWrapUntrustedEnvelopePreservesErrorEnvelope(t *testing.T) {
	envelope := json.RawMessage(`{"error":{"code":"invalid_arguments","message":"bad args"},"refs":[],"truncated":false}`)
	wrapper := &recordingWrapper{}
	wrapped, err := tools.WrapUntrustedEnvelope(envelope, wrapper)
	if err != nil {
		t.Fatalf("WrapUntrustedEnvelope: %v", err)
	}
	root := decodeWrappedEnvelope(t, wrapped)
	errorBody, ok := root["error"].(map[string]any)
	if !ok || errorBody["code"] != "invalid_arguments" {
		t.Fatalf("error envelope was changed: %s", wrapped)
	}
	if len(wrapper.fields) != 0 {
		t.Fatalf("error envelope wrapped fields: %v", wrapper.fields)
	}
}

func TestWrapUntrustedEnvelopeRejectsNilWrapper(t *testing.T) {
	_, err := tools.WrapUntrustedEnvelope(json.RawMessage(`{"data":{}}`), nil)
	if err == nil || !strings.Contains(err.Error(), "nil wrapper") {
		t.Fatalf("error = %v, want nil wrapper error", err)
	}
}

func TestWrapUntrustedEnvelopeEscapesBoundaryInValue(t *testing.T) {
	// The wrapper itself escapes the boundary token; this test proves the
	// envelope walker routes the value through FieldWrapper rather than
	// rebuilding the text.
	envelope := json.RawMessage(`{"data":{"memo":"token=abc","untrusted_fields":["memo"]}}`)
	wrapped, err := tools.WrapUntrustedEnvelope(envelope, escapeTokenWrapper{})
	if err != nil {
		t.Fatalf("WrapUntrustedEnvelope: %v", err)
	}
	if strings.Contains(string(wrapped), "abc") {
		t.Fatalf("boundary token was not escaped: %s", wrapped)
	}
}

type escapeTokenWrapper struct{}

func (escapeTokenWrapper) Wrap(field, value string) string {
	return "<<" + field + ":" + strings.ReplaceAll(value, "abc", "[escaped]") + ">>"
}
