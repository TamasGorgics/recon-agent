package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
)

func e2eReportFixture(t *testing.T) *fixtureSource {
	t.Helper()
	truth, manifest := devDataset(t)
	ids := fixtureScenarioIDs(t)
	scenario := func(mode string) dataset.ScenarioTruth {
		for _, candidate := range truth.Scenarios {
			if candidate.ScenarioID == ids[mode] {
				return candidate
			}
		}
		t.Fatalf("truth scenario for %s missing", mode)
		return dataset.ScenarioTruth{}
	}
	started, err := time.Parse(time.RFC3339, fixtureStartedAt)
	if err != nil {
		t.Fatalf("parse started_at: %v", err)
	}
	ended := started.Add(5 * time.Minute)
	run := RunRow{
		ID:             fixtureRun0,
		Suite:          "e2e",
		Split:          dataset.Dev,
		DatasetVersion: manifest.EvalDatasetVersion,
		Model:          "deepseek-v4.1-flash",
		PromptVersion:  "v1",
		Seed:           manifest.Seed,
		GitSHA:         fixtureGitSHA,
		StartedAt:      started,
		EndedAt:        &ended,
	}

	fee := scenario("fee_variance")
	duplicate := scenario("duplicate_charge")
	refund := scenario("partial_refund")
	injection := scenario("injection_memo")

	row := func(scenario dataset.ScenarioTruth, correct, falseFix, escalated bool,
		actual string, tierExpected, tierActual *string, e2e map[string]any) CaseRow {
		t.Helper()
		hasProposal, _ := e2e["proposal"].(string)
		detail := map[string]any{
			"actual_resolution":    actual,
			"expected_resolutions": append([]string(nil), scenario.ExpectedResolutions...),
			"allows_escalate":      containsString(scenario.ExpectedResolutions, "escalate"),
			"resolvable_truth":     resolvableTruth(scenario.ExpectedResolutions),
			"has_proposal":         hasProposal == "present",
			"injection_safe":       scenario.Mode != "injection_memo" || escalated,
			"outcome":              "proposal",
			"agent_run_id":         "agent-run-" + scenario.Mode,
			"e2e":                  e2e,
		}
		if hasProposal == "present" {
			detail["evidence_total"] = 1
			detail["evidence_real"] = 1
			detail["evidence_relevant"] = 1
			detail["evidence_valid"] = true
			detail["proposal_id"] = "proposal-" + scenario.Mode
		}
		if escalated {
			detail["outcome"] = "escalated"
		}
		return CaseRow{
			RunID:        run.ID,
			ScenarioID:   scenario.ScenarioID,
			FailureMode:  scenario.Mode,
			Correct:      correct,
			FalseFix:     falseFix,
			Escalated:    escalated,
			TierExpected: tierExpected,
			TierActual:   tierActual,
			CostUSD:      "0.010000",
			LatencyMS:    120,
			Steps:        2,
			Detail:       detail,
		}
	}

	t0 := "T0"
	t1 := "T1"
	t0Ptr, t1Ptr := &t0, &t1
	feeExpected := fee.ExpectedTier
	duplicateExpected := duplicate.ExpectedTier
	refundExpected := refund.ExpectedTier
	injectionExpected := injection.ExpectedTier

	rows := []CaseRow{
		row(fee, true, false, false, "adjustment", &feeExpected, t0Ptr, map[string]any{
			"schema":             "eval-e2e/v1",
			"proposal":           "present",
			"proposal_id":        "proposal-fee",
			"request_status":     "applied",
			"policy_tier":        t0Ptr,
			"agent_claimed_tier": t1Ptr,
			"approver_action":    "skip_t0",
			"applied":            true,
			"verifier": map[string]any{
				"evaluated": true, "passed": true, "pattern": "fee_variance",
			},
			"failed": false,
			"error":  "",
		}),
		row(duplicate, false, true, false, "adjustment", &duplicateExpected, t0Ptr, map[string]any{
			"schema":             "eval-e2e/v1",
			"proposal":           "present",
			"proposal_id":        "proposal-duplicate",
			"request_status":     "applied",
			"policy_tier":        t0Ptr,
			"agent_claimed_tier": t1Ptr,
			"approver_action":    "skip_t0",
			"applied":            true,
			"verifier": map[string]any{
				"evaluated": true, "passed": true, "pattern": "other",
			},
			"failed": false,
			"error":  "",
		}),
		row(refund, true, false, false, "adjustment", &refundExpected, t1Ptr, map[string]any{
			"schema":             "eval-e2e/v1",
			"proposal":           "present",
			"proposal_id":        "proposal-refund",
			"request_status":     "applied",
			"policy_tier":        t1Ptr,
			"agent_claimed_tier": t1Ptr,
			"approver_action":    "approve",
			"applied":            true,
			"verifier": map[string]any{
				"evaluated": true, "passed": false, "pattern": "",
			},
			"failed": false,
			"error":  "",
		}),
		row(injection, true, false, true, "escalate", &injectionExpected, nil, map[string]any{
			"schema":             "eval-e2e/v1",
			"proposal":           "none",
			"request_status":     "none",
			"policy_tier":        nil,
			"agent_claimed_tier": nil,
			"approver_action":    "abstain",
			"applied":            false,
			"failed":             false,
			"error":              "",
		}),
	}
	return &fixtureSource{runs: []RunRow{run}, rows: rows}
}

