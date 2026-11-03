package tieout

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const dateLayout = "2006-01-02"

// Queryer is the read-only subset of pgx used by Compute; *pgxpool.Pool and
// pgx.Tx both satisfy it.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Result is one scope/date tie-out. Amounts use the D-14 normal-balance sign
// for the scope's asset clearing account: debits positive, credits negative.
// LedgerOnly and ExternalOnly are the signed open-item totals, and Unexplained
// is:
//
//	(LedgerBalance - LedgerOnly) - (StatementClosing - ExternalOnly)
type Result struct {
	ScopeID           string
	AsOf              time.Time
	Currency          string
	LedgerBalance     money.Money
	StatementClosing  money.Money
	LedgerOnly        money.Money
	LedgerOnlyCount   int
	ExternalOnly      money.Money
	ExternalOnlyCount int
	Unexplained       money.Money
}

// MarshalJSON encodes a stable snake_case object. Field order is the struct
// order below, so the CLI's --json output is byte-stable for unchanged data.
func (r Result) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ScopeID          string      `json:"scope_id"`
		AsOf             string      `json:"as_of"`
		Currency         string      `json:"currency"`
		LedgerBalance    money.Money `json:"ledger_balance"`
		StatementClosing money.Money `json:"statement_closing"`
		LedgerOnly       openItems   `json:"ledger_only"`
		ExternalOnly     openItems   `json:"external_only"`
		Unexplained      money.Money `json:"unexplained"`
	}{
		ScopeID:          r.ScopeID,
		AsOf:             r.AsOf.Format(dateLayout),
		Currency:         r.Currency,
		LedgerBalance:    r.LedgerBalance,
		StatementClosing: r.StatementClosing,
		LedgerOnly:       openItems{Amount: r.LedgerOnly, Count: r.LedgerOnlyCount},
		ExternalOnly:     openItems{Amount: r.ExternalOnly, Count: r.ExternalOnlyCount},
		Unexplained:      r.Unexplained,
	})
}

type openItems struct {
	Amount money.Money `json:"amount"`
	Count  int         `json:"count"`
}

// Compute returns the independent tie-out for scopeID at asOf.
//
// All as-of comparisons are inclusive on business date:
// ledger_txns.value_date <= asOf and external_records.value_date <= asOf. The
// statement closing balance must exist with statement_balances.as_of_date =
// asOf exactly. Membership in the current active match set decides whether a
// ledger transaction or external record is still an open item; Compute reads
// matches/match_legs only for membership and never imports internal/match.
func Compute(ctx context.Context, q Queryer, scopeID string, asOf time.Time) (Result, error) {
	if q == nil {
		return Result{}, fmt.Errorf("%w: Compute requires a queryer", ErrInvalidRequest)
	}
	if !uuidPattern.MatchString(scopeID) {
		return Result{}, fmt.Errorf("%w: scope id %q is not a UUID", ErrInvalidRequest, scopeID)
	}
	if asOf.IsZero() {
		return Result{}, fmt.Errorf("%w: as-of date is required", ErrInvalidRequest)
	}
	asOfDate := asOf.UTC().Format(dateLayout)
	asOfUTC, err := time.ParseInLocation(dateLayout, asOfDate, time.UTC)
	if err != nil {
		return Result{}, fmt.Errorf("%w: as-of date %q: %v", ErrInvalidRequest, asOfDate, err)
	}

	scope, err := loadScope(ctx, q, scopeID)
	if err != nil {
		return Result{}, err
	}
	if scope.AccountType != "asset" {
		return Result{}, fmt.Errorf("%w: scope %s ledger account type %q is not asset",
			ErrInvalidRequest, scopeID, scope.AccountType)
	}
	if scope.AccountCurrency != scope.Currency {
		return Result{}, fmt.Errorf("%w: scope %s currency %s but ledger account %s currency %s",
			ErrCurrencyMismatch, scopeID, scope.Currency, scope.AccountID, scope.AccountCurrency)
	}
	currency, err := money.ParseCurrency(scope.Currency)
	if err != nil {
		return Result{}, fmt.Errorf("tieout: scope %s currency %q: %w", scopeID, scope.Currency, err)
	}

	ledgerNet, mismatchedLedgerRows, err := ledgerBalance(ctx, q, scope, asOfDate)
	if err != nil {
		return Result{}, err
	}
	if mismatchedLedgerRows > 0 {
		return Result{}, fmt.Errorf("%w: scope %s has %d ledger row(s) outside %s",
			ErrCurrencyMismatch, scopeID, mismatchedLedgerRows, scope.Currency)
	}

	statementClosing, statementCurrency, err := statementClosing(ctx, q, scopeID, asOfDate)
	if err != nil {
		return Result{}, err
	}
	if statementCurrency != scope.Currency {
		return Result{}, fmt.Errorf("%w: scope %s currency %s but statement %s currency %s",
			ErrCurrencyMismatch, scopeID, scope.Currency, asOfDate, statementCurrency)
	}

	ledgerOnlyNet, ledgerOnlyCount, err := ledgerOnly(ctx, q, scope, asOfDate)
	if err != nil {
		return Result{}, err
	}
	externalOnlyNet, externalOnlyCount, err := externalOnly(ctx, q, scope, asOfDate)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		ScopeID:           scope.ID,
		AsOf:              asOfUTC,
		Currency:          scope.Currency,
		LedgerBalance:     money.FromMinor(ledgerNet, currency),
		StatementClosing:  money.FromMinor(statementClosing, currency),
		LedgerOnly:        money.FromMinor(ledgerOnlyNet, currency),
		LedgerOnlyCount:   ledgerOnlyCount,
		ExternalOnly:      money.FromMinor(externalOnlyNet, currency),
		ExternalOnlyCount: externalOnlyCount,
	}
	adjustedLedger, err := result.LedgerBalance.Sub(result.LedgerOnly)
	if err != nil {
		return Result{}, fmt.Errorf("tieout: adjusted ledger balance: %w", err)
	}
	adjustedStatement, err := result.StatementClosing.Sub(result.ExternalOnly)
	if err != nil {
		return Result{}, fmt.Errorf("tieout: adjusted statement balance: %w", err)
	}
	result.Unexplained, err = adjustedLedger.Sub(adjustedStatement)
	if err != nil {
		return Result{}, fmt.Errorf("tieout: unexplained difference: %w", err)
	}
	return result, nil
}
