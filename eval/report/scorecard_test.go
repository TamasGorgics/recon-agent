package report

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/policy/verify"
)

func scorecardMetricValues(card Scorecard) map[string]string {
	values := make(map[string]string, len(card.Metrics))
	for _, metric := range card.Metrics {
		values[metric.Name] = metric.Value
	}
	return values
}

func TestVerifierScorecardNotApplicable(t *testing.T) {
	source := goldenFixture(t)
	built, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(built.Scorecards) != 1 {
		t.Fatalf("scorecards = %+v", built.Scorecards)
	}
	card := built.Scorecards[0]
	if card.Name != "verifier" || card.Applicable {
		t.Fatalf("verifier scorecard = %+v, want an inapplicable verifier card", card)
	}
	if card.DeferredTo != "" {
		t.Fatalf("verifier scorecard deferred_to = %q, want empty", card.DeferredTo)
	}
	if card.Reason != "no verifier outcomes in this run" {
		t.Fatalf("verifier scorecard reason = %q", card.Reason)
	}
	if card.Cases != 0 || card.Metrics == nil || len(card.Metrics) != 0 {
		t.Fatalf("verifier scorecard = %+v, want 0 cases and an empty metrics array", card)
	}
	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("verifier: not applicable (no verifier outcomes in this run)")) {
		t.Fatalf("markdown missing not-applicable verifier card:\n%s", markdown)
	}
	if bytes.Contains(markdown, []byte("until M4-05")) {
		t.Fatalf("markdown still carries the M4-05 placeholder:\n%s", markdown)
	}
}

