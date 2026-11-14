package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/money"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// payloadLine is one stored adjustment/write-off line. The stored payload is
// the canonical buildPayload shape from internal/agent/proposal, so unknown
// fields are a contract violation and are rejected.
type payloadLine struct {
	AccountCode string `json:"account_code"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type linesPayload struct {
	Lines []payloadLine `json:"lines"`
}

// decodePayloadLines strictly decodes payload.lines. A fractional or
// over-int64 amount, unknown field, trailing content, or empty list is a
// deterministic payload error.
func decodePayloadLines(raw []byte) ([]payloadLine, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("payload is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload linesPayload
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode payload lines: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode payload lines: trailing data")
	}
	if len(payload.Lines) == 0 {
		return nil, errors.New("payload lines are empty")
	}
	return payload.Lines, nil
}

// decodeEvidence reads the stored evidence JSON array. It is deliberately
// strict: the column is written only by the proposal tools.
func decodeEvidence(raw []byte) ([]string, error) {
	var evidence []string
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, fmt.Errorf("decode proposal evidence: %w", err)
	}
	if evidence == nil {
		evidence = []string{}
	}
	return evidence, nil
}

// resolvedAccount is the apply-relevant account projection.
type resolvedAccount struct {
	ID       string
	Currency string
	Status   string
}

// applyLedgerLines validates the stored lines against the live accounts and
// posts the adjustment transaction. Every validation error is deterministic
// and becomes an apply_failed(state_changed) failure; a genuine infrastructure
// error is returned as-is so the proposal stays approved.
func (s *Service) applyLedgerLines(ctx context.Context, tx pgx.Tx, locked lockedProposal, resolved proposal.State) (string, error) {
	lines, err := decodePayloadLines(locked.Payload)
	if err != nil {
		return "", &revalidationFailure{reason: FailureStateChanged, cause: err}
	}
	accounts, err := loadApplyAccounts(ctx, tx, lines)
	if err != nil {
		// The loader can return a deterministic duplicate-code error or a
		// database error; only the former becomes apply_failed.
		return "", revalidationFailureFor(fmt.Errorf("load apply accounts: %w", err), FailureStateChanged)
	}
	ledgerLines, err := validateApplyLines(lines, accounts, resolved)
	if err != nil {
		return "", &revalidationFailure{reason: FailureStateChanged, cause: err}
	}
	valueDate, err := s.valueDate(ctx, tx, resolved, locked.Evidence)
	if err != nil {
		return "", revalidationFailureFor(fmt.Errorf("resolve value date: %w", err), FailureStateChanged)
	}

	result, err := s.ledger.Post(ctx, tx, ledger.PostRequest{
		IdempotencyKey: "proposal:" + locked.ID,
		Kind:           ledger.KindAdjustment,
		ValueDate:      valueDate,
		Description:    "proposal " + locked.ID + " " + locked.Type,
		Lines:          ledgerLines,
		Actor:          s.actor,
	})
	if err != nil {
		if isDeterministicLedgerError(err) {
			return "", &revalidationFailure{
				reason: FailureStateChanged,
				cause:  fmt.Errorf("post adjustment: %w", err),
			}
		}
		return "", fmt.Errorf("executor: post adjustment: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("%w: ledger returned an empty transaction id", ErrApplyUnavailable)
	}
	return result.ID, nil
}

// loadApplyAccounts maps every payload account_code to its live account row.
func loadApplyAccounts(ctx context.Context, tx pgx.Tx, lines []payloadLine) (map[string]resolvedAccount, error) {
	codes := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		if _, duplicate := seen[line.AccountCode]; duplicate {
			return nil, fmt.Errorf("duplicate account_code %q", line.AccountCode)
		}
		seen[line.AccountCode] = struct{}{}
		codes = append(codes, line.AccountCode)
	}
	rows, err := tx.Query(ctx,
		`SELECT code, id::text, currency, status
		   FROM accounts
		  WHERE code = ANY($1::text[])`, codes)
	if err != nil {
		return nil, fmt.Errorf("load accounts: %w", err)
	}
	defer rows.Close()

	accounts := make(map[string]resolvedAccount, len(codes))
	for rows.Next() {
		var (
			code    string
			account resolvedAccount
		)
		if err := rows.Scan(&code, &account.ID, &account.Currency, &account.Status); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts[code] = account
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return accounts, nil
}

// validateApplyLines enforces the FIN-10/FIN-13 pre-checks the executor owns:
// positive amounts, a known direction and ISO currency, one line per account,
// active accounts with matching currency, one transaction currency, and
// balanced debits/credits with overflow-checked sums. The ledger service
// repeats the authoritative checks and its database triggers remain the
// backstop.
func validateApplyLines(lines []payloadLine, accounts map[string]resolvedAccount, resolved proposal.State) ([]ledger.Line, error) {
	if len(lines) < 2 {
		return nil, errors.New("at least two lines are required")
	}
	breakCurrencies := make(map[string]struct{}, len(resolved.Breaks))
	for _, item := range resolved.Breaks {
		breakCurrencies[item.Currency] = struct{}{}
	}

	var (
		currency    string
		totalDebit  int64
		totalCredit int64
		out         = make([]ledger.Line, 0, len(lines))
	)
	for i, line := range lines {
		if line.AccountCode == "" {
			return nil, fmt.Errorf("line %d has an empty account code", i)
		}
		if line.AmountMinor <= 0 {
			return nil, fmt.Errorf("line %d amount must be positive", i)
		}
		if _, err := money.ParseCurrency(line.Currency); err != nil {
			return nil, fmt.Errorf("line %d currency: %w", i, err)
		}
		if currency == "" {
			currency = line.Currency
		} else if line.Currency != currency {
			return nil, fmt.Errorf("line %d currency %s differs from %s", i, line.Currency, currency)
		}
		account, ok := accounts[line.AccountCode]
		if !ok {
			return nil, fmt.Errorf("line %d account %q does not exist", i, line.AccountCode)
		}
		if account.Status != "active" {
			return nil, fmt.Errorf("line %d account %q is %s", i, line.AccountCode, account.Status)
		}
		if account.Currency != line.Currency {
			return nil, fmt.Errorf("line %d account %q is %s, entry is %s",
				i, line.AccountCode, account.Currency, line.Currency)
		}

		var direction ledger.Direction
		switch line.Direction {
		case string(ledger.Debit):
			direction = ledger.Debit
			var err error
			totalDebit, err = addMinor(totalDebit, line.AmountMinor)
			if err != nil {
				return nil, err
			}
		case string(ledger.Credit):
			direction = ledger.Credit
			var err error
			totalCredit, err = addMinor(totalCredit, line.AmountMinor)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("line %d direction %q is not debit or credit", i, line.Direction)
		}
		out = append(out, ledger.Line{
			AccountID:   account.ID,
			Direction:   direction,
			AmountMinor: line.AmountMinor,
			Currency:    line.Currency,
		})
	}

	if _, ok := breakCurrencies[currency]; !ok {
		return nil, fmt.Errorf("transaction currency %s is not a break currency", currency)
	}
	if totalDebit != totalCredit {
		return nil, fmt.Errorf("debits %d do not equal credits %d", totalDebit, totalCredit)
	}
	return out, nil
}

// addMinor adds two int64 money amounts with overflow checks.
func addMinor(total, amount int64) (int64, error) {
	if amount > 0 && total > math.MaxInt64-amount {
		return 0, errors.New("amount overflow")
	}
	if amount < 0 && total < math.MinInt64-amount {
		return 0, errors.New("amount overflow")
	}
	return total + amount, nil
}

// isDeterministicLedgerError reports the ledger/domain errors that mean the
// stored proposal can never post. They map to apply_failed(state_changed)
// instead of an unbounded retry. Programming/configuration errors
// (ErrNoTransaction, ErrNoClock) deliberately stay infrastructure errors.
func isDeterministicLedgerError(err error) bool {
	return errors.Is(err, ledger.ErrInvalidRequest) ||
		errors.Is(err, ledger.ErrEmptyTransaction) ||
		errors.Is(err, ledger.ErrInvalidLine) ||
		errors.Is(err, ledger.ErrUnbalanced) ||
		errors.Is(err, ledger.ErrIdempotencyConflict) ||
		errors.Is(err, ledger.ErrAccountNotFound) ||
		errors.Is(err, ledger.ErrAccountNotPostable) ||
		errors.Is(err, ledger.ErrEntryCurrencyMismatch) ||
		errors.Is(err, ledger.ErrAdjustmentNotAllowed) ||
		errors.Is(err, ledger.ErrCustomerFundsT2)
}
