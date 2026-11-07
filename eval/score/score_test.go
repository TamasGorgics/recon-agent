package score

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
)

func TestScoreCaseFixtures(t *testing.T) {
	for _, fixture := range caseFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			result, err := ScoreCase(fixture.truth, fixture.actual, fixture.split)
			if err != nil {
				t.Fatalf("ScoreCase: %v", err)
			}
			if result.Correct != fixture.correct {
				t.Errorf("Correct = %v, want %v", result.Correct, fixture.correct)
			}
			if result.FalseFix != fixture.falseFix {
				t.Errorf("FalseFix = %v, want %v", result.FalseFix, fixture.falseFix)
			}
			if result.Escalated != fixture.escalated {
				t.Errorf("Escalated = %v, want %v", result.Escalated, fixture.escalated)
			}
			if result.Split != string(fixture.split) {
				t.Errorf("Split = %q, want %q", result.Split, fixture.split)
			}
			if result.ScenarioID != fixture.truth.ScenarioID || result.Mode != fixture.truth.Mode {
				t.Errorf("scenario = %q/%q, want %q/%q",
					result.ScenarioID, result.Mode, fixture.truth.ScenarioID, fixture.truth.Mode)
			}
			if result.TierExpected == nil || *result.TierExpected != fixture.truth.ExpectedTier {
				t.Errorf("TierExpected = %v, want %q", result.TierExpected, fixture.truth.ExpectedTier)
			}
			gotError, _ := result.Detail[DetailError].(string)
			if gotError != fixture.errorCode {
				t.Errorf("Detail[%s] = %q, want %q", DetailError, gotError, fixture.errorCode)
			}
			if got := detailBool(result.Detail, DetailHasProposal); got != (fixture.actual.Proposal != nil) {
				t.Errorf("has_proposal = %v, want %v", got, fixture.actual.Proposal != nil)
			}
			if fixture.evidenceOK != nil {
				got := detailBool(result.Detail, DetailEvidenceValid)
				if got != *fixture.evidenceOK {
					t.Errorf("evidence_valid = %v, want %v (detail %v)",
						got, *fixture.evidenceOK, result.Detail)
				}
			}
		})
	}
}

func TestScoreCaseRejectsInvalidTruth(t *testing.T) {
	zeroVersion := strings.Repeat("0", 64)
	if err := ValidateTruth(dataset.GroundTruth{
		SchemaVersion:           "2",
		GeneratorDatasetVersion: zeroVersion,
		Scenarios:               []dataset.ScenarioTruth{truthLink()},
	}); err == nil {
		t.Fatal("ValidateTruth accepted an unknown schema version")
	}

	linkWithoutExpected := truthLink()
	linkWithoutExpected.ExpectedLink = nil

	duplicateWithoutExpected := truthDuplicate()
	duplicateWithoutExpected.ExpectedDuplicate = nil

	adjustmentWithoutExpected := truthAdjustment()
	adjustmentWithoutExpected.ExpectedAdjustment = nil

	cases := map[string]dataset.ScenarioTruth{
		"link without expected_link":             linkWithoutExpected,
		"duplicate without expected_duplicate":   duplicateWithoutExpected,
		"adjustment without expected_adjustment": adjustmentWithoutExpected,
		"legacy propose_link token": func() dataset.ScenarioTruth {
			truth := truthLink()
			truth.ExpectedResolutions = []string{"propose_link"}
			truth.PrimaryResolution = "propose_link"
			return truth
		}(),
		"empty allowed set": func() dataset.ScenarioTruth {
			truth := truthLink()
			truth.ExpectedResolutions = nil
			return truth
		}(),
		"unknown tier": func() dataset.ScenarioTruth {
			truth := truthLink()
			truth.ExpectedTier = "T9"
			return truth
		}(),
		"primary outside allowed set": func() dataset.ScenarioTruth {
			truth := truthLink()
			truth.PrimaryResolution = "escalate"
			return truth
		}(),
	}
	for name, truth := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ScoreCase(truth, escalatedActual(), dataset.Test); err == nil {
				t.Fatal("ScoreCase accepted invalid truth")
			}
		})
	}
}

// covers: FIN-05, FIN-10
func TestScoreCaseRejectsUnbalancedAndFractionalTruth(t *testing.T) {
	unbalanced := truthAdjustment()
	unbalanced.ExpectedAdjustment.Lines[1].AmountMinor = 249
	if _, err := ScoreCase(unbalanced, escalatedActual(), dataset.Test); err == nil {
		t.Fatal("ScoreCase accepted unbalanced expected adjustment lines")
	}

	fractional := proposalActual(adjustmentProposalRaw(t, "adjustment",
		`{"lines":[{"account_code":"fees_expense_usd","direction":"debit","amount_minor":250.5,"currency":"USD"},{"account_code":"clearing_psp_a_usd","direction":"credit","amount_minor":250,"currency":"USD"}]}`))
	result, err := ScoreCase(truthAdjustment(), fractional, dataset.Test)
	if err != nil {
		t.Fatalf("ScoreCase fractional amount returned a hard error: %v", err)
	}
	if result.Correct || result.Detail[DetailError] != ErrorInvalidProposalPayload {
		t.Errorf("fractional amount result = correct %v, error %v", result.Correct, result.Detail[DetailError])
	}
}

