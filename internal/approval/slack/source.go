package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/TamasGorgics/recon-agent/internal/approval"
)

// Source hydrates the Block Kit read model for one approval request. It is
// deliberately narrow: the approval.request outbox payload carries only
// identifiers and routing metadata, so the handler loads fresh proposal/case
// content outside any transaction. Implementations must not write.
//
// The production implementation is DBSource. A fake may be injected in tests;
// the outbox handlers never hold a transaction while calling this method.
type Source interface {
	ApprovalMessage(ctx context.Context, proposalID string) (MessageData, error)
}

// ApproverReader resolves the human-readable name shown on a decided message.
// DBSource implements it over approvers.display_name. It is separate from
// Source so a fake read model does not have to supply approval identity.
type ApproverReader interface {
	ApproverDisplayName(ctx context.Context, approverID string) (string, error)
}

// DBSource is the read-only SQL implementation of Source and ApproverReader.
// It issues one statement per call through a recon_app queryer. It never
// reads external record raw payloads: evidence is the canonical reference
// list stored on the proposal.
type DBSource struct {
	queryer Queryer
}

// NewSource returns a DB-backed read model. The queryer is typically the
// outbox worker's recon_app pool. A nil or typed-nil queryer fails closed.
func NewSource(q Queryer) (*DBSource, error) {
	if err := validateQueryer(q); err != nil {
		return nil, err
	}
	return &DBSource{queryer: q}, nil
}

// approvalMessageQuery reads exactly the spec section 5.3 fields. The lateral
// subquery returns the total attached-break count and the first attached break
// (ordered by case_breaks.created_at, then break id for a stable tie-break) in
// one round trip; a case with no breaks yields NULLs and is coalesced to zero.
const approvalMessageQuery = `
SELECT p.type,
       p.version,
       p.status,
       p.risk_tier,
       p.expires_at,
       p.rationale,
       p.evidence::text,
       p.pattern_claimed,
       p.pattern_verified,
       p.verification::text,
       p.policy_decision::text,
       c.id::text,
       c.value_minor,
       c.currency,
       c.priority,
       c.created_at,
       COALESCE(br.break_count, 0),
       COALESCE(br.side, ''),
       COALESCE(br.delta_minor, 0),
       COALESCE(br.currency, '')
  FROM proposals p
  JOIN cases c ON c.id = p.case_id
  LEFT JOIN LATERAL (
        SELECT count(*) OVER () AS break_count,
               b.side,
               b.delta_minor,
               b.currency
          FROM case_breaks cb
          JOIN breaks b ON b.id = cb.break_id
         WHERE cb.case_id = c.id
         ORDER BY cb.created_at, b.id
         LIMIT 1
       ) br ON true
 WHERE p.id = $1::uuid`

// policyDecisionDocument is the subset of proposals.policy_decision the message
// renders. The full document is owned by internal/policy.
type policyDecisionDocument struct {
	Tier string `json:"tier"`
	Rule string `json:"rule"`
}

