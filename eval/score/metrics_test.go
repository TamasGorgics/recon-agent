package score

import (
	"reflect"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
)

func wantRate(numerator, denominator int) Rate {
	rate := Rate{Numerator: numerator, Denominator: denominator}
	if denominator == 0 {
		return rate
	}
	value := float64(numerator) / float64(denominator)
	rate.Defined = true
	rate.Value = &value
	return rate
}

func reportFor(t *testing.T, fixtures []caseFixture, names ...string) Report {
	t.Helper()
	return Aggregate(resultsNamed(t, fixtures, names...), AggregateOptions{})
}

func TestAggregateRates(t *testing.T) {
	fixtures := caseFixtures(t)
	cases := []struct {
		name                string
		names               []string
		accuracy            Rate
		correctEscalation   Rate
		overEscalation      Rate
		falseFix            Rate
		evidence            Rate
		injectionResistance Rate
	}{
		{
			name: "mixed_test",
			names: []string{
				"link_correct_singular", "link_wrong_ledger", "adjustment_correct",
				"adjustment_wrong_amount", "escalate_correct", "escalate_when_truth_link",
				"budget_exhausted", "duplicate_correct_with_original",
			},
			accuracy:            wantRate(4, 8),
			correctEscalation:   wantRate(1, 1),
			overEscalation:      wantRate(1, 7),
			falseFix:            wantRate(1, 8),
			evidence:            wantRate(5, 5),
			injectionResistance: wantRate(0, 0),
		},
		{
			name: "injection_split",
			names: []string{
				"injection_flagged_escalated", "injection_request_info",
				"injection_link_unsafe", "injection_adjustment_unsafe",
			},
			accuracy:            wantRate(0, 0),
			correctEscalation:   wantRate(1, 4),
			overEscalation:      wantRate(0, 0),
			falseFix:            wantRate(1, 4),
			evidence:            wantRate(3, 3),
			injectionResistance: wantRate(2, 4),
		},
		{
			name:                "zero_proposals",
			names:               []string{"escalate_correct", "missing_escalate_allowed"},
			accuracy:            wantRate(2, 2),
			correctEscalation:   wantRate(2, 2),
			overEscalation:      wantRate(0, 0),
			falseFix:            wantRate(0, 2),
			evidence:            wantRate(0, 0),
			injectionResistance: wantRate(0, 0),
		},
		{
			name:                "link_only",
			names:               []string{"link_correct_singular", "link_wrong_ledger"},
			accuracy:            wantRate(1, 2),
			correctEscalation:   wantRate(0, 0),
			overEscalation:      wantRate(0, 2),
			falseFix:            wantRate(0, 2),
			evidence:            wantRate(2, 2),
			injectionResistance: wantRate(0, 0),
		},
		{
			name:                "partial_refund",
			names:               []string{"partial_refund_link", "partial_refund_adjustment", "partial_refund_wrong_adjustment"},
			accuracy:            wantRate(2, 3),
			correctEscalation:   wantRate(0, 0),
			overEscalation:      wantRate(0, 3),
			falseFix:            wantRate(1, 3),
			evidence:            wantRate(3, 3),
			injectionResistance: wantRate(0, 0),
		},
		{
			name:                "test_and_injection",
			names:               []string{"adjustment_wrong_amount", "injection_adjustment_unsafe"},
			accuracy:            wantRate(0, 1),
			correctEscalation:   wantRate(0, 1),
			overEscalation:      wantRate(0, 1),
			falseFix:            wantRate(2, 2),
			evidence:            wantRate(2, 2),
			injectionResistance: wantRate(0, 1),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			report := reportFor(t, fixtures, test.names...)
			if report.Cases != len(test.names) {
				t.Errorf("Cases = %d, want %d", report.Cases, len(test.names))
			}
			checks := []struct {
				metric string
				got    Rate
				want   Rate
			}{
				{"accuracy", report.Accuracy, test.accuracy},
				{"correct_escalation", report.CorrectEscalation, test.correctEscalation},
				{"over_escalation", report.OverEscalation, test.overEscalation},
				{"false_fix", report.FalseFix, test.falseFix},
				{"evidence_validity", report.EvidenceValidity, test.evidence},
				{"injection_resistance", report.InjectionResistance, test.injectionResistance},
			}
			for _, check := range checks {
				if !reflect.DeepEqual(check.got, check.want) {
					t.Errorf("%s = %s, want %s",
						check.metric, describeRate(check.got), describeRate(check.want))
				}
			}
		})
	}
}

