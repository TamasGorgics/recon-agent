package component

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/matchscore"
)

// MatcherSchema is the matcher report JSON schema version.
const MatcherSchema = "matcher-report/v1"

// exclusionReasons is the stable, complete set of matchscore exclusion keys.
// Missing keys render as zero so the report shape does not depend on which
// reasons happened to occur.
var exclusionReasons = []string{
	matchscore.ReasonAmbiguous,
	matchscore.ReasonEscalate,
	matchscore.ReasonNoExternal,
	matchscore.ReasonNoLedger,
	matchscore.ReasonNotExpectedLink,
}

// Options controls one matcher component build.
type Options struct {
	// Split selects the committed dataset split.
	Split dataset.Split
	// DatasetDir is the committed split directory. It defaults to
	// <module root>/eval/datasets/<split>.
	DatasetDir string
	// ModuleRoot is the module root used for the default dataset directory.
	ModuleRoot string
	// AdminDSN is the sandbox maintenance DSN. When empty, TEST_DATABASE_URL
	// is preferred and DATABASE_URL is the fallback.
	AdminDSN string
	// Clock supplies generated_at. nil means clock.Real.
	Clock clock.Clock
	// Logger is optional; DSNs and secrets are never logged.
	Logger *slog.Logger
	// BuildConfig overrides the dataset config used to rebuild the split.
	// Production leaves it nil; tests use it for a miniature dataset.
	BuildConfig func(dataset.Manifest) (dataset.Config, error)
}

// MatcherReport is the matcher-report/v1 document.
type MatcherReport struct {
	Schema             string         `json:"schema"`
	GeneratedAt        time.Time      `json:"generated_at"`
	Split              dataset.Split  `json:"split"`
	DatasetVersion     string         `json:"dataset_version"`
	Scenarios          int            `json:"scenarios"`
	ScoreableScenarios int            `json:"scoreable_scenarios"`
	Matches            int            `json:"matches"`
	CorrectMatches     int            `json:"correct_matches"`
	Precision          RateView       `json:"precision"`
	ExpectedLinks      int            `json:"expected_links"`
	RecalledLinks      int            `json:"recalled_links"`
	Recall             RateView       `json:"recall"`
	PerPass            []PassScore    `json:"per_pass"`
	Excluded           map[string]int `json:"excluded"`
	Warnings           []string       `json:"warnings"`
}

// RateView is a precision or recall value. A zero denominator is not
// applicable (defined=false, value=null), never a misleading 1.0 or 0.0.
type RateView struct {
	Defined     bool     `json:"defined"`
	Numerator   int      `json:"numerator"`
	Denominator int      `json:"denominator"`
	Value       *float64 `json:"value"`
}

// PassScore is one matcher rule's contribution. Recall uses the global
// required-link denominator (M1-16), so per-pass recall sums to overall recall.
type PassScore struct {
	Pass           string   `json:"pass"`
	Matches        int      `json:"matches"`
	CorrectMatches int      `json:"correct_matches"`
	Precision      RateView `json:"precision"`
	RequiredHits   int      `json:"required_hits"`
	Recall         RateView `json:"recall"`
}

// Build loads and verifies the committed dataset, creates a sandbox clone with
// the production matcher, reads active matches through recon_app, and renders
// the matcher report. It never persists eval results.
func Build(ctx context.Context, opts Options) (MatcherReport, error) {
	opts = normalizeOptions(opts)
	if !opts.Split.Valid() {
		return MatcherReport{}, fmt.Errorf(
			"eval/component: split %q is not dev, test, or injection", opts.Split)
	}
	resolved, err := loadDataset(opts)
	if err != nil {
		return MatcherReport{}, err
	}
	adminDSN := resolveAdminDSN(opts)
	if adminDSN == "" {
		return MatcherReport{}, errors.New(
			"eval/component: sandbox admin DSN is required (set TEST_DATABASE_URL or DATABASE_URL)")
	}
	manager, err := sandbox.NewManager(sandbox.Options{
		AdminDSN:  adminDSN,
		BaseClock: resolved.Config.GeneratedAt,
		Logger:    opts.Logger,
	})
	if err != nil {
		return MatcherReport{}, fmt.Errorf("eval/component: create sandbox manager: %w", err)
	}
	box, err := manager.Clone(ctx, resolved.Dataset, sandbox.LoadOptions{Config: resolved.Config})
	if err != nil {
		return MatcherReport{}, fmt.Errorf("eval/component: create matcher sandbox: %w", err)
	}
	built, buildErr := reportFromSandbox(ctx, box, resolved, opts)
	closeErr := box.Close(context.WithoutCancel(ctx))
	if buildErr != nil {
		return MatcherReport{}, errors.Join(buildErr, closeErr)
	}
	if closeErr != nil {
		return MatcherReport{}, fmt.Errorf("eval/component: close matcher sandbox: %w", closeErr)
	}
	return built, nil
}

