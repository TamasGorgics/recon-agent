package match

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Audit action and entity type written once per run (M1-12).
const (
	// ActionRun is the single per-run summary audit action.
	ActionRun = "match.run"
	// EntityRun names the match_runs row the summary describes.
	EntityRun = "match_run"
)

// Errors returned by Engine.Run. They are sentinels so callers can distinguish
// a caller mistake from a database failure.
var (
	// ErrInvalidRequest means the engine was constructed without its required
	// dependencies or Run was called without a scope id.
	ErrInvalidRequest = errors.New("match: invalid request")
	// ErrInvalidWindow means the run's value-date window is missing or inverted.
	ErrInvalidWindow = errors.New("match: invalid value-date window")
	// ErrScopeNotFound means no reconciliation scope has the requested id.
	ErrScopeNotFound = errors.New("match: scope not found")
)

// Options configures an Engine. Passes is optional; nil uses DefaultPasses so
// M1-13 can append passes without changing the engine.
type Options struct {
	// DB starts the run transaction. *pgxpool.Pool satisfies it.
	DB db.Beginner
	// Clock supplies run and audit timestamps; domain code never calls time.Now.
	Clock clock.Clock
	// Actor is the typed audit actor, normally system:matcher.
	Actor audit.Actor
	// Config is the matcher configuration; a nil tolerance map selects defaults.
	Config config.MatchConfig
	// Passes overrides the ordered pass chain. Nil means DefaultPasses(Config).
	Passes []Pass
}

// RunRequest is one scope reconciliation over an inclusive value-date window.
type RunRequest struct {
	ScopeID string
	Window  Window
}

// RunStats is stored in match_runs.stats and in the per-run audit summary.
// Counts are rows considered or matched, never money. Breaks are break
// candidates (M1-14 persists them); M1-13 itself writes no breaks table rows.
type RunStats struct {
	ExternalCandidates int            `json:"external_candidates"`
	LedgerCandidates   int            `json:"ledger_candidates"`
	Matched            int            `json:"matched"`
	UnmatchedExternal  int            `json:"unmatched_external"`
	UnmatchedLedger    int            `json:"unmatched_ledger"`
	Breaks             int            `json:"breaks"`
	BreaksBySide       map[string]int `json:"breaks_by_side"`
	ByPass             map[string]int `json:"by_pass"`
}

// RunResult describes a completed run. Breaks are the deterministic break
// candidates proposed by passes 3 and 5; the caller (M1-14) inserts them.
type RunResult struct {
	RunID      string
	ScopeID    string
	ConfigHash string
	Stats      RunStats
	Breaks     []BreakCandidate
}

// Engine runs the ordered matching passes for one scope at a time. Runs are
// incremental: each run loads only records without an active match or leg, so
// re-running unchanged data inserts no new matches.
type Engine struct {
	db         db.Beginner
	clock      clock.Clock
	actor      audit.Actor
	configHash string
	passes     []Pass
}

// New validates options and builds an engine.
func New(opts Options) (*Engine, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("%w: DB is required", ErrInvalidRequest)
	}
	if opts.Clock == nil {
		return nil, fmt.Errorf("%w: Clock is required", ErrInvalidRequest)
	}
	if !opts.Actor.Valid() {
		return nil, fmt.Errorf("%w: audit actor is not valid", ErrInvalidRequest)
	}
	cfg := opts.Config
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	passes := opts.Passes
	if len(passes) == 0 {
		passes = DefaultPasses(cfg)
	}
	return &Engine{
		db:         opts.DB,
		clock:      opts.Clock,
		actor:      opts.Actor,
		configHash: ConfigHash(cfg),
		passes:     append([]Pass(nil), passes...),
	}, nil
}

// Run reconciles one scope. It records exactly one match_runs row, inserts the
// accepted match headers and legs, and appends exactly one system:matcher audit
// summary in the same transaction. A failed run rolls back completely.
func (e *Engine) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if req.ScopeID == "" {
		return RunResult{}, fmt.Errorf("%w: ScopeID is required", ErrInvalidRequest)
	}
	if !req.Window.Valid() {
		return RunResult{}, fmt.Errorf("%w: From and To are required and To must not precede From", ErrInvalidWindow)
	}

	now := e.clock.Now().UTC().Truncate(time.Microsecond)
	var result RunResult
	err := db.WithTx(ctx, e.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// Serialize runs per scope without blocking other scopes. This makes
		// concurrent incremental runs safe; the partial unique indexes remain
		// the database-level guarantee.
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, req.ScopeID); err != nil {
			return fmt.Errorf("match: lock scope %s: %w", req.ScopeID, err)
		}

		scope, err := loadScope(ctx, tx, req.ScopeID)
		if err != nil {
			return err
		}
		runID, err := insertRun(ctx, tx, scope.ID, now, e.configHash)
		if err != nil {
			return err
		}

		external, err := loadExternalCandidates(ctx, tx, scope, req.Window)
		if err != nil {
			return err
		}
		ledger, err := loadLedgerCandidates(ctx, tx, scope, req.Window)
		if err != nil {
			return err
		}

		runContext := NewRunContext(scope, req.Window, now, external, ledger)
		candidates, breaks, counts, err := EvaluateWithBreaks(ctx, runContext, e.passes)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			if err := insertCandidate(ctx, tx, scope.ID, runID, candidate); err != nil {
				return err
			}
		}

		breaksBySide := make(map[string]int)
		for _, candidate := range breaks {
			breaksBySide[string(candidate.Side)]++
		}
		stats := RunStats{
			ExternalCandidates: len(external),
			LedgerCandidates:   len(ledger),
			Matched:            len(candidates),
			UnmatchedExternal:  len(external) - len(candidates),
			UnmatchedLedger:    len(ledger) - runContext.usedLedgerCount(),
			Breaks:             len(breaks),
			BreaksBySide:       breaksBySide,
			ByPass:             counts,
		}
		if err := finishRun(ctx, tx, runID, now, stats); err != nil {
			return err
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      e.actor,
			Action:     ActionRun,
			EntityType: EntityRun,
			EntityID:   runID,
			After: map[string]any{
				"scope_id":    scope.ID,
				"status":      "succeeded",
				"config_hash": e.configHash,
				"stats":       stats,
			},
			Timestamp: now,
		}); err != nil {
			return fmt.Errorf("match: append run summary: %w", err)
		}

		result = RunResult{
			RunID:      runID,
			ScopeID:    scope.ID,
			ConfigHash: e.configHash,
			Stats:      stats,
			Breaks:     breaks,
		}
		return nil
	})
	if err != nil {
		return RunResult{}, err
	}
	return result, nil
}

// ConfigHash returns the stable SHA-256 hex digest of the matcher config. It
// records which tolerance table produced a run's matches.
func ConfigHash(cfg config.MatchConfig) string {
	cfg = cfg.WithDefaults()
	sum := sha256.Sum256([]byte(cfg.Canonical()))
	return hex.EncodeToString(sum[:])
}
