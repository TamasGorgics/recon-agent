package state

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
)

// Status is one of the six case statuses from spec 3.1/8.1. The set is closed:
// Valid reports false for anything else.
type Status string

// The six case statuses in spec order.
const (
	StatusOpen            Status = "open"
	StatusInAgent         Status = "in_agent"
	StatusProposalPending Status = "proposal_pending"
	StatusWaitingInfo     Status = "waiting_info"
	StatusEscalated       Status = "escalated"
	StatusResolved        Status = "resolved"
)

var allStatuses = []Status{
	StatusOpen,
	StatusInAgent,
	StatusProposalPending,
	StatusWaitingInfo,
	StatusEscalated,
	StatusResolved,
}

// AllStatuses returns the six case statuses in spec order. The returned slice
// is a copy; callers cannot mutate the package set.
func AllStatuses() []Status {
	out := make([]Status, len(allStatuses))
	copy(out, allStatuses)
	return out
}

// Valid reports whether s is one of the six case statuses.
func (s Status) Valid() bool {
	for _, candidate := range allStatuses {
		if s == candidate {
			return true
		}
	}
	return false
}

// Audit action and entity type written by Transition. ActionCaseTransition is
// byte-identical to the existing cases.ActionCaseTransition value; keeping the
// literal here lets the state package stand alone without importing cases.
const (
	// ActionCaseTransition is the audit action for a case status change.
	ActionCaseTransition = "case.transition"
	// EntityCase is the audit entity type for a case.
	EntityCase = "case"
)

// Snapshot is the post-transition case row. It contains only the columns the
// state machine reads and writes; priority, value, currency, and break
// membership are the caller's model. UpdatedAt is the transition time because
// the cases table has no updated_at column (the returned row was just updated).
type Snapshot struct {
	CaseID           string
	Status           Status
	Version          int
	Attempts         int
	EscalationReason *string
	ResolutionType   *string
	ResolvedAt       *time.Time
	LeaseOwner       *string
	LeaseExpiresAt   *time.Time
	UpdatedAt        time.Time
}

// TransitionRequest is one optimistic-concurrency case status change. The
// caller passes the version it read; a mismatch is ErrStale and changes
// nothing.
//
// CaseID and ExpectedVersion follow the repository's existing string/int
// identifier and cases.version INT conventions rather than the spec sketch's
// uuid.UUID/int64; the canonical UUID check is still enforced. ExpectedVersion
// is non-negative: cases.CreateCase inserts version 0, so a freshly created
// case must be transitionable. Now is the caller's injected clock (FIN-43). A
// zero Now falls back to the wall clock for tests and migration callers;
// production callers should always set it.
type TransitionRequest struct {
	CaseID          string
	To              Status
	ExpectedVersion int
	Actor           string
	Reason          string
	ResolutionType  string
	TraceID         string
	Now             time.Time
}

var caseIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const snapshotColumns = `id::text, status, version, attempts, escalation_reason,
	resolved_at, resolution_type, leased_by, lease_expires_at`

// Transition performs one case status change inside the caller's transaction.
// It locks the row FOR UPDATE, validates request shape, stored status,
// self/stale/terminal/edge, actor class, reason, and resolution in the
// documented order, then applies the guarded update and exactly one
// case.transition audit entry. It never starts, commits, or rolls back a
// transaction.
func Transition(ctx context.Context, tx pgx.Tx, req TransitionRequest) (Snapshot, error) {
	if tx == nil {
		return Snapshot{}, fmt.Errorf("%w: caller transaction is required", ErrInvalidRequest)
	}
	parsed, err := parseRequest(req)
	if err != nil {
		return Snapshot{}, err
	}
	now := req.Now
	if now.IsZero() {
		//nolint:forbidigo // spec 4.1: a zero Now falls back to the wall clock; callers should inject.
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Microsecond)

	before, err := scanSnapshot(tx.QueryRow(ctx,
		`SELECT `+snapshotColumns+` FROM cases WHERE id = $1::uuid FOR UPDATE`,
		parsed.caseID), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrCaseNotFound, parsed.caseID)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("cases/state: lock case: %w", err)
	}
	change, err := decide(before, parsed)
	if err != nil {
		return Snapshot{}, err
	}

	after, err := scanSnapshot(tx.QueryRow(ctx,
		`UPDATE cases
		    SET status = $2::text,
		        version = version + 1,
		        leased_by = CASE WHEN $2 = 'in_agent' THEN leased_by ELSE NULL END,
		        lease_expires_at = CASE WHEN $2 = 'in_agent' THEN lease_expires_at ELSE NULL END,
		        escalation_reason = CASE WHEN $2 = 'escalated' THEN $3::text ELSE NULL END,
		        resolved_at = CASE WHEN $2 = 'resolved' THEN $4::timestamptz ELSE NULL END,
		        resolution_type = CASE WHEN $2 = 'resolved' THEN $5::text ELSE NULL END
		  WHERE id = $1::uuid AND version = $6
		 RETURNING `+snapshotColumns,
		parsed.caseID, string(parsed.to), change.reason, now,
		change.resolution, parsed.expectedVersion), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("%w: case %s", ErrStale, parsed.caseID)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("cases/state: update case: %w", err)
	}

	beforePayload := transitionPayload(before)
	afterPayload := transitionPayload(after)
	if change.reason != "" {
		afterPayload["reason"] = change.reason
	}
	var traceID *string
	if parsed.traceID != "" {
		traceID = &parsed.traceID
	}
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      parsed.actor,
		Action:     ActionCaseTransition,
		EntityType: EntityCase,
		EntityID:   after.CaseID,
		Before:     beforePayload,
		After:      afterPayload,
		TraceID:    traceID,
		Timestamp:  now,
	}); err != nil {
		return Snapshot{}, fmt.Errorf("cases/state: append transition audit: %w", err)
	}
	return after, nil
}

