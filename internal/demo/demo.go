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

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	caselifecycle "github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
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

// evalOwnedSchemas lists the non-public schemas created by the eval
// migrations. DROP SCHEMA public CASCADE leaves them behind, so Reset must
// drop them explicitly before re-running migrations; otherwise the next
// migration run collides with their existing tables (M3-02 eval_truth caused
// SQLSTATE 42P07 on every reset after the first).
var evalOwnedSchemas = []string{"eval_truth"}

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

// Demo seed commands serialize on one transaction-scoped advisory lock, so two
// concurrent `recon demo ...` invocations cannot interleave their presence
// checks, seed writes, or marker diffs. lockSeedTables then serializes the seed
// against production writers on the tables it writes.
const (
	demoSeedLockClass int32 = 0x4d3505
	demoSeedLockObj   int32 = 505
)

// lockDemoSeed takes the advisory lock shared by Run, RunOps, and RunQuality.
// It must be the first statement of the outer seed transaction.
func lockDemoSeed(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
		demoSeedLockClass, demoSeedLockObj); err != nil {
		return fmt.Errorf("demo: acquire seed lock: %w", err)
	}
	return nil
}

// txBeginner adapts an already-open transaction to db.Beginner. The nested
// transaction is a pgx savepoint inside the outer seed transaction, so every
// service's own db.WithTx call joins the one transaction and nothing commits
// early. The isolation level is ignored: the outer transaction defines it.
type txBeginner struct{ tx pgx.Tx }

func (b txBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.tx.Begin(ctx)
}

func (b txBeginner) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return b.tx.Begin(ctx)
}

// seedInputs is the generated dataset shared by the three seed commands. It is
// prepared outside the transaction because it touches only a temp directory.
type seedInputs struct {
	dir      string
	truth    synth.GroundTruth
	manifest synth.Manifest
}

