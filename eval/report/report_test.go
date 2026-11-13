package report

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
)

var updateGolden = flag.Bool("update", false, "update eval/report golden fixtures")

func TestGoldenReport(t *testing.T) {
	source := goldenFixture(t)
	report, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir:    devDatasetDir(t),
		Clock:         fixtureClock(t),
		MinSample:     DefaultMinSample,
		AppendixLimit: DefaultAppendixLimit,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	markdown, err := RenderMarkdown(report)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	jsonBytes, err := RenderJSON(report)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	checkGolden(t, filepath.Join("testdata", "report.md"), markdown)
	checkGolden(t, filepath.Join("testdata", "report.json"), jsonBytes)
}

func TestBuildReportShape(t *testing.T) {
	source := goldenFixture(t)
	built, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if built.Schema != Schema {
		t.Fatalf("schema = %q", built.Schema)
	}
	if built.Run.ID != fixtureRun1 {
		t.Fatalf("run id = %q, want requested %q", built.Run.ID, fixtureRun1)
	}
	if len(built.Runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(built.Runs))
	}
	for index, run := range built.Runs {
		if run.RepeatIndex == nil || *run.RepeatIndex != index {
			t.Fatalf("runs[%d] repeat index = %v", index, run.RepeatIndex)
		}
	}
	if built.Dataset.Matched != true || built.Dataset.Split != "dev" {
		t.Fatalf("dataset = %+v", built.Dataset)
	}
	if built.Headline.Cases != 12 {
		t.Fatalf("headline cases = %d, want 12", built.Headline.Cases)
	}
	accuracy := built.Headline.Accuracy
	if !accuracy.Defined || accuracy.Aggregation != aggregationMeanOfRepeats {
		t.Fatalf("accuracy = %+v", accuracy)
	}
	if accuracy.Numerator != nil || accuracy.Denominator != nil {
		t.Fatalf("repeat accuracy must not carry a pooled denominator: %+v", accuracy)
	}
	if accuracy.Value == nil || *accuracy.Value < 0.66 || *accuracy.Value > 0.67 {
		t.Fatalf("accuracy mean = %v", accuracy.Value)
	}
	if accuracy.Repeat == nil || accuracy.Repeat.K != 3 {
		t.Fatalf("accuracy repeat = %+v", accuracy.Repeat)
	}
	if accuracy.Repeat.Stddev == nil {
		t.Fatal("accuracy repeat stddev is nil, want k=3 spread")
	}
	if accuracy.WilsonLow != nil || accuracy.WilsonHigh != nil {
		t.Fatal("repeat mean must not carry a Wilson interval")
	}
	if accuracy.MinSampleMet {
		t.Fatal("accuracy min_sample_met = true for a below-30 group")
	}
	for _, perRun := range accuracy.Repeat.PerRun {
		if perRun.WilsonLow == nil || perRun.WilsonHigh == nil {
			t.Fatalf("per-run accuracy interval missing: %+v", perRun)
		}
	}
	if built.Headline.InjectionResistance.Defined {
		t.Fatal("injection resistance defined on a dev run")
	}
	if built.Headline.InjectionResistance.Value != nil {
		t.Fatal("undefined injection resistance has a value")
	}
	if _, ok := built.ByMode["fee_variance"]; !ok {
		t.Fatalf("by_mode = %v", built.ByMode)
	}
	if _, ok := built.ByDifficulty["realistic"]; !ok {
		t.Fatalf("by_difficulty = %v", built.ByDifficulty)
	}
	if built.Calibration == nil || !built.Calibration.Applicable {
		t.Fatalf("calibration = %+v, want an applicable M3-06 section", built.Calibration)
	}
	if len(built.Scorecards) != 1 || built.Scorecards[0].Name != "verifier" {
		t.Fatalf("scorecards = %v, want only the verifier card", built.Scorecards)
	}
	if built.Scorecards[0].Applicable ||
		built.Scorecards[0].Reason != "no verifier outcomes in this run" {
		t.Fatalf("verifier scorecard = %+v", built.Scorecards[0])
	}
	if len(built.FailureAppendix) != 4 {
		t.Fatalf("appendix = %d, want 4", len(built.FailureAppendix))
	}
	wantOrder := []string{
		fixtureScenarioIDs(t)["duplicate_charge"],
		fixtureScenarioIDs(t)["fee_variance"],
		fixtureScenarioIDs(t)["injection_memo"],
		fixtureScenarioIDs(t)["partial_refund"],
	}
	for index, want := range wantOrder {
		if built.FailureAppendix[index].ScenarioID != want {
			t.Fatalf("appendix[%d] = %s, want %s", index,
				built.FailureAppendix[index].ScenarioID, want)
		}
	}
	if built.FailureAppendix[0].TranscriptStatus != transcriptStatusAvailable {
		t.Fatalf("appendix[0] transcript = %s", built.FailureAppendix[0].TranscriptStatus)
	}
	if built.FailureAppendix[1].TranscriptStatus != transcriptStatusUnavailable {
		t.Fatalf("appendix[1] transcript = %s", built.FailureAppendix[1].TranscriptStatus)
	}
	if built.FailureAppendix[2].TranscriptStatus != transcriptStatusUnsupported {
		t.Fatalf("appendix[2] transcript = %s", built.FailureAppendix[2].TranscriptStatus)
	}
	for _, entry := range built.FailureAppendix {
		if entry.RepeatsFailed != 1 || entry.Repeats != 3 {
			t.Fatalf("appendix repeats = %d/%d, want 1/3", entry.RepeatsFailed, entry.Repeats)
		}
	}

	markdown, err := RenderMarkdown(built)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !bytes.Contains(markdown, []byte("## Headline")) ||
		!bytes.Contains(markdown, []byte("## Failure appendix")) {
		t.Fatalf("markdown missing sections:\n%s", markdown)
	}
	jsonBytes, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	for _, forbidden := range []string{`"args"`, `"result"`, `"raw"`, `"memo"`, `"dsn"`, `"api_key"`} {
		if bytes.Contains(jsonBytes, []byte(forbidden)) {
			t.Fatalf("JSON contains forbidden key %s", forbidden)
		}
	}
}

