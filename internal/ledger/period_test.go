//go:build integration

// covers: FIN-10, FIN-12, FIN-13, FIN-30, FIN-56
package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/periods"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

// TestPostClosedPeriodAdjusts covers FIN-56/D-2: a value date inside a closed
// period posts in the current open period with adjusts_period_id pointing at the
// closed period.
func TestPostClosedPeriodAdjusts(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	closed := newPeriod(t, d, "2031-01-01", "2031-03-31", "closed")
	narrowOpen := newPeriod(t, d, "2026-06-01", "2026-06-30", "open")

	req := chargeRequest(t, clearing, revenue, 1100, uniqueKey(t))
	req.ValueDate = mustDate(t, "2031-02-15")
	res := postCommitted(t, d, app, svc, req)

	if res.PeriodID != narrowOpen {
		t.Errorf("posting period = %q, want current open period %q", res.PeriodID, narrowOpen)
	}
	if res.AdjustsPeriodID == nil || *res.AdjustsPeriodID != closed {
		t.Fatalf("adjusts period = %v, want closed period %q", res.AdjustsPeriodID, closed)
	}

	var storedPeriod, storedAdjusts string
	if err := d.Pool.QueryRow(ctx,
		`SELECT period_id::text, adjusts_period_id::text FROM ledger_txns WHERE id = $1::uuid`, res.ID).
		Scan(&storedPeriod, &storedAdjusts); err != nil {
		t.Fatalf("read period columns: %v", err)
	}
	if storedPeriod != narrowOpen || storedAdjusts != closed {
		t.Errorf("stored periods = %s/%s, want %s/%s", storedPeriod, storedAdjusts, narrowOpen, closed)
	}
	if got := testutil.AssertAudited(t, d.Pool, res.ID); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
}

// TestPostPrefersNarrowOpenPeriod documents the overlapping-period rule: when
// the seeded broad period and a narrow period both contain value_date, the most
// specific (latest start) period wins.
func TestPostPrefersNarrowOpenPeriod(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	narrow := newPeriod(t, d, "2026-03-01", "2026-03-31", "open")
	req := chargeRequest(t, clearing, revenue, 600, uniqueKey(t))
	req.ValueDate = mustDate(t, "2026-03-15")
	res := postCommitted(t, d, app, svc, req)
	if res.PeriodID != narrow {
		t.Fatalf("posting period = %q, want narrow open period %q", res.PeriodID, narrow)
	}
	if res.AdjustsPeriodID != nil {
		t.Fatalf("adjusts period = %q, want NULL", *res.AdjustsPeriodID)
	}
}

// TestPostValueDateOutsideAnyPeriod posts in the current open period with no
// adjustment link when no period contains the value date (D-2).
func TestPostValueDateOutsideAnyPeriod(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	seeded := seededOpenPeriodID(t, d)
	req := chargeRequest(t, clearing, revenue, 300, uniqueKey(t))
	req.ValueDate = mustDate(t, "2032-01-05")
	res := postCommitted(t, d, app, svc, req)
	if res.PeriodID != seeded {
		t.Fatalf("posting period = %q, want current open period %q", res.PeriodID, seeded)
	}
	if res.AdjustsPeriodID != nil {
		t.Fatalf("adjusts period = %q, want NULL", *res.AdjustsPeriodID)
	}
}

// TestPostClosedPeriodBoundaries checks the containing-period comparison is
// inclusive on both ends.
func TestPostClosedPeriodBoundaries(t *testing.T) {
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")
	closed := newPeriod(t, d, "2031-01-01", "2031-03-31", "closed")

	for _, valueDate := range []string{"2031-01-01", "2031-03-31"} {
		t.Run(valueDate, func(t *testing.T) {
			req := chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
			req.ValueDate = mustDate(t, valueDate)
			res := postCommitted(t, d, app, svc, req)
			if res.AdjustsPeriodID == nil || *res.AdjustsPeriodID != closed {
				t.Fatalf("adjusts period = %v, want %q", res.AdjustsPeriodID, closed)
			}
		})
	}
}

