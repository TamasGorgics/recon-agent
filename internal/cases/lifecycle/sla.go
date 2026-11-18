package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/bizdays"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Missing-settlement SLA vocabulary (spec 5.5).
const (
	// ActionCaseSLASkippedMixedSource is written once per mixed-source case
	// that the SLA classifier cannot assign to a single source. It never
	// escalates the case and never guesses a source.
	ActionCaseSLASkippedMixedSource = "case.sla_skipped_mixed_source"

	// ReasonMissingSettlementSLA is the case.transition/escalation reason.
	ReasonMissingSettlementSLA = casestate.EscalationMissingSettlementSLA

	// defaultSLABatchSize is the FOR UPDATE SKIP LOCKED batch size.
	defaultSLABatchSize = 100
	// maxSLABatchesPerSweep is the hard per-call cap (default batch size x
	// this value). A caller that wants to drain more calls the sweep again.
	maxSLABatchesPerSweep = 10
)

// candidateStatuses are the four case statuses the SLA sweep may escalate.
// Escalated and resolved cases are deliberately excluded.
var candidateStatuses = []casestate.Status{
	casestate.StatusOpen,
	casestate.StatusInAgent,
	casestate.StatusProposalPending,
	casestate.StatusWaitingInfo,
}

// SweepResult reports one EscalateMissingSettlementSLA sweep. Processed counts
// locked candidate cases examined; Escalated counts state transitions written;
// Skipped counts processed cases left alone (mixed source, a non-ledger-only or
// missing break, an unknown calendar, or the due date not yet passed).
// MixedSource is the subset of Skipped with more than one attached source.
type SweepResult struct {
	Processed   int
	Escalated   int
	Skipped     int
	MixedSource int
	Batches     int
}

// slaBreak is the classifier projection of one attached break.
type slaBreak struct {
	status     string
	side       string
	source     string
	detectedAt time.Time
}

// EscalateMissingSettlementSLA escalates cases whose attached ledger-only
// breaks have gone missing past the configured business-day SLA (spec 5.5).
//
// The sweep selects candidates in deterministic case-ID order with FOR UPDATE
// SKIP LOCKED, in bounded batches of 100 and at most 10 batches per call. A
// candidate must be open/in_agent/proposal_pending/waiting_info, have at least
// one attached break, and every attached non-void break must be ledger_only.
// The source is derived from the breaks' reconciliation scopes; a mixed-source
// case is skipped and gets one deduplicated case.sla_skipped_mixed_source
// audit instead of an escalation.
//
// The anchor is the earliest detected_at among attached breaks, converted to
// the source timezone. At/after the source cutoff the clock starts on the next
// business day; otherwise it starts on the anchor date. The due date is N
// configured business days after that start. A case is escalated only when the
// source-local date of now is strictly after the due date.
//
// Each escalation goes through cases/state.Transition with actor
// system:case-lifecycle and reason missing_settlement_sla on the locked
// version, producing exactly one case.transition audit. A second or concurrent
// sweep sees the escalated status (or waits on the row lock) and writes
// nothing. A zero now falls back to the service clock for convenience.
func (s *Service) EscalateMissingSettlementSLA(ctx context.Context, now time.Time) (SweepResult, error) {
	if now.IsZero() {
		now = s.clock.Now()
	}
	now = now.UTC()

	var total SweepResult
	if len(s.sources) == 0 {
		return total, nil
	}
	traceID := traceIDFromContext(ctx)

	var cursor any
	for batch := 0; batch < maxSLABatchesPerSweep; batch++ {
		var batchResult SweepResult
		var selected int
		var lastID string
		err := db.WithTx(ctx, s.database, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			batchResult = SweepResult{}
			selected = 0
			lastID = ""
			rows, err := tx.Query(ctx, slaCandidateQuery, cursor, defaultSLABatchSize)
			if err != nil {
				return err
			}
			candidates := make([]slaCandidate, 0, defaultSLABatchSize)
			for rows.Next() {
				var candidate slaCandidate
				var status string
				if err := rows.Scan(&candidate.caseID, &status, &candidate.version); err != nil {
					rows.Close()
					return err
				}
				candidate.status = casestate.Status(status)
				candidates = append(candidates, candidate)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()

			for _, candidate := range candidates {
				selected++
				lastID = candidate.caseID
				batchResult.Processed++
				outcome, err := s.escalateMissingSettlementCase(ctx, tx, candidate, now, traceID, &batchResult)
				if err != nil {
					return err
				}
				if outcome {
					batchResult.Escalated++
				}
			}
			return nil
		})
		if err != nil {
			return SweepResult{}, fmt.Errorf("cases/lifecycle: missing-settlement SLA sweep: %w", err)
		}
		total.Processed += batchResult.Processed
		total.Escalated += batchResult.Escalated
		total.Skipped += batchResult.Skipped
		total.MixedSource += batchResult.MixedSource
		total.Batches++
		if selected < defaultSLABatchSize {
			break
		}
		cursor = lastID
	}
	return total, nil
}

