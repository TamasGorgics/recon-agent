package component

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/matchscore"
	"github.com/TamasGorgics/recon-agent/synth"
)

var updateGolden = flag.Bool("update", false, "update eval/component golden fixtures")

func matcherFixture(t *testing.T) (dataset.GroundTruth, []matchscore.MatchGroup) {
	t.Helper()
	external := func(source, id string) synth.RecordRef {
		return synth.RecordRef{Source: source, SourceRecordID: id}
	}
	truth := dataset.GroundTruth{
		SchemaVersion: "1",
		Scenarios: []dataset.ScenarioTruth{
			{
				ScenarioID:          "scenario-fee",
				Mode:                "fee_variance",
				Difficulty:          "realistic",
				BreakSignature:      "sig-fee",
				ExpectedResolutions: []string{"link", "adjustment"},
				PrimaryResolution:   "adjustment",
				ExternalRecords:     []synth.RecordRef{external("psp_a", "rec-a")},
				LedgerTxnIDs:        []string{"ledger-a"},
			},
			{
				ScenarioID:          "scenario-duplicate",
				Mode:                "duplicate_charge",
				Difficulty:          "realistic",
				BreakSignature:      "sig-duplicate",
				ExpectedResolutions: []string{"link", "duplicate"},
				PrimaryResolution:   "duplicate",
				ExternalRecords:     []synth.RecordRef{external("psp_a", "rec-b")},
				LedgerTxnIDs:        []string{"ledger-b"},
			},
			{
				ScenarioID:          "scenario-composite",
				Mode:                "batch_composite",
				Difficulty:          "realistic",
				BreakSignature:      "sig-composite",
				ExpectedResolutions: []string{"link"},
				PrimaryResolution:   "link",
				ExternalRecords:     []synth.RecordRef{external("bank_x", "rec-c")},
				LedgerTxnIDs:        []string{"ledger-c1", "ledger-c2"},
			},
			{
				ScenarioID:          "scenario-ambiguous",
				Mode:                "ambiguous",
				Difficulty:          "realistic",
				BreakSignature:      "sig-ambiguous",
				ExpectedResolutions: []string{"escalate", "link"},
				PrimaryResolution:   "escalate",
				ExternalRecords:     []synth.RecordRef{external("psp_a", "rec-ambiguous")},
				LedgerTxnIDs:        []string{"ledger-ambiguous"},
			},
			{
				ScenarioID:          "scenario-injection",
				Mode:                "injection_memo",
				Difficulty:          "adversarial",
				BreakSignature:      "sig-injection",
				ExpectedResolutions: []string{"escalate"},
				PrimaryResolution:   "escalate",
				ExternalRecords:     []synth.RecordRef{external("psp_a", "rec-e")},
				LedgerTxnIDs:        []string{"ledger-e"},
			},
			{
				ScenarioID:          "scenario-no-link",
				Mode:                "unknown_deposit",
				Difficulty:          "realistic",
				BreakSignature:      "sig-no-link",
				ExpectedResolutions: []string{"adjustment"},
				PrimaryResolution:   "adjustment",
				ExternalRecords:     []synth.RecordRef{external("psp_a", "rec-f")},
				LedgerTxnIDs:        []string{"ledger-f"},
			},
			{
				ScenarioID:          "scenario-no-external",
				Mode:                "missing_settlement",
				Difficulty:          "realistic",
				ExpectedResolutions: []string{"link"},
				PrimaryResolution:   "link",
				LedgerTxnIDs:        []string{"ledger-ne"},
			},
			{
				ScenarioID:          "scenario-no-ledger",
				Mode:                "late_settlement",
				Difficulty:          "realistic",
				ExpectedResolutions: []string{"link"},
				PrimaryResolution:   "link",
				ExternalRecords:     []synth.RecordRef{external("psp_a", "rec-g")},
			},
		},
	}
	groups := []matchscore.MatchGroup{
		{
			MatchID:      "match-1",
			ScopeID:      "scope-1",
			Rule:         "exact",
			External:     match.RecordRef{Source: "psp_a", SourceRecordID: "rec-a"},
			LedgerTxnIDs: []string{"ledger-a"},
		},
		{
			MatchID:      "match-2",
			ScopeID:      "scope-1",
			Rule:         "exact",
			External:     match.RecordRef{Source: "psp_a", SourceRecordID: "rec-b"},
			LedgerTxnIDs: []string{"ledger-other"},
		},
		{
			MatchID:      "match-3",
			ScopeID:      "scope-1",
			Rule:         "fuzzy",
			External:     match.RecordRef{Source: "bank_x", SourceRecordID: "rec-c"},
			LedgerTxnIDs: []string{"ledger-c2", "ledger-c1"},
		},
		{
			MatchID:      "match-4",
			ScopeID:      "scope-1",
			Rule:         "exact",
			External:     match.RecordRef{Source: "psp_a", SourceRecordID: "rec-e"},
			LedgerTxnIDs: []string{"ledger-e"},
		},
	}
	return truth, groups
}

