//go:build integration

// covers: FIN-42
package dashboards_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/dashboards"
	"github.com/TamasGorgics/recon-agent/internal/demo"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
)

// TestM504DemoOpsCoverage is T4: after the documented reset + demo-ops path,
// every Postgres panel SQL from ops.json returns rows, the canonical aging
// buckets all appear, the resolution precedence fixture classifies escalated,
// and panel 2's minor-unit exposure equals the base-break aggregate exactly.
func TestM504DemoOpsCoverage(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	runDemoOps(t, d)
	conn := openDashboardConn(t, d)
	dashboard := loadOpsDashboard(t)

	today := databaseUTCDate(t, d)

	t.Run("panel 1 match rate by source", func(t *testing.T) {
		rows, err := conn.Query(ctx, panelSQL(t, dashboard, 1))
		if err != nil {
			t.Fatalf("execute panel 1: %v", err)
		}
		defer rows.Close()
		sources := map[string]bool{}
		count := 0
		for rows.Next() {
			var bucket time.Time
			var source string
			var rate *string
			if err := rows.Scan(&bucket, &source, &rate); err != nil {
				t.Fatalf("scan panel 1: %v", err)
			}
			sources[source] = true
			count++
			if rate == nil {
				t.Errorf("panel 1 source %s has a NULL match rate; the seeded runs must have candidates", source)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate panel 1: %v", err)
		}
		if count == 0 {
			t.Fatal("panel 1 returned no rows")
		}
		if len(sources) < 2 {
			t.Fatalf("panel 1 sources = %v, want at least two", sources)
		}
	})

	t.Run("panel 2 unmatched value by currency", func(t *testing.T) {
		type row struct {
			currency string
			exposure int64
			signed   int64
			breaks   int64
			major    string
		}
		rows, err := conn.Query(ctx, panelSQL(t, dashboard, 2))
		if err != nil {
			t.Fatalf("execute panel 2: %v", err)
		}
		got := map[string]row{}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.currency, &r.major, &r.exposure, &r.signed, &r.breaks); err != nil {
				rows.Close()
				t.Fatalf("scan panel 2: %v", err)
			}
			got[r.currency] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate panel 2: %v", err)
		}
		if len(got) < 2 {
			t.Fatalf("panel 2 currencies = %v, want at least two", got)
		}

		baseRows, err := d.Pool.Query(ctx,
			`SELECT currency, count(*)::bigint, SUM(abs(delta_minor))::bigint,
			        SUM(delta_minor)::bigint
			   FROM breaks
			  WHERE status IN ('open', 'in_case')
			  GROUP BY currency
			  ORDER BY currency`)
		if err != nil {
			t.Fatalf("query base break exposure: %v", err)
		}
		defer baseRows.Close()
		base := map[string]row{}
		for baseRows.Next() {
			var r row
			if err := baseRows.Scan(&r.currency, &r.breaks, &r.exposure, &r.signed); err != nil {
				t.Fatalf("scan base break exposure: %v", err)
			}
			base[r.currency] = r
		}
		if err := baseRows.Err(); err != nil {
			t.Fatalf("iterate base break exposure: %v", err)
		}
		if len(base) != len(got) {
			t.Fatalf("panel 2 currencies = %d, base break currencies = %d", len(got), len(base))
		}
		for currency, want := range base {
			view, ok := got[currency]
			if !ok {
				t.Errorf("panel 2 is missing currency %s", currency)
				continue
			}
			if view.breaks != want.breaks || view.exposure != want.exposure || view.signed != want.signed {
				t.Errorf("panel 2 %s = count %d exposure %d signed %d, want %d/%d/%d",
					currency, view.breaks, view.exposure, view.signed,
					want.breaks, want.exposure, want.signed)
			}
		}
	})

	t.Run("panel 3 aging buckets and fixtures", func(t *testing.T) {
		rows, err := conn.Query(ctx, panelSQL(t, dashboard, 3))
		if err != nil {
			t.Fatalf("execute panel 3: %v", err)
		}
		buckets := map[string]bool{}
		types := map[string]bool{}
		count := 0
		for rows.Next() {
			var breakType, bucket string
			var breaks, exposure int64
			if err := rows.Scan(&breakType, &bucket, &breaks, &exposure); err != nil {
				rows.Close()
				t.Fatalf("scan panel 3: %v", err)
			}
			buckets[bucket] = true
			types[breakType] = true
			count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate panel 3: %v", err)
		}
		if count == 0 {
			t.Fatal("panel 3 returned no rows")
		}
		for _, bucket := range []string{"0-1", "2-3", "4-7", ">7", "unknown"} {
			if !buckets[bucket] {
				t.Errorf("panel 3 is missing bucket %s (got %v)", bucket, buckets)
			}
		}
		for _, breakType := range []string{"ledger_only", "external_only", "amount_mismatch"} {
			if !types[breakType] {
				t.Errorf("panel 3 is missing break type %s (got %v)", breakType, types)
			}
		}

		agedRows, err := conn.Query(ctx,
			`SELECT min_value_date, age_days, bucket FROM v_ops_break_aging`)
		if err != nil {
			t.Fatalf("query aging rows: %v", err)
		}
		defer agedRows.Close()
		var minDates []time.Time
		var nullDate, futureDate bool
		for agedRows.Next() {
			var minDate *time.Time
			var ageDays *int32
			var bucket string
			if err := agedRows.Scan(&minDate, &ageDays, &bucket); err != nil {
				t.Fatalf("scan aging row: %v", err)
			}
			if minDate == nil {
				nullDate = true
				if bucket != "unknown" {
					t.Errorf("missing value date bucket = %s, want unknown", bucket)
				}
				continue
			}
			minDates = append(minDates, minDate.UTC())
			if minDate.After(today) {
				futureDate = true
				if bucket != "unknown" {
					t.Errorf("future value date bucket = %s, want unknown", bucket)
				}
			}
		}
		if err := agedRows.Err(); err != nil {
			t.Fatalf("iterate aging rows: %v", err)
		}
		for _, offset := range []int{0, -2, -5, -60} {
			want := today.AddDate(0, 0, offset)
			if !containsDate(minDates, want) {
				t.Errorf("aging fixtures are missing value date %s (min dates %v)", want.Format("2006-01-02"), minDates)
			}
		}
		if !nullDate {
			t.Error("aging fixtures are missing the unresolved-reference unknown case")
		}
		if !futureDate {
			t.Error("aging fixtures are missing the future-dated unknown case")
		}

		var twoRefMin *time.Time
		var twoRefCount int
		if err := d.Pool.QueryRow(ctx,
			`SELECT v.min_value_date, count(*)
			   FROM breaks b
			   JOIN v_ops_break_aging v ON v.break_id = b.id::text
			  WHERE jsonb_array_length(b.external_refs) = 2
			    AND b.external_refs::text LIKE '%demo-ops:%'
			  GROUP BY v.min_value_date`).
			Scan(&twoRefMin, &twoRefCount); err != nil {
			t.Fatalf("query two-ref aging fixture: %v", err)
		}
		if twoRefCount != 1 || twoRefMin == nil || !twoRefMin.Equal(today.AddDate(0, 0, -60)) {
			t.Errorf("two-ref fixture min = %v (count %d), want %s", twoRefMin, twoRefCount, today.AddDate(0, 0, -60).Format("2006-01-02"))
		}
	})

	t.Run("panel 4 approval queue stats", func(t *testing.T) {
		type stat struct {
			tier     string
			pending  int64
			oldest   int64
			approved int64
			rejected int64
			median   *int64
			p95      *int64
		}
		rows, err := conn.Query(ctx, panelSQL(t, dashboard, 4))
		if err != nil {
			t.Fatalf("execute panel 4: %v", err)
		}
		got := map[string]stat{}
		for rows.Next() {
			var s stat
			if err := rows.Scan(&s.tier, &s.pending, &s.oldest, &s.approved, &s.rejected, &s.median, &s.p95); err != nil {
				rows.Close()
				t.Fatalf("scan panel 4: %v", err)
			}
			got[s.tier] = s
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate panel 4: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("panel 4 risk tiers = %v, want T0/T1/T2", got)
		}
		requireQueueStat(t, "T0", got["T0"], 0, 0, 0, nil, nil)
		requireQueueStat(t, "T1", got["T1"], 1, 2, 0, int64Ptr(9000), int64Ptr(10620))
		requireQueueStat(t, "T2", got["T2"], 1, 1, 1, int64Ptr(10800), int64Ptr(10800))
		if got["T1"].oldest <= 0 || got["T2"].oldest <= 0 {
			t.Errorf("pending ages = T1 %d T2 %d, want both > 0", got["T1"].oldest, got["T2"].oldest)
		}
	})

	t.Run("panel 5 resolution mix", func(t *testing.T) {
		rows, err := conn.Query(ctx, panelSQL(t, dashboard, 5))
		if err != nil {
			t.Fatalf("execute panel 5: %v", err)
		}
		classifications := map[string]int64{}
		days := 0
		for rows.Next() {
			var bucket time.Time
			var classification string
			var cases int64
			if err := rows.Scan(&bucket, &classification, &cases); err != nil {
				rows.Close()
				t.Fatalf("scan panel 5: %v", err)
			}
			classifications[classification] += cases
			days++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate panel 5: %v", err)
		}
		if days == 0 {
			t.Fatal("panel 5 returned no rows")
		}
		want := map[string]int64{"auto": 2, "human": 2, "escalated": 2}
		if !reflect.DeepEqual(classifications, want) {
			t.Errorf("panel 5 classifications = %v, want %v", classifications, want)
		}

		var total, distinct int64
		if err := conn.QueryRow(ctx,
			`SELECT count(*), count(DISTINCT case_id) FROM v_ops_resolution_mix`).
			Scan(&total, &distinct); err != nil {
			t.Fatalf("count resolution mix: %v", err)
		}
		if total != 6 || distinct != 6 {
			t.Errorf("resolution mix rows/distinct = %d/%d, want 6/6", total, distinct)
		}

		precedenceRows, err := d.Pool.Query(ctx,
			`SELECT v.classification, v.event_at, c.created_at
			   FROM v_ops_resolution_mix v
			   JOIN cases c ON c.id::text = v.case_id
			   JOIN proposals p ON p.case_id = c.id
			   JOIN approvals a ON a.proposal_id = p.id AND a.decision = 'approved'
			   JOIN applied_proposals ap ON ap.proposal_id = p.id
			  WHERE c.status = 'escalated'`)
		if err != nil {
			t.Fatalf("query escalated precedence fixture: %v", err)
		}
		defer precedenceRows.Close()
		precedence := 0
		for precedenceRows.Next() {
			var classification string
			var eventAt, createdAt time.Time
			if err := precedenceRows.Scan(&classification, &eventAt, &createdAt); err != nil {
				t.Fatalf("scan escalated precedence fixture: %v", err)
			}
			precedence++
			if classification != "escalated" {
				t.Errorf("precedence classification = %s, want escalated", classification)
			}
			if !eventAt.Equal(createdAt) {
				t.Errorf("escalated event_at = %v, want created_at %v", eventAt, createdAt)
			}
		}
		if err := precedenceRows.Err(); err != nil {
			t.Fatalf("iterate escalated precedence fixture: %v", err)
		}
		if precedence == 0 {
			t.Fatal("escalated case with an approved+applied proposal is missing")
		}

		var excluded int64
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM v_ops_resolution_mix v
			   JOIN cases c ON c.id::text = v.case_id
			  WHERE c.status IN ('proposal_pending', 'waiting_info')`).
			Scan(&excluded); err != nil {
			t.Fatalf("count excluded resolution rows: %v", err)
		}
		if excluded != 0 {
			t.Errorf("unqualified cases classified in the mix: %d", excluded)
		}
	})
}

// TestM504DemoOpsDeterministic is T5: two reset runs with the same seed emit
// identical count summaries and identical ordered panel values. IDs, age_days,
// pending_seconds, and oldest_pending_seconds are deliberately excluded.
func TestM504DemoOpsDeterministic(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	firstSummary, first := resetRunSnapshot(t, d, ctx)
	secondSummary, second := resetRunSnapshot(t, d, ctx)
	if firstSummary != secondSummary {
		t.Fatalf("summary output differs between reset runs:\nfirst:\n%s\nsecond:\n%s", firstSummary, secondSummary)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("stable panel values differ between reset runs:\nfirst=%v\nsecond=%v", first, second)
	}
}

func resetRunSnapshot(t *testing.T, d *testdb.DB, ctx context.Context) (string, panelSnapshot) {
	t.Helper()
	if err := demo.Reset(ctx, d.DSN); err != nil {
		t.Fatalf("demo reset: %v", err)
	}
	summary, err := demo.RunOps(ctx, demo.Options{
		DBURL:      d.DSN,
		Seed:       1,
		Days:       14,
		Difficulty: synth.DifficultyRealistic,
	})
	if err != nil {
		t.Fatalf("demo ops: %v", err)
	}
	var out bytes.Buffer
	if err := summary.Write(&out); err != nil {
		t.Fatalf("write summary: %v", err)
	}
	conn := openDashboardConn(t, d)
	return out.String(), snapshotPanels(t, ctx, conn)
}

type panelSnapshot struct {
	MatchRate  []string
	Unmatched  []string
	Aging      []string
	Approvals  []string
	Resolution []string
}

func snapshotPanels(t *testing.T, ctx context.Context, conn *pgx.Conn) panelSnapshot {
	t.Helper()
	var snap panelSnapshot

	rows, err := conn.Query(ctx,
		`SELECT bucket_start, source, matched, considered, COALESCE(match_rate::text, '<null>')
		   FROM v_ops_match_rate ORDER BY bucket_start, source`)
	if err != nil {
		t.Fatalf("snapshot match rate: %v", err)
	}
	for rows.Next() {
		var bucket time.Time
		var source string
		var matched, considered int64
		var rate string
		if err := rows.Scan(&bucket, &source, &matched, &considered, &rate); err != nil {
			rows.Close()
			t.Fatalf("scan match rate: %v", err)
		}
		snap.MatchRate = append(snap.MatchRate,
			fmt.Sprintf("%s|%s|%d|%d|%s", bucket.UTC().Format("2006-01-02"), source, matched, considered, rate))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate match rate: %v", err)
	}

	rows, err = conn.Query(ctx,
		`SELECT currency, exposure_major::text, exposure_minor, signed_delta_minor, break_count
		   FROM v_ops_unmatched_value ORDER BY currency`)
	if err != nil {
		t.Fatalf("snapshot unmatched value: %v", err)
	}
	for rows.Next() {
		var currency, major string
		var exposure, signed, breaks int64
		if err := rows.Scan(&currency, &major, &exposure, &signed, &breaks); err != nil {
			rows.Close()
			t.Fatalf("scan unmatched value: %v", err)
		}
		snap.Unmatched = append(snap.Unmatched,
			fmt.Sprintf("%s|%s|%d|%d|%d", currency, major, exposure, signed, breaks))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate unmatched value: %v", err)
	}

	rows, err = conn.Query(ctx,
		`SELECT break_type, bucket, bucket_order, count(*)::bigint,
		        SUM(abs(delta_minor))::bigint
		   FROM v_ops_break_aging
		  GROUP BY break_type, bucket, bucket_order
		  ORDER BY break_type, bucket_order`)
	if err != nil {
		t.Fatalf("snapshot aging: %v", err)
	}
	for rows.Next() {
		var breakType, bucket string
		var order int32
		var breaks, exposure int64
		if err := rows.Scan(&breakType, &bucket, &order, &breaks, &exposure); err != nil {
			rows.Close()
			t.Fatalf("scan aging: %v", err)
		}
		snap.Aging = append(snap.Aging,
			fmt.Sprintf("%s|%s|%d|%d|%d", breakType, bucket, order, breaks, exposure))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate aging: %v", err)
	}

	// oldest_pending_seconds is a query-time value and is excluded by T5.
	rows, err = conn.Query(ctx,
		`SELECT risk_tier,
		        count(*) FILTER (WHERE status = 'pending_approval')::bigint,
		        count(*) FILTER (WHERE decision = 'approved')::bigint,
		        count(*) FILTER (WHERE decision = 'rejected')::bigint,
		        ROUND(percentile_cont(0.5) WITHIN GROUP (ORDER BY approval_seconds)
		              FILTER (WHERE decision = 'approved'))::bigint,
		        ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY approval_seconds)
		              FILTER (WHERE decision = 'approved'))::bigint
		   FROM v_ops_approval_queue
		  GROUP BY risk_tier
		  ORDER BY risk_tier`)
	if err != nil {
		t.Fatalf("snapshot approvals: %v", err)
	}
	for rows.Next() {
		var tier string
		var pending, approved, rejected int64
		var median, p95 *int64
		if err := rows.Scan(&tier, &pending, &approved, &rejected, &median, &p95); err != nil {
			rows.Close()
			t.Fatalf("scan approvals: %v", err)
		}
		snap.Approvals = append(snap.Approvals,
			fmt.Sprintf("%s|%d|%d|%d|%s|%s", tier, pending, approved, rejected,
				formatInt64Ptr(median), formatInt64Ptr(p95)))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate approvals: %v", err)
	}

	rows, err = conn.Query(ctx,
		`SELECT (date_trunc('day', event_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS day,
		        classification, count(*)::bigint
		   FROM v_ops_resolution_mix
		  GROUP BY 1, 2
		  ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("snapshot resolution mix: %v", err)
	}
	for rows.Next() {
		var day time.Time
		var classification string
		var cases int64
		if err := rows.Scan(&day, &classification, &cases); err != nil {
			rows.Close()
			t.Fatalf("scan resolution mix: %v", err)
		}
		snap.Resolution = append(snap.Resolution,
			fmt.Sprintf("%s|%s|%d", day.UTC().Format("2006-01-02"), classification, cases))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate resolution mix: %v", err)
	}
	return snap
}

