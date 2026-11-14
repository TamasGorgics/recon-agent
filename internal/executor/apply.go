package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Apply applies one approved proposal exactly once.
//
// It runs the apply transaction at SERIALIZABLE with bounded retries, and a
// re-validation failure (expired, break_resolved, state_changed) is recorded
// as apply_failed in a second transaction. Recorded failures return a nil
// error with Result.Status=apply_failed; typed errors leave the proposal
// status unchanged.
func (s *Service) Apply(ctx context.Context, proposalID string) (Result, error) {
	id, err := canonicalProposalID(proposalID)
	if err != nil {
		return Result{}, err
	}

	var result Result
	err = db.WithTxPolicy(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, s.retry,
		func(ctx context.Context, tx pgx.Tx) error {
			result = Result{}
			outcome, err := s.applyTx(ctx, tx, id)
			if err != nil {
				return err
			}
			result = outcome
			return nil
		})
	if err != nil {
		var failure *revalidationFailure
		if errors.As(err, &failure) {
			slog.Warn("executor: revalidation failed",
				"proposal_id", id, "reason", failure.reason, "cause", failure.cause)
			return s.fail(ctx, id, failure.reason)
		}
		if errors.Is(err, errAppliedByAnother) {
			return s.replay(ctx, id)
		}
		return Result{}, err
	}
	return result, nil
}

// applyTx performs every write of one apply inside the caller's serializable
// transaction. It returns a *revalidationFailure to abort and record
// apply_failed in a new transaction.
func (s *Service) applyTx(ctx context.Context, tx pgx.Tx, proposalID string) (Result, error) {
	locked, err := lockProposal(ctx, tx, proposalID)
	if err != nil {
		return Result{}, err
	}
	if locked.Status == state.StatusApplied {
		ledgerTxnID, err := loadApplied(ctx, tx, proposalID)
		if err != nil {
			return Result{}, err
		}
		return Result{
			ProposalID:  proposalID,
			Status:      StatusReplayed,
			LedgerTxnID: ledgerTxnID,
			Replayed:    true,
		}, nil
	}
	if locked.Status != state.StatusApproved && locked.Status != state.StatusAutoApproved {
		return Result{}, fmt.Errorf("%w: status %s", ErrNotApplicable, locked.Status)
	}

	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	if locked.ExpiresAt != nil && !locked.ExpiresAt.After(now) {
		return Result{}, &revalidationFailure{reason: FailureExpired}
	}

	resolved, err := s.revalidate(ctx, tx, locked)
	if err != nil {
		return Result{}, err
	}

	switch locked.Type {
	case "adjustment", "write_off":
		ledgerTxnID, err := s.applyLedgerLines(ctx, tx, locked, resolved)
		if err != nil {
			return Result{}, err
		}
		if err := s.insertApplied(ctx, tx, locked.ID, ledgerTxnID, now); err != nil {
			return Result{}, err
		}
		if err := s.resolveTargets(ctx, tx, locked, resolved, now); err != nil {
			return Result{}, err
		}
		if err := s.markApplied(ctx, tx, locked, ledgerTxnID, now); err != nil {
			return Result{}, err
		}
		return Result{
			ProposalID:  proposalID,
			Status:      StatusApplied,
			LedgerTxnID: ledgerTxnID,
		}, nil
	case "link":
		if err := s.applyLink(ctx, tx, locked, resolved, now); err != nil {
			return Result{}, err
		}
		if err := s.insertApplied(ctx, tx, locked.ID, "", now); err != nil {
			return Result{}, err
		}
		if err := s.resolveTargets(ctx, tx, locked, resolved, now); err != nil {
			return Result{}, err
		}
		if err := s.markApplied(ctx, tx, locked, "", now); err != nil {
			return Result{}, err
		}
		return Result{ProposalID: proposalID, Status: StatusApplied}, nil
	case "duplicate":
		if err := s.applyDuplicate(ctx, tx, locked, resolved, now); err != nil {
			return Result{}, err
		}
		if err := s.insertApplied(ctx, tx, locked.ID, "", now); err != nil {
			return Result{}, err
		}
		if err := s.resolveTargets(ctx, tx, locked, resolved, now); err != nil {
			return Result{}, err
		}
		if err := s.markApplied(ctx, tx, locked, "", now); err != nil {
			return Result{}, err
		}
		return Result{ProposalID: proposalID, Status: StatusApplied}, nil
	default:
		// request_info and unknown types are never applied by the executor.
		// Leave the approved proposal untouched.
		return Result{}, fmt.Errorf("%w: proposal type %q", ErrNotApplicable, locked.Type)
	}
}

