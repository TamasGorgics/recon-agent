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

// Late-match audit and reason vocabulary. break.superseded and
// case.late_match_skipped are lifecycle-owned actions, matching the existing
// break.create/break.in_case style.
const (
	// ActionBreakSuperseded is written once for every break this operation
	// moves from open/in_case to superseded.
	ActionBreakSuperseded = "break.superseded"
	// ActionCaseLateMatchSkipped is written when a resolved case keeps its
	// non-late-match resolution.
	ActionCaseLateMatchSkipped = "case.late_match_skipped"
	// EntityBreak is the audit entity type for a break.
	EntityBreak = "break"
	// ReasonCaseLateMatch is the case.transition reason for a full late-match
	// resolution.
	ReasonCaseLateMatch = "late_match"
)

// LateMatchCaseResult is the per-case outcome of ReconcileLateMatch. Cases are
// reported in ascending case-id order, the order in which they were locked.
type LateMatchCaseResult struct {
	CaseID string
	// Status is the stored case status after this call.
	Status string
	// BreaksSuperseded counts breaks this call changed to superseded.
	BreaksSuperseded int
	// ProposalID is the active proposal this call superseded, if any.
	ProposalID string
	// CancelInserted is true when this call inserted the approval.cancel row.
	// A duplicate idempotency key is success with false.
	CancelInserted bool
	// Resolved is true when this call resolved the case superseded_by_match.
	Resolved bool
	// Reopened is true when this call partially reopened the case to open.
	Reopened bool
	// SkippedReason is non-empty when the case was not mutated:
	// SkipAlreadySuperseded or SkipResolvedOtherResolution.
	SkippedReason string
}

// LateMatchResult summarizes one ReconcileLateMatch call. Reconciled is false
// when the match is no longer active; in that case Cases is empty and nothing
// was written.
type LateMatchResult struct {
	MatchID    string
	Reconciled bool
	Cases      []LateMatchCaseResult
}

// Skip reasons reported in LateMatchCaseResult.SkippedReason.
const (
	// SkipAlreadySuperseded means the case was already resolved by a late match;
	// the call is an idempotent no-op.
	SkipAlreadySuperseded = "already_superseded_by_match"
	// SkipResolvedOtherResolution means a human/admin resolution is
	// authoritative; the call wrote only case.late_match_skipped.
	SkipResolvedOtherResolution = "resolved_other_resolution"
)

// Break statuses from the M1-14 schema. The lifecycle package reads and writes
// the status directly; the state authorities do not own break status.
const (
	breakStatusOpen       = "open"
	breakStatusInCase     = "in_case"
	breakStatusResolved   = "resolved"
	breakStatusSuperseded = "superseded"
	breakStatusVoid       = "void"
)

// attachedBreak is one break attached to a locked case.
type attachedBreak struct {
	ID     string
	Status string
}

// lockedCase is the case row ReconcileLateMatch locks before any proposal or
// break.
type lockedCase struct {
	ID             string
	Status         casestate.Status
	Version        int
	ResolutionType *string
}

