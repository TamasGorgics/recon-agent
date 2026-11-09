package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// CompareSchema is the JSON schema of the run comparison document.
const CompareSchema = "compare/v1"

// Classification values used by compare/v1. Undefined rates are
// ClassificationNotApplicable and are never regressions or improvements.
const (
	ClassificationRegression    = "regression"
	ClassificationImprovement   = "improvement"
	ClassificationNoChange      = "no_change"
	ClassificationNotApplicable = "n/a"
)

// ErrCompareMismatch marks a metadata mismatch between the two compared runs
// (suite, split, or dataset version). The CLI maps it to exit code 2; load,
// validation, database, render and write failures stay exit code 1.
var ErrCompareMismatch = errors.New("eval/report: comparison metadata mismatch")

// CompareOptions controls comparison construction. The clock is injected so
// generated_at is deterministic in goldens.
type CompareOptions struct {
	// DatasetDir is the committed split directory. It defaults to
	// <module root>/eval/datasets/<split>.
	DatasetDir string
	// ModuleRoot is the module root used for the default dataset directory.
	ModuleRoot string
	// Clock supplies generated_at. nil means clock.Real.
	Clock clock.Clock
	// MinSample is the minimum per-rate denominator passed to report
	// construction. <= 0 means DefaultMinSample.
	MinSample int
	// ReportOnly marks a gating regression as informational for the footer.
	// It changes only the CLI exit code, never the comparison numbers.
	ReportOnly bool
}

// Comparison is the compare/v1 document. Markdown and JSON render from this
// single model so numbers and flags cannot drift.
type Comparison struct {
	Schema       string                  `json:"schema"`
	GeneratedAt  time.Time               `json:"generated_at"`
	A            CompareRun              `json:"a"`
	B            CompareRun              `json:"b"`
	Overall      CompareBlock            `json:"overall"`
	ByMode       map[string]CompareBlock `json:"by_mode"`
	Cost         CompareCost             `json:"cost"`
	Latency      CompareLatency          `json:"latency"`
	Steps        CompareSteps            `json:"steps"`
	ExitCriteria CompareExitCriteria     `json:"exit_criteria"`
	Warnings     []string                `json:"warnings"`
}

// CompareRun is one side's whitelisted run metadata and case counts.
type CompareRun struct {
	RunID                  string        `json:"run_id"`
	Suite                  string        `json:"suite"`
	Split                  dataset.Split `json:"split"`
	DatasetVersion         string        `json:"dataset_version"`
	Model                  string        `json:"model"`
	PromptVersion          string        `json:"prompt_version"`
	Seed                   int64         `json:"seed"`
	GitSHA                 string        `json:"git_sha"`
	RepeatGroup            string        `json:"repeat_group"`
	RepeatIndex            *int          `json:"repeat_index"`
	Cases                  int           `json:"cases"`
	Proposals              int           `json:"proposals"`
	ConfidenceObservations int           `json:"confidence_observations"`
	// Complete is false when the run record is partial/aborted, has no case
	// results, or does not cover every scoreable scenario in the committed
	// dataset. MissingScoreable/MissingModes explain an incomplete side.
	Complete         bool     `json:"complete"`
	Aborted          bool     `json:"aborted"`
	AbortReason      string   `json:"abort_reason"`
	Partial          bool     `json:"partial"`
	MissingScoreable int      `json:"missing_scoreable"`
	MissingModes     []string `json:"missing_modes"`
}

// CompareBlock is the overall or one per-mode comparison block. Rates are
// proportions; delta_pp is in percentage points (B − A).
type CompareBlock struct {
	Cases               CompareCounts `json:"cases"`
	Accuracy            CompareRate   `json:"accuracy"`
	FalseFix            CompareRate   `json:"false_fix"`
	CorrectEscalation   CompareRate   `json:"correct_escalation"`
	OverEscalation      CompareRate   `json:"over_escalation"`
	EvidenceValidity    CompareRate   `json:"evidence_validity"`
	InjectionResistance CompareRate   `json:"injection_resistance"`
}

// CompareCounts is a two-sided case count.
type CompareCounts struct {
	A int `json:"a"`
	B int `json:"b"`
}