// revalidate loads the current case/break/evidence projection and enforces the
// proposal's assumptions. Structural resolution drift is break_resolved;
// every other hash mismatch is state_changed.
func (s *Service) revalidate(ctx context.Context, tx pgx.Tx, locked lockedProposal) (proposal.State, error) {
	evidence, err := decodeEvidence(locked.Evidence)
	if err != nil {
		return proposal.State{}, &revalidationFailure{reason: FailureStateChanged, cause: err}
	}
	resolved, err := proposal.ResolveState(ctx, tx, locked.CaseID, evidence)
	if err != nil {
		// A database failure while loading the projection is infrastructure:
		// return it unchanged so the proposal stays approved. Only a plain
		// resolution error is deterministic drift.
		return proposal.State{}, revalidationFailureFor(
			fmt.Errorf("resolve proposal state: %w", err), FailureStateChanged)
	}
	if resolved.Case.Status == "resolved" || resolved.Case.ResolutionType != "" {
		return proposal.State{}, &revalidationFailure{
			reason: FailureBreakResolved,
			cause:  fmt.Errorf("case %s is resolved (%s)", locked.CaseID, resolved.Case.ResolutionType),
		}
	}
	for _, item := range resolved.Breaks {
		if item.Status != "open" && item.Status != "in_case" {
			return proposal.State{}, &revalidationFailure{
				reason: FailureBreakResolved,
				cause:  fmt.Errorf("break %s is %s", item.ID, item.Status),
			}
		}
	}
	if len(resolved.Breaks) == 0 {
		return proposal.State{}, &revalidationFailure{
			reason: FailureBreakResolved,
			cause:  errors.New("case has no attached break"),
		}
	}
	matches, err := stateHashMatches(resolved, locked.StateHash)
	if err != nil {
		return proposal.State{}, fmt.Errorf("executor: compare state hash: %w", err)
	}
	if !matches {
		return proposal.State{}, &revalidationFailure{
			reason: FailureStateChanged,
			cause:  errors.New("proposal state_hash does not match the current state"),
		}
	}
	return resolved, nil
}

