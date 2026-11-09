package report

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RenderMarkdown renders the human report. It is derived from the same Report
// model as RenderJSON, so the two surfaces cannot drift.
func RenderMarkdown(report Report) ([]byte, error) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Eval report — run %s\n\n", mdEscape(report.Run.ID))
	writeMetadata(&builder, report)
	writeWarnings(&builder, report.Warnings)
	writeHeadline(&builder, report.Headline)
	writePartitionTable(&builder, "By failure mode", report.ByMode)
	writePartitionTable(&builder, "By difficulty", report.ByDifficulty)
	writeRepeats(&builder, report.Repeats)
	writeCalibration(&builder, report.Calibration)
	writeScorecards(&builder, report.Scorecards)
	writeAppendix(&builder, report.FailureAppendix)
	return []byte(builder.String()), nil
}

func writeMetadata(builder *strings.Builder, report Report) {
	run := report.Run
	builder.WriteString("| Field | Value |\n|---|---|\n")
	row := func(field, value string) {
		fmt.Fprintf(builder, "| %s | %s |\n", field, mdEscape(value))
	}
	row("Suite", run.Suite)
	row("Split", string(run.Split))
	row("Dataset version", run.DatasetVersion)
	row("Model", run.Model)
	row("Prompt version", run.PromptVersion)
	row("Seed", fmt.Sprintf("%d", run.Seed))
	row("Git SHA", valueOrNA(run.GitSHA))
	row("Started", formatTime(run.StartedAt))
	row("Ended", formatTimePtr(run.EndedAt))
	aborted := "no"
	if run.Aborted {
		aborted = "yes"
		if run.AbortReason != "" {
			aborted += " (" + run.AbortReason + ")"
		}
	}
	row("Aborted", aborted)
	row("Cost cap (USD)", valueOrNAPtr(run.CostCapUSD))
	row("Repeat group", valueOrNA(run.RepeatGroup))
	if run.RepeatIndex == nil {
		row("Repeat index", "n/a")
	} else {
		row("Repeat index", fmt.Sprintf("%d", *run.RepeatIndex))
	}
	if len(report.Runs) > 1 {
		parts := make([]string, 0, len(report.Runs))
		for _, member := range report.Runs {
			index := "n/a"
			if member.RepeatIndex != nil {
				index = fmt.Sprintf("%d", *member.RepeatIndex)
			}
			parts = append(parts, fmt.Sprintf("r%s=%s", index, member.ID))
		}
		row("Repeat runs", strings.Join(parts, ", "))
	}
	row("Generated at", report.GeneratedAt.Format(time.RFC3339))
	builder.WriteString("\n")
}

func writeWarnings(builder *strings.Builder, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	builder.WriteString("Warnings:\n\n")
	for _, warning := range warnings {
		fmt.Fprintf(builder, "- %s\n", mdEscape(warning))
	}
	builder.WriteString("\n")
}

func writeHeadline(builder *strings.Builder, headline MetricBlock) {
	builder.WriteString("## Headline\n\n")
	builder.WriteString("| Metric | Value | 95% CI | n | Min sample |\n")
	builder.WriteString("|---|---|---|---|---|\n")
	fmt.Fprintf(builder, "| Cases | %d | — | — | — |\n", headline.Cases)
	writeRateRow(builder, "Accuracy", headline.Accuracy)
	writeRateRow(builder, "False-fix", headline.FalseFix)
	writeRateRow(builder, "Correct escalation", headline.CorrectEscalation)
	writeRateRow(builder, "Over-escalation", headline.OverEscalation)
	writeRateRow(builder, "Evidence validity", headline.EvidenceValidity)
	writeRateRow(builder, "Injection resistance", headline.InjectionResistance)
	writeCostRow(builder, "Cost total (USD)", headline.Cost.TotalUSD, headline.Cost.Cases)
	writeCostRow(builder, "Cost mean (USD)", headline.Cost.MeanUSD, headline.Cost.Cases)
	writeLatencyRow(builder, "Latency p50 (ms)", headline.Latency.P50MS,
		headline.Latency.Observations)
	writeLatencyRow(builder, "Latency p95 (ms)", headline.Latency.P95MS,
		headline.Latency.Observations)
	writeStepsRow(builder, "Steps total", fmt.Sprintf("%d", headline.Steps.Total),
		headline.Steps.Cases)
	writeStepsRow(builder, "Steps mean", valueOrNAPtr(headline.Steps.Mean),
		headline.Steps.Cases)
	fmt.Fprintf(builder, "| Auto-resolve precision | n/a | — | — | deferred to %s |\n",
		mdEscape(headline.AutoResolvePrecision.DeferredTo))
	builder.WriteString("\n")
}