// CompareRate is one rate comparison. a/a_ci and b/b_ci are null when the
// side's denominator is zero; overlap and delta_pp are null when either side
// is undefined. classification is "n/a" for any undefined side.
type CompareRate struct {
	A              *float64    `json:"a"`
	B              *float64    `json:"b"`
	AN             *int        `json:"a_n"`
	BN             *int        `json:"b_n"`
	DeltaPP        *float64    `json:"delta_pp"`
	ACI            *[2]float64 `json:"a_ci"`
	BCI            *[2]float64 `json:"b_ci"`
	Overlap        *bool       `json:"overlap"`
	Direction      string      `json:"direction"`
	Gating         bool        `json:"gating"`
	Classification string      `json:"classification"`
}

// CompareCost reports mean cost in decimal strings. delta_pct is null when
// the baseline mean is not positive.
type CompareCost struct {
	ACases   int      `json:"a_cases"`
	BCases   int      `json:"b_cases"`
	AMeanUSD *string  `json:"a_mean_usd"`
	BMeanUSD *string  `json:"b_mean_usd"`
	DeltaUSD *string  `json:"delta_usd"`
	DeltaPct *float64 `json:"delta_pct"`
}

// CompareLatency reports nearest-rank p50/p95 deltas in whole milliseconds.
type CompareLatency struct {
	AP50MS      *int64   `json:"a_p50_ms"`
	BP50MS      *int64   `json:"b_p50_ms"`
	P50DeltaMS  *int64   `json:"p50_delta_ms"`
	P50DeltaPct *float64 `json:"p50_delta_pct"`
	AP95MS      *int64   `json:"a_p95_ms"`
	BP95MS      *int64   `json:"b_p95_ms"`
	P95DeltaMS  *int64   `json:"p95_delta_ms"`
	P95DeltaPct *float64 `json:"p95_delta_pct"`
}

// CompareSteps reports the steps-mean delta as decimal text with two
// fractional digits.
type CompareSteps struct {
	AMean      *string `json:"a_mean"`
	BMean      *string `json:"b_mean"`
	DeltaSteps *string `json:"delta_steps"`
}

// CompareExitCriteria records whether a gating regression was found and
// whether either run was incomplete. Failed and Incomplete stay true under
// --report-only; the CLI suppresses their exit codes instead. Incomplete
// dominates Failed: an incomplete comparison exits 2 (unless --report-only)
// because its gating decision cannot be trusted.
type CompareExitCriteria struct {
	Failed         bool                `json:"failed"`
	ReportOnly     bool                `json:"report_only"`
	Incomplete     bool                `json:"incomplete"`
	IncompleteRuns []string            `json:"incomplete_runs"`
	Regressions    []CompareRegression `json:"regressions"`
}

// CompareRegression is one gating regression. Scope is "overall" or "mode";
// Mode is only set for a per-mode regression.
type CompareRegression struct {
	Scope   string   `json:"scope"`
	Mode    string   `json:"mode,omitempty"`
	Metric  string   `json:"metric"`
	A       *float64 `json:"a"`
	B       *float64 `json:"b"`
	DeltaPP *float64 `json:"delta_pp"`
}

type compareMetricSpec struct {
	Name           string
	Display        string
	HigherIsBetter bool
	Gating         bool
}

var compareMetricSpecs = []compareMetricSpec{
	{Name: metricAccuracy, Display: "Accuracy", HigherIsBetter: true, Gating: true},
	{Name: metricFalseFix, Display: "False-fix", HigherIsBetter: false, Gating: true},
	{Name: metricCorrectEscalation, Display: "Correct escalation", HigherIsBetter: true, Gating: true},
	{Name: metricOverEscalation, Display: "Over-escalation", HigherIsBetter: false, Gating: false},
	{Name: metricEvidenceValidity, Display: "Evidence validity", HigherIsBetter: true, Gating: true},
	{Name: metricInjectionResistance, Display: "Injection resistance", HigherIsBetter: true, Gating: false},
}

// compareGatingOrder is the fixed order used for exit_criteria.regressions:
// false-fix increase, accuracy decrease, evidence-validity decrease, and
// correct-escalation decrease. Injection resistance and over-escalation are
// classified but non-gating in M3-07.
var compareGatingOrder = []string{
	metricFalseFix,
	metricAccuracy,
	metricEvidenceValidity,
	metricCorrectEscalation,
}

