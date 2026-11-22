package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// Audit and outbox vocabulary owned by the approval service.
const (
	// ActionProposalPolicy is the audit action for the routed policy decision.
	ActionProposalPolicy = "proposal.policy"
	// TopicApprovalRequest is the outbox topic M4-12 delivers to Slack/ticket.
	TopicApprovalRequest = "approval.request"
	// TopicApprovalUpdate is the outbox topic for an in-place decided-message
	// update. The decision path that enqueues it lands in a later wave; the
	// Slack handler consumes it now.
	TopicApprovalUpdate = "approval.update"
	// ReasonApprovalRequired is the pending_approval transition reason.
	ReasonApprovalRequired = "approval_required"
)

// canonicalUUIDPattern accepts only the lowercase canonical textual UUID form,
// so a malformed or upper-case id never reaches a uuid cast.
var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// loadedProposal is the proposal projection Request needs. Payload, evidence,
// and confidence are untrusted agent-produced data, decoded strictly by the
// facts builder where relevant.
type loadedProposal struct {
	ID              string
	CaseID          string
	Type            string
	Payload         json.RawMessage
	Evidence        []string
	Confidence      decimal.Decimal
	PatternVerified *string
	Status          state.Status
	TraceID         *string
}

// Request routes one proposed proposal. It runs the verifier, evaluates policy,
// applies FIN-53 limits to a T0 result, then in one transaction persists the
// decision, sets expires_at, transitions to auto_approved or pending_approval,
// appends proposal.policy audit, and (T1/T2 only) enqueues exactly one
// approval.request outbox row. It never calls a channel adapter and never sends
// a network message.
//
// A proposal that is not proposed returns ErrAlreadyRequested before any
// verification, policy, or write. Verification/policy reads happen outside the
// final transaction; the transaction re-locks the proposal and re-checks the
// status, so a concurrent first Request wins and the loser writes nothing.
func (s *Service) Request(ctx context.Context, proposalID string) (RequestResult, error) {
	if s == nil {
		return RequestResult{}, fmt.Errorf("%w: nil service", ErrInvalidRequest)
	}
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return RequestResult{}, fmt.Errorf("%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidRequest, proposalID)
	}

	proposal, err := s.loadProposal(ctx, proposalID)
	if err != nil {
		return RequestResult{}, err
	}
	if proposal.Status != state.StatusProposed {
		return RequestResult{ProposalID: proposal.ID, Status: string(proposal.Status)},
			fmt.Errorf("%w: proposal %s is %s", ErrAlreadyRequested, proposal.ID, proposal.Status)
	}

	caseCurrency, err := s.loadCaseCurrency(ctx, proposal.CaseID)
	if err != nil {
		return RequestResult{}, err
	}
	autoApplyEnabled, err := s.loadAutoApplyEnabled(ctx)
	if err != nil {
		return RequestResult{}, err
	}
	facts, err := buildFacts(proposal, caseCurrency, autoApplyEnabled)
	if err != nil {
		return RequestResult{}, err
	}

	verification, err := s.verifier.Verify(ctx, proposal.ID)
	if err != nil {
		return RequestResult{}, fmt.Errorf("approval: verify proposal %s: %w", proposal.ID, err)
	}
	// The production verifier persists proposals.pattern_verified during
	// Verify. The pre-verification row is stale, so reload it before policy
	// evaluation; otherwise an agent-created proposal is always T1
	// pattern_not_allowed even when verification passes.
	refreshed, err := s.loadPatternVerified(ctx, proposal.ID)
	if err != nil {
		return RequestResult{}, err
	}
	proposal.PatternVerified = refreshed

	ctx, decision, limitsReason, err := s.evaluatePolicy(ctx, &proposal, verification, facts)
	if err != nil {
		return RequestResult{}, err
	}

	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	expiresAt := now.Add(s.ttl)
	channel := channelForTier(decision.Tier)
	finalStatus := state.StatusAutoApproved
	transitionReason := ""
	if decision.Tier != policy.TierT0 {
		finalStatus = state.StatusPendingApproval
		transitionReason = ReasonApprovalRequired
	}

	var result RequestResult
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var (
			currentStatus string
			version       int
		)
		err := tx.QueryRow(ctx,
			`SELECT status, version FROM proposals WHERE id = $1::uuid FOR UPDATE`,
			proposal.ID).Scan(&currentStatus, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrProposalNotFound, proposal.ID)
		}
		if err != nil {
			return fmt.Errorf("approval: lock proposal %s: %w", proposal.ID, err)
		}
		if currentStatus != string(state.StatusProposed) {
			return fmt.Errorf("%w: proposal %s is %s", ErrAlreadyRequested, proposal.ID, currentStatus)
		}

		if err := s.policy.Loaded.EnsureVersion(ctx, tx, now); err != nil {
			return fmt.Errorf("approval: ensure policy version: %w", err)
		}
		if err := policy.PersistDecision(ctx, tx, proposal.ID, decision); err != nil {
			return fmt.Errorf("approval: persist policy decision: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE proposals SET expires_at = $2 WHERE id = $1::uuid`,
			proposal.ID, expiresAt); err != nil {
			return fmt.Errorf("approval: set proposal expiry: %w", err)
		}
		if _, err := s.state.Transition(ctx, tx, state.TransitionRequest{
			ProposalID:      proposal.ID,
			To:              finalStatus,
			ExpectedVersion: version,
			Actor:           s.systemActor,
			Reason:          transitionReason,
		}); err != nil {
			return fmt.Errorf("approval: transition proposal: %w", err)
		}

		auditAfter := map[string]any{
			"tier":          string(decision.Tier),
			"rule":          decision.Rule,
			"reasons":       append([]string(nil), decision.Reasons...),
			"config_hash":   decision.ConfigHash,
			"ttl_seconds":   int64(s.ttl / time.Second),
			"auto_approved": decision.Tier == policy.TierT0,
			"status":        string(finalStatus),
		}
		if limitsReason != "" {
			auditAfter["limits_reason"] = limitsReason
		}
		if channel != "" {
			auditAfter["channel"] = channel
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.systemActor,
			Action:     ActionProposalPolicy,
			EntityType: state.EntityProposal,
			EntityID:   proposal.ID,
			Before:     map[string]any{"status": string(state.StatusProposed)},
			After:      auditAfter,
			Timestamp:  now,
		}); err != nil {
			return fmt.Errorf("approval: append policy audit: %w", err)
		}

		if decision.Tier != policy.TierT0 {
			payload, err := json.Marshal(approvalRequestPayload{
				ProposalID:  proposal.ID,
				CaseID:      proposal.CaseID,
				Tier:        string(decision.Tier),
				Channel:     channel,
				RequestedAt: now.Format(time.RFC3339Nano),
			})
			if err != nil {
				return fmt.Errorf("approval: encode approval request: %w", err)
			}
			inserted, err := outbox.EnqueueAggregate(ctx, tx, "proposal", proposal.ID,
				TopicApprovalRequest, payload, approvalRequestKey(proposal.ID))
			if err != nil {
				return fmt.Errorf("approval: enqueue approval request: %w", err)
			}
			if !inserted {
				// A duplicate key with a still-proposed row means another writer
				// already enqueued this request. Treat the zero-row conflict as
				// an error, never as success (M4-07/Q-91 lesson).
				return fmt.Errorf("%w: %s already exists", ErrAlreadyRequested,
					approvalRequestKey(proposal.ID))
			}
		}

		result = RequestResult{
			ProposalID:   proposal.ID,
			Tier:         decision.Tier,
			Status:       string(finalStatus),
			Channel:      channel,
			ExpiresAt:    expiresAt,
			AutoApproved: decision.Tier == policy.TierT0,
			LimitsReason: limitsReason,
		}
		return nil
	})
	if err != nil {
		return RequestResult{}, err
	}
	return result, nil
}

