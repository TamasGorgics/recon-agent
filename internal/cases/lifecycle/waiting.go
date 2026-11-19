package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
)

// Waiting-info re-entry vocabulary (spec 5.4).
const (
	// ReasonHumanReply is the case.transition reason for a human answer.
	ReasonHumanReply = "human_reply"
	// ReasonNewIngest is the case.transition reason for a new ingest touching a
	// waiting_info case.
	ReasonNewIngest = "new_ingest"
	// IngestActor is the only actor allowed to resume a case through the ingest
	// hook. M4-12's external request-info transport owns human replies and must
	// pass the human actor with ReasonHumanReply.
	IngestActor = "system:ingest"
)

// ResumeWaiting returns a waiting_info case to the open queue after a human
// reply or a new ingest touches it. It is designed for the caller's
// transaction (M4-12's callback transport and the ingest attach hook) and
// never starts, commits, or rolls back a transaction.
//
// The case is locked FOR UPDATE. open is an idempotent no-op that returns the
// current snapshot and writes no audit. waiting_info transitions to open
// through the state authority, which writes the single case.transition audit.
// Any other status is ErrIllegalTransition: resolved and escalated cases are
// never auto-reopened, and in_agent/proposal_pending are not waiting states.
// Reason and actor are a closed pair: human_reply requires a human actor and
// new_ingest requires system:ingest. Repeated calls see open and therefore
// produce no duplicate audit.
func (s *Service) ResumeWaiting(ctx context.Context, tx pgx.Tx,
	caseID, actor, reason string) (casestate.Snapshot, error) {
	request, err := parseWaitingRequest(tx, caseID, actor, reason)
	if err != nil {
		return casestate.Snapshot{}, err
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	traceID := traceIDFromContext(ctx)

	snapshot, err := lockCaseSnapshot(ctx, tx, request.caseID, now)
	if err != nil {
		return casestate.Snapshot{}, err
	}
	switch snapshot.Status {
	case casestate.StatusOpen:
		return snapshot, nil
	case casestate.StatusWaitingInfo:
		updated, err := casestate.Transition(ctx, tx, casestate.TransitionRequest{
			CaseID:          request.caseID,
			To:              casestate.StatusOpen,
			ExpectedVersion: snapshot.Version,
			Actor:           request.actor.String(),
			Reason:          request.reason,
			TraceID:         traceIDValue(traceID),
			Now:             now,
		})
		if err != nil {
			return casestate.Snapshot{}, fmt.Errorf(
				"cases/lifecycle: resume waiting case %s: %w", request.caseID, err)
		}
		return updated, nil
	default:
		return casestate.Snapshot{}, fmt.Errorf(
			"%w: case %s is %s; only waiting_info can resume",
			casestate.ErrIllegalTransition, request.caseID, snapshot.Status)
	}
}

// OnIngestTouchedCase is the ingest/case-attach hook. It is invoked for every
// case whose break was part of an ingest's break-creation pass, inside that
// pass's transaction (cases.Service's CaseTouchedHook). Only a waiting_info
// case is resumed; every other status is a silent no-op so an ordinary attach
// never fails the ingest or reopens a resolved/escalated case.
//
// The lifecycle package does not import internal/cases: the hook type lives in
// the cases package and cmd/api wires this method value, which keeps the
// dependency direction one-way and avoids an import cycle.
func (s *Service) OnIngestTouchedCase(ctx context.Context, tx pgx.Tx, caseID string) error {
	if isNilValue(tx) {
		return ErrNoTransaction
	}
	if !uuidPattern.MatchString(caseID) {
		return fmt.Errorf("%w: case id %q is not a canonical UUID",
			ErrInvalidRequest, caseID)
	}
	canonical := canonicalUUID(caseID)
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	snapshot, err := lockCaseSnapshot(ctx, tx, canonical, now)
	if err != nil {
		return err
	}
	if snapshot.Status != casestate.StatusWaitingInfo {
		return nil
	}
	if _, err := s.ResumeWaiting(ctx, tx, canonical, IngestActor, ReasonNewIngest); err != nil {
		return fmt.Errorf("cases/lifecycle: ingest touched case %s: %w", canonical, err)
	}
	return nil
}

// waitingRequest is a shape-validated ResumeWaiting call.
type waitingRequest struct {
	caseID string
	actor  audit.Actor
	reason string
}

// parseWaitingRequest validates identifiers, actor, reason, and the reason/
// actor pair before any database work.
func parseWaitingRequest(tx pgx.Tx, caseID, actor, reason string) (waitingRequest, error) {
	if isNilValue(tx) {
		return waitingRequest{}, ErrNoTransaction
	}
	if !uuidPattern.MatchString(caseID) {
		return waitingRequest{}, fmt.Errorf("%w: case id %q is not a canonical UUID",
			ErrInvalidRequest, caseID)
	}
	parsedActor, err := audit.ParseActor(actor)
	if err != nil {
		return waitingRequest{}, fmt.Errorf("%w: %v", ErrInvalidActor, err)
	}
	if parsedActor.Type() == audit.ActorAgent {
		return waitingRequest{}, fmt.Errorf("%w: agent actor %s", ErrActorNotAllowed, parsedActor)
	}
	trimmedReason := strings.TrimSpace(reason)
	switch trimmedReason {
	case ReasonHumanReply:
		if parsedActor.Type() != audit.ActorHuman {
			return waitingRequest{}, fmt.Errorf(
				"%w: %s requires a human actor, got %s",
				ErrInvalidWaitingReason, ReasonHumanReply, parsedActor)
		}
	case ReasonNewIngest:
		if parsedActor.String() != IngestActor {
			return waitingRequest{}, fmt.Errorf(
				"%w: %s requires the %s actor, got %s",
				ErrInvalidWaitingReason, ReasonNewIngest, IngestActor, parsedActor)
		}
	default:
		return waitingRequest{}, fmt.Errorf(
			"%w: %q (want %s or %s)",
			ErrInvalidWaitingReason, trimmedReason, ReasonHumanReply, ReasonNewIngest)
	}
	return waitingRequest{
		caseID: canonicalUUID(caseID),
		actor:  parsedActor,
		reason: trimmedReason,
	}, nil
}

// lockCaseSnapshot locks and reads the case columns the state authority owns.
// The column list mirrors cases/state's snapshotColumns; the lifecycle package
// cannot import the unexported constant.
func lockCaseSnapshot(ctx context.Context, tx pgx.Tx, caseID string,
	now time.Time) (casestate.Snapshot, error) {
	var (
		snapshot casestate.Snapshot
		status   string
	)
	err := tx.QueryRow(ctx,
		`SELECT id::text, status, version, attempts, escalation_reason,
		        resolved_at, resolution_type, leased_by, lease_expires_at
		   FROM cases
		  WHERE id = $1::uuid
		    FOR UPDATE`, caseID).
		Scan(&snapshot.CaseID, &status, &snapshot.Version, &snapshot.Attempts,
			&snapshot.EscalationReason, &snapshot.ResolvedAt,
			&snapshot.ResolutionType, &snapshot.LeaseOwner,
			&snapshot.LeaseExpiresAt)
	if err := wrapCaseLockError(err, caseID); err != nil {
		return casestate.Snapshot{}, err
	}
	snapshot.Status = casestate.Status(status)
	if !snapshot.Status.Valid() {
		return casestate.Snapshot{}, fmt.Errorf("%w: stored %q", ErrInvalidCaseStatus, status)
	}
	snapshot.UpdatedAt = now
	return snapshot, nil
}

// wrapCaseLockError converts pgx.ErrNoRows into the case-not-found sentinel and
// annotates other errors with the case id.
func wrapCaseLockError(err error, caseID string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrCaseNotFound, caseID)
	}
	return fmt.Errorf("cases/lifecycle: lock case %s: %w", caseID, err)
}
