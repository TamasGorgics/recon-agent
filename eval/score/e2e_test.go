package score

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// e2eCaseSpec is one hand-built e2e case result. The detail mirrors the
// eval-e2e/v1 fields the scorer consumes.
type e2eCaseSpec struct {
	proposal      string
	requestStatus string
	policyTier    *string
	claimedTier   *string
	expectedTier  *string
	action        string
	applied       bool
	correct       bool
	falseFix      bool
	failed        bool
	verifier      map[string]any
}

func e2eString(value string) *string { return &value }

func e2eCases(t *testing.T, specs []e2eCaseSpec) []CaseResult {
	t.Helper()
	results := make([]CaseResult, 0, len(specs))
	for index, spec := range specs {
		detail := map[string]any{
			"schema":             E2ESchemaVersion,
			"proposal":           spec.proposal,
			"request_status":     spec.requestStatus,
			"policy_tier":        spec.policyTier,
			"agent_claimed_tier": spec.claimedTier,
			"approver_action":    spec.action,
			"applied":            spec.applied,
			"failed":             spec.failed,
			"error":              "",
		}
		if spec.failed {
			detail["error"] = "synthetic failure"
		}
		if spec.verifier != nil {
			detail["verifier"] = spec.verifier
		}
		results = append(results, CaseResult{
			ScenarioID:   fmt.Sprintf("e2e-scenario-%02d", index),
			Correct:      spec.correct,
			FalseFix:     spec.falseFix,
			TierExpected: spec.expectedTier,
			TierActual:   spec.policyTier,
			Detail:       map[string]any{DetailE2E: detail},
		})
	}
	return results
}

func e2eVerifierDetail(passed bool, pattern string) map[string]any {
	return map[string]any{
		"evaluated": true,
		"passed":    passed,
		"pattern":   pattern,
	}
}

func e2ePrecisionCases(t *testing.T) []CaseResult {
	t.Helper()
	t0 := e2eString("T0")
	t1 := e2eString("T1")
	t2 := e2eString("T2")
	return e2eCases(t, []e2eCaseSpec{
		{proposal: "present", requestStatus: "applied", policyTier: t0,
			claimedTier: t1, expectedTier: t0, action: "skip_t0",
			applied: true, correct: true},
		{proposal: "present", requestStatus: "applied", policyTier: t0,
			claimedTier: t1, expectedTier: t0, action: "skip_t0",
			applied: true, correct: true},
		{proposal: "present", requestStatus: "applied", policyTier: t0,
			claimedTier: t1, expectedTier: t0, action: "skip_t0",
			applied: true, correct: false, falseFix: true},
		{proposal: "present", requestStatus: "auto_approved", policyTier: t0,
			claimedTier: t1, expectedTier: t0, action: "skip_t0",
			applied: false, failed: true},
		{proposal: "present", requestStatus: "applied", policyTier: t1,
			claimedTier: t1, expectedTier: t1, action: "approve",
			applied: true, correct: true},
		{proposal: "present", requestStatus: "applied", policyTier: t2,
			claimedTier: t1, expectedTier: t2, action: "approve",
			applied: true, correct: false},
		{proposal: "present", requestStatus: "rejected", policyTier: t1,
			claimedTier: t1, expectedTier: t1, action: "reject"},
		{proposal: "none", requestStatus: "none", action: "abstain", correct: true},
	})
}

func e2eVerifierCases(t *testing.T) []CaseResult {
	t.Helper()
	t1 := e2eString("T1")
	return e2eCases(t, []e2eCaseSpec{
		{proposal: "present", requestStatus: "applied", policyTier: t1,
			expectedTier: t1, action: "approve", applied: true,
			correct: false, falseFix: true,
			verifier: e2eVerifierDetail(true, "fee_variance")},
		{proposal: "present", requestStatus: "applied", policyTier: t1,
			expectedTier: t1, action: "approve", applied: true,
			correct:  true,
			verifier: e2eVerifierDetail(true, "fee_variance")},
		{proposal: "present", requestStatus: "applied", policyTier: t1,
			expectedTier: t1, action: "approve", applied: true,
			correct: false, falseFix: true,
			verifier: e2eVerifierDetail(true, "other")},
		{proposal: "present", requestStatus: "applied", policyTier: t1,
			expectedTier: t1, action: "approve", applied: true,
			correct: false, falseFix: true,
			verifier: e2eVerifierDetail(false, "")},
	})
}