type approvalRequestPayload struct {
	ProposalID  string `json:"proposal_id"`
	CaseID      string `json:"case_id"`
	Tier        string `json:"tier"`
	Channel     string `json:"channel"`
	RequestedAt string `json:"requested_at"`
}

func approvalRequestKey(proposalID string) string {
	return TopicApprovalRequest + ":" + proposalID
}

func channelForTier(tier policy.Tier) string {
	switch tier {
	case policy.TierT1:
		return ChannelSlack
	case policy.TierT2:
		return ChannelTicket
	default:
		return ""
	}
}

// mapLimitsT1 replaces an allowed T0 decision with a valid T1 decision. The
// three named limits reasons keep their own code; every other reason maps to
// value_unknown while the exact reason stays in the limits audit field.
func mapLimitsT1(base policy.Decision, reason string) policy.Decision {
	code := policy.CodeValueUnknown
	switch reason {
	case limits.ReasonAutoApplyDisabled:
		code = policy.CodeAutoApplyDisabled
	case limits.ReasonDailyCapExceeded:
		code = policy.CodeDailyCapExceeded
	case limits.ReasonBreakRateSpike:
		code = policy.CodeBreakRateSpike
	}
	return policy.Decision{
		SchemaVersion: policy.SchemaVersion,
		Tier:          policy.TierT1,
		Rule:          code,
		Reasons:       []string{code},
		ConfigHash:    base.ConfigHash,
	}
}

