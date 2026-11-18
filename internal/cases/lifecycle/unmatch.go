package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
)

// Admin-unmatch audit vocabulary and bounds.
const (
	// ActionMatchUnmatch is the one match audit an admin unmatch writes.
	ActionMatchUnmatch = "match.unmatch"
	// ActionBreakUnmatched is written once for every linked break this
	// operation reopens from in_case/resolved/superseded to open.
	ActionBreakUnmatched = "break.unmatched"
	// EntityMatch is the audit entity type for a match.
	EntityMatch = "match"
	// maxUnmatchReasonBytes is the admin-unmatch reason bound after trimming.
	// It matches the case authority's free-text reason bound.
	maxUnmatchReasonBytes = 500
)

// UnmatchResult reports one successful admin unmatch. Counts refer only to
// rows this call changed; already-open breaks and cases are not counted.
type UnmatchResult struct {
	MatchID            string
	BreaksReopened     int
	CasesReopened      int
	ProposalsCancelled int
	UnmatchedAt        time.Time
}

// unmatchRequest is a shape-validated unmatch call with trimmed values.
type unmatchRequest struct {
	matchID string
	reason  string
	actor   audit.Actor
}

// ValidateUnmatchInput validates the `recon match unmatch` flag values without
// a transaction or database access. The CLI uses it to map malformed flags to
// usage errors before opening a pool; Unmatch repeats the same validation.
func ValidateUnmatchInput(matchID, reason, actor string) error {
	_, err := parseUnmatchInput(matchID, reason, actor)
	return err
}

