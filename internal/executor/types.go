package executor

import (
	"context"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
)

// Loop defaults from the M4-06 specification.
const (
	// DefaultBatchSize bounds one approved-queue poll.
	DefaultBatchSize = 20
	// DefaultPollInterval is the idle wait between empty polls.
	DefaultPollInterval = 250 * time.Millisecond
)

// Result.Status values. "applied" and "apply_failed" mirror the proposal
// status the service recorded; "replayed" marks a call that observed an
// already-applied proposal and wrote nothing.
const (
	StatusApplied     = "applied"
	StatusApplyFailed = "apply_failed"
	StatusReplayed    = "replayed"
)

// FailureReason values are the exact apply_failed reason enum enforced by
// internal/approval/state.
const (
	FailureStateChanged  = "state_changed"
	FailureBreakResolved = "break_resolved"
	FailureExpired       = "expired"
)

// Options is the executor dependency set. NewService copies it, so callers may
// reuse or mutate the value after construction. Pool must connect as
// recon_executor in production; tests inject an isolated role pool.
type Options struct {
	// Pool starts the apply and failure transactions.
	Pool db.Beginner
	// Ledger posts adjustment/write-off transactions in the apply transaction.
	Ledger *ledger.Service
	// State performs the guarded proposal transitions.
	State *state.Service
	// Clock supplies every timestamp; the executor never reads the wall clock.
	Clock clock.Clock
	// Retry bounds serializable/deadlock retries. The zero value means the
	// internal/db defaults (5 attempts, full-jitter exponential backoff).
	Retry db.RetryPolicy
	// ExecutorRunID is written to applied_proposals and the outbox payload.
	// It must be stable per process and is injectable for tests.
	ExecutorRunID string
	// BatchSize is the approved-queue poll size. Zero means DefaultBatchSize.
	BatchSize int
	// PollInterval is the empty-queue wait. Zero means DefaultPollInterval.
	PollInterval time.Duration
	// Sleep is the interruptible poll wait. Nil means a context-aware timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Result is the outcome of one Apply call.
//
// A re-validation failure that the service recorded as apply_failed is a
// successful Apply: the error is nil, Status is StatusApplyFailed, and
// FailureReason is set. Pre-condition errors are returned as typed sentinels;
// infrastructure/transient database errors are returned unchanged. Both leave
// the proposal status unchanged.
type Result struct {
	ProposalID    string
	Status        string
	LedgerTxnID   string
	FailureReason string
	Replayed      bool
}
