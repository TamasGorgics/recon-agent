package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/score"
)

const (
	aggregationSingleRun     = "single_run"
	aggregationMeanOfRepeats = "mean_of_repeats"
)

type runReport struct {
	run          RunRow
	results      []score.CaseResult
	report       score.Report
	modes        map[string]score.Report
	difficulties map[string]score.Report
}

func newRunReport(run RunRow, results []score.CaseResult,
	truth map[string]dataset.ScenarioTruth) runReport {
	modeResults := make(map[string][]score.CaseResult)
	difficultyResults := make(map[string][]score.CaseResult)
	for _, result := range results {
		modeResults[result.Mode] = append(modeResults[result.Mode], result)
		difficulty := truth[result.ScenarioID].Difficulty
		difficultyResults[difficulty] = append(difficultyResults[difficulty], result)
	}
	modes := make(map[string]score.Report, len(modeResults))
	for mode, subset := range modeResults {
		modes[mode] = score.Aggregate(subset, score.AggregateOptions{})
	}
	difficulties := make(map[string]score.Report, len(difficultyResults))
	for difficulty, subset := range difficultyResults {
		difficulties[difficulty] = score.Aggregate(subset, score.AggregateOptions{})
	}
	return runReport{
		run:          run,
		results:      results,
		report:       score.Aggregate(results, score.AggregateOptions{}),
		modes:        modes,
		difficulties: difficulties,
	}
}

func reconstructResults(run RunRow, rows []CaseRow,
	truth map[string]dataset.ScenarioTruth) ([]score.CaseResult, error) {
	results := make([]score.CaseResult, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		scenario, ok := truth[row.ScenarioID]
		if !ok {
			return nil, fmt.Errorf(
				"eval/report: run %s has scenario %s absent from the committed truth",
				run.ID, row.ScenarioID)
		}
		if _, exists := seen[row.ScenarioID]; exists {
			return nil, fmt.Errorf("eval/report: run %s has duplicate result for scenario %s",
				run.ID, row.ScenarioID)
		}
		seen[row.ScenarioID] = struct{}{}
		if row.FailureMode != scenario.Mode {
			return nil, fmt.Errorf(
				"eval/report: run %s scenario %s failure_mode %q does not match truth mode %q",
				run.ID, row.ScenarioID, row.FailureMode, scenario.Mode)
		}
		cost, err := decimal.NewFromString(strings.TrimSpace(row.CostUSD))
		if err != nil {
			return nil, fmt.Errorf(
				"eval/report: run %s scenario %s cost_usd %q: %w",
				run.ID, row.ScenarioID, row.CostUSD, err)
		}
		if cost.IsNegative() {
			return nil, fmt.Errorf(
				"eval/report: run %s scenario %s cost_usd %s is negative",
				run.ID, row.ScenarioID, cost)
		}
		if row.LatencyMS < 0 {
			return nil, fmt.Errorf(
				"eval/report: run %s scenario %s latency_ms %d is negative",
				run.ID, row.ScenarioID, row.LatencyMS)
		}
		if row.Steps < 0 {
			return nil, fmt.Errorf(
				"eval/report: run %s scenario %s steps %d is negative",
				run.ID, row.ScenarioID, row.Steps)
		}
		detail := row.Detail
		if detail == nil {
			detail = map[string]any{}
		}
		results = append(results, score.CaseResult{
			Split:        string(run.Split),
			ScenarioID:   row.ScenarioID,
			Mode:         row.FailureMode,
			Correct:      row.Correct,
			FalseFix:     row.FalseFix,
			Escalated:    row.Escalated,
			TierExpected: cloneString(row.TierExpected),
			TierActual:   cloneString(row.TierActual),
			Confidence:   cloneDecimal(row.Confidence),
			CostUSD:      cost,
			Latency:      durationMilliseconds(row.LatencyMS),
			Steps:        row.Steps,
			Detail:       detail,
		})
	}
	return results, nil
}

