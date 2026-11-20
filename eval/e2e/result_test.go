package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

func TestResolutionForType(t *testing.T) {
	cases := map[string]string{
		"link":         "link",
		"adjustment":   "adjustment",
		"write_off":    "write_off",
		"duplicate":    "duplicate",
		"request_info": "",
		"unknown":      "",
	}
	for proposalType, want := range cases {
		if got := resolutionForType(proposalType); got != want {
			t.Errorf("resolutionForType(%q) = %q, want %q", proposalType, got, want)
		}
	}
}

func TestParseExternalRef(t *testing.T) {
	valid := []struct {
		ref      string
		source   string
		recordID string
	}{
		{"ext:psp_a:rec-1", "psp_a", "rec-1"},
		{"ext:bank_x:0012:34", "bank_x", "0012:34"},
	}
	for _, tc := range valid {
		source, recordID, ok := parseExternalRef(tc.ref)
		if !ok || source != tc.source || recordID != tc.recordID {
			t.Errorf("parseExternalRef(%q) = (%q, %q, %v), want (%q, %q, true)",
				tc.ref, source, recordID, ok, tc.source, tc.recordID)
		}
	}
	invalid := []string{"", " psp_a:rec-1", "ext:", "ext:psp_a:", "ext::rec", "psp_a:rec-1", "ext:psp_a:rec 1"}
	for _, ref := range invalid {
		if source, recordID, ok := parseExternalRef(ref); ok {
			t.Errorf("parseExternalRef(%q) = (%q, %q, true), want false", ref, source, recordID)
		}
	}
}

func TestAdjustmentPayloadFacts(t *testing.T) {
	payload := []byte(`{"lines":[
		{"account_code":"fees_expense","direction":"debit","amount_minor":700,"currency":"USD"},
		{"account_code":"suspense","direction":"credit","amount_minor":700,"currency":"USD"}
	]}`)
	amount, currency, err := adjustmentPayloadFacts(payload)
	if err != nil {
		t.Fatalf("adjustmentPayloadFacts: %v", err)
	}
	if amount != 700 || currency != "USD" {
		t.Fatalf("adjustment facts = (%d, %q), want (700, USD)", amount, currency)
	}

	mixed := []byte(`{"lines":[
		{"direction":"debit","amount_minor":100,"currency":"USD"},
		{"direction":"debit","amount_minor":200,"currency":"EUR"}
	]}`)
	amount, currency, err = adjustmentPayloadFacts(mixed)
	if err != nil {
		t.Fatalf("adjustmentPayloadFacts mixed: %v", err)
	}
	if amount != 300 || currency != "" {
		t.Fatalf("mixed facts = (%d, %q), want (300, empty)", amount, currency)
	}

	if _, _, err := adjustmentPayloadFacts([]byte("not json")); err == nil {
		t.Fatal("malformed payload did not fail")
	}
}

func TestCitedRefs(t *testing.T) {
	payload := []byte(`{"ledger_txn_ids":["b","a"],"external_ref":"ext:psp_a:dup-2","duplicate_of":"ext:psp_a:dup-1"}`)
	refs, err := citedRefs(payload)
	if err != nil {
		t.Fatalf("citedRefs: %v", err)
	}
	if len(refs.ledgerRefs) != 2 || refs.ledgerRefs[0] != "b" || refs.ledgerRefs[1] != "a" {
		t.Fatalf("ledger refs = %v", refs.ledgerRefs)
	}
	if len(refs.externalRefs) != 2 || refs.externalPrimary != "ext:psp_a:dup-2" {
		t.Fatalf("external refs = %v primary=%q", refs.externalRefs, refs.externalPrimary)
	}

	singular := []byte(`{"ledger_txn_id":"abc","external_ref":"ext:psp_a:rec"}`)
	refs, err = citedRefs(singular)
	if err != nil {
		t.Fatalf("citedRefs singular: %v", err)
	}
	if len(refs.ledgerRefs) != 1 || refs.ledgerRefs[0] != "abc" {
		t.Fatalf("singular ledger refs = %v", refs.ledgerRefs)
	}
}

func TestBreakKindForSides(t *testing.T) {
	cases := []struct {
		sides []string
		want  string
	}{
		{[]string{"external_only"}, "other"},
		{[]string{"ledger_only"}, "other"},
		{[]string{"amount_mismatch"}, "fee_variance"},
		{[]string{"external_only", "amount_mismatch"}, "fee_variance"},
		{[]string{"duplicate", "amount_mismatch"}, "duplicate_charge"},
		{[]string{"other"}, "other"},
	}
	for _, tc := range cases {
		if got := breakKindForSides(tc.sides); got != tc.want {
			t.Errorf("breakKindForSides(%v) = %q, want %q", tc.sides, got, tc.want)
		}
	}
}