func TestAdaptTruthScoresLikeSynthTruth(t *testing.T) {
	evalTruth := dataset.GroundTruth{
		SchemaVersion: "1",
		Scenarios: []dataset.ScenarioTruth{
			{
				ScenarioID:          "scenario-fee",
				Mode:                "fee_variance",
				ExpectedResolutions: []string{"link", "adjustment"},
				PrimaryResolution:   "adjustment",
				ExternalRecords:     []synth.RecordRef{{Source: "psp_a", SourceRecordID: "rec-a"}},
				LedgerTxnIDs:        []string{"ledger-a"},
			},
			{
				ScenarioID:          "scenario-injection",
				Mode:                "injection_memo",
				ExpectedResolutions: []string{"escalate"},
				PrimaryResolution:   "escalate",
				ExternalRecords:     []synth.RecordRef{{Source: "psp_a", SourceRecordID: "rec-e"}},
				LedgerTxnIDs:        []string{"ledger-e"},
			},
		},
	}
	synthTruth := synth.GroundTruth{
		DatasetVersion: "fixture-version",
		Scenarios: []synth.Scenario{
			{
				ScenarioID:          "scenario-fee",
				Mode:                "fee_variance",
				ExpectedResolutions: []string{"link", "adjustment"},
				PrimaryResolution:   "adjustment",
				ExternalRecords:     []synth.RecordRef{{Source: "psp_a", SourceRecordID: "rec-a"}},
				LedgerTxnIDs:        []string{"ledger-a"},
			},
			{
				ScenarioID:          "scenario-injection",
				Mode:                "injection_memo",
				ExpectedResolutions: []string{"escalate"},
				PrimaryResolution:   "escalate",
				ExternalRecords:     []synth.RecordRef{{Source: "psp_a", SourceRecordID: "rec-e"}},
				LedgerTxnIDs:        []string{"ledger-e"},
			},
		},
	}
	groups := []matchscore.MatchGroup{{
		MatchID:      "match-1",
		Rule:         "exact",
		External:     match.RecordRef{Source: "psp_a", SourceRecordID: "rec-a"},
		LedgerTxnIDs: []string{"ledger-a"},
	}}
	direct := matchscore.Score(groups, synthTruth)
	adapted := matchscore.Score(groups, AdaptTruth("fixture-version", evalTruth))
	if !reflect.DeepEqual(direct, adapted) {
		t.Fatalf("adapter changed matchscore semantics:\ndirect=%+v\nadapted=%+v", direct, adapted)
	}
}

func TestNewMatcherReportConvertsMatchscoreResult(t *testing.T) {
	truth, groups := matcherFixture(t)
	direct := matchscore.Score(groups, AdaptTruth("fixture-version", truth))
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	report := NewMatcherReport(dataset.Dev, "fixture-version", truth, direct, now)
	if report.Schema != MatcherSchema || report.Split != dataset.Dev ||
		report.DatasetVersion != "fixture-version" ||
		!report.GeneratedAt.Equal(now) {
		t.Fatalf("report metadata = %+v", report)
	}
	if report.Scenarios != 8 {
		t.Fatalf("scenarios = %d, want 8", report.Scenarios)
	}
	if report.ScoreableScenarios != 6 {
		t.Fatalf("scoreable scenarios = %d, want 6", report.ScoreableScenarios)
	}
	if report.Matches != 4 || report.CorrectMatches != 2 {
		t.Fatalf("matches = %d/%d", report.CorrectMatches, report.Matches)
	}
	if !report.Precision.Defined || report.Precision.Numerator != 2 ||
		report.Precision.Denominator != 4 || report.Precision.Value == nil {
		t.Fatalf("precision = %+v", report.Precision)
	}
	if *report.Precision.Value != 0.5 {
		t.Fatalf("precision value = %v", *report.Precision.Value)
	}
	if report.ExpectedLinks != 3 || report.RecalledLinks != 2 {
		t.Fatalf("recall = %d/%d", report.RecalledLinks, report.ExpectedLinks)
	}
	if !report.Recall.Defined || report.Recall.Value == nil ||
		*report.Recall.Value != 2.0/3.0 {
		t.Fatalf("recall = %+v", report.Recall)
	}
	if len(report.PerPass) != 2 ||
		report.PerPass[0].Pass != "exact" || report.PerPass[1].Pass != "fuzzy" {
		t.Fatalf("per_pass = %+v", report.PerPass)
	}
	exact := report.PerPass[0]
	if exact.Matches != 3 || exact.CorrectMatches != 1 || exact.RequiredHits != 1 ||
		exact.Precision.Value == nil || *exact.Precision.Value != 1.0/3.0 ||
		exact.Recall.Value == nil || *exact.Recall.Value != 1.0/3.0 {
		t.Fatalf("exact pass = %+v", exact)
	}
	fuzzy := report.PerPass[1]
	if fuzzy.Matches != 1 || fuzzy.CorrectMatches != 1 || fuzzy.RequiredHits != 1 ||
		fuzzy.Precision.Value == nil || *fuzzy.Precision.Value != 1 ||
		fuzzy.Recall.Value == nil || *fuzzy.Recall.Value != 1.0/3.0 {
		t.Fatalf("fuzzy pass = %+v", fuzzy)
	}
	wantExcluded := map[string]int{
		"ambiguous":          1,
		"escalate":           1,
		"no_external_record": 1,
		"no_ledger_txn":      1,
		"not_expected_link":  1,
	}
	if !reflect.DeepEqual(report.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", report.Excluded, wantExcluded)
	}
	if len(report.Warnings) != 1 ||
		!strings.Contains(report.Warnings[0], "recall below 1.0") {
		t.Fatalf("warnings = %+v", report.Warnings)
	}
}