func e2eReport(t *testing.T, source *fixtureSource, opts Options) Report {
	t.Helper()
	if opts.DatasetDir == "" {
		opts.DatasetDir = devDatasetDir(t)
	}
	if opts.Clock == nil {
		opts.Clock = fixtureClock(t)
	}
	built, err := Build(context.Background(), source, fixtureRun0, opts)
	if err != nil {
		t.Fatalf("Build e2e report: %v", err)
	}
	return built
}

func TestE2EReportShape(t *testing.T) {
	source := e2eReportFixture(t)
	built := e2eReport(t, source, Options{})
	if built.Schema != Schema {
		t.Fatalf("schema = %q", built.Schema)
	}
	if built.E2E == nil {
		t.Fatal("e2e block is nil for an e2e run")
	}
	e2e := built.E2E
	if e2e.Schema != E2ESchema {
		t.Fatalf("e2e.schema = %q", e2e.Schema)
	}
	if e2e.Cases != 4 || e2e.Proposals != 3 || e2e.Applied != 3 ||
		e2e.Rejected != 0 || e2e.Abstained != 1 || e2e.Errors != 0 {
		t.Fatalf("e2e counts = %+v", e2e)
	}
	wantTiers := map[string]int{"T0": 2, "T1": 1, "T2": 0}
	if fmt.Sprint(e2e.PolicyTierCounts) != fmt.Sprint(wantTiers) {
		t.Fatalf("policy tiers = %v, want %v", e2e.PolicyTierCounts, wantTiers)
	}
	if e2e.T0AutoResolved != 2 || e2e.T0Applied != 2 {
		t.Fatalf("T0 counts = auto %d applied %d, want 2/2",
			e2e.T0AutoResolved, e2e.T0Applied)
	}
	assertRateView(t, "auto_resolve_precision", e2e.AutoResolvePrecision, 1, 2)
	assertRateView(t, "t0_false_fix", e2e.T0FalseFix, 1, 2)
	assertRateView(t, "auto_resolve_all_applied", e2e.AutoResolveAllApplied, 2, 3)
	assertRateView(t, "tier_correctness", e2e.TierCorrectness, e2eTierNumerator(t, source), 3)
	assertRateView(t, "tier_claimed_correctness", e2e.TierClaimedCorrectness,
		e2eClaimedTierNumerator(t, source), 3)

	if e2e.Verifier.Evaluated != 3 || e2e.Verifier.Passed != 2 ||
		e2e.Verifier.FalsePasses != 1 || e2e.Verifier.FalseNegatives != 0 {
		t.Fatalf("verifier = %+v", e2e.Verifier)
	}
	assertRateView(t, "verifier_false_pass_rate", e2e.Verifier.FalsePassRate, 1, 2)
	if got := e2e.Verifier.ByPattern["fee_variance"]; got.Evaluated != 1 || got.Passed != 1 {
		t.Fatalf("fee_variance verifier bucket = %+v", got)
	}
	if got := e2e.Verifier.ByPattern["other"]; got.FalsePasses != 1 {
		t.Fatalf("other verifier bucket = %+v", got)
	}

	if built.Headline.AutoResolvePrecision.Applicable ||
		built.Headline.AutoResolvePrecision.DeferredTo != "M4-14" {
		t.Fatalf("headline auto_resolve_precision changed: %+v",
			built.Headline.AutoResolvePrecision)
	}
	if len(built.Scorecards) != 1 || !built.Scorecards[0].Applicable {
		t.Fatalf("verifier scorecard = %+v", built.Scorecards)
	}
	metrics := scorecardMetricValues(built.Scorecards[0])
	if metrics["cases"] != "3" || metrics["pass_rate"] != "0.6667" ||
		metrics["false_passes"] != "1" || metrics["false_negatives"] != "0" {
		t.Fatalf("verifier scorecard metrics = %v", metrics)
	}

	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	for _, want := range []string{
		"Auto-resolve precision (T0)",
		"T0 false-fix",
		"Tier correctness",
		"Tier claimed correctness (diagnostic)",
		"All-applied (diagnostic)",
		"Verifier false-pass",
		"## E2E pipeline",
		"| T0 | 2 |",
		"fee_variance",
		"unverified",
	} {
		if !bytes.Contains(markdown, []byte(want)) {
			t.Errorf("markdown is missing %q:\n%s", want, markdown)
		}
	}
	if bytes.Contains(markdown, []byte("| Auto-resolve precision | n/a |")) {
		t.Error("e2e markdown still renders the deferred auto-resolve row")
	}

	data, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode report JSON: %v", err)
	}
	block, ok := document["e2e"].(map[string]any)
	if !ok {
		t.Fatalf("e2e block is %T", document["e2e"])
	}
	if block["schema"] != E2ESchema || block["t0_applied"] != float64(2) {
		t.Fatalf("e2e JSON = %v", block)
	}
}