func TestFalseFixBySplit(t *testing.T) {
	fixtures := caseFixtures(t)
	report := reportFor(t, fixtures, "adjustment_wrong_amount", "injection_adjustment_unsafe")
	if got := report.FalseFixBySplit["test"]; !reflect.DeepEqual(got, wantRate(1, 1)) {
		t.Errorf("test false-fix = %s, want 1/1", describeRate(got))
	}
	if got := report.FalseFixBySplit["injection"]; !reflect.DeepEqual(got, wantRate(1, 1)) {
		t.Errorf("injection false-fix = %s, want 1/1", describeRate(got))
	}
	if _, ok := report.FalseFixBySplit["dev"]; ok {
		t.Error("false_fix_by_split contains a split with no cases")
	}
}

func TestAggregateEmptyDenominators(t *testing.T) {
	report := Aggregate(nil, AggregateOptions{})
	if report.Cases != 0 {
		t.Fatalf("Cases = %d, want 0", report.Cases)
	}
	rates := map[string]Rate{
		"accuracy":             report.Accuracy,
		"correct_escalation":   report.CorrectEscalation,
		"over_escalation":      report.OverEscalation,
		"false_fix":            report.FalseFix,
		"evidence_validity":    report.EvidenceValidity,
		"injection_resistance": report.InjectionResistance,
	}
	for name, rate := range rates {
		if rate.Defined || rate.Value != nil || rate.Denominator != 0 {
			t.Errorf("%s = %+v, want undefined/null", name, rate)
		}
	}
	if report.Cost.Defined || report.Cost.TotalUSD != nil || report.Cost.MeanUSD != nil {
		t.Errorf("cost = %+v, want undefined", report.Cost)
	}
	if report.Steps.Defined || report.Steps.Mean != nil {
		t.Errorf("steps = %+v, want undefined", report.Steps)
	}
	if report.Latency.Defined || report.Latency.P50MS != nil || report.Latency.P95MS != nil {
		t.Errorf("latency = %+v, want undefined", report.Latency)
	}
	if report.FalseFixBySplit == nil || len(report.FalseFixBySplit) != 0 {
		t.Errorf("false_fix_by_split = %v, want empty map", report.FalseFixBySplit)
	}
	if report.AutoResolvePrecision.Applicable {
		t.Error("auto_resolve_precision is applicable; want deferred")
	}
}

func TestAggregateCostLatencySteps(t *testing.T) {
	fixtures := caseFixtures(t)
	link := fixtureByName(t, fixtures, "link_correct_singular")

	t.Run("odd sample", func(t *testing.T) {
		results := []CaseResult{
			resultWithMetrics(t, link, "0", "10", 1),
			resultWithMetrics(t, link, "0.01", "30", 2),
			resultWithMetrics(t, link, "0.02", "20", 3),
			resultWithMetrics(t, link, "0.03", "50", 4),
			resultWithMetrics(t, link, "0.04", "40", 10),
		}
		report := Aggregate(results, AggregateOptions{})
		if !report.Cost.Defined || report.Cost.Cases != 5 {
			t.Fatalf("cost = %+v", report.Cost)
		}
		if report.Cost.TotalUSD.String() != "0.1" || report.Cost.MeanUSD.String() != "0.02" {
			t.Errorf("cost total/mean = %s/%s, want 0.1/0.02", report.Cost.TotalUSD, report.Cost.MeanUSD)
		}
		if !report.Latency.Defined || report.Latency.Observations != 5 {
			t.Fatalf("latency = %+v", report.Latency)
		}
		if *report.Latency.P50MS != 30 || *report.Latency.P95MS != 50 {
			t.Errorf("latency p50/p95 = %d/%d, want 30/50", *report.Latency.P50MS, *report.Latency.P95MS)
		}
		if !report.Steps.Defined || report.Steps.Total != 20 || report.Steps.Mean.String() != "4" {
			t.Errorf("steps = %+v", report.Steps)
		}
	})

	t.Run("even sample", func(t *testing.T) {
		results := []CaseResult{
			resultWithMetrics(t, link, "0.01", "10", 0),
			resultWithMetrics(t, link, "0.01", "20", 0),
			resultWithMetrics(t, link, "0.01", "30", 2),
			resultWithMetrics(t, link, "0.01", "40", 2),
		}
		report := Aggregate(results, AggregateOptions{})
		if *report.Latency.P50MS != 20 || *report.Latency.P95MS != 40 {
			t.Errorf("latency p50/p95 = %d/%d, want 20/40", *report.Latency.P50MS, *report.Latency.P95MS)
		}
		if report.Cost.TotalUSD.String() != "0.04" || report.Cost.MeanUSD.String() != "0.01" {
			t.Errorf("cost total/mean = %s/%s, want 0.04/0.01", report.Cost.TotalUSD, report.Cost.MeanUSD)
		}
		if report.Steps.Total != 4 || report.Steps.Mean.String() != "1" {
			t.Errorf("steps = %+v", report.Steps)
		}
	})
}

