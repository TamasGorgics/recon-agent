package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Decision vocabulary owned by the approval service.
const (
	// ActionApprovalDecided is the audit action for one human decision.
	ActionApprovalDecided = "approval.decided"
	// ReasonExpired is the pending_approval -> expired transition reason used
	// when a late decision finds the proposal past its TTL.
	ReasonExpired = "expired"
	// ReasonDecisionRejected is the deterministic fallback rejection reason.
	// Decide requires a non-empty source, so this is only a defensive default
	// for the state machine's reason-required rejected edge.
	ReasonDecisionRejected = "decision_rejected"
)

// approverIdentity is the approvers row Decide needs. APIKeyID is the optional
// config API key that identifies the same human on a channel callback.
type approverIdentity struct {
	ID       string
	Role     string
	Active   bool
	APIKeyID *string
}

// Decide records one human approved/rejected decision. Validation and approver
// resolution happen before any write, and the decision itself runs in one
// transaction: the proposal is locked, a late decision expires the proposal
// instead, four-eyes is checked against audit_log, the approvals row and
// human-actor state transition are written, and approval.decided is appended.
//
// Idempotency: a repeated click by the same approver with the same decision is
// a replay that writes nothing and returns the stored decided_at. Any other
// decision on an already decided proposal is ErrDecisionConflict.
func (s *Service) Decide(ctx context.Context, proposalID string, d Decision, approverID, source string) (DecisionResult, error) {
	if s == nil {
		return DecisionResult{}, fmt.Errorf("%w: nil service", ErrInvalidRequest)
	}
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return DecisionResult{}, fmt.Errorf("%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidRequest, proposalID)
	}
	if err := validateApproverID(approverID); err != nil {
		return DecisionResult{}, err
	}
	source, err := validateDecisionInput(d, source)
	if err != nil {
		return DecisionResult{}, err
	}

	approver, err := s.loadApproverByID(ctx, approverID)
	if err != nil {
		return DecisionResult{}, err
	}
	human, err := audit.HumanActor(approver.ID)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("%w: approver actor: %v", ErrInvalidRequest, err)
	}

	var (
		result    DecisionResult
		expiredAt *time.Time
	)
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// WithTx may retry; never carry an expiry result or result snapshot
		// from a failed attempt into a committed one.
		expiredAt = nil
		result = DecisionResult{}
		proposal, err := lockDecidableProposal(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC().Truncate(time.Microsecond)

		switch proposal.Status {
		case state.StatusPendingApproval:
			if proposal.ExpiresAt != nil && !proposal.ExpiresAt.After(now) {
				// The expiry transition must commit, so the ErrExpired result is
				// returned after the transaction commits, never from inside the
				// callback (which would roll the transition back).
				if _, err := s.state.Transition(ctx, tx, state.TransitionRequest{
					ProposalID:      proposalID,
					To:              state.StatusExpired,
					ExpectedVersion: proposal.Version,
					Actor:           s.expiryActor,
					Reason:          ReasonExpired,
				}); err != nil {
					return fmt.Errorf("approval: expire proposal %s: %w", proposalID, err)
				}
				at := proposal.ExpiresAt.UTC()
				expiredAt = &at
				return nil
			}

			coMaker, err := s.hasCoMakerAudit(ctx, tx, approver, proposalID, proposal.CaseID, proposal.AgentRunID)
			if err != nil {
				return err
			}
			if coMaker {
				return fmt.Errorf("%w: approver %s made proposal %s", ErrCoMaker, approver.ID, proposalID)
			}

			target := state.StatusApproved
			reason := ""
			if d == DecisionRejected {
				target = state.StatusRejected
				reason = source
				if strings.TrimSpace(reason) == "" {
					reason = ReasonDecisionRejected
				}
			}

			decidedAt, inserted, err := insertApproval(ctx, tx, proposalID, source, approver.ID, d, now)
			if err != nil {
				return err
			}
			if !inserted {
				// A duplicate (proposal, approver) row while the proposal is
				// still pending can only be drift. Treat the same decision as a
				// replay, anything else as a conflict (Q-91 lesson: never let a
				// zero-row write pass silently as success).
				existing, err := loadApprovalDecision(ctx, tx, proposalID, approver.ID)
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: proposal %s has an approvals conflict",
						ErrDecisionConflict, proposalID)
				}
				if err != nil {
					return fmt.Errorf("approval: read existing approval: %w", err)
				}
				if existing.Decision != d {
					return fmt.Errorf("%w: approver %s already decided %s on proposal %s",
						ErrDecisionConflict, approver.ID, existing.Decision, proposalID)
				}
				result = DecisionResult{
					ProposalID: proposalID,
					Status:     string(decisionStatus(d)),
					Decision:   d,
					ApproverID: approver.ID,
					DecidedAt:  existing.DecidedAt,
					Replayed:   true,
				}
				return nil
			}

			updated, err := s.state.Transition(ctx, tx, state.TransitionRequest{
				ProposalID:      proposalID,
				To:              target,
				ExpectedVersion: proposal.Version,
				Actor:           human,
				Reason:          reason,
			})
			if err != nil {
				return fmt.Errorf("approval: transition proposal %s: %w", proposalID, err)
			}

			if _, err := audit.Append(ctx, tx, audit.Entry{
				Actor:      human,
				Action:     ActionApprovalDecided,
				EntityType: state.EntityProposal,
				EntityID:   proposalID,
				Before: map[string]any{
					"status":  string(state.StatusPendingApproval),
					"version": proposal.Version,
				},
				After: map[string]any{
					"status":      string(target),
					"version":     updated.Version,
					"decision":    string(d),
					"source":      source,
					"approver_id": approver.ID,
				},
				Timestamp: now,
			}); err != nil {
				return fmt.Errorf("approval: append decision audit: %w", err)
			}

			result = DecisionResult{
				ProposalID: proposalID,
				Status:     string(target),
				Decision:   d,
				ApproverID: approver.ID,
				DecidedAt:  decidedAt,
				Replayed:   false,
			}
			return nil

		case state.StatusApproved, state.StatusRejected, state.StatusApplied:
			existing, err := loadApprovalDecision(ctx, tx, proposalID, approver.ID)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: proposal %s is %s; approver %s has no decision",
					ErrDecisionConflict, proposalID, proposal.Status, approver.ID)
			}
			if err != nil {
				return fmt.Errorf("approval: read existing approval: %w", err)
			}
			if existing.Decision != d {
				return fmt.Errorf("%w: approver %s decided %s on proposal %s",
					ErrDecisionConflict, approver.ID, existing.Decision, proposalID)
			}
			result = DecisionResult{
				ProposalID: proposalID,
				Status:     string(proposal.Status),
				Decision:   d,
				ApproverID: approver.ID,
				DecidedAt:  existing.DecidedAt,
				Replayed:   true,
			}
			return nil

		default:
			return fmt.Errorf("%w: proposal %s is %s", ErrNotPending, proposalID, proposal.Status)
		}
	})
	if err != nil {
		return DecisionResult{}, err
	}
	if expiredAt != nil {
		return DecisionResult{}, fmt.Errorf("%w: proposal %s expired at %s",
			ErrExpired, proposalID, expiredAt.Format(time.RFC3339))
	}
	return result, nil
}