// TestPostFailsWhenNoOpenPeriodExists proves the service refuses to guess when
// every period is closed.
func TestPostFailsWhenNoOpenPeriodExists(t *testing.T) {
	ctx := context.Background()
	d := newLedgerDB(t)
	app := openRolePool(t, d, db.RoleApp)
	svc := testService(t, config.LedgerConfig{})
	clearing := newAccount(t, d, "clearing_usd", "USD", "asset", "active")
	revenue := newAccount(t, d, "revenue_usd", "USD", "revenue", "active")

	if _, err := d.Pool.Exec(ctx, `UPDATE periods SET status = 'closed', closed_at = $1 WHERE status = 'open'`, mustTime(t, testNow)); err != nil {
		t.Fatalf("close all periods: %v", err)
	}

	req := chargeRequest(t, clearing, revenue, 100, uniqueKey(t))
	if err := postError(t, d, app, svc, req); !errors.Is(err, periods.ErrNoOpenPeriod) {
		t.Fatalf("no-open-period error = %v, want periods.ErrNoOpenPeriod", err)
	}
	if got := countRows(t, d, `SELECT count(*) FROM ledger_txns`); got != 0 {
		t.Fatalf("ledger_txns count = %d, want 0", got)
	}
}

// TestResolvePeriodSelection exercises internal/periods directly for the
// selection rules that are awkward to isolate through Post.
func TestResolvePeriodSelection(t *testing.T) {
	ctx := context.Background()
	today := mustTime(t, testNow)

	t.Run("open containing period", func(t *testing.T) {
		d := newLedgerDB(t)
		narrow := newPeriod(t, d, "2031-01-01", "2031-12-31", "open")
		got, err := periods.Resolve(ctx, d.Pool, mustDate(t, "2031-06-01"), today)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.PeriodID != narrow || got.AdjustsPeriodID != nil {
			t.Fatalf("selection = {%s %v}, want {%s nil}", got.PeriodID, got.AdjustsPeriodID, narrow)
		}
	})

	t.Run("closed containing period", func(t *testing.T) {
		d := newLedgerDB(t)
		seeded := seededOpenPeriodID(t, d)
		closed := newPeriod(t, d, "2032-01-01", "2032-06-30", "closed")
		got, err := periods.Resolve(ctx, d.Pool, mustDate(t, "2032-03-01"), today)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.PeriodID != seeded {
			t.Errorf("period = %q, want current open %q", got.PeriodID, seeded)
		}
		if got.AdjustsPeriodID == nil || *got.AdjustsPeriodID != closed {
			t.Fatalf("adjusts = %v, want %q", got.AdjustsPeriodID, closed)
		}
	})

	t.Run("no containing period", func(t *testing.T) {
		d := newLedgerDB(t)
		seeded := seededOpenPeriodID(t, d)
		got, err := periods.Resolve(ctx, d.Pool, mustDate(t, "2040-01-01"), today)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.PeriodID != seeded || got.AdjustsPeriodID != nil {
			t.Fatalf("selection = {%s %v}, want {%s nil}", got.PeriodID, got.AdjustsPeriodID, seeded)
		}
	})

	t.Run("no open period", func(t *testing.T) {
		d := newLedgerDB(t)
		if _, err := d.Pool.Exec(ctx,
			`UPDATE periods SET status = 'closed', closed_at = $1 WHERE status = 'open'`, today); err != nil {
			t.Fatalf("close all periods: %v", err)
		}
		if _, err := periods.Resolve(ctx, d.Pool, mustDate(t, "2026-01-01"), today); !errors.Is(err, periods.ErrNoOpenPeriod) {
			t.Fatalf("Resolve error = %v, want ErrNoOpenPeriod", err)
		}
	})
}