// slaCandidate is one locked case row from the sweep query.
type slaCandidate struct {
	caseID  string
	status  casestate.Status
	version int
}

// slaCandidateQuery selects the next bounded, deterministically ordered batch.
// The classifier is applied in SQL so non-missing-settlement cases are never
// locked; the Go classifier re-checks the same rules before writing anything.
const slaCandidateQuery = `
SELECT c.id::text, c.status, c.version
  FROM cases c
 WHERE c.status IN ('open', 'in_agent', 'proposal_pending', 'waiting_info')
   AND EXISTS (
       SELECT 1 FROM case_breaks cb WHERE cb.case_id = c.id)
   AND NOT EXISTS (
       SELECT 1
         FROM case_breaks cb
         JOIN breaks b ON b.id = cb.break_id
        WHERE cb.case_id = c.id
          AND b.status <> 'void'
          AND b.side <> 'ledger_only')
   AND ($1::uuid IS NULL OR c.id > $1::uuid)
 ORDER BY c.id
 LIMIT $2
   FOR UPDATE OF c SKIP LOCKED`

// escalateMissingSettlementCase classifies one locked case and escalates it if
// the source-local date is past the due date. It returns true when a
// case.transition was written.
func (s *Service) escalateMissingSettlementCase(ctx context.Context, tx pgx.Tx,
	candidate slaCandidate, now time.Time, traceID *string, result *SweepResult) (bool, error) {
	if !isCandidateStatus(candidate.status) {
		result.Skipped++
		return false, nil
	}
	breaks, err := attachedSLABreaks(ctx, tx, candidate.caseID)
	if err != nil {
		return false, fmt.Errorf("cases/lifecycle: read SLA breaks for case %s: %w",
			candidate.caseID, err)
	}
	if len(breaks) == 0 {
		result.Skipped++
		return false, nil
	}
	source, mixed, eligible := classifySLABreaks(breaks)
	if !eligible {
		result.Skipped++
		return false, nil
	}
	if mixed {
		if err := s.appendSLAMixedSourceAudit(ctx, tx, candidate, breaks, now, traceID); err != nil {
			return false, fmt.Errorf("cases/lifecycle: mixed-source SLA audit for case %s: %w",
				candidate.caseID, err)
		}
		result.Skipped++
		result.MixedSource++
		return false, nil
	}
	calendar, ok := s.sources.Calendar(source)
	if !ok {
		// A break can reference a source that has no FIN-41 calendar (for
		// example a source removed from config). There is no safe due date, so
		// the case is left for an operator instead of guessed at.
		result.Skipped++
		return false, nil
	}
	due := slaDueDate(calendar, earliestDetectedAt(breaks), s.slaBusinessDays)
	nowLocalDate := bizdays.DateOnly(now.In(calendar.Location()))
	if !nowLocalDate.After(due) {
		result.Skipped++
		return false, nil
	}
	_, err = casestate.Transition(ctx, tx, casestate.TransitionRequest{
		CaseID:          candidate.caseID,
		To:              casestate.StatusEscalated,
		ExpectedVersion: candidate.version,
		Actor:           CaseLifecycleActor,
		Reason:          ReasonMissingSettlementSLA,
		TraceID:         traceIDValue(traceID),
		Now:             now,
	})
	if err != nil {
		// The row is locked and the version came from that lock, so these
		// errors mean the case changed outside this batch. Skip, do not fail
		// the sweep: a concurrent escalation or a no-longer-eligible case is
		// not an error.
		if errors.Is(err, casestate.ErrStale) ||
			errors.Is(err, casestate.ErrIllegalTransition) ||
			errors.Is(err, casestate.ErrAlreadyInState) ||
			errors.Is(err, casestate.ErrTerminalState) {
			result.Skipped++
			return false, nil
		}
		return false, fmt.Errorf("cases/lifecycle: escalate missing-settlement case %s: %w",
			candidate.caseID, err)
	}
	return true, nil
}

