package tools_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func TestExtractRefsSuccessEnvelope(t *testing.T) {
	raw := json.RawMessage(`{
		"data": {"id": "` + testRunID + `"},
		"refs": ["ledger_txn:` + testTxnID + `", "ext:psp_a:rec-1"],
		"untrusted_fields": [],
		"truncated": false
	}`)

	refs, err := tools.ExtractRefs(raw)
	if err != nil {
		t.Fatalf("ExtractRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %v, want 2", refs)
	}
	if refs[0] != "ledger_txn:"+testTxnID || refs[1] != "ext:psp_a:rec-1" {
		t.Fatalf("refs = %v", refs)
	}
}

func TestExtractRefsSkipsErrorsAndTruncation(t *testing.T) {
	cases := map[string]json.RawMessage{
		"error envelope": json.RawMessage(`{
			"error": {"code": "not_found", "message": "missing"},
			"refs": [], "untrusted_fields": [], "truncated": false
		}`),
		"truncated": json.RawMessage(`{
			"data": {"omitted": true}, "refs": [], "untrusted_fields": [], "truncated": true
		}`),
		"null error": json.RawMessage(`{
			"error": null, "refs": ["ext:psp_a:rec-1"], "untrusted_fields": [], "truncated": false
		}`),
		"omitted": json.RawMessage(`{}`),
		"empty":   json.RawMessage(``),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			refs, err := tools.ExtractRefs(raw)
			if err != nil {
				t.Fatalf("ExtractRefs: %v", err)
			}
			if name == "null error" {
				if len(refs) != 1 {
					t.Fatalf("refs = %v, want 1", refs)
				}
				return
			}
			if len(refs) != 0 {
				t.Fatalf("refs = %v, want empty", refs)
			}
		})
	}
}

func TestExtractRefsRejectsMalformedAndInvalid(t *testing.T) {
	if _, err := tools.ExtractRefs(json.RawMessage(`not json`)); err == nil {
		t.Fatal("malformed JSON: want error")
	}
	if _, err := tools.ExtractRefs(json.RawMessage(`{"refs": ["ledger_txn:not-a-uuid"], "truncated": false}`)); !errors.Is(err, tools.ErrInvalidRef) {
		t.Fatalf("invalid ref error = %v, want ErrInvalidRef", err)
	}
}
