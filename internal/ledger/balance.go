package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// Queryer is the read-only subset of pgx used by Balance; *pgxpool.Pool and
// pgx.Tx both satisfy it.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Balance returns the account's balance as of asOf, inclusive of
// value_date = asOf (D-2), with the account's normal balance sign (D-14):
//
//	asset, expense:        debits - credits
//	liability, equity,
//	revenue:               credits - debits
//
// asOf is a business date; its UTC calendar date is used. An unknown account
// returns ErrAccountNotFound, a zero asOf or invalid account id returns
// ErrInvalidRequest.
func Balance(ctx context.Context, q Queryer, accountID string, asOf time.Time) (money.Money, error) {
	if q == nil {
		return money.Money{}, fmt.Errorf("%w: Balance requires a queryer", ErrInvalidRequest)
	}
	if !isUUID(accountID) {
		return money.Money{}, fmt.Errorf("%w: account id %q is not a UUID", ErrInvalidRequest, accountID)
	}
	if asOf.IsZero() {
		return money.Money{}, fmt.Errorf("%w: as-of date is required", ErrInvalidRequest)
	}

	var (
		accountType string
		currency    string
		net         int64
	)
	err := q.QueryRow(ctx,
		`SELECT a.type, a.currency,
		        (COALESCE((
		            SELECT sum(CASE WHEN e.direction = 'debit'
		                            THEN e.amount_minor
		                            ELSE -e.amount_minor END)
		              FROM ledger_entries e
		              JOIN ledger_txns t ON t.id = e.txn_id
		             WHERE e.account_id = a.id
		               AND t.value_date <= $2::date
		        ), 0))::bigint
		   FROM accounts a
		  WHERE a.id = $1::text::uuid`,
		accountID, asOf.UTC().Format(dateLayout)).
		Scan(&accountType, &currency, &net)
	if errors.Is(err, pgx.ErrNoRows) {
		return money.Money{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	if err != nil {
		return money.Money{}, fmt.Errorf("ledger: balance query: %w", err)
	}

	switch accountType {
	case "asset", "expense":
		// Normal sign is already debits minus credits.
	case "liability", "equity", "revenue":
		net = -net
	default:
		return money.Money{}, fmt.Errorf("%w: account %s has unknown type %q", ErrInvalidRequest, accountID, accountType)
	}

	parsed, err := money.ParseCurrency(currency)
	if err != nil {
		return money.Money{}, fmt.Errorf("ledger: account %s currency %q: %w", accountID, currency, err)
	}
	return money.FromMinor(net, parsed), nil
}