func e2eTierCases(t *testing.T) []CaseResult {
	t.Helper()
	t0 := e2eString("T0")
	t1 := e2eString("T1")
	t2 := e2eString("T2")
	return e2eCases(t, []e2eCaseSpec{
		{proposal: "present", requestStatus: "applied", policyTier: t0,
			claimedTier: t1, expectedTier: t0, action: "skip_t0", applied: true, correct: true},
		{proposal: "present", requestStatus: "applied", policyTier: t1,
			claimedTier: t1, expectedTier: t1, action: "approve", applied: true, correct: true},
		{proposal: "present", requestStatus: "applied", policyTier: t1,
			claimedTier: t1, expectedTier: t2, action: "approve", applied: true, correct: false},
		{proposal: "present", requestStatus: "applied", policyTier: t2,
			claimedTier: t2, expectedTier: t2, action: "approve", applied: true, correct: true},
		{proposal: "none", requestStatus: "none", claimedTier: nil,
			expectedTier: t1, action: "abstain", correct: true},
		{proposal: "present", requestStatus: "auto_approved", claimedTier: t1,
			expectedTier: t2, action: "skip_t0", failed: true},
		{proposal: "present", requestStatus: "applied", policyTier: t2,
			claimedTier: t2, expectedTier: nil, action: "approve", applied: true, correct: true},
	})
}

func TestAggregateE2EPrecisionAndDiagnostics(t *testing.T) {
	metrics, err := AggregateE2E(e2ePrecisionCases(t))
	if err != nil {
		t.Fatalf("AggregateE2E: %v", err)
	}
	if metrics.Cases != 8 || metrics.Proposals != 7 || metrics.Applied != 5 ||
		metrics.Rejected != 1 || metrics.Abstained != 1 || metrics.Errors != 1 {
		t.Fatalf("counts = %+v", metrics)
	}
	wantTiers := map[string]int{"T0": 4, "T1": 2, "T2": 1}
	if !reflect.DeepEqual(metrics.PolicyTierCounts, wantTiers) {
		t.Fatalf("policy_tier_counts = %v, want %v", metrics.PolicyTierCounts, wantTiers)
	}
	if metrics.T0AutoResolved != 4 || metrics.T0Applied != 3 {
		t.Fatalf("T0 counts = auto %d applied %d, want 4/3",
			metrics.T0AutoResolved, metrics.T0Applied)
	}
	checks := []struct {
		name string
		got  Rate
		want Rate
	}{
		{"auto_resolve_precision", metrics.AutoResolvePrecision, wantRate(2, 4)},
		{"t0_false_fix", metrics.T0FalseFix, wantRate(1, 3)},
		{"auto_resolve_all_applied", metrics.AutoResolveAllApplied, wantRate(3, 5)},
	}
	for _, check := range checks {
		if !reflect.DeepEqual(check.got, check.want) {
			t.Errorf("%s = %s, want %s", check.name,
				describeRate(check.got), describeRate(check.want))
		}
	}
}

func TestAggregateE2EUndefinedDenominators(t *testing.T) {
	t1 := e2eString("T1")

	t.Run("empty", func(t *testing.T) {
		metrics, err := AggregateE2E(nil)
		if err != nil {
			t.Fatalf("AggregateE2E: %v", err)
		}
		if metrics.Cases != 0 || metrics.Proposals != 0 || metrics.T0AutoResolved != 0 ||
			metrics.T0Applied != 0 {
			t.Fatalf("empty counts = %+v", metrics)
		}
		for name, rate := range map[string]Rate{
			"auto_resolve_precision":   metrics.AutoResolvePrecision,
			"t0_false_fix":             metrics.T0FalseFix,
			"auto_resolve_all_applied": metrics.AutoResolveAllApplied,
			"tier_correctness":         metrics.TierCorrectness,
			"tier_claimed_correctness": metrics.TierClaimedCorrectness,
			"verifier_false_pass":      metrics.Verifier.FalsePassRate,
		} {
			if rate.Defined || rate.Value != nil || rate.Denominator != 0 {
				t.Errorf("%s = %+v, want undefined", name, rate)
			}
		}
		if len(metrics.Verifier.ByPattern) != 0 {
			t.Errorf("by_pattern = %v, want empty", metrics.Verifier.ByPattern)
		}
	})

	t.Run("no t0", func(t *testing.T) {
		metrics, err := AggregateE2E(e2eCases(t, []e2eCaseSpec{{
			proposal: "present", requestStatus: "applied", policyTier: t1,
			expectedTier: t1, action: "approve", applied: true, correct: true,
		}}))
		if err != nil {
			t.Fatalf("AggregateE2E: %v", err)
		}
		if metrics.AutoResolvePrecision.Defined || metrics.T0FalseFix.Defined {
			t.Fatalf("T0 rates = %+v / %+v, want undefined",
				metrics.AutoResolvePrecision, metrics.T0FalseFix)
		}
		if got := metrics.AutoResolveAllApplied; !reflect.DeepEqual(got, wantRate(1, 1)) {
			t.Fatalf("all applied = %s, want 1/1", describeRate(got))
		}
	})

	t.Run("t0 never applied", func(t *testing.T) {
		t0 := e2eString("T0")
		metrics, err := AggregateE2E(e2eCases(t, []e2eCaseSpec{
			{proposal: "present", requestStatus: "auto_approved", policyTier: t0,
				expectedTier: t0, action: "skip_t0", failed: true},
			{proposal: "present", requestStatus: "auto_approved", policyTier: t0,
				expectedTier: t0, action: "skip_t0", failed: true},
		}))
		if err != nil {
			t.Fatalf("AggregateE2E: %v", err)
		}
		if got := metrics.AutoResolvePrecision; !reflect.DeepEqual(got, wantRate(0, 2)) {
			t.Fatalf("precision = %s, want a defined 0/2, never a pass",
				describeRate(got))
		}
		if metrics.T0FalseFix.Defined || metrics.T0FalseFix.Value != nil {
			t.Fatalf("t0_false_fix = %+v, want undefined at zero applied", metrics.T0FalseFix)
		}
		if metrics.T0Applied != 0 || metrics.T0AutoResolved != 2 {
			t.Fatalf("T0 counts = %+v", metrics)
		}
	})
}