func TestVerifierScorecardSupplied(t *testing.T) {
	summary := &VerifierSummary{
		Total: VerifierCounts{
			Evaluated: 10, Passed: 6, FalsePasses: 0, FalseNegatives: 1,
		},
		ByPattern: map[string]VerifierCounts{
			"reference_typo": {Evaluated: 4, Passed: 2, FalsePasses: 0, FalseNegatives: 1},
			"fee_variance":   {Evaluated: 6, Passed: 4, FalsePasses: 0, FalseNegatives: 0},
		},
	}
	source := goldenFixture(t)
	opts := Options{
		DatasetDir:      devDatasetDir(t),
		Clock:           fixtureClock(t),
		VerifierSummary: summary,
	}
	built, err := Build(context.Background(), source, fixtureRun1, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(built.Scorecards) != 1 {
		t.Fatalf("scorecards = %+v", built.Scorecards)
	}
	card := built.Scorecards[0]
	if !card.Applicable || card.DeferredTo != "" || card.Reason != "" {
		t.Fatalf("verifier scorecard = %+v, want applicable with no deferral/reason", card)
	}
	if card.Cases != 10 {
		t.Fatalf("scorecard cases = %d, want 10", card.Cases)
	}
	values := scorecardMetricValues(card)
	wantValues := map[string]string{
		"cases":                          "10",
		"pass_rate":                      "0.6000",
		"false_passes":                   "0",
		"false_negatives":                "1",
		"fee_variance.cases":             "6",
		"fee_variance.pass_rate":         "0.6667",
		"fee_variance.false_passes":      "0",
		"fee_variance.false_negatives":   "0",
		"reference_typo.cases":           "4",
		"reference_typo.pass_rate":       "0.5000",
		"reference_typo.false_passes":    "0",
		"reference_typo.false_negatives": "1",
	}
	for name, want := range wantValues {
		if got := values[name]; got != want {
			t.Fatalf("metric %q = %q, want %q", name, got, want)
		}
	}
	wantOrder := []string{
		"cases", "pass_rate", "false_passes", "false_negatives",
		"fee_variance.cases", "fee_variance.pass_rate",
		"fee_variance.false_passes", "fee_variance.false_negatives",
		"reference_typo.cases", "reference_typo.pass_rate",
		"reference_typo.false_passes", "reference_typo.false_negatives",
	}
	if len(card.Metrics) != len(wantOrder) {
		t.Fatalf("metrics = %+v, want %d entries", card.Metrics, len(wantOrder))
	}
	for index, name := range wantOrder {
		if card.Metrics[index].Name != name {
			t.Fatalf("metric[%d] = %q, want %q", index, card.Metrics[index].Name, name)
		}
	}

	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	for _, want := range []string{
		"- verifier: 10 cases",
		"  - cases: 10 cases evaluated verifier outcomes",
		"  - pass_rate: 0.6000 ratio passed / evaluated",
		"  - false_passes: 0 cases verified pass, ground-truth fail",
		"  - fee_variance.cases: 6 cases evaluated verifier outcomes",
		"  - reference_typo.false_negatives: 1 cases verified fail, ground-truth pass",
	} {
		if !bytes.Contains(markdown, []byte(want)) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}

	jsonBytes, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !bytes.Contains(jsonBytes, []byte(`"applicable": true`)) ||
		!bytes.Contains(jsonBytes, []byte(`"name": "fee_variance.false_passes"`)) {
		t.Fatalf("JSON missing supplied verifier scorecard:\n%s", jsonBytes)
	}

	// A map-backed summary must render identically across builds.
	again, err := Build(context.Background(), goldenFixture(t), fixtureRun1, opts)
	if err != nil {
		t.Fatalf("Build again: %v", err)
	}
	againMarkdown, err := RenderMarkdown(again)
	if err != nil {
		t.Fatalf("RenderMarkdown again: %v", err)
	}
	if !bytes.Equal(markdown, againMarkdown) {
		t.Fatalf("markdown is not deterministic:\nfirst:\n%s\nsecond:\n%s", markdown, againMarkdown)
	}
	againJSON, err := RenderJSON(again)
	if err != nil {
		t.Fatalf("RenderJSON again: %v", err)
	}
	if !bytes.Equal(jsonBytes, againJSON) {
		t.Fatalf("JSON is not deterministic:\nfirst:\n%s\nsecond:\n%s", jsonBytes, againJSON)
	}
}

func TestVerifierScorecardNilVersusEmpty(t *testing.T) {
	nilCard, err := verifierScorecard(nil)
	if err != nil {
		t.Fatalf("verifierScorecard(nil): %v", err)
	}
	if nilCard.Applicable {
		t.Fatalf("nil summary card = %+v, want not applicable", nilCard)
	}
	if nilCard.Reason != "no verifier outcomes in this run" || nilCard.DeferredTo != "" {
		t.Fatalf("nil summary card = %+v", nilCard)
	}

	emptyCard, err := verifierScorecard(&VerifierSummary{})
	if err != nil {
		t.Fatalf("verifierScorecard(empty): %v", err)
	}
	if !emptyCard.Applicable {
		t.Fatalf("empty summary card = %+v, want applicable with zero cases", emptyCard)
	}
	if emptyCard.Cases != 0 || emptyCard.DeferredTo != "" || emptyCard.Reason != "" {
		t.Fatalf("empty summary card = %+v", emptyCard)
	}
	values := scorecardMetricValues(emptyCard)
	if values["cases"] != "0" || values["pass_rate"] != "n/a" ||
		values["false_passes"] != "0" || values["false_negatives"] != "0" {
		t.Fatalf("empty summary metrics = %+v", emptyCard.Metrics)
	}
	if len(emptyCard.Metrics) != 4 {
		t.Fatalf("empty summary metrics = %+v, want four total metrics", emptyCard.Metrics)
	}
}

func TestVerifierScorecardRejectsInvalidCounts(t *testing.T) {
	tests := map[string]*VerifierSummary{
		"negative evaluated": {
			Total: VerifierCounts{Evaluated: -1},
		},
		"passed exceeds evaluated": {
			Total: VerifierCounts{Evaluated: 1, Passed: 2},
		},
		"false passes exceed passed": {
			Total: VerifierCounts{Evaluated: 2, Passed: 1, FalsePasses: 2},
		},
		"false negatives exceed failed": {
			Total: VerifierCounts{Evaluated: 2, Passed: 1, FalseNegatives: 2},
		},
		"total does not match patterns": {
			Total: VerifierCounts{Evaluated: 2, Passed: 1},
			ByPattern: map[string]VerifierCounts{
				"fee_variance": {Evaluated: 1, Passed: 1},
			},
		},
		"empty pattern name": {
			ByPattern: map[string]VerifierCounts{"": {}},
		},
		"pattern whitespace": {
			ByPattern: map[string]VerifierCounts{" fee_variance": {}},
		},
	}
	for name, summary := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := verifierScorecard(summary); err == nil {
				t.Fatal("invalid verifier summary did not fail")
			}
		})
	}

	source := goldenFixture(t)
	_, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir:      devDatasetDir(t),
		Clock:           fixtureClock(t),
		VerifierSummary: &VerifierSummary{Total: VerifierCounts{Evaluated: -1}},
	})
	if err == nil || !strings.Contains(err.Error(), "verifier summary") {
		t.Fatalf("Build invalid summary error = %v", err)
	}
}

