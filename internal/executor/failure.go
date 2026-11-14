package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// revalidationFailure aborts the serializable apply transaction and asks Apply
// to record apply_failed in a new transaction. It is deliberately not a
// pgconn.PgError, so internal/db never retries it.
type revalidationFailure struct {
	reason string
	cause  error
}

func (f *revalidationFailure) Error() string {
	if f.cause == nil {
		return "executor: re-validation failed: " + f.reason
	}
	return "executor: re-validation failed: " + f.reason + ": " + f.cause.Error()
}

func (f *revalidationFailure) Unwrap() error { return f.cause }

// fail records apply_failed in a fresh transaction. It never writes a ledger
// transaction, applied row, outbox row, or break/case resolution. If another
// executor applied the proposal first, it returns the replayed result instead.
func (s *Service) fail(ctx context.Context, proposalID, reason string) (Result, error) {
	var result Result
	err := db.WithTxPolicy(ctx, s.pool, pgx.TxOptions{}, s.retry, func(ctx context.Context, tx pgx.Tx) error {
		result = Result{}

		var (
			status  string
			version int
		)
		err := tx.QueryRow(ctx,
			`SELECT status, version FROM proposals WHERE id = $1::uuid FOR UPDATE`,
			proposalID).Scan(&status, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProposalNotFound
		}
		if err != nil {
			return fmt.Errorf("executor: lock proposal for apply_failed: %w", err)
		}

		switch state.Status(status) {
		case state.StatusApplied:
			ledgerTxnID, err := loadApplied(ctx, tx, proposalID)
			if err != nil {
				return err
			}
			result = Result{
				ProposalID:  proposalID,
				Status:      StatusReplayed,
				LedgerTxnID: ledgerTxnID,
				Replayed:    true,
			}
			return nil
		case state.StatusApproved, state.StatusAutoApproved:
			// Fall through to the guarded transition.
		default:
			return fmt.Errorf("%w: status %s", ErrNotApplicable, status)
		}

		if _, err := s.state.Transition(ctx, tx, state.TransitionRequest{
			ProposalID:      proposalID,
			To:              state.StatusApplyFailed,
			ExpectedVersion: version,
			Actor:           s.actor,
			Reason:          reason,
		}); err != nil {
			return fmt.Errorf("executor: transition proposal apply_failed: %w", err)
		}
		result = Result{
			ProposalID:    proposalID,
			Status:        StatusApplyFailed,
			FailureReason: reason,
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