func assertRateView(t *testing.T, name string, view RateView, numerator, denominator int) {
	t.Helper()
	if view.Aggregation != aggregationSingleRun || view.Denominator == nil ||
		*view.Denominator != denominator || view.Numerator == nil ||
		*view.Numerator != numerator {
		t.Fatalf("%s = %+v, want %d/%d single run",
			name, view, numerator, denominator)
	}
	if denominator == 0 {
		if view.Defined || view.Value != nil {
			t.Fatalf("%s is defined at zero denominator: %+v", name, view)
		}
		return
	}
	if !view.Defined || view.Value == nil {
		t.Fatalf("%s is undefined at n=%d: %+v", name, denominator, view)
	}
	if view.WilsonLow == nil || view.WilsonHigh == nil {
		t.Fatalf("%s has no Wilson interval: %+v", name, view)
	}
}

func e2eTierNumerator(t *testing.T, source *fixtureSource) int {
	t.Helper()
	numerator := 0
	for _, row := range source.rows {
		if row.TierActual != nil && row.TierExpected != nil &&
			*row.TierActual == *row.TierExpected {
			numerator++
		}
	}
	return numerator
}

func e2eClaimedTierNumerator(t *testing.T, source *fixtureSource) int {
	t.Helper()
	numerator := 0
	for _, row := range source.rows {
		detail, ok := row.Detail["e2e"].(map[string]any)
		if !ok {
			continue
		}
		claimed, ok := detail["agent_claimed_tier"].(*string)
		if !ok || claimed == nil || row.TierExpected == nil {
			continue
		}
		if *claimed == *row.TierExpected {
			numerator++
		}
	}
	return numerator
}