func cloneDecimal(value *decimal.Decimal) *decimal.Decimal {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func buildHeadline(runData []runReport, minSample int) MetricBlock {
	if len(runData) == 1 {
		return singleMetricBlock(runData[0].report, minSample)
	}
	reports := make([]score.Report, 0, len(runData))
	runs := make([]RunRow, 0, len(runData))
	for _, data := range runData {
		reports = append(reports, data.report)
		runs = append(runs, data.run)
	}
	return repeatMetricBlock(reports, runs, minSample)
}

type partitionKind int

const (
	partitionMode partitionKind = iota
	partitionDifficulty
)

func buildPartitions(runData []runReport, minSample int, kind partitionKind) map[string]MetricBlock {
	keys := make(map[string]struct{})
	for _, data := range runData {
		source := data.modes
		if kind == partitionDifficulty {
			source = data.difficulties
		}
		for key := range source {
			keys[key] = struct{}{}
		}
	}
	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)

	out := make(map[string]MetricBlock, len(names))
	for _, name := range names {
		reports := make([]score.Report, 0, len(runData))
		runs := make([]RunRow, 0, len(runData))
		for _, data := range runData {
			source := data.modes
			if kind == partitionDifficulty {
				source = data.difficulties
			}
			reports = append(reports, source[name])
			runs = append(runs, data.run)
		}
		if len(runData) == 1 {
			out[name] = singleMetricBlock(reports[0], minSample)
		} else {
			out[name] = repeatMetricBlock(reports, runs, minSample)
		}
	}
	return out
}

func singleMetricBlock(report score.Report, minSample int) MetricBlock {
	return MetricBlock{
		Cases:                report.Cases,
		Accuracy:             singleRateView(report.Accuracy, minSample),
		FalseFix:             singleRateView(report.FalseFix, minSample),
		CorrectEscalation:    singleRateView(report.CorrectEscalation, minSample),
		OverEscalation:       singleRateView(report.OverEscalation, minSample),
		EvidenceValidity:     singleRateView(report.EvidenceValidity, minSample),
		InjectionResistance:  singleRateView(report.InjectionResistance, minSample),
		Cost:                 singleCostView(report.Cost),
		Latency:              singleLatencyView(report.Latency),
		Steps:                singleStepsView(report.Steps),
		AutoResolvePrecision: report.AutoResolvePrecision,
	}
}

func repeatMetricBlock(reports []score.Report, runs []RunRow, minSample int) MetricBlock {
	cases := 0
	for _, report := range reports {
		cases += report.Cases
	}
	return MetricBlock{
		Cases:                cases,
		Accuracy:             repeatRateView(metricAccuracy, reports, runs, minSample),
		FalseFix:             repeatRateView(metricFalseFix, reports, runs, minSample),
		CorrectEscalation:    repeatRateView(metricCorrectEscalation, reports, runs, minSample),
		OverEscalation:       repeatRateView(metricOverEscalation, reports, runs, minSample),
		EvidenceValidity:     repeatRateView(metricEvidenceValidity, reports, runs, minSample),
		InjectionResistance:  repeatRateView(metricInjectionResistance, reports, runs, minSample),
		Cost:                 repeatCostView(reports, runs),
		Latency:              repeatLatencyView(reports, runs),
		Steps:                repeatStepsView(reports, runs),
		AutoResolvePrecision: reports[0].AutoResolvePrecision,
	}
}

const (
	metricAccuracy            = "accuracy"
	metricFalseFix            = "false_fix"
	metricCorrectEscalation   = "correct_escalation"
	metricOverEscalation      = "over_escalation"
	metricEvidenceValidity    = "evidence_validity"
	metricInjectionResistance = "injection_resistance"
)

func metricRate(report score.Report, metric string) score.Rate {
	switch metric {
	case metricAccuracy:
		return report.Accuracy
	case metricFalseFix:
		return report.FalseFix
	case metricCorrectEscalation:
		return report.CorrectEscalation
	case metricOverEscalation:
		return report.OverEscalation
	case metricEvidenceValidity:
		return report.EvidenceValidity
	case metricInjectionResistance:
		return report.InjectionResistance
	default:
		return score.Rate{}
	}
}

