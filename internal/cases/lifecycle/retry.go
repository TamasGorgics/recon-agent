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
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Retry and expiry vocabulary (spec 5.3).
const (
	// ActionCaseRetrySkipped is written once per (case, proposal) when the hook
	// finds a case in a status other than proposal_pending, open, or escalated.
	ActionCaseRetrySkipped = "case.retry_skipped"
	// ReasonProposalRejected is the case.transition reason for a rejected
	// proposal that still has attempts left.
	ReasonProposalRejected = "proposal_rejected"
	// ReasonProposalExpired is the case.transition reason for an expired
	// proposal that still has attempts left.
	ReasonProposalExpired = "proposal_expired"
	// ReasonAttemptsExhausted is the shared attempt-cap escalation reason. It
	// is the state authority's constant so the agent and proposal paths cannot
	// drift.
	ReasonAttemptsExhausted = casestate.EscalationAttemptsExhausted

	// proposalStatusRejected and proposalStatusExpired are the two closed M4-03
	// proposal statuses that trigger a case retry.
	proposalStatusRejected = "rejected"
	proposalStatusExpired  = "expired"
)

// caseLifecycleActorID is the system component id for lifecycle-owned case
// writes.
const caseLifecycleActorID = "case-lifecycle"

// CaseLifecycleActor is the canonical system actor for lifecycle-owned case
// writes. It is used by retry/expiry and waiting-info re-entry; late match
// keeps its own caller-supplied actor.
const CaseLifecycleActor = "system:" + caseLifecycleActorID

// AttemptCap is the existing shared case attempt cap (cases.DefaultMaxAttempts,
// also used by agent failures and lease reclamation). M4-10 deliberately adds
// no second counter and no config: the retry path and the agent-failure path
// count into the same cases.attempts column. The equality is asserted by a
// lifecycle test so the two constants cannot drift silently.
const AttemptCap = 3

// Sweep batch bounds for RetryStalledProposals. One call processes at most one
// bounded batch; callers that want to drain the backlog call the sweep again.
const (
	// DefaultRetryStalledLimit is the batch size when the caller passes a
	// non-positive limit.
	DefaultRetryStalledLimit = 100
	// MaxRetryStalledLimit caps a caller-supplied batch size.
	MaxRetryStalledLimit = 1000
)

// RetryStalledResult reports one RetryStalledProposals batch.
type RetryStalledResult struct {
	// Processed is the number of proposals the batch locked and examined.
	Processed int
	// Retried is the number of cases reopened with proposal_rejected or
	// proposal_expired.
	Retried int
	// Escalated is the number of cases escalated with attempts_exhausted.
	Escalated int
	// Skipped is the number of cases that were no-ops: already open/escalated,
	// or a status that received one case.retry_skipped audit.
	Skipped int
}

// proposalCloseOutcome is the internal per-proposal result the sweep
// aggregates. The exported hook signature stays error-only.
type proposalCloseOutcome int

const (
	// closeIgnored means the supplied proposal status is not rejected/expired.
	closeIgnored proposalCloseOutcome = iota
	// closeNoop means the case was already open or escalated.
	closeNoop
	// closeSkipped means the case had an unrelated status and one
	// case.retry_skipped audit was considered/written.
	closeSkipped
	// closeRetried means the case was reopened with attempts still remaining.
	closeRetried
	// closeEscalated means the case reached the attempt cap and was escalated.
	closeEscalated
)

// OnProposalClosed applies the spec 5.3 retry/expiry rule for one proposal in
// the caller's transaction. It is a no-op for every proposal status other than
// rejected and expired. The caller owns the proposal row lock (approval Decide
// locks the proposal before its transition; RetryStalledProposals locks it with
// SKIP LOCKED). This method never starts, commits, or rolls back a transaction.
//
// The case is locked FOR UPDATE. Already open or escalated is an idempotent
// no-op: repeated hooks for the same proposal therefore never increment
// attempts twice. Any other status is a no-op with exactly one
// case.retry_skipped audit per (case, proposal), so a late match's authoritative
// resolution is never overwritten. For proposal_pending, attempts increments
// exactly once and the status moves to open (next < cap) or escalated
// (next >= cap) through the state authority in the same transaction; the
// attempts update does not bump version a second time.
//
// reason is the proposal's own rejection/expiry reason. It is audit context
// only: the case.transition reason is derived from proposalStatus so callers
// cannot mislabel the retry.
func (s *Service) OnProposalClosed(ctx context.Context, tx pgx.Tx,
	proposalID, proposalStatus, reason string) error {
	_, err := s.closeProposal(ctx, tx, proposalID, proposalStatus, reason)
	return err
}