// DecideByAPIKey maps a configured API key id to the approvers row and then
// runs the same decision path as Decide. A missing, inactive, or non-approver
// row returns the corresponding typed error before any proposal write.
func (s *Service) DecideByAPIKey(ctx context.Context, proposalID string, d Decision, apiKeyID, source string) (DecisionResult, error) {
	if s == nil {
		return DecisionResult{}, fmt.Errorf("%w: nil service", ErrInvalidRequest)
	}
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return DecisionResult{}, fmt.Errorf("%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidRequest, proposalID)
	}
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return DecisionResult{}, fmt.Errorf("%w: api key id is required", ErrInvalidRequest)
	}
	if _, err := validateDecisionInput(d, source); err != nil {
		return DecisionResult{}, err
	}

	approver, err := s.loadApproverByAPIKey(ctx, apiKeyID)
	if err != nil {
		return DecisionResult{}, err
	}
	return s.Decide(ctx, proposalID, d, approver.ID, source)
}

// validateApproverID rejects agent/system actor strings before any database
// call (FIN-33) and otherwise requires the canonical lowercase UUID form.
func validateApproverID(approverID string) error {
	switch {
	case strings.HasPrefix(approverID, "agent:") || approverID == "agent":
		return fmt.Errorf("%w: approver id %q is an agent actor", ErrAgentActor, approverID)
	case strings.HasPrefix(approverID, "system:") || approverID == "system":
		return fmt.Errorf("%w: approver id %q is a system actor", ErrAgentActor, approverID)
	}
	if !canonicalUUIDPattern.MatchString(approverID) {
		return fmt.Errorf("%w: approver id %q is not a canonical lowercase UUID",
			ErrInvalidRequest, approverID)
	}
	return nil
}