func singleRateView(rate score.Rate, minSample int) RateView {
	numerator, denominator := rate.Numerator, rate.Denominator
	view := RateView{
		Defined:      rate.Defined,
		Aggregation:  aggregationSingleRun,
		Numerator:    &numerator,
		Denominator:  &denominator,
		MinSample:    minSample,
		MinSampleMet: denominator > 0 && denominator >= minSample,
	}
	if rate.Defined && rate.Value != nil {
		value := canonicalFloat(*rate.Value)
		interval, _ := Wilson(rate.Numerator, rate.Denominator)
		view.Value = &value
		view.WilsonLow = &interval.Low
		view.WilsonHigh = &interval.High
	}
	return view
}

func repeatRateView(metric string, reports []score.Report, runs []RunRow, minSample int) RateView {
	view := RateView{
		Aggregation: aggregationMeanOfRepeats,
		MinSample:   minSample,
		Repeat: &RepeatRate{
			K:      len(runs),
			PerRun: make([]PerRunRate, 0, len(runs)),
		},
	}
	definedAll := len(reports) > 0
	sampleMet := len(reports) > 0
	values := make([]float64, 0, len(reports))
	for index, report := range reports {
		rate := metricRate(report, metric)
		perRun := PerRunRate{
			RunID:       runs[index].ID,
			RepeatIndex: repeatIndexValue(runs[index]),
			Numerator:   rate.Numerator,
			Denominator: rate.Denominator,
		}
		if rate.Defined && rate.Value != nil {
			value := canonicalFloat(*rate.Value)
			perRun.Value = &value
			interval, _ := Wilson(rate.Numerator, rate.Denominator)
			perRun.WilsonLow = &interval.Low
			perRun.WilsonHigh = &interval.High
			values = append(values, value)
		} else {
			definedAll = false
		}
		if rate.Denominator <= 0 || rate.Denominator < minSample {
			sampleMet = false
		}
		view.Repeat.PerRun = append(view.Repeat.PerRun, perRun)
	}
	view.MinSampleMet = definedAll && sampleMet
	if definedAll {
		mean, minimum, maximum, stddev := meanSpread(values)
		view.Defined = true
		view.Value = &mean
		view.Repeat.Min = &minimum
		view.Repeat.Max = &maximum
		view.Repeat.Stddev = stddev
	}
	return view
}

func singleCostView(cost score.CostMetric) CostView {
	view := CostView{Defined: cost.Defined, Cases: cost.Cases}
	if cost.Defined && cost.TotalUSD != nil && cost.MeanUSD != nil {
		total := moneyString(*cost.TotalUSD)
		mean := moneyString(*cost.MeanUSD)
		view.TotalUSD = &total
		view.MeanUSD = &mean
	}
	return view
}

