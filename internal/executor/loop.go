package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// errRunStopped is an internal sentinel that makes Run return nil after a
// context cancellation observed inside a poll wait.
var errRunStopped = errors.New("executor: run stopped")

// Run polls the approved queue until ctx is cancelled.
//
// One poll reads at most BatchSize approved/auto-approved proposals in
// deterministic order, including rows whose expires_at has passed. Apply owns
// the expiry decision and records apply_failed(expired), so an expired row is
// still drained instead of staying approved forever. Every proposal is applied
// in its own transaction; a typed failure is logged and the poll continues so
// one bad proposal never stops the loop. An infrastructure error leaves the
// proposal for the next poll. When the queue is empty, or when a poll made no
// progress at all (for example rows that are not applicable), Run waits
// PollInterval through the injected Sleep. Context cancellation returns nil.
//
// Run performs no network I/O and never shares a transaction between
// proposals. Multiple Run instances may race; Apply's proposal lock, version
// check, and applied_proposals unique key decide one winner.
func (s *Service) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		ids, err := s.pollApproved(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("executor: poll approved queue failed", "error", err)
			if stop := s.wait(ctx); stop != nil {
				return runStopResult(stop)
			}
			continue
		}
		if len(ids) == 0 {
			if stop := s.wait(ctx); stop != nil {
				return runStopResult(stop)
			}
			continue
		}

		progress := false
		for _, id := range ids {
			if ctx.Err() != nil {
				return nil
			}
			result, err := s.Apply(ctx, id)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				slog.Warn("executor: apply failed; continuing", "proposal_id", id, "error", err)
				continue
			}
			progress = true
			if result.Status == StatusApplyFailed {
				slog.Warn("executor: proposal apply_failed",
					"proposal_id", id, "reason", result.FailureReason)
			}
		}
		if !progress {
			if stop := s.wait(ctx); stop != nil {
				return runStopResult(stop)
			}
		}
	}
}

// runStopResult maps a cancellation observed by wait to Run's nil return.
func runStopResult(err error) error {
	if errors.Is(err, errRunStopped) {
		return nil
	}
	return err
}

// pollApproved reads one deterministic batch of approved/auto-approved
// proposals regardless of expires_at. Expired rows are deliberately included:
// Apply owns the expiry decision and transitions them to apply_failed(expired),
// so filtering here would strand them in approved forever. The read runs in a
// short read-only transaction because db.Beginner is the only database handle
// the service contract exposes.
func (s *Service) pollApproved(ctx context.Context) ([]string, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: begin poll transaction: %w", ErrApplyUnavailable, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx,
		`SELECT id::text
		   FROM proposals
		  WHERE status IN ('approved', 'auto_approved')
		  ORDER BY expires_at NULLS LAST, created_at, id
		  LIMIT $1`,
		s.batchSize)
	if err != nil {
		return nil, fmt.Errorf("%w: query approved queue: %w", ErrApplyUnavailable, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%w: scan approved queue: %w", ErrApplyUnavailable, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate approved queue: %w", ErrApplyUnavailable, err)
	}
	return ids, nil
}

// wait sleeps for PollInterval. It returns nil to continue polling,
// errRunStopped when the context or the injected sleep was cancelled, and an
// unexpected sleep error otherwise.
func (s *Service) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errRunStopped
	}
	if err := s.sleep(ctx, s.pollInterval); err != nil {
		if ctx.Err() != nil ||
			errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			return errRunStopped
		}
		return err
	}
	return nil
}