func writeRateRow(builder *strings.Builder, name string, view RateView) {
	fmt.Fprintf(builder, "| %s | %s | %s | %s | %s |\n",
		name, rateValue(view), rateCI(view), rateN(view), rateMinSample(view))
}

func writeCostRow(builder *strings.Builder, name string, value *string, cases int) {
	fmt.Fprintf(builder, "| %s | %s | — | %s | — |\n",
		name, valueOrNAPtr(value), countText(cases))
}

func writeLatencyRow(builder *strings.Builder, name string, value *int64, observations int) {
	text := "n/a"
	if value != nil {
		text = fmt.Sprintf("%d", *value)
	}
	fmt.Fprintf(builder, "| %s | %s | — | %s | — |\n", name, text, countText(observations))
}

func writeStepsRow(builder *strings.Builder, name, value string, cases int) {
	fmt.Fprintf(builder, "| %s | %s | — | %s | — |\n", name, value, countText(cases))
}

func writePartitionTable(builder *strings.Builder, title string, blocks map[string]MetricBlock) {
	fmt.Fprintf(builder, "## %s\n\n", title)
	if len(blocks) == 0 {
		builder.WriteString("No results.\n\n")
		return
	}
	builder.WriteString("Values are proportions; brackets are 95% Wilson intervals. " +
		"Repeat-group values are per-run means with sample spread.\n\n")
	builder.WriteString("| Name | Cases | Accuracy | False-fix | Correct esc. | Over esc. | " +
		"Evidence | Inject. | Min sample |\n")
	builder.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	names := make([]string, 0, len(blocks))
	for name := range blocks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		block := blocks[name]
		fmt.Fprintf(builder, "| %s | %d | %s | %s | %s | %s | %s | %s | %s |\n",
			mdEscape(name), block.Cases,
			rateWithCI(block.Accuracy),
			rateWithCI(block.FalseFix),
			rateWithCI(block.CorrectEscalation),
			rateWithCI(block.OverEscalation),
			rateWithCI(block.EvidenceValidity),
			rateWithCI(block.InjectionResistance),
			partitionMinSample(block))
	}
	builder.WriteString("\n")
}

func writeRepeats(builder *strings.Builder, repeats *Repeats) {
	builder.WriteString("## Repeats\n\n")
	if repeats == nil {
		builder.WriteString("_Single run; repeat spread not applicable._\n\n")
		return
	}
	fmt.Fprintf(builder, "Repeat group `%s`, k=%d. Values are per-run first, then "+
		"mean ± sample spread; repeat cases are never pooled into one binomial sample.\n\n",
		mdEscape(repeats.Group), repeats.K)
	builder.WriteString("| Metric | Mean | Min | Max | Stddev | Per run |\n")
	builder.WriteString("|---|---|---|---|---|---|\n")
	names := make([]string, 0, len(repeats.Metrics))
	for name := range repeats.Metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		metric := repeats.Metrics[name]
		fmt.Fprintf(builder, "| %s | %s | %s | %s | %s | %s |\n",
			mdEscape(repeatMetricLabel(name)),
			floatPtrText(metric.Mean, 4),
			floatPtrText(metric.Min, 4),
			floatPtrText(metric.Max, 4),
			floatPtrText(metric.Stddev, 4),
			perRunMetricText(metric.PerRun, 4))
	}
	if repeats.Cost != nil {
		fmt.Fprintf(builder, "| Cost mean (USD) | %s | %s | %s | %s | %s |\n",
			valueOrNAPtr(repeats.Cost.MeanUSD),
			valueOrNAPtr(repeats.Cost.MinUSD),
			valueOrNAPtr(repeats.Cost.MaxUSD),
			valueOrNAPtr(repeats.Cost.StddevUSD),
			perRunCostText(repeats.Cost.PerRun))
	}
	if repeats.K == 1 {
		builder.WriteString("\n_Single run; repeat spread is not applicable._\n")
	}
	builder.WriteString("\n")
	builder.WriteString("| Run | Repeat | Cases | Cost total (USD) | Cost mean (USD) | " +
		"p50 (ms) | p95 (ms) | Steps total | Steps mean |\n")
	builder.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, run := range repeats.PerRun {
		fmt.Fprintf(builder, "| %s | %d | %d | %s | %s | %s | %s | %d | %s |\n",
			mdEscape(run.RunID), run.RepeatIndex, run.Cases,
			valueOrNA(run.CostTotalUSD), valueOrNA(run.CostMeanUSD),
			valueOrNAInt64(run.LatencyP50MS), valueOrNAInt64(run.LatencyP95MS),
			run.StepsTotal, valueOrNA(run.StepsMean))
	}
	builder.WriteString("\n")
}

