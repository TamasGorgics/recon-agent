//go:build integration

// covers: FIN-42
package dashboards_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// dashboardFixture is the deterministic seed used to exercise every panel SQL.
// IDs are captured so assertions can distinguish the aging fixtures without
// relying on row order. All day-relative dates use the database's UTC date; all
// event timestamps are anchored to one Go instant, truncated to the microsecond
// precision Postgres stores.
type dashboardFixture struct {
	today     time.Time
	anchor    time.Time
	breaks    map[string]string
	cases     map[string]string
	proposals map[string]string
}

type extRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

func mustRefs(t *testing.T, refs ...extRef) string {
	t.Helper()
	data, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("marshal external refs: %v", err)
	}
	return string(data)
}

func timePtr(t time.Time) *time.Time { return &t }

func int32Ptr(v int32) *int32 { return &v }

func int64Ptr(v int64) *int64 { return &v }

func dateOnly(t time.Time) string { return t.Format("2006-01-02") }

// TestM504ViewPanels seeds fixtures for all five panels and executes the panel
// SQL from the M5-04 spec with only $__timeFilter replaced by TRUE. Every query
// runs through the real recon_dashboards_ro login, proving owner-rights views
// work without base-table grants.
func TestM504ViewPanels(t *testing.T) {
	d := testdb.New(t)
	f := seedDashboardFixture(t, d)
	conn := openDashboardConn(t, d)
	ctx := context.Background()

	t.Run("panel 1 match rate", func(t *testing.T) {
		type row struct {
			bucket     time.Time
			source     string
			matched    int64
			considered int64
			rate       string
		}
		rows, err := conn.Query(ctx,
			`SELECT bucket_start, source, matched, considered,
			        COALESCE(match_rate::text, '<null>')
			   FROM v_ops_match_rate
			  ORDER BY bucket_start, source`)
		if err != nil {
			t.Fatalf("query v_ops_match_rate: %v", err)
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.bucket, &r.source, &r.matched, &r.considered, &r.rate); err != nil {
				rows.Close()
				t.Fatalf("scan v_ops_match_rate: %v", err)
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_ops_match_rate: %v", err)
		}

		want := []row{
			{f.today.AddDate(0, 0, -1), "psp_a", 1, 2, "0.500000"},
			{f.today, "bank_x", 0, 5, "0.000000"},
			{f.today, "psp_a", 3, 4, "0.750000"},
			{f.today, "psp_zero", 0, 0, "<null>"},
		}
		if len(got) != len(want) {
			t.Fatalf("v_ops_match_rate rows = %v, want %v", got, want)
		}
		for i := range want {
			if !got[i].bucket.Equal(want[i].bucket) || got[i].source != want[i].source ||
				got[i].matched != want[i].matched || got[i].considered != want[i].considered ||
				got[i].rate != want[i].rate {
				t.Errorf("v_ops_match_rate[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}

		requireRows(t, conn,
			`SELECT bucket_start AS time, source, match_rate
			   FROM v_ops_match_rate
			  WHERE TRUE
			  ORDER BY bucket_start, source`, len(want))
	})

	t.Run("panel 2 unmatched value", func(t *testing.T) {
		type row struct {
			currency   string
			breaks     int64
			exposure   int64
			signed     int64
			major      string
			oldestSeen time.Time
		}
		rows, err := conn.Query(ctx,
			`SELECT currency, break_count, exposure_minor, signed_delta_minor,
			        exposure_major::text, oldest_detected_at
			   FROM v_ops_unmatched_value
			  ORDER BY currency`)
		if err != nil {
			t.Fatalf("query v_ops_unmatched_value: %v", err)
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.currency, &r.breaks, &r.exposure, &r.signed, &r.major, &r.oldestSeen); err != nil {
				rows.Close()
				t.Fatalf("scan v_ops_unmatched_value: %v", err)
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_ops_unmatched_value: %v", err)
		}

		want := []row{
			{"JPY", 1, 500, 500, "500.000", f.anchor.Add(-9 * time.Hour)},
			{"USD", 7, 2800, 2200, "28.000", f.anchor.Add(-7 * time.Hour)},
		}
		if len(got) != len(want) {
			t.Fatalf("v_ops_unmatched_value rows = %v, want %v", got, want)
		}
		for i := range want {
			if got[i].currency != want[i].currency || got[i].breaks != want[i].breaks ||
				got[i].exposure != want[i].exposure || got[i].signed != want[i].signed ||
				got[i].major != want[i].major {
				t.Errorf("v_ops_unmatched_value[%d] = %+v, want %+v", i, got[i], want[i])
			}
			requireCloseTime(t, "oldest_detected_at", got[i].oldestSeen, want[i].oldestSeen)
		}

		requireRows(t, conn,
			`SELECT currency, exposure_major, exposure_minor, signed_delta_minor, break_count
			   FROM v_ops_unmatched_value
			  ORDER BY currency`, len(want))
	})

	t.Run("panel 3 break aging", func(t *testing.T) {
		type group struct {
			order    int32
			breaks   int64
			exposure int64
		}
		rows, err := conn.Query(ctx,
			`SELECT break_type, bucket, bucket_order,
			        count(*)::bigint, SUM(abs(delta_minor))::bigint
			   FROM v_ops_break_aging
			  GROUP BY break_type, bucket, bucket_order
			  ORDER BY break_type, bucket_order`)
		if err != nil {
			t.Fatalf("query v_ops_break_aging groups: %v", err)
		}
		got := make(map[string]group)
		for rows.Next() {
			var breakType, bucket string
			var g group
			if err := rows.Scan(&breakType, &bucket, &g.order, &g.breaks, &g.exposure); err != nil {
				rows.Close()
				t.Fatalf("scan v_ops_break_aging group: %v", err)
			}
			got[breakType+"|"+bucket] = g
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_ops_break_aging groups: %v", err)
		}
		want := map[string]group{
			"ledger_only|0-1":         {1, 1, 100},
			"ledger_only|>7":          {4, 2, 900},
			"external_only|2-3":       {2, 2, 700},
			"external_only|unknown":   {5, 1, 600},
			"amount_mismatch|4-7":     {3, 1, 300},
			"amount_mismatch|unknown": {5, 1, 700},
		}
		for key, wantGroup := range want {
			if gotGroup, ok := got[key]; !ok || gotGroup != wantGroup {
				t.Errorf("aging group %s = %+v/%t, want %+v", key, gotGroup, ok, wantGroup)
			}
		}
		if len(got) != len(want) {
			t.Errorf("aging groups = %v, want %v", got, want)
		}

		type aging struct {
			minDate *time.Time
			ageDays *int32
			bucket  string
			order   int32
		}
		rows, err = conn.Query(ctx,
			`SELECT break_id, min_value_date, age_days, bucket, bucket_order
			   FROM v_ops_break_aging`)
		if err != nil {
			t.Fatalf("query v_ops_break_aging rows: %v", err)
		}
		aged := make(map[string]aging)
		for rows.Next() {
			var id string
			var a aging
			if err := rows.Scan(&id, &a.minDate, &a.ageDays, &a.bucket, &a.order); err != nil {
				rows.Close()
				t.Fatalf("scan v_ops_break_aging row: %v", err)
			}
			aged[id] = a
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_ops_break_aging rows: %v", err)
		}

		wantAged := map[string]aging{
			f.breaks["b0"]:       {timePtr(f.today), int32Ptr(0), "0-1", 1},
			f.breaks["b2"]:       {timePtr(f.today.AddDate(0, 0, -2)), int32Ptr(2), "2-3", 2},
			f.breaks["b5"]:       {timePtr(f.today.AddDate(0, 0, -5)), int32Ptr(5), "4-7", 3},
			f.breaks["b60"]:      {timePtr(f.today.AddDate(0, 0, -60)), int32Ptr(60), ">7", 4},
			f.breaks["bmulti"]:   {timePtr(f.today.AddDate(0, 0, -60)), int32Ptr(60), ">7", 4},
			f.breaks["bunknown"]: {nil, nil, "unknown", 5},
			f.breaks["bfuture"]:  {timePtr(f.today.AddDate(0, 0, 5)), int32Ptr(-5), "unknown", 5},
			f.breaks["bjpy"]:     {timePtr(f.today.AddDate(0, 0, -2)), int32Ptr(2), "2-3", 2},
		}
		for key, wantRow := range wantAged {
			gotRow, ok := aged[key]
			if !ok {
				t.Errorf("aging row %s is missing", key)
				continue
			}
			if !equalTimePtr(gotRow.minDate, wantRow.minDate) || !equalInt32Ptr(gotRow.ageDays, wantRow.ageDays) ||
				gotRow.bucket != wantRow.bucket || gotRow.order != wantRow.order {
				t.Errorf("aging row %s = %+v, want %+v", key, gotRow, wantRow)
			}
		}
		if _, ok := aged[f.breaks["bresolved"]]; ok {
			t.Error("resolved break appears in v_ops_break_aging")
		}
		if len(aged) != len(wantAged) {
			t.Errorf("aging rows = %v, want %d rows", aged, len(wantAged))
		}

		requireRows(t, conn,
			`SELECT break_type, bucket, count(*)::bigint AS breaks,
			        SUM(abs(delta_minor))::bigint AS exposure_minor
			   FROM v_ops_break_aging
			  GROUP BY break_type, bucket, bucket_order
			  ORDER BY break_type, bucket_order`, len(want))
	})

	t.Run("panel 4 approval queue", func(t *testing.T) {
		type queueRow struct {
			riskTier string
			status   string
			decision string
			approval *int64
			pending  *int64
		}
		rows, err := conn.Query(ctx,
			`SELECT proposal_id, risk_tier, status, COALESCE(decision, '<null>'),
			        approval_seconds, pending_seconds
			   FROM v_ops_approval_queue`)
		if err != nil {
			t.Fatalf("query v_ops_approval_queue: %v", err)
		}
		queue := make(map[string]queueRow)
		for rows.Next() {
			var id string
			var r queueRow
			if err := rows.Scan(&id, &r.riskTier, &r.status, &r.decision, &r.approval, &r.pending); err != nil {
				rows.Close()
				t.Fatalf("scan v_ops_approval_queue: %v", err)
			}
			queue[id] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_ops_approval_queue: %v", err)
		}
		if len(queue) != len(f.proposals) {
			t.Fatalf("approval queue rows = %d, want one per proposal (%d)", len(queue), len(f.proposals))
		}

		p1 := queue[f.proposals["p1"]]
		if p1.status != "pending_approval" || p1.decision != "<null>" || p1.approval != nil {
			t.Errorf("pending proposal p1 = %+v, want pending with no decision/approval", p1)
		}
		requireApproxSeconds(t, "p1 pending_seconds", p1.pending, 120)
		p2 := queue[f.proposals["p2"]]
		if p2.status != "pending_approval" || p2.decision != "<null>" || p2.approval != nil {
			t.Errorf("pending proposal p2 = %+v, want pending with no decision/approval", p2)
		}
		requireApproxSeconds(t, "p2 pending_seconds", p2.pending, 300)

		p3 := queue[f.proposals["p3"]]
		if p3.decision != "approved" || p3.approval == nil || *p3.approval != 100 || p3.pending != nil {
			t.Errorf("approved proposal p3 = %+v, want approved with 100s latency", p3)
		}
		p6 := queue[f.proposals["p6"]]
		if p6.decision != "approved" || p6.approval == nil || *p6.approval != 200 {
			t.Errorf("approved proposal p6 = %+v, want earliest approval 200s", p6)
		}
		p8 := queue[f.proposals["p8"]]
		if p8.decision != "approved" || p8.approval == nil || *p8.approval != 300 {
			t.Errorf("approved proposal p8 = %+v, want 300s", p8)
		}
		p4 := queue[f.proposals["p4"]]
		if p4.decision != "rejected" || p4.approval != nil {
			t.Errorf("rejected proposal p4 = %+v, want rejected with no latency", p4)
		}
		p9 := queue[f.proposals["p9"]]
		if p9.decision != "rejected" || p9.approval != nil {
			t.Errorf("proposal p9 = %+v, want rejected to win and suppress latency", p9)
		}
		p5 := queue[f.proposals["p5"]]
		if p5.decision != "<null>" || p5.approval != nil || p5.pending != nil {
			t.Errorf("auto proposal p5 = %+v, want no decision/approval/pending", p5)
		}

		type stat struct {
			pending  int64
			oldest   int64
			approved int64
			rejected int64
			median   *int64
			p95      *int64
		}
		rows, err = conn.Query(ctx,
			`SELECT risk_tier,
			        count(*) FILTER (WHERE status = 'pending_approval')::bigint AS pending,
			        COALESCE(max(pending_seconds) FILTER (WHERE status = 'pending_approval'), 0)::bigint
			            AS oldest_pending_seconds,
			        count(*) FILTER (WHERE decision = 'approved')::bigint AS approved,
			        count(*) FILTER (WHERE decision = 'rejected')::bigint AS rejected,
			        ROUND(percentile_cont(0.5) WITHIN GROUP (ORDER BY approval_seconds)
			              FILTER (WHERE decision = 'approved' AND TRUE))::bigint
			            AS median_approval_seconds,
			        ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY approval_seconds)
			              FILTER (WHERE decision = 'approved' AND TRUE))::bigint
			            AS p95_approval_seconds
			   FROM v_ops_approval_queue
			  GROUP BY risk_tier
			  ORDER BY risk_tier`)
		if err != nil {
			t.Fatalf("query panel 4 stats: %v", err)
		}
		stats := make(map[string]stat)
		for rows.Next() {
			var tier string
			var s stat
			if err := rows.Scan(&tier, &s.pending, &s.oldest, &s.approved, &s.rejected, &s.median, &s.p95); err != nil {
				rows.Close()
				t.Fatalf("scan panel 4 stats: %v", err)
			}
			stats[tier] = s
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate panel 4 stats: %v", err)
		}
		if len(stats) != 3 {
			t.Fatalf("panel 4 risk tiers = %v, want T0/T1/T2", stats)
		}
		requireStat(t, "T0", stats["T0"], 1, 2, 0, int64Ptr(150), int64Ptr(195))
		requireStat(t, "T1", stats["T1"], 1, 0, 1, nil, nil)
		requireStat(t, "T2", stats["T2"], 0, 1, 1, int64Ptr(300), int64Ptr(300))
		requireApproxRange(t, "T0 oldest_pending_seconds", stats["T0"].oldest, 120, 180)
		requireApproxRange(t, "T1 oldest_pending_seconds", stats["T1"].oldest, 300, 360)
	})

	t.Run("panel 5 resolution mix", func(t *testing.T) {
		type resolution struct {
			classification string
			eventAt        time.Time
			createdAt      time.Time
			resolvedAt     *time.Time
		}
		rows, err := conn.Query(ctx,
			`SELECT case_id, classification, event_at, created_at, resolved_at
			   FROM v_ops_resolution_mix`)
		if err != nil {
			t.Fatalf("query v_ops_resolution_mix: %v", err)
		}
		mix := make(map[string]resolution)
		for rows.Next() {
			var id string
			var r resolution
			if err := rows.Scan(&id, &r.classification, &r.eventAt, &r.createdAt, &r.resolvedAt); err != nil {
				rows.Close()
				t.Fatalf("scan resolution mix: %v", err)
			}
			mix[id] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate resolution mix: %v", err)
		}

		if len(mix) != 4 {
			t.Fatalf("resolution mix rows = %v, want four classified cases", mix)
		}
		if got := mix[f.cases["human"]]; got.classification != "human" {
			t.Errorf("human case = %+v, want human", got)
		}
		if got := mix[f.cases["auto"]]; got.classification != "auto" {
			t.Errorf("auto case = %+v, want auto", got)
		}
		escalated := mix[f.cases["escalated"]]
		if escalated.classification != "escalated" {
			t.Errorf("escalated case = %+v, want escalated (precedence over its approved proposal)", escalated)
		}
		if !escalated.eventAt.Equal(escalated.createdAt) {
			t.Errorf("escalated event_at = %v, want created_at %v", escalated.eventAt, escalated.createdAt)
		}
		both := mix[f.cases["both"]]
		if both.classification != "human" {
			t.Errorf("auto+human case = %+v, want human (human > auto)", both)
		}
		for _, key := range []string{"rejected", "none"} {
			if got, ok := mix[f.cases[key]]; ok {
				t.Errorf("unqualified case %s is classified as %q, want excluded", key, got.classification)
			}
		}

		var total, distinct int64
		if err := conn.QueryRow(ctx,
			`SELECT count(*), count(DISTINCT case_id) FROM v_ops_resolution_mix`).
			Scan(&total, &distinct); err != nil {
			t.Fatalf("count resolution mix: %v", err)
		}
		if total != 4 || distinct != 4 {
			t.Fatalf("resolution mix rows/distinct = %d/%d, want 4/4 (one row per case)", total, distinct)
		}

		type dayRow struct {
			bucket         time.Time
			classification string
			cases          int64
		}
		rows, err = conn.Query(ctx,
			`SELECT (date_trunc('day', event_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS time,
			        classification, count(*)::bigint AS cases
			   FROM v_ops_resolution_mix
			  WHERE TRUE
			  GROUP BY 1, 2
			  ORDER BY 1, 2`)
		if err != nil {
			t.Fatalf("query panel 5: %v", err)
		}
		var days []dayRow
		for rows.Next() {
			var r dayRow
			if err := rows.Scan(&r.bucket, &r.classification, &r.cases); err != nil {
				rows.Close()
				t.Fatalf("scan panel 5: %v", err)
			}
			days = append(days, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate panel 5: %v", err)
		}
		wantDays := []dayRow{
			{f.today.AddDate(0, 0, -4), "escalated", 1},
			{f.today, "auto", 1},
			{f.today, "human", 2},
		}
		if len(days) != len(wantDays) {
			t.Fatalf("panel 5 rows = %v, want %v", days, wantDays)
		}
		for i := range wantDays {
			if !days[i].bucket.Equal(wantDays[i].bucket) ||
				days[i].classification != wantDays[i].classification ||
				days[i].cases != wantDays[i].cases {
				t.Errorf("panel 5[%d] = %+v, want %+v", i, days[i], wantDays[i])
			}
		}
	})
}

func requireRows(t *testing.T, conn *pgx.Conn, query string, want int) {
	t.Helper()
	rows, err := conn.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("execute panel query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate panel query: %v", err)
	}
	if count != want {
		t.Fatalf("panel query returned %d row(s), want %d", count, want)
	}
}

func requireCloseTime(t *testing.T, label string, got, want time.Time) {
	t.Helper()
	delta := got.Sub(want)
	if delta < 0 {
		delta = -delta
	}
	if delta > time.Second {
		t.Fatalf("%s = %v, want %v (delta %v)", label, got, want, delta)
	}
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func equalInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func requireApproxSeconds(t *testing.T, label string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is NULL, want about %d", label, want)
	}
	if *got < want || *got > want+60 {
		t.Fatalf("%s = %d, want [%d,%d]", label, *got, want, want+60)
	}
}

func requireApproxRange(t *testing.T, label string, got, low, high int64) {
	t.Helper()
	if got < low || got > high {
		t.Fatalf("%s = %d, want [%d,%d]", label, got, low, high)
	}
}

func requireStat(t *testing.T, tier string, got struct {
	pending  int64
	oldest   int64
	approved int64
	rejected int64
	median   *int64
	p95      *int64
}, pending, approved, rejected int64, median, p95 *int64) {
	t.Helper()
	if got.pending != pending || got.approved != approved || got.rejected != rejected {
		t.Errorf("%s stats = %+v, want pending=%d approved=%d rejected=%d", tier, got, pending, approved, rejected)
	}
	if !equalInt64Ptr(got.median, median) || !equalInt64Ptr(got.p95, p95) {
		t.Errorf("%s median/p95 = %v/%v, want %v/%v", tier, ptr(got.median), ptr(got.p95), ptr(median), ptr(p95))
	}
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ptr(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func seedDashboardFixture(t *testing.T, d *testdb.DB) *dashboardFixture {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	f := &dashboardFixture{
		anchor:    time.Now().UTC().Truncate(time.Microsecond),
		breaks:    make(map[string]string),
		cases:     make(map[string]string),
		proposals: make(map[string]string),
	}
	if err := tx.QueryRow(ctx, `SELECT (now() AT TIME ZONE 'UTC')::date`).Scan(&f.today); err != nil {
		t.Fatalf("read database UTC date: %v", err)
	}
	f.today = f.today.UTC()

	clearingUSD := insertAccount(t, ctx, tx, "dash_clearing_usd", "asset", "USD")
	revenueUSD := insertAccount(t, ctx, tx, "dash_revenue_usd", "revenue", "USD")

	var periodID string
	if err := tx.QueryRow(ctx,
		`SELECT id::text FROM periods WHERE status = 'open' ORDER BY start_date LIMIT 1`).
		Scan(&periodID); err != nil {
		t.Fatalf("read seeded open period: %v", err)
	}

	pspUSD := insertScope(t, ctx, tx, clearingUSD, "psp_a", "USD")
	bankUSD := insertScope(t, ctx, tx, clearingUSD, "bank_x", "USD")
	zeroUSD := insertScope(t, ctx, tx, clearingUSD, "psp_zero", "USD")

	today10 := f.today.Add(10 * time.Hour)
	yesterday10 := f.today.AddDate(0, 0, -1).Add(10 * time.Hour)
	pspRun := insertMatchRun(t, ctx, tx, pspUSD, "succeeded", today10,
		`{"matched":3,"external_candidates":4}`)
	insertMatchRun(t, ctx, tx, pspUSD, "succeeded", yesterday10,
		`{"matched":1,"external_candidates":2}`)
	insertMatchRun(t, ctx, tx, bankUSD, "succeeded", today10,
		`{"matched":0,"external_candidates":5}`)
	insertMatchRun(t, ctx, tx, zeroUSD, "succeeded", today10,
		`{"matched":0,"external_candidates":0}`)
	insertMatchRun(t, ctx, tx, pspUSD, "failed", today10,
		`{"matched":999,"external_candidates":999}`)

	batchPSPUSD := insertBatch(t, ctx, tx, "psp_a", "USD", "dash-file-psp-usd")
	batchBankUSD := insertBatch(t, ctx, tx, "bank_x", "USD", "dash-file-bank-usd")
	batchPSPJPY := insertBatch(t, ctx, tx, "psp_a", "JPY", "dash-file-psp-jpy")
	insertExternalRecord(t, ctx, tx, batchPSPUSD, "psp_a", "rec-2", "USD", f.anchor, f.today.AddDate(0, 0, -2))
	insertExternalRecord(t, ctx, tx, batchPSPUSD, "psp_a", "rec-5", "USD", f.anchor, f.today.AddDate(0, 0, -5))
	insertExternalRecord(t, ctx, tx, batchPSPUSD, "psp_a", "rec-60", "USD", f.anchor, f.today.AddDate(0, 0, -60))
	insertExternalRecord(t, ctx, tx, batchBankUSD, "bank_x", "bx-5", "USD", f.anchor, f.today.AddDate(0, 0, -5))
	insertExternalRecord(t, ctx, tx, batchPSPJPY, "psp_a", "jpy-rec-2", "JPY", f.anchor, f.today.AddDate(0, 0, -2))
	insertExternalRecord(t, ctx, tx, batchBankUSD, "bank_x", "future-1", "USD", f.anchor, f.today.AddDate(0, 0, 5))

	ledgerToday := insertLedgerTxn(t, ctx, tx, clearingUSD, revenueUSD, periodID, f.today, "dash-ledger-today", 100)

	signatureSeq := 0
	signature := func() string {
		signatureSeq++
		return fmt.Sprintf("%064x", signatureSeq)
	}
	f.breaks["b0"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "ledger_only",
		[]string{ledgerToday}, "[]", 100, "USD", "open", f.anchor.Add(-1*time.Hour))
	f.breaks["b2"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "external_only",
		nil, mustRefs(t, extRef{"psp_a", "rec-2"}), 200, "USD", "open", f.anchor.Add(-2*time.Hour))
	f.breaks["b5"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "amount_mismatch",
		nil, mustRefs(t, extRef{"psp_a", "rec-5"}), -300, "USD", "open", f.anchor.Add(-3*time.Hour))
	f.breaks["b60"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "ledger_only",
		nil, mustRefs(t, extRef{"psp_a", "rec-60"}), 400, "USD", "open", f.anchor.Add(-4*time.Hour))
	f.breaks["bmulti"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "ledger_only",
		nil, mustRefs(t, extRef{"psp_a", "rec-60"}, extRef{"bank_x", "bx-5"}), 500, "USD", "open",
		f.anchor.Add(-5*time.Hour))
	f.breaks["bunknown"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "external_only",
		nil, mustRefs(t, extRef{"psp_a", "missing-1"}), 600, "USD", "open", f.anchor.Add(-6*time.Hour))
	f.breaks["bfuture"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "amount_mismatch",
		nil, mustRefs(t, extRef{"bank_x", "future-1"}), 700, "USD", "open", f.anchor.Add(-7*time.Hour))
	f.breaks["bresolved"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "external_only",
		nil, mustRefs(t, extRef{"psp_a", "rec-2"}), 800, "USD", "resolved", f.anchor.Add(-8*time.Hour))
	f.breaks["bjpy"] = insertBreak(t, ctx, tx, signature(), pspUSD, pspRun, "external_only",
		nil, mustRefs(t, extRef{"psp_a", "jpy-rec-2"}), 500, "JPY", "open", f.anchor.Add(-9*time.Hour))

	f.cases["pending1"] = insertCase(t, ctx, tx, "proposal_pending", f.anchor.Add(-24*time.Hour), nil, nil)
	f.cases["pending2"] = insertCase(t, ctx, tx, "proposal_pending", f.anchor.Add(-24*time.Hour), nil, nil)
	f.cases["human"] = insertCase(t, ctx, tx, "resolved", f.anchor.Add(-48*time.Hour), timePtr(f.today), nil)
	f.cases["auto"] = insertCase(t, ctx, tx, "resolved", f.anchor.Add(-72*time.Hour), timePtr(f.today), nil)
	f.cases["escalated"] = insertCase(t, ctx, tx, "escalated", f.anchor.Add(-96*time.Hour), nil,
		strPtr("budget_exhausted"))
	f.cases["both"] = insertCase(t, ctx, tx, "resolved", f.anchor.Add(-120*time.Hour), timePtr(f.today), nil)
	f.cases["rejected"] = insertCase(t, ctx, tx, "resolved", f.anchor.Add(-144*time.Hour), timePtr(f.today), nil)
	f.cases["none"] = insertCase(t, ctx, tx, "resolved", f.anchor.Add(-168*time.Hour), timePtr(f.today), nil)
	f.cases["open9"] = insertCase(t, ctx, tx, "open", f.anchor.Add(-24*time.Hour), nil, nil)

	runFor := func(caseID string, at time.Time) string {
		return insertAgentRun(t, ctx, tx, caseID, at.Add(time.Minute))
	}

	p1Created := f.anchor.Add(-120 * time.Second)
	p2Created := f.anchor.Add(-300 * time.Second)
	p3Created := f.anchor.Add(-47 * time.Hour)
	p4Created := f.anchor.Add(-143 * time.Hour)
	p5Created := f.anchor.Add(-71 * time.Hour)
	p6Created := f.anchor.Add(-95 * time.Hour)
	p7Created := f.anchor.Add(-119 * time.Hour)
	p8Created := f.anchor.Add(-118 * time.Hour)
	p9Created := f.anchor.Add(-23 * time.Hour)

	f.proposals["p1"] = insertProposal(t, ctx, tx, f.cases["pending1"],
		runFor(f.cases["pending1"], p1Created), "T0", "pending_approval", p1Created, "dash-p1")
	f.proposals["p2"] = insertProposal(t, ctx, tx, f.cases["pending2"],
		runFor(f.cases["pending2"], p2Created), "T1", "pending_approval", p2Created, "dash-p2")
	f.proposals["p3"] = insertProposal(t, ctx, tx, f.cases["human"],
		runFor(f.cases["human"], p3Created), "T0", "applied", p3Created, "dash-p3")
	f.proposals["p4"] = insertProposal(t, ctx, tx, f.cases["rejected"],
		runFor(f.cases["rejected"], p4Created), "T1", "rejected", p4Created, "dash-p4")
	f.proposals["p5"] = insertProposal(t, ctx, tx, f.cases["auto"],
		runFor(f.cases["auto"], p5Created), "T1", "auto_approved", p5Created, "dash-p5")
	f.proposals["p6"] = insertProposal(t, ctx, tx, f.cases["escalated"],
		runFor(f.cases["escalated"], p6Created), "T0", "applied", p6Created, "dash-p6")
	f.proposals["p7"] = insertProposal(t, ctx, tx, f.cases["both"],
		runFor(f.cases["both"], p7Created), "T1", "applied", p7Created, "dash-p7")
	f.proposals["p8"] = insertProposal(t, ctx, tx, f.cases["both"],
		runFor(f.cases["both"], p8Created), "T2", "applied", p8Created, "dash-p8")
	f.proposals["p9"] = insertProposal(t, ctx, tx, f.cases["open9"],
		runFor(f.cases["open9"], p9Created), "T2", "rejected", p9Created, "dash-p9")

	approver1 := insertApprover(t, ctx, tx, "Dashboard Approver One")
	approver2 := insertApprover(t, ctx, tx, "Dashboard Approver Two")

	insertApproval(t, ctx, tx, f.proposals["p3"], approver1, "approved", p3Created.Add(100*time.Second))
	insertApproval(t, ctx, tx, f.proposals["p4"], approver1, "rejected", p4Created.Add(100*time.Second))
	insertApproval(t, ctx, tx, f.proposals["p6"], approver1, "approved", p6Created.Add(200*time.Second))
	insertApproval(t, ctx, tx, f.proposals["p6"], approver2, "approved", p6Created.Add(300*time.Second))
	insertApproval(t, ctx, tx, f.proposals["p8"], approver1, "approved", p8Created.Add(300*time.Second))
	insertApproval(t, ctx, tx, f.proposals["p9"], approver2, "rejected", p9Created.Add(50*time.Second))
	insertApproval(t, ctx, tx, f.proposals["p9"], approver1, "approved", p9Created.Add(100*time.Second))

	for key, created := range map[string]time.Time{
		"p3": p3Created,
		"p5": p5Created,
		"p6": p6Created,
		"p7": p7Created,
		"p8": p8Created,
	} {
		insertApplied(t, ctx, tx, f.proposals[key], created.Add(400*time.Second), "dash-exec-"+key)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit dashboard fixture: %v", err)
	}
	return f
}

func strPtr(s string) *string { return &s }

func insertAccount(t *testing.T, ctx context.Context, tx pgx.Tx, code, accountType, currency string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, $3, 'active')
		 RETURNING id::text`,
		code, accountType, currency).Scan(&id); err != nil {
		t.Fatalf("insert account %s: %v", code, err)
	}
	return id
}

func insertScope(t *testing.T, ctx context.Context, tx pgx.Tx, accountID, source, currency string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, $3)
		 RETURNING id::text`, accountID, source, currency).Scan(&id); err != nil {
		t.Fatalf("insert scope %s/%s: %v", source, currency, err)
	}
	return id
}

func insertMatchRun(t *testing.T, ctx context.Context, tx pgx.Tx, scopeID, status string, startedAt time.Time, stats string) string {
	t.Helper()
	var finishedAt *time.Time
	if status != "running" {
		finishedAt = timePtr(startedAt.Add(time.Minute))
	}
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $3, $4, $5::jsonb, 'dash-config')
		 RETURNING id::text`,
		scopeID, startedAt, finishedAt, status, stats).Scan(&id); err != nil {
		t.Fatalf("insert match run: %v", err)
	}
	return id
}

func insertBatch(t *testing.T, ctx context.Context, tx pgx.Tx, source, currency, fileHash string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO settlement_batches (source, declared_total_minor, currency, file_hash)
		 VALUES ($1, 0, $2, $3)
		 RETURNING id::text`, source, currency, fileHash).Scan(&id); err != nil {
		t.Fatalf("insert settlement batch %s: %v", fileHash, err)
	}
	return id
}

func insertExternalRecord(t *testing.T, ctx context.Context, tx pgx.Tx, batchID, source, sourceRecordID, currency string, occurredAt, valueDate time.Time) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, occurred_at,
		      value_date, batch_id, raw_hash)
		 VALUES ($1, $2, 'charge', 100, $3, $4, $5::date, $6::uuid, $7)
		 RETURNING id::text`,
		source, sourceRecordID, currency, occurredAt, dateOnly(valueDate), batchID,
		"raw:"+source+":"+sourceRecordID).Scan(&id); err != nil {
		t.Fatalf("insert external record %s/%s: %v", source, sourceRecordID, err)
	}
	return id
}

func insertLedgerTxn(t *testing.T, ctx context.Context, tx pgx.Tx, clearingID, revenueID, periodID string, valueDate time.Time, key string, amount int64) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ($1::date, 'charge', $2, $3, 'dash fixture', $4::uuid)
		 RETURNING id::text`,
		dateOnly(valueDate), key, "hash:"+key, periodID).Scan(&id); err != nil {
		t.Fatalf("insert ledger txn %s: %v", key, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
		 VALUES ($1::uuid, $2::uuid, 'debit', $3, 'USD')`, id, clearingID, amount); err != nil {
		t.Fatalf("insert debit entry: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
		 VALUES ($1::uuid, $2::uuid, 'credit', $3, 'USD')`, id, revenueID, amount); err != nil {
		t.Fatalf("insert credit entry: %v", err)
	}
	return id
}

