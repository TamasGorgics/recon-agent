package gate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RenderJSON renders the gate document with stable indentation and a trailing
// newline. generated_at is the only volatile field; the injected clock pins it
// in tests.
func RenderJSON(document Document) ([]byte, error) {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// RenderMarkdown renders the human gate decision. It is derived from the same
// Document model as RenderJSON so the two surfaces cannot drift.
func RenderMarkdown(document Document) ([]byte, error) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Eval gate — baseline %s vs candidate %s\n\n",
		mdEscape(document.Baseline.RunID), mdEscape(document.Candidate.RunID))
	builder.WriteString("Baseline is the reference; the candidate is the run under test. ")
	builder.WriteString("All deltas are `candidate − baseline`.\n\n")
	writeGateMetadata(&builder, document)
	writeGateWarnings(&builder, document.Warnings)
	writeGateResults(&builder, document.Results)
	writeGateCalibration(&builder, document.Calibration)
	writeGateDecision(&builder, document)
	return []byte(builder.String()), nil
}

func writeGateMetadata(builder *strings.Builder, document Document) {
	builder.WriteString("| Field | Baseline | Candidate |\n|---|---|---|\n")
	row := func(field, baseline, candidate string) {
		fmt.Fprintf(builder, "| %s | %s | %s |\n",
			field, mdEscape(baseline), mdEscape(candidate))
	}
	a, b := document.Baseline, document.Candidate
	row("Run ID", a.RunID, b.RunID)
	row("Suite", a.Suite, b.Suite)
	row("Split", a.Split, b.Split)
	row("Dataset version", a.DatasetVersion, b.DatasetVersion)
	row("Seed", fmt.Sprintf("%d", a.Seed), fmt.Sprintf("%d", b.Seed))
	row("Model", a.Model, b.Model)
	row("Prompt version", a.PromptVersion, b.PromptVersion)
	row("Git SHA", valueOrNA(a.GitSHA), valueOrNA(b.GitSHA))
	row("Cases", fmt.Sprintf("%d", a.Cases), fmt.Sprintf("%d", b.Cases))
	row("Complete", yesNo(a.Complete), yesNo(b.Complete))
	row("Missing scoreable", fmt.Sprintf("%d", a.MissingScoreable),
		fmt.Sprintf("%d", b.MissingScoreable))
	row("Missing modes", missingModes(a.MissingModes), missingModes(b.MissingModes))
	row("Generated at", document.GeneratedAt.Format(time.RFC3339), "")
	row("Thresholds", fmt.Sprintf("%s (schema %d, confidence %.2f, min_sample %d)",
		valueOrNA(document.Thresholds.Path), document.Thresholds.SchemaVersion,
		document.Thresholds.Confidence, document.Thresholds.MinSample), "")
	row("Threshold baseline split", document.Thresholds.BaselineSplit, "")
	builder.WriteString("\n")
}

func writeGateWarnings(builder *strings.Builder, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	builder.WriteString("Warnings:\n\n")
	for _, warning := range warnings {
		fmt.Fprintf(builder, "- %s\n", mdEscape(warning))
	}
	builder.WriteString("\n")
}

func writeGateResults(builder *strings.Builder, results []Result) {
	builder.WriteString("## Thresholds\n\n")
	builder.WriteString("| # | ID | Metric | Status | Baseline | Candidate | n (A/B) | Delta (pp) | Reason |\n")
	builder.WriteString("|---:|---|---|---|---|---|---:|---:|---|\n")
	for index, result := range results {
		fmt.Fprintf(builder, "| %d | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			index+1, mdEscape(result.ID), mdEscape(result.Metric),
			mdEscape(result.Status), rateText(result.Baseline),
			rateText(result.Candidate), countText(result.BaselineN, result.CandidateN),
			deltaText(result.DeltaPP), mdEscape(result.Reason))
	}
	builder.WriteString("\n")
	for _, result := range results {
		switch result.ID {
		case ThresholdT0FalseFix:
			if result.Status == StatusNotApplicable {
				builder.WriteString(
					"T0 false-fix: not applicable until M4-14; it is never counted as a pass.\n\n")
			}
		case ThresholdInjection:
			if result.Status == StatusNotApplicable {
				builder.WriteString(
					"Injection resistance: n/a on test; dedicated split required (M6-02/M6-05).\n\n")
			}
		}
	}
}

func writeGateCalibration(builder *strings.Builder, calibration CalibrationBlock) {
	builder.WriteString("## Calibration\n\n")
	builder.WriteString("Calibration is reported, not gated; ECE never changes the exit code.\n\n")
	builder.WriteString("| Side | Applicable | N | ECE | Populated bins |\n")
	builder.WriteString("|---|---|---:|---:|---|\n")
	side := func(name string, value CalibrationSide) {
		fmt.Fprintf(builder, "| %s | %s | %d | %s | %d/%d |\n",
			name, yesNo(value.Applicable), value.N, eceText(value.ECE),
			value.NonEmptyBins, value.Bins)
	}
	side("Baseline", calibration.Baseline)
	side("Candidate", calibration.Candidate)
	builder.WriteString("\n")
}

func writeGateDecision(builder *strings.Builder, document Document) {
	builder.WriteString("## Decision\n\n")
	fmt.Fprintf(builder, "Exit code: `%d`\n\n", document.Exit.Code)
	if document.Exit.ReportOnly {
		builder.WriteString(
			"Report-only: **yes** — the result is informational and does not certify the gate.\n\n")
	}
	if document.Exit.Incomplete {
		fmt.Fprintf(builder, "Incomplete run data: %s. The gate cannot certify an incomplete run.\n\n",
			mdEscape(strings.Join(document.Exit.IncompleteRuns, ", ")))
	}
	if len(document.Exit.FailedIDs) > 0 {
		fmt.Fprintf(builder, "Failed IDs: %s\n\n",
			mdEscape(strings.Join(document.Exit.FailedIDs, ", ")))
	}
	if len(document.Exit.NotApplicableIDs) > 0 {
		fmt.Fprintf(builder, "Not applicable IDs: %s\n\n",
			mdEscape(strings.Join(document.Exit.NotApplicableIDs, ", ")))
	}
	if len(document.Exit.Reasons) > 0 {
		builder.WriteString("Reasons:\n\n")
		for _, reason := range document.Exit.Reasons {
			fmt.Fprintf(builder, "- %s\n", mdEscape(reason))
		}
		builder.WriteString("\n")
	}
}

func rateText(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.4f", *value)
}

func eceText(value *float64) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.4f", *value)
}

func countText(baseline, candidate *int) string {
	return textOrDash(baseline) + "/" + textOrDash(candidate)
}

func textOrDash(value *int) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *value)
}

func deltaText(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%+.2f", *value)
}

func valueOrNA(value string) string {
	if strings.TrimSpace(value) == "" {
		return "n/a"
	}
	return value
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func missingModes(modes []string) string {
	if len(modes) == 0 {
		return "none"
	}
	return strings.Join(modes, ", ")
}

var mdReplacer = strings.NewReplacer(
	"|", "\\|",
	"\n", " ",
	"\r", " ",
)

func mdEscape(value string) string {
	return mdReplacer.Replace(value)
}