// Compare loads two scored runs, builds each named run's report model (the
// report-level repeat aggregation is intentionally not used), and compares the
// baseline A with the candidate B. All deltas are B − A.
//
// The two runs must share suite, split, and dataset_version; a mismatch wraps
// ErrCompareMismatch. Model and prompt version may differ. When either run is a
// repeat member, only that named run is compared and a warning says so.
//
// A run that is partial, aborted, has no case results, or omits scoreable
// scenarios makes the comparison incomplete: Build's completeness warnings are
// copied into Warnings, CompareRun records the missing counts, and
// exit_criteria.incomplete is set so the CLI can fail closed (exit 2 unless
// --report-only). The per-rate Wilson/Gating logic is unchanged.
func Compare(ctx context.Context, src Source, runA, runB string, opts CompareOptions) (Comparison, error) {
	if src == nil {
		return Comparison{}, errors.New("eval/report: source is required")
	}
	runA = normalizeRunID(runA)
	runB = normalizeRunID(runB)
	if runA == "" {
		return Comparison{}, errors.New("eval/report: baseline run id is required")
	}
	if runB == "" {
		return Comparison{}, errors.New("eval/report: candidate run id is required")
	}
	opts = normalizeCompareOptions(opts)

	rowA, err := src.LoadRun(ctx, runA)
	if err != nil {
		return Comparison{}, fmt.Errorf("eval/report: load baseline run %s: %w", runA, err)
	}
	if rowA.ID == "" {
		rowA.ID = runA
	}
	rowB, err := src.LoadRun(ctx, runB)
	if err != nil {
		return Comparison{}, fmt.Errorf("eval/report: load candidate run %s: %w", runB, err)
	}
	if rowB.ID == "" {
		rowB.ID = runB
	}
	if err := validateCompareMetadata(rowA, rowB); err != nil {
		return Comparison{}, err
	}

	reportA, err := buildNamedRun(ctx, src, rowA, opts)
	if err != nil {
		return Comparison{}, err
	}
	reportB, err := buildNamedRun(ctx, src, rowB, opts)
	if err != nil {
		return Comparison{}, err
	}

	overall := compareBlock(reportA.Headline, reportB.Headline)
	byMode := compareModes(reportA.ByMode, reportB.ByMode)
	regressions := collectRegressions(overall, byMode)
	incompleteRuns := compareIncompleteRuns(rowA, reportA, rowB, reportB)

	comparison := Comparison{
		Schema:      CompareSchema,
		GeneratedAt: opts.Clock.Now().UTC(),
		A:           compareRunMetadata(rowA, reportA),
		B:           compareRunMetadata(rowB, reportB),
		Overall:     overall,
		ByMode:      byMode,
		Cost:        compareCost(reportA.Headline.Cost, reportB.Headline.Cost),
		Latency:     compareLatency(reportA.Headline.Latency, reportB.Headline.Latency),
		Steps:       compareSteps(reportA.Headline.Steps, reportB.Headline.Steps),
		ExitCriteria: CompareExitCriteria{
			Failed:         len(regressions) > 0,
			ReportOnly:     opts.ReportOnly,
			Incomplete:     len(incompleteRuns) > 0,
			IncompleteRuns: incompleteRuns,
			Regressions:    regressions,
		},
		Warnings: compareWarnings(rowA, rowB, reportA, reportB),
	}
	return comparison, nil
}

func normalizeRunID(runID string) string {
	return strings.ToLower(strings.TrimSpace(runID))
}

func normalizeCompareOptions(opts CompareOptions) CompareOptions {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	return opts
}

func validateCompareMetadata(a, b RunRow) error {
	mismatches := make([]string, 0, 3)
	if a.Suite != b.Suite {
		mismatches = append(mismatches, fmt.Sprintf("suite %q != %q", a.Suite, b.Suite))
	}
	if a.Split != b.Split {
		mismatches = append(mismatches, fmt.Sprintf("split %q != %q", a.Split, b.Split))
	}
	if a.DatasetVersion != b.DatasetVersion {
		mismatches = append(mismatches, fmt.Sprintf("dataset_version %q != %q",
			a.DatasetVersion, b.DatasetVersion))
	}
	if len(mismatches) == 0 {
		return nil
	}
	return fmt.Errorf("%w: run %s vs run %s: %s", ErrCompareMismatch, a.ID, b.ID,
		strings.Join(mismatches, "; "))
}

// singleRunSource makes Build see exactly one named run, even when that run is
// a repeat-group member. Repeat-group aggregation is M3-05 report behavior, not
// run-to-run comparison behavior.
type singleRunSource struct {
	src Source
	run RunRow
}

func (s singleRunSource) LoadRun(ctx context.Context, runID string) (RunRow, error) {
	if strings.EqualFold(strings.TrimSpace(runID), s.run.ID) {
		return s.run, nil
	}
	return s.src.LoadRun(ctx, runID)
}

