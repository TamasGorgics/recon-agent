package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
)

// ReasonInfoRequested is the case.transition reason for a proposal that needs
// more information. It is deliberately distinct from the retry reasons so a
// waiting_info case never counts as a consumed attempt.
const ReasonInfoRequested = "info_requested"

// RequestInfo implements the request-info half of the M4-12 rejection path. It
// runs only in the caller's transaction (the approval decision transaction),
// never starts, commits, or rolls back, and is wired as
// approval.Options.InfoRequestedHook.
//
// The proposal must already be rejected by the decision that invoked this hook.
// The case is locked FOR UPDATE: proposal_pending moves to waiting_info through
// the state authority with actor system:case-lifecycle and reason
// info_requested; waiting_info is an idempotent no-op; any other case status is
// ErrIllegalTransition. The method never increments cases.attempts, never calls
// OnProposalClosed, and never escalates.
func (s *Service) RequestInfo(ctx context.Context, tx pgx.Tx, proposalID string) error {
	if isNilValue(tx) {
		return ErrNoTransaction
	}
	if !uuidPattern.MatchString(proposalID) {
		return fmt.Errorf("%w: proposal id %q is not a canonical UUID",
			ErrInvalidRequest, proposalID)
	}
	canonical := canonicalUUID(proposalID)

	var (
		caseID string
		status string
	)
	err := tx.QueryRow(ctx,
		`SELECT case_id::text, status FROM proposals WHERE id = $1::uuid`,
		canonical).Scan(&caseID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrProposalNotFound, canonical)
	}
	if err != nil {
		return fmt.Errorf("cases/lifecycle: read proposal %s: %w", canonical, err)
	}
	if strings.TrimSpace(status) != proposalStatusRejected {
		return fmt.Errorf("%w: proposal %s is %s; request-info requires rejected",
			casestate.ErrIllegalTransition, canonical, status)
	}
	if caseID == "" {
		return fmt.Errorf("%w: proposal %s has no case", ErrInvalidRequest, canonical)
	}

	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	traceID := traceIDFromContext(ctx)
	snapshot, err := lockCaseSnapshot(ctx, tx, caseID, now)
	if err != nil {
		return err
	}

	switch snapshot.Status {
	case casestate.StatusWaitingInfo:
		// A repeated hook (or an earlier request-info) already applied it.
		return nil
	case casestate.StatusProposalPending:
		if _, err := casestate.Transition(ctx, tx, casestate.TransitionRequest{
			CaseID:          caseID,
			To:              casestate.StatusWaitingInfo,
			ExpectedVersion: snapshot.Version,
			Actor:           CaseLifecycleActor,
			Reason:          ReasonInfoRequested,
			TraceID:         traceIDValue(traceID),
			Now:             now,
		}); err != nil {
			return fmt.Errorf("cases/lifecycle: request info for case %s after proposal %s: %w",
				caseID, canonical, err)
		}
		return nil
	default:
		return fmt.Errorf(
			"%w: case %s is %s; request-info requires proposal_pending or waiting_info",
			casestate.ErrIllegalTransition, caseID, snapshot.Status)
	}
}
