package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/approver"
	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// ErrE2EFailed is the fail-closed pipeline error. Every RunCase error wraps
// it; callers persist the partial Result before reporting/aborting.
var ErrE2EFailed = errors.New("eval/e2e: pipeline failed")

// Proposal statuses the router understands. They are the closed approval state
// vocabulary and are intentionally spelled here rather than imported from the
// state package so the pipeline's routing table is reviewable in one place.
const (
	statusProposed        = "proposed"
	statusPendingApproval = "pending_approval"
	statusApproved        = "approved"
	statusAutoApproved    = "auto_approved"
	statusApplied         = "applied"
	statusApplyFailed     = "apply_failed"
	statusRejected        = "rejected"
	statusExpired         = "expired"
	statusSuperseded      = "superseded"
)

const maxErrorLength = 320

// Input is the per-case pipeline input. ProposalID is empty when the agent
// produced no proposal. Case/Proposal are the caller's observable projections;
// the read model refreshes them so a verifier-written pattern_verified is
// visible to the approver.
type Input struct {
	ProposalID       string
	CaseID           string
	ScopeID          string
	AsOf             time.Time
	Case             approver.CaseFacts
	Proposal         approver.ProposalFacts
	AgentClaimedTier *string
}

// Pipeline is the per-case approval/executor/tie-out orchestration. It is
// wired per case so the approval, executor, and approver services use the
// case's fake clock.
type Pipeline struct {
	Approval *approval.Service
	Executor *executor.Service
	Approver *approver.Approver
	Tieout   tieout.Queryer
	Loader   Loader
	Clock    clock.Clock
}

type stage int

const (
	stageRoute stage = iota
	stageApprover
	stageExecutor
	stageTieout
)

// RunCase drives one case through the frozen pipeline order. On success it
// returns a valid eval-e2e/v1 result. On any pipeline error it returns the
// partial result with Failed=true and a bounded Error, and the returned error
// wraps ErrE2EFailed. A deterministic rejection is a successful result, not a
// pipeline error.
func (p *Pipeline) RunCase(ctx context.Context, in Input) (Result, error) {
	res := Result{
		Schema:         SchemaVersion,
		Proposal:       ProposalNone,
		RequestStatus:  RequestStatusNone,
		ApproverAction: ActionNone,
	}
	err := p.run(ctx, in, &res)
	if err != nil {
		res.Failed = true
		res.Error = safeError(err)
		res.normalizeForFailure()
		return res, fmt.Errorf("%w: %s", ErrE2EFailed, res.Error)
	}
	if err := res.Validate(); err != nil {
		res.Failed = true
		res.Error = safeError(err)
		return res, fmt.Errorf("%w: %s", ErrE2EFailed, res.Error)
	}
	return res, nil
}

func (p *Pipeline) run(ctx context.Context, in Input, res *Result) error {
	if p == nil {
		return errors.New("nil pipeline")
	}
	if err := p.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(in.ProposalID) == "" {
		res.ApproverAction = ActionAbstain
		res.ApproverRule = approver.RuleNoProposal
		return nil
	}
	if !canonicalUUIDPattern.MatchString(in.ProposalID) {
		return fmt.Errorf("proposal id %q is not a canonical UUID", in.ProposalID)
	}
	res.Proposal = ProposalPresent
	res.ProposalID = in.ProposalID
	res.AgentClaimedTier = normalizeTier(in.AgentClaimedTier)

	state, err := p.Loader.ProposalState(ctx, in.ProposalID)
	if err != nil {
		return err
	}
	if in.CaseID != "" && state.CaseID != in.CaseID {
		return errors.New("proposal belongs to a different case than the input")
	}
	if state.Type == "request_info" {
		// The lifecycle routes request_info to waiting_info; it never reaches
		// the approval decision path.
		res.Proposal = ProposalNone
		res.ProposalID = ""
		res.ApproverAction = ActionAbstain
		res.ApproverRule = approver.RuleNoProposal
		return nil
	}
	if state.PolicyTier != nil && !validTier(*state.PolicyTier) {
		return fmt.Errorf("proposal has unknown policy tier %q", *state.PolicyTier)
	}
	res.RequestStatus = state.Status
	if state.Status == statusApplyFailed {
		// apply_failed is not a result status; the request itself reached
		// approved or auto_approved and the failure is carried by Failed/Error.
		res.RequestStatus = approvedRequestStatus(state.PolicyTier)
	}
	res.PolicyTier = state.PolicyTier
	res.Verifier = state.Verifier()
	if res.AgentClaimedTier == nil && state.Status == statusProposed {
		res.AgentClaimedTier = normalizeTier(state.PolicyTier)
	}

	var (
		caseFacts     approver.CaseFacts
		proposalFacts approver.ProposalFacts
	)
	if err := p.loadFacts(ctx, in, state, &caseFacts, &proposalFacts); err != nil {
		return err
	}

	current := stageRoute
	for {
		switch current {
		case stageRoute:
			next, err := p.route(ctx, in, res, &state, &caseFacts, &proposalFacts)
			if err != nil {
				return err
			}
			current = next
		case stageApprover:
			next, err := p.approve(ctx, res, &state, caseFacts, proposalFacts)
			if err != nil {
				return err
			}
			current = next
		case stageExecutor:
			next, err := p.execute(ctx, res, &state)
			if err != nil {
				return err
			}
			current = next
		case stageTieout:
			return p.computeTieout(ctx, in, res)
		default:
			return errors.New("internal routing stage is invalid")
		}
	}
}