func TestBuildZeroCases(t *testing.T) {
	truth, manifest := devDataset(t)
	_ = truth
	run := fixtureBaseRun(t, 0, 0)
	run.RepeatGroup = ""
	run.RepeatIndex = nil
	source := &fixtureSource{runs: []RunRow{run}}
	built, err := Build(context.Background(), source, run.ID, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build zero cases: %v", err)
	}
	if built.Headline.Cases != 0 {
		t.Fatalf("cases = %d", built.Headline.Cases)
	}
	if built.Headline.Accuracy.Defined || built.Headline.FalseFix.Defined {
		t.Fatalf("zero-case rates are defined: %+v", built.Headline.Accuracy)
	}
	if built.Headline.Cost.Defined || built.Headline.Latency.Defined {
		t.Fatalf("zero-case cost/latency defined")
	}
	if len(built.Warnings) == 0 {
		t.Fatal("zero-case report has no warning")
	}
	if built.Dataset.EvalDatasetVersion != manifest.EvalDatasetVersion {
		t.Fatalf("dataset version = %q", built.Dataset.EvalDatasetVersion)
	}
}

func TestBuildMissingRunAndUnknownScenario(t *testing.T) {
	source := goldenFixture(t)
	if _, err := Build(context.Background(), source,
		"99999999-9999-4999-8999-999999999999", Options{DatasetDir: devDatasetDir(t)}); err == nil {
		t.Fatal("missing run did not fail")
	}

	source = goldenFixture(t)
	source.rows[0].ScenarioID = "scenario-absent-from-truth"
	if _, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	}); err == nil || !strings.Contains(err.Error(), "absent from the committed truth") {
		t.Fatalf("unknown scenario error = %v", err)
	}
}

func TestBuildModeMismatchAndBadCost(t *testing.T) {
	source := goldenFixture(t)
	source.rows[0].FailureMode = "not_the_truth_mode"
	if _, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	}); err == nil || !strings.Contains(err.Error(), "does not match truth mode") {
		t.Fatalf("mode mismatch error = %v", err)
	}

	source = goldenFixture(t)
	source.rows[0].CostUSD = "not-money"
	if _, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	}); err == nil || !strings.Contains(err.Error(), "cost_usd") {
		t.Fatalf("bad cost error = %v", err)
	}
}

func fixtureBaseRun(t *testing.T, index int, repeatIndex int) RunRow {
	t.Helper()
	_, manifest := devDataset(t)
	started, err := time.Parse(time.RFC3339, fixtureStartedAt)
	if err != nil {
		t.Fatalf("parse fixture time: %v", err)
	}
	ended := started.Add(10 * time.Minute)
	id := []string{fixtureRun0, fixtureRun1, fixtureRun2}[index]
	repeat := repeatIndex
	cap := "1.000000"
	return RunRow{
		ID:             id,
		Suite:          "agent",
		Split:          dataset.Dev,
		DatasetVersion: manifest.EvalDatasetVersion,
		Model:          "deepseek-v4.1-flash",
		PromptVersion:  "v1",
		Seed:           manifest.Seed,
		GitSHA:         fixtureGitSHA,
		StartedAt:      started,
		EndedAt:        &ended,
		RepeatGroup:    fixtureGroup,
		RepeatIndex:    &repeat,
		CostCapUSD:     &cap,
	}
}

// covers: FIN-01, FIN-05
func TestReportMoneyStaysDecimalText(t *testing.T) {
	source := goldenFixture(t)
	built, err := Build(context.Background(), source, fixtureRun1, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	data, err := RenderJSON(built)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode report JSON: %v", err)
	}
	headline, ok := document["headline"].(map[string]any)
	if !ok {
		t.Fatalf("headline is not an object: %T", document["headline"])
	}
	cost, ok := headline["cost"].(map[string]any)
	if !ok {
		t.Fatalf("headline cost is not an object: %T", headline["cost"])
	}
	moneyPattern := regexp.MustCompile(`^[0-9]+\.[0-9]{6}$`)
	for _, key := range []string{"total_usd", "mean_usd"} {
		text, ok := cost[key].(string)
		if !ok {
			t.Fatalf("headline cost.%s is %T, want decimal string", key, cost[key])
		}
		if !moneyPattern.MatchString(text) {
			t.Fatalf("headline cost.%s = %q, want 6 fractional digits", key, text)
		}
	}
	repeats, ok := document["repeats"].(map[string]any)
	if !ok {
		t.Fatalf("repeats is not an object: %T", document["repeats"])
	}
	repeatCost, ok := repeats["cost"].(map[string]any)
	if !ok {
		t.Fatalf("repeats cost is not an object: %T", repeats["cost"])
	}
	for _, key := range []string{"mean_usd", "min_usd", "max_usd", "stddev_usd"} {
		text, ok := repeatCost[key].(string)
		if !ok {
			t.Fatalf("repeats cost.%s is %T, want decimal string", key, repeatCost[key])
		}
		if !moneyPattern.MatchString(text) {
			t.Fatalf("repeats cost.%s = %q, want 6 fractional digits", key, text)
		}
	}
}

func checkGolden(t *testing.T, path string, got []byte) {
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
		t.Fatalf("read golden %s (run go test ./eval/report -run TestGolden -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden %s drifted\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}