// requested is a shape-validated TransitionRequest with trimmed reason and
// resolution values.
type requested struct {
	caseID          string
	to              Status
	expectedVersion int
	actor           audit.Actor
	class           actorClass
	reason          string
	resolution      string
	traceID         string
}

// parseRequest validates the request shape before the row is read. The order
// is deterministic and documented in spec 4.8: case id, target status,
// expected version, actor syntax, then the forbidden-resolution check.
func parseRequest(req TransitionRequest) (requested, error) {
	if req.CaseID == "" {
		return requested{}, fmt.Errorf("%w: case id is required", ErrInvalidRequest)
	}
	if !caseIDPattern.MatchString(req.CaseID) {
		return requested{}, fmt.Errorf("%w: case id %q is not a canonical UUID",
			ErrInvalidRequest, req.CaseID)
	}
	if !req.To.Valid() {
		return requested{}, fmt.Errorf("%w: target status %q is not one of the six case statuses",
			ErrInvalidRequest, req.To)
	}
	if req.ExpectedVersion < 0 {
		return requested{}, fmt.Errorf("%w: expected version must be non-negative", ErrInvalidRequest)
	}
	actor, class, err := parseActor(req.Actor)
	if err != nil {
		return requested{}, err
	}
	if req.To != StatusResolved && req.ResolutionType != "" {
		return requested{}, fmt.Errorf("%w: resolution type is forbidden for target %s",
			ErrInvalidRequest, req.To)
	}
	return requested{
		caseID:          req.CaseID,
		to:              req.To,
		expectedVersion: req.ExpectedVersion,
		actor:           actor,
		class:           class,
		reason:          strings.TrimSpace(req.Reason),
		resolution:      strings.TrimSpace(req.ResolutionType),
		traceID:         req.TraceID,
	}, nil
}

// plan is the validated reason and resolution the SQL update applies.
type plan struct {
	reason     string
	resolution string
}

// decide validates a shape-valid request against the locked row in the
// deterministic order from spec 4.8: stored status, self, stale, terminal,
// edge, actor, reason, resolution.
func decide(before Snapshot, req requested) (plan, error) {
	if !before.Status.Valid() {
		return plan{}, fmt.Errorf("%w: stored status %q", ErrInvalidStatus, before.Status)
	}
	if before.Status == req.to {
		return plan{}, fmt.Errorf("%w: %s", ErrAlreadyInState, req.to)
	}
	if before.Version != req.expectedVersion {
		return plan{}, fmt.Errorf("%w: have %d, caller expected %d",
			ErrStale, before.Version, req.expectedVersion)
	}
	if before.Status == StatusResolved && req.to != StatusOpen {
		return plan{}, fmt.Errorf("%w: %s", ErrTerminalState, before.Status)
	}
	candidate, ok := lookupEdge(before.Status, req.to)
	if !ok {
		return plan{}, fmt.Errorf("%w: %s -> %s",
			ErrIllegalTransition, before.Status, req.to)
	}
	if req.class == actorClassAgent {
		return plan{}, fmt.Errorf("%w: agent actor %s", ErrActorNotAllowed, req.actor)
	}
	rule := candidate.actorFor(req.class)
	if !rule.allowed {
		return plan{}, fmt.Errorf("%w: %s actor on %s -> %s",
			ErrActorNotAllowed, req.class, before.Status, req.to)
	}
	if req.class == actorClassHuman && req.resolution == ResolutionSupersededByMatch {
		return plan{}, fmt.Errorf("%w: a human actor may not claim %s",
			ErrActorNotAllowed, ResolutionSupersededByMatch)
	}
	reason, err := rule.reason.validate(before.Status, req.to, req.reason)
	if err != nil {
		return plan{}, err
	}
	resolution := ""
	if candidate.resolutionRequired {
		resolution, err = validateResolution(req.resolution)
		if err != nil {
			return plan{}, err
		}
	}
	return plan{reason: reason, resolution: resolution}, nil
}

func scanSnapshot(row pgx.Row, updatedAt time.Time) (Snapshot, error) {
	var (
		snapshot Snapshot
		status   string
	)
	if err := row.Scan(&snapshot.CaseID, &status, &snapshot.Version,
		&snapshot.Attempts, &snapshot.EscalationReason, &snapshot.ResolvedAt,
		&snapshot.ResolutionType, &snapshot.LeaseOwner,
		&snapshot.LeaseExpiresAt); err != nil {
		return Snapshot{}, err
	}
	snapshot.Status = Status(status)
	snapshot.UpdatedAt = updatedAt
	return snapshot, nil
}

func transitionPayload(snapshot Snapshot) map[string]any {
	return map[string]any{
		"status":            string(snapshot.Status),
		"version":           snapshot.Version,
		"resolution_type":   snapshot.ResolutionType,
		"resolved_at":       snapshot.ResolvedAt,
		"escalation_reason": snapshot.EscalationReason,
	}
}