func (p *Pipeline) validate() error {
	if p.Approval == nil {
		return errors.New("approval service is required")
	}
	if p.Executor == nil {
		return errors.New("executor service is required")
	}
	if p.Approver == nil {
		return errors.New("simulated approver is required")
	}
	if p.Loader == nil {
		return errors.New("read model loader is required")
	}
	if p.Clock == nil {
		return errors.New("clock is required")
	}
	return nil
}

func (p *Pipeline) route(ctx context.Context, in Input, res *Result,
	state *ProposalState, caseFacts *approver.CaseFacts,
	proposalFacts *approver.ProposalFacts) (stage, error) {
	switch state.Status {
	case statusProposed:
		if res.AgentClaimedTier == nil {
			res.AgentClaimedTier = normalizeTier(state.PolicyTier)
		}
		requested, err := p.Approval.Request(ctx, state.ProposalID)
		if errors.Is(err, approval.ErrAlreadyRequested) {
			if err := p.reload(ctx, in, res, state, caseFacts, proposalFacts); err != nil {
				return stageRoute, err
			}
			return stageRoute, nil
		}
		if err != nil {
			return stageRoute, fmt.Errorf("request approval: %w", err)
		}
		switch requested.Tier {
		case policy.TierT0:
			if requested.Status != statusAutoApproved {
				return stageRoute, fmt.Errorf(
					"T0 request returned status %q, want %s", requested.Status, statusAutoApproved)
			}
		case policy.TierT1, policy.TierT2:
			if requested.Status != statusPendingApproval {
				return stageRoute, fmt.Errorf(
					"tier %s request returned status %q, want %s",
					requested.Tier, requested.Status, statusPendingApproval)
			}
		default:
			return stageRoute, fmt.Errorf("request returned unsupported tier %q", requested.Tier)
		}

		tier := string(requested.Tier)
		state.Status = requested.Status
		state.PolicyTier = &tier
		res.RequestStatus = requested.Status
		res.PolicyTier = state.PolicyTier

		// The verifier persists pattern_verified during Request; reload before
		// building the approver's case facts or reading the verifier block.
		if err := p.reload(ctx, in, res, state, caseFacts, proposalFacts); err != nil {
			return stageRoute, err
		}
		if state.Status != requested.Status {
			// A concurrent writer advanced the proposal between Request and
			// the reload. Re-route from the freshly observed status.
			return stageRoute, nil
		}
		return stageApprover, nil

	case statusPendingApproval:
		if err := p.reload(ctx, in, res, state, caseFacts, proposalFacts); err != nil {
			return stageRoute, err
		}
		return stageApprover, nil

	case statusApproved, statusAutoApproved, statusApplyFailed:
		setActionFromState(res, *state)
		return stageExecutor, nil

	case statusApplied:
		res.RequestStatus = statusApplied
		res.Applied = true
		res.LedgerTxnID = state.LedgerTxnID
		setActionFromState(res, *state)
		return stageTieout, nil

	case statusRejected:
		res.RequestStatus = statusRejected
		res.ApproverAction = ActionReject
		return stageTieout, nil

	case statusExpired, statusSuperseded:
		res.RequestStatus = state.Status
		res.ApproverAction = ActionNone
		return stageTieout, nil

	default:
		return stageRoute, fmt.Errorf("proposal has unknown status %q", state.Status)
	}
}