// attachedSLABreaks reads every break attached to caseID. The case row is
// already locked; break sides/scopes are immutable, and void status only
// relaxes the classifier.
func attachedSLABreaks(ctx context.Context, tx pgx.Tx, caseID string) ([]slaBreak, error) {
	rows, err := tx.Query(ctx,
		`SELECT b.status, b.side, s.source, b.detected_at
		   FROM case_breaks cb
		   JOIN breaks b ON b.id = cb.break_id
		   JOIN reconciliation_scopes s ON s.id = b.scope_id
		  WHERE cb.case_id = $1::uuid
		  ORDER BY b.id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var breaks []slaBreak
	for rows.Next() {
		var item slaBreak
		if err := rows.Scan(&item.status, &item.side, &item.source, &item.detectedAt); err != nil {
			return nil, err
		}
		breaks = append(breaks, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return breaks, nil
}

// classifySLABreaks applies the spec 5.5 classifier. mixed reports more than
// one distinct attached source; eligible is false for a case with no breaks or
// any attached non-void break whose side is not ledger_only.
func classifySLABreaks(breaks []slaBreak) (source string, mixed, eligible bool) {
	if len(breaks) == 0 {
		return "", false, false
	}
	sources := make(map[string]struct{}, len(breaks))
	for _, item := range breaks {
		sources[item.source] = struct{}{}
		if item.status != "void" && item.side != "ledger_only" {
			return "", false, false
		}
	}
	if len(sources) > 1 {
		return "", true, true
	}
	for name := range sources {
		source = name
	}
	return source, false, true
}

// earliestDetectedAt is the anchor: the earliest detected_at among all attached
// breaks, including void breaks (spec 5.5 says attached breaks, not non-void).
func earliestDetectedAt(breaks []slaBreak) time.Time {
	anchor := breaks[0].detectedAt
	for _, item := range breaks[1:] {
		if item.detectedAt.Before(anchor) {
			anchor = item.detectedAt
		}
	}
	return anchor
}

// slaDueDate computes the business-day due DATE. At/after cutoff the start is
// the next business day; otherwise it is the anchor's local date. The due date
// is N business days after that start.
func slaDueDate(calendar bizdays.Calendar, anchor time.Time, businessDays int) time.Time {
	localDate := bizdays.DateOnly(anchor.In(calendar.Location()))
	start := localDate
	if calendar.AtOrAfterCutoff(anchor) {
		start = calendar.BusinessDayOnOrAfter(localDate.AddDate(0, 0, 1))
	}
	return calendar.AddBusinessDays(start, businessDays)
}

// appendSLAMixedSourceAudit writes one case.sla_skipped_mixed_source audit per
// case. The case row is locked, so the exists check is a sufficient
// idempotency guard across repeated sweeps.
func (s *Service) appendSLAMixedSourceAudit(ctx context.Context, tx pgx.Tx,
	candidate slaCandidate, breaks []slaBreak, now time.Time, traceID *string) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM audit_log
		      WHERE action = $1 AND entity_type = $2 AND entity_id = $3)`,
		ActionCaseSLASkippedMixedSource, EntityCase, candidate.caseID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	sourceSet := make(map[string]struct{}, len(breaks))
	for _, item := range breaks {
		sourceSet[item.source] = struct{}{}
	}
	sources := make([]string, 0, len(sourceSet))
	for source := range sourceSet {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	before := map[string]any{
		"status":  string(candidate.status),
		"version": candidate.version,
	}
	after := map[string]any{
		"status":         string(candidate.status),
		"version":        candidate.version,
		"skipped_reason": "mixed_source",
		"sources":        sources,
	}
	_, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      s.lifecycleActor(),
		Action:     ActionCaseSLASkippedMixedSource,
		EntityType: EntityCase,
		EntityID:   candidate.caseID,
		Before:     before,
		After:      after,
		TraceID:    traceID,
		Timestamp:  now,
	})
	return err
}

// isCandidateStatus reports whether status is one of the four SLA candidate
// statuses.
func isCandidateStatus(status casestate.Status) bool {
	for _, candidate := range candidateStatuses {
		if status == candidate {
			return true
		}
	}
	return false
}