// loadPatternVerified reloads the verifier-owned column after Verify commits.
func (s *Service) loadPatternVerified(ctx context.Context, proposalID string) (*string, error) {
	var pattern *string
	if err := s.pool.QueryRow(ctx,
		`SELECT pattern_verified FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&pattern); err != nil {
		return nil, fmt.Errorf("approval: reload pattern_verified %s: %w", proposalID, err)
	}
	return pattern, nil
}

func (s *Service) loadProposal(ctx context.Context, proposalID string) (loadedProposal, error) {
	var (
		proposal      loadedProposal
		status        string
		payload       []byte
		evidenceRaw   []byte
		confidenceRaw string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, case_id::text, type, payload, evidence,
		        confidence::text, pattern_verified, status, trace_id
		   FROM proposals
		  WHERE id = $1::uuid`, proposalID).
		Scan(&proposal.ID, &proposal.CaseID, &proposal.Type, &payload,
			&evidenceRaw, &confidenceRaw, &proposal.PatternVerified, &status,
			&proposal.TraceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedProposal{}, fmt.Errorf("%w: %s", ErrProposalNotFound, proposalID)
	}
	if err != nil {
		return loadedProposal{}, fmt.Errorf("approval: load proposal %s: %w", proposalID, err)
	}
	if err := json.Unmarshal(evidenceRaw, &proposal.Evidence); err != nil {
		return loadedProposal{}, fmt.Errorf("%w: decode proposal evidence: %v", ErrInvalidRequest, err)
	}
	confidence, err := decimal.NewFromString(confidenceRaw)
	if err != nil {
		return loadedProposal{}, fmt.Errorf("%w: decode proposal confidence: %v", ErrInvalidRequest, err)
	}
	proposal.Payload = payload
	proposal.Confidence = confidence
	proposal.Status = state.Status(status)
	return proposal, nil
}

func (s *Service) loadCaseCurrency(ctx context.Context, caseID string) (string, error) {
	var currency string
	err := s.pool.QueryRow(ctx, `SELECT currency FROM cases WHERE id = $1::uuid`, caseID).
		Scan(&currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: case %s", ErrProposalNotFound, caseID)
	}
	if err != nil {
		return "", fmt.Errorf("approval: load case %s: %w", caseID, err)
	}
	return currency, nil
}