func insertBreak(t *testing.T, ctx context.Context, tx pgx.Tx, signature, scopeID, runID, side string, ledgerIDs []string, refsJSON string, delta int64, currency, status string, detectedAt time.Time) string {
	t.Helper()
	if ledgerIDs == nil {
		ledgerIDs = []string{}
	}
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids, external_refs,
		      delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, $5::text[]::uuid[], $6::jsonb,
		         $7, $8, $9, $10)
		 RETURNING id::text`,
		signature, scopeID, runID, side, ledgerIDs, refsJSON, delta, currency, status, detectedAt).
		Scan(&id); err != nil {
		t.Fatalf("insert break %s: %v", side, err)
	}
	return id
}

func insertCase(t *testing.T, ctx context.Context, tx pgx.Tx, status string, createdAt time.Time, resolvedAt *time.Time, escalationReason *string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO cases
		     (value_minor, currency, status, escalation_reason, created_at, resolved_at)
		 VALUES (0, 'USD', $1, $2, $3, $4)
		 RETURNING id::text`,
		status, escalationReason, createdAt, resolvedAt).Scan(&id); err != nil {
		t.Fatalf("insert case %s: %v", status, err)
	}
	return id
}

func insertAgentRun(t *testing.T, ctx context.Context, tx pgx.Tx, caseID string, startedAt time.Time) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO agent_runs
		     (case_id, model, prompt_version, started_at, ended_at, outcome)
		 VALUES ($1::uuid, 'dash-model', 'dash-prompt', $2, $3, 'proposal')
		 RETURNING id::text`,
		caseID, startedAt, startedAt.Add(time.Minute)).Scan(&id); err != nil {
		t.Fatalf("insert agent run: %v", err)
	}
	return id
}

func insertProposal(t *testing.T, ctx context.Context, tx pgx.Tx, caseID, runID string, riskTier any, status string, createdAt time.Time, key string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      risk_tier, status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'dash fixture', 0.9,
		         $3, $4, $5, $6, $7)
		 RETURNING id::text`,
		caseID, runID, riskTier, status, "state:"+key, key, createdAt).Scan(&id); err != nil {
		t.Fatalf("insert proposal %s: %v", key, err)
	}
	return id
}

func insertApprover(t *testing.T, ctx context.Context, tx pgx.Tx, name string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO approvers (display_name, role) VALUES ($1, 'approver')
		 RETURNING id::text`, name).Scan(&id); err != nil {
		t.Fatalf("insert approver %s: %v", name, err)
	}
	return id
}

func insertApproval(t *testing.T, ctx context.Context, tx pgx.Tx, proposalID, approverID, decision string, decidedAt time.Time) {
	t.Helper()
	if _, err := tx.Exec(ctx,
		`INSERT INTO approvals
		     (proposal_id, channel, approver_id, decision, decided_at, signature_valid)
		 VALUES ($1::uuid, 'test', $2::uuid, $3, $4, true)`,
		proposalID, approverID, decision, decidedAt); err != nil {
		t.Fatalf("insert %s approval: %v", decision, err)
	}
}

func insertApplied(t *testing.T, ctx context.Context, tx pgx.Tx, proposalID string, appliedAt time.Time, executorRunID string) {
	t.Helper()
	if _, err := tx.Exec(ctx,
		`INSERT INTO applied_proposals (proposal_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, $2, $3)`, proposalID, appliedAt, executorRunID); err != nil {
		t.Fatalf("insert applied proposal %s: %v", executorRunID, err)
	}
}