func (s singleRunSource) LoadRepeatGroup(ctx context.Context, groupID string) ([]RunRow, error) {
	return s.src.LoadRepeatGroup(ctx, groupID)
}

func (s singleRunSource) LoadCaseResults(ctx context.Context, runIDs []string) ([]CaseRow, error) {
	return s.src.LoadCaseResults(ctx, runIDs)
}

func buildNamedRun(ctx context.Context, src Source, row RunRow, opts CompareOptions) (Report, error) {
	named := row
	named.RepeatGroup = ""
	named.RepeatIndex = nil
	built, err := Build(ctx, singleRunSource{src: src, run: named}, named.ID, Options{
		DatasetDir: opts.DatasetDir,
		ModuleRoot: opts.ModuleRoot,
		Clock:      opts.Clock,
		MinSample:  opts.MinSample,
	})
	if err != nil {
		return Report{}, fmt.Errorf("eval/report: compare run %s: %w", row.ID, err)
	}
	return built, nil
}

// compareIncompleteRuns names each side that did not cover the committed
// dataset (missing scoreable scenarios, zero cases, partial ended_at, or
// aborted). The returned slice is never nil so JSON stays [] for complete
// comparisons. A run compared with itself is listed once.
func compareIncompleteRuns(rowA RunRow, reportA Report, rowB RunRow, reportB Report) []string {
	runs := make([]string, 0, 2)
	add := func(row RunRow, built Report) {
		if built.completeness[row.ID].complete() {
			return
		}
		if len(runs) == 1 && runs[0] == row.ID {
			return
		}
		runs = append(runs, row.ID)
	}
	add(rowA, reportA)
	add(rowB, reportB)
	return runs
}

func compareRunMetadata(row RunRow, built Report) CompareRun {
	proposals := 0
	if built.Headline.EvidenceValidity.Denominator != nil {
		proposals = *built.Headline.EvidenceValidity.Denominator
	}
	confidence := 0
	if built.Calibration != nil {
		confidence = built.Calibration.N
	}
	completeness := built.completeness[row.ID]
	return CompareRun{
		RunID:                  row.ID,
		Suite:                  row.Suite,
		Split:                  row.Split,
		DatasetVersion:         row.DatasetVersion,
		Model:                  row.Model,
		PromptVersion:          row.PromptVersion,
		Seed:                   row.Seed,
		GitSHA:                 row.GitSHA,
		RepeatGroup:            row.RepeatGroup,
		RepeatIndex:            cloneInt(row.RepeatIndex),
		Cases:                  built.Headline.Cases,
		Proposals:              proposals,
		ConfidenceObservations: confidence,
		Complete:               completeness.complete(),
		Aborted:                completeness.Aborted,
		AbortReason:            completeness.AbortReason,
		Partial:                completeness.Partial,
		MissingScoreable:       completeness.MissingScoreable,
		MissingModes:           cloneStrings(completeness.MissingModes),
	}
}

func compareBlock(a, b MetricBlock) CompareBlock {
	return CompareBlock{
		Cases:               CompareCounts{A: a.Cases, B: b.Cases},
		Accuracy:            compareRate(metricAccuracy, a.Accuracy, b.Accuracy),
		FalseFix:            compareRate(metricFalseFix, a.FalseFix, b.FalseFix),
		CorrectEscalation:   compareRate(metricCorrectEscalation, a.CorrectEscalation, b.CorrectEscalation),
		OverEscalation:      compareRate(metricOverEscalation, a.OverEscalation, b.OverEscalation),
		EvidenceValidity:    compareRate(metricEvidenceValidity, a.EvidenceValidity, b.EvidenceValidity),
		InjectionResistance: compareRate(metricInjectionResistance, a.InjectionResistance, b.InjectionResistance),
	}
}

func compareModes(a, b map[string]MetricBlock) map[string]CompareBlock {
	names := make(map[string]struct{}, len(a)+len(b))
	for name := range a {
		names[name] = struct{}{}
	}
	for name := range b {
		names[name] = struct{}{}
	}
	out := make(map[string]CompareBlock, len(names))
	for name := range names {
		out[name] = compareBlock(a[name], b[name])
	}
	return out
}