func (p *Pipeline) approve(ctx context.Context, res *Result, state *ProposalState,
	caseFacts approver.CaseFacts, proposalFacts approver.ProposalFacts) (stage, error) {
	if state.Status != statusPendingApproval && state.Status != statusAutoApproved {
		return stageRoute, fmt.Errorf("approver called in status %s", state.Status)
	}
	if state.PolicyTier == nil {
		return stageRoute, errors.New("proposal has no policy tier")
	}
	requested := approval.RequestResult{
		ProposalID:   state.ProposalID,
		Tier:         policy.Tier(*state.PolicyTier),
		Status:       state.Status,
		AutoApproved: state.Status == statusAutoApproved,
	}
	if state.ExpiresAt != nil {
		requested.ExpiresAt = *state.ExpiresAt
	}
	outcome, err := p.Approver.Act(ctx, requested, caseFacts, proposalFacts)
	if err != nil {
		return stageRoute, fmt.Errorf("simulated approver: %w", err)
	}
	res.ApproverAction = string(outcome.Action)
	res.ApproverRule = outcome.Rule
	switch outcome.Action {
	case approver.ActionSkipT0:
		if state.Status != statusAutoApproved && requested.Tier != policy.TierT0 {
			return stageRoute, errors.New("approver skipped T0 on a non-T0 proposal")
		}
		return stageExecutor, nil
	case approver.ActionApprove:
		if state.Status != statusPendingApproval {
			return stageRoute, errors.New("approver approved a non-pending proposal")
		}
		return stageExecutor, nil
	case approver.ActionReject:
		if state.Status != statusPendingApproval {
			return stageRoute, errors.New("approver rejected a non-pending proposal")
		}
		state.Status = statusRejected
		res.RequestStatus = statusRejected
		return stageTieout, nil
	case approver.ActionAbstain:
		return stageRoute, errors.New("approver abstained while the proposal was pending")
	default:
		return stageRoute, fmt.Errorf("approver returned unsupported action %q", outcome.Action)
	}
}

func (p *Pipeline) execute(ctx context.Context, res *Result, state *ProposalState) (stage, error) {
	result, err := p.Executor.Apply(ctx, state.ProposalID)
	if err != nil {
		return stageRoute, fmt.Errorf("executor apply: %w", err)
	}
	switch result.Status {
	case executor.StatusApplied, executor.StatusReplayed:
		if ledgerAffecting(state.Type) && result.LedgerTxnID == "" {
			return stageRoute, errors.New("executor applied without a ledger transaction id")
		}
		state.Status = statusApplied
		state.LedgerTxnID = result.LedgerTxnID
		res.RequestStatus = statusApplied
		res.Applied = true
		res.LedgerTxnID = result.LedgerTxnID
		return stageTieout, nil
	case executor.StatusApplyFailed:
		res.RequestStatus = approvedRequestStatus(state.PolicyTier)
		return stageRoute, fmt.Errorf("executor recorded apply_failed: %s", result.FailureReason)
	default:
		return stageRoute, fmt.Errorf("executor returned unsupported status %q", result.Status)
	}
}

func (p *Pipeline) computeTieout(ctx context.Context, in Input, res *Result) error {
	if in.ScopeID == "" && in.AsOf.IsZero() {
		return nil
	}
	if in.ScopeID == "" || in.AsOf.IsZero() {
		return errors.New("tie-out requires both a scope id and an as-of time")
	}
	if p.Tieout == nil {
		return errors.New("tie-out queryer is required")
	}
	result, err := tieout.Compute(ctx, p.Tieout, in.ScopeID, in.AsOf)
	if err != nil {
		return fmt.Errorf("tie-out: %w", err)
	}
	res.Tieout = tieOutFrom(result)
	return nil
}

// reload refreshes the proposal state and the two observable facts from the
// read model. It is the only place that overlays the verifier's
// pattern_verified onto CaseFacts.BreakKind.
func (p *Pipeline) reload(ctx context.Context, in Input, res *Result,
	state *ProposalState, caseFacts *approver.CaseFacts,
	proposalFacts *approver.ProposalFacts) error {
	refreshed, err := p.Loader.ProposalState(ctx, state.ProposalID)
	if err != nil {
		return err
	}
	*state = refreshed
	if state.PolicyTier != nil && !validTier(*state.PolicyTier) {
		return fmt.Errorf("proposal has unknown policy tier %q", *state.PolicyTier)
	}
	res.RequestStatus = state.Status
	if state.PolicyTier != nil {
		res.PolicyTier = state.PolicyTier
	}
	res.Verifier = state.Verifier()
	return p.loadFacts(ctx, in, *state, caseFacts, proposalFacts)
}

