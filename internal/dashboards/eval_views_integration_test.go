//go:build integration

// covers: FIN-42
package dashboards_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// Fixed fixture days keep every assertion independent of the wall clock.
// Eval views bucket by the stored run timestamp, never by now().
var (
	evalDay1 = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	evalDay2 = time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	evalDay3 = time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	evalDay4 = time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	evalDay5 = time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC)
)

type evalColumnSpec struct {
	name     string
	dataType string
}

// TestM505EvalViewSchemaEmpty is FIN-M5-05-01 and FIN-M5-05-09: the seven
// eval view definitions in spec §3.2–§3.8 (the prose says "six"; see the
// W1 note on the counting defect) have the exact column order/types, every
// `day` is a timestamptz, and a fresh migrated database returns zero rows with
// NULL ratios rather than zero-filled rows.
func TestM505EvalViewSchemaEmpty(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	conn := openDashboardConn(t, d)

	want := map[string][]evalColumnSpec{
		"v_eval_accuracy": {
			{"day", "timestamp with time zone"},
			{"model", "text"},
			{"prompt_version", "text"},
			{"correct", "bigint"},
			{"total", "bigint"},
			{"accuracy", "numeric"},
		},
		"v_eval_false_fix": {
			{"day", "timestamp with time zone"},
			{"model", "text"},
			{"prompt_version", "text"},
			{"false_fix", "bigint"},
			{"total", "bigint"},
			{"rate", "numeric"},
		},
		"v_eval_cost_latency": {
			{"day", "timestamp with time zone"},
			{"model", "text"},
			{"prompt_version", "text"},
			{"avg_cost_usd", "numeric"},
			{"total_cost_usd", "numeric"},
			{"avg_latency_ms", "double precision"},
			{"p50_latency_ms", "double precision"},
			{"p95_latency_ms", "double precision"},
		},
		"v_eval_override_rate": {
			{"day", "timestamp with time zone"},
			{"model", "text"},
			{"prompt_version", "text"},
			{"rejections", "bigint"},
			{"decisions", "bigint"},
			{"rate", "numeric"},
		},
		"v_eval_apply_failure_rate": {
			{"day", "timestamp with time zone"},
			{"model", "text"},
			{"prompt_version", "text"},
			{"failures", "bigint"},
			{"attempts", "bigint"},
			{"rate", "numeric"},
		},
		"v_eval_escalations": {
			{"day", "timestamp with time zone"},
			{"reason", "text"},
			{"count", "bigint"},
		},
		"v_eval_escalation_rate": {
			{"day", "timestamp with time zone"},
			{"escalated", "bigint"},
			{"total_cases", "bigint"},
			{"rate", "numeric"},
		},
	}

	for view, wantColumns := range want {
		rows, err := d.Pool.Query(ctx,
			`SELECT column_name, data_type
			   FROM information_schema.columns
			  WHERE table_schema = 'public' AND table_name = $1
			  ORDER BY ordinal_position`, view)
		if err != nil {
			t.Fatalf("read %s columns: %v", view, err)
		}
		var got []evalColumnSpec
		for rows.Next() {
			var column evalColumnSpec
			if err := rows.Scan(&column.name, &column.dataType); err != nil {
				rows.Close()
				t.Fatalf("scan %s column: %v", view, err)
			}
			got = append(got, column)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s columns: %v", view, err)
		}
		if len(got) != len(wantColumns) {
			t.Fatalf("%s columns = %v, want %v", view, got, wantColumns)
		}
		for i := range wantColumns {
			if got[i] != wantColumns[i] {
				t.Errorf("%s column %d = %+v, want %+v", view, i, got[i], wantColumns[i])
			}
		}

		var count int64
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(&count); err != nil {
			t.Fatalf("dashboard SELECT %s on empty database: %v", view, err)
		}
		if count != 0 {
			t.Errorf("%s rows on a fresh database = %d, want 0", view, count)
		}
	}

	// Every ratio over the empty view is NULL; never a synthetic zero.
	for view, ratio := range map[string]string{
		"v_eval_accuracy":           "accuracy",
		"v_eval_false_fix":          "rate",
		"v_eval_override_rate":      "rate",
		"v_eval_apply_failure_rate": "rate",
		"v_eval_escalation_rate":    "rate",
	} {
		var null bool
		if err := conn.QueryRow(ctx,
			`SELECT max(`+ratio+`) IS NULL FROM `+view).Scan(&null); err != nil {
			t.Fatalf("read empty %s.%s: %v", view, ratio, err)
		}
		if !null {
			t.Errorf("%s.%s over an empty view is not NULL", view, ratio)
		}
	}
	var costAggregatesNull bool
	if err := conn.QueryRow(ctx,
		`SELECT avg(avg_cost_usd) IS NULL
		        AND sum(total_cost_usd) IS NULL
		        AND avg(avg_latency_ms) IS NULL
		        AND avg(p50_latency_ms) IS NULL
		        AND avg(p95_latency_ms) IS NULL
		   FROM v_eval_cost_latency`).Scan(&costAggregatesNull); err != nil {
		t.Fatalf("read empty v_eval_cost_latency aggregates: %v", err)
	}
	if !costAggregatesNull {
		t.Error("v_eval_cost_latency aggregates over an empty view are not all NULL")
	}
}

