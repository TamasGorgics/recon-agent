package score

import (
	"encoding/json"
	"fmt"
)

// E2E detail keys and values. The e2e pipeline persists an eval-e2e/v1 result
// under Detail[DetailE2E]; the scorer projects the closed vocabulary it needs
// and never reaches into the pipeline package.
const (
	// DetailE2E is the eval_case_results.detail key carrying the e2e result.
	DetailE2E = "e2e"
	// E2ESchemaVersion is the only e2e result schema this package understands.
	E2ESchemaVersion = "eval-e2e/v1"
	// E2EUnverifiedPattern buckets verifier evaluations that did not pass.
	// proposals.pattern_verified is non-null only on a pass, so a failed
	// evaluation has no verified pattern. The report needs a bucket for it to
	// keep report.VerifierSummary's total-equals-per-pattern invariant.
	E2EUnverifiedPattern = "unverified"
)

// E2E verifier vocabulary mirrored from eval/e2e. The scorer keeps local
// constants so it does not import the pipeline package.
const (
	e2eProposalPresent    = "present"
	e2eProposalNone       = "none"
	e2eStatusNone         = "none"
	e2eStatusAutoApproved = "auto_approved"
	e2eStatusPending      = "pending_approval"
	e2eStatusApproved     = "approved"
	e2eStatusApplied      = "applied"
	e2eStatusRejected     = "rejected"
	e2eStatusExpired      = "expired"
	e2eStatusSuperseded   = "superseded"
	e2eActionNone         = "none"
	e2eActionSkipT0       = "skip_t0"
	e2eActionApprove      = "approve"
	e2eActionReject       = "reject"
	e2eActionAbstain      = "abstain"
	e2ePolicyTierT0       = "T0"
	e2ePolicyTierT1       = "T1"
	e2ePolicyTierT2       = "T2"
)

// E2EVerifierCounts is one verifier bucket for the e2e report. Evaluated is
// every verifier outcome; Passed is the subset the verifier accepted;
// FalsePasses are accepted outcomes whose scored effect was a false fix.
// FalseNegatives stays zero in M4-14 (Q-98 residual).
type E2EVerifierCounts struct {
	Evaluated      int `json:"evaluated"`
	Passed         int `json:"passed"`
	FalsePasses    int `json:"false_passes"`
	FalseNegatives int `json:"false_negatives"`
}

// E2EVerifierMetrics is the verifier projection of an e2e run. FalsePassRate
// is FalsePasses / Passed and is undefined when nothing passed.
type E2EVerifierMetrics struct {
	Evaluated      int                          `json:"evaluated"`
	Passed         int                          `json:"passed"`
	FalsePasses    int                          `json:"false_passes"`
	FalseNegatives int                          `json:"false_negatives"`
	FalsePassRate  Rate                         `json:"false_pass_rate"`
	ByPattern      map[string]E2EVerifierCounts `json:"by_pattern"`
}

// E2EMetrics is the additive e2e metric set for one standalone run. Rates are
// undefined on an empty denominator; they are never silently zero or one.
//
// Definitions (spec §4.3):
//   - AutoResolvePrecision: T0 auto-approved, applied and correct without a
//     false fix / all T0 auto-approved. An apply failure stays in the
//     denominator.
//   - AutoResolveAllApplied: applied any tier and correct without a false fix /
//     applied any tier. Diagnostic only; never blended into precision.
//   - T0FalseFix: T0 auto-approved, applied and false-fix / T0 auto-approved
//     and applied.
//   - TierCorrectness: tier_actual (the policy tier for e2e runs) equals
//     tier_expected / proposals with both tiers. No-proposal and escalation
//     rows are excluded.
//   - TierClaimedCorrectness: agent-claimed tier equals tier_expected /
//     proposals with both tiers. Diagnostic only.
type E2EMetrics struct {
	Cases                  int                `json:"cases"`
	Proposals              int                `json:"proposals"`
	Applied                int                `json:"applied"`
	Rejected               int                `json:"rejected"`
	Abstained              int                `json:"abstained"`
	Errors                 int                `json:"errors"`
	PolicyTierCounts       map[string]int     `json:"policy_tier_counts"`
	T0AutoResolved         int                `json:"t0_auto_resolved"`
	T0Applied              int                `json:"t0_applied"`
	T0FalseFix             Rate               `json:"t0_false_fix"`
	AutoResolvePrecision   Rate               `json:"auto_resolve_precision"`
	AutoResolveAllApplied  Rate               `json:"auto_resolve_all_applied"`
	TierCorrectness        Rate               `json:"tier_correctness"`
	TierClaimedCorrectness Rate               `json:"tier_claimed_correctness"`
	Verifier               E2EVerifierMetrics `json:"verifier"`
}

