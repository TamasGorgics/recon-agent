package matchscore_test

import (
	"math"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/matchscore"
	"github.com/TamasGorgics/recon-agent/synth"
)

func scenario(id, mode string, resolutions []string, primary string,
	ledger []string, refs ...synth.RecordRef) synth.Scenario {
	return synth.Scenario{
		ScenarioID:          id,
		Mode:                mode,
		ExpectedResolutions: resolutions,
		PrimaryResolution:   primary,
		LedgerTxnIDs:        ledger,
		ExternalRecords:     refs,
	}
}

func ref(source, id string) synth.RecordRef {
	return synth.RecordRef{Source: source, SourceRecordID: id}
}

func truth(scenarios ...synth.Scenario) synth.GroundTruth {
	return synth.GroundTruth{DatasetVersion: "test", Scenarios: scenarios}
}

func group(id, rule, source, record string, ledger ...string) matchscore.MatchGroup {
	return matchscore.MatchGroup{
		MatchID:      id,
		Rule:         rule,
		External:     match.RecordRef{Source: source, SourceRecordID: record},
		LedgerTxnIDs: ledger,
	}
}

func approx(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

// TestScorePerfect pins the metric definitions on a hand-built perfect run.
func TestScorePerfect(t *testing.T) {
	gt := truth(
		scenario("clean-1", "clean", []string{"link"}, "link", []string{"l1"}, ref("psp_a", "r1")),
		scenario("clean-2", "clean", []string{"link"}, "link", []string{"l2"}, ref("psp_a", "r2")),
		scenario("partial-1", "partial_refund", []string{"link", "adjustment"}, "adjustment",
			[]string{"l3"}, ref("psp_a", "r3")),
		scenario("fee-1", "fee_variance", []string{"adjustment"}, "adjustment",
			[]string{"l4"}, ref("psp_a", "r4")),
		scenario("amb-1", "ambiguous", []string{"request_info", "escalate"}, "request_info",
			[]string{"l5", "l6"}, ref("psp_a", "ramb")),
	)
	matches := []matchscore.MatchGroup{
		group("m1", "exact", "psp_a", "r1", "l1"),
		group("m2", "fuzzy", "psp_a", "r2", "l2"),
		group("m3", "reference_tolerance", "psp_a", "r3", "l3"),
		group("m4", "reference_tolerance", "psp_a", "r4", "l4"),
	}
	result := matchscore.Score(matches, gt)

	approx(t, result.Precision, 1, "precision")
	approx(t, result.Recall, 1, "recall")
	if result.Matches != 4 || result.CorrectMatches != 4 {
		t.Errorf("matches = %d/%d, want 4/4", result.CorrectMatches, result.Matches)
	}
	if result.ExpectedLinks != 3 || result.RecalledLinks != 3 {
		t.Errorf("links = %d/%d, want 3/3", result.RecalledLinks, result.ExpectedLinks)
	}
	if result.Excluded["ambiguous"] != 1 {
		t.Errorf("ambiguous exclusions = %d, want 1", result.Excluded["ambiguous"])
	}
	exact := result.PerPass["exact"]
	if exact.Matches != 1 || exact.CorrectMatches != 1 {
		t.Errorf("exact pass = %+v", exact)
	}
	approx(t, exact.Precision, 1, "exact precision")
	approx(t, exact.Recall, 1.0/3.0, "exact recall")
}

// TestScoreMissing pins recall when some expected links are not matched.
func TestScoreMissing(t *testing.T) {
	gt := truth(
		scenario("clean-1", "clean", []string{"link"}, "link", []string{"l1"}, ref("psp_a", "r1")),
		scenario("clean-2", "clean", []string{"link"}, "link", []string{"l2"}, ref("psp_a", "r2")),
		scenario("partial-1", "partial_refund", []string{"link", "adjustment"}, "adjustment",
			[]string{"l3"}, ref("psp_a", "r3")),
	)
	matches := []matchscore.MatchGroup{
		group("m1", "exact", "psp_a", "r1", "l1"),
	}
	result := matchscore.Score(matches, gt)

	approx(t, result.Precision, 1, "precision")
	approx(t, result.Recall, 1.0/3.0, "recall")
	if result.RecalledLinks != 1 || result.ExpectedLinks != 3 {
		t.Errorf("links = %d/%d, want 1/3", result.RecalledLinks, result.ExpectedLinks)
	}
}

// TestScoreFalsePositive pins precision when a match has no ground-truth
// counterpart or claims the wrong ledger set.
func TestScoreFalsePositive(t *testing.T) {
	gt := truth(
		scenario("clean-1", "clean", []string{"link"}, "link", []string{"l1"}, ref("psp_a", "r1")),
		scenario("clean-2", "clean", []string{"link"}, "link", []string{"l2"}, ref("psp_a", "r2")),
	)
	matches := []matchscore.MatchGroup{
		group("m1", "exact", "psp_a", "r1", "l1"),
		group("m2", "exact", "psp_a", "r2", "l1"),       // wrong ledger set
		group("m3", "fuzzy", "psp_a", "surprise", "l9"), // no ground truth
	}
	result := matchscore.Score(matches, gt)

	approx(t, result.Precision, 1.0/3.0, "precision")
	approx(t, result.Recall, 0.5, "recall")
	if result.CorrectMatches != 1 || result.Matches != 3 {
		t.Errorf("matches = %d/%d, want 1/3", result.CorrectMatches, result.Matches)
	}
}

// TestScoreExcludesAmbiguousAndEscalations pins the documented exclusion:
// ambiguous/escalate scenarios never enter the recall denominator.
func TestScoreExcludesAmbiguousAndEscalations(t *testing.T) {
	gt := truth(
		scenario("clean-1", "clean", []string{"link"}, "link", []string{"l1"}, ref("psp_a", "r1")),
		scenario("amb-1", "ambiguous", []string{"request_info", "escalate"}, "request_info",
			[]string{"l2"}, ref("psp_a", "r2")),
		scenario("esc-1", "chargeback", []string{"escalate"}, "escalate",
			[]string{"l3"}, ref("psp_a", "r3")),
	)
	result := matchscore.Score(nil, gt)

	if result.ExpectedLinks != 1 {
		t.Errorf("expected links = %d, want 1 (ambiguous and escalate excluded)", result.ExpectedLinks)
	}
	if result.Excluded["ambiguous"] != 1 {
		t.Errorf("ambiguous exclusions = %d, want 1", result.Excluded["ambiguous"])
	}
	if result.Excluded["escalate"] != 1 {
		t.Errorf("escalate exclusions = %d, want 1", result.Excluded["escalate"])
	}
	approx(t, result.Precision, 1, "precision with no matches")
	approx(t, result.Recall, 0, "recall")
}

// TestScoreAcceptsHD3PartialRefund proves a partial_refund link is accepted:
// HD-3 allows either link or adjustment, and the scorer must not require the
// primary resolution.
func TestScoreAcceptsHD3PartialRefund(t *testing.T) {
	gt := truth(
		scenario("partial-1", "partial_refund", []string{"link", "adjustment"}, "adjustment",
			[]string{"l1"}, ref("psp_a", "r1")),
	)
	matches := []matchscore.MatchGroup{
		group("m1", "reference_tolerance", "psp_a", "r1", "l1"),
	}
	result := matchscore.Score(matches, gt)
	approx(t, result.Precision, 1, "precision")
	approx(t, result.Recall, 1, "recall")
}

// TestScoreCompositeNeedsExactLedgerSet pins that a composite match is correct
// only when it claims exactly the ground-truth leg set.
func TestScoreCompositeNeedsExactLedgerSet(t *testing.T) {
	gt := truth(
		scenario("composite-1", "batch_composite", []string{"link"}, "link",
			[]string{"l1", "l2", "l3"}, ref("psp_a", "payout")),
	)
	good := matchscore.Score([]matchscore.MatchGroup{
		group("m1", "composite_batch", "psp_a", "payout", "l3", "l1", "l2"),
	}, gt)
	approx(t, good.Precision, 1, "composite precision")
	approx(t, good.Recall, 1, "composite recall")

	bad := matchscore.Score([]matchscore.MatchGroup{
		group("m1", "composite_batch", "psp_a", "payout", "l1", "l2"),
	}, gt)
	approx(t, bad.Precision, 0, "subset composite precision")
	approx(t, bad.Recall, 0, "subset composite recall")
}
