//go:build integration

package demo_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/demo"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
)

func pinnedQualityClock() time.Time {
	return time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
}

func qualityOptions(d *testdb.DB, now time.Time) demo.Options {
	return demo.Options{
		DBURL:      d.DSN,
		Seed:       1,
		Days:       14,
		Difficulty: synth.DifficultyRealistic,
		Clock:      clock.NewFake(now),
	}
}

func runQuality(t *testing.T, d *testdb.DB, now time.Time) demo.QualitySummary {
	t.Helper()
	summary, err := demo.RunQuality(context.Background(), qualityOptions(d, now))
	if err != nil {
		t.Fatalf("demo quality: %v", err)
	}
	return summary
}

func qualityRowCounts(t *testing.T, d *testdb.DB) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	counts := map[string]int64{}
	queries := map[string]string{
		"eval_runs":         `SELECT count(*) FROM eval_runs`,
		"eval_case_results": `SELECT count(*) FROM eval_case_results`,
		"quality_proposals": `SELECT count(*) FROM proposals WHERE idempotency_key LIKE 'demo-quality:%'`,
		"quality_runs":      `SELECT count(*) FROM agent_runs WHERE trace_id LIKE 'demo-quality:%'`,
		"quality_approvals": `SELECT count(*) FROM approvals WHERE channel = 'demo-quality'`,
		"quality_applied": `SELECT count(*) FROM applied_proposals ap
		                      JOIN proposals p ON p.id = ap.proposal_id
		                     WHERE p.idempotency_key LIKE 'demo-quality:%'`,
		"quality_cases": `SELECT count(*) FROM (
		                     SELECT case_id AS id FROM proposals WHERE idempotency_key LIKE 'demo-quality:%'
		                     UNION
		                     SELECT entity_id::uuid FROM audit_log
		                      WHERE action = 'case.transition' AND after->>'status' = 'escalated'
		                   ) cases`,
		"case_audits": `SELECT count(*) FROM audit_log
		                 WHERE action = 'case.transition' AND after->>'status' = 'escalated'`,
		"apply_failed_audits": `SELECT count(*) FROM audit_log
		                         WHERE action = 'proposal.transition'
		                           AND after->>'status' = 'apply_failed'`,
	}
	for name, query := range queries {
		var count int64
		if err := d.Pool.QueryRow(ctx, query).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		counts[name] = count
	}
	return counts
}

func TestM505DemoQualityLayersAndCounts(t *testing.T) {
	d := testdb.New(t)
	summary := runQuality(t, d, pinnedQualityClock())

	if summary.Ops == nil {
		t.Fatal("fresh demo quality run did not seed the M1+ops lower layers")
	}
	if got, want := summary.Layers, "M1+ops+quality"; got != want {
		t.Errorf("layers = %q, want %q", got, want)
	}
	want := demo.QualitySummary{
		EvalRuns:       13,
		EvalCaseResult: 64,
		Approvers:      2,
		AgentRuns:      10,
		Proposals:      10,
		Approvals:      5,
		Approved:       3,
		Rejected:       2,
		Applied:        6,
		ApplyFailed:    2,
		Cases:          12,
		Escalations:    4,
	}
	for name, pair := range map[string]struct {
		got  int
		want int
	}{
		"eval_runs":    {summary.EvalRuns, want.EvalRuns},
		"eval_results": {summary.EvalCaseResult, want.EvalCaseResult},
		"approvers":    {summary.Approvers, want.Approvers},
		"agent_runs":   {summary.AgentRuns, want.AgentRuns},
		"proposals":    {summary.Proposals, want.Proposals},
		"approvals":    {summary.Approvals, want.Approvals},
		"approved":     {summary.Approved, want.Approved},
		"rejected":     {summary.Rejected, want.Rejected},
		"applied":      {summary.Applied, want.Applied},
		"apply_failed": {summary.ApplyFailed, want.ApplyFailed},
		"cases":        {summary.Cases, want.Cases},
		"escalations":  {summary.Escalations, want.Escalations},
	} {
		if pair.got != pair.want {
			t.Errorf("%s = %d, want %d", name, pair.got, pair.want)
		}
	}
	if got, want := summary.EscalationKnown, map[string]int{"policy": 2, "low_confidence": 1, "unknown": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("escalation reasons = %v, want %v", got, want)
	}

	counts := qualityRowCounts(t, d)
	if got, want := counts["eval_runs"], int64(13); got != want {
		t.Errorf("eval_runs rows = %d, want %d", got, want)
	}
	if got, want := counts["eval_case_results"], int64(64); got != want {
		t.Errorf("eval_case_results rows = %d, want %d", got, want)
	}
	if got, want := counts["quality_proposals"], int64(10); got != want {
		t.Errorf("quality proposals = %d, want %d", got, want)
	}
	if got, want := counts["quality_runs"], int64(10); got != want {
		t.Errorf("quality agent runs = %d, want %d", got, want)
	}
	if got, want := counts["quality_approvals"], int64(5); got != want {
		t.Errorf("quality approvals = %d, want %d", got, want)
	}
	if got, want := counts["quality_applied"], int64(6); got != want {
		t.Errorf("quality applied proposals = %d, want %d", got, want)
	}
	if got, want := counts["quality_cases"], int64(12); got != want {
		t.Errorf("quality cases = %d, want %d", got, want)
	}
	if got, want := counts["case_audits"], int64(4); got != want {
		t.Errorf("escalation audits = %d, want %d", got, want)
	}
	if got, want := counts["apply_failed_audits"], int64(2); got != want {
		t.Errorf("apply_failed audits = %d, want %d", got, want)
	}

	// The ops views stay non-empty (the quality layer is a strict superset).
	for _, view := range []string{
		"v_ops_match_rate", "v_ops_unmatched_value", "v_ops_break_aging",
		"v_ops_approval_queue", "v_ops_resolution_mix",
		"v_eval_accuracy", "v_eval_false_fix", "v_eval_cost_latency",
		"v_eval_override_rate", "v_eval_apply_failure_rate",
		"v_eval_escalations", "v_eval_escalation_rate",
	} {
		var count int64
		if err := d.Pool.QueryRow(context.Background(),
			"SELECT count(*) FROM "+view).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", view, err)
		}
		if count == 0 {
			t.Errorf("%s has no rows after demo quality", view)
		}
	}
}

