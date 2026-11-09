// Package report builds and renders versioned eval reports from durable
// eval_runs/eval_case_results rows.
//
// Reports are built from the committed eval dataset truth plus the durable
// score columns. They never read tool arguments, raw tool results, memos,
// DSNs, or API keys; the only transcript content is the already-sanitized
// version-1 snapshot in eval_case_results.detail["transcript"]. Snapshot
// sanitization is not full PII/PAN scrubbing; M6-06 owns retention and PAN
// scanning (see docs/eval-report.md and spec §5.7).
package report

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/score"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// Schema is the JSON report schema version.
const Schema = "eval-report/v1"

// Locked defaults from the M3-05 contract.
const (
	// DefaultMinSample is the minimum scored-case denominator per rate, mode
	// and difficulty before a metric is flagged.
	DefaultMinSample = 30
	// DefaultAppendixLimit caps the failure appendix.
	DefaultAppendixLimit = 20
)

// Source is the narrow read interface used by Build. The CLI wires the
// Postgres implementation; tests use deterministic fixtures.
type Source interface {
	LoadRun(ctx context.Context, runID string) (RunRow, error)
	LoadRepeatGroup(ctx context.Context, groupID string) ([]RunRow, error)
	LoadCaseResults(ctx context.Context, runIDs []string) ([]CaseRow, error)
}

// RunRow is the whitelisted metadata Build needs from one eval_runs row.
// config is parsed by the source into Aborted/AbortReason/CostCapUSD so the
// report never dumps arbitrary config keys.
type RunRow struct {
	ID             string
	Suite          string
	Split          dataset.Split
	DatasetVersion string
	Model          string
	PromptVersion  string
	Seed           int64
	GitSHA         string
	StartedAt      time.Time
	EndedAt        *time.Time
	RepeatGroup    string
	RepeatIndex    *int
	Aborted        bool
	AbortReason    string
	CostCapUSD     *string
}

// CaseRow is one eval_case_results row. CostUSD stays decimal text end to end
// so no money value passes through float64.
type CaseRow struct {
	RunID        string
	ScenarioID   string
	FailureMode  string
	Correct      bool
	FalseFix     bool
	Escalated    bool
	TierExpected *string
	TierActual   *string
	Confidence   *decimal.Decimal
	CostUSD      string
	LatencyMS    int
	Steps        int
	Detail       map[string]any
}

// Options controls report construction. The clock is injected; production uses
// clock.Real and goldens pin clock.Fake.
type Options struct {
	// DatasetDir is the committed split directory. It defaults to
	// <module root>/eval/datasets/<split>.
	DatasetDir string
	// ModuleRoot is the module root used for the default dataset directory.
	ModuleRoot string
	// Clock supplies generated_at. nil means clock.Real.
	Clock clock.Clock
	// MinSample is the minimum per-rate denominator. <= 0 means
	// DefaultMinSample.
	MinSample int
	// AppendixLimit caps the failure appendix. <= 0 means
	// DefaultAppendixLimit.
	AppendixLimit int
}

// Report is the eval-report/v1 document. Markdown and JSON render from this
// single model so their numbers cannot drift.
type Report struct {
	Schema          string                 `json:"schema"`
	GeneratedAt     time.Time              `json:"generated_at"`
	Run             RunMetadata            `json:"run"`
	Runs            []RunMetadata          `json:"runs"`
	Dataset         DatasetInfo            `json:"dataset"`
	Headline        MetricBlock            `json:"headline"`
	ByMode          map[string]MetricBlock `json:"by_mode"`
	ByDifficulty    map[string]MetricBlock `json:"by_difficulty"`
	Repeats         *Repeats               `json:"repeats"`
	Calibration     *Calibration           `json:"calibration"`
	Scorecards      []Scorecard            `json:"scorecards"`
	FailureAppendix []AppendixCase         `json:"failure_appendix"`
	Warnings        []string               `json:"warnings"`

	// completeness is the structured provenance behind the human-readable
	// completeness warnings. It is deliberately unexported and therefore not
	// part of the eval-report/v1 JSON contract; Compare uses it to fail closed
	// on incomplete runs without parsing warning text.
	completeness map[string]runCompleteness
}

// runCompleteness is one run's coverage verdict against the committed dataset.
// Warnings holds the exact completeness warnings emitted by Build so compare
// can surface the same wording.
type runCompleteness struct {
	Partial          bool
	Aborted          bool
	AbortReason      string
	NoCaseResults    bool
	MissingScoreable int
	MissingModes     []string
	Warnings         []string
}

