package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// Queryer is the app-pool read surface used by DBSpend.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DBSpend reads the UTC-day agent spend through the recon_app pool.
type DBSpend struct {
	Queryer Queryer
}

// SpendSince sums agent_runs.cost_usd for runs started at or after since.
func (s DBSpend) SpendSince(ctx context.Context, since time.Time) (decimal.Decimal, error) {
	if s.Queryer == nil {
		return decimal.Zero, errors.New("worker: spend reader has no queryer")
	}
	var text string
	if err := s.Queryer.QueryRow(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0)::text
		   FROM agent_runs
		  WHERE started_at >= $1`, since).Scan(&text); err != nil {
		return decimal.Zero, fmt.Errorf("worker: read daily spend: %w", err)
	}
	total, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Zero, fmt.Errorf("worker: parse daily spend: %w", err)
	}
	if total.IsNegative() {
		return decimal.Zero, nil
	}
	return total, nil
}