func TestM505DemoQualityViews(t *testing.T) {
	d := testdb.New(t)
	runQuality(t, d, pinnedQualityClock())
	ctx := context.Background()

	type accRow struct {
		day     string
		model   string
		prompt  string
		correct int64
		total   int64
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT to_char(day AT TIME ZONE 'UTC', 'YYYY-MM-DD'), model, prompt_version, correct, total
		   FROM v_eval_accuracy
		  WHERE model IN ($1, $2)
		  ORDER BY 1, 2, 3`, demo.QualityModelA, demo.QualityModelB)
	if err != nil {
		t.Fatalf("query accuracy: %v", err)
	}
	gotAcc := map[string]accRow{}
	for rows.Next() {
		var row accRow
		if err := rows.Scan(&row.day, &row.model, &row.prompt, &row.correct, &row.total); err != nil {
			rows.Close()
			t.Fatalf("scan accuracy: %v", err)
		}
		gotAcc[row.day+"|"+row.model+"|"+row.prompt] = row
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate accuracy: %v", err)
	}
	wantAcc := map[string][2]int64{
		"2026-11-19|demo-model-a|demo-prompt-v1": {4, 6},
		"2026-11-21|demo-model-a|demo-prompt-v1": {3, 5},
		"2026-11-20|demo-model-a|demo-prompt-v2": {4, 6},
		"2026-11-22|demo-model-a|demo-prompt-v2": {3, 5},
		"2026-11-21|demo-model-b|demo-prompt-v1": {4, 6},
		"2026-11-23|demo-model-b|demo-prompt-v1": {3, 5},
		"2026-11-24|demo-model-b|demo-prompt-v2": {4, 6},
		"2026-11-25|demo-model-b|demo-prompt-v2": {3, 5},
	}
	if len(gotAcc) != len(wantAcc) {
		t.Errorf("accuracy rows = %d, want %d (%v)", len(gotAcc), len(wantAcc), gotAcc)
	}
	for key, want := range wantAcc {
		row, ok := gotAcc[key]
		if !ok {
			t.Errorf("accuracy missing %s", key)
			continue
		}
		if row.correct != want[0] || row.total != want[1] {
			t.Errorf("accuracy %s = %d/%d, want %d/%d", key, row.correct, row.total, want[0], want[1])
		}
	}
	if _, ok := gotAcc["2026-11-24|demo-model-a|demo-prompt-v1"]; ok {
		t.Error("accuracy includes the excluded repeat-group/matcher day for demo-model-a/demo-prompt-v1")
	}

	// False-fix includes injection and excludes repeat/unfinished/matcher.
	type ffRow struct {
		day, model, prompt string
		falseFix, total    int64
	}
	rows, err = d.Pool.Query(ctx,
		`SELECT to_char(day AT TIME ZONE 'UTC', 'YYYY-MM-DD'), model, prompt_version, false_fix, total
		   FROM v_eval_false_fix
		  WHERE model IN ($1, $2)
		  ORDER BY 1, 2, 3`, demo.QualityModelA, demo.QualityModelB)
	if err != nil {
		t.Fatalf("query false-fix: %v", err)
	}
	gotFF := map[string]ffRow{}
	for rows.Next() {
		var row ffRow
		if err := rows.Scan(&row.day, &row.model, &row.prompt, &row.falseFix, &row.total); err != nil {
			rows.Close()
			t.Fatalf("scan false-fix: %v", err)
		}
		gotFF[row.day+"|"+row.model+"|"+row.prompt] = row
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate false-fix: %v", err)
	}
	if got, ok := gotFF["2026-11-23|demo-model-a|demo-prompt-v1"]; !ok || got.falseFix != 2 || got.total != 4 {
		t.Errorf("injection false-fix group = %+v, want false_fix=2 total=4", got)
	}
	if _, ok := gotFF["2026-11-24|demo-model-a|demo-prompt-v1"]; ok {
		t.Error("false-fix includes the excluded repeat/matcher rows")
	}
	if got, ok := gotFF["2026-11-24|demo-model-b|demo-prompt-v2"]; !ok || got.falseFix != 1 || got.total != 6 {
		t.Errorf("unfinished-run day false-fix group = %+v, want false_fix=1 total=6 (exclusion rows absent)", got)
	}
	if got, ok := gotFF["2026-11-25|demo-model-b|demo-prompt-v2"]; !ok || got.falseFix != 3 || got.total != 9 {
		t.Errorf("injection+test day false-fix group = %+v, want false_fix=3 total=9", got)
	}

	// Cost/latency includes the distinctive injection group and never the
	// 777.000000 exclusion values.
	var avgCost, totalCost, p50, p95, avgLatency float64
	if err := d.Pool.QueryRow(ctx,
		`SELECT avg_cost_usd::double precision, total_cost_usd::double precision,
		        p50_latency_ms, p95_latency_ms, avg_latency_ms
		   FROM v_eval_cost_latency
		  WHERE model = $1 AND prompt_version = $2
		    AND day = TIMESTAMPTZ '2026-11-23 00:00:00+00'`,
		demo.QualityModelA, demo.QualityPromptV1).Scan(&avgCost, &totalCost, &p50, &p95, &avgLatency); err != nil {
		t.Fatalf("query injection cost/latency: %v", err)
	}
	if diff := avgCost - 0.4999995; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("injection avg cost = %v, want 0.4999995", avgCost)
	}
	if diff := totalCost - 1.999998; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("injection total cost = %v, want 1.999998", totalCost)
	}
	if p50 != 2202 || p95 != 9101 {
		t.Errorf("injection latency p50/p95 = %v/%v, want 2202/9101 (nearest-rank)", p50, p95)
	}
	if diff := avgLatency - 5351.25; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("injection avg latency = %v, want 5351.25", avgLatency)
	}
	var maxCost float64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(avg_cost_usd), 0)::double precision
		   FROM v_eval_cost_latency WHERE model IN ($1, $2)`,
		demo.QualityModelA, demo.QualityModelB).Scan(&maxCost); err != nil {
		t.Fatalf("max cost: %v", err)
	}
	if maxCost > 100 {
		t.Errorf("cost/latency view includes exclusion rows: max avg cost = %v", maxCost)
	}

	// Override: five terminal quality decisions, two rejections.
	var decisions, rejections int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(sum(decisions), 0), COALESCE(sum(rejections), 0)
		   FROM v_eval_override_rate WHERE model IN ($1, $2)`,
		demo.QualityModelA, demo.QualityModelB).Scan(&decisions, &rejections); err != nil {
		t.Fatalf("query override: %v", err)
	}
	if decisions != 5 || rejections != 2 {
		t.Errorf("override decisions/rejections = %d/%d, want 5/2", decisions, rejections)
	}

	// Apply failure: strict terminal denominator; the cross-day audit moves
	// proposal-10 from its created day (Nov 21) to the audit day (Nov 22).
	var attempts, failures int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(sum(attempts), 0), COALESCE(sum(failures), 0)
		   FROM v_eval_apply_failure_rate WHERE model IN ($1, $2)`,
		demo.QualityModelA, demo.QualityModelB).Scan(&attempts, &failures); err != nil {
		t.Fatalf("query apply-failure: %v", err)
	}
	if attempts != 8 || failures != 2 {
		t.Errorf("apply attempts/failures = %d/%d, want 8/2", attempts, failures)
	}
	var crossDayFailures int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(sum(failures), 0)
		   FROM v_eval_apply_failure_rate
		  WHERE model = $1 AND prompt_version = $2
		    AND day = TIMESTAMPTZ '2026-11-22 00:00:00+00'`,
		demo.QualityModelB, demo.QualityPromptV2).Scan(&crossDayFailures); err != nil {
		t.Fatalf("query cross-day apply failure: %v", err)
	}
	if crossDayFailures != 1 {
		t.Errorf("cross-day audit failures on Nov 22 = %d, want 1", crossDayFailures)
	}
	var createdDayFailures int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(sum(failures), 0)
		   FROM v_eval_apply_failure_rate
		  WHERE model = $1 AND prompt_version = $2
		    AND day = TIMESTAMPTZ '2026-11-21 00:00:00+00'`,
		demo.QualityModelB, demo.QualityPromptV2).Scan(&createdDayFailures); err != nil {
		t.Fatalf("query created-day apply failure: %v", err)
	}
	if createdDayFailures != 0 {
		t.Errorf("created-day failures = %d, want 0 (latest audit attribution)", createdDayFailures)
	}

	// Escalation reasons: policy x2, low_confidence x1, unknown x1.
	rows, err = d.Pool.Query(ctx,
		`SELECT reason, sum(count)::bigint FROM v_eval_escalations GROUP BY reason`)
	if err != nil {
		t.Fatalf("query escalations: %v", err)
	}
	reasons := map[string]int64{}
	for rows.Next() {
		var reason string
		var count int64
		if err := rows.Scan(&reason, &count); err != nil {
			rows.Close()
			t.Fatalf("scan escalation: %v", err)
		}
		reasons[reason] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate escalations: %v", err)
	}
	wantReasons := map[string]int64{"policy": 2, "low_confidence": 1, "unknown": 1}
	if !reflect.DeepEqual(reasons, wantReasons) {
		t.Errorf("escalation reasons = %v, want %v", reasons, wantReasons)
	}

	// Cohort rate: case-4 was created Nov 22 and escalated Nov 23, so it is
	// counted in the Nov 22 creation-day cohort, not the event day.
	var escalated, total int64
	var rate *float64
	if err := d.Pool.QueryRow(ctx,
		`SELECT escalated, total_cases, rate::double precision
		   FROM v_eval_escalation_rate
		  WHERE day = TIMESTAMPTZ '2026-11-22 00:00:00+00'`).Scan(&escalated, &total, &rate); err != nil {
		t.Fatalf("query escalation rate: %v", err)
	}
	if escalated < 1 || total < escalated || rate == nil {
		t.Errorf("cohort Nov 22 = escalated %d total %d rate %v, want escalated>=1 and non-NULL", escalated, total, rate)
	}
	var anchorRate *float64
	if err := d.Pool.QueryRow(ctx,
		`SELECT rate::double precision FROM v_eval_escalation_rate
		  WHERE day = TIMESTAMPTZ '2026-11-25 00:00:00+00'`).Scan(&anchorRate); err != nil {
		t.Fatalf("query anchor escalation rate: %v", err)
	}
	if anchorRate == nil {
		t.Error("anchor-day escalation rate is NULL; the anchor cohort must have cases")
	}
}

