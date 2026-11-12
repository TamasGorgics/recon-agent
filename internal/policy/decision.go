package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Tier is the approved risk tier of a proposal.
type Tier string

// Risk tiers, strictest last. A decision can only be tightened by confidence.
const (
	TierT0 Tier = "T0"
	TierT1 Tier = "T1"
	TierT2 Tier = "T2"
)

// Rule and reason codes. This is the closed vocabulary from the policy
// contract: reasons are machine codes only, never amounts or untrusted text.
const (
	CodeHighValue             = "high_value"
	CodeWriteOffLimit         = "write_off_limit"
	CodeCrossCurrency         = "cross_currency"
	CodeCustomerFunds         = "customer_funds"
	CodeRepeatedCounterparty  = "repeated_counterparty"
	CodeTypeNotAllowed        = "type_not_allowed"
	CodePatternNotAllowed     = "pattern_not_allowed"
	CodeVerifierMissing       = "verifier_missing"
	CodeVerifierFailed        = "verifier_failed"
	CodeVerifierMismatch      = "verifier_mismatch"
	CodeEvidenceInvalid       = "evidence_invalid"
	CodeValueUnknown          = "value_unknown"
	CodeValueCap              = "value_cap"
	CodeAutoApplyDisabled     = "auto_apply_disabled"
	CodeAutoApply             = "auto_apply"
	CodeDefaultSingleApproval = "default_single_approval"
	CodeConfidenceDowngrade   = "confidence_downgrade"
)

var codeSet = map[string]struct{}{
	CodeHighValue:             {},
	CodeWriteOffLimit:         {},
	CodeCrossCurrency:         {},
	CodeCustomerFunds:         {},
	CodeRepeatedCounterparty:  {},
	CodeTypeNotAllowed:        {},
	CodePatternNotAllowed:     {},
	CodeVerifierMissing:       {},
	CodeVerifierFailed:        {},
	CodeVerifierMismatch:      {},
	CodeEvidenceInvalid:       {},
	CodeValueUnknown:          {},
	CodeValueCap:              {},
	CodeAutoApplyDisabled:     {},
	CodeAutoApply:             {},
	CodeDefaultSingleApproval: {},
	CodeConfidenceDowngrade:   {},
}

// Tier-specific rule sets. A T0 decision can only record the auto-apply rule;
// a T1 decision records one T0 gate (or the defensive default) or the
// confidence downgrade; a T2 decision records a T2 predicate or the confidence
// downgrade. T2 predicates are never valid on T0/T1.
var (
	t0RuleSet = map[string]struct{}{
		CodeAutoApply: {},
	}
	t1RuleSet = map[string]struct{}{
		CodeTypeNotAllowed:        {},
		CodePatternNotAllowed:     {},
		CodeVerifierMissing:       {},
		CodeVerifierFailed:        {},
		CodeVerifierMismatch:      {},
		CodeEvidenceInvalid:       {},
		CodeValueUnknown:          {},
		CodeValueCap:              {},
		CodeAutoApplyDisabled:     {},
		CodeDefaultSingleApproval: {},
		CodeConfidenceDowngrade:   {},
	}
	t2RuleSet = map[string]struct{}{
		CodeHighValue:            {},
		CodeWriteOffLimit:        {},
		CodeCrossCurrency:        {},
		CodeCustomerFunds:        {},
		CodeRepeatedCounterparty: {},
		CodeConfidenceDowngrade:  {},
	}
)

// ErrInvalidDecision reports a decision that cannot be persisted.
var ErrInvalidDecision = errors.New("policy: invalid decision")

var configHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Decision is the stable policy decision document stored on a proposal. The
// field order is frozen: Decision.JSON is the only marshaling path.
type Decision struct {
	SchemaVersion int      `json:"schema_version"`
	Tier          Tier     `json:"tier"`
	Rule          string   `json:"rule"`
	Reasons       []string `json:"reasons"`
	ConfigHash    string   `json:"config_hash"`
}

// Validate rejects an unknown tier or code, a schema version other than 1,
// nil/duplicate reasons, a rule that is not allowed for the decision's tier, a
// rule missing from reasons, and a malformed config hash.
func (d Decision) Validate() error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version %d, want %d", ErrInvalidDecision, d.SchemaVersion, SchemaVersion)
	}
	switch d.Tier {
	case TierT0, TierT1, TierT2:
	default:
		return fmt.Errorf("%w: unknown tier %q", ErrInvalidDecision, d.Tier)
	}
	if _, ok := codeSet[d.Rule]; !ok {
		return fmt.Errorf("%w: unknown rule %q", ErrInvalidDecision, d.Rule)
	}
	var tierRules map[string]struct{}
	switch d.Tier {
	case TierT0:
		tierRules = t0RuleSet
	case TierT1:
		tierRules = t1RuleSet
	case TierT2:
		tierRules = t2RuleSet
	}
	if _, ok := tierRules[d.Rule]; !ok {
		return fmt.Errorf("%w: rule %q is not valid for tier %s", ErrInvalidDecision, d.Rule, d.Tier)
	}
	if d.Reasons == nil {
		return fmt.Errorf("%w: reasons must not be nil", ErrInvalidDecision)
	}
	seen := make(map[string]struct{}, len(d.Reasons))
	for _, reason := range d.Reasons {
		if _, ok := codeSet[reason]; !ok {
			return fmt.Errorf("%w: unknown reason %q", ErrInvalidDecision, reason)
		}
		if _, ok := seen[reason]; ok {
			return fmt.Errorf("%w: duplicate reason %q", ErrInvalidDecision, reason)
		}
		seen[reason] = struct{}{}
	}
	if _, ok := seen[d.Rule]; !ok {
		return fmt.Errorf("%w: rule %q must be one of reasons", ErrInvalidDecision, d.Rule)
	}
	if !configHashPattern.MatchString(d.ConfigHash) {
		return fmt.Errorf("%w: config_hash must be 64 lowercase hex characters", ErrInvalidDecision)
	}
	return nil
}

// JSON validates and marshals the decision. It returns the exact bytes that
// PersistDecision stores.
func (d Decision) JSON() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %v", ErrInvalidDecision, err)
	}
	return data, nil
}
