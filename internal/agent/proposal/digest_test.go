package proposal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func digestOf(t *testing.T, fields Fields) string {
	t.Helper()
	digest, err := Digest(fields)
	if err != nil {
		t.Fatalf("Digest(): %v", err)
	}
	return digest
}

// TestDigestCanonicalDocument pins the exact canonical bytes the digest covers:
// sorted keys at every level, evidence sorted unique, null and "" distinct, and
// no HTML escaping.
func TestDigestCanonicalDocument(t *testing.T) {
	t.Parallel()
	fields := Fields{
		Type:       "request_info",
		Payload:    map[string]any{"z": json.Number("1.50"), "question": "a<&>b"},
		Rationale:  "why <not>",
		Evidence:   []string{"b", "a", "a"},
		Confidence: "0.5000",
	}
	wantDocument := `{"confidence":"0.5000","evidence":["a","b"],"pattern_claimed":null,"payload":{"question":"a<&>b","z":1.50},"rationale":"why <not>","type":"request_info"}`
	sum := sha256.Sum256([]byte(wantDocument))
	want := hex.EncodeToString(sum[:])
	if got := digestOf(t, fields); got != want {
		t.Fatalf("Digest() = %s, want %s\ncanonical document: %s", got, want, wantDocument)
	}
}

// TestDigestEvidenceOrderInsensitive proves recorded evidence ordering is not
// part of proposal identity.
func TestDigestEvidenceOrderInsensitive(t *testing.T) {
	t.Parallel()
	first := Fields{
		Type: "link", Rationale: "why", Confidence: "0.9000",
		Payload:  map[string]any{"ledger_txn_id": unitTxnID, "external_ref": "ext:psp_a:rec-1"},
		Evidence: []string{"ledger_txn:" + unitTxnID, "case:" + unitCaseID},
	}
	second := first
	second.Evidence = []string{"case:" + unitCaseID, "ledger_txn:" + unitTxnID, "case:" + unitCaseID}
	if digestOf(t, first) != digestOf(t, second) {
		t.Fatal("evidence order or duplicates changed the digest")
	}
}

// TestDigestPatternNullVsEmpty proves the approved null != "" rule.
func TestDigestPatternNullVsEmpty(t *testing.T) {
	t.Parallel()
	nilPattern := Fields{Type: "link", Rationale: "why", Confidence: "0.9000",
		Payload: map[string]any{"ledger_txn_id": unitTxnID}, Evidence: []string{"case:" + unitCaseID}}
	emptyPattern := nilPattern
	empty := ""
	emptyPattern.PatternClaimed = &empty
	if digestOf(t, nilPattern) == digestOf(t, emptyPattern) {
		t.Fatal("pattern_claimed null and empty string produced the same digest")
	}
	claimed := "rounding"
	claimedPattern := nilPattern
	claimedPattern.PatternClaimed = &claimed
	if digestOf(t, claimedPattern) == digestOf(t, nilPattern) || digestOf(t, claimedPattern) == digestOf(t, emptyPattern) {
		t.Fatal("pattern_claimed value collided with null or empty")
	}
}