func repeatCostView(reports []score.Report, runs []RunRow) CostView {
	view := CostView{
		Cases: 0,
		Repeat: &RepeatCost{
			K:      len(runs),
			PerRun: make([]PerRunCost, 0, len(runs)),
		},
	}
	definedAll := len(reports) > 0
	means := make([]decimal.Decimal, 0, len(reports))
	totals := decimal.Zero
	totalCases := 0
	for index, report := range reports {
		perRun := PerRunCost{
			RunID:       runs[index].ID,
			RepeatIndex: repeatIndexValue(runs[index]),
			Cases:       report.Cost.Cases,
		}
		totalCases += report.Cost.Cases
		if report.Cost.Defined && report.Cost.TotalUSD != nil && report.Cost.MeanUSD != nil {
			total := moneyString(*report.Cost.TotalUSD)
			mean := moneyString(*report.Cost.MeanUSD)
			perRun.TotalUSD = &total
			perRun.MeanUSD = &mean
			means = append(means, *report.Cost.MeanUSD)
			totals = totals.Add(*report.Cost.TotalUSD)
		} else {
			definedAll = false
		}
		view.Repeat.PerRun = append(view.Repeat.PerRun, perRun)
	}
	view.Cases = totalCases
	if !definedAll || len(means) == 0 {
		return view
	}
	view.Defined = true
	totalText := moneyString(totals)
	meanValue := decimal.Avg(means[0], means[1:]...)
	meanText := moneyString(meanValue)
	view.TotalUSD = &totalText
	view.MeanUSD = &meanText
	view.Repeat.MeanUSD = &meanText

	minimum, maximum := means[0], means[0]
	sum := decimal.Zero
	for _, mean := range means {
		if mean.LessThan(minimum) {
			minimum = mean
		}
		if mean.GreaterThan(maximum) {
			maximum = mean
		}
		sum = sum.Add(mean)
	}
	minimumText := moneyString(minimum)
	maximumText := moneyString(maximum)
	view.Repeat.MinUSD = &minimumText
	view.Repeat.MaxUSD = &maximumText
	if len(means) > 1 {
		variance := decimal.Zero
		for _, mean := range means {
			delta := mean.Sub(meanValue)
			variance = variance.Add(delta.Mul(delta))
		}
		variance = variance.Div(decimal.NewFromInt(int64(len(means) - 1)))
		stddev, err := variance.SqrtRound(18)
		if err == nil {
			stddevText := moneyString(stddev)
			view.Repeat.StddevUSD = &stddevText
		}
	}
	return view
}

func singleLatencyView(latency score.LatencyMetric) LatencyView {
	view := LatencyView{
		Defined:      latency.Defined,
		Observations: latency.Observations,
	}
	if latency.Defined {
		view.P50MS = cloneInt64(latency.P50MS)
		view.P95MS = cloneInt64(latency.P95MS)
	}
	return view
}

func repeatLatencyView(reports []score.Report, runs []RunRow) LatencyView {
	view := LatencyView{
		Repeat: &RepeatLatency{
			K:     len(runs),
			P50MS: &RepeatValue{PerRun: make([]RepeatMetricValue, 0, len(runs))},
			P95MS: &RepeatValue{PerRun: make([]RepeatMetricValue, 0, len(runs))},
		},
	}
	definedAll := len(reports) > 0
	p50s := make([]float64, 0, len(reports))
	p95s := make([]float64, 0, len(reports))
	for index, report := range reports {
		view.Observations += report.Latency.Observations
		p50 := latencyValue(report.Latency.P50MS)
		p95 := latencyValue(report.Latency.P95MS)
		if !report.Latency.Defined || p50 == nil || p95 == nil {
			definedAll = false
		} else {
			p50s = append(p50s, float64(*p50))
			p95s = append(p95s, float64(*p95))
		}
		view.Repeat.P50MS.PerRun = append(view.Repeat.P50MS.PerRun, RepeatMetricValue{
			RunID:       runs[index].ID,
			RepeatIndex: repeatIndexValue(runs[index]),
			Value:       p50,
		})
		view.Repeat.P95MS.PerRun = append(view.Repeat.P95MS.PerRun, RepeatMetricValue{
			RunID:       runs[index].ID,
			RepeatIndex: repeatIndexValue(runs[index]),
			Value:       p95,
		})
	}
	if !definedAll {
		return view
	}
	view.Defined = true
	p50Mean := int64(roundFloat(meanOf(p50s)))
	p95Mean := int64(roundFloat(meanOf(p95s)))
	view.P50MS = &p50Mean
	view.P95MS = &p95Mean
	setRepeatValue(view.Repeat.P50MS, p50s)
	setRepeatValue(view.Repeat.P95MS, p95s)
	return view
}

func singleStepsView(steps score.StepsMetric) StepsView {
	view := StepsView{Defined: steps.Defined, Cases: steps.Cases, Total: steps.Total}
	if steps.Defined && steps.Mean != nil {
		mean := steps.Mean.StringFixed(2)
		view.Mean = &mean
	}
	return view
}

