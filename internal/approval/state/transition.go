package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Audit actions and entity types written by this package.
const (
	// ActionProposalTransition is the audit action for a status change.
	ActionProposalTransition = "proposal.transition"
	// EntityProposal is the audit entity type for a proposal.
	EntityProposal = "proposal"
)

// Proposal is the snapshot returned by Transition. It contains only the
// columns the state machine reads and writes; payload, rationale, evidence,
// policy, and state_hash are deliberately absent.
type Proposal struct {
	ID                   string
	CaseID               string
	Status               Status
	Version              int
	ExpiresAt            *time.Time
	SupersedesProposalID *string
}

// TransitionRequest is one optimistic-concurrency proposal status change.
// The caller passes the version it read; a mismatch is ErrStale and changes
// nothing. TraceID is optional and nil until M5-01/M4-11 supplies it.
type TransitionRequest struct {
	ProposalID      string
	To              Status
	ExpectedVersion int
	Actor           audit.Actor
	Reason          string
	TraceID         *string
}

// Service decides and records proposal status transitions. Transition uses
// the caller's transaction; ExpireDue opens one short transaction per row.
type Service struct {
	db          db.Beginner
	clock       clock.Clock
	expiryActor audit.Actor
}

// NewService validates its dependencies and returns a proposal state service.
// A nil database or clock is a construction error wrapping ErrInvalidRequest.
func NewService(b db.Beginner, c clock.Clock) (*Service, error) {
	if b == nil {
		return nil, fmt.Errorf("%w: nil database", ErrInvalidRequest)
	}
	if c == nil {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidRequest)
	}
	expiryActor, err := audit.SystemActor("expiry")
	if err != nil {
		return nil, fmt.Errorf("%w: expiry actor: %v", ErrInvalidRequest, err)
	}
	return &Service{db: b, clock: c, expiryActor: expiryActor}, nil
}

const proposalColumns = `id::text, case_id::text, status, version,
	expires_at, supersedes_proposal_id::text`

// Transition performs one proposal status change inside the caller's
// transaction. It locks the row, checks ExpectedVersion before any write,
// applies the guarded status/version update, and appends exactly one
// proposal.transition audit entry. It never starts, commits, or rolls back a
// transaction, so a caller rollback removes both the change and its audit row
// (FIN-30).
func (s *Service) Transition(ctx context.Context, tx pgx.Tx, req TransitionRequest) (Proposal, error) {
	if req.ProposalID == "" {
		return Proposal{}, fmt.Errorf("%w: proposal id is required", ErrInvalidRequest)
	}
	if req.ExpectedVersion < 0 {
		return Proposal{}, fmt.Errorf("%w: expected version must be non-negative", ErrInvalidRequest)
	}
	if tx == nil {
		return Proposal{}, fmt.Errorf("%w: caller transaction is required", ErrInvalidRequest)
	}
	if !req.To.Valid() {
		return Proposal{}, fmt.Errorf("%w: %q", ErrUnknownStatus, req.To)
	}
	if !req.Actor.Valid() {
		return Proposal{}, fmt.Errorf("%w: actor is not valid", ErrInvalidActor)
	}

	before, err := scanProposal(tx.QueryRow(ctx,
		`SELECT `+proposalColumns+` FROM proposals WHERE id = $1::uuid FOR UPDATE`,
		req.ProposalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrProposalNotFound
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("approval/state: lock proposal: %w", err)
	}
	if !before.Status.Valid() {
		return Proposal{}, fmt.Errorf("%w: stored status %q", ErrUnknownStatus, before.Status)
	}
	if before.Version != req.ExpectedVersion {
		return Proposal{}, fmt.Errorf("%w: have %d, caller expected %d",
			ErrStale, before.Version, req.ExpectedVersion)
	}
	// Self-transition is checked before terminal so a replayed decision gets a
	// stable no-op signal instead of an illegal/terminal classification.
	if before.Status == req.To {
		return Proposal{}, fmt.Errorf("%w: %s", ErrAlreadyInState, req.To)
	}
	if Terminal(before.Status) {
		return Proposal{}, fmt.Errorf("%w: %s", ErrTerminalState, before.Status)
	}
	edge, ok := Lookup(before.Status, req.To)
	if !ok {
		return Proposal{}, fmt.Errorf("%w: %s -> %s",
			ErrIllegalTransition, before.Status, req.To)
	}
	if !edge.AllowsActor(req.Actor.Type()) {
		return Proposal{}, fmt.Errorf("%w: %s actor on %s -> %s",
			ErrActorNotAllowed, req.Actor.Type(), before.Status, req.To)
	}
	reason := strings.TrimSpace(req.Reason)
	if err := edge.ValidateReason(reason); err != nil {
		return Proposal{}, err
	}

	updated, err := scanProposal(tx.QueryRow(ctx,
		`UPDATE proposals
		    SET status = $2, version = version + 1
		  WHERE id = $1::uuid AND version = $3
		 RETURNING `+proposalColumns,
		req.ProposalID, string(req.To), req.ExpectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrStale
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("approval/state: update proposal: %w", err)
	}

	after := map[string]any{
		"status":  string(req.To),
		"version": updated.Version,
	}
	if reason != "" {
		after["reason"] = reason
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      req.Actor,
		Action:     ActionProposalTransition,
		EntityType: EntityProposal,
		EntityID:   req.ProposalID,
		Before:     map[string]any{"status": string(before.Status), "version": before.Version},
		After:      after,
		TraceID:    req.TraceID,
		Timestamp:  now,
	}); err != nil {
		return Proposal{}, fmt.Errorf("approval/state: append transition audit: %w", err)
	}
	return updated, nil
}

func scanProposal(row pgx.Row) (Proposal, error) {
	var (
		p          Proposal
		status     string
		supersedes *string
	)
	if err := row.Scan(&p.ID, &p.CaseID, &status, &p.Version, &p.ExpiresAt, &supersedes); err != nil {
		return Proposal{}, err
	}
	p.Status = Status(status)
	p.SupersedesProposalID = supersedes
	return p, nil
}