func runDemoOps(t *testing.T, d *testdb.DB) {
	t.Helper()
	ctx := context.Background()
	if err := demo.Reset(ctx, d.DSN); err != nil {
		t.Fatalf("demo reset: %v", err)
	}
	if _, err := demo.RunOps(ctx, demo.Options{
		DBURL:      d.DSN,
		Seed:       1,
		Days:       14,
		Difficulty: synth.DifficultyRealistic,
	}); err != nil {
		t.Fatalf("demo ops: %v", err)
	}
}

func loadOpsDashboard(t *testing.T) *dashboards.Dashboard {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "grafana", "dashboards", "ops.json")
	dashboard, err := dashboards.LoadDashboardFile(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return dashboard
}

// panelSQL returns the panel's exact Postgres query with only the three
// allowed time macros replaced by TRUE.
func panelSQL(t *testing.T, dashboard *dashboards.Dashboard, panelID int) string {
	t.Helper()
	panel := dashboard.PanelByID(panelID)
	if panel == nil {
		t.Fatalf("dashboard panel %d is missing", panelID)
	}
	if len(panel.Targets) == 0 || strings.TrimSpace(panel.Targets[0].RawSQL) == "" {
		t.Fatalf("dashboard panel %d has no SQL target", panelID)
	}
	sql := panel.Targets[0].RawSQL
	for _, macro := range []string{
		"$__timeFilter(bucket_start)",
		"$__timeFilter(event_at)",
		"$__timeFilter(approved_at)",
	} {
		sql = strings.ReplaceAll(sql, macro, "TRUE")
	}
	if strings.Contains(sql, "$__") {
		t.Fatalf("panel %d has an unhandled Grafana macro: %s", panelID, sql)
	}
	return sql
}