// Unmatch performs the audited admin unmatch described in spec 5.6. It takes
// the caller's transaction and never starts, commits, or rolls back one.
//
// The match row is locked first. Missing is ErrMatchNotFound; a non-active
// match is ErrMatchNotActive before any write, so repeating a successful call
// is a typed conflict with no duplicate audit. Linked breaks come from the same
// matchLink/consumedBreakIDs projection late match uses (the schema has no
// breaks.match_id). Linked cases are the cases attached to those breaks, in
// ascending case-id order.
//
// Before the first write it refuses with ErrAdjustmentApplied when any linked
// adjustment/write_off proposal has an applied_proposals row with a non-null
// ledger_txn_id. The message names the ledger transaction and the future
// `recon reverse --txn <id>` command, which does not exist in M4-10.
//
// Lock order is match -> cases (ascending) -> proposals -> breaks. The write
// order is: mark the match unmatched (the existing trigger syncs match_legs),
// supersede active proposed/pending_approval proposals with reason
// case_reopened (cancel row + audit), reopen linked breaks, reopen linked cases
// through cases/state.Transition, then append the single match.unmatch audit.
func (s *Service) Unmatch(ctx context.Context, tx pgx.Tx,
	matchID, reason, actor string) (UnmatchResult, error) {
	if isNilValue(tx) {
		return UnmatchResult{}, ErrNoTransaction
	}
	req, err := parseUnmatchInput(matchID, reason, actor)
	if err != nil {
		return UnmatchResult{}, err
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	traceID := traceIDFromContext(ctx)

	link, err := loadActiveMatchLink(ctx, tx, req.matchID)
	if err != nil {
		return UnmatchResult{}, err
	}
	if link.Status != matchStatusActive {
		return UnmatchResult{}, fmt.Errorf("%w: match %s is %s",
			ErrMatchNotActive, req.matchID, link.Status)
	}

	breakIDs, err := consumedBreakIDs(ctx, tx, link)
	if err != nil {
		return UnmatchResult{}, err
	}
	caseIDs, err := caseIDsForBreaks(ctx, tx, breakIDs)
	if err != nil {
		return UnmatchResult{}, err
	}

	// Cases are locked before proposals and breaks. Every validation that can
	// reject the request happens before the match update below.
	cases := make([]unmatchCase, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		locked, err := lockUnmatchCase(ctx, tx, caseID)
		if err != nil {
			return UnmatchResult{}, err
		}
		cases = append(cases, locked)
	}
	if err := ensureNoAppliedAdjustment(ctx, tx, caseIDs); err != nil {
		return UnmatchResult{}, err
	}

	// Lock the active proposal for each case, if any, then the linked breaks.
	// This is the match -> cases -> proposals -> breaks acquisition order; all
	// row locks are held before the first write. SupersedeActiveProposal reuses
	// the proposal locks below.
	activeProposals := make([]bool, len(cases))
	for i := range cases {
		active, err := lockActiveProposalForCase(ctx, tx, cases[i].ID)
		if err != nil {
			return UnmatchResult{}, err
		}
		activeProposals[i] = active
	}
	breaks, err := lockLinkedBreaks(ctx, tx, breakIDs)
	if err != nil {
		return UnmatchResult{}, err
	}
	for _, linked := range breaks {
		if !validBreakStatus(linked.Status) {
			return UnmatchResult{}, fmt.Errorf("%w: break %s status %q",
				ErrInvalidBreakStatus, linked.ID, linked.Status)
		}
	}

	if err := markMatchUnmatched(ctx, tx, req, now); err != nil {
		return UnmatchResult{}, err
	}
	result := UnmatchResult{MatchID: req.matchID, UnmatchedAt: now}

	for i := range cases {
		if !activeProposals[i] {
			continue
		}
		superseded, err := s.SupersedeActiveProposal(ctx, tx, cases[i].ID, "",
			req.actor.String(), ReasonCaseReopened)
		if err != nil {
			return UnmatchResult{}, fmt.Errorf(
				"cases/lifecycle: supersede proposal for unmatch case %s: %w",
				cases[i].ID, err)
		}
		if superseded.Superseded {
			result.ProposalsCancelled++
		}
	}

	for _, linked := range breaks {
		changed, err := s.reopenLinkedBreak(ctx, tx, linked, req.matchID,
			req.actor, req.reason, now, traceID)
		if err != nil {
			return UnmatchResult{}, err
		}
		if changed {
			result.BreaksReopened++
		}
	}

	for _, locked := range cases {
		if locked.Status == casestate.StatusOpen {
			continue
		}
		if _, err := casestate.Transition(ctx, tx, casestate.TransitionRequest{
			CaseID:          locked.ID,
			To:              casestate.StatusOpen,
			ExpectedVersion: locked.Version,
			Actor:           req.actor.String(),
			Reason:          req.reason,
			TraceID:         traceIDValue(traceID),
			Now:             now,
		}); err != nil {
			return UnmatchResult{}, fmt.Errorf(
				"cases/lifecycle: reopen unmatch case %s: %w", locked.ID, err)
		}
		result.CasesReopened++
	}

	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      req.actor,
		Action:     ActionMatchUnmatch,
		EntityType: EntityMatch,
		EntityID:   req.matchID,
		Before:     map[string]any{"status": matchStatusActive},
		After: map[string]any{
			"status":              matchStatusUnmatched,
			"match_id":            req.matchID,
			"reason":              req.reason,
			"actor":               req.actor.String(),
			"breaks_reopened":     result.BreaksReopened,
			"cases_reopened":      result.CasesReopened,
			"proposals_cancelled": result.ProposalsCancelled,
			"unmatched_at":        now,
		},
		TraceID:   traceID,
		Timestamp: now,
	}); err != nil {
		return UnmatchResult{}, fmt.Errorf("cases/lifecycle: append match.unmatch audit: %w", err)
	}
	return result, nil
}

// parseUnmatchInput validates everything that does not need a database read.
// The order is match UUID, actor, then reason so the CLI's usage errors are
// deterministic.
func parseUnmatchInput(matchID, reason, actor string) (unmatchRequest, error) {
	if !uuidPattern.MatchString(matchID) {
		return unmatchRequest{}, fmt.Errorf("%w: match id %q is not a canonical UUID",
			ErrInvalidRequest, matchID)
	}
	parsedActor, err := parseUnmatchActor(actor)
	if err != nil {
		return unmatchRequest{}, err
	}
	trimmedReason, err := validateUnmatchReason(reason)
	if err != nil {
		return unmatchRequest{}, err
	}
	return unmatchRequest{
		matchID: canonicalUUID(matchID),
		reason:  trimmedReason,
		actor:   parsedActor,
	}, nil
}