// resolveTargets resolves the attached breaks and the case with guarded
// updates inside the apply transaction. A status moved since re-validation is
// a break_resolved failure; every update writes an audit row in the same
// transaction (FIN-30).
func (s *Service) resolveTargets(ctx context.Context, tx pgx.Tx, locked lockedProposal, resolved proposal.State, now time.Time) error {
	for _, item := range resolved.Breaks {
		tag, err := tx.Exec(ctx,
			`UPDATE breaks SET status = 'resolved'
			  WHERE id = $1::uuid AND status IN ('open', 'in_case')`, item.ID)
		if err != nil {
			return fmt.Errorf("executor: resolve break %s: %w", item.ID, err)
		}
		if tag.RowsAffected() != 1 {
			return &revalidationFailure{
				reason: FailureBreakResolved,
				cause:  fmt.Errorf("break %s is no longer open or in_case", item.ID),
			}
		}
		if err := s.appendAudit(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionBreakResolved,
			EntityType: EntityBreak,
			EntityID:   item.ID,
			Before:     map[string]any{"status": item.Status},
			After: map[string]any{
				"status":      "resolved",
				"case_id":     locked.CaseID,
				"proposal_id": locked.ID,
			},
			Timestamp: now,
		}); err != nil {
			return err
		}
	}

	var (
		caseStatus     string
		caseVersion    int
		resolutionType *string
	)
	err := tx.QueryRow(ctx,
		`SELECT status, version, resolution_type FROM cases WHERE id = $1::uuid FOR UPDATE`,
		locked.CaseID).Scan(&caseStatus, &caseVersion, &resolutionType)
	if errors.Is(err, pgx.ErrNoRows) {
		return &revalidationFailure{
			reason: FailureStateChanged,
			cause:  fmt.Errorf("case %s no longer exists", locked.CaseID),
		}
	}
	if err != nil {
		return fmt.Errorf("executor: lock case %s: %w", locked.CaseID, err)
	}
	if caseStatus == "resolved" || resolutionType != nil {
		return &revalidationFailure{
			reason: FailureBreakResolved,
			cause:  fmt.Errorf("case %s is already resolved", locked.CaseID),
		}
	}
	switch caseStatus {
	case "open", "in_agent", "proposal_pending", "waiting_info":
	default:
		return &revalidationFailure{
			reason: FailureBreakResolved,
			cause:  fmt.Errorf("case %s is %s", locked.CaseID, caseStatus),
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE cases
		    SET status = 'resolved',
		        version = version + 1,
		        resolved_at = COALESCE(resolved_at, $2),
		        resolution_type = $3,
		        leased_by = NULL,
		        lease_expires_at = NULL
		  WHERE id = $1::uuid AND version = $4 AND status = $5`,
		locked.CaseID, now, locked.Type, caseVersion, caseStatus)
	if err != nil {
		return fmt.Errorf("executor: resolve case %s: %w", locked.CaseID, err)
	}
	if tag.RowsAffected() != 1 {
		return &revalidationFailure{
			reason: FailureStateChanged,
			cause:  fmt.Errorf("case %s changed while applying", locked.CaseID),
		}
	}
	return s.appendAudit(ctx, tx, audit.Entry{
		Actor:      s.actor,
		Action:     ActionCaseResolved,
		EntityType: EntityCase,
		EntityID:   locked.CaseID,
		Before:     map[string]any{"status": caseStatus, "version": caseVersion},
		After: map[string]any{
			"status":          "resolved",
			"version":         caseVersion + 1,
			"resolution_type": locked.Type,
			"proposal_id":     locked.ID,
		},
		Timestamp: now,
	})
}

// markApplied transitions the proposal, writes the executor summary audit
// entry, and enqueues the outbox row. All three share the apply transaction.
func (s *Service) markApplied(ctx context.Context, tx pgx.Tx, locked lockedProposal, ledgerTxnID string, now time.Time) error {
	if _, err := s.state.Transition(ctx, tx, state.TransitionRequest{
		ProposalID:      locked.ID,
		To:              state.StatusApplied,
		ExpectedVersion: locked.Version,
		Actor:           s.actor,
	}); err != nil {
		return fmt.Errorf("executor: transition proposal applied: %w", err)
	}

	if err := s.appendAudit(ctx, tx, audit.Entry{
		Actor:      s.actor,
		Action:     ActionApplied,
		EntityType: EntityProposal,
		EntityID:   locked.ID,
		Before:     map[string]any{"status": string(locked.Status)},
		After: map[string]any{
			"status":          "applied",
			"ledger_txn_id":   ledgerTxnID,
			"applied_at":      now.Format(time.RFC3339),
			"executor_run_id": s.executorRunID,
		},
		Timestamp: now,
	}); err != nil {
		return err
	}

	return s.enqueueApplied(ctx, tx, locked, ledgerTxnID, now)
}

// insertApplied writes the FIN-23 exactly-once record. A unique violation means
// another executor committed first; the caller re-reads in a new transaction.
func (s *Service) insertApplied(ctx context.Context, tx pgx.Tx, proposalID, ledgerTxnID string, now time.Time) error {
	var ledgerArg any
	if ledgerTxnID != "" {
		ledgerArg = ledgerTxnID
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO applied_proposals
		     (proposal_id, ledger_txn_id, applied_at, executor_run_id)
		 VALUES ($1::uuid, $2::uuid, $3, $4)`,
		proposalID, ledgerArg, now, s.executorRunID)
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errAppliedByAnother
	}
	return fmt.Errorf("executor: insert applied_proposals: %w", err)
}

