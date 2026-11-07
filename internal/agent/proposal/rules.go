package proposal

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/money"
)

// lineInput is one balanced-entry line. Amounts are integer minor units
// (FIN-05); the handler rejects fractional JSON before this type is populated.
type lineInput struct {
	AccountCode string `json:"account_code"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type caseInfo struct {
	ID     string
	Status string
}

type accountInfo struct {
	Code     string
	Currency string
	Status   string
}

func loadCase(ctx context.Context, q tools.Queryer, caseID string) (caseInfo, error) {
	var info caseInfo
	err := q.QueryRow(ctx,
		`SELECT id, status FROM v_agent_cases WHERE id = $1`, caseID).
		Scan(&info.ID, &info.Status)
	if err != nil {
		return caseInfo{}, fmt.Errorf("proposal: load case: %w", err)
	}
	return info, nil
}

func loadAccounts(ctx context.Context, q tools.Queryer, codes []string) (map[string]accountInfo, error) {
	accounts := make(map[string]accountInfo, len(codes))
	rows, err := q.Query(ctx,
		`SELECT code, currency, status
		   FROM v_agent_accounts
		  WHERE code = ANY($1::text[])`, codes)
	if err != nil {
		return nil, fmt.Errorf("proposal: load accounts: %w", err)
	}
	values, err := tools.CollectRows(rows, func(row pgx.Rows) (accountInfo, error) {
		var account accountInfo
		if err := row.Scan(&account.Code, &account.Currency, &account.Status); err != nil {
			return accountInfo{}, err
		}
		return account, nil
	})
	if err != nil {
		return nil, fmt.Errorf("proposal: load accounts: %w", err)
	}
	for _, account := range values {
		accounts[account.Code] = account
	}
	return accounts, nil
}

// checkAdjustment applies the balanced-entry, account, allowlist, delta and
// write-off rules. It returns a business_rule_violation CodedError with a short
// reason code and never echoes an account code or amount.
func checkAdjustment(kind string, lines []lineInput, breaks []BreakState, accounts map[string]accountInfo, ledger config.LedgerConfig, writeOffAccounts []string) error {
	if len(lines) < 2 {
		return ruleViolation("too_few_lines")
	}

	debits := make(map[string]int64)
	credits := make(map[string]int64)
	hasWriteOff := false
	for _, line := range lines {
		if line.AmountMinor <= 0 {
			return ruleViolation("non_positive_amount")
		}
		if _, err := money.ParseCurrency(line.Currency); err != nil {
			return ruleViolation(reasonCurrencyInvalid)
		}
		if line.Direction != "debit" && line.Direction != "credit" {
			return ruleViolation("invalid_direction")
		}
		account, ok := accounts[line.AccountCode]
		if !ok {
			return ruleViolation(reasonAccountNotFound)
		}
		if account.Status != "active" {
			return ruleViolation(reasonAccountInactive)
		}
		if account.Currency != line.Currency {
			return ruleViolation(reasonCurrencyMismatch)
		}
		if !ledger.AllowsAdjustment(line.AccountCode) {
			return ruleViolation(reasonAccountNotAllowed)
		}
		if containsString(writeOffAccounts, line.AccountCode) {
			hasWriteOff = true
		}
		var err error
		if line.Direction == "debit" {
			debits[line.Currency], err = addMinor(debits[line.Currency], line.AmountMinor)
		} else {
			credits[line.Currency], err = addMinor(credits[line.Currency], line.AmountMinor)
		}
		if err != nil {
			return err
		}
	}

	currencies := make(map[string]struct{}, len(debits)+len(credits))
	for currency := range debits {
		currencies[currency] = struct{}{}
	}
	for currency := range credits {
		currencies[currency] = struct{}{}
	}
	for currency := range currencies {
		if debits[currency] != credits[currency] {
			return ruleViolation(reasonUnbalanced)
		}
	}

	delta, err := breakDeltaByCurrency(breaks)
	if err != nil {
		return err
	}
	for currency, debitTotal := range debits {
		if debitTotal > delta[currency] {
			return ruleViolation(reasonExceedsDelta)
		}
	}

	if kind == "write_off" && !hasWriteOff {
		return ruleViolation(reasonWriteOffRequired)
	}
	return nil
}

func breakDeltaByCurrency(breaks []BreakState) (map[string]int64, error) {
	totals := make(map[string]int64)
	for _, item := range breaks {
		total, err := addMinor(totals[item.Currency], absMinor(item.DeltaMinor))
		if err != nil {
			return nil, err
		}
		totals[item.Currency] = total
	}
	return totals, nil
}

func addMinor(total, amount int64) (int64, error) {
	if amount > 0 && total > math.MaxInt64-amount {
		return 0, ruleViolation("amount_overflow")
	}
	if amount < 0 && total < math.MinInt64-amount {
		return 0, ruleViolation("amount_overflow")
	}
	return total + amount, nil
}

func absMinor(value int64) int64 {
	if value == math.MinInt64 {
		return math.MaxInt64
	}
	if value < 0 {
		return -value
	}
	return value
}

// ledgerTxnInCaseScope reports whether the txn and external ref co-occur in at
// least one break attached to the case.
func ledgerTxnInCaseScope(ledgerTxnID, externalRef string, breaks []BreakState) bool {
	for _, item := range breaks {
		if containsString(item.LedgerTxnIDs, ledgerTxnID) &&
			containsString(item.ExternalRefs, externalRef) {
			return true
		}
	}
	return false
}

func externalRefInCaseScope(externalRef string, breaks []BreakState) bool {
	for _, item := range breaks {
		if containsString(item.ExternalRefs, externalRef) {
			return true
		}
	}
	return false
}

// ledgerTxnInScope reports whether the ledger transaction has an entry on the
// reconciliation scope ledger account for the external record's source and
// currency. Composite legs need not be attached to a case break; they only need
// to belong to the same reconciliation scope as the external record.
func ledgerTxnInScope(ctx context.Context, q tools.Queryer, ledgerTxnID, source, recordID string) (bool, error) {
	var inScope bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1
		      FROM v_agent_external_records er
		      JOIN v_agent_ledger_txns t ON t.id = $1
		      JOIN v_agent_ledger_entries le ON le.txn_id = t.id
		      JOIN v_agent_scopes s ON s.ledger_account_id = le.account_id
		     WHERE er.source = $2
		       AND er.source_record_id = $3
		       AND s.source = er.source
		       AND s.currency = er.currency
		       AND le.currency = er.currency
		)`, ledgerTxnID, source, recordID).Scan(&inScope); err != nil {
		return false, fmt.Errorf("proposal: check ledger transaction scope: %w", err)
	}
	return inScope, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func splitExternalRef(ref string) (source, recordID string, err error) {
	if err := tools.ValidateRef(ref); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidEvidence, err)
	}
	if !strings.HasPrefix(ref, "ext:") {
		return "", "", fmt.Errorf("%w: not an external reference", ErrInvalidEvidence)
	}
	rest := strings.TrimPrefix(ref, "ext:")
	source, recordID, _ = strings.Cut(rest, ":")
	return source, recordID, nil
}

func ledgerTxnExists(ctx context.Context, q tools.Queryer, txnID string) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM v_agent_ledger_txns WHERE id = $1)`, txnID).
		Scan(&exists); err != nil {
		return false, fmt.Errorf("proposal: check ledger transaction: %w", err)
	}
	return exists, nil
}

func externalRecordExists(ctx context.Context, q tools.Queryer, source, recordID string) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM v_agent_external_records
		     WHERE source = $1 AND source_record_id = $2)`, source, recordID).
		Scan(&exists); err != nil {
		return false, fmt.Errorf("proposal: check external record: %w", err)
	}
	return exists, nil
}