type verifierScorecardCase struct {
	pattern  string
	wantPass bool
	result   verify.Result
}

// verifierGroundTruthCases exercises one pass and one adversarial fail for each
// pure evaluator. They are deterministic fixtures, not a dev/test dataset run:
// live population from durable proposal rows (and seeded fx_rates) is deferred
// to the eval harness and tracked in Q-89.
func verifierGroundTruthCases() []verifierScorecardCase {
	at := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	lines := func(total int64, currency string) []verify.AdjustmentLine {
		return []verify.AdjustmentLine{
			{AccountCode: "clearing", Direction: "debit", AmountMinor: total, Currency: currency},
			{AccountCode: "fees_expense_psp_a", Direction: "credit", AmountMinor: total, Currency: currency},
		}
	}

	fee := verify.FeeVarianceInput{
		Claimed: verify.PatternFeeVariance,
		External: verify.ExternalRecord{
			Source: "psp_a", SourceRecordID: "ext-1", Kind: "charge",
			Currency: "USD", AmountMinor: 100_000, FeeMinor: 20_000, OccurredAt: at,
		},
		Ledger: verify.LedgerTxn{
			ID: "ledger-1", ValueDate: at, Currency: "USD", FeeMinor: 42, NetMinor: 119_958,
		},
		Schedules: []verify.FeeSchedule{{
			ID: "schedule-1", Source: "psp_a", Kind: "charge", Currency: "USD",
			PctBPS: 1, FixedMinor: 30,
			ValidFrom: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
			ValidTo:   time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC),
		}},
		Lines: lines(19_958, "USD"),
	}
	feeFail := fee
	feeFail.Ledger.FeeMinor = 43

	fx := verify.FXRoundingInput{
		Claimed: verify.PatternFXRounding,
		External: verify.ExternalRecord{
			Source: "psp_a", SourceRecordID: "ext-fx-1", Kind: "fx",
			Currency: "EUR", AmountMinor: 91, OccurredAt: at,
		},
		Ledger: verify.LedgerTxn{
			ID: "ledger-fx-1", ValueDate: at, Currency: "EUR", NetMinor: 90,
		},
		Rates: []verify.FXRate{{
			Base: "USD", Quote: "EUR", Source: "psp_a", Rate: "0.9",
			AsOf: at.Add(-24 * time.Hour),
		}},
		Lines: lines(1, "EUR"),
	}
	fxFail := fx
	fxFail.Ledger.NetMinor = 89

	rounding := verify.RoundingInput{
		Claimed: verify.PatternRounding,
		External: verify.ExternalRecord{
			Source: "psp_a", SourceRecordID: "ext-round-1", Kind: "charge",
			Currency: "USD", AmountMinor: 1_000, OccurredAt: at,
		},
		Ledger: verify.LedgerTxn{
			ID: "ledger-round-1", ValueDate: at, Currency: "USD", NetMinor: 999,
		},
		Lines: lines(1, "USD"),
	}
	roundingFail := rounding
	roundingFail.Ledger.NetMinor = 998

	reference := verify.ReferenceTypoInput{
		Claimed: verify.PatternReferenceTypo,
		External: verify.ExternalRecord{
			Source: "psp_a", SourceRecordID: "ext-ref-1", Kind: "charge",
			Currency: "USD", AmountMinor: 1_000, OccurredAt: at,
		},
		Candidates: []verify.Candidate{{
			LedgerTxnID: "ledger-ref-1", AmountMinor: 1_000,
			Currency: "USD", ValueDate: at.Add(48 * time.Hour),
		}},
		ProposedLedgerIDs: []string{"ledger-ref-1"},
	}
	referenceFail := reference
	referenceFail.Candidates = append(
		append([]verify.Candidate(nil), reference.Candidates...),
		verify.Candidate{
			LedgerTxnID: "ledger-ref-2", AmountMinor: 1_000,
			Currency: "USD", ValueDate: at.Add(48 * time.Hour),
		},
	)

	opts := verify.DefaultOptions()
	return []verifierScorecardCase{
		{verify.PatternFeeVariance, true, verify.VerifyFeeVariance(fee, opts)},
		{verify.PatternFeeVariance, false, verify.VerifyFeeVariance(feeFail, opts)},
		{verify.PatternFXRounding, true, verify.VerifyFXRounding(fx, opts)},
		{verify.PatternFXRounding, false, verify.VerifyFXRounding(fxFail, opts)},
		{verify.PatternRounding, true, verify.VerifyRounding(rounding, opts)},
		{verify.PatternRounding, false, verify.VerifyRounding(roundingFail, opts)},
		{verify.PatternReferenceTypo, true, verify.VerifyReferenceTypo(reference, opts)},
		{verify.PatternReferenceTypo, false, verify.VerifyReferenceTypo(referenceFail, opts)},
	}
}