// ReconcileLateMatch reconciles the cases attached to breaks consumed by an
// active match. It is designed to run inside the matcher's transaction
// immediately after the match header and legs are inserted, and it never
// starts, commits, or rolls back a transaction.
//
// Linkage: the schema has no breaks.match_id. A break in the match's scope is
// consumed when its ledger_txn_ids overlap an active match leg OR its
// external_refs (JSON array of {"source","source_record_id"}) contain the
// match's external record. See matchLink.
//
// Lock order is match -> cases (ascending id) -> proposals -> breaks -> outbox.
// The match header is locked first; all affected cases are then locked in
// ascending id order; each case's active proposed/pending_approval proposal is
// locked before its attached breaks; the approval.cancel outbox insert happens
// after the break writes. Case resolution/reopen goes through the
// internal/cases/state authority, which re-uses the already-held case lock.
//
// Per case:
//   - resolved with superseded_by_match: idempotent skip, no writes;
//   - resolved with any other resolution: skip and append exactly one
//     case.late_match_skipped audit (deduplicated per match/case);
//   - otherwise, consumed open/in_case breaks become superseded with one
//     break.superseded audit each;
//   - when every attached break is terminal (resolved/superseded/void), the
//     case is resolved superseded_by_match with reason late_match and any
//     active proposed/pending proposal is superseded and cancelled;
//   - when some attached breaks remain active, any active proposed/pending
//     proposal is superseded and cancelled and the case is reopened to open
//     with reason partial_late_match when it is not already open. The case is
//     never resolved in this branch.
//
// approved/auto_approved proposals are never touched; the executor owns their
// revalidation. Repeated calls for the same match are idempotent by state and
// write no duplicate break/case/proposal transition, cancel row, or audit.
func (s *Service) ReconcileLateMatch(ctx context.Context, tx pgx.Tx,
	matchID, actor string) (LateMatchResult, error) {
	if isNilValue(tx) {
		return LateMatchResult{}, ErrNoTransaction
	}
	if !uuidPattern.MatchString(matchID) {
		return LateMatchResult{}, fmt.Errorf("%w: match id %q is not a canonical UUID",
			ErrInvalidRequest, matchID)
	}
	parsedActor, err := audit.ParseActor(actor)
	if err != nil {
		return LateMatchResult{}, fmt.Errorf("%w: %v", ErrInvalidActor, err)
	}
	if parsedActor.Type() != audit.ActorSystem {
		return LateMatchResult{}, fmt.Errorf("%w: late match requires a system actor, got %s",
			ErrActorNotAllowed, parsedActor)
	}
	canonicalMatchID := canonicalUUID(matchID)
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	traceID := traceIDFromContext(ctx)

	link, err := loadActiveMatchLink(ctx, tx, canonicalMatchID)
	if err != nil {
		return LateMatchResult{}, err
	}
	result := LateMatchResult{MatchID: canonicalMatchID}
	if !link.active() {
		// An unmatched match is a historical link. There is no active
		// consumption to reconcile, so this is a typed no-op.
		return result, nil
	}
	result.Reconciled = true

	consumedIDs, err := consumedBreakIDs(ctx, tx, link)
	if err != nil {
		return LateMatchResult{}, err
	}
	caseIDs, err := caseIDsForBreaks(ctx, tx, consumedIDs)
	if err != nil {
		return LateMatchResult{}, err
	}

	// Lock every affected case before touching a proposal or break. This is the
	// global lock order that lets late match, supersession, and later unmatch
	// serialize without a case<->proposal cycle.
	cases := make([]lockedCase, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		locked, err := lockLateMatchCase(ctx, tx, caseID)
		if err != nil {
			return LateMatchResult{}, err
		}
		cases = append(cases, locked)
	}

	consumed := make(map[string]struct{}, len(consumedIDs))
	for _, id := range consumedIDs {
		consumed[id] = struct{}{}
	}
	for _, locked := range cases {
		outcome, err := s.reconcileLateMatchCase(ctx, tx, link, consumed, locked,
			parsedActor, now, traceID)
		if err != nil {
			return LateMatchResult{}, err
		}
		result.Cases = append(result.Cases, outcome)
	}
	return result, nil
}

// lockLateMatchCase locks one case FOR UPDATE and returns the fields the
// state authority needs.
func lockLateMatchCase(ctx context.Context, tx pgx.Tx, caseID string) (lockedCase, error) {
	locked := lockedCase{ID: caseID}
	var status string
	err := tx.QueryRow(ctx,
		`SELECT status, version, resolution_type
		   FROM cases
		  WHERE id = $1::uuid
		    FOR UPDATE`, caseID).
		Scan(&status, &locked.Version, &locked.ResolutionType)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedCase{}, fmt.Errorf("%w: %s", ErrCaseNotFound, caseID)
	}
	if err != nil {
		return lockedCase{}, fmt.Errorf("cases/lifecycle: lock case %s: %w", caseID, err)
	}
	locked.Status = casestate.Status(status)
	if !locked.Status.Valid() {
		return lockedCase{}, fmt.Errorf("%w: stored %q", ErrInvalidCaseStatus, status)
	}
	return locked, nil
}

