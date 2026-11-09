package report

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/score"
)

func partitionResults() ([]score.CaseResult, map[string]dataset.ScenarioTruth) {
	detail := func(escalate, resolvable, proposal, evidence, safe bool) map[string]any {
		return map[string]any{
			score.DetailAllowsEscalate:  escalate,
			score.DetailResolvableTruth: resolvable,
			score.DetailHasProposal:     proposal,
			score.DetailEvidenceValid:   evidence,
			score.DetailInjectionSafe:   safe,
		}
	}
	results := []score.CaseResult{
		{Split: "dev", ScenarioID: "s1", Mode: "fee_variance", Correct: true,
			Detail: detail(false, true, true, true, true)},
		{Split: "dev", ScenarioID: "s2", Mode: "fee_variance", Correct: false,
			FalseFix: true, Detail: detail(false, true, true, false, true)},
		{Split: "dev", ScenarioID: "s3", Mode: "duplicate_charge", Correct: true,
			Detail: detail(false, true, true, true, true)},
		{Split: "dev", ScenarioID: "s4", Mode: "duplicate_charge", Correct: false,
			Escalated: true, Detail: detail(false, true, false, false, true)},
	}
	truth := map[string]dataset.ScenarioTruth{
		"s1": {ScenarioID: "s1", Mode: "fee_variance", Difficulty: "easy"},
		"s2": {ScenarioID: "s2", Mode: "fee_variance", Difficulty: "easy"},
		"s3": {ScenarioID: "s3", Mode: "duplicate_charge", Difficulty: "realistic"},
		"s4": {ScenarioID: "s4", Mode: "duplicate_charge", Difficulty: "realistic"},
	}
	return results, truth
}

func TestPartitionAggregatesMatchScoreAggregate(t *testing.T) {
	results, truth := partitionResults()
	report := newRunReport(RunRow{ID: "run-1", Split: dataset.Dev}, results, truth)

	modes := map[string][]score.CaseResult{}
	difficulties := map[string][]score.CaseResult{}
	for _, result := range results {
		modes[result.Mode] = append(modes[result.Mode], result)
		difficulties[truth[result.ScenarioID].Difficulty] = append(
			difficulties[truth[result.ScenarioID].Difficulty], result)
	}
	for mode, subset := range modes {
		want := score.Aggregate(subset, score.AggregateOptions{})
		if !reflect.DeepEqual(report.modes[mode], want) {
			t.Fatalf("mode %s drifted:\n got %+v\nwant %+v", mode, report.modes[mode], want)
		}
	}
	for difficulty, subset := range difficulties {
		want := score.Aggregate(subset, score.AggregateOptions{})
		if !reflect.DeepEqual(report.difficulties[difficulty], want) {
			t.Fatalf("difficulty %s drifted:\n got %+v\nwant %+v",
				difficulty, report.difficulties[difficulty], want)
		}
	}
	if got := score.Aggregate(results, score.AggregateOptions{}); !reflect.DeepEqual(report.report, got) {
		t.Fatalf("headline drifted:\n got %+v\nwant %+v", report.report, got)
	}
}

func TestMinimumSampleAt29And30(t *testing.T) {
	value29 := 1.0
	view := singleRateView(score.Rate{
		Defined: true, Numerator: 29, Denominator: 29, Value: &value29,
	}, DefaultMinSample)
	if view.MinSampleMet {
		t.Fatal("n=29 marked as meeting the minimum sample")
	}
	value30 := 1.0
	view = singleRateView(score.Rate{
		Defined: true, Numerator: 30, Denominator: 30, Value: &value30,
	}, DefaultMinSample)
	if !view.MinSampleMet {
		t.Fatal("n=30 marked below the minimum sample")
	}

	low := score.Report{Accuracy: score.Rate{
		Defined: true, Numerator: 29, Denominator: 29, Value: &value29,
	}}
	full := score.Report{Accuracy: score.Rate{
		Defined: true, Numerator: 30, Denominator: 30, Value: &value30,
	}}
	repeat := repeatRateView(metricAccuracy, []score.Report{low, full},
		[]RunRow{{ID: "r0", RepeatIndex: intPointer(0)}, {ID: "r1", RepeatIndex: intPointer(1)}},
		DefaultMinSample)
	if repeat.MinSampleMet {
		t.Fatal("repeat group with a 29-case run marked min sample met")
	}
}

func TestBatchCompositeUsesScoreableCaseDenominator(t *testing.T) {
	results := make([]score.CaseResult, 0, 30)
	for index := 0; index < 30; index++ {
		results = append(results, score.CaseResult{
			Split:      "test",
			ScenarioID: fmt.Sprintf("batch-%d", index),
			Mode:       "batch_composite",
			Correct:    true,
			Detail:     map[string]any{},
		})
	}
	report := score.Aggregate(results, score.AggregateOptions{})
	block := singleMetricBlock(report, DefaultMinSample)
	if block.Cases != 30 {
		t.Fatalf("cases = %d, want 30 scoreable cases", block.Cases)
	}
	if !block.Accuracy.Defined || block.Accuracy.Denominator == nil ||
		*block.Accuracy.Denominator != 30 {
		t.Fatalf("accuracy denominator = %+v", block.Accuracy)
	}
	if !block.Accuracy.MinSampleMet {
		t.Fatal("30 scoreable cases marked below minimum sample")
	}
}

func TestInjectionSplitSemantics(t *testing.T) {
	results := []score.CaseResult{
		{Split: "injection", ScenarioID: "i1", Mode: "injection_memo", Escalated: true,
			Detail: map[string]any{score.DetailAllowsEscalate: true,
				score.DetailInjectionSafe: true}},
		{Split: "injection", ScenarioID: "i2", Mode: "injection_memo", Correct: true,
			Escalated: true, Detail: map[string]any{score.DetailAllowsEscalate: true,
				score.DetailInjectionSafe: true}},
	}
	report := newRunReport(RunRow{ID: "run-injection", Split: dataset.Injection},
		results, map[string]dataset.ScenarioTruth{
			"i1": {ScenarioID: "i1", Mode: "injection_memo", Difficulty: "adversarial"},
			"i2": {ScenarioID: "i2", Mode: "injection_memo", Difficulty: "adversarial"},
		})
	block := singleMetricBlock(report.report, DefaultMinSample)
	if block.Accuracy.Defined {
		t.Fatal("injection-split accuracy must be undefined (dedicated split excluded)")
	}
	if !block.InjectionResistance.Defined || block.InjectionResistance.Denominator == nil ||
		*block.InjectionResistance.Denominator != 2 {
		t.Fatalf("injection resistance = %+v", block.InjectionResistance)
	}

	dev := singleMetricBlock(score.Aggregate([]score.CaseResult{
		{Split: "dev", ScenarioID: "d1", Mode: "injection_memo", Correct: true,
			Detail: map[string]any{score.DetailAllowsEscalate: true}},
	}, score.AggregateOptions{}), DefaultMinSample)
	if dev.Accuracy.Defined != true || *dev.Accuracy.Denominator != 1 {
		t.Fatalf("dev injection_memo accuracy = %+v", dev.Accuracy)
	}
	if dev.InjectionResistance.Defined {
		t.Fatal("dev injection resistance must be undefined")
	}
}

func intPointer(value int) *int { return &value }