func writeCalibration(builder *strings.Builder, calibration *Calibration) {
	builder.WriteString("## Calibration\n\n")
	if calibration == nil {
		builder.WriteString("Not applicable (no calibration data).\n\n")
		return
	}
	if !calibration.Applicable {
		fmt.Fprintf(builder, "Not applicable: no proposal confidence observations (target: %s).\n\n",
			mdEscape(calibration.Target))
		return
	}
	fmt.Fprintf(builder, "Target: %s. N=%d proposal confidence observations in %d equal-width bins; "+
		"confidence 1.0 is in the last bin. ECE is weighted by bin count and is descriptive only.\n\n",
		mdEscape(calibration.Target), calibration.N, calibration.Bins)
	builder.WriteString("| Bin | Count | Mean confidence | Accuracy | Gap |\n")
	builder.WriteString("|---|---|---|---|---|\n")
	for _, bin := range calibration.Reliability {
		fmt.Fprintf(builder, "| %s | %d | %s | %s | %s |\n",
			mdEscape(bin.Bin), bin.Count,
			floatPtrText(bin.MeanConfidence, 4),
			floatPtrText(bin.Accuracy, 4),
			floatPtrText(bin.Gap, 4))
	}
	builder.WriteString("\n")
	fmt.Fprintf(builder, "Expected calibration error (ECE): %s\n\n",
		floatPtrText(calibration.ECE, 4))
	if calibration.Scope == CalibrationScopeRepeatGroup && len(calibration.PerRunECE) > 0 {
		parts := make([]string, 0, len(calibration.PerRunECE))
		for _, perRun := range calibration.PerRunECE {
			parts = append(parts, fmt.Sprintf("r%d=%s (n=%d)", perRun.RepeatIndex,
				floatPtrText(perRun.ECE, 4), perRun.N))
		}
		fmt.Fprintf(builder, "Per-run ECE: %s\n\n", strings.Join(parts, "; "))
	}
}

func writeScorecards(builder *strings.Builder, scorecards []Scorecard) {
	builder.WriteString("## Component scorecards\n\n")
	if len(scorecards) == 0 {
		builder.WriteString("No component scorecards.\n\n")
		return
	}
	for _, scorecard := range scorecards {
		if !scorecard.Applicable {
			fmt.Fprintf(builder, "- %s: not applicable", mdEscape(scorecard.Name))
			if scorecard.DeferredTo != "" {
				fmt.Fprintf(builder, " until %s", mdEscape(scorecard.DeferredTo))
			}
			if scorecard.Reason != "" {
				fmt.Fprintf(builder, " (%s)", mdEscape(scorecard.Reason))
			}
			builder.WriteString("\n")
			continue
		}
		fmt.Fprintf(builder, "- %s: %d cases\n", mdEscape(scorecard.Name), scorecard.Cases)
		for _, metric := range scorecard.Metrics {
			fmt.Fprintf(builder, "  - %s: %s %s %s\n", mdEscape(metric.Name),
				mdEscape(metric.Value), mdEscape(metric.Unit), mdEscape(metric.Note))
		}
	}
	builder.WriteString("\n")
}

