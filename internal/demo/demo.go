// Package demo runs the M1 end-to-end demonstration pipeline:
//
//	generate -> load ledger + ingest -> match -> cases -> tie-out -> summary.
//
// The pipeline is deliberately deterministic: seed, difficulty, day count and
// failure-mode mix define the dataset, and the summary contains no database
// UUIDs. Running it twice against the same reset database therefore prints the
// same numbers. Reset is dev-only and refuses to run when RECON_ENV is
// production.
package demo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/matchscore"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
	"github.com/TamasGorgics/recon-agent/migrations"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// Defaults for `recon demo seed` and TestMilestone1Gate.
const (
	DefaultSeed       int64 = 1
	DefaultDays             = 14
	DefaultDifficulty       = synth.DifficultyRealistic
	// DefaultScenarioCount spreads failure modes evenly over all twelve M1
	// modes. It exercises break/case creation without changing the generator's
	// deterministic output rules.
	DefaultScenarioCount = 24
)

// ErrResetProduction is returned by Reset when RECON_ENV=production.
var ErrResetProduction = errors.New("demo: --reset is refused when RECON_ENV=production")

// Options configures one demo run. Zero values select the documented defaults.
type Options struct {
	DBURL      string
	Seed       int64
	Days       int
	Difficulty string
	Modes      modes.Config

	Synth  config.SynthConfig
	Match  config.MatchConfig
	Ledger config.LedgerConfig
	// Sources overrides the generated per-source settlement calendars; empty
	// uses Synth.Sources and then the generator defaults.
	Sources map[string]config.SourceConfig

	Clock clock.Clock
}