func compareRate(metric string, a, b RateView) CompareRate {
	spec := compareSpec(metric)
	out := CompareRate{
		Direction:      directionLabel(spec.HigherIsBetter),
		Gating:         spec.Gating,
		Classification: ClassificationNotApplicable,
	}
	if a.Defined && a.Value != nil {
		value := *a.Value
		out.A = &value
	}
	if b.Defined && b.Value != nil {
		value := *b.Value
		out.B = &value
	}
	if a.Denominator != nil {
		denominator := *a.Denominator
		out.AN = &denominator
	}
	if b.Denominator != nil {
		denominator := *b.Denominator
		out.BN = &denominator
	}
	if a.WilsonLow != nil && a.WilsonHigh != nil {
		out.ACI = &[2]float64{*a.WilsonLow, *a.WilsonHigh}
	}
	if b.WilsonLow != nil && b.WilsonHigh != nil {
		out.BCI = &[2]float64{*b.WilsonLow, *b.WilsonHigh}
	}
	if out.A == nil || out.B == nil {
		return out
	}
	delta := roundCompareFloat((*out.B - *out.A) * 100)
	out.DeltaPP = &delta
	if out.ACI == nil || out.BCI == nil {
		return out
	}
	overlap := !(out.BCI[1] < out.ACI[0] || out.BCI[0] > out.ACI[1])
	out.Overlap = &overlap
	if overlap {
		out.Classification = ClassificationNoChange
		return out
	}
	if spec.HigherIsBetter {
		if out.BCI[1] < out.ACI[0] {
			out.Classification = ClassificationRegression
		} else {
			out.Classification = ClassificationImprovement
		}
		return out
	}
	if out.BCI[0] > out.ACI[1] {
		out.Classification = ClassificationRegression
	} else {
		out.Classification = ClassificationImprovement
	}
	return out
}

func compareSpec(metric string) compareMetricSpec {
	for _, spec := range compareMetricSpecs {
		if spec.Name == metric {
			return spec
		}
	}
	return compareMetricSpec{Name: metric, Display: metric}
}

func directionLabel(higherIsBetter bool) string {
	if higherIsBetter {
		return "higher_is_better"
	}
	return "lower_is_better"
}

func blockRate(block CompareBlock, metric string) CompareRate {
	switch metric {
	case metricAccuracy:
		return block.Accuracy
	case metricFalseFix:
		return block.FalseFix
	case metricCorrectEscalation:
		return block.CorrectEscalation
	case metricOverEscalation:
		return block.OverEscalation
	case metricEvidenceValidity:
		return block.EvidenceValidity
	case metricInjectionResistance:
		return block.InjectionResistance
	default:
		return CompareRate{}
	}
}