func writeAppendix(builder *strings.Builder, cases []AppendixCase) {
	builder.WriteString("## Failure appendix\n\n")
	if len(cases) == 0 {
		builder.WriteString("No failures.\n")
		return
	}
	builder.WriteString("| # | Scenario | Mode | Difficulty | Outcome | Expected | Actual | " +
		"Correct | False fix | Confidence | Evidence | Error | Agent run | Proposal | " +
		"Repeats failed |\n")
	builder.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for index, entry := range cases {
		fmt.Fprintf(builder, "| %d | %s | %s | %s | %s | %s | %s | %t | %t | %s | %s | %s | %s | %s | %d/%d |\n",
			index+1,
			mdEscape(entry.ScenarioID),
			mdEscape(entry.Mode),
			mdEscape(entry.Difficulty),
			mdEscape(entry.Outcome),
			mdEscape(joinOrNA(entry.ExpectedResolutions)),
			mdEscape(valueOrNA(entry.ActualResolution)),
			entry.Correct,
			entry.FalseFix,
			floatPtrText(entry.Confidence, 4),
			evidenceText(entry),
			mdEscape(valueOrNA(entry.ErrorCode)),
			mdEscape(valueOrNA(entry.AgentRunID)),
			mdEscape(valueOrNA(entry.ProposalID)),
			entry.RepeatsFailed,
			entry.Repeats)
	}
	builder.WriteString("\n")
	for _, entry := range cases {
		writeTranscriptBlock(builder, entry)
	}
}

func writeTranscriptBlock(builder *strings.Builder, entry AppendixCase) {
	fmt.Fprintf(builder, "### %s\n\n", mdEscape(entry.ScenarioID))
	fmt.Fprintf(builder, "Locator: `%s`\n\n", mdEscape(entry.Locator))
	switch entry.TranscriptStatus {
	case transcriptStatusAvailable:
	case transcriptStatusUnsupported:
		builder.WriteString("Transcript unsupported (unknown snapshot version).\n\n")
		return
	default:
		builder.WriteString("Transcript unavailable (run predates snapshot v1).\n\n")
		return
	}
	snapshot := entry.Transcript
	if snapshot == nil {
		builder.WriteString("Transcript unavailable (run predates snapshot v1).\n\n")
		return
	}
	if snapshot.Proposal != nil {
		fmt.Fprintf(builder, "Proposal: `%s` (%s)\n\n",
			mdEscape(snapshot.Proposal.ID), mdEscape(snapshot.Proposal.Type))
	} else {
		builder.WriteString("Proposal: none\n\n")
	}
	if snapshot.Assistant != "" {
		for _, line := range strings.Split(snapshot.Assistant, "\n") {
			fmt.Fprintf(builder, "> %s\n", mdEscape(line))
		}
		builder.WriteString("\n")
	}
	if len(snapshot.ToolSteps) == 0 {
		builder.WriteString("_No tool steps recorded._\n\n")
		return
	}
	builder.WriteString("Steps:\n\n")
	for _, step := range snapshot.ToolSteps {
		refs := "—"
		if len(step.Refs) > 0 {
			refs = strings.Join(step.Refs, ", ")
		}
		errorCode := "—"
		if step.ErrorCode != "" {
			errorCode = step.ErrorCode
		}
		fmt.Fprintf(builder, "%d. `%s` refs=%s error=%s truncated=%t\n",
			step.Seq, mdEscape(step.Tool), mdEscape(refs),
			mdEscape(errorCode), step.Truncated)
	}
	if snapshot.Truncated {
		builder.WriteString("\n_Snapshot truncated by the safe bound._\n")
	}
	builder.WriteString("\n")
}

func rateValue(view RateView) string {
	if !view.Defined || view.Value == nil {
		return "n/a"
	}
	if view.Aggregation == aggregationMeanOfRepeats {
		if view.Repeat != nil && view.Repeat.Stddev != nil {
			return fmt.Sprintf("%.4f ± %.4f", *view.Value, *view.Repeat.Stddev)
		}
		return fmt.Sprintf("%.4f", *view.Value)
	}
	return fmt.Sprintf("%.4f", *view.Value)
}

func rateCI(view RateView) string {
	if !view.Defined || view.WilsonLow == nil || view.WilsonHigh == nil {
		return "—"
	}
	return fmt.Sprintf("[%.4f, %.4f]", *view.WilsonLow, *view.WilsonHigh)
}

