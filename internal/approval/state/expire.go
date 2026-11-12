package state

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// expiryBatchLimit caps one ExpireDue call at 100 committed transitions. A
// partial call is allowed; callers may call again to drain the backlog.
const expiryBatchLimit = 100

// errNoExpiryCandidate ends the per-row loop without failing the call.
var errNoExpiryCandidate = errors.New("approval/state: no due proposal")

// skippableExpiryError reports a row that changed under the lock. Because the
// row was selected FOR UPDATE and re-checked by Transition, this is defensive:
// stop the call rather than spin on a row that did not change.
func skippableExpiryError(err error) bool {
	return errors.Is(err, ErrStale) ||
		errors.Is(err, ErrAlreadyInState) ||
		errors.Is(err, ErrTerminalState) ||
		errors.Is(err, ErrIllegalTransition)
}

// ExpireDue transitions due proposed/pending_approval proposals to expired and
// returns how many rows committed. It reads the injected clock once per call,
// uses one short transaction per row with FOR UPDATE SKIP LOCKED LIMIT 1, and
// writes the system:expiry audit entry through Transition. A second call is
// idempotent; the expires_at <= now boundary is inclusive.
//
// approved and auto_approved proposals are never expired here: M4-06 refuses
// them at apply time and records apply_failed with reason expired. expires_at
// is set by M4-04/M4-11; NULL means no expiry.
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	now := s.clock.Now().UTC()
	expired := 0
	for expired < expiryBatchLimit {
		var didExpire bool
		err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			didExpire = false
			var (
				proposalID string
				version    int
			)
			err := tx.QueryRow(ctx,
				`SELECT id::text, version
				   FROM proposals
				  WHERE status IN ('proposed', 'pending_approval')
				    AND expires_at IS NOT NULL
				    AND expires_at <= $1
				  ORDER BY expires_at, id
				  FOR UPDATE SKIP LOCKED
				  LIMIT 1`, now).Scan(&proposalID, &version)
			if errors.Is(err, pgx.ErrNoRows) {
				return errNoExpiryCandidate
			}
			if err != nil {
				return fmt.Errorf("approval/state: select due proposal: %w", err)
			}
			if _, err := s.Transition(ctx, tx, TransitionRequest{
				ProposalID:      proposalID,
				To:              StatusExpired,
				ExpectedVersion: version,
				Actor:           s.expiryActor,
				Reason:          "expired",
			}); err != nil {
				return err
			}
			didExpire = true
			return nil
		})
		switch {
		case errors.Is(err, errNoExpiryCandidate):
			return expired, nil
		case err != nil && skippableExpiryError(err):
			return expired, nil
		case err != nil:
			return expired, err
		}
		if didExpire {
			expired++
		}
	}
	return expired, nil
}
