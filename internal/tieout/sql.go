package tieout

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// scope is the reconciliation scope plus the account attributes the sign
// convention depends on.
type scope struct {
	ID              string
	AccountID       string
	Source          string
	Currency        string
	AccountType     string
	AccountCurrency string
}

func loadScope(ctx context.Context, q Queryer, scopeID string) (scope, error) {
	var s scope
	err := q.QueryRow(ctx,
		`SELECT s.id::text, s.ledger_account_id::text, s.source, s.currency,
		        a.type, a.currency
		   FROM reconciliation_scopes s
		   JOIN accounts a ON a.id = s.ledger_account_id
		  WHERE s.id = $1::uuid`,
		scopeID).Scan(&s.ID, &s.AccountID, &s.Source, &s.Currency, &s.AccountType, &s.AccountCurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return scope{}, fmt.Errorf("%w: %s", ErrScopeNotFound, scopeID)
	}
	if err != nil {
		return scope{}, fmt.Errorf("tieout: load scope %s: %w", scopeID, err)
	}
	return s, nil
}

// ledgerBalance sums the clearing account's normal-balance amount through
// asOf (D-14: asset => debits - credits) and reports any row whose currency
// differs from the scope currency.
func ledgerBalance(ctx context.Context, q Queryer, s scope, asOf string) (int64, int64, error) {
	var (
		net        int64
		mismatched int64
	)
	err := q.QueryRow(ctx,
		`SELECT COALESCE(sum(CASE WHEN e.direction = 'debit'
		                          THEN e.amount_minor
		                          ELSE -e.amount_minor END), 0)::bigint,
		        count(*) FILTER (WHERE e.currency <> $3)::bigint
		   FROM ledger_entries e
		   JOIN ledger_txns t ON t.id = e.txn_id
		  WHERE e.account_id = $1::uuid
		    AND t.value_date <= $2::date`,
		s.AccountID, asOf, s.Currency).Scan(&net, &mismatched)
	if err != nil {
		return 0, 0, fmt.Errorf("tieout: ledger balance for scope %s: %w", s.ID, err)
	}
	return net, mismatched, nil
}

// statementClosing reads the statement balance for asOf exactly. A missing row
// is ErrNoStatement, never a zero (D-2).
func statementClosing(ctx context.Context, q Queryer, scopeID, asOf string) (int64, string, error) {
	var (
		closing  int64
		currency string
	)
	err := q.QueryRow(ctx,
		`SELECT closing_minor, currency
		   FROM statement_balances
		  WHERE scope_id = $1::uuid
		    AND as_of_date = $2::date`,
		scopeID, asOf).Scan(&closing, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: scope %s as of %s", ErrNoStatement, scopeID, asOf)
	}
	if err != nil {
		return 0, "", fmt.Errorf("tieout: statement balance for scope %s as of %s: %w", scopeID, asOf, err)
	}
	return closing, currency, nil
}

// ledgerOnly returns the signed open-item total and transaction count for
// ledger transactions through asOf that no active match leg claims.
func ledgerOnly(ctx context.Context, q Queryer, s scope, asOf string) (int64, int, error) {
	var (
		net   int64
		count int
	)
	err := q.QueryRow(ctx,
		`SELECT COALESCE(sum(net), 0)::bigint, count(*)::int
		   FROM (
		       SELECT t.id,
		              sum(CASE WHEN e.direction = 'debit'
		                       THEN e.amount_minor
		                       ELSE -e.amount_minor END)::bigint AS net
		         FROM ledger_txns t
		         JOIN ledger_entries e ON e.txn_id = t.id
		        WHERE e.account_id = $1::uuid
		          AND t.value_date <= $2::date
		          AND e.currency = $3
		          AND NOT EXISTS (
		              SELECT 1 FROM match_legs ml
		               WHERE ml.ledger_txn_id = t.id AND ml.status = 'active'
		          )
		        GROUP BY t.id
		   ) open_txns`,
		s.AccountID, asOf, s.Currency).Scan(&net, &count)
	if err != nil {
		return 0, 0, fmt.Errorf("tieout: ledger-only items for scope %s: %w", s.ID, err)
	}
	return net, count, nil
}

// externalOnly returns the signed open-item total and record count for
// external records through asOf that no active match header claims. The sign
// convention is the external record's signed amount on the clearing account:
// positive is a debit.
func externalOnly(ctx context.Context, q Queryer, s scope, asOf string) (int64, int, error) {
	var (
		net   int64
		count int
	)
	err := q.QueryRow(ctx,
		`SELECT COALESCE(sum(er.amount_minor), 0)::bigint, count(*)::int
		   FROM external_records er
		  WHERE er.source = $1
		    AND er.currency = $2
		    AND er.value_date <= $3::date
		    AND NOT EXISTS (
		        SELECT 1 FROM matches m
		         WHERE m.external_record_id = er.id AND m.status = 'active'
		    )`,
		s.Source, s.Currency, asOf).Scan(&net, &count)
	if err != nil {
		return 0, 0, fmt.Errorf("tieout: external-only items for scope %s: %w", s.ID, err)
	}
	return net, count, nil
}