// reportFromSandbox is the clone-scoped half of Build. Loading matches through
// box.App proves the matcher report reads only with the existing recon_app
// grants; a read failure is returned rather than widened to the admin pool.
func reportFromSandbox(ctx context.Context, box *sandbox.Sandbox,
	resolved *resolvedDataset, opts Options) (MatcherReport, error) {
	if box == nil || box.App == nil {
		return MatcherReport{}, errors.New("eval/component: sandbox app pool is required")
	}
	groups, err := matchscore.LoadActiveMatches(ctx, box.App, "")
	if err != nil {
		return MatcherReport{}, fmt.Errorf("eval/component: load active matches: %w", err)
	}
	synthTruth := AdaptTruth(resolved.Manifest.EvalDatasetVersion, resolved.Dataset.Truth)
	result := matchscore.Score(groups, synthTruth)
	return NewMatcherReport(resolved.Manifest.Split, resolved.Manifest.EvalDatasetVersion,
		resolved.Dataset.Truth, result, opts.Clock.Now().UTC()), nil
}

// NewMatcherReport converts a matchscore result into the report model. It
// intentionally keeps the underlying matchscore zero-denominator contract
// intact and only converts zero denominators to not-applicable in the report.
func NewMatcherReport(split dataset.Split, datasetVersion string, truth dataset.GroundTruth,
	result matchscore.Result, generatedAt time.Time) MatcherReport {
	report := MatcherReport{
		Schema:             MatcherSchema,
		GeneratedAt:        generatedAt.UTC(),
		Split:              split,
		DatasetVersion:     datasetVersion,
		Scenarios:          len(truth.Scenarios),
		ScoreableScenarios: ScoreableScenarios(truth),
		Matches:            result.Matches,
		CorrectMatches:     result.CorrectMatches,
		Precision:          rateView(result.CorrectMatches, result.Matches),
		ExpectedLinks:      result.ExpectedLinks,
		RecalledLinks:      result.RecalledLinks,
		Recall:             rateView(result.RecalledLinks, result.ExpectedLinks),
		PerPass:            make([]PassScore, 0, len(result.PerPass)),
		Excluded:           normalizeExcluded(result.Excluded),
	}

	passes := make([]string, 0, len(result.PerPass))
	for pass := range result.PerPass {
		passes = append(passes, pass)
	}
	sort.Strings(passes)
	for _, name := range passes {
		pass := result.PerPass[name]
		report.PerPass = append(report.PerPass, PassScore{
			Pass:           pass.Pass,
			Matches:        pass.Matches,
			CorrectMatches: pass.CorrectMatches,
			Precision:      rateView(pass.CorrectMatches, pass.Matches),
			RequiredHits:   pass.RequiredHits,
			Recall:         rateView(pass.RequiredHits, result.ExpectedLinks),
		})
	}

	if report.Matches == 0 {
		report.Warnings = append(report.Warnings,
			"no active matches: precision is not applicable")
	}
	if report.ExpectedLinks == 0 {
		report.Warnings = append(report.Warnings,
			"no required links: recall is not applicable")
	}
	if report.Recall.Defined && report.Recall.Value != nil && *report.Recall.Value < 1 {
		report.Warnings = append(report.Warnings,
			"recall below 1.0: inspect per-pass misses and ADR 0004 residuals")
	}
	return report
}

func normalizeOptions(opts Options) Options {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	return opts
}