func TestM505DemoQualityDeterministic(t *testing.T) {
	now := pinnedQualityClock()
	first := testdb.New(t)
	second := testdb.New(t)
	summaryFirst := runQuality(t, first, now)
	summarySecond := runQuality(t, second, now)

	var firstOut, secondOut strings.Builder
	if err := summaryFirst.Write(&firstOut); err != nil {
		t.Fatalf("write first summary: %v", err)
	}
	if err := summarySecond.Write(&secondOut); err != nil {
		t.Fatalf("write second summary: %v", err)
	}
	// Fresh runs print the full M1+ops summary too; the UUID-free text must be
	// identical for the same seed and pinned clock.
	if firstOut.String() != secondOut.String() {
		t.Fatalf("summary differs between deterministic seeds:\nfirst:\n%s\nsecond:\n%s",
			firstOut.String(), secondOut.String())
	}

	firstRows := qualityViewSnapshot(t, first)
	secondRows := qualityViewSnapshot(t, second)
	if !reflect.DeepEqual(firstRows, secondRows) {
		t.Fatalf("view snapshots differ between deterministic seeds:\nfirst=%v\nsecond=%v",
			firstRows, secondRows)
	}
}

// qualityViewSnapshot captures the quality rows of every v_eval_* view. Ops
// rows are excluded from the model-keyed views because their timestamps use
// the database clock, not the pinned quality clock.
func qualityViewSnapshot(t *testing.T, d *testdb.DB) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	queries := map[string]string{
		"accuracy": `SELECT to_char(day AT TIME ZONE 'UTC','YYYY-MM-DD') || '|' || model || '|' || prompt_version || '|' ||
		                   correct || '|' || total || '|' || COALESCE(accuracy::text,'<null>')
		               FROM v_eval_accuracy WHERE model IN ('demo-model-a','demo-model-b')
		              ORDER BY 1`,
		"false_fix": `SELECT to_char(day AT TIME ZONE 'UTC','YYYY-MM-DD') || '|' || model || '|' || prompt_version || '|' ||
		                     false_fix || '|' || total || '|' || COALESCE(rate::text,'<null>')
		                FROM v_eval_false_fix WHERE model IN ('demo-model-a','demo-model-b')
		               ORDER BY 1`,
		"cost_latency": `SELECT to_char(day AT TIME ZONE 'UTC','YYYY-MM-DD') || '|' || model || '|' || prompt_version || '|' ||
		                        avg_cost_usd::text || '|' || total_cost_usd::text || '|' ||
		                        avg_latency_ms::text || '|' || p50_latency_ms::text || '|' || p95_latency_ms::text
		                   FROM v_eval_cost_latency WHERE model IN ('demo-model-a','demo-model-b')
		                  ORDER BY 1`,
		"override": `SELECT to_char(day AT TIME ZONE 'UTC','YYYY-MM-DD') || '|' || model || '|' || prompt_version || '|' ||
		                    rejections || '|' || decisions || '|' || COALESCE(rate::text,'<null>')
		               FROM v_eval_override_rate WHERE model IN ('demo-model-a','demo-model-b')
		              ORDER BY 1`,
		"apply_failure": `SELECT to_char(day AT TIME ZONE 'UTC','YYYY-MM-DD') || '|' || model || '|' || prompt_version || '|' ||
		                         failures || '|' || attempts || '|' || COALESCE(rate::text,'<null>')
		                    FROM v_eval_apply_failure_rate WHERE model IN ('demo-model-a','demo-model-b')
		                   ORDER BY 1`,
		"escalations": `SELECT to_char(day AT TIME ZONE 'UTC','YYYY-MM-DD') || '|' || reason || '|' || count
		                  FROM v_eval_escalations ORDER BY 1`,
	}
	for name, query := range queries {
		rows, err := d.Pool.Query(ctx, query)
		if err != nil {
			t.Fatalf("snapshot %s: %v", name, err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", name, err)
			}
			out = append(out, name+":"+row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s: %v", name, err)
		}
	}
	sort.Strings(out)
	return out
}