// validateDecisionInput checks the closed decision set and the mandatory
// source, returning the trimmed source that is stored as the channel.
func validateDecisionInput(d Decision, source string) (string, error) {
	if !d.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidDecision, d)
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("%w: decision source is required", ErrInvalidRequest)
	}
	return source, nil
}

func decisionStatus(d Decision) state.Status {
	if d == DecisionRejected {
		return state.StatusRejected
	}
	return state.StatusApproved
}

// lockedDecidableProposal is the row snapshot Decide takes under FOR UPDATE.
type lockedDecidableProposal struct {
	Status     state.Status
	Version    int
	CaseID     string
	AgentRunID string
	ExpiresAt  *time.Time
}

func lockDecidableProposal(ctx context.Context, tx pgx.Tx, proposalID string) (lockedDecidableProposal, error) {
	var (
		proposal  lockedDecidableProposal
		status    string
		expiresAt pgtype.Timestamptz
	)
	err := tx.QueryRow(ctx,
		`SELECT status, version, case_id::text, agent_run_id::text, expires_at
		   FROM proposals
		  WHERE id = $1::uuid
		 FOR UPDATE`, proposalID).
		Scan(&status, &proposal.Version, &proposal.CaseID, &proposal.AgentRunID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedDecidableProposal{}, fmt.Errorf("%w: %s", ErrProposalNotFound, proposalID)
	}
	if err != nil {
		return lockedDecidableProposal{}, fmt.Errorf("approval: lock proposal %s: %w", proposalID, err)
	}
	proposal.Status = state.Status(status)
	if !proposal.Status.Valid() {
		return lockedDecidableProposal{}, fmt.Errorf("%w: stored status %q",
			state.ErrUnknownStatus, proposal.Status)
	}
	if expiresAt.Valid {
		if expiresAt.InfinityModifier != pgtype.Finite {
			return lockedDecidableProposal{}, fmt.Errorf("%w: proposal %s expires_at is %s",
				state.ErrNonFiniteExpiry, proposalID, expiresAt.InfinityModifier)
		}
		at := expiresAt.Time
		proposal.ExpiresAt = &at
	}
	return proposal, nil
}

// insertApproval appends the append-only approvals row. A zero-row insert
// means the (proposal, approver) key already exists; the caller decides whether
// that is a replay or a conflict.
func insertApproval(
	ctx context.Context,
	tx pgx.Tx,
	proposalID, channel, approverID string,
	d Decision,
	decidedAt time.Time,
) (time.Time, bool, error) {
	var stored time.Time
	err := tx.QueryRow(ctx,
		`INSERT INTO approvals
		     (proposal_id, channel, external_ref, approver_id, decision,
		      decided_at, signature_valid)
		 VALUES ($1::uuid, $2, NULL, $3::uuid, $4, $5, true)
		 ON CONFLICT (proposal_id, approver_id) DO NOTHING
		 RETURNING decided_at`,
		proposalID, channel, approverID, string(d), decidedAt).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("approval: insert approval: %w", err)
	}
	return stored, true, nil
}