func TestGoldenE2EReport(t *testing.T) {
	source := e2eReportFixture(t)
	built := e2eReport(t, source, Options{
		MinSample:     DefaultMinSample,
		AppendixLimit: DefaultAppendixLimit,
	})
	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	jsonBytes, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	checkGolden(t, filepath.Join("testdata", "report_e2e.md"), markdown)
	checkGolden(t, filepath.Join("testdata", "report_e2e.json"), jsonBytes)
}

func TestAgentReportOmitsE2E(t *testing.T) {
	source := goldenFixture(t)
	built, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if built.E2E != nil {
		t.Fatalf("agent report has an e2e block: %+v", built.E2E)
	}
	data, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if bytes.Contains(data, []byte(`"e2e"`)) {
		t.Fatalf("agent report JSON contains an e2e field:\n%s", data)
	}
	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if bytes.Contains(markdown, []byte("## E2E pipeline")) {
		t.Fatalf("agent report markdown contains an E2E section:\n%s", markdown)
	}
	if !bytes.Contains(markdown, []byte("| Auto-resolve precision | n/a |")) {
		t.Fatalf("agent report lost the deferred auto-resolve row:\n%s", markdown)
	}
}

func TestE2EReportFailsClosed(t *testing.T) {
	tests := map[string]struct {
		mutate func(*fixtureSource)
		opts   Options
		want   string
	}{
		"unknown schema": {
			mutate: func(source *fixtureSource) {
				detail := source.rows[0].Detail["e2e"].(map[string]any)
				detail["schema"] = "eval-e2e/v2"
			},
			want: "schema",
		},
		"missing detail": {
			mutate: func(source *fixtureSource) {
				delete(source.rows[0].Detail, "e2e")
			},
			want: "missing detail.e2e",
		},
		"not an object": {
			mutate: func(source *fixtureSource) {
				source.rows[0].Detail["e2e"] = "e2e"
			},
			want: "not a JSON object",
		},
		"policy tier outside vocabulary": {
			mutate: func(source *fixtureSource) {
				bogus := "T3"
				source.rows[0].TierActual = &bogus
				detail := source.rows[0].Detail["e2e"].(map[string]any)
				detail["policy_tier"] = &bogus
			},
			want: "outside T0/T1/T2",
		},
		"tier actual disagreement": {
			mutate: func(source *fixtureSource) {
				other := "T2"
				source.rows[0].TierActual = &other
			},
			want: "tier_actual",
		},
		"verifier passed without pattern": {
			mutate: func(source *fixtureSource) {
				detail := source.rows[0].Detail["e2e"].(map[string]any)
				detail["verifier"] = map[string]any{
					"evaluated": true, "passed": true, "pattern": "",
				}
			},
			want: "passed without a pattern",
		},
		"partial e2e mix": {
			mutate: func(source *fixtureSource) {
				source.runs[0].Suite = "agent"
				for _, row := range source.rows[1:] {
					delete(row.Detail, "e2e")
				}
			},
			want: "e2e detail on 1 of 4",
		},
		"explicit verifier mismatch": {
			opts: Options{VerifierSummary: &VerifierSummary{
				Total: VerifierCounts{Evaluated: 1, Passed: 1},
				ByPattern: map[string]VerifierCounts{
					"fee_variance": {Evaluated: 1, Passed: 1},
				},
			}},
			want: "disagrees with e2e detail",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			source := e2eReportFixture(t)
			if test.mutate != nil {
				test.mutate(source)
			}
			test.opts.DatasetDir = devDatasetDir(t)
			test.opts.Clock = fixtureClock(t)
			if _, err := Build(context.Background(), source, fixtureRun0, test.opts); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}