func repeatStepsView(reports []score.Report, runs []RunRow) StepsView {
	view := StepsView{
		Repeat: &RepeatSteps{
			K:    len(runs),
			Mean: &RepeatValue{PerRun: make([]RepeatMetricValue, 0, len(runs))},
		},
		Cases: 0,
	}
	definedAll := len(reports) > 0
	means := make([]decimal.Decimal, 0, len(reports))
	meanValues := make([]float64, 0, len(reports))
	for index, report := range reports {
		view.Cases += report.Steps.Cases
		view.Total += report.Steps.Total
		var value *float64
		if report.Steps.Defined && report.Steps.Mean != nil {
			means = append(means, *report.Steps.Mean)
			floatValue := canonicalFloat(report.Steps.Mean.InexactFloat64())
			value = &floatValue
			meanValues = append(meanValues, floatValue)
		} else {
			definedAll = false
		}
		view.Repeat.Mean.PerRun = append(view.Repeat.Mean.PerRun, RepeatMetricValue{
			RunID:       runs[index].ID,
			RepeatIndex: repeatIndexValue(runs[index]),
			Value:       value,
		})
	}
	if !definedAll || len(means) == 0 {
		return view
	}
	view.Defined = true
	groupMean := decimal.Avg(means[0], means[1:]...)
	meanText := groupMean.StringFixed(2)
	view.Mean = &meanText
	setRepeatValue(view.Repeat.Mean, meanValues)
	return view
}

type RepeatValue struct {
	Mean   *float64            `json:"mean"`
	Min    *float64            `json:"min"`
	Max    *float64            `json:"max"`
	Stddev *float64            `json:"stddev"`
	PerRun []RepeatMetricValue `json:"per_run"`
}

func setRepeatValue(target *RepeatValue, values []float64) {
	if len(values) == 0 {
		return
	}
	mean, minimum, maximum, stddev := meanSpread(values)
	target.Mean = &mean
	target.Min = &minimum
	target.Max = &maximum
	target.Stddev = stddev
}

