package approver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// Stored proposal status values the approver needs. The frozen approval
// contract names these strings; the package deliberately does not import the
// approval state package so its only approval dependency is the service API.
const (
	statusPendingApproval = "pending_approval"
	statusAutoApproved    = "auto_approved"
	statusApproved        = "approved"
	statusApplied         = "applied"
	statusRejected        = "rejected"
)

// Stable sentinel errors. Errors wrap one of these where a typed outcome
// exists; they never contain payload contents, memos, proofs, or identifiers
// beyond the proposal id already in the request.
var (
	// ErrNotPending means the routed request is neither T0/auto-approved nor
	// pending_approval. The approver makes no decision.
	ErrNotPending = errors.New("eval/approver: proposal is not pending approval")
	// ErrNoApprover means the decider or the configured approver identity is
	// missing or invalid.
	ErrNoApprover = errors.New("eval/approver: no approver identity or decider")
	// ErrNoSource means the configured decision source is missing or is not
	// the eval-only source "e2e".
	ErrNoSource = errors.New("eval/approver: no valid decision source")
	// ErrRuleTable means the compile-time predicate table is empty, malformed,
	// or missing a required field rule.
	ErrRuleTable = errors.New("eval/approver: invalid rule table")
)

// ErrInvalidFacts marks structurally missing or malformed observable input.
// It is unexported because callers should treat it as a pipeline failure: the
// approver never turns malformed input into an approval or a rejection.
var errInvalidFacts = errors.New("eval/approver: invalid observable facts")

// errRecordSource means a predicate needs cited records but no RecordSource
// was configured.
var errRecordSource = errors.New("eval/approver: record source is required for cited records")

// Action is the simulated approver's observable outcome.
type Action string

const (
	// ActionSkipT0 means the request was auto-approved at T0. The decider is
	// never called.
	ActionSkipT0 Action = "skip_t0"
	// ActionApprove means every exact-match predicate passed and the decider
	// approved the proposal.
	ActionApprove Action = "approve"
	// ActionReject means a predicate failed and the decider rejected the
	// proposal with a stable rule id.
	ActionReject Action = "reject"
	// ActionAbstain means there was no proposal to decide.
	ActionAbstain Action = "abstain"
)

// CaseFacts is the observable case projection. Every field comes from the
// case, its breaks, and the reconciliation scope; none of it comes from
// eval_truth. DeltaMinor is the absolute observable discrepancy in minor units.
type CaseFacts struct {
	CaseID       string
	ScopeID      string
	BreakKind    string // observable break type, e.g. fee_variance
	DeltaMinor   int64
	Currency     string
	Counterparty string
}

// ProposalFacts is the observable projection of one stored proposal.
//
// AmountMinor is the debit total for adjustment/write_off, or the amount
// implied by the cited records for link/duplicate. LedgerRefs are canonical
// ledger transaction UUIDs. ExternalRefs are canonical ext:<source>:<record>
// refs. Resolution is the normalized resolution claimed by the proposal
// (link, adjustment, write_off, duplicate).
type ProposalFacts struct {
	ProposalID   string
	Type         string // adjustment | link | duplicate | write_off
	Resolution   string // normalized resolution claimed by the proposal
	AmountMinor  int64
	Currency     string
	LedgerRefs   []string
	ExternalRefs []string
	Counterparty string
}

// LedgerRecord is the observable projection of one cited ledger transaction.
// AmountMinor is the transaction's effect on the reconciliation scope account.
type LedgerRecord struct {
	ID          string
	ScopeID     string
	AmountMinor int64
	Currency    string
}

// ExternalRecord is the observable projection of one cited external record.
type ExternalRecord struct {
	ID           string
	ScopeID      string
	AmountMinor  int64
	Currency     string
	Counterparty string
}

// RecordSource resolves the ledger and external records cited by a proposal.
// Implementations must read only production/eval-sandbox data, never
// eval_truth or another answer source.
type RecordSource interface {
	Ledger(ctx context.Context, ids []string) ([]LedgerRecord, error)
	External(ctx context.Context, ids []string) ([]ExternalRecord, error)
}

// Decider is the narrow decision surface the approver is allowed to use.
// *approval.Service satisfies it; the approver never writes approved/rejected
// state directly.
type Decider interface {
	Decide(ctx context.Context, proposalID string, d approval.Decision,
		approverID, source string) (approval.DecisionResult, error)
}

// Config is the fixed simulated-approver configuration. ApproverID is the
// seeded synthetic approver UUID; Source must be exactly "e2e".
type Config struct {
	ApproverID string
	Source     string
}

// Outcome is the result of one Act call. Decision is zero for skip_t0 and
// abstain because the decider is not called.
type Outcome struct {
	Action   Action
	Rule     string
	Reason   string
	Decision approval.DecisionResult
}

// Approver is the deterministic, truth-blind simulated approver.
type Approver struct {
	decider Decider
	records RecordSource
	cfg     Config
}

// New validates the decider, synthetic identity, and rule table. records may
// be nil only for proposals that cite no ledger or external records; a
// predicate that needs cited records fails closed with a pipeline error.
func New(decider Decider, records RecordSource, cfg Config) (*Approver, error) {
	if isNilValue(decider) {
		return nil, fmt.Errorf("%w: decider is required", ErrNoApprover)
	}
	if err := validateApproverID(cfg.ApproverID); err != nil {
		return nil, err
	}
	if cfg.Source != SyntheticSource {
		return nil, fmt.Errorf("%w: source must be %q", ErrNoSource, SyntheticSource)
	}
	if err := validateRuleTable(ruleTable); err != nil {
		return nil, err
	}
	return &Approver{decider: decider, records: records, cfg: cfg}, nil
}