func TestScoreCaseRejectsInvalidActualMetrics(t *testing.T) {
	truth := truthLink()
	cases := []struct {
		name   string
		actual Actual
	}{
		{name: "negative cost", actual: Actual{
			Outcome: "escalated", CostUSD: decimal.RequireFromString("-0.01"),
		}},
		{name: "negative steps", actual: Actual{Outcome: "escalated", Steps: -1}},
		{name: "negative latency", actual: Actual{Outcome: "escalated", Latency: -time.Millisecond}},
		{name: "negative loop steps", actual: func() Actual {
			loop := -1
			return Actual{Outcome: "escalated", LoopSteps: &loop}
		}()},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ScoreCase(truth, test.actual, dataset.Test); !errors.Is(err, ErrInvalidActual) {
				t.Fatalf("ScoreCase error = %v, want ErrInvalidActual", err)
			}
		})
	}
	if _, err := ScoreCase(truth, escalatedActual(), dataset.Split("production")); err == nil {
		t.Fatal("ScoreCase accepted an unknown split")
	}
}

func TestLinkEquivalence(t *testing.T) {
	cases := []struct {
		name   string
		truth  dataset.ScenarioTruth
		ids    []string
		ref    string
		plural bool
		want   bool
	}{
		{name: "singular", truth: truthLink(), ids: []string{ledgerAlpha}, ref: refOf(pspRecordA), want: true},
		{name: "plural one element", truth: truthLink(), ids: []string{ledgerAlpha}, ref: refOf(pspRecordA), plural: true, want: true},
		{name: "composite exact set", truth: truthCompositeLink(),
			ids: []string{ledgerGamma, ledgerAlpha, ledgerBeta}, ref: refOf(pspRecordA), plural: true, want: true},
		{name: "composite missing leg", truth: truthCompositeLink(),
			ids: []string{ledgerAlpha, ledgerGamma}, ref: refOf(pspRecordA), plural: true},
		{name: "composite extra leg", truth: truthCompositeLink(),
			ids: []string{ledgerAlpha, ledgerBeta, ledgerGamma, ledgerDelta}, ref: refOf(pspRecordA), plural: true},
		{name: "single wrong leg", truth: truthLink(),
			ids: []string{ledgerBeta}, ref: refOf(pspRecordA)},
		{name: "wrong external", truth: truthLink(),
			ids: []string{ledgerAlpha}, ref: refOf(pspRecordB)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			proposal := &Proposal{
				Type:     "link",
				Payload:  buildLinkPayload(t, test.ids, test.ref, test.plural),
				Evidence: []string{"ledger_txn:" + test.ids[0], test.ref},
			}
			result, err := ScoreCase(test.truth, proposalActual(proposal), dataset.Test)
			if err != nil {
				t.Fatalf("ScoreCase: %v", err)
			}
			if result.Correct != test.want {
				t.Errorf("Correct = %v, want %v (%v)", result.Correct, test.want, result.Detail)
			}
		})
	}
}

func TestAdjustmentMultiset(t *testing.T) {
	base := expectedAdjustmentLines()
	cases := []struct {
		name  string
		lines []dataset.ExpectedLine
		want  bool
	}{
		{name: "exact", lines: base, want: true},
		{name: "reordered", lines: []dataset.ExpectedLine{base[1], base[0]}, want: true},
		{name: "missing line", lines: []dataset.ExpectedLine{base[0]}},
		{name: "extra line", lines: []dataset.ExpectedLine{
			base[0], base[1],
			{AccountCode: "fees_expense_usd", Direction: "credit", AmountMinor: 250, Currency: "USD"},
		}},
		{name: "duplicated line", lines: []dataset.ExpectedLine{base[0], base[0], base[1]}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			actual := proposalActual(adjustmentProposal(t, "adjustment", test.lines...))
			result, err := ScoreCase(truthAdjustment(), actual, dataset.Test)
			if err != nil {
				t.Fatalf("ScoreCase: %v", err)
			}
			if result.Correct != test.want {
				t.Errorf("Correct = %v, want %v", result.Correct, test.want)
			}
		})
	}
}