// complete reports whether the run record and its case results cover the
// committed dataset. Missing modes imply missing scoreable scenarios, but are
// tracked separately for diagnostics.
func (c runCompleteness) complete() bool {
	return !c.Partial && !c.Aborted && !c.NoCaseResults && c.MissingScoreable == 0
}

// RunMetadata is the whitelisted run provenance. It never contains arbitrary
// eval_runs.config keys.
type RunMetadata struct {
	ID             string        `json:"id"`
	Suite          string        `json:"suite"`
	Split          dataset.Split `json:"split"`
	DatasetVersion string        `json:"dataset_version"`
	Model          string        `json:"model"`
	PromptVersion  string        `json:"prompt_version"`
	Seed           int64         `json:"seed"`
	GitSHA         string        `json:"git_sha"`
	StartedAt      time.Time     `json:"started_at"`
	EndedAt        *time.Time    `json:"ended_at"`
	Aborted        bool          `json:"aborted"`
	AbortReason    string        `json:"abort_reason"`
	CostCapUSD     *string       `json:"cost_cap_usd"`
	RepeatGroup    string        `json:"repeat_group"`
	RepeatIndex    *int          `json:"repeat_index"`
}

// DatasetInfo records the committed dataset that was rebuilt and verified.
type DatasetInfo struct {
	Split              dataset.Split `json:"split"`
	EvalDatasetVersion string        `json:"eval_dataset_version"`
	Matched            bool          `json:"matched"`
}

// MetricBlock is the headline or one per-mode/per-difficulty metric block.
type MetricBlock struct {
	Cases                int                  `json:"cases"`
	Accuracy             RateView             `json:"accuracy"`
	FalseFix             RateView             `json:"false_fix"`
	CorrectEscalation    RateView             `json:"correct_escalation"`
	OverEscalation       RateView             `json:"over_escalation"`
	EvidenceValidity     RateView             `json:"evidence_validity"`
	InjectionResistance  RateView             `json:"injection_resistance"`
	Cost                 CostView             `json:"cost"`
	Latency              LatencyView          `json:"latency"`
	Steps                StepsView            `json:"steps"`
	AutoResolvePrecision score.DeferredMetric `json:"auto_resolve_precision"`
}

// RateView wraps one score.Rate with the Wilson interval and minimum-sample
// fields. A single run carries numerator/denominator; a repeat-group mean has
// null numerator/denominator and no Wilson interval on the mean.
type RateView struct {
	Defined      bool        `json:"defined"`
	Aggregation  string      `json:"aggregation"`
	Numerator    *int        `json:"numerator"`
	Denominator  *int        `json:"denominator"`
	Value        *float64    `json:"value"`
	WilsonLow    *float64    `json:"wilson_low"`
	WilsonHigh   *float64    `json:"wilson_high"`
	MinSample    int         `json:"min_sample"`
	MinSampleMet bool        `json:"min_sample_met"`
	Repeat       *RepeatRate `json:"repeat"`
}

// RepeatRate is the repeat-group summary attached to a rate view.
type RepeatRate struct {
	K      int          `json:"k"`
	Min    *float64     `json:"min"`
	Max    *float64     `json:"max"`
	Stddev *float64     `json:"stddev"`
	PerRun []PerRunRate `json:"per_run"`
}

// PerRunRate is one repeat member's rate and its own Wilson interval. Repeat
// cases are never pooled into one binomial sample.
type PerRunRate struct {
	RunID       string   `json:"run_id"`
	RepeatIndex int      `json:"repeat_index"`
	Numerator   int      `json:"numerator"`
	Denominator int      `json:"denominator"`
	Value       *float64 `json:"value"`
	WilsonLow   *float64 `json:"wilson_low"`
	WilsonHigh  *float64 `json:"wilson_high"`
}

// CostView reports decimal-string money. Counts are per run in the repeats
// block; the group mean is the mean of the per-run means, never pooled.
type CostView struct {
	Defined  bool        `json:"defined"`
	Cases    int         `json:"cases"`
	TotalUSD *string     `json:"total_usd"`
	MeanUSD  *string     `json:"mean_usd"`
	Repeat   *RepeatCost `json:"repeat"`
}