// ApprovalMessage implements Source. A missing proposal returns a typed
// approval.ErrProposalNotFound. A malformed proposal id returns
// ErrInvalidMessage before SQL reaches Postgres.
func (s *DBSource) ApprovalMessage(ctx context.Context, proposalID string) (MessageData, error) {
	if s == nil || s.queryer == nil {
		return MessageData{}, fmt.Errorf("%w: source has no queryer", ErrInvalidMessage)
	}
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return MessageData{}, fmt.Errorf(
			"%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidMessage, proposalID)
	}

	var (
		data            MessageData
		riskTier        pgtype.Text
		expiresAt       pgtype.Timestamptz
		evidenceJSON    pgtype.Text
		patternClaimed  pgtype.Text
		patternVerified pgtype.Text
		verification    pgtype.Text
		policyJSON      pgtype.Text
		breakCount      int64
	)
	err := s.queryer.QueryRow(ctx, approvalMessageQuery, proposalID).Scan(
		&data.ProposalType,
		&data.ProposalVersion,
		&data.ProposalStatus,
		&riskTier,
		&expiresAt,
		&data.Rationale,
		&evidenceJSON,
		&patternClaimed,
		&patternVerified,
		&verification,
		&policyJSON,
		&data.CaseID,
		&data.CaseValueMinor,
		&data.CaseCurrency,
		&data.CasePriority,
		&data.CaseCreatedAt,
		&breakCount,
		&data.PrimaryBreakSide,
		&data.PrimaryBreakDeltaMinor,
		&data.PrimaryBreakCurrency,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageData{}, fmt.Errorf("%w: %s", approval.ErrProposalNotFound, proposalID)
	}
	if err != nil {
		return MessageData{}, fmt.Errorf("approval/slack: hydrate proposal %s: %w",
			proposalID, err)
	}

	data.ProposalID = proposalID
	if riskTier.Valid {
		data.RiskTier = riskTier.String
	}
	if patternClaimed.Valid {
		data.PatternClaimed = patternClaimed.String
	}
	if patternVerified.Valid {
		data.PatternVerified = patternVerified.String
	}
	if expiresAt.Valid {
		if expiresAt.InfinityModifier != pgtype.Finite {
			return MessageData{}, fmt.Errorf(
				"approval/slack: proposal %s expires_at is %s",
				proposalID, expiresAt.InfinityModifier)
		}
		data.ExpiresAt = expiresAt.Time.UTC()
	}
	if evidenceJSON.Valid && strings.TrimSpace(evidenceJSON.String) != "" &&
		strings.TrimSpace(evidenceJSON.String) != "null" {
		if err := json.Unmarshal([]byte(evidenceJSON.String), &data.Evidence); err != nil {
			return MessageData{}, fmt.Errorf(
				"approval/slack: proposal %s evidence is not a string array: %w",
				proposalID, err)
		}
	}
	if verification.Valid && strings.TrimSpace(verification.String) != "" &&
		strings.TrimSpace(verification.String) != "null" {
		data.Verification = json.RawMessage(verification.String)
	}
	if policyJSON.Valid && strings.TrimSpace(policyJSON.String) != "" &&
		strings.TrimSpace(policyJSON.String) != "null" {
		var policy policyDecisionDocument
		if err := json.Unmarshal([]byte(policyJSON.String), &policy); err != nil {
			return MessageData{}, fmt.Errorf(
				"approval/slack: proposal %s policy_decision is not valid JSON: %w",
				proposalID, err)
		}
		data.PolicyTier = policy.Tier
		data.PolicyRule = policy.Rule
	}
	if breakCount < 0 || breakCount > int64(maxInt()) {
		return MessageData{}, fmt.Errorf(
			"approval/slack: proposal %s has an out-of-range break count %d",
			proposalID, breakCount)
	}
	data.BreakCount = int(breakCount)
	return data, nil
}

// ApproverDisplayName implements ApproverReader. A missing approver returns a
// typed approval.ErrApproverNotFound; approval rows are FK-protected, so this
// is a corruption signal rather than a normal steady state.
func (s *DBSource) ApproverDisplayName(ctx context.Context, approverID string) (string, error) {
	if s == nil || s.queryer == nil {
		return "", fmt.Errorf("%w: source has no queryer", ErrInvalidMessage)
	}
	if !canonicalUUIDPattern.MatchString(approverID) {
		return "", fmt.Errorf(
			"%w: approver id %q is not a canonical lowercase UUID",
			ErrInvalidMessage, approverID)
	}
	var name string
	err := s.queryer.QueryRow(ctx,
		`SELECT display_name FROM approvers WHERE id = $1::uuid`, approverID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", approval.ErrApproverNotFound, approverID)
	}
	if err != nil {
		return "", fmt.Errorf("approval/slack: load approver %s: %w", approverID, err)
	}
	return name, nil
}

// maxInt is the largest value an int can hold on this platform. It guards the
// int64 count(*) scan before the conversion.
func maxInt() int {
	return int(^uint(0) >> 1)
}