// Act evaluates the frozen decision table in order and, for T1/T2, calls the
// decider exactly once. It never calls the decider for T0, no-proposal, or an
// invalid request.
func (a *Approver) Act(ctx context.Context, req approval.RequestResult,
	c CaseFacts, p ProposalFacts) (Outcome, error) {
	if a == nil {
		return Outcome{}, fmt.Errorf("%w: nil approver", ErrNoApprover)
	}

	// Table order is frozen: T0 bypass wins even if a no-proposal fact is
	// supplied, so a caller can never turn an auto-approved request into an
	// abstain or a decision.
	if req.Tier == policy.TierT0 || req.Status == statusAutoApproved {
		return Outcome{
			Action: ActionSkipT0,
			Rule:   RuleT0AutoApproved,
			Reason: "proposal was auto-approved at T0; no simulated decision",
		}, nil
	}
	if p.ProposalID == "" {
		return Outcome{
			Action: ActionAbstain,
			Rule:   RuleNoProposal,
			Reason: "no proposal to decide",
		}, nil
	}
	if req.Status != statusPendingApproval {
		return Outcome{}, fmt.Errorf("%w: status %q", ErrNotPending, req.Status)
	}
	if req.Tier != policy.TierT1 && req.Tier != policy.TierT2 {
		return Outcome{}, fmt.Errorf("eval/approver: pending request has unsupported tier %q", req.Tier)
	}
	if req.ProposalID != p.ProposalID {
		return Outcome{}, errors.New("eval/approver: request and proposal facts identify different proposals")
	}
	if err := validateFacts(c, p); err != nil {
		return Outcome{}, err
	}

	e := &evaluation{caseFacts: c, proposal: p, records: a.records}
	for _, rule := range ruleTable {
		err := rule.Check(ctx, e)
		if err == nil {
			continue
		}
		var mismatch *ruleMismatch
		if errors.As(err, &mismatch) {
			return a.decide(ctx, p.ProposalID, approval.DecisionRejected,
				ActionReject, mismatch.rule, mismatch.reason)
		}
		return Outcome{}, fmt.Errorf("eval/approver: evaluate %s: %w", rule.ID, err)
	}
	return a.decide(ctx, p.ProposalID, approval.DecisionApproved,
		ActionApprove, RuleExactMatch, reasonExactMatch)
}

// decide performs the only state-changing operation in this package: a call
// to the approval service. It validates the returned decision so a decider
// bug cannot be reported as a successful action.
func (a *Approver) decide(ctx context.Context, proposalID string, d approval.Decision,
	action Action, rule, reason string) (Outcome, error) {
	result, err := a.decider.Decide(ctx, proposalID, d, a.cfg.ApproverID, a.cfg.Source)
	if err != nil {
		return Outcome{}, fmt.Errorf("eval/approver: decide proposal %s: %w", proposalID, err)
	}
	if err := validateDecisionResult(proposalID, d, a.cfg.ApproverID, result); err != nil {
		return Outcome{}, err
	}
	return Outcome{Action: action, Rule: rule, Reason: reason, Decision: result}, nil
}

// validateDecisionResult rejects a decision result that does not describe the
// decision this package asked for. It never echoes identifiers returned by a
// faulty decider.
func validateDecisionResult(proposalID string, d approval.Decision,
	approverID string, result approval.DecisionResult) error {
	if result.ProposalID != proposalID {
		return errors.New("eval/approver: decider returned a different proposal id")
	}
	if result.Decision != d {
		return errors.New("eval/approver: decider returned a different decision")
	}
	if result.ApproverID != approverID {
		return errors.New("eval/approver: decider returned a different approver id")
	}
	switch d {
	case approval.DecisionApproved:
		// A replay after apply reports the proposal's post-decision status,
		// which is "applied"; a fresh approval reports "approved".
		if result.Status != statusApproved && result.Status != statusApplied {
			return errors.New("eval/approver: decider returned an unexpected approved status")
		}
	case approval.DecisionRejected:
		if result.Status != statusRejected {
			return errors.New("eval/approver: decider returned an unexpected rejected status")
		}
	default:
		return fmt.Errorf("%w: %q", approval.ErrInvalidDecision, d)
	}
	return nil
}

// validateFacts fails closed on structurally missing or malformed observable
// case facts. Proposal-field problems are predicate mismatches, not malformed
// input, and are reported as stable rejection rules by the rule table.
func validateFacts(c CaseFacts, p ProposalFacts) error {
	switch {
	case !canonicalUUIDPattern.MatchString(c.CaseID):
		return fmt.Errorf("%w: case id is missing or malformed", errInvalidFacts)
	case !canonicalUUIDPattern.MatchString(c.ScopeID):
		return fmt.Errorf("%w: scope id is missing or malformed", errInvalidFacts)
	case strings.TrimSpace(c.BreakKind) == "":
		return fmt.Errorf("%w: break kind is missing", errInvalidFacts)
	case c.BreakKind != strings.TrimSpace(c.BreakKind):
		return fmt.Errorf("%w: break kind is padded", errInvalidFacts)
	case !currencyPattern.MatchString(c.Currency):
		return fmt.Errorf("%w: case currency is missing or malformed", errInvalidFacts)
	case !canonicalUUIDPattern.MatchString(p.ProposalID):
		return fmt.Errorf("%w: proposal id is missing or malformed", errInvalidFacts)
	default:
		return nil
	}
}

// isNilValue reports whether value is nil or a typed nil inside an interface.
// It mirrors the approval service's dependency check so a typed-nil decider or
// record source is rejected at construction.
func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