func rateN(view RateView) string {
	if view.Denominator != nil {
		return fmt.Sprintf("%d/%d", *view.Numerator, *view.Denominator)
	}
	if view.Repeat != nil {
		return fmt.Sprintf("k=%d", view.Repeat.K)
	}
	return "—"
}

func rateMinSample(view RateView) string {
	if !view.Defined {
		return "—"
	}
	if view.MinSampleMet {
		return "ok"
	}
	if view.Denominator != nil && *view.Denominator > 0 {
		return fmt.Sprintf("⚠ n=%d < %d", *view.Denominator, view.MinSample)
	}
	return "⚠ per-run n<" + fmt.Sprintf("%d", view.MinSample)
}

func rateWithCI(view RateView) string {
	if !view.Defined || view.Value == nil {
		return "n/a"
	}
	if view.Aggregation == aggregationMeanOfRepeats {
		if view.Repeat != nil && view.Repeat.Stddev != nil {
			return fmt.Sprintf("%.4f ± %.4f", *view.Value, *view.Repeat.Stddev)
		}
		return fmt.Sprintf("%.4f", *view.Value)
	}
	ci := rateCI(view)
	if ci == "—" {
		return fmt.Sprintf("%.4f", *view.Value)
	}
	return fmt.Sprintf("%.4f %s", *view.Value, ci)
}

func partitionMinSample(block MetricBlock) string {
	smallest := 0
	any := false
	for _, view := range []RateView{
		block.Accuracy,
		block.FalseFix,
		block.CorrectEscalation,
		block.OverEscalation,
		block.EvidenceValidity,
		block.InjectionResistance,
	} {
		for _, denominator := range rateDenominators(view) {
			if denominator <= 0 {
				continue
			}
			if !any || denominator < smallest {
				smallest = denominator
				any = true
			}
		}
	}
	if !any {
		return "—"
	}
	if smallest < block.Accuracy.MinSample {
		return fmt.Sprintf("⚠ n=%d < %d", smallest, block.Accuracy.MinSample)
	}
	return "ok"
}

func rateDenominators(view RateView) []int {
	if view.Denominator != nil {
		return []int{*view.Denominator}
	}
	if view.Repeat == nil {
		return nil
	}
	out := make([]int, 0, len(view.Repeat.PerRun))
	for _, perRun := range view.Repeat.PerRun {
		out = append(out, perRun.Denominator)
	}
	return out
}

func repeatMetricLabel(name string) string {
	switch name {
	case "accuracy":
		return "Accuracy"
	case "false_fix":
		return "False-fix"
	case "correct_escalation":
		return "Correct escalation"
	case "over_escalation":
		return "Over-escalation"
	case "evidence_validity":
		return "Evidence validity"
	case "injection_resistance":
		return "Injection resistance"
	case "latency_p50_ms":
		return "Latency p50 (ms)"
	case "latency_p95_ms":
		return "Latency p95 (ms)"
	case "steps_mean":
		return "Steps mean"
	default:
		return name
	}
}

func perRunMetricText(values []RepeatMetricValue, places int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("r%d=%s", value.RepeatIndex,
			floatPtrText(value.Value, places)))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
}

func perRunCostText(values []PerRunCost) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("r%d=%s", value.RepeatIndex,
			valueOrNAPtr(value.MeanUSD)))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
}

func floatPtrText(value *float64, places int) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.*f", places, *value)
}

func evidenceText(entry AppendixCase) string {
	return fmt.Sprintf("%d/%d real, %d relevant",
		entry.EvidenceReal, entry.EvidenceTotal, entry.EvidenceRelevant)
}

func countText(value int) string {
	if value <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d", value)
}

func valueOrNA(value string) string {
	if strings.TrimSpace(value) == "" {
		return "n/a"
	}
	return value
}

func valueOrNAPtr(value *string) string {
	if value == nil {
		return "n/a"
	}
	return valueOrNA(*value)
}

func valueOrNAInt64(value *int64) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d", *value)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "n/a"
	}
	return value.UTC().Format(time.RFC3339)
}

func formatTimePtr(value *time.Time) string {
	if value == nil {
		return "n/a"
	}
	return formatTime(*value)
}

func mdEscape(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "`", "'")
	return value
}
