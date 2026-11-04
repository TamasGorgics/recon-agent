// Package periods selects the posting period for a ledger transaction (D-2).
//
// Selection rules:
//
//  1. The period containing value_date wins if it is open. When several periods
//     overlap (the M1-03 seed is broad and tests create narrow ones), the most
//     specific period - the one with the latest start_date, then the earliest
//     end_date - is used.
//  2. If that period is closed, the transaction posts in the current open
//     period: the open period containing today's clock date, else the latest
//     open period. The closed period is returned as AdjustsPeriodID (FIN-56).
//  3. If no period contains value_date, the transaction posts in the current
//     open period with no adjustment link.
//
// The package never reads the wall clock: callers pass the injected clock's
// current time as today.
package periods

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNoOpenPeriod means there is no open period to post into. It is returned
// instead of silently posting somewhere else.
var ErrNoOpenPeriod = errors.New("periods: no open period")

// ErrInvalidDate means a required date is zero.
var ErrInvalidDate = errors.New("periods: invalid date")

// Queryer is the subset of pgx used by Resolve; *pgxpool.Pool and pgx.Tx both
// satisfy it.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Selection is the period a transaction posts into, plus the closed period the
// transaction adjusts when the value date falls in one.
type Selection struct {
	// PeriodID is the open period that will hold the transaction.
	PeriodID string
	// AdjustsPeriodID is the closed period containing value_date, or nil when
	// the transaction posts in its own open period.
	AdjustsPeriodID *string
}

const dateLayout = "2006-01-02"

// Resolve selects the posting period for valueDate at the clock's today.
func Resolve(ctx context.Context, q Queryer, valueDate, today time.Time) (Selection, error) {
	if q == nil {
		return Selection{}, errors.New("periods: Resolve requires a queryer")
	}
	if valueDate.IsZero() {
		return Selection{}, fmt.Errorf("%w: value_date is zero", ErrInvalidDate)
	}
	if today.IsZero() {
		return Selection{}, fmt.Errorf("%w: today is zero", ErrInvalidDate)
	}
	value := dateOnly(valueDate)

	var (
		containingID     string
		containingStatus string
	)
	err := q.QueryRow(ctx,
		`SELECT id::text, status
		   FROM periods
		  WHERE start_date <= $1::date AND end_date >= $1::date
		  ORDER BY start_date DESC, end_date ASC
		  LIMIT 1`, value).
		Scan(&containingID, &containingStatus)
	switch {
	case err == nil && containingStatus == "open":
		return Selection{PeriodID: containingID}, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return Selection{}, fmt.Errorf("periods: find containing period: %w", err)
	}

	// Either the containing period is closed or none contains the date: post in
	// the current open period.
	openID, err := currentOpenPeriod(ctx, q, dateOnly(today))
	if err != nil {
		return Selection{}, err
	}
	if containingStatus == "closed" {
		adjusts := containingID
		return Selection{PeriodID: openID, AdjustsPeriodID: &adjusts}, nil
	}
	return Selection{PeriodID: openID}, nil
}

// currentOpenPeriod returns the open period containing today, else the latest
// open period.
func currentOpenPeriod(ctx context.Context, q Queryer, today string) (string, error) {
	var id string
	err := q.QueryRow(ctx,
		`SELECT id::text
		   FROM periods
		  WHERE status = 'open' AND start_date <= $1::date AND end_date >= $1::date
		  ORDER BY start_date DESC, end_date ASC
		  LIMIT 1`, today).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("periods: find open period containing today: %w", err)
	}

	err = q.QueryRow(ctx,
		`SELECT id::text
		   FROM periods
		  WHERE status = 'open'
		  ORDER BY start_date DESC, end_date DESC
		  LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoOpenPeriod
	}
	if err != nil {
		return "", fmt.Errorf("periods: find latest open period: %w", err)
	}
	return id, nil
}

// dateOnly normalizes a timestamp to the business date in UTC.
func dateOnly(t time.Time) string {
	return t.UTC().Format(dateLayout)
}