// TestM505EvalViewFixtures is FIN-M5-05-02, -03, -04 and the exclusion part of
// -25. It seeds completed and excluded eval runs, then runs the view SQL through
// the real recon_dashboards_ro login. Accuracy excludes injection; false-fix
// and cost/latency include it; repeat/unfinished/matcher rows are absent; the
// nearest-rank percentiles are pinned for even and odd counts.
func TestM505EvalViewFixtures(t *testing.T) {
	d := testdb.New(t)
	seedEvalViewFixture(t, d)
	conn := openDashboardConn(t, d)
	ctx := context.Background()

	t.Run("accuracy excludes injection and non-qualifying runs", func(t *testing.T) {
		type row struct {
			day     time.Time
			model   string
			prompt  string
			correct int64
			total   int64
			rate    string
		}
		rows, err := conn.Query(ctx,
			`SELECT day, model, prompt_version, correct, total, round(accuracy, 6)::text
			   FROM v_eval_accuracy
			  ORDER BY day, model, prompt_version`)
		if err != nil {
			t.Fatalf("query v_eval_accuracy: %v", err)
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.day, &r.model, &r.prompt, &r.correct, &r.total, &r.rate); err != nil {
				rows.Close()
				t.Fatalf("scan v_eval_accuracy: %v", err)
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_eval_accuracy: %v", err)
		}

		want := []row{
			{evalDay1, "model-a", "p1", 2, 4, "0.500000"},
			{evalDay2, "model-a", "p1", 2, 4, "0.500000"},
			{evalDay3, "model-b", "p2", 3, 5, "0.600000"},
		}
		if len(got) != len(want) {
			t.Fatalf("v_eval_accuracy rows = %v, want %v", got, want)
		}
		for i := range want {
			requireUTCMidnight(t, "v_eval_accuracy", got[i].day)
			if !got[i].day.Equal(want[i].day) || got[i].model != want[i].model ||
				got[i].prompt != want[i].prompt || got[i].correct != want[i].correct ||
				got[i].total != want[i].total || got[i].rate != want[i].rate {
				t.Errorf("v_eval_accuracy[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
		var excluded int64
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM v_eval_accuracy
			  WHERE model = 'model-z' OR day = $1::timestamptz OR total = 0`,
			evalDay4).Scan(&excluded); err != nil {
			t.Fatalf("count excluded accuracy rows: %v", err)
		}
		if excluded != 0 {
			t.Errorf("v_eval_accuracy contains %d excluded/repeat/unfinished/matcher row(s)", excluded)
		}
	})

	t.Run("false fix includes injection and excludes non-qualifying runs", func(t *testing.T) {
		type row struct {
			day      time.Time
			model    string
			prompt   string
			falseFix int64
			total    int64
			rate     string
		}
		rows, err := conn.Query(ctx,
			`SELECT day, model, prompt_version, false_fix, total, round(rate, 6)::text
			   FROM v_eval_false_fix
			  ORDER BY day, model, prompt_version`)
		if err != nil {
			t.Fatalf("query v_eval_false_fix: %v", err)
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.day, &r.model, &r.prompt, &r.falseFix, &r.total, &r.rate); err != nil {
				rows.Close()
				t.Fatalf("scan v_eval_false_fix: %v", err)
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_eval_false_fix: %v", err)
		}

		want := []row{
			{evalDay1, "model-a", "p1", 1, 4, "0.250000"},
			{evalDay2, "model-a", "p1", 3, 6, "0.500000"},
			{evalDay3, "model-b", "p2", 1, 5, "0.200000"},
		}
		if len(got) != len(want) {
			t.Fatalf("v_eval_false_fix rows = %v, want %v", got, want)
		}
		for i := range want {
			requireUTCMidnight(t, "v_eval_false_fix", got[i].day)
			if !got[i].day.Equal(want[i].day) || got[i].model != want[i].model ||
				got[i].prompt != want[i].prompt || got[i].falseFix != want[i].falseFix ||
				got[i].total != want[i].total || got[i].rate != want[i].rate {
				t.Errorf("v_eval_false_fix[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("cost latency averages and nearest-rank percentiles", func(t *testing.T) {
		type row struct {
			day        time.Time
			model      string
			prompt     string
			avgCost    string
			totalCost  string
			avgLatency float64
			p50        float64
			p95        float64
		}
		rows, err := conn.Query(ctx,
			`SELECT day, model, prompt_version,
			        round(avg_cost_usd, 6)::text, round(total_cost_usd, 6)::text,
			        avg_latency_ms, p50_latency_ms, p95_latency_ms
			   FROM v_eval_cost_latency
			  ORDER BY day, model, prompt_version`)
		if err != nil {
			t.Fatalf("query v_eval_cost_latency: %v", err)
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.day, &r.model, &r.prompt, &r.avgCost, &r.totalCost,
				&r.avgLatency, &r.p50, &r.p95); err != nil {
				rows.Close()
				t.Fatalf("scan v_eval_cost_latency: %v", err)
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_eval_cost_latency: %v", err)
		}

		want := []row{
			{evalDay1, "model-a", "p1", "0.250000", "1.000000", 25, 20, 40},
			{evalDay2, "model-a", "p1", "6.330000", "37.980000", 1000, 300, 4000},
			{evalDay3, "model-b", "p2", "0.030000", "0.150000", 30, 30, 50},
		}
		if len(got) != len(want) {
			t.Fatalf("v_eval_cost_latency rows = %v, want %v", got, want)
		}
		for i := range want {
			requireUTCMidnight(t, "v_eval_cost_latency", got[i].day)
			if !got[i].day.Equal(want[i].day) || got[i].model != want[i].model ||
				got[i].prompt != want[i].prompt || got[i].avgCost != want[i].avgCost ||
				got[i].totalCost != want[i].totalCost {
				t.Errorf("v_eval_cost_latency[%d] = %+v, want %+v", i, got[i], want[i])
			}
			requireFloat(t, fmt.Sprintf("v_eval_cost_latency[%d].avg_latency_ms", i), got[i].avgLatency, want[i].avgLatency)
			requireFloat(t, fmt.Sprintf("v_eval_cost_latency[%d].p50_latency_ms", i), got[i].p50, want[i].p50)
			requireFloat(t, fmt.Sprintf("v_eval_cost_latency[%d].p95_latency_ms", i), got[i].p95, want[i].p95)
		}
	})
}

// TestM505OverrideFixtures is FIN-M5-05-05: terminal approved/rejected decisions
// only, pending approvals excluded, zero-rejection rate is a measured zero, and
// a decision whose agent run is missing groups under 'unknown' without losing
// the rejection.
func TestM505OverrideFixtures(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	seedOverrideFixture(t, d)
	conn := openDashboardConn(t, d)

	type row struct {
		day        time.Time
		model      string
		prompt     string
		rejections int64
		decisions  int64
		rate       string
	}
	rows, err := conn.Query(ctx,
		`SELECT day, model, prompt_version, rejections, decisions, round(rate, 6)::text
		   FROM v_eval_override_rate
		  ORDER BY day, model, prompt_version`)
	if err != nil {
		t.Fatalf("query v_eval_override_rate: %v", err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.day, &r.model, &r.prompt, &r.rejections, &r.decisions, &r.rate); err != nil {
			rows.Close()
			t.Fatalf("scan v_eval_override_rate: %v", err)
		}
		got = append(got, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate v_eval_override_rate: %v", err)
	}

	want := []row{
		{evalDay1, "model-a", "p1", 1, 2, "0.500000"},
		{evalDay1, "unknown", "unknown", 1, 1, "1.000000"},
		{evalDay2, "model-b", "p2", 0, 1, "0.000000"},
	}
	if len(got) != len(want) {
		t.Fatalf("v_eval_override_rate rows = %v, want %v", got, want)
	}
	for i := range want {
		requireUTCMidnight(t, "v_eval_override_rate", got[i].day)
		if !got[i].day.Equal(want[i].day) || got[i].model != want[i].model ||
			got[i].prompt != want[i].prompt || got[i].rejections != want[i].rejections ||
			got[i].decisions != want[i].decisions || got[i].rate != want[i].rate {
			t.Errorf("v_eval_override_rate[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	var totalDecisions, totalRejections int64
	if err := conn.QueryRow(ctx,
		`SELECT COALESCE(sum(decisions), 0), COALESCE(sum(rejections), 0)
		   FROM v_eval_override_rate`).Scan(&totalDecisions, &totalRejections); err != nil {
		t.Fatalf("sum v_eval_override_rate: %v", err)
	}
	if totalDecisions != 4 || totalRejections != 2 {
		t.Errorf("override totals decisions/rejections = %d/%d, want 4/2 (pending excluded, unknown kept)",
			totalDecisions, totalRejections)
	}
}

// TestM505ApplyFailureFixtures is FIN-M5-05-06: strict applied+apply_failed
// denominator, applied_at attribution, latest apply_failed audit attribution,
// created_at fallback, exclusion of every other proposal status, and no double
// counting of one proposal.
func TestM505ApplyFailureFixtures(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	seedApplyFailureFixture(t, d)
	conn := openDashboardConn(t, d)

	type row struct {
		day      time.Time
		model    string
		prompt   string
		failures int64
		attempts int64
		rate     string
	}
	rows, err := conn.Query(ctx,
		`SELECT day, model, prompt_version, failures, attempts, round(rate, 6)::text
		   FROM v_eval_apply_failure_rate
		  ORDER BY day, model, prompt_version`)
	if err != nil {
		t.Fatalf("query v_eval_apply_failure_rate: %v", err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.day, &r.model, &r.prompt, &r.failures, &r.attempts, &r.rate); err != nil {
			rows.Close()
			t.Fatalf("scan v_eval_apply_failure_rate: %v", err)
		}
		got = append(got, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate v_eval_apply_failure_rate: %v", err)
	}

	want := []row{
		{evalDay1, "model-a", "p1", 0, 1, "0.000000"},
		{evalDay1, "model-b", "p2", 1, 2, "0.500000"},
		{evalDay2, "model-a", "p1", 2, 2, "1.000000"},
		{evalDay3, "model-a", "p1", 1, 1, "1.000000"},
		{evalDay4, "model-a", "p1", 0, 1, "0.000000"},
		{evalDay5, "unknown", "unknown", 1, 1, "1.000000"},
	}
	if len(got) != len(want) {
		t.Fatalf("v_eval_apply_failure_rate rows = %v, want %v", got, want)
	}
	for i := range want {
		requireUTCMidnight(t, "v_eval_apply_failure_rate", got[i].day)
		if !got[i].day.Equal(want[i].day) || got[i].model != want[i].model ||
			got[i].prompt != want[i].prompt || got[i].failures != want[i].failures ||
			got[i].attempts != want[i].attempts || got[i].rate != want[i].rate {
			t.Errorf("v_eval_apply_failure_rate[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	var attempts, failures int64
	if err := conn.QueryRow(ctx,
		`SELECT COALESCE(sum(attempts), 0), COALESCE(sum(failures), 0)
		   FROM v_eval_apply_failure_rate`).Scan(&attempts, &failures); err != nil {
		t.Fatalf("sum v_eval_apply_failure_rate: %v", err)
	}
	if attempts != 8 || failures != 5 {
		t.Errorf("apply totals attempts/failures = %d/%d, want 8/5 (strict terminal denominator, one row per proposal)",
			attempts, failures)
	}
}

// TestM505EscalationFixtures is FIN-M5-05-07 and -08: escalation reasons from
// audit (empty/missing -> unknown, non-escalation rows excluded), event counts,
// and a same-cohort rate keyed by case creation day. A later escalation still
// counts in its creation-day cohort; an event day with no created cases emits
// no rate row; a case escalated twice counts once in the rate; a zero numerator
// over a nonzero denominator is a measured zero.
func TestM505EscalationFixtures(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	seedEscalationFixture(t, d)
	conn := openDashboardConn(t, d)

	t.Run("reasons", func(t *testing.T) {
		type row struct {
			day    time.Time
			reason string
			count  int64
		}
		rows, err := conn.Query(ctx,
			`SELECT day, reason, count
			   FROM v_eval_escalations
			  ORDER BY day, reason`)
		if err != nil {
			t.Fatalf("query v_eval_escalations: %v", err)
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.day, &r.reason, &r.count); err != nil {
				rows.Close()
				t.Fatalf("scan v_eval_escalations: %v", err)
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_eval_escalations: %v", err)
		}

		want := []row{
			{evalDay1, "policy", 2},
			{evalDay1, "unknown", 1},
			{evalDay2, "low_confidence", 1},
			{evalDay2, "unknown", 1},
			{evalDay4, "policy", 1},
		}
		if len(got) != len(want) {
			t.Fatalf("v_eval_escalations rows = %v, want %v", got, want)
		}
		for i := range want {
			requireUTCMidnight(t, "v_eval_escalations", got[i].day)
			if !got[i].day.Equal(want[i].day) || got[i].reason != want[i].reason || got[i].count != want[i].count {
				t.Errorf("v_eval_escalations[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("same cohort rate", func(t *testing.T) {
		type row struct {
			day       time.Time
			escalated int64
			total     int64
			rate      string
		}
		rows, err := conn.Query(ctx,
			`SELECT day, escalated, total_cases, round(rate, 6)::text
			   FROM v_eval_escalation_rate
			  ORDER BY day`)
		if err != nil {
			t.Fatalf("query v_eval_escalation_rate: %v", err)
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.day, &r.escalated, &r.total, &r.rate); err != nil {
				rows.Close()
				t.Fatalf("scan v_eval_escalation_rate: %v", err)
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate v_eval_escalation_rate: %v", err)
		}

		want := []row{
			{evalDay1, 4, 5, "0.800000"},
			{evalDay2, 0, 2, "0.000000"},
			{evalDay3, 1, 1, "1.000000"},
		}
		if len(got) != len(want) {
			t.Fatalf("v_eval_escalation_rate rows = %v, want %v", got, want)
		}
		for i := range want {
			requireUTCMidnight(t, "v_eval_escalation_rate", got[i].day)
			if !got[i].day.Equal(want[i].day) || got[i].escalated != want[i].escalated ||
				got[i].total != want[i].total || got[i].rate != want[i].rate {
				t.Errorf("v_eval_escalation_rate[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
		var totalEscalated, totalCases int64
		if err := conn.QueryRow(ctx,
			`SELECT COALESCE(sum(escalated), 0), COALESCE(sum(total_cases), 0)
			   FROM v_eval_escalation_rate`).Scan(&totalEscalated, &totalCases); err != nil {
			t.Fatalf("sum v_eval_escalation_rate: %v", err)
		}
		if totalEscalated != 5 || totalCases != 8 {
			t.Errorf("cohort totals escalated/cases = %d/%d, want 5/8 (double escalation counted once; later event in creation cohort)",
				totalEscalated, totalCases)
		}
	})
}

func insertEvalRun(t *testing.T, ctx context.Context, tx pgx.Tx, suite, split, model, prompt string, startedAt time.Time, endedAt *time.Time, repeatGroup *string) string {
	t.Helper()
	var repeatIndex *int
	if repeatGroup != nil {
		index := 0
		repeatIndex = &index
	}
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO eval_runs
		     (suite, split, dataset_version, model, prompt_version, seed,
		      started_at, ended_at, repeat_group, repeat_index)
		 VALUES ($1, $2, 'm5-05-fixture', $3, $4, 1, $5, $6, $7::uuid, $8)
		 RETURNING id::text`,
		suite, split, model, prompt, startedAt, endedAt, repeatGroup, repeatIndex).Scan(&id); err != nil {
		t.Fatalf("insert eval run %s/%s %s/%s: %v", suite, split, model, prompt, err)
	}
	return id
}

type evalResultFixture struct {
	correct  bool
	falseFix bool
	costUSD  string
	latency  int
}

func insertEvalResults(t *testing.T, ctx context.Context, tx pgx.Tx, runID string, results []evalResultFixture) {
	t.Helper()
	for i, result := range results {
		if _, err := tx.Exec(ctx,
			`INSERT INTO eval_case_results
			     (eval_run_id, scenario_id, failure_mode, correct, false_fix,
			      escalated, cost_usd, latency_ms)
			 VALUES ($1::uuid, $2, 'fixture', $3, $4, false, $5::numeric, $6)`,
			runID, fmt.Sprintf("scenario-%02d", i+1), result.correct, result.falseFix,
			result.costUSD, result.latency); err != nil {
			t.Fatalf("insert eval result %s/%d: %v", runID, i+1, err)
		}
	}
}

func seedEvalViewFixture(t *testing.T, d *testdb.DB) {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin eval fixture transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	finished := func(start time.Time) *time.Time {
		end := start.Add(time.Hour)
		return &end
	}

	// Included: one agent/dev day and one e2e/test day for model-a/p1.
	r1 := insertEvalRun(t, ctx, tx, "agent", "dev", "model-a", "p1",
		evalDay1.Add(10*time.Hour+30*time.Minute), finished(evalDay1.Add(10*time.Hour+30*time.Minute)), nil)
	insertEvalResults(t, ctx, tx, r1, []evalResultFixture{
		{correct: true, falseFix: false, costUSD: "0.10", latency: 10},
		{correct: false, falseFix: true, costUSD: "0.20", latency: 20},
		{correct: true, falseFix: false, costUSD: "0.30", latency: 30},
		{correct: false, falseFix: false, costUSD: "0.40", latency: 40},
	})

	r2 := insertEvalRun(t, ctx, tx, "e2e", "test", "model-a", "p1",
		evalDay2.Add(9*time.Hour), finished(evalDay2.Add(9*time.Hour)), nil)
	insertEvalResults(t, ctx, tx, r2, []evalResultFixture{
		{correct: true, falseFix: false, costUSD: "0.50", latency: 100},
		{correct: true, falseFix: false, costUSD: "1.50", latency: 200},
		{correct: false, falseFix: true, costUSD: "2.50", latency: 300},
		{correct: false, falseFix: false, costUSD: "3.50", latency: 400},
	})

	// Injection: excluded from accuracy but included in false-fix and
	// cost/latency with deliberately distinctive values.
	r3 := insertEvalRun(t, ctx, tx, "agent", "injection", "model-a", "p1",
		evalDay2.Add(23*time.Hour+45*time.Minute), finished(evalDay2.Add(23*time.Hour+45*time.Minute)), nil)
	insertEvalResults(t, ctx, tx, r3, []evalResultFixture{
		{correct: false, falseFix: true, costUSD: "9.99", latency: 1000},
		{correct: false, falseFix: true, costUSD: "19.99", latency: 4000},
	})

	// Included odd-count group: nearest-rank p50 is the third value.
	r4 := insertEvalRun(t, ctx, tx, "agent", "dev", "model-b", "p2",
		evalDay3.Add(10*time.Hour), finished(evalDay3.Add(10*time.Hour)), nil)
	insertEvalResults(t, ctx, tx, r4, []evalResultFixture{
		{correct: true, falseFix: false, costUSD: "0.01", latency: 10},
		{correct: true, falseFix: false, costUSD: "0.02", latency: 20},
		{correct: true, falseFix: false, costUSD: "0.03", latency: 30},
		{correct: false, falseFix: true, costUSD: "0.04", latency: 40},
		{correct: false, falseFix: false, costUSD: "0.05", latency: 50},
	})

	// Excluded runs: repeat group, unfinished, and matcher suite. Their
	// distinctive values must not leak into any eval view.
	repeatGroup := "00000000-0000-4000-8000-000000000005"
	r5 := insertEvalRun(t, ctx, tx, "agent", "dev", "model-a", "p1",
		evalDay4.Add(10*time.Hour), finished(evalDay4.Add(10*time.Hour)), &repeatGroup)
	insertEvalResults(t, ctx, tx, r5, []evalResultFixture{
		{correct: true, falseFix: false, costUSD: "777", latency: 777},
	})
	r6 := insertEvalRun(t, ctx, tx, "agent", "dev", "model-a", "p1",
		evalDay4.Add(11*time.Hour), nil, nil)
	insertEvalResults(t, ctx, tx, r6, []evalResultFixture{
		{correct: true, falseFix: false, costUSD: "777", latency: 777},
	})
	r7 := insertEvalRun(t, ctx, tx, "matcher", "dev", "model-z", "p9",
		evalDay4.Add(12*time.Hour), finished(evalDay4.Add(12*time.Hour)), nil)
	insertEvalResults(t, ctx, tx, r7, []evalResultFixture{
		{correct: true, falseFix: false, costUSD: "777", latency: 777},
	})

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit eval fixture: %v", err)
	}
}

func insertEvalAgentRun(t *testing.T, ctx context.Context, tx pgx.Tx, caseID, model, prompt string, startedAt time.Time) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO agent_runs
		     (case_id, model, prompt_version, started_at, ended_at, outcome)
		 VALUES ($1::uuid, $2, $3, $4, $5, 'proposal')
		 RETURNING id::text`,
		caseID, model, prompt, startedAt, startedAt.Add(time.Minute)).Scan(&id); err != nil {
		t.Fatalf("insert eval agent run %s/%s: %v", model, prompt, err)
	}
	return id
}

func seedOverrideFixture(t *testing.T, d *testdb.DB) {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin override fixture transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	approver := insertApprover(t, ctx, tx, "Eval Override Approver")

	insertDecision := func(key, model, prompt, proposalStatus, decision string, createdAt, decidedAt time.Time) {
		caseID := insertCase(t, ctx, tx, "open", createdAt.Add(-time.Hour), nil, nil)
		runID := insertEvalAgentRun(t, ctx, tx, caseID, model, prompt, createdAt.Add(-30*time.Minute))
		proposalID := insertProposal(t, ctx, tx, caseID, runID, nil, proposalStatus, createdAt, "override-"+key)
		if decision != "" {
			insertApproval(t, ctx, tx, proposalID, approver, decision, decidedAt)
		}
	}

	// model-a/p1: one approved and one rejected terminal decision.
	insertDecision("a-approved", "model-a", "p1", "approved", "approved",
		evalDay1.Add(8*time.Hour), evalDay1.Add(9*time.Hour))
	insertDecision("a-rejected", "model-a", "p1", "rejected", "rejected",
		evalDay1.Add(8*time.Hour+10*time.Minute), evalDay1.Add(10*time.Hour))
	// A pending approval has no approvals row and must not enter either side.
	insertDecision("a-pending", "model-a", "p1", "pending_approval", "",
		evalDay1.Add(8*time.Hour+20*time.Minute), time.Time{})

	// model-b/p2: a single approval is a measured zero rejection rate.
	insertDecision("b-approved", "model-b", "p2", "approved", "approved",
		evalDay2.Add(8*time.Hour), evalDay2.Add(9*time.Hour))

	// Unknown run: the schema's FK normally prevents this shape, so the
	// fallback is exercised with a replica-mode bypass in a disposable test
	// database. The rejection must still count under 'unknown'.
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("enable replica mode to seed a missing agent run (requires the superuser test role): %v", err)
	}
	unknownCase := insertCase(t, ctx, tx, "open", evalDay1.Add(-2*time.Hour), nil, nil)
	const danglingRun = "00000000-0000-4000-8000-0000000000aa"
	unknownProposal := insertProposal(t, ctx, tx, unknownCase, danglingRun, nil, "rejected",
		evalDay1.Add(8*time.Hour+30*time.Minute), "override-unknown")
	insertApproval(t, ctx, tx, unknownProposal, approver, "rejected", evalDay1.Add(11*time.Hour))
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = origin`); err != nil {
		t.Fatalf("restore session_replication_role: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit override fixture: %v", err)
	}
}

func seedApplyFailureFixture(t *testing.T, d *testdb.DB) {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin apply fixture transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	proposalFor := func(key, model, prompt, status string, createdAt time.Time) string {
		caseID := insertCase(t, ctx, tx, "open", createdAt.Add(-time.Hour), nil, nil)
		runID := insertEvalAgentRun(t, ctx, tx, caseID, model, prompt, createdAt.Add(-30*time.Minute))
		return insertProposal(t, ctx, tx, caseID, runID, nil, status, createdAt, "apply-"+key)
	}

	// Applied attempts are attributed to applied_at. a2 has no
	// applied_proposals row and must be excluded even though its status is
	// applied.
	a1 := proposalFor("a1", "model-a", "p1", "applied", evalDay1.Add(8*time.Hour))
	insertApplied(t, ctx, tx, a1, evalDay1.Add(12*time.Hour), "exec-a1")
	proposalFor("a2-no-row", "model-a", "p1", "applied", evalDay1.Add(8*time.Hour+10*time.Minute))
	a3 := proposalFor("a3", "model-a", "p1", "applied", evalDay4.Add(8*time.Hour))
	insertApplied(t, ctx, tx, a3, evalDay4.Add(12*time.Hour), "exec-a3")

	// Apply-failed attribution: latest audit timestamp wins; a missing audit
	// falls back to created_at.
	f1 := proposalFor("f1", "model-a", "p1", "apply_failed", evalDay1.Add(7*time.Hour))
	appendEvalAudit(t, ctx, tx, "proposal.transition", "proposal", f1,
		map[string]any{"status": "apply_failed", "reason": "state_changed"}, evalDay2.Add(9*time.Hour))
	proposalFor("f2", "model-a", "p1", "apply_failed", evalDay3.Add(8*time.Hour))
	f3 := proposalFor("f3", "model-a", "p1", "apply_failed", evalDay1.Add(6*time.Hour))
	appendEvalAudit(t, ctx, tx, "proposal.transition", "proposal", f3,
		map[string]any{"status": "apply_failed", "reason": "break_resolved"}, evalDay1.Add(6*time.Hour+30*time.Minute))
	appendEvalAudit(t, ctx, tx, "proposal.transition", "proposal", f3,
		map[string]any{"status": "apply_failed", "reason": "expired"}, evalDay2.Add(11*time.Hour))

	// Excluded statuses on both sides.
	proposalFor("pending", "model-a", "p1", "pending_approval", evalDay1.Add(8*time.Hour))
	proposalFor("rejected", "model-a", "p1", "rejected", evalDay1.Add(8*time.Hour))
	proposalFor("superseded", "model-a", "p1", "superseded", evalDay1.Add(8*time.Hour))
	proposalFor("approved", "model-a", "p1", "approved", evalDay1.Add(8*time.Hour))

	// Second group: one applied and one fallback failure on the same day.
	a4 := proposalFor("a4", "model-b", "p2", "applied", evalDay1.Add(17*time.Hour))
	insertApplied(t, ctx, tx, a4, evalDay1.Add(18*time.Hour), "exec-a4")
	proposalFor("f4", "model-b", "p2", "apply_failed", evalDay1.Add(19*time.Hour))

	// Unknown run: a terminal apply_failed proposal whose agent_run_id does
	// not resolve groups as 'unknown' at its created_at fallback day.
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("enable replica mode to seed a missing agent run (requires the superuser test role): %v", err)
	}
	unknownCase := insertCase(t, ctx, tx, "open", evalDay5.Add(-time.Hour), nil, nil)
	const danglingRun = "00000000-0000-4000-8000-0000000000bb"
	insertProposal(t, ctx, tx, unknownCase, danglingRun, nil, "apply_failed",
		evalDay5.Add(8*time.Hour), "apply-unknown")
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = origin`); err != nil {
		t.Fatalf("restore session_replication_role: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit apply fixture: %v", err)
	}
}

func seedEscalationFixture(t *testing.T, d *testdb.DB) {
	t.Helper()
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin escalation fixture transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	caseIDs := make(map[string]string, 8)
	for i, key := range []string{"c1", "c2", "c3", "c4", "c8"} {
		caseIDs[key] = insertCase(t, ctx, tx, "open", evalDay1.Add(time.Duration(i+1)*time.Hour), nil, nil)
	}
	for i, key := range []string{"c5", "c6"} {
		caseIDs[key] = insertCase(t, ctx, tx, "open", evalDay2.Add(time.Duration(i+1)*time.Hour), nil, nil)
	}
	caseIDs["c7"] = insertCase(t, ctx, tx, "open", evalDay3.Add(time.Hour), nil, nil)

	escalate := func(key, reason string, at time.Time, includeReason bool) {
		after := map[string]any{"status": "escalated"}
		if includeReason {
			after["reason"] = reason
		}
		appendEvalAudit(t, ctx, tx, "case.transition", "case", caseIDs[key], after, at)
	}
	escalate("c1", "policy", evalDay1.Add(10*time.Hour), true)
	escalate("c1", "policy", evalDay1.Add(11*time.Hour), true) // twice: two events, one rate case
	escalate("c2", "low_confidence", evalDay2.Add(10*time.Hour), true)
	escalate("c3", "", evalDay1.Add(12*time.Hour), true)       // empty -> unknown
	escalate("c4", "", evalDay2.Add(12*time.Hour), false)      // missing -> unknown
	escalate("c7", "policy", evalDay4.Add(10*time.Hour), true) // later event, creation-day cohort
	// Non-escalation and non-case-transition audit rows must be excluded.
	appendEvalAudit(t, ctx, tx, "case.transition", "case", caseIDs["c5"],
		map[string]any{"status": "waiting_info", "reason": "human_reply"}, evalDay2.Add(10*time.Hour))
	appendEvalAudit(t, ctx, tx, "proposal.transition", "proposal",
		"00000000-0000-4000-8000-0000000000cc",
		map[string]any{"status": "escalated", "reason": "policy"}, evalDay2.Add(13*time.Hour))

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit escalation fixture: %v", err)
	}
}

func appendEvalAudit(t *testing.T, ctx context.Context, tx pgx.Tx, action, entityType, entityID string, after map[string]any, at time.Time) {
	t.Helper()
	actor, err := audit.SystemActor("m5-05-eval-fixture")
	if err != nil {
		t.Fatalf("build audit actor: %v", err)
	}
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      actor,
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		After:      after,
		Timestamp:  at,
	}); err != nil {
		t.Fatalf("append %s audit for %s: %v", action, entityID, err)
	}
}

func requireUTCMidnight(t *testing.T, view string, day time.Time) {
	t.Helper()
	utc := day.UTC()
	if utc.Hour() != 0 || utc.Minute() != 0 || utc.Second() != 0 || utc.Nanosecond() != 0 {
		t.Errorf("%s day = %v, want UTC midnight", view, day)
	}
}

func requireFloat(t *testing.T, label string, got, want float64) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}
