package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
)

// Audit actions and entity types written directly by the executor. The
// proposal transition and ledger posting audit rows belong to their own
// packages.
const (
	// ActionApplied is the executor's summary audit entry for a committed
	// apply. It complements, and never replaces, proposal.transition and
	// ledger.post.
	ActionApplied = "executor.applied"
	// ActionBreakResolved records the guarded break resolution.
	ActionBreakResolved = "break.resolve"
	// ActionCaseResolved records the minimal apply-time case resolution.
	ActionCaseResolved = "case.resolve"

	EntityProposal = "proposal"
	EntityBreak    = "break"
	EntityCase     = "case"
)

// Service applies approved proposals exactly once. It is safe for concurrent
// use: every operation gets its own transaction and PostgreSQL row locks plus
// the applied_proposals primary key decide races.
type Service struct {
	pool          db.Beginner
	ledger        *ledger.Service
	state         *state.Service
	clock         clock.Clock
	retry         db.RetryPolicy
	executorRunID string
	batchSize     int
	pollInterval  time.Duration
	sleep         func(ctx context.Context, d time.Duration) error
	actor         audit.Actor
}

// NewService validates opts, applies the loop defaults, copies the dependency
// set, and returns an executor service.
func NewService(opts Options) (*Service, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidOptions)
	}
	if opts.Ledger == nil {
		return nil, fmt.Errorf("%w: nil ledger service", ErrInvalidOptions)
	}
	if opts.State == nil {
		return nil, fmt.Errorf("%w: nil state service", ErrInvalidOptions)
	}
	if opts.Clock == nil {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidOptions)
	}
	runID := strings.TrimSpace(opts.ExecutorRunID)
	if runID == "" {
		return nil, fmt.Errorf("%w: executor run id is required", ErrInvalidOptions)
	}
	batchSize := opts.BatchSize
	if batchSize == 0 {
		batchSize = DefaultBatchSize
	}
	if batchSize < 0 {
		return nil, fmt.Errorf("%w: batch size must be positive", ErrInvalidOptions)
	}
	pollInterval := opts.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}
	if pollInterval < 0 {
		return nil, fmt.Errorf("%w: poll interval must be positive", ErrInvalidOptions)
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	actor, err := audit.SystemActor("executor")
	if err != nil {
		return nil, fmt.Errorf("%w: executor actor: %v", ErrInvalidOptions, err)
	}
	return &Service{
		pool:          opts.Pool,
		ledger:        opts.Ledger,
		state:         opts.State,
		clock:         opts.Clock,
		retry:         opts.Retry,
		executorRunID: runID,
		batchSize:     batchSize,
		pollInterval:  pollInterval,
		sleep:         sleep,
		actor:         actor,
	}, nil
}

// sleepContext waits for d or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