// TestNormalizeConfidenceFourDecimals proves stored and wire confidence forms
// hash identically after the fixed four-decimal normalization.
func TestNormalizeConfidenceFourDecimals(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"0.85":    "0.8500",
		"0.8500":  "0.8500",
		"0.12345": "0.1235",
		"1":       "1.0000",
		"0":       "0.0000",
	}
	for input, want := range cases {
		got, err := NormalizeConfidence(input)
		if err != nil {
			t.Fatalf("NormalizeConfidence(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeConfidence(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := NormalizeConfidence("nope"); err == nil {
		t.Fatal("NormalizeConfidence(nope) error = nil")
	}
}

// TestIdempotencyKeyMatchesRecorder proves the exported pure key derivation is
// byte-identical to the unexported production helper.
func TestIdempotencyKeyMatchesRecorder(t *testing.T) {
	t.Parallel()
	fields := Fields{
		Type:       "adjustment",
		Payload:    map[string]any{"lines": []map[string]any{{"account_code": "suspense_usd", "direction": "credit", "amount_minor": 250, "currency": "USD"}}},
		Rationale:  "fee variance",
		Evidence:   []string{"case:" + unitCaseID, "ledger_txn:" + unitTxnID},
		Confidence: "0.9000",
	}
	want, err := idempotencyKey(unitCaseID, unitRunID, fields.Type, fields.Payload,
		fields.Rationale, fields.Evidence, fields.Confidence, "")
	if err != nil {
		t.Fatalf("idempotencyKey(): %v", err)
	}
	got, err := IdempotencyKey(unitCaseID, unitRunID, fields)
	if err != nil {
		t.Fatalf("IdempotencyKey(): %v", err)
	}
	if got != want {
		t.Fatalf("IdempotencyKey() = %s, want %s", got, want)
	}
}

// TestRebuildNormalizesProposalFields proves the replay rebuild path applies
// the same sanitization, evidence, confidence, payload and pattern rules as the
// recording pipeline.
func TestRebuildNormalizesProposalFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		kind          string
		args          map[string]any
		wantType      string
		wantPayload   map[string]any
		wantRationale string
		wantEvidence  []string
		wantPattern   *string
	}{
		{
			name:          "link",
			kind:          "link",
			args:          linkArgs(),
			wantType:      "link",
			wantPayload:   map[string]any{"ledger_txn_id": unitTxnID, "external_ref": "ext:psp_a:rec-1"},
			wantRationale: "fee variance on the settlement batch",
			wantEvidence:  []string{"case:" + unitCaseID},
		},
		{
			name:          "adjustment lines sorted",
			kind:          "adjustment",
			args:          adjustmentArgs(),
			wantType:      "adjustment",
			wantPayload:   map[string]any{"lines": []map[string]any{{"account_code": "fees_expense_usd", "direction": "debit", "amount_minor": int64(100), "currency": "USD"}, {"account_code": "suspense_usd", "direction": "credit", "amount_minor": int64(100), "currency": "USD"}}},
			wantRationale: "fee variance on the settlement batch",
			wantEvidence:  []string{"case:" + unitCaseID},
		},
		{
			name:          "write off",
			kind:          "write_off",
			args:          adjustmentArgs(),
			wantType:      "write_off",
			wantPayload:   map[string]any{"lines": []map[string]any{{"account_code": "fees_expense_usd", "direction": "debit", "amount_minor": int64(100), "currency": "USD"}, {"account_code": "suspense_usd", "direction": "credit", "amount_minor": int64(100), "currency": "USD"}}},
			wantRationale: "fee variance on the settlement batch",
			wantEvidence:  []string{"case:" + unitCaseID},
		},
		{
			name:          "duplicate",
			kind:          "duplicate",
			args:          duplicateArgs(),
			wantType:      "duplicate",
			wantPayload:   map[string]any{"external_ref": "ext:psp_a:rec-1"},
			wantRationale: "fee variance on the settlement batch",
			wantEvidence:  []string{"case:" + unitCaseID},
		},
		{
			name:          "request info",
			kind:          "request_info",
			args:          requestInfoArgs(),
			wantType:      "request_info",
			wantPayload:   map[string]any{"question": "Which invoice does this payout settle?"},
			wantRationale: "fee variance on the settlement batch",
			wantEvidence:  []string{"case:" + unitCaseID},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields, err := Rebuild(test.kind, mustJSON(t, test.args), unitCaseID)
			if err != nil {
				t.Fatalf("Rebuild(): %v", err)
			}
			if fields.Type != test.wantType {
				t.Fatalf("type = %q, want %q", fields.Type, test.wantType)
			}
			if fields.Rationale != test.wantRationale {
				t.Fatalf("rationale = %q, want %q", fields.Rationale, test.wantRationale)
			}
			if fields.Confidence != "0.9000" {
				t.Fatalf("confidence = %q, want 0.9000", fields.Confidence)
			}
			if fields.PatternClaimed != test.wantPattern {
				t.Fatalf("pattern = %v, want %v", fields.PatternClaimed, test.wantPattern)
			}
			assertFieldsPayload(t, fields.Payload, test.wantPayload)
			assertEvidence(t, fields.Evidence, test.wantEvidence)
		})
	}
}

// TestRebuildPatternEmptyBecomesNull reproduces the recorder's empty -> SQL NULL
// rule for pattern_claimed.
func TestRebuildPatternEmptyBecomesNull(t *testing.T) {
	t.Parallel()
	args := adjustmentArgs()
	args["pattern"] = ""
	fields, err := Rebuild("adjustment", mustJSON(t, args), unitCaseID)
	if err != nil {
		t.Fatalf("Rebuild(): %v", err)
	}
	if fields.PatternClaimed != nil {
		t.Fatalf("pattern = %q, want nil for empty input", *fields.PatternClaimed)
	}
	args["pattern"] = "rounding"
	fields, err = Rebuild("adjustment", mustJSON(t, args), unitCaseID)
	if err != nil {
		t.Fatalf("Rebuild(): %v", err)
	}
	if fields.PatternClaimed == nil || *fields.PatternClaimed != "rounding" {
		t.Fatalf("pattern = %v, want rounding", fields.PatternClaimed)
	}
}

// TestRebuildRejectsWrongCase proves the replay rebuild path applies the same
// run/case binding rule as the production tool.
func TestRebuildRejectsWrongCase(t *testing.T) {
	t.Parallel()
	args := adjustmentArgs()
	args["case_id"] = otherCaseID
	if _, err := Rebuild("adjustment", mustJSON(t, args), unitCaseID); err == nil {
		t.Fatal("Rebuild() wrong case error = nil")
	}
}

func assertFieldsPayload(t *testing.T, got, want map[string]any) {
	t.Helper()
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got payload: %v", err)
	}
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want payload: %v", err)
	}
	if string(gotRaw) != string(wantRaw) {
		t.Fatalf("payload = %s, want %s", gotRaw, wantRaw)
	}
}

func assertEvidence(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("evidence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("evidence = %v, want %v", got, want)
		}
	}
}
