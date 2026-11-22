package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	approvalstate "github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// Audit action, entity type, and outbox identifiers for proposal supersession.
const (
	// ActionCaseProposalSuperseded is the lifecycle audit action written in
	// addition to the proposal authority's proposal.transition row.
	ActionCaseProposalSuperseded = "case.proposal_superseded"
	// EntityCase is the audit entity type for a case.
	EntityCase = "case"
	// AggregateApproval is the outbox aggregate type for approval events.
	AggregateApproval = "approval"
	// TopicApprovalCancel asks M4-12 to cancel the proposal's external
	// approval request.
	TopicApprovalCancel = "approval.cancel"
)

// The three spec 5.1 supersede reasons. Callers select the value that matches
// the operation (replacement, late match, or admin unmatch); the service
// passes it to the proposal authority unchanged.
const (
	// ReasonNewProposal marks a newer proposal replacing the old one.
	ReasonNewProposal = "superseded_by_new_proposal"
	// ReasonLateMatch marks a late match making the proposal moot.
	ReasonLateMatch = "superseded_by_match"
	// ReasonCaseReopened marks an admin unmatch reopening the case.
	ReasonCaseReopened = "case_reopened"
)

// maxReasonRunes matches the proposal authority's superseded reason bound.
const maxReasonRunes = 2000

var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// SupersedeResult reports what SupersedeActiveProposal observed and did.
//
// Superseded is true only when this call transitioned the old active proposal.
// ProposalID is the old active proposal id when one was found; it is also set
// for the newProposalID == active no-op so a caller can tell "nothing to do"
// from "this is already my replacement". CancelInserted is true only when this
// call inserted the approval.cancel row (a duplicate idempotency key is
// success with false).
type SupersedeResult struct {
	Superseded     bool
	ProposalID     string
	CancelInserted bool
}

// cancelPayload is the deterministic approval.cancel payload. Field names and
// order are the spec 5.1 contract.
type cancelPayload struct {
	ProposalID string `json:"proposal_id"`
	CaseID     string `json:"case_id"`
	Reason     string `json:"reason"`
}

// CancelIdempotencyKey returns the stable outbox key for one proposal's
// approval cancellation.
func CancelIdempotencyKey(proposalID string) string {
	return TopicApprovalCancel + ":" + proposalID
}

// SupersedeActiveProposal transitions the case's one active
// proposed/pending_approval proposal to superseded and enqueues exactly one
// approval.cancel outbox row in the caller's transaction.
//
// It locks the case FOR UPDATE, validates the caller actor class (human or
// system; agent:* is rejected), finds the active proposal under the partial
// unique index, and leaves the case status unchanged. A resolved case is
// ErrTerminalState, except for the admin-unmatch cancel-only path
// (ReasonCaseReopened with no replacement), which must supersede the active
// proposal before reopening the case (spec 5.6). No active proposal, or a
// newProposalID equal to the active proposal, is an idempotent no-op.
// newProposalID == "" means cancel-only.
//
// The old proposal is transitioned by the M4-03 authority with the caller
// transaction and its version CAS; the authority writes the
// proposal.transition audit. This method adds one case.proposal_superseded
// lifecycle audit. The outbox insert uses ON CONFLICT (idempotency_key) DO
// NOTHING, so a retry that loses the race is success.
//
// CaseID and proposal IDs follow the repository's string identifier convention
// (the same choice internal/cases/state made); canonical UUID syntax is still
// required. The method never starts, commits, or rolls back a transaction.
func (s *Service) SupersedeActiveProposal(ctx context.Context, tx pgx.Tx,
	caseID, newProposalID, actor, reason string) (SupersedeResult, error) {
	req, err := parseSupersedeRequest(ctx, tx, caseID, newProposalID, actor, reason)
	if err != nil {
		return SupersedeResult{}, err
	}

	var status string
	err = tx.QueryRow(ctx,
		`SELECT status FROM cases WHERE id = $1::uuid FOR UPDATE`, req.caseID).
		Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return SupersedeResult{}, fmt.Errorf("%w: %s", ErrCaseNotFound, req.caseID)
	}
	if err != nil {
		return SupersedeResult{}, fmt.Errorf("cases/lifecycle: lock case %s: %w", req.caseID, err)
	}
	caseStatus := casestate.Status(status)
	if !caseStatus.Valid() {
		return SupersedeResult{}, fmt.Errorf("%w: stored %q", ErrInvalidCaseStatus, status)
	}
	if caseStatus == casestate.StatusResolved && !reopenSupersede(req) {
		return SupersedeResult{}, fmt.Errorf("%w: %s", ErrTerminalState, caseStatus)
	}

	var active struct {
		id      string
		status  approvalstate.Status
		version int
	}
	err = tx.QueryRow(ctx,
		`SELECT id::text, status, version
		   FROM proposals
		  WHERE case_id = $1::uuid
		    AND status IN ('proposed', 'pending_approval')
		  ORDER BY created_at ASC, id ASC
		  LIMIT 1
		    FOR UPDATE`, req.caseID).
		Scan(&active.id, &active.status, &active.version)
	if errors.Is(err, pgx.ErrNoRows) {
		// No active proposal: the operation already happened (or there was
		// never anything to supersede). This is the idempotent replay path.
		return SupersedeResult{}, nil
	}
	if err != nil {
		return SupersedeResult{}, fmt.Errorf("cases/lifecycle: find active proposal for case %s: %w",
			req.caseID, err)
	}
	if req.newProposalID != "" && req.newProposalID == active.id {
		return SupersedeResult{ProposalID: active.id}, nil
	}

	updated, err := s.proposals.Transition(ctx, tx, approvalstate.TransitionRequest{
		ProposalID:      active.id,
		To:              approvalstate.StatusSuperseded,
		ExpectedVersion: active.version,
		Actor:           s.proposalActor,
		Reason:          req.reason,
		TraceID:         req.traceID,
	})
	if err != nil {
		return SupersedeResult{}, fmt.Errorf("cases/lifecycle: supersede proposal %s: %w",
			active.id, err)
	}

	payload, err := json.Marshal(cancelPayload{
		ProposalID: active.id,
		CaseID:     req.caseID,
		Reason:     req.reason,
	})
	if err != nil {
		return SupersedeResult{}, fmt.Errorf("cases/lifecycle: encode approval.cancel payload: %w", err)
	}
	inserted, err := outbox.EnqueueAggregate(ctx, tx, AggregateApproval, active.id,
		TopicApprovalCancel, payload, CancelIdempotencyKey(active.id))
	if err != nil {
		return SupersedeResult{}, fmt.Errorf("cases/lifecycle: enqueue %s for %s: %w",
			TopicApprovalCancel, active.id, err)
	}

	before := map[string]any{
		"proposal_id": active.id,
		"status":      string(active.status),
		"version":     active.version,
	}
	after := map[string]any{
		"proposal_id": updated.ID,
		"status":      string(updated.Status),
		"version":     updated.Version,
		"reason":      req.reason,
	}
	if req.newProposalID == "" {
		after["new_proposal_id"] = nil
	} else {
		after["new_proposal_id"] = req.newProposalID
	}
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      req.actor,
		Action:     ActionCaseProposalSuperseded,
		EntityType: EntityCase,
		EntityID:   req.caseID,
		Before:     before,
		After:      after,
		TraceID:    req.traceID,
		Timestamp:  s.clock.Now().UTC().Truncate(time.Microsecond),
	}); err != nil {
		return SupersedeResult{}, fmt.Errorf("cases/lifecycle: append supersede audit for case %s: %w",
			req.caseID, err)
	}

	return SupersedeResult{
		Superseded:     true,
		ProposalID:     active.id,
		CancelInserted: inserted,
	}, nil
}