// reconcileLateMatchCase applies the per-case rules after all cases are locked.
func (s *Service) reconcileLateMatchCase(ctx context.Context, tx pgx.Tx,
	link matchLink, consumed map[string]struct{}, locked lockedCase,
	actor audit.Actor, now time.Time, traceID *string) (LateMatchCaseResult, error) {
	outcome := LateMatchCaseResult{CaseID: locked.ID, Status: string(locked.Status)}

	if locked.Status == casestate.StatusResolved {
		if locked.ResolutionType != nil &&
			strings.TrimSpace(*locked.ResolutionType) == casestate.ResolutionSupersededByMatch {
			outcome.SkippedReason = SkipAlreadySuperseded
			return outcome, nil
		}
		if err := s.appendLateMatchSkippedAudit(ctx, tx, locked, link.MatchID,
			actor, now, traceID); err != nil {
			return LateMatchCaseResult{}, err
		}
		outcome.SkippedReason = SkipResolvedOtherResolution
		return outcome, nil
	}

	// Proposals come before breaks in the global lock order. SupersedeActiveProposal
	// re-locks this row below; the explicit lock is what establishes the order.
	hasActiveProposal, err := lockActiveProposalForCase(ctx, tx, locked.ID)
	if err != nil {
		return LateMatchCaseResult{}, err
	}

	attached, err := lockAttachedBreaks(ctx, tx, locked.ID)
	if err != nil {
		return LateMatchCaseResult{}, err
	}
	allTerminal := true
	for i := range attached {
		item := &attached[i]
		if !validBreakStatus(item.Status) {
			return LateMatchCaseResult{}, fmt.Errorf("%w: break %s status %q",
				ErrInvalidBreakStatus, item.ID, item.Status)
		}
		if _, ok := consumed[item.ID]; ok &&
			(item.Status == breakStatusOpen || item.Status == breakStatusInCase) {
			tag, err := tx.Exec(ctx,
				`UPDATE breaks
				    SET status = $2
				  WHERE id = $1::uuid AND status IN ('open', 'in_case')`,
				item.ID, breakStatusSuperseded)
			if err != nil {
				return LateMatchCaseResult{}, fmt.Errorf(
					"cases/lifecycle: supersede break %s: %w", item.ID, err)
			}
			if tag.RowsAffected() == 1 {
				if err := s.appendBreakSupersededAudit(ctx, tx, item.ID,
					item.Status, locked.ID, link.MatchID, actor, now, traceID); err != nil {
					return LateMatchCaseResult{}, err
				}
				item.Status = breakStatusSuperseded
				outcome.BreaksSuperseded++
			}
		}
		if !terminalBreakStatus(item.Status) {
			allTerminal = false
		}
	}

	if hasActiveProposal {
		superseded, err := s.SupersedeActiveProposal(ctx, tx, locked.ID, "",
			actor.String(), ReasonLateMatch)
		if err != nil {
			return LateMatchCaseResult{}, fmt.Errorf(
				"cases/lifecycle: supersede active proposal for case %s: %w", locked.ID, err)
		}
		outcome.ProposalID = superseded.ProposalID
		outcome.CancelInserted = superseded.CancelInserted
	}

	if allTerminal {
		updated, err := casestate.Transition(ctx, tx, casestate.TransitionRequest{
			CaseID:          locked.ID,
			To:              casestate.StatusResolved,
			ExpectedVersion: locked.Version,
			Actor:           actor.String(),
			Reason:          ReasonCaseLateMatch,
			ResolutionType:  casestate.ResolutionSupersededByMatch,
			TraceID:         traceIDValue(traceID),
			Now:             now,
		})
		if err != nil {
			return LateMatchCaseResult{}, fmt.Errorf(
				"cases/lifecycle: resolve late-matched case %s: %w", locked.ID, err)
		}
		outcome.Resolved = true
		outcome.Status = string(updated.Status)
		return outcome, nil
	}

	// Partial late match (wave 3b covers the behavior tests): cancel any active
	// proposal and reopen the case for the remaining breaks. A partially
	// consumed case is never resolved here.
	if locked.Status != casestate.StatusOpen {
		updated, err := casestate.Transition(ctx, tx, casestate.TransitionRequest{
			CaseID:          locked.ID,
			To:              casestate.StatusOpen,
			ExpectedVersion: locked.Version,
			Actor:           actor.String(),
			Reason:          casestate.ReasonPartialLateMatch,
			TraceID:         traceIDValue(traceID),
			Now:             now,
		})
		if err != nil {
			return LateMatchCaseResult{}, fmt.Errorf(
				"cases/lifecycle: reopen partially late-matched case %s: %w", locked.ID, err)
		}
		outcome.Reopened = true
		outcome.Status = string(updated.Status)
	}
	return outcome, nil
}

// lockActiveProposalForCase locks the one active proposed/pending_approval
// proposal, if any, before breaks are locked. approved/auto_approved proposals
// are deliberately excluded and left for the executor.
func lockActiveProposalForCase(ctx context.Context, tx pgx.Tx, caseID string) (bool, error) {
	var proposalID string
	err := tx.QueryRow(ctx,
		`SELECT id::text
		   FROM proposals
		  WHERE case_id = $1::uuid
		    AND status IN ('proposed', 'pending_approval')
		  ORDER BY created_at ASC, id ASC
		  LIMIT 1
		    FOR UPDATE`, caseID).
		Scan(&proposalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cases/lifecycle: lock active proposal for case %s: %w",
			caseID, err)
	}
	return true, nil
}