// parseUnmatchActor requires human:<id>. system:* and agent:* are well-formed
// but not allowed; empty/malformed actors are ErrInvalidActor. The suffix uses
// the case authority's rules (non-empty, at most 128 bytes, valid UTF-8, no
// whitespace or control characters) so a later state transition cannot reject
// a value this service already wrote.
func parseUnmatchActor(raw string) (audit.Actor, error) {
	parsed, err := audit.ParseActor(raw)
	if err != nil {
		return audit.Actor{}, fmt.Errorf("%w: %v", ErrInvalidActor, err)
	}
	if parsed.Type() != audit.ActorHuman {
		return audit.Actor{}, fmt.Errorf("%w: unmatch requires a human actor, got %s",
			ErrActorNotAllowed, parsed)
	}
	id := parsed.ID()
	if len(id) > maxUnmatchActorIDBytes {
		return audit.Actor{}, fmt.Errorf("%w: actor id exceeds %d bytes",
			ErrInvalidActor, maxUnmatchActorIDBytes)
	}
	if !utf8.ValidString(id) {
		return audit.Actor{}, fmt.Errorf("%w: actor id is not valid UTF-8", ErrInvalidActor)
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return audit.Actor{}, fmt.Errorf(
				"%w: actor id %q contains whitespace or a control character",
				ErrInvalidActor, id)
		}
	}
	return parsed, nil
}

// validateUnmatchReason trims and bounds the reason to 1..500 bytes using the
// case authority's free-text rules, so the match column and every case audit
// carry the same value and a case transition cannot fail on it later.
func validateUnmatchReason(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: unmatch reason is required", ErrInvalidReason)
	}
	if !utf8.ValidString(trimmed) {
		return "", fmt.Errorf("%w: reason is not valid UTF-8", ErrInvalidReason)
	}
	if len(trimmed) > maxUnmatchReasonBytes {
		return "", fmt.Errorf("%w: reason exceeds %d bytes",
			ErrInvalidReason, maxUnmatchReasonBytes)
	}
	for _, r := range trimmed {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: reason contains control character U+%04X",
				ErrInvalidReason, r)
		}
	}
	return trimmed, nil
}

// unmatchCase is the case projection the unmatch transaction locks.
type unmatchCase struct {
	ID      string
	Status  casestate.Status
	Version int
}

// lockUnmatchCase locks one linked case FOR UPDATE and validates its stored
// status before the first write.
func lockUnmatchCase(ctx context.Context, tx pgx.Tx, caseID string) (unmatchCase, error) {
	locked := unmatchCase{ID: caseID}
	var status string
	err := tx.QueryRow(ctx,
		`SELECT status, version
		   FROM cases
		  WHERE id = $1::uuid
		    FOR UPDATE`, caseID).
		Scan(&status, &locked.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return unmatchCase{}, fmt.Errorf("%w: %s", ErrCaseNotFound, caseID)
	}
	if err != nil {
		return unmatchCase{}, fmt.Errorf("cases/lifecycle: lock case %s for unmatch: %w",
			caseID, err)
	}
	locked.Status = casestate.Status(status)
	if !locked.Status.Valid() {
		return unmatchCase{}, fmt.Errorf("%w: stored %q", ErrInvalidCaseStatus, status)
	}
	return locked, nil
}

// ensureNoAppliedAdjustment is the refusal check. It reads the applied ledger
// transaction for any adjustment/write_off proposal on a linked case and
// returns ErrAdjustmentApplied before the first mutation. Nil ledger_txn_id
// rows (a failed application) do not block the unmatch.
func ensureNoAppliedAdjustment(ctx context.Context, tx pgx.Tx, caseIDs []string) error {
	if len(caseIDs) == 0 {
		return nil
	}
	var (
		ledgerTxnID string
		proposalID  string
		proposalTyp string
	)
	err := tx.QueryRow(ctx,
		`SELECT ap.ledger_txn_id::text, p.id::text, p.type
		   FROM applied_proposals ap
		   JOIN proposals p ON p.id = ap.proposal_id
		  WHERE p.case_id::text = ANY($1::text[])
		    AND p.type IN ('adjustment', 'write_off')
		    AND ap.ledger_txn_id IS NOT NULL
		  ORDER BY ap.ledger_txn_id, p.id
		  LIMIT 1`, caseIDs).
		Scan(&ledgerTxnID, &proposalID, &proposalTyp)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cases/lifecycle: check applied adjustments for unmatch: %w", err)
	}
	return fmt.Errorf("%w: %s proposal %s on a linked case was applied to ledger transaction %s; reconcile that ledger transaction first with the future `recon reverse --txn %s` command (not implemented in this release), then retry the unmatch",
		ErrAdjustmentApplied, proposalTyp, proposalID, ledgerTxnID, ledgerTxnID)
}

