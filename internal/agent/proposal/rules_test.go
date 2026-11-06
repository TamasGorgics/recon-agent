package proposal

import (
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

func testLedgerConfig() config.LedgerConfig {
	return config.LedgerConfig{AdjustmentAccounts: []string{
		"fees_expense_usd", "suspense_usd", "fx_gain_loss_usd", "fees_inactive_usd",
	}}
}

func testAccounts() map[string]accountInfo {
	return map[string]accountInfo{
		"fees_expense_usd":  {Code: "fees_expense_usd", Currency: "USD", Status: "active"},
		"suspense_usd":      {Code: "suspense_usd", Currency: "USD", Status: "active"},
		"fx_gain_loss_usd":  {Code: "fx_gain_loss_usd", Currency: "USD", Status: "active"},
		"fees_inactive_usd": {Code: "fees_inactive_usd", Currency: "USD", Status: "inactive"},
		"cash_usd":          {Code: "cash_usd", Currency: "USD", Status: "active"},
	}
}

func usdBreaks(delta int64) []BreakState {
	return []BreakState{{ID: "b1", Currency: "USD", DeltaMinor: delta}}
}

func balancedLines(amount int64) []lineInput {
	return []lineInput{
		{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: amount, Currency: "USD"},
		{AccountCode: "suspense_usd", Direction: "credit", AmountMinor: amount, Currency: "USD"},
	}
}

// TestAdjustmentBusinessRules is the table of deterministic money/account
// rejections for balanced entries.
//
// covers: FIN-13
func TestAdjustmentBusinessRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		kind      string
		lines     []lineInput
		breaks    []BreakState
		accounts  map[string]accountInfo
		wantError bool
		wantRule  string
	}{
		{
			name:     "balanced",
			kind:     "adjustment",
			lines:    balancedLines(250),
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
		},
		{
			name:     "exact delta",
			kind:     "adjustment",
			lines:    balancedLines(500),
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
		},
		{
			name:     "unbalanced",
			kind:     "adjustment",
			lines:    []lineInput{{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 100, Currency: "USD"}, {AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 99, Currency: "USD"}},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "unbalanced",
		},
		{
			name:     "above delta",
			kind:     "adjustment",
			lines:    balancedLines(501),
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "exceeds_delta",
		},
		{
			name:     "zero delta",
			kind:     "adjustment",
			lines:    balancedLines(1),
			breaks:   usdBreaks(0),
			accounts: testAccounts(),
			wantRule: "exceeds_delta",
		},
		{
			name:     "absolute delta",
			kind:     "adjustment",
			lines:    balancedLines(100),
			breaks:   usdBreaks(-100),
			accounts: testAccounts(),
		},
		{
			name:     "account currency mismatch",
			kind:     "adjustment",
			lines:    []lineInput{{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 100, Currency: "EUR"}, {AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 100, Currency: "EUR"}},
			breaks:   []BreakState{{ID: "b1", Currency: "EUR", DeltaMinor: 500}},
			accounts: testAccounts(),
			wantRule: "currency_mismatch",
		},
		{
			name:     "unknown currency",
			kind:     "adjustment",
			lines:    []lineInput{{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 100, Currency: "ZZZ"}, {AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 100, Currency: "ZZZ"}},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "currency_invalid",
		},
		{
			name:     "unknown account",
			kind:     "adjustment",
			lines:    []lineInput{{AccountCode: "missing_usd", Direction: "debit", AmountMinor: 100, Currency: "USD"}, {AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 100, Currency: "USD"}},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "account_not_found",
		},
		{
			name:     "inactive account",
			kind:     "adjustment",
			lines:    []lineInput{{AccountCode: "fees_inactive_usd", Direction: "debit", AmountMinor: 100, Currency: "USD"}, {AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 100, Currency: "USD"}},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "account_inactive",
		},
		{
			name:     "not allowlisted",
			kind:     "adjustment",
			lines:    []lineInput{{AccountCode: "cash_usd", Direction: "debit", AmountMinor: 100, Currency: "USD"}, {AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 100, Currency: "USD"}},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "account_not_allowed",
		},
		{
			name:     "non-positive amount",
			kind:     "adjustment",
			lines:    []lineInput{{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 0, Currency: "USD"}, {AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 0, Currency: "USD"}},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "non_positive_amount",
		},
		{
			name:     "too few lines",
			kind:     "adjustment",
			lines:    []lineInput{{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 100, Currency: "USD"}},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "too_few_lines",
		},
		{
			name: "multi-currency one side unbalanced",
			kind: "adjustment",
			lines: []lineInput{
				{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 100, Currency: "USD"},
				{AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 100, Currency: "USD"},
				{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 50, Currency: "USD"},
			},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "unbalanced",
		},
		{
			name: "write-off requires designated account",
			kind: "write_off",
			lines: []lineInput{
				{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 100, Currency: "USD"},
				{AccountCode: "fx_gain_loss_usd", Direction: "credit", AmountMinor: 100, Currency: "USD"},
			},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
			wantRule: "write_off_account_required",
		},
		{
			name: "write-off with suspense",
			kind: "write_off",
			lines: []lineInput{
				{AccountCode: "fees_expense_usd", Direction: "debit", AmountMinor: 100, Currency: "USD"},
				{AccountCode: "suspense_usd", Direction: "credit", AmountMinor: 100, Currency: "USD"},
			},
			breaks:   usdBreaks(500),
			accounts: testAccounts(),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := checkAdjustment(test.kind, test.lines, test.breaks, test.accounts,
				testLedgerConfig(), []string{"suspense_usd"})
			if test.wantRule == "" {
				if err != nil {
					t.Fatalf("checkAdjustment() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkAdjustment() error = nil, want %s", test.wantRule)
			}
			if !errors.Is(err, ErrBusinessRule) {
				t.Fatalf("error = %v, want ErrBusinessRule", err)
			}
			var coded *tools.CodedError
			if !errors.As(err, &coded) || coded.Code != tools.CodeBusinessRuleViolation {
				t.Fatalf("error = %v, want business_rule_violation", err)
			}
			if !strings.Contains(coded.Message, test.wantRule) {
				t.Fatalf("message = %q, want reason %q", coded.Message, test.wantRule)
			}
		})
	}
}

func TestLinkAndDuplicateScope(t *testing.T) {
	t.Parallel()
	extRef := "ext:psp_a:rec-1"
	breaks := []BreakState{{
		ID:           "b1",
		Currency:     "USD",
		LedgerTxnIDs: []string{unitTxnID},
		ExternalRefs: []string{extRef},
	}}
	if !ledgerTxnInCaseScope(unitTxnID, extRef, breaks) {
		t.Fatal("ledger txn and external ref were not found in the same break")
	}
	if ledgerTxnInCaseScope(unitTxnID, "ext:psp_a:other", breaks) {
		t.Fatal("unrelated external ref matched the break scope")
	}
	if !externalRefInCaseScope(extRef, breaks) {
		t.Fatal("external ref not in case scope")
	}
	if externalRefInCaseScope("ext:psp_a:other", breaks) {
		t.Fatal("unrelated external ref matched the case scope")
	}
}