// reopenSupersede reports whether the call is the admin-unmatch cancel-only
// path that reopens a resolved case. It is the only case where superseding a
// proposal on a terminal case is legal: spec 5.6 requires the active proposal
// to be cancelled before the case transition to open.
func reopenSupersede(req supersedeRequest) bool {
	return req.newProposalID == "" && req.reason == ReasonCaseReopened
}

// supersedeRequest is a shape-validated call with a trimmed reason.
type supersedeRequest struct {
	caseID        string
	newProposalID string
	actor         audit.Actor
	reason        string
	traceID       *string
}

// parseSupersedeRequest validates everything that does not need a database
// read: transaction presence, canonical ids, actor class, and the reason that
// matches the replacement/cancel-only shape.
func parseSupersedeRequest(ctx context.Context, tx pgx.Tx, caseID, newProposalID, actor, reason string) (supersedeRequest, error) {
	if isNilValue(tx) {
		return supersedeRequest{}, ErrNoTransaction
	}
	if !uuidPattern.MatchString(caseID) {
		return supersedeRequest{}, fmt.Errorf("%w: case id %q is not a canonical UUID",
			ErrInvalidRequest, caseID)
	}
	if newProposalID != "" && !uuidPattern.MatchString(newProposalID) {
		return supersedeRequest{}, fmt.Errorf("%w: new proposal id %q is not a canonical UUID",
			ErrInvalidRequest, newProposalID)
	}
	parsedActor, err := audit.ParseActor(actor)
	if err != nil {
		return supersedeRequest{}, fmt.Errorf("%w: %v", ErrInvalidActor, err)
	}
	if parsedActor.Type() == audit.ActorAgent {
		return supersedeRequest{}, fmt.Errorf("%w: agent actor %s", ErrActorNotAllowed, parsedActor)
	}
	trimmedReason, err := validateSupersedeReason(reason)
	if err != nil {
		return supersedeRequest{}, err
	}
	return supersedeRequest{
		caseID:        canonicalUUID(caseID),
		newProposalID: canonicalUUID(newProposalID),
		actor:         parsedActor,
		reason:        trimmedReason,
		traceID:       traceIDFromContext(ctx),
	}, nil
}

// validateSupersedeReason trims and bounds the reason. The proposal authority's
// superseded edges take free text (it has no supersession reason enum), so the
// service does not invent a second closed set; callers use the exported
// ReasonNewProposal/ReasonLateMatch/ReasonCaseReopened values. Empty, over-long,
// and control-character reasons are rejected before any database write.
func validateSupersedeReason(reason string) (string, error) {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return "", fmt.Errorf("%w: supersede reason is required", ErrInvalidReason)
	}
	if len([]rune(trimmed)) > maxReasonRunes {
		return "", fmt.Errorf("%w: reason exceeds %d runes", ErrInvalidReason, maxReasonRunes)
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: reason contains control character U+%04X", ErrInvalidReason, r)
		}
	}
	return trimmed, nil
}

// canonicalUUID lowercases a validated UUID so the audit/outbox values are
// stable regardless of the caller's case.
func canonicalUUID(value string) string {
	return strings.ToLower(value)
}

// traceIDFromContext returns the active OTel span trace id when one is present.
// It deliberately ignores the legacy API correlation key so an inbound
// traceparent can never be persisted as an audit trace ID. The wave-2
// signature has no explicit TraceID parameter.
func traceIDFromContext(ctx context.Context) *string {
	id := telemetry.SpanTraceID(ctx)
	if id == "" {
		return nil
	}
	return &id
}