func TestAggregateE2EVerifierFalsePass(t *testing.T) {
	metrics, err := AggregateE2E(e2eVerifierCases(t))
	if err != nil {
		t.Fatalf("AggregateE2E: %v", err)
	}
	verifier := metrics.Verifier
	if verifier.Evaluated != 4 || verifier.Passed != 3 || verifier.FalsePasses != 2 ||
		verifier.FalseNegatives != 0 {
		t.Fatalf("verifier counts = %+v", verifier)
	}
	if got := verifier.FalsePassRate; !reflect.DeepEqual(got, wantRate(2, 3)) {
		t.Fatalf("false pass rate = %s, want 2/3", describeRate(got))
	}
	wantPatterns := map[string]E2EVerifierCounts{
		"fee_variance":       {Evaluated: 2, Passed: 2, FalsePasses: 1},
		"other":              {Evaluated: 1, Passed: 1, FalsePasses: 1},
		E2EUnverifiedPattern: {Evaluated: 1, Passed: 0, FalsePasses: 0},
	}
	if !reflect.DeepEqual(verifier.ByPattern, wantPatterns) {
		t.Fatalf("by_pattern = %+v, want %+v", verifier.ByPattern, wantPatterns)
	}

	t.Run("no passes is undefined", func(t *testing.T) {
		t1 := e2eString("T1")
		metrics, err := AggregateE2E(e2eCases(t, []e2eCaseSpec{{
			proposal: "present", requestStatus: "applied", policyTier: t1,
			expectedTier: t1, action: "approve", applied: true, correct: true,
			verifier: e2eVerifierDetail(false, ""),
		}}))
		if err != nil {
			t.Fatalf("AggregateE2E: %v", err)
		}
		if metrics.Verifier.Evaluated != 1 || metrics.Verifier.Passed != 0 {
			t.Fatalf("verifier counts = %+v", metrics.Verifier)
		}
		if metrics.Verifier.FalsePassRate.Defined || metrics.Verifier.FalsePassRate.Value != nil {
			t.Fatalf("false pass rate = %+v, want undefined", metrics.Verifier.FalsePassRate)
		}
	})
}

func TestAggregateE2ETierCorrectness(t *testing.T) {
	metrics, err := AggregateE2E(e2eTierCases(t))
	if err != nil {
		t.Fatalf("AggregateE2E: %v", err)
	}
	if got := metrics.TierCorrectness; !reflect.DeepEqual(got, wantRate(3, 4)) {
		t.Fatalf("tier correctness = %s, want 3/4", describeRate(got))
	}
	if got := metrics.TierClaimedCorrectness; !reflect.DeepEqual(got, wantRate(2, 5)) {
		t.Fatalf("claimed tier correctness = %s, want 2/5", describeRate(got))
	}
}