// e2eDetail is the narrow projection of an eval-e2e/v1 result used by scoring.
// Unknown JSON fields are ignored: the pipeline owns its full result schema and
// this package only reads the frozen scoring vocabulary.
type e2eDetail struct {
	Schema           string       `json:"schema"`
	Proposal         string       `json:"proposal"`
	RequestStatus    string       `json:"request_status"`
	PolicyTier       *string      `json:"policy_tier"`
	AgentClaimedTier *string      `json:"agent_claimed_tier"`
	ApproverAction   string       `json:"approver_action"`
	Applied          bool         `json:"applied"`
	Verifier         *e2eVerifier `json:"verifier"`
	Failed           bool         `json:"failed"`
}

type e2eVerifier struct {
	Evaluated bool   `json:"evaluated"`
	Passed    bool   `json:"passed"`
	Pattern   string `json:"pattern"`
}

// AggregateE2E folds e2e case results into the M4-14 metric set. Every result
// must carry a valid eval-e2e/v1 detail; a missing, unknown-schema, or
// contradictory detail is a hard error (fail closed, never a silent zero).
func AggregateE2E(results []CaseResult) (E2EMetrics, error) {
	metrics := E2EMetrics{
		PolicyTierCounts: map[string]int{
			e2ePolicyTierT0: 0,
			e2ePolicyTierT1: 0,
			e2ePolicyTierT2: 0,
		},
		Verifier: E2EVerifierMetrics{ByPattern: map[string]E2EVerifierCounts{}},
	}
	metrics.Cases = len(results)

	var (
		precisionNumerator, precisionDenominator     int
		t0AppliedDenominator, t0FalseFixNumerator    int
		allAppliedNumerator, allAppliedDenominator   int
		tierCorrectNumerator, tierCorrectDenominator int
		tierClaimedNumerator, tierClaimedDenominator int
	)

	for _, result := range results {
		detail, err := parseE2EDetail(result)
		if err != nil {
			return E2EMetrics{}, scenarioError(result, err)
		}

		if detail.Proposal == e2eProposalPresent {
			metrics.Proposals++
		}
		if detail.PolicyTier != nil {
			metrics.PolicyTierCounts[*detail.PolicyTier]++
		}
		if detail.Applied {
			metrics.Applied++
		}
		if detail.RequestStatus == e2eStatusRejected {
			metrics.Rejected++
		}
		if detail.ApproverAction == e2eActionAbstain {
			metrics.Abstained++
		}
		if detail.Failed {
			metrics.Errors++
		}

		isT0 := detail.Proposal == e2eProposalPresent &&
			detail.PolicyTier != nil && *detail.PolicyTier == e2ePolicyTierT0
		if isT0 {
			precisionDenominator++
			if detail.Applied {
				t0AppliedDenominator++
				if result.FalseFix {
					t0FalseFixNumerator++
				}
				if result.Correct && !result.FalseFix {
					precisionNumerator++
				}
			}
		}
		if detail.Applied {
			allAppliedDenominator++
			if result.Correct && !result.FalseFix {
				allAppliedNumerator++
			}
		}

		if result.TierActual != nil && result.TierExpected != nil {
			tierCorrectDenominator++
			if *result.TierActual == *result.TierExpected {
				tierCorrectNumerator++
			}
		}
		if detail.AgentClaimedTier != nil && result.TierExpected != nil {
			tierClaimedDenominator++
			if *detail.AgentClaimedTier == *result.TierExpected {
				tierClaimedNumerator++
			}
		}

		if detail.Verifier != nil {
			metrics.Verifier.Evaluated++
			bucket := E2EUnverifiedPattern
			if detail.Verifier.Passed {
				metrics.Verifier.Passed++
				bucket = detail.Verifier.Pattern
			}
			counts := metrics.Verifier.ByPattern[bucket]
			counts.Evaluated++
			if detail.Verifier.Passed {
				counts.Passed++
				if result.FalseFix {
					counts.FalsePasses++
					metrics.Verifier.FalsePasses++
				}
			}
			metrics.Verifier.ByPattern[bucket] = counts
		}
	}

	metrics.T0AutoResolved = precisionDenominator
	metrics.T0Applied = t0AppliedDenominator
	metrics.AutoResolvePrecision = newRate(precisionNumerator, precisionDenominator)
	metrics.T0FalseFix = newRate(t0FalseFixNumerator, t0AppliedDenominator)
	metrics.AutoResolveAllApplied = newRate(allAppliedNumerator, allAppliedDenominator)
	metrics.TierCorrectness = newRate(tierCorrectNumerator, tierCorrectDenominator)
	metrics.TierClaimedCorrectness = newRate(tierClaimedNumerator, tierClaimedDenominator)
	metrics.Verifier.FalsePassRate = newRate(metrics.Verifier.FalsePasses, metrics.Verifier.Passed)
	return metrics, nil
}