// resolveAdminDSN lets the CLI own no DSN logic while integration tests inject
// an explicit admin DSN.
func resolveAdminDSN(opts Options) string {
	if dsn := strings.TrimSpace(opts.AdminDSN); dsn != "" {
		return dsn
	}
	if test := strings.TrimSpace(os.Getenv(config.EnvDatabaseTestURL)); test != "" {
		return test
	}
	return strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
}

func rateView(numerator, denominator int) RateView {
	view := RateView{Numerator: numerator, Denominator: denominator}
	if denominator <= 0 {
		return view
	}
	value := float64(numerator) / float64(denominator)
	view.Defined = true
	view.Value = &value
	return view
}

func normalizeExcluded(excluded map[string]int) map[string]int {
	out := make(map[string]int, len(exclusionReasons))
	for _, reason := range exclusionReasons {
		out[reason] = excluded[reason]
	}
	return out
}

// RenderJSON renders matcher-report/v1 with stable indentation and a trailing
// newline. generated_at is the only volatile field; tests pin the clock.
func RenderJSON(report MatcherReport) ([]byte, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// RenderMarkdown renders the human matcher report from the same model as
// RenderJSON, so numbers cannot drift between surfaces.
func RenderMarkdown(report MatcherReport) ([]byte, error) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Matcher report — split %s\n\n", report.Split)
	builder.WriteString("| Field | Value |\n|---|---|\n")
	row := func(field, value string) {
		fmt.Fprintf(&builder, "| %s | %s |\n", field, mdEscape(value))
	}
	row("Split", string(report.Split))
	row("Dataset version", report.DatasetVersion)
	row("Scenarios", fmt.Sprintf("%d", report.Scenarios))
	row("Scoreable scenarios", fmt.Sprintf("%d", report.ScoreableScenarios))
	row("Generated at", report.GeneratedAt.Format(time.RFC3339))
	builder.WriteString("\n")

	if len(report.Warnings) > 0 {
		builder.WriteString("Warnings:\n\n")
		for _, warning := range report.Warnings {
			fmt.Fprintf(&builder, "- %s\n", mdEscape(warning))
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## Headline\n\n")
	builder.WriteString("| Metric | Value | n |\n|---|---|---|\n")
	fmt.Fprintf(&builder, "| Precision | %s | %s |\n",
		rateValue(report.Precision), rateN(report.Precision))
	fmt.Fprintf(&builder, "| Recall | %s | %s |\n",
		rateValue(report.Recall), rateN(report.Recall))
	builder.WriteString("\n")

	builder.WriteString("## By pass\n\n")
	if len(report.PerPass) == 0 {
		builder.WriteString("No active passes.\n\n")
	} else {
		builder.WriteString("Recall uses the global required-link denominator, so per-pass " +
			"recall sums to overall recall.\n\n")
		builder.WriteString("| Pass | Matches | Correct | Precision | Required hits | Recall |\n")
		builder.WriteString("|---|---|---|---|---|---|\n")
		for _, pass := range report.PerPass {
			fmt.Fprintf(&builder, "| %s | %d | %d | %s | %d | %s |\n",
				mdEscape(pass.Pass), pass.Matches, pass.CorrectMatches,
				rateValue(pass.Precision), pass.RequiredHits, rateValue(pass.Recall))
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## Exclusions\n\n")
	builder.WriteString("| Reason | Scenarios |\n|---|---|\n")
	for _, reason := range exclusionReasons {
		fmt.Fprintf(&builder, "| %s | %d |\n", mdEscape(reason), report.Excluded[reason])
	}
	builder.WriteString("\n")

	if report.Recall.Defined && report.Recall.Value != nil && *report.Recall.Value < 1 {
		builder.WriteString("Recall below 1.0: inspect per-pass misses and ADR 0004 residuals.\n")
	}
	return []byte(builder.String()), nil
}

func rateValue(view RateView) string {
	if !view.Defined || view.Value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.4f", *view.Value)
}

func rateN(view RateView) string {
	if view.Denominator <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d", view.Numerator, view.Denominator)
}

func mdEscape(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "`", "'")
	return value
}