// closeProposal is OnProposalClosed with the sweep's outcome classification.
func (s *Service) closeProposal(ctx context.Context, tx pgx.Tx,
	proposalID, proposalStatus, reason string) (proposalCloseOutcome, error) {
	closedStatus, ok := retryableProposalStatus(proposalStatus)
	if !ok {
		return closeIgnored, nil
	}
	if isNilValue(tx) {
		return closeIgnored, ErrNoTransaction
	}
	if !uuidPattern.MatchString(proposalID) {
		return closeIgnored, fmt.Errorf("%w: proposal id %q is not a canonical UUID",
			ErrInvalidRequest, proposalID)
	}
	canonicalProposalID := canonicalUUID(proposalID)

	var caseID string
	err := tx.QueryRow(ctx,
		`SELECT case_id::text FROM proposals WHERE id = $1::uuid`,
		canonicalProposalID).Scan(&caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return closeIgnored, fmt.Errorf("%w: %s", ErrProposalNotFound, canonicalProposalID)
	}
	if err != nil {
		return closeIgnored, fmt.Errorf("cases/lifecycle: read proposal %s: %w",
			canonicalProposalID, err)
	}

	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	traceID := traceIDFromContext(ctx)

	var (
		status   string
		version  int
		attempts int
	)
	err = tx.QueryRow(ctx,
		`SELECT status, version, attempts
		   FROM cases
		  WHERE id = $1::uuid
		    FOR UPDATE`, caseID).
		Scan(&status, &version, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return closeIgnored, fmt.Errorf("%w: %s", ErrCaseNotFound, caseID)
	}
	if err != nil {
		return closeIgnored, fmt.Errorf("cases/lifecycle: lock case %s: %w", caseID, err)
	}
	caseStatus := casestate.Status(status)
	if !caseStatus.Valid() {
		return closeIgnored, fmt.Errorf("%w: stored %q", ErrInvalidCaseStatus, status)
	}

	switch caseStatus {
	case casestate.StatusOpen, casestate.StatusEscalated:
		// A previous hook/sweep already applied this retry, or the case was
		// escalated for another reason. Never increment again.
		return closeNoop, nil
	case casestate.StatusProposalPending:
		// The normal retry path.
	default:
		if err := s.appendRetrySkippedAudit(ctx, tx, caseID, caseStatus, version,
			attempts, canonicalProposalID, closedStatus, reason, now, traceID); err != nil {
			return closeIgnored, err
		}
		return closeSkipped, nil
	}

	next := attempts + 1
	target := casestate.StatusOpen
	caseReason := ReasonProposalRejected
	if closedStatus == proposalStatusExpired {
		caseReason = ReasonProposalExpired
	}
	if next >= AttemptCap {
		target = casestate.StatusEscalated
		caseReason = ReasonAttemptsExhausted
	}

	snapshot, err := casestate.Transition(ctx, tx, casestate.TransitionRequest{
		CaseID:          caseID,
		To:              target,
		ExpectedVersion: version,
		Actor:           CaseLifecycleActor,
		Reason:          caseReason,
		TraceID:         traceIDValue(traceID),
		Now:             now,
	})
	if err != nil {
		return closeIgnored, fmt.Errorf("cases/lifecycle: retry case %s after %s proposal %s: %w",
			caseID, closedStatus, canonicalProposalID, err)
	}

	if _, err := incrementAttemptsLocked(ctx, tx, caseID, snapshot.Version); err != nil {
		return closeIgnored, err
	}
	if target == casestate.StatusEscalated {
		return closeEscalated, nil
	}
	return closeRetried, nil
}

// retryableProposalStatus normalizes the closed proposal status. Anything other
// than rejected/expired is ignored before any database access.
func retryableProposalStatus(status string) (string, bool) {
	switch trimmed := strings.TrimSpace(status); trimmed {
	case proposalStatusRejected, proposalStatusExpired:
		return trimmed, true
	default:
		return "", false
	}
}