// parseE2EDetail decodes and validates one row's e2e detail and cross-checks
// the persisted tier columns. detail["e2e"] may be a decoded JSON object (the
// report path) or a typed result value (the runner path); a JSON round-trip
// accepts both without importing eval/e2e.
func parseE2EDetail(result CaseResult) (e2eDetail, error) {
	raw, ok := result.Detail[DetailE2E]
	if !ok || raw == nil {
		return e2eDetail{}, fmt.Errorf("missing detail.%s object", DetailE2E)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return e2eDetail{}, fmt.Errorf("detail.%s is not a JSON object: %w", DetailE2E, err)
	}
	var detail e2eDetail
	if err := json.Unmarshal(encoded, &detail); err != nil {
		return e2eDetail{}, fmt.Errorf("detail.%s is not a JSON object: %w", DetailE2E, err)
	}
	if err := detail.validate(); err != nil {
		return e2eDetail{}, err
	}
	if err := validateTierColumns(result, detail); err != nil {
		return e2eDetail{}, err
	}
	return detail, nil
}

func (d e2eDetail) validate() error {
	if d.Schema != E2ESchemaVersion {
		return fmt.Errorf("detail.%s schema %q is not %q", DetailE2E, d.Schema, E2ESchemaVersion)
	}
	switch d.Proposal {
	case e2eProposalPresent, e2eProposalNone:
	default:
		return fmt.Errorf("detail.%s.proposal %q is unknown", DetailE2E, d.Proposal)
	}
	if !validE2ERequestStatus(d.RequestStatus) {
		return fmt.Errorf("detail.%s.request_status %q is unknown", DetailE2E, d.RequestStatus)
	}
	if d.Proposal == e2eProposalNone && d.RequestStatus != e2eStatusNone {
		return fmt.Errorf("detail.%s has no proposal but request_status %q",
			DetailE2E, d.RequestStatus)
	}
	if d.PolicyTier != nil {
		if !validE2ETier(*d.PolicyTier) {
			return fmt.Errorf("detail.%s.policy_tier %q is outside T0/T1/T2",
				DetailE2E, *d.PolicyTier)
		}
		if d.Proposal == e2eProposalNone {
			return fmt.Errorf("detail.%s has no proposal but carries policy tier %q",
				DetailE2E, *d.PolicyTier)
		}
	}
	if d.AgentClaimedTier != nil && !validE2ETier(*d.AgentClaimedTier) {
		return fmt.Errorf("detail.%s.agent_claimed_tier %q is outside T0/T1/T2",
			DetailE2E, *d.AgentClaimedTier)
	}
	if !validE2EAction(d.ApproverAction) {
		return fmt.Errorf("detail.%s.approver_action %q is unknown",
			DetailE2E, d.ApproverAction)
	}
	if d.Applied && d.RequestStatus != e2eStatusApplied {
		return fmt.Errorf("detail.%s is applied but request_status is %q",
			DetailE2E, d.RequestStatus)
	}
	if d.Verifier != nil {
		if !d.Verifier.Evaluated {
			return fmt.Errorf("detail.%s.verifier is present but not evaluated", DetailE2E)
		}
		if d.Verifier.Passed && d.Verifier.Pattern == "" {
			return fmt.Errorf("detail.%s.verifier passed without a pattern", DetailE2E)
		}
		if !d.Verifier.Passed && d.Verifier.Pattern != "" {
			return fmt.Errorf("detail.%s.verifier failed but carries pattern %q",
				DetailE2E, d.Verifier.Pattern)
		}
	}
	return nil
}

// validateTierColumns enforces the spec's provenance rule: tier_actual stores
// the e2e policy tier, so a disagreement is malformed data, never a metric
// input.
func validateTierColumns(result CaseResult, detail e2eDetail) error {
	switch {
	case detail.PolicyTier == nil && result.TierActual == nil:
		return nil
	case detail.PolicyTier == nil:
		return fmt.Errorf("tier_actual %q disagrees with a null e2e policy tier",
			*result.TierActual)
	case result.TierActual == nil:
		return fmt.Errorf("tier_actual is null but e2e policy tier is %q", *detail.PolicyTier)
	case *result.TierActual != *detail.PolicyTier:
		return fmt.Errorf("tier_actual %q disagrees with e2e policy tier %q",
			*result.TierActual, *detail.PolicyTier)
	default:
		return nil
	}
}

func validE2ERequestStatus(status string) bool {
	switch status {
	case e2eStatusNone, e2eStatusAutoApproved, e2eStatusPending, e2eStatusApproved,
		e2eStatusApplied, e2eStatusRejected, e2eStatusExpired, e2eStatusSuperseded:
		return true
	default:
		return false
	}
}

func validE2EAction(action string) bool {
	switch action {
	case e2eActionNone, e2eActionSkipT0, e2eActionApprove, e2eActionReject, e2eActionAbstain:
		return true
	default:
		return false
	}
}

func validE2ETier(tier string) bool {
	switch tier {
	case e2ePolicyTierT0, e2ePolicyTierT1, e2ePolicyTierT2:
		return true
	default:
		return false
	}
}

func scenarioError(result CaseResult, err error) error {
	if result.ScenarioID == "" {
		return fmt.Errorf("eval/score: e2e metrics: %w", err)
	}
	return fmt.Errorf("eval/score: e2e metrics for scenario %s: %w", result.ScenarioID, err)
}