func latencyValue(value *int64) *float64 {
	if value == nil {
		return nil
	}
	converted := float64(*value)
	return &converted
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func meanOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func roundFloat(value float64) float64 {
	if value < 0 {
		return float64(int64(value - 0.5))
	}
	return float64(int64(value + 0.5))
}

func buildRepeats(runData []runReport, minSample int) *Repeats {
	if len(runData) == 0 {
		return nil
	}
	reports := make([]score.Report, 0, len(runData))
	runs := make([]RunRow, 0, len(runData))
	for _, data := range runData {
		reports = append(reports, data.report)
		runs = append(runs, data.run)
	}
	repeats := &Repeats{
		Group:   runs[0].RepeatGroup,
		K:       len(runs),
		Metrics: make(map[string]RepeatMetric, 9),
		Cost:    repeatCostView(reports, runs).Repeat,
		PerRun:  make([]RepeatRun, 0, len(runs)),
	}
	for _, metric := range []string{
		metricAccuracy,
		metricFalseFix,
		metricCorrectEscalation,
		metricOverEscalation,
		metricEvidenceValidity,
		metricInjectionResistance,
	} {
		repeats.Metrics[metric] = repeatMetric(metric, reports, runs)
	}
	latency := repeatLatencyView(reports, runs)
	if latency.Repeat != nil {
		repeats.Metrics["latency_p50_ms"] = repeatFloatMetric(latency.Repeat.P50MS)
		repeats.Metrics["latency_p95_ms"] = repeatFloatMetric(latency.Repeat.P95MS)
	}
	steps := repeatStepsView(reports, runs)
	if steps.Repeat != nil {
		repeats.Metrics["steps_mean"] = repeatFloatMetric(steps.Repeat.Mean)
	}
	for index, data := range runData {
		perRun := RepeatRun{
			RunID:       runs[index].ID,
			RepeatIndex: repeatIndexValue(runs[index]),
			Cases:       data.report.Cases,
			StepsTotal:  data.report.Steps.Total,
		}
		if data.report.Cost.Defined && data.report.Cost.TotalUSD != nil &&
			data.report.Cost.MeanUSD != nil {
			perRun.CostTotalUSD = moneyString(*data.report.Cost.TotalUSD)
			perRun.CostMeanUSD = moneyString(*data.report.Cost.MeanUSD)
		}
		if data.report.Latency.Defined {
			perRun.LatencyP50MS = cloneInt64(data.report.Latency.P50MS)
			perRun.LatencyP95MS = cloneInt64(data.report.Latency.P95MS)
		}
		if data.report.Steps.Defined && data.report.Steps.Mean != nil {
			perRun.StepsMean = data.report.Steps.Mean.StringFixed(2)
		}
		repeats.PerRun = append(repeats.PerRun, perRun)
	}
	return repeats
}

func repeatMetric(metric string, reports []score.Report, runs []RunRow) RepeatMetric {
	perRun := make([]RepeatMetricValue, 0, len(reports))
	values := make([]float64, 0, len(reports))
	definedAll := len(reports) > 0
	for index, report := range reports {
		rate := metricRate(report, metric)
		var value *float64
		if rate.Defined && rate.Value != nil {
			copied := canonicalFloat(*rate.Value)
			value = &copied
			values = append(values, copied)
		} else {
			definedAll = false
		}
		perRun = append(perRun, RepeatMetricValue{
			RunID:       runs[index].ID,
			RepeatIndex: repeatIndexValue(runs[index]),
			Value:       value,
		})
	}
	out := RepeatMetric{PerRun: perRun}
	if definedAll {
		mean, minimum, maximum, stddev := meanSpread(values)
		out.Mean = &mean
		out.Min = &minimum
		out.Max = &maximum
		out.Stddev = stddev
	}
	return out
}

func repeatFloatMetric(source *RepeatValue) RepeatMetric {
	if source == nil {
		return RepeatMetric{PerRun: []RepeatMetricValue{}}
	}
	return RepeatMetric{
		Mean:   source.Mean,
		Min:    source.Min,
		Max:    source.Max,
		Stddev: source.Stddev,
		PerRun: source.PerRun,
	}
}

func sampleWarnings(byMode, byDifficulty map[string]MetricBlock, minSample int) []string {
	warnings := make([]string, 0, 2)
	if names := flaggedPartitions(byMode, minSample); len(names) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"minimum sample not met for failure modes: %s", strings.Join(names, ", ")))
	}
	if names := flaggedPartitions(byDifficulty, minSample); len(names) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"minimum sample not met for difficulties: %s", strings.Join(names, ", ")))
	}
	return warnings
}

func flaggedPartitions(blocks map[string]MetricBlock, minSample int) []string {
	names := make([]string, 0, len(blocks))
	for name, block := range blocks {
		if block.Cases == 0 {
			continue
		}
		flagged := false
		for _, view := range []RateView{
			block.Accuracy,
			block.FalseFix,
			block.CorrectEscalation,
			block.OverEscalation,
			block.EvidenceValidity,
			block.InjectionResistance,
		} {
			if rateBelowMinimum(view, minSample) {
				flagged = true
				break
			}
		}
		if flagged {
			names = append(names, fmt.Sprintf("%s (n=%d)", name, block.Cases))
		}
	}
	sort.Strings(names)
	return names
}

func rateBelowMinimum(view RateView, minSample int) bool {
	if view.Denominator != nil {
		return *view.Denominator > 0 && *view.Denominator < minSample
	}
	if view.Repeat == nil {
		return false
	}
	for _, perRun := range view.Repeat.PerRun {
		if perRun.Denominator > 0 && perRun.Denominator < minSample {
			return true
		}
	}
	return false
}

func durationMilliseconds(value int) time.Duration {
	return time.Duration(value) * time.Millisecond
}
