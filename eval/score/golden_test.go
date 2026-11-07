package score

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "update eval/score golden fixtures")

// TestGoldenCaseResults pins the full CaseResult JSON for every equivalence and
// edge fixture. Run with -update only when the scoring contract intentionally
// changes.
func TestGoldenCaseResults(t *testing.T) {
	fixtures := caseFixtures(t)
	results := scoreFixtures(t, fixtures)
	checkGolden(t, filepath.Join("testdata", "case_results.json"), results)
}

// TestGoldenReports pins the full Report JSON for every metric, including each
// undefined denominator, nearest-rank odd/even samples, and the deferred
// auto-resolve metric.
func TestGoldenReports(t *testing.T) {
	fixtures := caseFixtures(t)
	link := fixtureByName(t, fixtures, "link_correct_singular")

	reports := map[string]Report{
		"empty": Aggregate(nil, AggregateOptions{}),
		"mixed_test": reportFor(t, fixtures,
			"link_correct_singular", "link_wrong_ledger", "adjustment_correct",
			"adjustment_wrong_amount", "escalate_correct", "escalate_when_truth_link",
			"budget_exhausted", "duplicate_correct_with_original"),
		"injection_split": reportFor(t, fixtures,
			"injection_flagged_escalated", "injection_request_info",
			"injection_link_unsafe", "injection_adjustment_unsafe"),
		"zero_proposals": reportFor(t, fixtures,
			"escalate_correct", "missing_escalate_allowed"),
		"link_only": reportFor(t, fixtures,
			"link_correct_singular", "link_wrong_ledger"),
		"partial_refund": reportFor(t, fixtures,
			"partial_refund_link", "partial_refund_adjustment", "partial_refund_wrong_adjustment"),
		"test_and_injection": reportFor(t, fixtures,
			"adjustment_wrong_amount", "injection_adjustment_unsafe"),
		"cost_latency_odd": Aggregate([]CaseResult{
			resultWithMetrics(t, link, "0", "10", 1),
			resultWithMetrics(t, link, "0.01", "30", 2),
			resultWithMetrics(t, link, "0.02", "20", 3),
			resultWithMetrics(t, link, "0.03", "50", 4),
			resultWithMetrics(t, link, "0.04", "40", 10),
		}, AggregateOptions{}),
		"cost_latency_even": Aggregate([]CaseResult{
			resultWithMetrics(t, link, "0.01", "10", 0),
			resultWithMetrics(t, link, "0.01", "20", 0),
			resultWithMetrics(t, link, "0.01", "30", 2),
			resultWithMetrics(t, link, "0.01", "40", 2),
		}, AggregateOptions{}),
	}
	checkGolden(t, filepath.Join("testdata", "reports.json"), reports)
}

func checkGolden(t *testing.T, path string, value any) {
	t.Helper()
	got, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden %s: %v", path, err)
	}
	got = append(got, '\n')
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden directory: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run go test ./eval/score -run TestGolden -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden %s drifted\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}