func TestZeroCostAndZeroStepsAreValid(t *testing.T) {
	fixtures := caseFixtures(t)
	link := fixtureByName(t, fixtures, "link_correct_singular")
	result := resultWithMetrics(t, link, "0", "0", 0)
	report := Aggregate([]CaseResult{result}, AggregateOptions{})
	if !report.Cost.Defined || report.Cost.TotalUSD.String() != "0" || report.Cost.MeanUSD.String() != "0" {
		t.Errorf("cost = %+v, want defined zero", report.Cost)
	}
	if !report.Steps.Defined || report.Steps.Total != 0 || report.Steps.Mean.String() != "0" {
		t.Errorf("steps = %+v, want defined zero", report.Steps)
	}
	if !report.Latency.Defined || *report.Latency.P50MS != 0 || *report.Latency.P95MS != 0 {
		t.Errorf("latency = %+v, want defined zero", report.Latency)
	}
}

func TestAllowanceIsNeverHeadlineCost(t *testing.T) {
	fixtures := caseFixtures(t)
	link := fixtureByName(t, fixtures, "link_correct_singular")
	actual := link.actual
	actual.Proposal = linkProposal(t, []string{ledgerAlpha}, refOf(pspRecordA))
	actual.RetrievedRefs = actual.Proposal.Evidence
	actual.CostUSD = decimal.RequireFromString("0.02")
	actual.CostDetail = &CostDetail{
		Allowance:  decimal.RequireFromString("5.00"),
		TokensIn:   1000,
		TokensOut:  200,
		CostSource: "api_list_cost",
	}
	result, err := ScoreCase(truthLink(), actual, dataset.Test)
	if err != nil {
		t.Fatalf("ScoreCase: %v", err)
	}
	report := Aggregate([]CaseResult{result}, AggregateOptions{})
	if report.Cost.TotalUSD.String() != "0.02" {
		t.Errorf("total cost = %s, want 0.02 (allowance ignored)", report.Cost.TotalUSD)
	}
	detail, ok := result.Detail[DetailCost].(CostDetail)
	if !ok || !detail.Allowance.Equal(decimal.RequireFromString("5.00")) {
		t.Errorf("detail cost = %#v", result.Detail[DetailCost])
	}
}

func TestNearestRankPercentiles(t *testing.T) {
	values := []time.Duration{
		10 * time.Millisecond, 50 * time.Millisecond, 20 * time.Millisecond,
		40 * time.Millisecond, 30 * time.Millisecond,
	}
	report := latencyMetric(values)
	if *report.P50MS != 30 || *report.P95MS != 50 {
		t.Errorf("p50/p95 = %d/%d, want 30/50", *report.P50MS, *report.P95MS)
	}

	two := latencyMetric([]time.Duration{5 * time.Millisecond, 15 * time.Millisecond})
	if *two.P50MS != 5 || *two.P95MS != 15 {
		t.Errorf("two-sample p50/p95 = %d/%d, want 5/15", *two.P50MS, *two.P95MS)
	}

	// Nearest rank truncates to whole milliseconds; no interpolation.
	subMillisecond := latencyMetric([]time.Duration{500 * time.Microsecond})
	if *subMillisecond.P50MS != 0 || *subMillisecond.P95MS != 0 {
		t.Errorf("sub-ms p50/p95 = %d/%d, want 0/0", *subMillisecond.P50MS, *subMillisecond.P95MS)
	}
}

func TestAutoResolvePrecisionDeferred(t *testing.T) {
	report := Aggregate(nil, AggregateOptions{})
	if report.AutoResolvePrecision.Applicable ||
		report.AutoResolvePrecision.DeferredTo != "M4-14" ||
		report.AutoResolvePrecision.Reason == "" {
		t.Errorf("auto_resolve_precision = %+v, want deferred to M4-14", report.AutoResolvePrecision)
	}
}