func TestM505DemoQualityFailClosedAndReset(t *testing.T) {
	d := testdb.New(t)
	now := pinnedQualityClock()
	first := runQuality(t, d, now)
	before := qualityRowCounts(t, d)

	if _, err := demo.RunQuality(context.Background(), qualityOptions(d, now)); !errors.Is(err, demo.ErrQualityDataPresent) {
		t.Fatalf("second run error = %v, want ErrQualityDataPresent", err)
	}
	afterRefusal := qualityRowCounts(t, d)
	if !reflect.DeepEqual(before, afterRefusal) {
		t.Fatalf("refused run changed counts:\nbefore=%v\nafter=%v", before, afterRefusal)
	}

	if err := demo.CheckQualityResetAllowed(context.Background(), d.DSN); err != nil {
		t.Fatalf("reset guard on demo-only data: %v", err)
	}
	if err := demo.Reset(context.Background(), d.DSN); err != nil {
		t.Fatalf("reset before rerun: %v", err)
	}
	second := runQuality(t, d, now)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("reset+rerun summary differs:\nfirst=%+v\nsecond=%+v", first, second)
	}
	afterReset := qualityRowCounts(t, d)
	if !reflect.DeepEqual(before, afterReset) {
		t.Fatalf("reset+rerun counts differ:\nfirst=%v\nafter=%v", before, afterReset)
	}
}