// lockAttachedBreaks locks every break attached to the case in ascending id
// order, including already-terminal breaks so the all-terminal decision is
// made from a stable snapshot.
func lockAttachedBreaks(ctx context.Context, tx pgx.Tx, caseID string) ([]attachedBreak, error) {
	rows, err := tx.Query(ctx,
		`SELECT b.id::text, b.status
		   FROM breaks b
		   JOIN case_breaks cb ON cb.break_id = b.id
		  WHERE cb.case_id = $1::uuid
		  ORDER BY b.id
		    FOR UPDATE OF b`, caseID)
	if err != nil {
		return nil, fmt.Errorf("cases/lifecycle: lock attached breaks for case %s: %w",
			caseID, err)
	}
	defer rows.Close()

	items := make([]attachedBreak, 0)
	for rows.Next() {
		var item attachedBreak
		if err := rows.Scan(&item.ID, &item.Status); err != nil {
			return nil, fmt.Errorf("cases/lifecycle: scan attached break: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases/lifecycle: iterate attached breaks: %w", err)
	}
	return items, nil
}

// appendBreakSupersededAudit writes the one break audit for a changed break.
func (s *Service) appendBreakSupersededAudit(ctx context.Context, tx pgx.Tx,
	breakID, oldStatus, caseID, matchID string, actor audit.Actor,
	now time.Time, traceID *string) error {
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      actor,
		Action:     ActionBreakSuperseded,
		EntityType: EntityBreak,
		EntityID:   breakID,
		Before:     map[string]any{"status": oldStatus},
		After: map[string]any{
			"status":   breakStatusSuperseded,
			"case_id":  caseID,
			"match_id": matchID,
			"reason":   ReasonCaseLateMatch,
		},
		TraceID:   traceID,
		Timestamp: now,
	}); err != nil {
		return fmt.Errorf("cases/lifecycle: append break.superseded audit for %s: %w",
			breakID, err)
	}
	return nil
}

// appendLateMatchSkippedAudit writes case.late_match_skipped once per
// (case, match). The case lock serializes callers, so the exists check is a
// sufficient idempotency guard without a second side table.
func (s *Service) appendLateMatchSkippedAudit(ctx context.Context, tx pgx.Tx,
	locked lockedCase, matchID string, actor audit.Actor,
	now time.Time, traceID *string) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM audit_log
		      WHERE action = $1 AND entity_type = $2 AND entity_id = $3
		        AND after->>'match_id' = $4)`,
		ActionCaseLateMatchSkipped, EntityCase, locked.ID, matchID).Scan(&exists); err != nil {
		return fmt.Errorf("cases/lifecycle: check late-match skip audit for case %s: %w",
			locked.ID, err)
	}
	if exists {
		return nil
	}
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      actor,
		Action:     ActionCaseLateMatchSkipped,
		EntityType: EntityCase,
		EntityID:   locked.ID,
		Before: map[string]any{
			"status":          string(locked.Status),
			"version":         locked.Version,
			"resolution_type": locked.ResolutionType,
		},
		After: map[string]any{
			"status":          string(locked.Status),
			"version":         locked.Version,
			"resolution_type": locked.ResolutionType,
			"match_id":        matchID,
			"reason":          SkipResolvedOtherResolution,
		},
		TraceID:   traceID,
		Timestamp: now,
	}); err != nil {
		return fmt.Errorf("cases/lifecycle: append late-match skip audit for case %s: %w",
			locked.ID, err)
	}
	return nil
}

// validBreakStatus reports whether status is one of the five break statuses.
func validBreakStatus(status string) bool {
	switch status {
	case breakStatusOpen, breakStatusInCase, breakStatusResolved,
		breakStatusSuperseded, breakStatusVoid:
		return true
	default:
		return false
	}
}

// terminalBreakStatus reports whether a break no longer blocks case
// resolution. A void break is inert and does not block.
func terminalBreakStatus(status string) bool {
	switch status {
	case breakStatusResolved, breakStatusSuperseded, breakStatusVoid:
		return true
	default:
		return false
	}
}

// traceIDValue converts the optional trace pointer to the state authority's
// string field.
func traceIDValue(traceID *string) string {
	if traceID == nil {
		return ""
	}
	return *traceID
}

// ReconcileLateMatchHook adapts ReconcileLateMatch to match.LateMatchHook. The
// matcher passes its typed system actor, normally system:matcher, and the match
// transaction; the lifecycle result is intentionally discarded because the
// matcher only needs success/failure.
func (s *Service) ReconcileLateMatchHook(ctx context.Context, tx pgx.Tx,
	matchID, actor string) error {
	_, err := s.ReconcileLateMatch(ctx, tx, matchID, actor)
	return err
}
