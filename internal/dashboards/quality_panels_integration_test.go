//go:build integration

// covers: FIN-M5-05-24
package dashboards_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/dashboards"
	"github.com/TamasGorgics/recon-agent/internal/demo"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
)

// TestM505QualityPanelsNonEmptyAfterSeed is FIN-M5-05-24: after `recon demo
// quality`, every quality panel query returns at least one row through the
// real read-only dashboard login, every timeseries has at least one non-NULL
// plotted value, and the escalation reasons table has at least three reasons.
// The time range is pinned to the seed clock's 14-day window.
func TestM505QualityPanelsNonEmptyAfterSeed(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := demo.RunQuality(ctx, demo.Options{
		DBURL:      d.DSN,
		Seed:       1,
		Days:       14,
		Difficulty: synth.DifficultyRealistic,
		Clock:      clock.NewFake(now),
	}); err != nil {
		t.Fatalf("demo quality: %v", err)
	}

	conn := openDashboardConn(t, d)
	dashboard, err := dashboards.LoadDashboardFile(
		filepath.Join("..", "..", "deploy", "grafana", "dashboards", "agent-quality.json"))
	if err != nil {
		t.Fatalf("load quality dashboard: %v", err)
	}

	anchor := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := anchor.AddDate(0, 0, -13).Format("2006-01-02")
	to := anchor.AddDate(0, 0, 1).Format("2006-01-02")

	for _, panel := range dashboard.Panels {
		if len(panel.Targets) == 0 {
			t.Errorf("panel %d %q has no targets", panel.ID, panel.Title)
			continue
		}
		for _, target := range panel.Targets {
			sql := strings.ReplaceAll(target.RawSQL, "$__timeFilter(day)",
				fmt.Sprintf("day >= TIMESTAMPTZ '%s 00:00:00+00' AND day <= TIMESTAMPTZ '%s 00:00:00+00'", from, to))
			if strings.Contains(sql, "$__") || strings.Contains(sql, "${") {
				t.Errorf("panel %d has an unhandled macro: %s", panel.ID, sql)
				continue
			}
			rows, err := conn.Query(ctx, sql)
			if err != nil {
				t.Errorf("panel %d target %s: %v", panel.ID, target.RefID, err)
				continue
			}
			fields := rows.FieldDescriptions()
			rowCount := 0
			nonNull := 0
			for rows.Next() {
				values, err := rows.Values()
				if err != nil {
					rows.Close()
					t.Fatalf("panel %d target %s values: %v", panel.ID, target.RefID, err)
				}
				rowCount++
				// Plotted values are every column after the time column except
				// the optional metric label. The table panel is (reason, count),
				// so its count at index 1 is the plotted value.
				for i := 1; i < len(values); i++ {
					if fields[i].Name == "metric" {
						continue
					}
					if values[i] != nil {
						nonNull++
					}
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Errorf("panel %d target %s iterate: %v", panel.ID, target.RefID, err)
				continue
			}
			if rowCount == 0 {
				t.Errorf("panel %d %q target %s returned no rows after demo quality",
					panel.ID, panel.Title, target.RefID)
				continue
			}
			if nonNull == 0 {
				t.Errorf("panel %d %q target %s has no non-NULL plotted values",
					panel.ID, panel.Title, target.RefID)
			}
			if panel.ID == 7 {
				if rowCount < 3 {
					t.Errorf("escalation reasons table rows = %d, want at least 3", rowCount)
				}
				if len(fields) != 2 || fields[0].Name != "reason" || fields[1].Name != "count" {
					t.Errorf("escalation reasons table columns = %v, want reason,count", fields)
				}
			}
			t.Logf("panel %d target %s: rows=%d non_null=%d", panel.ID, target.RefID, rowCount, nonNull)
		}
	}
}