// storedApproval is the projection needed to replay a decision.
type storedApproval struct {
	Decision  Decision
	DecidedAt time.Time
}

func loadApprovalDecision(ctx context.Context, tx pgx.Tx, proposalID, approverID string) (storedApproval, error) {
	var (
		decision  string
		decidedAt time.Time
	)
	err := tx.QueryRow(ctx,
		`SELECT decision, decided_at
		   FROM approvals
		  WHERE proposal_id = $1::uuid AND approver_id = $2::uuid`,
		proposalID, approverID).Scan(&decision, &decidedAt)
	if err != nil {
		return storedApproval{}, err
	}
	return storedApproval{Decision: Decision(decision), DecidedAt: decidedAt}, nil
}

// hasCoMakerAudit reports whether the approver is already a human actor on the
// proposal, its case, or its agent run. Both the approver UUID and the optional
// API key id are identities the same human can leave in audit_log.
func (s *Service) hasCoMakerAudit(
	ctx context.Context,
	tx pgx.Tx,
	approver approverIdentity,
	proposalID, caseID, agentRunID string,
) (bool, error) {
	identities := []string{approver.ID}
	if approver.APIKeyID != nil && strings.TrimSpace(*approver.APIKeyID) != "" {
		identities = append(identities, *approver.APIKeyID)
	}
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1
		       FROM audit_log
		      WHERE actor_type = 'human'
		        AND actor_id = ANY($1::text[])
		        AND ((entity_type = 'proposal' AND entity_id = $2)
		          OR (entity_type = 'case' AND entity_id = $3)
		          OR ($4 <> '' AND entity_type = 'agent_run' AND entity_id = $4))
		 )`, identities, proposalID, caseID, agentRunID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("approval: check co-maker audit: %w", err)
	}
	return exists, nil
}

const approverColumns = `id::text, role, active, api_key_id`

// loadApproverByID resolves an approver UUID and enforces the approver/admin
// role and active state.
func (s *Service) loadApproverByID(ctx context.Context, approverID string) (approverIdentity, error) {
	approver, err := scanApprover(s.pool.QueryRow(ctx,
		`SELECT `+approverColumns+` FROM approvers WHERE id = $1::uuid`, approverID))
	if errors.Is(err, pgx.ErrNoRows) {
		return approverIdentity{}, fmt.Errorf("%w: %s", ErrApproverNotFound, approverID)
	}
	if err != nil {
		return approverIdentity{}, fmt.Errorf("approval: load approver %s: %w", approverID, err)
	}
	return validateApprover(approver)
}

// loadApproverByAPIKey resolves the configured approver.api_key_id mapping.
// The key value is never echoed in an error.
func (s *Service) loadApproverByAPIKey(ctx context.Context, apiKeyID string) (approverIdentity, error) {
	approver, err := scanApprover(s.pool.QueryRow(ctx,
		`SELECT `+approverColumns+` FROM approvers WHERE api_key_id = $1`, apiKeyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return approverIdentity{}, fmt.Errorf("%w: no approver is mapped to that API key", ErrApproverNotFound)
	}
	if err != nil {
		return approverIdentity{}, fmt.Errorf("approval: load approver by API key: %w", err)
	}
	return validateApprover(approver)
}

func scanApprover(row pgx.Row) (approverIdentity, error) {
	var approver approverIdentity
	err := row.Scan(&approver.ID, &approver.Role, &approver.Active, &approver.APIKeyID)
	return approver, err
}

// validateApprover applies the role and active checks in the public error
// order: role first (ErrApproverRole), then active (ErrApproverInactive).
func validateApprover(approver approverIdentity) (approverIdentity, error) {
	if approver.Role != "approver" && approver.Role != "admin" {
		return approverIdentity{}, fmt.Errorf("%w: approver %s has role %s",
			ErrApproverRole, approver.ID, approver.Role)
	}
	if !approver.Active {
		return approverIdentity{}, fmt.Errorf("%w: approver %s", ErrApproverInactive, approver.ID)
	}
	return approver, nil
}