// appendRetrySkippedAudit writes one case.retry_skipped audit per (case,
// proposal). The case lock serializes callers, so the exists check is a
// sufficient idempotency guard; repeated hooks do not append a second row.
func (s *Service) appendRetrySkippedAudit(ctx context.Context, tx pgx.Tx,
	caseID string, status casestate.Status, version, attempts int,
	proposalID, proposalStatus, proposalReason string,
	now time.Time, traceID *string) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM audit_log
		      WHERE action = $1 AND entity_type = $2 AND entity_id = $3
		        AND after->>'proposal_id' = $4)`,
		ActionCaseRetrySkipped, EntityCase, caseID, proposalID).Scan(&exists); err != nil {
		return fmt.Errorf("cases/lifecycle: check retry skip audit for case %s: %w",
			caseID, err)
	}
	if exists {
		return nil
	}
	before := map[string]any{
		"status":          string(status),
		"version":         version,
		"attempts":        attempts,
		"proposal_id":     proposalID,
		"proposal_status": string(proposalStatus),
	}
	after := map[string]any{
		"status":          string(status),
		"version":         version,
		"attempts":        attempts,
		"proposal_id":     proposalID,
		"proposal_status": string(proposalStatus),
		"skipped_reason":  "case_not_proposal_pending",
	}
	if reason := strings.TrimSpace(proposalReason); reason != "" {
		after["proposal_reason"] = reason
	}
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      s.lifecycleActor(),
		Action:     ActionCaseRetrySkipped,
		EntityType: EntityCase,
		EntityID:   caseID,
		Before:     before,
		After:      after,
		TraceID:    traceID,
		Timestamp:  now,
	}); err != nil {
		return fmt.Errorf("cases/lifecycle: append retry skip audit for case %s: %w",
			caseID, err)
	}
	return nil
}

// incrementAttemptsLocked adds one attempt under the already-held case row
// lock and the post-transition version. It is the same guarded update as
// internal/cases.incrementAttempts (that helper is package-private and the
// lifecycle package deliberately depends on the state authority, not the
// cases service); bumping version here would invalidate the transition's
// snapshot, so this update must not touch it.
func incrementAttemptsLocked(ctx context.Context, tx pgx.Tx,
	caseID string, version int) (int, error) {
	var attempts int
	err := tx.QueryRow(ctx,
		`UPDATE cases
		    SET attempts = attempts + 1
		  WHERE id = $1::uuid AND version = $2
		  RETURNING attempts`, caseID, version).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: attempts for %s", casestate.ErrStale, caseID)
	}
	if err != nil {
		return 0, fmt.Errorf("cases/lifecycle: increment attempts for %s: %w", caseID, err)
	}
	return attempts, nil
}

// RetryStalledProposals repairs rejected/expired proposals whose case is still
// proposal_pending, the fallback for a decision path that did not run
// OnProposalClosed in its transaction.
//
// One call runs one bounded batch in one transaction. The query locks
// candidate proposal rows with FOR UPDATE SKIP LOCKED in deterministic
// (created_at, id) order, is limited to the latest proposal of each case, and
// requires the case status to still be proposal_pending. Each row then reuses
// OnProposalClosed, so the sweep and the same-transaction hook produce the same
// case transition, attempts increment, and audit. Approved/auto-approved
// proposals and resolved/escalated cases are never selected, and a second
// sweep finds nothing left.
func (s *Service) RetryStalledProposals(ctx context.Context, limit int) (RetryStalledResult, error) {
	limit = retryStalledLimit(limit)
	var result RetryStalledResult
	err := db.WithTx(ctx, s.database, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		result = RetryStalledResult{}
		rows, err := tx.Query(ctx,
			`SELECT p.id::text, p.status
			   FROM proposals p
			   JOIN cases c ON c.id = p.case_id
			  WHERE p.status IN ('rejected', 'expired')
			    AND c.status = 'proposal_pending'
			    AND p.id = (SELECT latest.id
			                  FROM proposals latest
			                 WHERE latest.case_id = p.case_id
			                 ORDER BY latest.created_at DESC, latest.id DESC
			                 LIMIT 1)
			  ORDER BY p.created_at, p.id
			  LIMIT $1
			    FOR UPDATE OF p SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("cases/lifecycle: select stalled proposals: %w", err)
		}
		type stalled struct {
			id     string
			status string
		}
		stalledRows := make([]stalled, 0, limit)
		for rows.Next() {
			var row stalled
			if err := rows.Scan(&row.id, &row.status); err != nil {
				rows.Close()
				return fmt.Errorf("cases/lifecycle: scan stalled proposal: %w", err)
			}
			stalledRows = append(stalledRows, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("cases/lifecycle: iterate stalled proposals: %w", err)
		}
		rows.Close()

		for _, row := range stalledRows {
			outcome, err := s.closeProposal(ctx, tx, row.id, row.status, "")
			if err != nil {
				return fmt.Errorf("cases/lifecycle: retry stalled proposal %s: %w",
					row.id, err)
			}
			result.Processed++
			switch outcome {
			case closeRetried:
				result.Retried++
			case closeEscalated:
				result.Escalated++
			case closeNoop, closeSkipped:
				result.Skipped++
			}
		}
		return nil
	})
	if err != nil {
		return RetryStalledResult{}, err
	}
	return result, nil
}

// retryStalledLimit applies the default and hard cap to the batch size.
func retryStalledLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultRetryStalledLimit
	case limit > MaxRetryStalledLimit:
		return MaxRetryStalledLimit
	default:
		return limit
	}
}

// lifecycleActor returns the typed system actor used for lifecycle audits.
// NewService validates it at construction.
func (s *Service) lifecycleActor() audit.Actor {
	return s.actor
}