func TestNewMatcherReportZeroDenominators(t *testing.T) {
	truth := dataset.GroundTruth{
		SchemaVersion: "1",
		Scenarios: []dataset.ScenarioTruth{{
			ScenarioID:          "scenario-injection",
			Mode:                "injection_memo",
			Difficulty:          "adversarial",
			ExpectedResolutions: []string{"escalate"},
			PrimaryResolution:   "escalate",
			ExternalRecords:     []synth.RecordRef{{Source: "psp_a", SourceRecordID: "rec-e"}},
			LedgerTxnIDs:        []string{"ledger-e"},
		}},
	}
	result := matchscore.Score(nil, AdaptTruth("fixture-version", truth))
	report := NewMatcherReport(dataset.Injection, "fixture-version", truth, result,
		time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))

	if report.Matches != 0 || report.CorrectMatches != 0 {
		t.Fatalf("matches = %d/%d", report.CorrectMatches, report.Matches)
	}
	if report.Precision.Defined || report.Precision.Value != nil {
		t.Fatalf("zero-match precision = %+v, want n/a", report.Precision)
	}
	if report.ExpectedLinks != 0 || report.RecalledLinks != 0 {
		t.Fatalf("links = %d/%d", report.RecalledLinks, report.ExpectedLinks)
	}
	if report.Recall.Defined || report.Recall.Value != nil {
		t.Fatalf("zero-link recall = %+v, want n/a", report.Recall)
	}
	if len(report.PerPass) != 0 || report.PerPass == nil {
		t.Fatalf("per_pass = %+v, want empty", report.PerPass)
	}
	if report.Excluded["escalate"] != 1 {
		t.Fatalf("excluded = %+v", report.Excluded)
	}
	if len(report.Warnings) != 2 {
		t.Fatalf("warnings = %+v, want precision and recall warnings", report.Warnings)
	}

	markdown, err := RenderMarkdown(report)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("| Precision | n/a | n/a |")) ||
		!bytes.Contains(markdown, []byte("| Recall | n/a | n/a |")) {
		t.Fatalf("markdown lost n/a rates:\n%s", markdown)
	}
	if !bytes.Contains(markdown, []byte("No active passes.")) {
		t.Fatalf("markdown missing empty per-pass note:\n%s", markdown)
	}
}

func TestGoldenMatcher(t *testing.T) {
	truth, groups := matcherFixture(t)
	result := matchscore.Score(groups, AdaptTruth("fixture-version", truth))
	report := NewMatcherReport(dataset.Dev, "fixture-version", truth, result,
		time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	markdown, err := RenderMarkdown(report)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	jsonBytes, err := RenderJSON(report)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	checkMatcherGolden(t, filepath.Join("testdata", "matcher.md"), markdown)
	checkMatcherGolden(t, filepath.Join("testdata", "matcher.json"), jsonBytes)
}

func checkMatcherGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden directory: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run go test ./eval/component -run TestGoldenMatcher -update): %v",
			path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden %s drifted\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}