// markMatchUnmatched performs the first write: the match status change that
// lets the existing matches_sync_leg_status trigger update match_legs.
func markMatchUnmatched(ctx context.Context, tx pgx.Tx, req unmatchRequest, now time.Time) error {
	tag, err := tx.Exec(ctx,
		`UPDATE matches
		    SET status = $2,
		        unmatched_at = $3,
		        unmatched_by = $4,
		        unmatch_reason = $5
		  WHERE id = $1::uuid
		    AND status = 'active'`,
		req.matchID, matchStatusUnmatched, now, req.actor.String(), req.reason)
	if err != nil {
		return fmt.Errorf("cases/lifecycle: mark match %s unmatched: %w", req.matchID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: match %s changed while locked", ErrMatchNotActive, req.matchID)
	}
	return nil
}

// linkedBreak is one break consumed by the match, locked in ascending id order.
type linkedBreak struct {
	ID     string
	Status string
}

// lockLinkedBreaks locks every break linked through matchLink/consumedBreakIDs
// before any break or case write.
func lockLinkedBreaks(ctx context.Context, tx pgx.Tx, breakIDs []string) ([]linkedBreak, error) {
	if len(breakIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id::text, status
		   FROM breaks
		  WHERE id::text = ANY($1::text[])
		  ORDER BY id
		    FOR UPDATE`, breakIDs)
	if err != nil {
		return nil, fmt.Errorf("cases/lifecycle: lock linked breaks for unmatch: %w", err)
	}
	defer rows.Close()

	items := make([]linkedBreak, 0, len(breakIDs))
	for rows.Next() {
		var item linkedBreak
		if err := rows.Scan(&item.ID, &item.Status); err != nil {
			return nil, fmt.Errorf("cases/lifecycle: scan linked break: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases/lifecycle: iterate linked breaks: %w", err)
	}
	if len(items) != len(breakIDs) {
		return nil, fmt.Errorf("%w: a linked break disappeared while locking",
			ErrInvalidBreakStatus)
	}
	return items, nil
}

// reopenLinkedBreak moves one in_case/resolved/superseded break to open and
// writes its one break.unmatched audit. open and void are left unchanged.
func (s *Service) reopenLinkedBreak(ctx context.Context, tx pgx.Tx,
	linked linkedBreak, matchID string, actor audit.Actor, reason string,
	now time.Time, traceID *string) (bool, error) {
	if !validBreakStatus(linked.Status) {
		return false, fmt.Errorf("%w: break %s status %q",
			ErrInvalidBreakStatus, linked.ID, linked.Status)
	}
	switch linked.Status {
	case breakStatusInCase, breakStatusResolved, breakStatusSuperseded:
		tag, err := tx.Exec(ctx,
			`UPDATE breaks
			    SET status = $2
			  WHERE id = $1::uuid
			    AND status = $3`,
			linked.ID, breakStatusOpen, linked.Status)
		if err != nil {
			return false, fmt.Errorf("cases/lifecycle: reopen break %s for unmatch: %w",
				linked.ID, err)
		}
		if tag.RowsAffected() != 1 {
			return false, fmt.Errorf("%w: break %s changed while locked",
				ErrInvalidBreakStatus, linked.ID)
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      actor,
			Action:     ActionBreakUnmatched,
			EntityType: EntityBreak,
			EntityID:   linked.ID,
			Before:     map[string]any{"status": linked.Status},
			After: map[string]any{
				"status":   breakStatusOpen,
				"match_id": matchID,
				"reason":   reason,
			},
			TraceID:   traceID,
			Timestamp: now,
		}); err != nil {
			return false, fmt.Errorf(
				"cases/lifecycle: append break.unmatched audit for %s: %w", linked.ID, err)
		}
		return true, nil
	default:
		return false, nil
	}
}

// maxUnmatchActorIDBytes mirrors the case authority's actor id bound.
const maxUnmatchActorIDBytes = 128