func TestFalseFixBranches(t *testing.T) {
	fixtures := caseFixtures(t)
	expectations := map[string]bool{
		"adjustment_wrong_amount":                true,
		"adjustment_fractional_amount":           true,
		"write_off_where_adjustment_expected":    true,
		"false_fix_adjustment_when_escalate":     true,
		"false_fix_adjustment_when_link":         true,
		"false_fix_adjustment_when_duplicate":    true,
		"false_fix_adjustment_when_request_info": true,
		"partial_refund_wrong_adjustment":        true,
		"link_wrong_ledger":                      false,
		"link_wrong_external":                    false,
		"duplicate_wrong_ref":                    false,
		"budget_exhausted":                       false,
		"invalid_output":                         false,
		"provider_error":                         false,
		"escalate_when_truth_link":               false,
		"partial_refund_adjustment":              false,
		"adjustment_correct":                     false,
		"write_off_allowed":                      false,
	}
	for name, want := range expectations {
		fixture := fixtureByName(t, fixtures, name)
		result, err := ScoreCase(fixture.truth, fixture.actual, fixture.split)
		if err != nil {
			t.Fatalf("%s: ScoreCase: %v", name, err)
		}
		if result.FalseFix != want {
			t.Errorf("%s: FalseFix = %v, want %v", name, result.FalseFix, want)
		}
	}
}

func TestEvidenceDetail(t *testing.T) {
	fixtures := caseFixtures(t)
	cases := map[string]struct {
		total, real, relevant int
		valid                 bool
	}{
		"evidence_empty":                   {0, 0, 0, false},
		"evidence_malformed":               {1, 0, 0, false},
		"evidence_unretrieved":             {2, 1, 2, false},
		"evidence_real_unrelated":          {1, 1, 0, false},
		"evidence_case_only":               {1, 1, 0, false},
		"evidence_real_and_relevant":       {3, 3, 1, true},
		"evidence_supporting_fx":           {2, 2, 1, true},
		"evidence_supporting_fee_schedule": {2, 2, 1, true},
		"evidence_supporting_unretrieved":  {2, 1, 1, false},
	}
	for name, want := range cases {
		fixture := fixtureByName(t, fixtures, name)
		result, err := ScoreCase(fixture.truth, fixture.actual, fixture.split)
		if err != nil {
			t.Fatalf("%s: ScoreCase: %v", name, err)
		}
		if got := result.Detail[DetailEvidenceTotal]; got != want.total {
			t.Errorf("%s: evidence_total = %v, want %d", name, got, want.total)
		}
		if got := result.Detail[DetailEvidenceReal]; got != want.real {
			t.Errorf("%s: evidence_real = %v, want %d", name, got, want.real)
		}
		if got := result.Detail[DetailEvidenceRelevant]; got != want.relevant {
			t.Errorf("%s: evidence_relevant = %v, want %d", name, got, want.relevant)
		}
		if got := detailBool(result.Detail, DetailEvidenceValid); got != want.valid {
			t.Errorf("%s: evidence_valid = %v, want %v", name, got, want.valid)
		}
	}
}

func TestDetailPayloadAndProvenance(t *testing.T) {
	confidence := decimal.RequireFromString("0.87")
	cost := &CostDetail{
		Allowance:    decimal.RequireFromString("5.00"),
		TokensIn:     1200,
		TokensOut:    300,
		CostSource:   "api_list_cost",
		CostMismatch: false,
	}
	latency := &LatencyDetail{LLMCallMS: 400, ToolCallMS: 100}
	loop := 3
	actual := proposalActual(linkProposal(t, []string{ledgerAlpha}, refOf(pspRecordA)))
	actual.Confidence = &confidence
	actual.CostUSD = decimal.RequireFromString("0.02")
	actual.Latency = mustDurationMilliseconds(t, "500")
	actual.Steps = 4
	actual.CostDetail = cost
	actual.LatencyDetail = latency
	actual.LoopSteps = &loop

	result, err := ScoreCase(truthLink(), actual, dataset.Test)
	if err != nil {
		t.Fatalf("ScoreCase: %v", err)
	}
	if result.Confidence == nil || !result.Confidence.Equal(confidence) {
		t.Errorf("Confidence = %v, want %s", result.Confidence, confidence)
	}
	if result.CostUSD.String() != "0.02" || result.Latency.Milliseconds() != 500 || result.Steps != 4 {
		t.Errorf("headline metrics = %s/%s/%d", result.CostUSD, result.Latency, result.Steps)
	}
	gotCost, ok := result.Detail[DetailCost].(CostDetail)
	if !ok || !gotCost.Allowance.Equal(cost.Allowance) || gotCost.TokensIn != 1200 {
		t.Errorf("detail cost = %#v", result.Detail[DetailCost])
	}
	gotLatency, ok := result.Detail[DetailLatency].(LatencyDetail)
	if !ok || gotLatency.LLMCallMS != 400 || gotLatency.ToolCallMS != 100 {
		t.Errorf("detail latency = %#v", result.Detail[DetailLatency])
	}
	if result.Detail[DetailLoopSteps] != 3 {
		t.Errorf("loop_steps = %v, want 3", result.Detail[DetailLoopSteps])
	}
}