func TestVerifierFixturesHaveZeroFalsePasses(t *testing.T) {
	// This is a deterministic fixture/contract check, not a live dev/test
	// population run. Durable per-run outcomes and fx_rates seeding are the
	// eval harness's job and are recorded in Q-89.
	summary := &VerifierSummary{ByPattern: map[string]VerifierCounts{}}
	for _, test := range verifierGroundTruthCases() {
		if test.result.Passed != test.wantPass {
			t.Fatalf("%s fixture: Passed = %t, want %t (reasons %v)",
				test.pattern, test.result.Passed, test.wantPass, test.result.Details.Reasons)
		}
		counts := summary.ByPattern[test.pattern]
		counts.Evaluated++
		if test.result.Passed {
			counts.Passed++
			if !test.wantPass {
				counts.FalsePasses++
			}
		} else if test.wantPass {
			counts.FalseNegatives++
		}
		summary.ByPattern[test.pattern] = counts
	}
	for _, counts := range summary.ByPattern {
		summary.Total.add(counts)
	}

	if summary.Total.Evaluated != 8 || summary.Total.Passed != 4 ||
		summary.Total.FalsePasses != 0 || summary.Total.FalseNegatives != 0 {
		t.Fatalf("summary total = %+v, want 8 evaluated / 4 passed / 0 false passes / 0 false negatives",
			summary.Total)
	}
	wantPatterns := map[string]VerifierCounts{
		verify.PatternFeeVariance:   {Evaluated: 2, Passed: 1},
		verify.PatternFXRounding:    {Evaluated: 2, Passed: 1},
		verify.PatternRounding:      {Evaluated: 2, Passed: 1},
		verify.PatternReferenceTypo: {Evaluated: 2, Passed: 1},
	}
	for pattern, want := range wantPatterns {
		if got := summary.ByPattern[pattern]; got != want {
			t.Fatalf("%s counts = %+v, want %+v", pattern, got, want)
		}
	}

	card, err := verifierScorecard(summary)
	if err != nil {
		t.Fatalf("verifierScorecard: %v", err)
	}
	values := scorecardMetricValues(card)
	if values["false_passes"] != "0" || values["false_negatives"] != "0" {
		t.Fatalf("scorecard false pass/negative = %q/%q, want 0/0",
			values["false_passes"], values["false_negatives"])
	}
	if values["pass_rate"] != "0.5000" {
		t.Fatalf("scorecard pass_rate = %q, want 0.5000", values["pass_rate"])
	}
	for _, pattern := range []string{
		verify.PatternFeeVariance, verify.PatternFXRounding,
		verify.PatternRounding, verify.PatternReferenceTypo,
	} {
		if got := values[pattern+".false_passes"]; got != "0" {
			t.Fatalf("%s false_passes = %q, want 0", pattern, got)
		}
	}
}