// RepeatCost is the repeat summary for cost. Money stays decimal text.
type RepeatCost struct {
	K         int          `json:"k"`
	MeanUSD   *string      `json:"mean_usd"`
	MinUSD    *string      `json:"min_usd"`
	MaxUSD    *string      `json:"max_usd"`
	StddevUSD *string      `json:"stddev_usd"`
	PerRun    []PerRunCost `json:"per_run"`
}

// PerRunCost is one repeat member's cost and case count.
type PerRunCost struct {
	RunID       string  `json:"run_id"`
	RepeatIndex int     `json:"repeat_index"`
	Cases       int     `json:"cases"`
	TotalUSD    *string `json:"total_usd"`
	MeanUSD     *string `json:"mean_usd"`
}

// LatencyView reports nearest-rank p50/p95 wall-clock latency in milliseconds.
type LatencyView struct {
	Defined      bool           `json:"defined"`
	Observations int            `json:"observations"`
	P50MS        *int64         `json:"p50_ms"`
	P95MS        *int64         `json:"p95_ms"`
	Repeat       *RepeatLatency `json:"repeat"`
}

// RepeatLatency is the repeat summary for latency percentile means.
type RepeatLatency struct {
	K     int          `json:"k"`
	P50MS *RepeatValue `json:"p50_ms"`
	P95MS *RepeatValue `json:"p95_ms"`
}

// StepsView reports tool-call steps. Mean is decimal text with two fractional
// digits; steps are counts, not money, but decimal formatting keeps the output
// deterministic.
type StepsView struct {
	Defined bool         `json:"defined"`
	Cases   int          `json:"cases"`
	Total   int          `json:"total"`
	Mean    *string      `json:"mean"`
	Repeat  *RepeatSteps `json:"repeat"`
}

// RepeatSteps is the repeat summary for the steps mean.
type RepeatSteps struct {
	K    int          `json:"k"`
	Mean *RepeatValue `json:"mean"`
}

// Repeats is the top-level repeat group summary. Metrics carry the per-run
// values used to compute the spread; cost is separate because money is text.
type Repeats struct {
	Group   string                  `json:"group"`
	K       int                     `json:"k"`
	Metrics map[string]RepeatMetric `json:"metrics"`
	Cost    *RepeatCost             `json:"cost"`
	PerRun  []RepeatRun             `json:"per_run"`
}

// RepeatMetric is mean/min/max/sample-stddev over per-run values. Stddev is
// null for k=1.
type RepeatMetric struct {
	Mean   *float64            `json:"mean"`
	Min    *float64            `json:"min"`
	Max    *float64            `json:"max"`
	Stddev *float64            `json:"stddev"`
	PerRun []RepeatMetricValue `json:"per_run"`
}

// RepeatMetricValue is one per-run value in a repeat metric.
type RepeatMetricValue struct {
	RunID       string   `json:"run_id"`
	RepeatIndex int      `json:"repeat_index"`
	Value       *float64 `json:"value"`
}

// RepeatRun carries the per-run counts and descriptive measures for a repeat
// group so counts are never multilied into a pooled denominator.
type RepeatRun struct {
	RunID        string `json:"run_id"`
	RepeatIndex  int    `json:"repeat_index"`
	Cases        int    `json:"cases"`
	CostTotalUSD string `json:"cost_total_usd"`
	CostMeanUSD  string `json:"cost_mean_usd"`
	LatencyP50MS *int64 `json:"latency_p50_ms"`
	LatencyP95MS *int64 `json:"latency_p95_ms"`
	StepsTotal   int    `json:"steps_total"`
	StepsMean    string `json:"steps_mean"`
}