func TestM505DemoQualityResetGuardRejectsUntaggedEvalRows(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO eval_runs (suite, split, dataset_version, model, prompt_version, seed, started_at)
		 VALUES ('agent', 'test', 'production-baseline', 'real-model', 'v9', 1, now())`); err != nil {
		t.Fatalf("insert untagged eval run: %v", err)
	}
	if err := demo.CheckQualityResetAllowed(ctx, d.DSN); !errors.Is(err, demo.ErrQualityUntaggedData) {
		t.Fatalf("reset guard error = %v, want ErrQualityUntaggedData", err)
	}
	if _, err := demo.RunQuality(ctx, qualityOptions(d, pinnedQualityClock())); !errors.Is(err, demo.ErrQualityDataPresent) {
		t.Fatalf("seed error = %v, want ErrQualityDataPresent", err)
	}
}

func TestM505DemoQualityLayeringAfterOps(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if _, err := demo.RunOps(ctx, demo.Options{
		DBURL: d.DSN, Seed: 1, Days: 14, Difficulty: synth.DifficultyRealistic,
	}); err != nil {
		t.Fatalf("demo ops: %v", err)
	}
	before := qualityRowCounts(t, d)
	summary := runQuality(t, d, pinnedQualityClock())
	if summary.Ops != nil {
		t.Fatal("quality run reseeded the ops layer instead of layering on it")
	}
	if !strings.Contains(summary.Layers, "already present") {
		t.Errorf("layers = %q, want an already-present layer note", summary.Layers)
	}
	after := qualityRowCounts(t, d)
	if before["quality_proposals"] != 0 || after["quality_proposals"] != 10 {
		t.Errorf("quality proposals before/after = %d/%d, want 0/10",
			before["quality_proposals"], after["quality_proposals"])
	}
	// The M1/ops rows were not duplicated by the quality run.
	var agentRuns, cases int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM agent_runs), (SELECT count(*) FROM cases)`).
		Scan(&agentRuns, &cases); err != nil {
		t.Fatalf("count agent runs/cases: %v", err)
	}
	if agentRuns != 18 {
		t.Errorf("agent runs = %d, want 8 ops + 10 quality", agentRuns)
	}
	if cases != 27 {
		t.Errorf("cases = %d, want 6 M1 + 9 ops + 12 quality", cases)
	}
}