func TestAggregateE2EMalformedDetail(t *testing.T) {
	present := "present"
	t0 := e2eString("T0")
	base := func() CaseResult {
		return CaseResult{
			ScenarioID:   "malformed",
			TierExpected: t0,
			TierActual:   t0,
			Detail: map[string]any{DetailE2E: map[string]any{
				"schema":          E2ESchemaVersion,
				"proposal":        present,
				"request_status":  "applied",
				"policy_tier":     t0,
				"approver_action": "skip_t0",
				"applied":         true,
			}},
		}
	}
	tests := map[string]struct {
		mutate func(*CaseResult)
		want   string
	}{
		"missing detail": {
			mutate: func(result *CaseResult) { result.Detail = map[string]any{} },
			want:   "missing detail.e2e",
		},
		"nil detail": {
			mutate: func(result *CaseResult) {
				result.Detail = map[string]any{DetailE2E: nil}
			},
			want: "missing detail.e2e",
		},
		"not an object": {
			mutate: func(result *CaseResult) {
				result.Detail = map[string]any{DetailE2E: "e2e"}
			},
			want: "not a JSON object",
		},
		"unknown schema": {
			mutate: func(result *CaseResult) {
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["schema"] = "eval-e2e/v2"
			},
			want: "schema",
		},
		"unknown proposal": {
			mutate: func(result *CaseResult) {
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["proposal"] = "maybe"
			},
			want: "proposal",
		},
		"policy tier outside vocabulary": {
			mutate: func(result *CaseResult) {
				bogus := "T3"
				result.TierActual = &bogus
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["policy_tier"] = bogus
			},
			want: "outside T0/T1/T2",
		},
		"tier actual disagreement": {
			mutate: func(result *CaseResult) {
				other := "T1"
				result.TierActual = &other
			},
			want: "tier_actual",
		},
		"tier actual null with policy tier": {
			mutate: func(result *CaseResult) { result.TierActual = nil },
			want:   "tier_actual is null",
		},
		"policy tier on no proposal": {
			mutate: func(result *CaseResult) {
				result.TierActual = nil
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["proposal"] = "none"
				detail["request_status"] = "none"
				detail["applied"] = false
			},
			want: "no proposal but carries policy tier",
		},
		"applied without applied status": {
			mutate: func(result *CaseResult) {
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["request_status"] = "auto_approved"
			},
			want: "applied but request_status",
		},
		"verifier not evaluated": {
			mutate: func(result *CaseResult) {
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["verifier"] = map[string]any{
					"evaluated": false, "passed": false, "pattern": "",
				}
			},
			want: "not evaluated",
		},
		"verifier passed without pattern": {
			mutate: func(result *CaseResult) {
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["verifier"] = e2eVerifierDetail(true, "")
			},
			want: "passed without a pattern",
		},
		"verifier failed with pattern": {
			mutate: func(result *CaseResult) {
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["verifier"] = e2eVerifierDetail(false, "fee_variance")
			},
			want: "failed but carries pattern",
		},
		"unknown request status": {
			mutate: func(result *CaseResult) {
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["request_status"] = "done"
				detail["applied"] = false
			},
			want: "request_status",
		},
		"unknown action": {
			mutate: func(result *CaseResult) {
				detail := result.Detail[DetailE2E].(map[string]any)
				detail["approver_action"] = "approve-ish"
			},
			want: "approver_action",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := base()
			test.mutate(&result)
			if _, err := AggregateE2E([]CaseResult{result}); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestGoldenE2EMetrics(t *testing.T) {
	precision, err := AggregateE2E(e2ePrecisionCases(t))
	if err != nil {
		t.Fatalf("precision metrics: %v", err)
	}
	verifier, err := AggregateE2E(e2eVerifierCases(t))
	if err != nil {
		t.Fatalf("verifier metrics: %v", err)
	}
	tiers, err := AggregateE2E(e2eTierCases(t))
	if err != nil {
		t.Fatalf("tier metrics: %v", err)
	}
	empty, err := AggregateE2E(nil)
	if err != nil {
		t.Fatalf("empty metrics: %v", err)
	}
	t1 := e2eString("T1")
	noPasses, err := AggregateE2E(e2eCases(t, []e2eCaseSpec{{
		proposal: "present", requestStatus: "applied", policyTier: t1,
		expectedTier: t1, action: "approve", applied: true, correct: true,
		verifier: e2eVerifierDetail(false, ""),
	}}))
	if err != nil {
		t.Fatalf("no-pass metrics: %v", err)
	}
	golden := map[string]E2EMetrics{
		"empty":     empty,
		"precision": precision,
		"tiers":     tiers,
		"verifier":  verifier,
		"no_passes": noPasses,
	}
	checkGolden(t, filepath.Join("testdata", "e2e_reports.json"), golden)
}