func (o Options) withDefaults() Options {
	if o.Seed == 0 {
		o.Seed = DefaultSeed
	}
	if o.Days == 0 {
		o.Days = DefaultDays
	}
	if o.Difficulty == "" {
		o.Difficulty = DefaultDifficulty
	}
	if o.Modes.Total == 0 && len(o.Modes.Counts) == 0 {
		o.Modes = DefaultModeConfig()
	}
	if len(o.Synth.Currencies) == 0 {
		o.Synth = config.DefaultSynthConfig()
	}
	if o.Match.FeeToleranceMinor == nil {
		o.Match = config.DefaultMatchConfig()
	}
	if len(o.Ledger.AdjustmentAccounts) == 0 {
		o.Ledger = config.DefaultLedgerConfig()
	}
	if len(o.Sources) == 0 {
		o.Sources = o.Synth.Sources
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	return o
}

// DefaultModeConfig returns the failure-mode mix shared by the CLI and gate.
func DefaultModeConfig() modes.Config {
	return modes.DefaultAllConfig(DefaultScenarioCount)
}

// Summary is one deterministic demo result. All counts are integers; the two
// ratios are the only floating-point values and are printed to four decimals.
type Summary struct {
	Seed           int64
	Days           int
	Difficulty     string
	DatasetVersion string
	Scopes         int

	Matches        int
	CorrectMatches int
	Precision      float64
	ExpectedLinks  int
	RecalledLinks  int
	Recall         float64
	PerPass        []matchscore.PassScore
	Excluded       map[string]int

	BreakTotal int
	Breaks     map[string]int
	Cases      int
	OpenCases  int
	TieOut     []TieOutSummary
}

// TieOutSummary is one statement scope's tie-out result. It deliberately omits
// the scope UUID so two runs print identical lines.
type TieOutSummary struct {
	Source           string
	Currency         string
	AsOf             string
	UnexplainedMinor int64
	Unexplained      string
}

type scopeRow struct {
	ID       string
	Source   string
	Currency string
}

// Run executes the pipeline against opts.DBURL. The database must already be
// migrated; call Reset first for the dev-only reset workflow.
func Run(ctx context.Context, opts Options) (Summary, error) {
	opts = opts.withDefaults()
	if strings.TrimSpace(opts.DBURL) == "" {
		return Summary{}, errors.New("demo: database URL is required")
	}

	dataset, truth, manifest, err := generate(opts)
	if err != nil {
		return Summary{}, err
	}
	dir, err := os.MkdirTemp("", "recon-demo-*")
	if err != nil {
		return Summary{}, fmt.Errorf("demo: create dataset directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := dataset.Write(dir); err != nil {
		return Summary{}, fmt.Errorf("demo: write dataset: %w", err)
	}

	pool, err := db.NewPool(ctx, opts.DBURL)
	if err != nil {
		return Summary{}, err
	}
	defer pool.Close()

	loadActor, err := audit.SystemActor("synth")
	if err != nil {
		return Summary{}, fmt.Errorf("demo: load actor: %w", err)
	}
	if _, err := synth.Load(ctx, synth.LoadOptions{
		Dir:     dir,
		DB:      pool,
		Clock:   opts.Clock,
		Actor:   loadActor,
		Sources: config.IngestConfig{Sources: opts.Sources},
		Ledger:  opts.Ledger,
	}); err != nil {
		return Summary{}, fmt.Errorf("demo: load dataset: %w", err)
	}

	scopes, err := loadScopes(ctx, pool)
	if err != nil {
		return Summary{}, err
	}
	window, err := runWindow(opts)
	if err != nil {
		return Summary{}, err
	}

	matcherActor, err := audit.SystemActor("matcher")
	if err != nil {
		return Summary{}, fmt.Errorf("demo: matcher actor: %w", err)
	}
	engine, err := match.New(match.Options{
		DB:     pool,
		Clock:  opts.Clock,
		Actor:  matcherActor,
		Config: opts.Match,
	})
	if err != nil {
		return Summary{}, err
	}
	caseActor, err := audit.SystemActor("cases")
	if err != nil {
		return Summary{}, fmt.Errorf("demo: cases actor: %w", err)
	}
	caseService, err := cases.NewService(pool, opts.Clock, caseActor)
	if err != nil {
		return Summary{}, err
	}

	for _, scope := range scopes {
		result, err := engine.Run(ctx, match.RunRequest{ScopeID: scope.ID, Window: window})
		if err != nil {
			return Summary{}, fmt.Errorf("demo: match scope %s/%s: %w", scope.Source, scope.Currency, err)
		}
		if len(result.Breaks) == 0 {
			continue
		}
		if _, err := caseService.CreateBreaks(ctx, cases.CreateBreaksRequest{
			ScopeID:    result.ScopeID,
			MatchRunID: result.RunID,
			Breaks:     result.Breaks,
		}); err != nil {
			return Summary{}, fmt.Errorf("demo: create breaks for %s/%s: %w", scope.Source, scope.Currency, err)
		}
	}

	groups, err := matchscore.LoadActiveMatches(ctx, pool, "")
	if err != nil {
		return Summary{}, err
	}
	scored := matchscore.Score(groups, truth)

	summary := Summary{
		Seed:           manifest.Seed,
		Days:           manifest.Days,
		Difficulty:     manifest.Difficulty,
		DatasetVersion: manifest.DatasetVersion,
		Scopes:         len(scopes),
		Matches:        scored.Matches,
		CorrectMatches: scored.CorrectMatches,
		Precision:      scored.Precision,
		ExpectedLinks:  scored.ExpectedLinks,
		RecalledLinks:  scored.RecalledLinks,
		Recall:         scored.Recall,
		PerPass:        orderedPasses(scored.PerPass),
		Excluded:       scored.Excluded,
	}
	if summary.Breaks, summary.BreakTotal, err = loadBreakCounts(ctx, pool); err != nil {
		return Summary{}, err
	}
	if summary.Cases, summary.OpenCases, err = loadCaseCounts(ctx, pool); err != nil {
		return Summary{}, err
	}
	if summary.TieOut, err = loadTieOut(ctx, pool, scopes); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

func generate(opts Options) (*synth.Dataset, synth.GroundTruth, synth.Manifest, error) {
	generated, err := synth.New(opts.Clock).Generate(synth.Options{
		Seed:       opts.Seed,
		Days:       opts.Days,
		Difficulty: opts.Difficulty,
		Config:     opts.Synth,
	})
	if err != nil {
		return nil, synth.GroundTruth{}, synth.Manifest{}, fmt.Errorf("demo: generate dataset: %w", err)
	}
	if opts.Modes.Total == 0 {
		return generated, generated.Truth, generated.Manifest, nil
	}
	mutated, err := modes.ApplyWithSources(generated, opts.Modes, opts.Sources)
	if err != nil {
		return nil, synth.GroundTruth{}, synth.Manifest{}, fmt.Errorf("demo: apply failure modes: %w", err)
	}
	// modes.Result exposes Files with the same paths; wrap it as a Dataset so
	// the caller has one write path.
	return &synth.Dataset{
		Manifest: mutated.Manifest.Manifest,
		Ledger:   mutated.Ledger,
		Truth:    mutated.Truth,
		Files:    mutated.Files,
	}, mutated.Truth, mutated.Manifest.Manifest, nil
}

func runWindow(opts Options) (match.Window, error) {
	base, err := time.ParseInLocation("2006-01-02", opts.Synth.BaseDate, time.UTC)
	if err != nil {
		return match.Window{}, fmt.Errorf("demo: synth base date %q: %w", opts.Synth.BaseDate, err)
	}
	return match.Window{
		From: base.AddDate(0, 0, -7),
		To:   base.AddDate(0, 0, opts.Days+60),
	}, nil
}

func loadScopes(ctx context.Context, pool *pgxpool.Pool) ([]scopeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT s.id::text, s.source, s.currency
		   FROM reconciliation_scopes s
		  ORDER BY s.source, s.currency`)
	if err != nil {
		return nil, fmt.Errorf("demo: load scopes: %w", err)
	}
	defer rows.Close()
	var scopes []scopeRow
	for rows.Next() {
		var scope scopeRow
		if err := rows.Scan(&scope.ID, &scope.Source, &scope.Currency); err != nil {
			return nil, fmt.Errorf("demo: scan scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("demo: iterate scopes: %w", err)
	}
	return scopes, nil
}

func loadBreakCounts(ctx context.Context, pool *pgxpool.Pool) (map[string]int, int, error) {
	rows, err := pool.Query(ctx, `SELECT side, count(*)::int FROM breaks GROUP BY side ORDER BY side`)
	if err != nil {
		return nil, 0, fmt.Errorf("demo: load breaks: %w", err)
	}
	defer rows.Close()
	counts := make(map[string]int)
	total := 0
	for rows.Next() {
		var side string
		var count int
		if err := rows.Scan(&side, &count); err != nil {
			return nil, 0, fmt.Errorf("demo: scan break count: %w", err)
		}
		counts[side] = count
		total += count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("demo: iterate break counts: %w", err)
	}
	return counts, total, nil
}

func loadCaseCounts(ctx context.Context, pool *pgxpool.Pool) (int, int, error) {
	var total, open int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int, count(*) FILTER (WHERE status = 'open')::int FROM cases`).
		Scan(&total, &open); err != nil {
		return 0, 0, fmt.Errorf("demo: load case counts: %w", err)
	}
	return total, open, nil
}

func loadTieOut(ctx context.Context, pool *pgxpool.Pool, scopes []scopeRow) ([]TieOutSummary, error) {
	var results []TieOutSummary
	for _, scope := range scopes {
		var asOf *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT max(as_of_date) FROM statement_balances WHERE scope_id = $1::uuid`,
			scope.ID).Scan(&asOf); err != nil {
			return nil, fmt.Errorf("demo: statement date for %s/%s: %w", scope.Source, scope.Currency, err)
		}
		if asOf == nil {
			continue
		}
		result, err := tieout.Compute(ctx, pool, scope.ID, *asOf)
		if err != nil {
			return nil, fmt.Errorf("demo: tie-out %s/%s: %w", scope.Source, scope.Currency, err)
		}
		results = append(results, TieOutSummary{
			Source:           scope.Source,
			Currency:         scope.Currency,
			AsOf:             asOf.Format("2006-01-02"),
			UnexplainedMinor: result.Unexplained.AmountMinor(),
			Unexplained:      result.Unexplained.String(),
		})
	}
	return results, nil
}

// passOrder is the stable display order for the matcher's rules.
var passOrder = []string{
	match.RuleExact,
	match.RuleReferenceTolerance,
	match.RuleCompositeBatch,
	match.RuleCompositeSubset,
	match.RuleFuzzy,
	match.RuleDuplicate,
}

func orderedPasses(perPass map[string]matchscore.PassScore) []matchscore.PassScore {
	seen := make(map[string]struct{}, len(perPass))
	out := make([]matchscore.PassScore, 0, len(perPass))
	for _, name := range passOrder {
		if pass, ok := perPass[name]; ok {
			out = append(out, pass)
			seen[name] = struct{}{}
		}
	}
	extra := make([]string, 0)
	for name := range perPass {
		if _, ok := seen[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		out = append(out, perPass[name])
	}
	return out
}

// Write prints the deterministic summary table used by `make demo-m1`.
func (s Summary) Write(out io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "M1 demo summary: seed=%d days=%d difficulty=%s dataset_version=%s\n",
		s.Seed, s.Days, s.Difficulty, s.DatasetVersion)
	fmt.Fprintf(&b, "  scopes=%d matches=%d correct=%d precision=%.4f expected_links=%d recalled_links=%d recall=%.4f\n",
		s.Scopes, s.Matches, s.CorrectMatches, s.Precision, s.ExpectedLinks, s.RecalledLinks, s.Recall)
	for _, pass := range s.PerPass {
		fmt.Fprintf(&b, "  pass %-20s matches=%-4d correct=%-4d precision=%.4f recall=%.4f\n",
			pass.Pass, pass.Matches, pass.CorrectMatches, pass.Precision, pass.Recall)
	}
	sideNames := make([]string, 0, len(s.Breaks))
	for side := range s.Breaks {
		sideNames = append(sideNames, side)
	}
	sort.Strings(sideNames)
	breakParts := make([]string, 0, len(sideNames))
	for _, side := range sideNames {
		breakParts = append(breakParts, fmt.Sprintf("%s=%d", side, s.Breaks[side]))
	}
	fmt.Fprintf(&b, "  breaks=%d (%s)\n", s.BreakTotal, strings.Join(breakParts, " "))
	fmt.Fprintf(&b, "  cases=%d open=%d\n", s.Cases, s.OpenCases)
	for _, t := range s.TieOut {
		fmt.Fprintf(&b, "  tieout %s/%s as_of=%s unexplained=%s\n",
			t.Source, t.Currency, t.AsOf, t.Unexplained)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// Reset implements the dev-only `--reset`: it drops and recreates the public
// schema and then applies every embedded migration. It is refused when
// RECON_ENV is production (case-insensitive), before any connection is made.
func Reset(ctx context.Context, dsn string) error {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RECON_ENV")), "production") {
		return ErrResetProduction
	}
	if strings.TrimSpace(dsn) == "" {
		return errors.New("demo: database URL is required for reset")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("demo: open database: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("demo: connect database: %w", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE`); err != nil {
		return fmt.Errorf("demo: drop public schema: %w", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `CREATE SCHEMA public`); err != nil {
		return fmt.Errorf("demo: create public schema: %w", err)
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("demo: configure goose: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("demo: migrate after reset: %w", err)
	}
	return nil
}