// enqueueApplied writes the proposal.applied outbox row idempotently. The
// executor has no table-level SELECT on outbox, so the UUID is generated in Go
// and the insert never uses RETURNING.
func (s *Service) enqueueApplied(ctx context.Context, tx pgx.Tx, locked lockedProposal, ledgerTxnID string, now time.Time) error {
	outboxID, err := proposal.NewUUID()
	if err != nil {
		return fmt.Errorf("executor: generate outbox id: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"proposal_id":     locked.ID,
		"case_id":         locked.CaseID,
		"type":            locked.Type,
		"ledger_txn_id":   ledgerTxnID,
		"applied_at":      now,
		"executor_run_id": s.executorRunID,
	})
	if err != nil {
		return fmt.Errorf("executor: encode outbox payload: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox
		     (id, aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES ($1::uuid, 'proposal', $2, 'proposal.applied', $3::jsonb, $4, $5, $5)
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		outboxID, locked.ID, string(payload), "proposal.applied:"+locked.ID, now); err != nil {
		return fmt.Errorf("executor: enqueue proposal.applied: %w", err)
	}
	return nil
}

// appendAudit writes one clock-stamped executor audit entry in the caller's
// transaction.
func (s *Service) appendAudit(ctx context.Context, tx pgx.Tx, entry audit.Entry) error {
	entry.Timestamp = entry.Timestamp.UTC().Truncate(time.Microsecond)
	if _, err := audit.Append(ctx, tx, entry); err != nil {
		return fmt.Errorf("executor: append %s audit: %w", entry.Action, err)
	}
	return nil
}

// replay re-reads an applied proposal in a fresh transaction after losing an
// applied_proposals insert race. A proposal that is no longer applied is not
// applicable.
func (s *Service) replay(ctx context.Context, proposalID string) (Result, error) {
	var result Result
	err := db.WithTxPolicy(ctx, s.pool, pgx.TxOptions{}, s.retry, func(ctx context.Context, tx pgx.Tx) error {
		result = Result{}
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM proposals WHERE id = $1::uuid`, proposalID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrProposalNotFound
			}
			return fmt.Errorf("executor: re-read proposal: %w", err)
		}
		if state.Status(status) != state.StatusApplied {
			return fmt.Errorf("%w: status %s", ErrNotApplicable, status)
		}
		ledgerTxnID, err := loadApplied(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		result = Result{
			ProposalID:  proposalID,
			Status:      StatusReplayed,
			LedgerTxnID: ledgerTxnID,
			Replayed:    true,
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// lockedProposal is the row Apply locks and re-validates.
type lockedProposal struct {
	ID        string
	CaseID    string
	Type      string
	Payload   []byte
	Evidence  []byte
	Status    state.Status
	Version   int
	StateHash string
	ExpiresAt *time.Time
}

func lockProposal(ctx context.Context, tx pgx.Tx, proposalID string) (lockedProposal, error) {
	var (
		locked lockedProposal
		status string
	)
	err := tx.QueryRow(ctx,
		`SELECT id::text, case_id::text, type, payload, evidence,
		        status, version, state_hash, expires_at
		   FROM proposals
		  WHERE id = $1::uuid
		  FOR UPDATE`, proposalID).
		Scan(&locked.ID, &locked.CaseID, &locked.Type, &locked.Payload,
			&locked.Evidence, &status, &locked.Version, &locked.StateHash,
			&locked.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedProposal{}, ErrProposalNotFound
	}
	if err != nil {
		return lockedProposal{}, fmt.Errorf("executor: lock proposal: %w", err)
	}
	locked.Status = state.Status(status)
	return locked, nil
}

func loadApplied(ctx context.Context, tx pgx.Tx, proposalID string) (string, error) {
	var ledgerTxnID *string
	err := tx.QueryRow(ctx,
		`SELECT ledger_txn_id::text FROM applied_proposals WHERE proposal_id = $1::uuid`,
		proposalID).Scan(&ledgerTxnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: proposal %s is applied but has no applied_proposals row",
			ErrInconsistentState, proposalID)
	}
	if err != nil {
		return "", fmt.Errorf("executor: load applied proposal: %w", err)
	}
	if ledgerTxnID == nil {
		return "", nil
	}
	return *ledgerTxnID, nil
}

// errAppliedByAnother aborts the apply transaction when the FIN-23 unique key
// rejects the applied_proposals insert. Apply re-reads and returns a replay.
var errAppliedByAnother = errors.New("executor: another executor applied the proposal")

// canonicalProposalID accepts a UUID in either case and returns the canonical
// lowercase spelling. Empty, whitespace-padded, and non-UUID values are typed
// ErrInvalidProposalID.
func canonicalProposalID(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", fmt.Errorf("%w: %q", ErrInvalidProposalID, raw)
	}
	normalized := strings.ToLower(raw)
	if !uuidPattern.MatchString(normalized) {
		return "", fmt.Errorf("%w: %q is not a UUID", ErrInvalidProposalID, raw)
	}
	return normalized, nil
}