func collectRegressions(overall CompareBlock, byMode map[string]CompareBlock) []CompareRegression {
	regressions := make([]CompareRegression, 0, 4)
	for _, metric := range compareGatingOrder {
		rate := blockRate(overall, metric)
		if rate.Classification != ClassificationRegression {
			continue
		}
		regressions = append(regressions, CompareRegression{
			Scope:   "overall",
			Metric:  metric,
			A:       cloneFloat(rate.A),
			B:       cloneFloat(rate.B),
			DeltaPP: cloneFloat(rate.DeltaPP),
		})
	}
	modes := make([]string, 0, len(byMode))
	for mode := range byMode {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	for _, mode := range modes {
		for _, metric := range compareGatingOrder {
			rate := blockRate(byMode[mode], metric)
			if rate.Classification != ClassificationRegression {
				continue
			}
			regressions = append(regressions, CompareRegression{
				Scope:   "mode",
				Mode:    mode,
				Metric:  metric,
				A:       cloneFloat(rate.A),
				B:       cloneFloat(rate.B),
				DeltaPP: cloneFloat(rate.DeltaPP),
			})
		}
	}
	return regressions
}

func compareCost(a, b CostView) CompareCost {
	out := CompareCost{
		ACases:   a.Cases,
		BCases:   b.Cases,
		AMeanUSD: cloneString(a.MeanUSD),
		BMeanUSD: cloneString(b.MeanUSD),
	}
	if a.MeanUSD == nil || b.MeanUSD == nil {
		return out
	}
	aMean := decimal.RequireFromString(*a.MeanUSD)
	bMean := decimal.RequireFromString(*b.MeanUSD)
	delta := bMean.Sub(aMean)
	deltaText := moneyString(delta)
	out.DeltaUSD = &deltaText
	if aMean.IsPositive() {
		percent := roundCompareFloat(delta.Div(aMean).Mul(decimal.NewFromInt(100)).InexactFloat64())
		out.DeltaPct = &percent
	}
	return out
}

func compareLatency(a, b LatencyView) CompareLatency {
	out := CompareLatency{
		AP50MS: cloneInt64(a.P50MS),
		BP50MS: cloneInt64(b.P50MS),
		AP95MS: cloneInt64(a.P95MS),
		BP95MS: cloneInt64(b.P95MS),
	}
	out.P50DeltaMS, out.P50DeltaPct = compareInt64Delta(a.P50MS, b.P50MS)
	out.P95DeltaMS, out.P95DeltaPct = compareInt64Delta(a.P95MS, b.P95MS)
	return out
}

func compareInt64Delta(a, b *int64) (*int64, *float64) {
	if a == nil || b == nil {
		return nil, nil
	}
	delta := *b - *a
	if *a <= 0 {
		return &delta, nil
	}
	percent := roundCompareFloat(float64(delta) / float64(*a) * 100)
	return &delta, &percent
}

func compareSteps(a, b StepsView) CompareSteps {
	out := CompareSteps{
		AMean: cloneString(a.Mean),
		BMean: cloneString(b.Mean),
	}
	if a.Mean == nil || b.Mean == nil {
		return out
	}
	delta := decimal.RequireFromString(*b.Mean).Sub(decimal.RequireFromString(*a.Mean))
	deltaText := delta.StringFixed(2)
	out.DeltaSteps = &deltaText
	return out
}

func compareWarnings(a, b RunRow, reportA, reportB Report) []string {
	warnings := make([]string, 0, 8)
	seen := make(map[string]struct{})
	add := func(warning string) {
		if warning == "" {
			return
		}
		if _, exists := seen[warning]; exists {
			return
		}
		seen[warning] = struct{}{}
		warnings = append(warnings, warning)
	}
	if a.ID == b.ID {
		add("comparing a run to itself")
	}
	addRunWarnings := func(run RunRow, built Report) {
		if run.RepeatGroup != "" {
			index := "n/a"
			if run.RepeatIndex != nil {
				index = fmt.Sprintf("%d", *run.RepeatIndex)
			}
			add(fmt.Sprintf(
				"run %s is a repeat member of group %s (repeat_index %s); comparing that run only (repeat-group comparison is deferred)",
				run.ID, run.RepeatGroup, index))
		}
		completeness, ok := built.completeness[run.ID]
		if ok {
			// Copy Build's completeness warnings verbatim: missing scoreable
			// scenarios, partial/aborted state, zero cases, and missing modes.
			for _, warning := range completeness.Warnings {
				add(warning)
			}
			return
		}
		// Defensive fallback for a Report not produced by Build in this process.
		if run.EndedAt == nil {
			add(fmt.Sprintf("run %s is partial: ended_at is null", run.ID))
		}
		if run.Aborted {
			reason := strings.TrimSpace(run.AbortReason)
			if reason == "" {
				reason = "unspecified"
			}
			add(fmt.Sprintf("run %s aborted (%s)", run.ID, reason))
		}
		if built.Headline.Cases == 0 {
			add(fmt.Sprintf("run %s has no case results", run.ID))
		}
	}
	addRunWarnings(a, reportA)
	addRunWarnings(b, reportB)
	return warnings
}

func roundCompareFloat(value float64) float64 {
	return math.Round(value*1e6) / 1e6
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

// RenderCompareJSON renders the compare/v1 document with stable indentation
// and a trailing newline. generated_at is the only volatile field.
func RenderCompareJSON(comparison Comparison) ([]byte, error) {
	data, err := json.MarshalIndent(comparison, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// RenderCompareMarkdown renders the human comparison. It is derived from the
// same Comparison model as RenderCompareJSON, so the two surfaces cannot drift.
func RenderCompareMarkdown(comparison Comparison) ([]byte, error) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Eval run comparison — %s vs %s\n\n",
		mdEscape(comparison.A.RunID), mdEscape(comparison.B.RunID))
	builder.WriteString("A is the baseline; B is the candidate. All deltas are `B − A`.\n\n")
	writeCompareMetadata(&builder, comparison)
	writeCompareWarnings(&builder, comparison.Warnings)
	writeCompareOverall(&builder, comparison.Overall)
	writeCompareByMode(&builder, comparison.ByMode)
	writeCompareDescriptive(&builder, comparison)
	writeCompareDecision(&builder, comparison)
	return []byte(builder.String()), nil
}

func writeCompareMetadata(builder *strings.Builder, comparison Comparison) {
	builder.WriteString("| Field | A (baseline) | B (candidate) |\n|---|---|---|\n")
	row := func(field, a, b string) {
		fmt.Fprintf(builder, "| %s | %s | %s |\n", field, mdEscape(a), mdEscape(b))
	}
	a, b := comparison.A, comparison.B
	row("Run ID", a.RunID, b.RunID)
	row("Suite", a.Suite, b.Suite)
	row("Split", string(a.Split), string(b.Split))
	row("Dataset version", a.DatasetVersion, b.DatasetVersion)
	row("Model", a.Model, b.Model)
	row("Prompt version", a.PromptVersion, b.PromptVersion)
	row("Seed", fmt.Sprintf("%d", a.Seed), fmt.Sprintf("%d", b.Seed))
	row("Git SHA", valueOrNA(a.GitSHA), valueOrNA(b.GitSHA))
	row("Repeat group", valueOrNA(a.RepeatGroup), valueOrNA(b.RepeatGroup))
	row("Repeat index", repeatIndexText(a.RepeatIndex), repeatIndexText(b.RepeatIndex))
	row("Cases", fmt.Sprintf("%d", a.Cases), fmt.Sprintf("%d", b.Cases))
	row("Proposals", fmt.Sprintf("%d", a.Proposals), fmt.Sprintf("%d", b.Proposals))
	row("Confidence observations", fmt.Sprintf("%d", a.ConfidenceObservations),
		fmt.Sprintf("%d", b.ConfidenceObservations))
	row("Complete", yesNoText(a.Complete), yesNoText(b.Complete))
	row("Missing scoreable", fmt.Sprintf("%d", a.MissingScoreable),
		fmt.Sprintf("%d", b.MissingScoreable))
	row("Missing modes", missingModesText(a.MissingModes), missingModesText(b.MissingModes))
	row("Aborted", abortedText(a), abortedText(b))
	row("Partial", yesNoText(a.Partial), yesNoText(b.Partial))
	builder.WriteString("\n")
}

func yesNoText(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func missingModesText(modes []string) string {
	if len(modes) == 0 {
		return "none"
	}
	return strings.Join(modes, ", ")
}

func abortedText(run CompareRun) string {
	if !run.Aborted {
		return "no"
	}
	if run.AbortReason == "" {
		return "yes"
	}
	return "yes (" + run.AbortReason + ")"
}

func repeatIndexText(value *int) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d", *value)
}

func writeCompareWarnings(builder *strings.Builder, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	builder.WriteString("Warnings:\n\n")
	for _, warning := range warnings {
		fmt.Fprintf(builder, "- %s\n", mdEscape(warning))
	}
	builder.WriteString("\n")
}

func writeCompareOverall(builder *strings.Builder, overall CompareBlock) {
	builder.WriteString("## Overall\n\n")
	builder.WriteString("Rates are proportions; brackets are 95% Wilson intervals. Delta is in " +
		"percentage points (B − A). `no change` includes overlapping intervals even when " +
		"the point delta is large.\n\n")
	writeCompareRateTable(builder, overall)
}

func writeCompareByMode(builder *strings.Builder, byMode map[string]CompareBlock) {
	builder.WriteString("## By failure mode\n\n")
	if len(byMode) == 0 {
		builder.WriteString("No results.\n\n")
		return
	}
	builder.WriteString("| Mode | Metric | A | B | Δ (pp) | A 95% CI | B 95% CI | Overlap | " +
		"Direction | Flag |\n")
	builder.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	modes := make([]string, 0, len(byMode))
	for mode := range byMode {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	for _, mode := range modes {
		block := byMode[mode]
		for _, spec := range compareMetricSpecs {
			label := mdEscape(mode) + " | " + spec.Display
			writeCompareRateRow(builder, label, spec, blockRate(block, spec.Name))
		}
	}
	builder.WriteString("\n")
}

func writeCompareRateTable(builder *strings.Builder, block CompareBlock) {
	builder.WriteString("| Metric | A | B | Δ (pp) | A 95% CI | B 95% CI | Overlap | " +
		"Direction | Flag |\n")
	builder.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, spec := range compareMetricSpecs {
		writeCompareRateRow(builder, spec.Display, spec, blockRate(block, spec.Name))
	}
	builder.WriteString("\n")
}

func writeCompareRateRow(builder *strings.Builder, label string, spec compareMetricSpec, rate CompareRate) {
	fmt.Fprintf(builder, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
		label,
		floatPtrText(rate.A, 4),
		floatPtrText(rate.B, 4),
		signedFloatPtrText(rate.DeltaPP, 2),
		compareCIText(rate.ACI),
		compareCIText(rate.BCI),
		overlapText(rate.Overlap),
		directionText(spec.HigherIsBetter),
		classificationText(rate.Classification))
}

func signedFloatPtrText(value *float64, places int) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.*f", places, *value)
}

func compareCIText(interval *[2]float64) string {
	if interval == nil {
		return "n/a"
	}
	return fmt.Sprintf("[%.4f, %.4f]", interval[0], interval[1])
}

func overlapText(value *bool) string {
	if value == nil {
		return "n/a"
	}
	if *value {
		return "yes"
	}
	return "no"
}

func directionText(higherIsBetter bool) string {
	if higherIsBetter {
		return "higher better"
	}
	return "lower better"
}

func classificationText(classification string) string {
	if classification == ClassificationNoChange {
		return "no change"
	}
	return classification
}

func writeCompareDescriptive(builder *strings.Builder, comparison Comparison) {
	builder.WriteString("## Cost, latency, and steps\n\n")
	builder.WriteString("Descriptive only; these measurements never drive the exit code.\n\n")
	builder.WriteString("| Measure | A | B | Δ | Δ% |\n|---|---|---|---|---|\n")
	fmt.Fprintf(builder, "| Cost mean (USD) | %s | %s | %s | %s |\n",
		valueOrNAPtr(comparison.Cost.AMeanUSD),
		valueOrNAPtr(comparison.Cost.BMeanUSD),
		valueOrNAPtr(comparison.Cost.DeltaUSD),
		signedFloatPtrText(comparison.Cost.DeltaPct, 2))
	writeCompareLatencyRow(builder, "Latency p50 (ms)",
		comparison.Latency.AP50MS, comparison.Latency.BP50MS,
		comparison.Latency.P50DeltaMS, comparison.Latency.P50DeltaPct)
	writeCompareLatencyRow(builder, "Latency p95 (ms)",
		comparison.Latency.AP95MS, comparison.Latency.BP95MS,
		comparison.Latency.P95DeltaMS, comparison.Latency.P95DeltaPct)
	fmt.Fprintf(builder, "| Steps mean | %s | %s | %s | — |\n",
		valueOrNAPtr(comparison.Steps.AMean),
		valueOrNAPtr(comparison.Steps.BMean),
		valueOrNAPtr(comparison.Steps.DeltaSteps))
	builder.WriteString("\n")
}

func writeCompareLatencyRow(builder *strings.Builder, label string,
	a, b, delta *int64, percent *float64) {
	fmt.Fprintf(builder, "| %s | %s | %s | %s | %s |\n",
		label,
		valueOrNAInt64(a),
		valueOrNAInt64(b),
		signedInt64PtrText(delta),
		signedFloatPtrText(percent, 2))
}

func signedInt64PtrText(value *int64) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+d", *value)
}

func writeCompareDecision(builder *strings.Builder, comparison Comparison) {
	builder.WriteString("## Decision\n\n")
	incomplete := comparison.ExitCriteria.Incomplete
	regressions := comparison.ExitCriteria.Regressions
	if !incomplete && len(regressions) == 0 {
		builder.WriteString("**No gating regression.**\n\n")
		if comparison.ExitCriteria.ReportOnly {
			builder.WriteString("--report-only: exit suppressed (would exit 0).\n")
		} else {
			builder.WriteString("Exit code 0.\n")
		}
		return
	}
	if incomplete {
		fmt.Fprintf(builder, "**Incomplete comparison:** run data is incomplete for %s; "+
			"the gating decision is not authoritative.\n\n",
			mdEscape(strings.Join(comparison.ExitCriteria.IncompleteRuns, ", ")))
	}
	if len(regressions) > 0 {
		labels := make([]string, 0, len(regressions))
		for _, regression := range regressions {
			labels = append(labels, regressionLabel(regression))
		}
		fmt.Fprintf(builder, "**Gating regression detected:** %s.\n\n",
			strings.Join(labels, "; "))
	}
	if comparison.ExitCriteria.ReportOnly {
		if incomplete {
			builder.WriteString("--report-only: exit suppressed (would exit 2 for incomplete data).\n")
		} else {
			builder.WriteString("--report-only: exit suppressed (would exit 1).\n")
		}
		return
	}
	if incomplete {
		builder.WriteString("Exit code 2 (incomplete run data).\n")
		return
	}
	builder.WriteString("Exit code 1.\n")
}

func regressionLabel(regression CompareRegression) string {
	metric := compareSpec(regression.Metric).Display
	if regression.Scope == "overall" {
		return metric + " (overall)"
	}
	return metric + " (mode " + regression.Mode + ")"
}