func databaseUTCDate(t *testing.T, d *testdb.DB) time.Time {
	t.Helper()
	var today time.Time
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT (now() AT TIME ZONE 'UTC')::date`).Scan(&today); err != nil {
		t.Fatalf("read database UTC date: %v", err)
	}
	return today.UTC()
}

func containsDate(dates []time.Time, want time.Time) bool {
	for _, date := range dates {
		if date.Equal(want) {
			return true
		}
	}
	return false
}

func requireQueueStat(t *testing.T, tier string, got struct {
	tier     string
	pending  int64
	oldest   int64
	approved int64
	rejected int64
	median   *int64
	p95      *int64
}, pending, approved, rejected int64, median, p95 *int64) {
	t.Helper()
	if got.pending != pending || got.approved != approved || got.rejected != rejected {
		t.Errorf("%s queue = pending %d approved %d rejected %d, want %d/%d/%d",
			tier, got.pending, got.approved, got.rejected, pending, approved, rejected)
	}
	if !equalInt64Ptr(got.median, median) || !equalInt64Ptr(got.p95, p95) {
		t.Errorf("%s median/p95 = %s/%s, want %s/%s", tier,
			formatInt64Ptr(got.median), formatInt64Ptr(got.p95),
			formatInt64Ptr(median), formatInt64Ptr(p95))
	}
}

func formatInt64Ptr(value *int64) string {
	if value == nil {
		return "<null>"
	}
	return fmt.Sprintf("%d", *value)
}