// Scorecard is the generic component-scorecard shape filled by M3-06/M4-05.
type Scorecard struct {
	Name       string            `json:"name"`
	Applicable bool              `json:"applicable"`
	DeferredTo string            `json:"deferred_to,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Cases      int               `json:"cases"`
	Metrics    []ScorecardMetric `json:"metrics"`
}

// ScorecardMetric is one scorecard measurement.
type ScorecardMetric struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Unit  string `json:"unit"`
	Note  string `json:"note"`
}

// verifierScorecard is the M3-06 placeholder filled by M4-05. M3-06 does not
// import or create internal/policy/verify.
func verifierScorecard() Scorecard {
	return Scorecard{
		Name:       "verifier",
		Applicable: false,
		DeferredTo: "M4-05",
		Reason:     "deterministic verifiers land in M4-05",
		Cases:      0,
		Metrics:    []ScorecardMetric{},
	}
}

// Build loads one run (or its complete repeat group), rebuilds and verifies
// the committed dataset, reconstructs score.CaseResults, and renders the
// report model. Any load/validation/version/truth drift is a hard error.
func Build(ctx context.Context, src Source, runID string, opts Options) (Report, error) {
	if src == nil {
		return Report{}, errors.New("eval/report: source is required")
	}
	runID = strings.ToLower(strings.TrimSpace(runID))
	if runID == "" {
		return Report{}, errors.New("eval/report: run id is required")
	}
	opts = normalizeOptions(opts)

	requested, err := src.LoadRun(ctx, runID)
	if err != nil {
		return Report{}, fmt.Errorf("eval/report: load run %s: %w", runID, err)
	}
	if requested.ID == "" {
		requested.ID = runID
	}
	runs := []RunRow{requested}
	if requested.RepeatGroup != "" {
		siblings, err := src.LoadRepeatGroup(ctx, requested.RepeatGroup)
		if err != nil {
			return Report{}, fmt.Errorf("eval/report: load repeat group %s: %w",
				requested.RepeatGroup, err)
		}
		runs, err = validateRepeatGroup(requested, siblings)
		if err != nil {
			return Report{}, err
		}
	}
	runs = sortedRunsByIndex(runs)

	resolved, err := loadDataset(runs[0], opts)
	if err != nil {
		return Report{}, err
	}

	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	rows, err := src.LoadCaseResults(ctx, ids)
	if err != nil {
		return Report{}, fmt.Errorf("eval/report: load case results: %w", err)
	}
	byRun, err := groupRowsByRun(ids, rows)
	if err != nil {
		return Report{}, err
	}

	runData := make([]runReport, 0, len(runs))
	for _, run := range runs {
		results, err := reconstructResults(run, byRun[run.ID], resolved.Scenarios)
		if err != nil {
			return Report{}, err
		}
		runData = append(runData, newRunReport(run, results, resolved.Scenarios))
	}

	warnings, completeness := initialWarnings(runs, runData, resolved)
	report := Report{
		Schema:      Schema,
		GeneratedAt: opts.Clock.Now().UTC(),
		Run:         toRunMetadata(requested),
		Runs:        metadataForRuns(runs),
		Dataset: DatasetInfo{
			Split:              resolved.Manifest.Split,
			EvalDatasetVersion: resolved.Manifest.EvalDatasetVersion,
			Matched:            true,
		},
		ByMode:       map[string]MetricBlock{},
		ByDifficulty: map[string]MetricBlock{},
		// M3-06 fills calibration and the verifier scorecard placeholder.
		Calibration:     nil,
		Scorecards:      []Scorecard{verifierScorecard()},
		FailureAppendix: []AppendixCase{},
	}
	if len(runData) > 1 || (len(runData) == 1 && runs[0].RepeatGroup != "") {
		report.Repeats = buildRepeats(runData, opts.MinSample)
	}
	report.Headline = buildHeadline(runData, opts.MinSample)
	report.ByMode = buildPartitions(runData, opts.MinSample, partitionMode)
	report.ByDifficulty = buildPartitions(runData, opts.MinSample, partitionDifficulty)
	calibration, err := buildCalibration(runData)
	if err != nil {
		return Report{}, err
	}
	report.Calibration = calibration

	appendix, transcriptWarnings := buildAppendix(runData, resolved.Scenarios, requested, opts)
	report.FailureAppendix = appendix
	warnings = append(warnings, transcriptWarnings...)
	warnings = append(warnings, sampleWarnings(report.ByMode, report.ByDifficulty,
		opts.MinSample)...)
	report.Warnings = warnings
	report.completeness = completeness
	return report, nil
}

func normalizeOptions(opts Options) Options {
	if opts.MinSample <= 0 {
		opts.MinSample = DefaultMinSample
	}
	if opts.AppendixLimit <= 0 {
		opts.AppendixLimit = DefaultAppendixLimit
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	return opts
}

func toRunMetadata(run RunRow) RunMetadata {
	return RunMetadata{
		ID:             run.ID,
		Suite:          run.Suite,
		Split:          run.Split,
		DatasetVersion: run.DatasetVersion,
		Model:          run.Model,
		PromptVersion:  run.PromptVersion,
		Seed:           run.Seed,
		GitSHA:         run.GitSHA,
		StartedAt:      run.StartedAt,
		EndedAt:        run.EndedAt,
		Aborted:        run.Aborted,
		AbortReason:    run.AbortReason,
		CostCapUSD:     cloneString(run.CostCapUSD),
		RepeatGroup:    run.RepeatGroup,
		RepeatIndex:    cloneInt(run.RepeatIndex),
	}
}

func metadataForRuns(runs []RunRow) []RunMetadata {
	out := make([]RunMetadata, 0, len(runs))
	for _, run := range runs {
		out = append(out, toRunMetadata(run))
	}
	return out
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func validateRepeatGroup(requested RunRow, siblings []RunRow) ([]RunRow, error) {
	if len(siblings) == 0 {
		return nil, fmt.Errorf("eval/report: repeat group %s has no runs",
			requested.RepeatGroup)
	}
	runs := sortedRunsByIndex(siblings)
	foundRequested := false
	seen := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		if run.ID == requested.ID {
			foundRequested = true
		}
		if _, exists := seen[run.ID]; exists {
			return nil, fmt.Errorf("eval/report: repeat group %s contains duplicate run %s",
				requested.RepeatGroup, run.ID)
		}
		seen[run.ID] = struct{}{}
		if run.RepeatGroup != requested.RepeatGroup {
			return nil, fmt.Errorf("eval/report: run %s is not in repeat group %s",
				run.ID, requested.RepeatGroup)
		}
		if run.RepeatIndex == nil {
			return nil, fmt.Errorf("eval/report: repeat run %s has no repeat_index", run.ID)
		}
	}
	if !foundRequested {
		return nil, fmt.Errorf("eval/report: requested run %s is missing from repeat group %s",
			requested.ID, requested.RepeatGroup)
	}
	for index, run := range runs {
		if *run.RepeatIndex != index {
			return nil, fmt.Errorf(
				"eval/report: repeat group %s is incomplete: expected repeat_index %d, got %d (run %s)",
				requested.RepeatGroup, index, *run.RepeatIndex, run.ID)
		}
	}
	base := runs[0]
	for _, run := range runs[1:] {
		if err := sameRepeatFingerprint(base, run); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func sameRepeatFingerprint(base, candidate RunRow) error {
	mismatches := make([]string, 0, 6)
	if candidate.Suite != base.Suite {
		mismatches = append(mismatches, fmt.Sprintf("suite %q != %q", candidate.Suite, base.Suite))
	}
	if candidate.Split != base.Split {
		mismatches = append(mismatches, fmt.Sprintf("split %q != %q", candidate.Split, base.Split))
	}
	if candidate.DatasetVersion != base.DatasetVersion {
		mismatches = append(mismatches, fmt.Sprintf("dataset_version %q != %q",
			candidate.DatasetVersion, base.DatasetVersion))
	}
	if candidate.Model != base.Model {
		mismatches = append(mismatches, fmt.Sprintf("model %q != %q", candidate.Model, base.Model))
	}
	if candidate.PromptVersion != base.PromptVersion {
		mismatches = append(mismatches, fmt.Sprintf("prompt_version %q != %q",
			candidate.PromptVersion, base.PromptVersion))
	}
	if candidate.Seed != base.Seed {
		mismatches = append(mismatches, fmt.Sprintf("seed %d != %d", candidate.Seed, base.Seed))
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("eval/report: repeat group %s has inconsistent runs (run %s): %s",
			base.RepeatGroup, candidate.ID, strings.Join(mismatches, "; "))
	}
	return nil
}

func sortedRunsByIndex(runs []RunRow) []RunRow {
	sorted := append([]RunRow(nil), runs...)
	sortRuns(sorted)
	return sorted
}

func sortRuns(runs []RunRow) {
	// Stable insertion sort keeps the helper dependency-free and deterministic.
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0; j-- {
			left, right := runs[j-1], runs[j]
			switch {
			case repeatIndexValue(left) > repeatIndexValue(right):
				runs[j-1], runs[j] = runs[j], runs[j-1]
			case repeatIndexValue(left) == repeatIndexValue(right) && left.ID > right.ID:
				runs[j-1], runs[j] = runs[j], runs[j-1]
			default:
				j = 0
			}
		}
	}
}

func repeatIndexValue(run RunRow) int {
	if run.RepeatIndex == nil {
		return -1
	}
	return *run.RepeatIndex
}

func groupRowsByRun(runIDs []string, rows []CaseRow) (map[string][]CaseRow, error) {
	known := make(map[string]struct{}, len(runIDs))
	for _, id := range runIDs {
		known[id] = struct{}{}
	}
	grouped := make(map[string][]CaseRow, len(runIDs))
	for _, row := range rows {
		if _, ok := known[row.RunID]; !ok {
			return nil, fmt.Errorf(
				"eval/report: case result for unknown run %s (scenario %s)",
				row.RunID, row.ScenarioID)
		}
		grouped[row.RunID] = append(grouped[row.RunID], row)
	}
	return grouped, nil
}

func initialWarnings(runs []RunRow, data []runReport,
	resolved *resolvedDataset) ([]string, map[string]runCompleteness) {
	warnings := make([]string, 0, len(runs)*4)
	completeness := make(map[string]runCompleteness, len(runs))
	for _, run := range runs {
		record := runCompleteness{
			Partial: run.EndedAt == nil,
			Aborted: run.Aborted,
		}
		if run.Aborted {
			record.AbortReason = strings.TrimSpace(run.AbortReason)
			if record.AbortReason == "" {
				record.AbortReason = "unspecified"
			}
		}
		if record.Partial {
			warning := fmt.Sprintf("run %s is partial: ended_at is null", run.ID)
			warnings = append(warnings, warning)
			record.Warnings = append(record.Warnings, warning)
		}
		if record.Aborted {
			warning := fmt.Sprintf("run %s aborted (%s)", run.ID, record.AbortReason)
			warnings = append(warnings, warning)
			record.Warnings = append(record.Warnings, warning)
		}
		completeness[run.ID] = record
	}
	var gitSHA string
	for _, run := range runs {
		if run.GitSHA == "" {
			continue
		}
		if gitSHA == "" {
			gitSHA = run.GitSHA
			continue
		}
		if run.GitSHA != gitSHA {
			warnings = append(warnings, fmt.Sprintf(
				"repeat group runs recorded different git SHAs (%s vs %s); allowed but noted",
				gitSHA, run.GitSHA))
			break
		}
	}

	scoreable := scoreableScenarios(resolved.Truth.Scenarios)
	for index, run := range runs {
		record := completeness[run.ID]
		if len(data[index].results) == 0 {
			record.NoCaseResults = true
			warning := fmt.Sprintf("run %s has no case results", run.ID)
			warnings = append(warnings, warning)
			record.Warnings = append(record.Warnings, warning)
		}
		present := make(map[string]struct{}, len(data[index].results))
		for _, result := range data[index].results {
			present[result.ScenarioID] = struct{}{}
		}
		missing := 0
		for scenarioID := range scoreable {
			if _, ok := present[scenarioID]; !ok {
				missing++
			}
		}
		if missing > 0 {
			record.MissingScoreable = missing
			state := "missing"
			if run.EndedAt == nil || run.Aborted {
				state = "partial run missing"
			}
			warning := fmt.Sprintf(
				"run %s is %s %d scoreable scenarios", run.ID, state, missing)
			warnings = append(warnings, warning)
			record.Warnings = append(record.Warnings, warning)
		}
		if run.EndedAt == nil || run.Aborted {
			expectedModes := truthModes(resolved.Truth.Scenarios)
			missingModes := make([]string, 0, len(expectedModes))
			for mode := range expectedModes {
				if _, ok := data[index].modes[mode]; !ok {
					missingModes = append(missingModes, mode)
				}
			}
			if len(missingModes) > 0 {
				sort.Strings(missingModes)
				record.MissingModes = missingModes
				warning := fmt.Sprintf(
					"partial run %s has no results for modes: %s",
					run.ID, strings.Join(missingModes, ", "))
				warnings = append(warnings, warning)
				record.Warnings = append(record.Warnings, warning)
			}
		}
		completeness[run.ID] = record
	}
	return warnings, completeness
}

func scoreableScenarios(scenarios []dataset.ScenarioTruth) map[string]struct{} {
	out := make(map[string]struct{}, len(scenarios))
	for _, scenario := range scenarios {
		if scenario.BreakSignature != "" || scenario.Mode == modes.InjectionMemo {
			out[scenario.ScenarioID] = struct{}{}
		}
	}
	return out
}

func truthModes(scenarios []dataset.ScenarioTruth) map[string]struct{} {
	out := make(map[string]struct{}, len(scenarios))
	for _, scenario := range scenarios {
		if scenario.BreakSignature != "" || scenario.Mode == modes.InjectionMemo {
			out[scenario.Mode] = struct{}{}
		}
	}
	return out
}

func moneyString(value decimal.Decimal) string {
	return value.StringFixed(6)
}
