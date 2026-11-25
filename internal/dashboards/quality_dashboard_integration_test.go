//go:build integration

// covers: FIN-42, FIN-M5-05-16
package dashboards_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/dashboards"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestM505QualityPanelSQLExecutesOnEmptyDB executes every quality panel query
// through the real read-only dashboard login on a fresh migrated database. It
// pins the selected v_eval_* columns and result shapes before W3 adds live
// seeded data: an empty database must return zero rows and no SQL error.
func TestM505QualityPanelSQLExecutesOnEmptyDB(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	conn := openDashboardConn(t, d)

	dashboard, err := dashboards.LoadDashboardFile(
		filepath.Join("..", "..", "deploy", "grafana", "dashboards", "agent-quality.json"))
	if err != nil {
		t.Fatalf("load quality dashboard: %v", err)
	}

	wantColumns := map[int][][]string{
		1: {{"time", "metric", "accuracy"}},
		2: {{"time", "metric", "rate"}},
		3: {{"time", "metric", "rate"}},
		4: {{"time", "metric", "rate"}},
		5: {{"time", "metric", "avg_cost_usd"}},
		6: {
			{"time", "metric", "p50_latency_ms"},
			{"time", "metric", "p95_latency_ms"},
		},
		7: {{"reason", "count"}},
		8: {{"time", "rate"}},
	}

	if len(dashboard.Panels) != len(wantColumns) {
		t.Fatalf("quality panels = %d, want %d", len(dashboard.Panels), len(wantColumns))
	}
	for id, columns := range wantColumns {
		panel := dashboard.PanelByID(id)
		if panel == nil {
			t.Errorf("panel %d is missing", id)
			continue
		}
		if len(panel.Targets) != len(columns) {
			t.Errorf("panel %d targets = %d, want %d", id, len(panel.Targets), len(columns))
			continue
		}
		for targetIndex, target := range panel.Targets {
			sql := strings.ReplaceAll(target.RawSQL, "$__timeFilter(day)", "TRUE")
			if strings.Contains(sql, "$__") || strings.Contains(sql, "${") {
				t.Errorf("panel %d target %s has an unhandled Grafana macro: %s", id, target.RefID, sql)
				continue
			}
			rows, err := conn.Query(ctx, sql)
			if err != nil {
				t.Errorf("panel %d target %s query on empty DB: %v", id, target.RefID, err)
				continue
			}
			fields := rows.FieldDescriptions()
			if len(fields) != len(columns[targetIndex]) {
				t.Errorf("panel %d target %s columns = %d, want %d (%v)",
					id, target.RefID, len(fields), len(columns[targetIndex]), columns[targetIndex])
			} else {
				for i, want := range columns[targetIndex] {
					if fields[i].Name != want {
						t.Errorf("panel %d target %s column %d = %q, want %q",
							id, target.RefID, i, fields[i].Name, want)
					}
				}
			}
			rowCount := 0
			for rows.Next() {
				rowCount++
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Errorf("panel %d target %s iterate on empty DB: %v", id, target.RefID, err)
			}
			if rowCount != 0 {
				t.Errorf("panel %d target %s rows on a fresh database = %d, want 0",
					id, target.RefID, rowCount)
			}
		}
	}
}