func prepareSeed(opts Options) (seedInputs, func(), error) {
	dataset, truth, manifest, err := generate(opts)
	if err != nil {
		return seedInputs{}, nil, err
	}
	dir, err := os.MkdirTemp("", "recon-demo-*")
	if err != nil {
		return seedInputs{}, nil, fmt.Errorf("demo: create dataset directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := dataset.Write(dir); err != nil {
		cleanup()
		return seedInputs{}, nil, fmt.Errorf("demo: write dataset: %w", err)
	}
	return seedInputs{dir: dir, truth: truth, manifest: manifest}, cleanup, nil
}

// Run executes the pipeline against opts.DBURL. The database must already be
// migrated; call Reset first for the dev-only reset workflow.
//
// The whole seed is one READ COMMITTED transaction that takes EXCLUSIVE locks
// on every seed target table before the pre-write marker snapshot. Concurrent
// writers are serialized: a writer that commits before the locks are acquired is
// visible to the checks/snapshot, and a writer that starts after the locks
// blocks until the seed commits, so it can never be adopted as demo data. A
// failure rolls back every row exactly and never runs a compensating reset, so
// untagged data committed concurrently is never destroyed.
func Run(ctx context.Context, opts Options) (Summary, error) {
	opts = opts.withDefaults()
	if strings.TrimSpace(opts.DBURL) == "" {
		return Summary{}, errors.New("demo: database URL is required")
	}

	inputs, cleanup, err := prepareSeed(opts)
	if err != nil {
		return Summary{}, err
	}
	defer cleanup()

	pool, err := db.NewPool(ctx, opts.DBURL)
	if err != nil {
		return Summary{}, err
	}
	defer pool.Close()

	var summary Summary
	if err := db.WithTx(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if err := lockDemoSeed(ctx, tx); err != nil {
				return err
			}
			if err := lockSeedTables(ctx, tx); err != nil {
				return err
			}
			var err error
			summary, err = runSeedTx(ctx, opts, tx, inputs)
			return err
		}); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

// runSeedTx runs the M1 pipeline, its counts, and the demo_metadata marker diff
// inside the caller's transaction. It never opens or commits a transaction of
// its own beyond pgx savepoints inside tx.
func runSeedTx(ctx context.Context, opts Options, tx pgx.Tx, inputs seedInputs) (Summary, error) {
	beginner := txBeginner{tx: tx}

	// Capture the M1-owned row keys before the pipeline writes, so only rows
	// created by this run are adopted into demo_metadata. The EXCLUSIVE table
	// locks are already held, so no other session can commit a row into these
	// tables until this transaction ends; a concurrent real row can never be
	// adopted.
	baseline, err := snapshotDemoRows(ctx, tx)
	if err != nil {
		return Summary{}, err
	}

	loadActor, err := audit.SystemActor("synth")
	if err != nil {
		return Summary{}, fmt.Errorf("demo: load actor: %w", err)
	}
	if _, err := synth.Load(ctx, synth.LoadOptions{
		Dir:     inputs.dir,
		DB:      beginner,
		Clock:   opts.Clock,
		Actor:   loadActor,
		Sources: config.IngestConfig{Sources: opts.Sources},
		Ledger:  opts.Ledger,
	}); err != nil {
		return Summary{}, fmt.Errorf("demo: load dataset: %w", err)
	}

	scopes, err := loadScopes(ctx, tx)
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
	lifecycleService, err := caselifecycle.NewService(caselifecycle.Options{
		DB:    beginner,
		Clock: opts.Clock,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("demo: case lifecycle service: %w", err)
	}
	engine, err := match.New(match.Options{
		DB:        beginner,
		Clock:     opts.Clock,
		Actor:     matcherActor,
		Config:    opts.Match,
		LateMatch: lifecycleService.ReconcileLateMatchHook,
	})
	if err != nil {
		return Summary{}, err
	}
	caseActor, err := audit.SystemActor("cases")
	if err != nil {
		return Summary{}, fmt.Errorf("demo: cases actor: %w", err)
	}
	caseService, err := cases.NewService(beginner, opts.Clock, caseActor)
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

	groups, err := matchscore.LoadActiveMatches(ctx, tx, "")
	if err != nil {
		return Summary{}, err
	}
	scored := matchscore.Score(groups, inputs.truth)

	summary := Summary{
		Seed:           inputs.manifest.Seed,
		Days:           inputs.manifest.Days,
		Difficulty:     inputs.manifest.Difficulty,
		DatasetVersion: inputs.manifest.DatasetVersion,
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
	if summary.Breaks, summary.BreakTotal, err = loadBreakCounts(ctx, tx); err != nil {
		return Summary{}, err
	}
	if summary.Cases, summary.OpenCases, err = loadCaseCounts(ctx, tx); err != nil {
		return Summary{}, err
	}
	if summary.TieOut, err = loadTieOut(ctx, tx, scopes); err != nil {
		return Summary{}, err
	}

	batchID, err := newDemoBatchID()
	if err != nil {
		return Summary{}, err
	}
	if err := markNewDemoRows(ctx, tx, batchID, baseline); err != nil {
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

func loadScopes(ctx context.Context, q demoQuerier) ([]scopeRow, error) {
	rows, err := q.Query(ctx,
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

func loadBreakCounts(ctx context.Context, q demoQuerier) (map[string]int, int, error) {
	rows, err := q.Query(ctx, `SELECT side, count(*)::int FROM breaks GROUP BY side ORDER BY side`)
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

func loadCaseCounts(ctx context.Context, q demoQuerier) (int, int, error) {
	var total, open int
	if err := q.QueryRow(ctx,
		`SELECT count(*)::int, count(*) FILTER (WHERE status = 'open')::int FROM cases`).
		Scan(&total, &open); err != nil {
		return 0, 0, fmt.Errorf("demo: load case counts: %w", err)
	}
	return total, open, nil
}

func loadTieOut(ctx context.Context, q demoQuerier, scopes []scopeRow) ([]TieOutSummary, error) {
	var results []TieOutSummary
	for _, scope := range scopes {
		var asOf *time.Time
		if err := q.QueryRow(ctx,
			`SELECT max(as_of_date) FROM statement_balances WHERE scope_id = $1::uuid`,
			scope.ID).Scan(&asOf); err != nil {
			return nil, fmt.Errorf("demo: statement date for %s/%s: %w", scope.Source, scope.Currency, err)
		}
		if asOf == nil {
			continue
		}
		result, err := tieout.Compute(ctx, q, scope.ID, *asOf)
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
// schema and the eval-owned non-public schemas, then applies every embedded
// migration. It is refused when RECON_ENV is production (case-insensitive),
// before any connection is made.
//
// Reset is guarded: before any DDL it takes ACCESS EXCLUSIVE locks on every
// table the reset destroys and verifies (in the same transaction) that every
// row is either migration-seeded reference data or recorded in demo_metadata
// by the M1/ops/quality demo layers. Any untagged row aborts with
// *UntaggedDataError without writing. This closes the check-then-drop race:
// a concurrent insert either commits before the lock and fails the guard, or
// blocks until the schema has been dropped.
func Reset(ctx context.Context, dsn string) error {
	return reset(ctx, dsn)
}

// reset is the one reset implementation. There is deliberately no unguarded
// variant: every destructive reset must pass the all-tables guard in the same
// transaction that takes the ACCESS EXCLUSIVE locks.
func reset(ctx context.Context, dsn string) error {
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

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("demo: begin reset transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := lockResetTables(ctx, tx); err != nil {
		return err
	}
	if err := checkResetAllowed(ctx, tx); err != nil {
		return err
	}

	for _, schema := range evalOwnedSchemas {
		if _, err := tx.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			return fmt.Errorf("demo: drop %s schema: %w", schema, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE`); err != nil {
		return fmt.Errorf("demo: drop public schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA public`); err != nil {
		return fmt.Errorf("demo: create public schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("demo: commit reset: %w", err)
	}
	committed = true

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("demo: configure goose: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("demo: migrate after reset: %w", err)
	}
	return nil
}
