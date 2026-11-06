package proposal

import (
	"encoding/json"
	"testing"
)

func testAdjustmentRequest() recordRequest {
	return recordRequest{
		kind: "adjustment",
		lines: []lineInput{
			{AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 250, Currency: "USD"},
			{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 250, Currency: "USD"},
		},
	}
}

func testKey(t *testing.T, req recordRequest) (string, map[string]any) {
	t.Helper()
	payload, err := buildPayload(req)
	if err != nil {
		t.Fatalf("buildPayload(): %v", err)
	}
	_, confidence, err := parseConfidence(json.Number("0.9"))
	if err != nil {
		t.Fatalf("parseConfidence(): %v", err)
	}
	key, err := idempotencyKey(unitCaseID, unitRunID, req.kind, payload, "fee variance",
		[]string{"case:" + unitCaseID, "ledger_txn:" + unitTxnID}, confidence, req.pattern)
	if err != nil {
		t.Fatalf("idempotencyKey(): %v", err)
	}
	return key, payload
}

// TestIdempotencyKey proves the key is deterministic, ignores line/evidence
// ordering, and changes with every semantic input.
//
// covers: FIN-12
func TestIdempotencyKey(t *testing.T) {
	t.Parallel()
	first, payload := testKey(t, testAdjustmentRequest())
	if len(first) != 64 {
		t.Fatalf("key length = %d, want 64 hex chars", len(first))
	}
	if payload["lines"].([]map[string]any)[0]["account_code"] != "fees_expense_usd" {
		t.Fatalf("lines are not sorted: %+v", payload["lines"])
	}

	reordered := testAdjustmentRequest()
	reordered.lines[0], reordered.lines[1] = reordered.lines[1], reordered.lines[0]
	second, _ := testKey(t, reordered)
	if first != second {
		t.Fatalf("line order changed the key: %s != %s", first, second)
	}

	duplicateEvidence, _ := idempotencyKey(unitCaseID, unitRunID, "adjustment", payload, "fee variance",
		[]string{"ledger_txn:" + unitTxnID, "case:" + unitCaseID, "case:" + unitCaseID}, "0.9000", "")
	if first != duplicateEvidence {
		t.Fatalf("evidence order/duplicates changed the key")
	}

	changedPayload, _ := buildPayload(recordRequest{
		kind: "adjustment",
		lines: []lineInput{
			{AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 251, Currency: "USD"},
			{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 251, Currency: "USD"},
		},
	})
	changed, _ := idempotencyKey(unitCaseID, unitRunID, "adjustment", changedPayload, "fee variance",
		[]string{"case:" + unitCaseID, "ledger_txn:" + unitTxnID}, "0.9000", "")
	if first == changed {
		t.Fatal("amount change did not change the key")
	}

	pattern, _ := idempotencyKey(unitCaseID, unitRunID, "adjustment", payload, "fee variance",
		[]string{"case:" + unitCaseID, "ledger_txn:" + unitTxnID}, "0.9000", "rounding")
	if first == pattern {
		t.Fatal("pattern change did not change the key")
	}

	otherCase, _ := idempotencyKey(otherCaseID, unitRunID, "adjustment", payload, "fee variance",
		[]string{"case:" + unitCaseID, "ledger_txn:" + unitTxnID}, "0.9000", "")
	if first == otherCase {
		t.Fatal("case change did not change the key")
	}
}

func TestParseConfidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw      string
		want     string
		wantFail bool
	}{
		{raw: "0", want: "0.0000"},
		{raw: "1", want: "1.0000"},
		{raw: "0.9", want: "0.9000"},
		{raw: "0.12345", want: "0.1235"},
		{raw: "0.99999", want: "1.0000"},
		{raw: "-0.1", wantFail: true},
		{raw: "1.01", wantFail: true},
		{raw: "not-a-number", wantFail: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.raw, func(t *testing.T) {
			t.Parallel()
			_, fixed, err := parseConfidence(json.Number(test.raw))
			if test.wantFail {
				if err == nil {
					t.Fatalf("parseConfidence(%q) error = nil", test.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConfidence(%q): %v", test.raw, err)
			}
			if fixed != test.want {
				t.Fatalf("parseConfidence(%q) = %q, want %q", test.raw, fixed, test.want)
			}
		})
	}
}