func TestM505DemoQualityForcedOpsFailureBlocksQuality(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	// Force the ops layer's first INSERT to fail with a check violation after
	// the M1 pipeline succeeds. That proves a failed ops layer stops the
	// quality layer before any eval or quality production row is written.
	if _, err := d.Pool.Exec(ctx,
		`ALTER TABLE approvers ADD CONSTRAINT quality_forced_ops_failure CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("break ops layer: %v", err)
	}
	if _, err := demo.RunQuality(ctx, qualityOptions(d, pinnedQualityClock())); err == nil {
		t.Fatal("demo quality succeeded with a broken ops layer")
	}
	for _, query := range []string{
		`SELECT count(*) FROM eval_runs`,
		`SELECT count(*) FROM eval_case_results`,
		`SELECT count(*) FROM proposals WHERE idempotency_key LIKE 'demo-quality:%'`,
		`SELECT count(*) FROM agent_runs WHERE trace_id LIKE 'demo-quality:%'`,
	} {
		var count int64
		if err := d.Pool.QueryRow(ctx, query).Scan(&count); err != nil {
			t.Fatalf("count after failed seed (%s): %v", query, err)
		}
		if count != 0 {
			t.Errorf("failed seed left rows (%s): %d", query, count)
		}
	}
}

func TestM505DemoQualityNoNetworkOrLLM(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	// A broken proxy would fail any accidental HTTP client construction or
	// request in the seed path.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	d := testdb.New(t)
	summary := runQuality(t, d, pinnedQualityClock())
	if summary.EvalRuns != 13 {
		t.Fatalf("eval runs = %d, want 13", summary.EvalRuns)
	}
}

func TestM505DemoQualitySummaryWritesCounts(t *testing.T) {
	d := testdb.New(t)
	summary := runQuality(t, d, pinnedQualityClock())
	var b strings.Builder
	if err := summary.Write(&b); err != nil {
		t.Fatalf("write summary: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "eval_runs=13 eval_case_results=64") {
		t.Errorf("summary missing eval counts:\n%s", out)
	}
	if strings.Contains(out, "00000000-0000") {
		t.Errorf("summary leaks a UUID:\n%s", out)
	}
}