func (p *Pipeline) loadFacts(ctx context.Context, in Input, state ProposalState,
	caseFacts *approver.CaseFacts, proposalFacts *approver.ProposalFacts) error {
	loadedCase, err := p.Loader.LoadCaseFacts(ctx, state.CaseID)
	if err != nil {
		return err
	}
	if in.Case.CaseID != "" && in.Case.CaseID != loadedCase.CaseID {
		return errors.New("case facts identify a different case than the input")
	}
	if in.Case.ScopeID != "" && in.Case.ScopeID != loadedCase.ScopeID {
		return errors.New("case facts identify a different scope than the input")
	}
	if state.PatternVerified != nil && *state.PatternVerified != "" {
		loadedCase.BreakKind = *state.PatternVerified
	}
	loadedProposal, err := p.Loader.LoadProposalFacts(ctx, state.ProposalID)
	if err != nil {
		return err
	}
	if in.Proposal.ProposalID != "" && in.Proposal.ProposalID != loadedProposal.ProposalID {
		return errors.New("proposal facts identify a different proposal than the input")
	}
	*caseFacts = loadedCase
	*proposalFacts = loadedProposal
	return nil
}

func setActionFromState(res *Result, state ProposalState) {
	if state.Status == statusAutoApproved ||
		(state.PolicyTier != nil && *state.PolicyTier == string(policy.TierT0)) {
		res.ApproverAction = ActionSkipT0
		res.ApproverRule = approver.RuleT0AutoApproved
		return
	}
	switch state.Status {
	case statusApproved, statusApplied, statusApplyFailed:
		res.ApproverAction = ActionApprove
		res.ApproverRule = approver.RuleExactMatch
	case statusRejected:
		res.ApproverAction = ActionReject
	default:
		res.ApproverAction = ActionNone
	}
}

func approvedRequestStatus(tier *string) string {
	if tier != nil && *tier == string(policy.TierT0) {
		return statusAutoApproved
	}
	return statusApproved
}

func ledgerAffecting(proposalType string) bool {
	return proposalType == "adjustment" || proposalType == "write_off"
}

func normalizeTier(tier *string) *string {
	if tier == nil || !validTier(*tier) {
		return nil
	}
	value := *tier
	return &value
}

// normalizeForFailure makes a partial failed result persistable: the frozen
// schema has no apply_failed request status, and an aborted route may not have
// reached a valid status/action yet. The failure itself is carried by
// Failed/Error; this never hides a successful apply.
func (r *Result) normalizeForFailure() {
	if r.Proposal != ProposalPresent && r.Proposal != ProposalNone {
		r.Proposal = ProposalNone
	}
	if r.Proposal == ProposalNone {
		r.ProposalID = ""
	} else if !canonicalUUIDPattern.MatchString(r.ProposalID) {
		r.Proposal = ProposalNone
		r.ProposalID = ""
	}
	if r.RequestStatus == statusApplyFailed {
		r.RequestStatus = approvedRequestStatus(r.PolicyTier)
	} else if !validRequestStatus(r.RequestStatus) {
		r.RequestStatus = RequestStatusNone
	}
	if r.Applied {
		r.RequestStatus = RequestStatusApplied
	}
	if r.PolicyTier != nil && !validTier(*r.PolicyTier) {
		r.PolicyTier = nil
	}
	if r.AgentClaimedTier != nil && !validTier(*r.AgentClaimedTier) {
		r.AgentClaimedTier = nil
	}
	if !validAction(r.ApproverAction) {
		r.ApproverAction = ActionNone
	}
	if r.Tieout != nil {
		if _, err := time.Parse(dateLayout, r.Tieout.AsOf); err != nil || r.Tieout.Currency == "" {
			r.Tieout = nil
		}
	}
	if r.LedgerTxnID != "" && !canonicalUUIDPattern.MatchString(r.LedgerTxnID) {
		r.LedgerTxnID = ""
	}
}

// safeError collapses whitespace and bounds length. Pipeline errors never
// include payloads, memos, DSNs, API keys, or raw record contents by
// construction; this is the second line of defence for database messages.
func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > maxErrorLength {
		message = message[:maxErrorLength] + "..."
	}
	return message
}