func fullResult(t *testing.T, asOf time.Time) Result {
	t.Helper()
	usd := money.USD
	tie := tieout.Result{
		ScopeID:          "10000000-0000-4000-8000-000000000001",
		AsOf:             asOf,
		Currency:         "USD",
		LedgerBalance:    money.FromMinor(1000, usd),
		StatementClosing: money.FromMinor(700, usd),
		LedgerOnly:       money.FromMinor(200, usd),
		ExternalOnly:     money.FromMinor(300, usd),
		Unexplained:      money.FromMinor(400, usd),
	}
	tier := "T1"
	claimed := "T2"
	return Result{
		Schema:           SchemaVersion,
		Proposal:         ProposalPresent,
		ProposalID:       "20000000-0000-4000-8000-000000000001",
		RequestStatus:    RequestStatusApplied,
		PolicyTier:       &tier,
		AgentClaimedTier: &claimed,
		ApproverAction:   ActionApprove,
		ApproverRule:     "exact_match",
		Applied:          true,
		LedgerTxnID:      "30000000-0000-4000-8000-000000000001",
		Tieout:           tieOutFrom(tie),
		Verifier:         &Verifier{Evaluated: true, Passed: true, Pattern: "fee_variance"},
	}
}

func TestResultDetailShape(t *testing.T) {
	result := fullResult(t, time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))
	if err := result.Validate(); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	document := result.Detail()
	if got := document[DetailKey]; got != result {
		t.Fatalf("Detail()[%q] = %#v, want the result", DetailKey, got)
	}
	encoded, err := result.DetailJSON()
	if err != nil {
		t.Fatalf("DetailJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if decoded["schema"] != SchemaVersion || decoded["request_status"] != RequestStatusApplied {
		t.Fatalf("decoded schema/status = %v/%v", decoded["schema"], decoded["request_status"])
	}
	tie, ok := decoded["tieout"].(map[string]any)
	if !ok {
		t.Fatalf("tieout = %#v, want object", decoded["tieout"])
	}
	if tie["unexplained_minor"].(float64) != 400 || tie["as_of"] != "2026-06-10" {
		t.Fatalf("tie-out JSON = %#v", tie)
	}
	if _, found := decoded["policy_tier"]; !found {
		t.Fatal("policy_tier missing from JSON")
	}
}

func TestResultValidateRejectsUnknownVocabulary(t *testing.T) {
	result := fullResult(t, time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))
	badTier := "T9"
	result.PolicyTier = &badTier
	if err := result.Validate(); err == nil {
		t.Fatal("unknown policy tier was accepted")
	}

	result = fullResult(t, time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))
	result.RequestStatus = RequestStatusApproved
	result.Applied = true
	if err := result.Validate(); err == nil {
		t.Fatal("applied flag without applied status was accepted")
	}

	result = fullResult(t, time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))
	result.Proposal = ProposalNone
	result.ProposalID = ""
	result.RequestStatus = RequestStatusNone
	result.PolicyTier = nil
	result.AgentClaimedTier = nil
	result.ApproverAction = ActionAbstain
	result.Applied = false
	result.LedgerTxnID = ""
	result.Tieout = nil
	result.Verifier = nil
	if err := result.Validate(); err != nil {
		t.Fatalf("no-proposal result rejected: %v", err)
	}
}

func TestSafeErrorIsBoundedAndCollapsed(t *testing.T) {
	err := errorString(strings.Repeat("x", maxErrorLength+100) + "\n\tsecond line")
	message := safeError(err)
	if len(message) > maxErrorLength+3 {
		t.Fatalf("safe error length = %d", len(message))
	}
	if strings.ContainsAny(message, "\n\t") {
		t.Fatalf("safe error contains whitespace controls: %q", message)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestNormalizeForFailureKeepsPartialResultsPersistable(t *testing.T) {
	tier := "T1"
	result := Result{
		Schema:         SchemaVersion,
		Proposal:       ProposalPresent,
		ProposalID:     "20000000-0000-4000-8000-000000000001",
		RequestStatus:  "apply_failed",
		PolicyTier:     &tier,
		ApproverAction: ActionApprove,
		Applied:        true,
		LedgerTxnID:    "not-a-uuid",
		Failed:         true,
		Error:          "boom",
	}
	result.normalizeForFailure()
	if err := result.Validate(); err != nil {
		t.Fatalf("normalized failed result rejected: %v", err)
	}
	if result.RequestStatus != RequestStatusApplied {
		t.Fatalf("request status = %q, want applied", result.RequestStatus)
	}
	if result.LedgerTxnID != "" {
		t.Fatalf("invalid ledger txn id survived: %q", result.LedgerTxnID)
	}
}
