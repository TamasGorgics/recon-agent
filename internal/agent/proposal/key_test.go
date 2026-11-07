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

func linkKey(t *testing.T, req recordRequest, confidence, rationale string, evidence []string) (string, map[string]any) {
	t.Helper()
	payload, err := buildPayload(req)
	if err != nil {
		t.Fatalf("buildPayload(): %v", err)
	}
	_, fixed, err := parseConfidence(json.Number(confidence))
	if err != nil {
		t.Fatalf("parseConfidence(): %v", err)
	}
	key, err := idempotencyKey(unitCaseID, unitRunID, "link", payload, rationale, evidence, fixed, "")
	if err != nil {
		t.Fatalf("idempotencyKey(): %v", err)
	}
	return key, payload
}

func linkEvidence(req recordRequest) []string {
	refs := []string{"ext:psp_a:rec-1"}
	if req.linkSet {
		for _, id := range req.ledgerTxnIDs {
			refs = append(refs, "ledger_txn:"+id)
		}
		return refs
	}
	return append(refs, "ledger_txn:"+req.ledgerTxnID)
}

// TestLinkSingularGolden pins the pre-composite singular payload and
// idempotency key byte for byte. The M2-08 plural form must not change them.
//
// covers: FIN-12
func TestLinkSingularGolden(t *testing.T) {
	t.Parallel()
	req := recordRequest{kind: "link", ledgerTxnID: unitTxnID, externalRef: "ext:psp_a:rec-1"}
	key, payload := linkKey(t, req, "0.87", "one payout covers three ledger charges",
		[]string{"ext:psp_a:rec-1", "ledger_txn:" + unitTxnID, "case:" + unitCaseID})
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	wantPayload := `{"external_ref":"ext:psp_a:rec-1","ledger_txn_id":"` + unitTxnID + `"}`
	if string(encoded) != wantPayload {
		t.Fatalf("singular payload = %s, want %s", encoded, wantPayload)
	}
	const wantKey = "a6e438042fb990aa8d07ed0369a97e641f3a60a9022e612294169d28d51b3a72"
	if key != wantKey {
		t.Fatalf("singular key = %s, want pre-change golden %s", key, wantKey)
	}
}

// TestLinkPluralKeySetSemantics proves the plural payload/key is the canonical
// exact set: input order is irrelevant, a one-element plural is a distinct wire
// form, and changing one leg changes the key.
//
// covers: FIN-12
func TestLinkPluralKeySetSemantics(t *testing.T) {
	t.Parallel()
	first := "77777777-7777-4777-8777-777777777777"
	second := "88888888-8888-4888-8888-888888888888"
	third := "99999999-9999-4999-8999-999999999998"

	ordered := recordRequest{kind: "link", linkSet: true,
		ledgerTxnIDs: []string{first, second, third}, externalRef: "ext:psp_a:rec-1"}
	reordered := recordRequest{kind: "link", linkSet: true,
		ledgerTxnIDs: []string{third, first, second}, externalRef: "ext:psp_a:rec-1"}
	orderedKey, orderedPayload := linkKey(t, ordered, "0.87", "one payout covers three ledger charges", linkEvidence(ordered))
	reorderedKey, reorderedPayload := linkKey(t, reordered, "0.87", "one payout covers three ledger charges", linkEvidence(reordered))
	if orderedKey != reorderedKey {
		t.Fatalf("leg order changed the key: %s != %s", orderedKey, reorderedKey)
	}
	orderedJSON, _ := json.Marshal(orderedPayload)
	reorderedJSON, _ := json.Marshal(reorderedPayload)
	if string(orderedJSON) != string(reorderedJSON) {
		t.Fatalf("leg order changed the payload: %s != %s", orderedJSON, reorderedJSON)
	}
	if ids := orderedPayload["ledger_txn_ids"].([]string); len(ids) != 3 ||
		ids[0] != first || ids[1] != second || ids[2] != third {
		t.Fatalf("plural payload is not sorted: %v", orderedPayload["ledger_txn_ids"])
	}
	if ordered.ledgerTxnIDs[0] != first {
		t.Fatal("buildPayload mutated the request order")
	}

	singular := recordRequest{kind: "link", ledgerTxnID: first, externalRef: "ext:psp_a:rec-1"}
	singularKey, singularPayload := linkKey(t, singular, "0.87", "one payout covers three ledger charges", linkEvidence(singular))
	one := recordRequest{kind: "link", linkSet: true,
		ledgerTxnIDs: []string{first}, externalRef: "ext:psp_a:rec-1"}
	oneKey, onePayload := linkKey(t, one, "0.87", "one payout covers three ledger charges", linkEvidence(one))
	if singularKey == oneKey {
		t.Fatal("singular and one-element plural collapsed to the same key")
	}
	if _, ok := singularPayload["ledger_txn_ids"]; ok {
		t.Fatal("singular payload contains ledger_txn_ids")
	}
	if ids, ok := onePayload["ledger_txn_ids"].([]string); !ok || len(ids) != 1 || ids[0] != first {
		t.Fatalf("one-element plural payload = %v", onePayload)
	}

	changed := recordRequest{kind: "link", linkSet: true,
		ledgerTxnIDs: []string{first, second, third, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
		externalRef:  "ext:psp_a:rec-1"}
	changedKey, _ := linkKey(t, changed, "0.87", "one payout covers three ledger charges", linkEvidence(changed))
	if orderedKey == changedKey {
		t.Fatal("adding one leg did not change the key")
	}

	reversedEvidence := []string{"ledger_txn:" + third, "ledger_txn:" + first, "ext:psp_a:rec-1", "ledger_txn:" + second}
	evidenceKey, _ := linkKey(t, ordered, "0.87", "one payout covers three ledger charges", reversedEvidence)
	if orderedKey != evidenceKey {
		t.Fatal("evidence order changed the key")
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