// loadAutoApplyEnabled fails closed when the switch row is absent: an absent
// switch cannot enable auto-apply, so policy records auto_apply_disabled.
func (s *Service) loadAutoApplyEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT enabled FROM system_switches WHERE name = $1`, limits.SwitchAutoApply).
		Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("approval: read auto-apply switch: %w", err)
	}
	return enabled, nil
}

// buildFacts derives the policy facts the engine cannot query itself. A
// malformed adjustment/write_off payload is rejected rather than guessed:
// M2-08 validated the stored proposal payload, so a decode failure is data
// corruption, not a policy input.
func buildFacts(proposal loadedProposal, caseCurrency string, autoApplyEnabled bool) (policy.Facts, error) {
	switch policy.ProposalType(proposal.Type) {
	case policy.TypeAdjustment, policy.TypeWriteOff:
		return adjustmentFacts(proposal.Payload, caseCurrency, autoApplyEnabled)
	case policy.TypeLink, policy.TypeDuplicate:
		return nonMonetaryFacts(caseCurrency, autoApplyEnabled), nil
	default:
		// request_info and any future non-monetary type still route through
		// policy, which rejects it as type_not_allowed (T1) and never
		// auto-applies it. M4-10 owns its lifecycle.
		return nonMonetaryFacts(caseCurrency, autoApplyEnabled), nil
	}
}

func nonMonetaryFacts(caseCurrency string, autoApplyEnabled bool) policy.Facts {
	return policy.Facts{
		Currency:         caseCurrency,
		CaseCurrency:     caseCurrency,
		EvidenceValid:    true,
		AutoApplyEnabled: autoApplyEnabled,
	}
}

type adjustmentPayload struct {
	Lines []adjustmentLine `json:"lines"`
}

type adjustmentLine struct {
	AccountCode string `json:"account_code"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// adjustmentFacts sums debit lines, records every line currency and account
// code, and uses the single debit currency as the proposal value currency. A
// missing debit line leaves Currency empty (value_unknown); mixed debit
// currencies also leave it empty, which fails closed to T2 cross_currency.
func adjustmentFacts(payload json.RawMessage, caseCurrency string, autoApplyEnabled bool) (policy.Facts, error) {
	var decoded adjustmentPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return policy.Facts{}, fmt.Errorf("%w: decode %s payload: %v", ErrInvalidRequest, "adjustment", err)
	}

	facts := policy.Facts{
		CaseCurrency:     caseCurrency,
		EvidenceValid:    true,
		AutoApplyEnabled: autoApplyEnabled,
	}
	debitCurrencies := make([]string, 0, 1)
	for i, line := range decoded.Lines {
		if line.AccountCode == "" {
			return policy.Facts{}, fmt.Errorf("%w: adjustment payload line %d has no account_code", ErrInvalidRequest, i)
		}
		if line.Direction != "debit" && line.Direction != "credit" {
			return policy.Facts{}, fmt.Errorf("%w: adjustment payload line %d direction %q", ErrInvalidRequest, i, line.Direction)
		}
		if line.AmountMinor <= 0 {
			return policy.Facts{}, fmt.Errorf("%w: adjustment payload line %d amount_minor must be positive", ErrInvalidRequest, i)
		}
		if line.Currency == "" {
			return policy.Facts{}, fmt.Errorf("%w: adjustment payload line %d has no currency", ErrInvalidRequest, i)
		}
		facts.AccountCodes = append(facts.AccountCodes, line.AccountCode)
		facts.LineCurrencies = append(facts.LineCurrencies, line.Currency)
		if line.Direction != "debit" {
			continue
		}
		if facts.ValueMinor > math.MaxInt64-line.AmountMinor {
			return policy.Facts{}, fmt.Errorf("%w: payload debit total overflows int64", ErrInvalidRequest)
		}
		facts.ValueMinor += line.AmountMinor
		if !containsString(debitCurrencies, line.Currency) {
			debitCurrencies = append(debitCurrencies, line.Currency)
		}
	}
	if len(debitCurrencies) == 1 {
		facts.Currency = debitCurrencies[0]
	}
	return facts, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
