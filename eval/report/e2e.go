package report

import (
	"fmt"

	"github.com/TamasGorgics/recon-agent/eval/score"
)

// E2ESchema is the schema identifier of the additive eval-report/v1 e2e
// block. The surrounding report schema stays eval-report/v1 (A2): the block is
// omitted entirely for agent-suite reports.
const E2ESchema = "eval-e2e-report/v1"

// e2eSuite is the canonical eval_runs.suite value for an e2e run.
const e2eSuite = "e2e"

// E2EBlock is the additive e2e section of eval-report/v1. Every rate is a
// per-standalone-run rate (repeat groups are out of scope in M4-14).
type E2EBlock struct {
	Schema                 string         `json:"schema"`
	Cases                  int            `json:"cases"`
	Proposals              int            `json:"proposals"`
	Applied                int            `json:"applied"`
	Rejected               int            `json:"rejected"`
	Abstained              int            `json:"abstained"`
	Errors                 int            `json:"errors"`
	PolicyTierCounts       map[string]int `json:"policy_tier_counts"`
	T0AutoResolved         int            `json:"t0_auto_resolved"`
	T0Applied              int            `json:"t0_applied"`
	T0FalseFix             RateView       `json:"t0_false_fix"`
	AutoResolvePrecision   RateView       `json:"auto_resolve_precision"`
	AutoResolveAllApplied  RateView       `json:"auto_resolve_all_applied"`
	TierCorrectness        RateView       `json:"tier_correctness"`
	TierClaimedCorrectness RateView       `json:"tier_claimed_correctness"`
	Verifier               VerifierE2E    `json:"verifier"`
}

// VerifierE2E is the e2e verifier summary. Evaluated counts every verifier
// outcome; Passed counts accepted outcomes; FalsePasses are accepted outcomes
// whose effect was a false fix. FalseNegatives is always zero in M4-14
// (Q-98 residual). ByPattern is keyed by pattern_verified, with failed
// outcomes in the reserved Unverified bucket because pattern_verified is null
// on a failure.
type VerifierE2E struct {
	Evaluated      int                       `json:"evaluated"`
	Passed         int                       `json:"passed"`
	FalsePasses    int                       `json:"false_passes"`
	FalseNegatives int                       `json:"false_negatives"`
	FalsePassRate  RateView                  `json:"false_pass_rate"`
	ByPattern      map[string]VerifierCounts `json:"by_pattern"`
}

// buildE2EBlock aggregates the e2e metrics for one standalone run. It also
// returns the verifier summary that fills the existing report verifier
// scorecard. nil means the run recorded no verifier outcomes at all.
func buildE2EBlock(data runReport, minSample int) (*E2EBlock, *VerifierSummary, error) {
	metrics, err := score.AggregateE2E(data.results)
	if err != nil {
		return nil, nil, err
	}
	block := &E2EBlock{
		Schema:                 E2ESchema,
		Cases:                  metrics.Cases,
		Proposals:              metrics.Proposals,
		Applied:                metrics.Applied,
		Rejected:               metrics.Rejected,
		Abstained:              metrics.Abstained,
		Errors:                 metrics.Errors,
		PolicyTierCounts:       cloneTierCounts(metrics.PolicyTierCounts),
		T0AutoResolved:         metrics.T0AutoResolved,
		T0Applied:              metrics.T0Applied,
		T0FalseFix:             singleRateView(metrics.T0FalseFix, minSample),
		AutoResolvePrecision:   singleRateView(metrics.AutoResolvePrecision, minSample),
		AutoResolveAllApplied:  singleRateView(metrics.AutoResolveAllApplied, minSample),
		TierCorrectness:        singleRateView(metrics.TierCorrectness, minSample),
		TierClaimedCorrectness: singleRateView(metrics.TierClaimedCorrectness, minSample),
		Verifier: VerifierE2E{
			Evaluated:      metrics.Verifier.Evaluated,
			Passed:         metrics.Verifier.Passed,
			FalsePasses:    metrics.Verifier.FalsePasses,
			FalseNegatives: metrics.Verifier.FalseNegatives,
			FalsePassRate:  singleRateView(metrics.Verifier.FalsePassRate, minSample),
			ByPattern:      verifierCountsByPattern(metrics.Verifier.ByPattern),
		},
	}
	summary, err := verifierSummaryFromE2E(block.Verifier)
	if err != nil {
		return nil, nil, err
	}
	return block, summary, nil
}

func cloneTierCounts(source map[string]int) map[string]int {
	out := map[string]int{"T0": 0, "T1": 0, "T2": 0}
	for tier, count := range source {
		out[tier] = count
	}
	return out
}

func verifierCountsByPattern(source map[string]score.E2EVerifierCounts) map[string]VerifierCounts {
	out := make(map[string]VerifierCounts, len(source))
	for pattern, counts := range source {
		out[pattern] = VerifierCounts{
			Evaluated:      counts.Evaluated,
			Passed:         counts.Passed,
			FalsePasses:    counts.FalsePasses,
			FalseNegatives: counts.FalseNegatives,
		}
	}
	return out
}

// verifierSummaryFromE2E converts the e2e verifier projection into the existing
// report VerifierSummary. The existing contract requires Total to equal the
// exact sum of ByPattern, so the total is rebuilt from the buckets rather than
// trusting the flat counts. A nil summary means no verifier outcomes.
func verifierSummaryFromE2E(verifier VerifierE2E) (*VerifierSummary, error) {
	if len(verifier.ByPattern) == 0 {
		return nil, nil
	}
	summary := &VerifierSummary{ByPattern: verifier.ByPattern}
	for pattern, counts := range verifier.ByPattern {
		if pattern == "" {
			return nil, fmt.Errorf("eval/report: e2e verifier pattern is empty")
		}
		summary.Total.add(counts)
	}
	if summary.Total.Evaluated != verifier.Evaluated ||
		summary.Total.Passed != verifier.Passed ||
		summary.Total.FalsePasses != verifier.FalsePasses {
		return nil, fmt.Errorf(
			"eval/report: e2e verifier counts disagree with the per-pattern buckets: total %+v, flat %d evaluated/%d passed/%d false passes",
			summary.Total, verifier.Evaluated, verifier.Passed, verifier.FalsePasses)
	}
	return summary, nil
}

// verifierSummariesEqual compares an explicit Options.VerifierSummary with the
// summary derived from e2e detail. For an e2e run the two are the same
// evidence; a disagreement is a hard error rather than a silent override.
func verifierSummariesEqual(left, right *VerifierSummary) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if left.Total != right.Total || len(left.ByPattern) != len(right.ByPattern) {
		return false
	}
	for pattern, counts := range left.ByPattern {
		other, ok := right.ByPattern[pattern]
		if !ok || other != counts {
			return false
		}
	}
	return true
}

// e2eReportRequired decides whether this run renders the additive e2e block.
// The run suite is authoritative; the defensive fallback accepts a run whose
// every case row carries an e2e detail even if the suite column is stale. A
// partial mix is malformed and fails closed.
func e2eReportRequired(run RunRow, rows []CaseRow) (bool, error) {
	if run.Suite == e2eSuite {
		return true, nil
	}
	if len(rows) == 0 {
		return false, nil
	}
	withDetail := 0
	for _, row := range rows {
		if _, ok := row.Detail[score.DetailE2E]; ok {
			withDetail++
		}
	}
	switch withDetail {
	case 0:
		return false, nil
	case len(rows):
		return true, nil
	default:
		return false, fmt.Errorf(
			"eval/report: run %s has e2e detail on %d of %d case results",
			run.ID, withDetail, len(rows))
	}
}
