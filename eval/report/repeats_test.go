package report

import (
	"context"
	"math"
	"strings"
	"testing"
)

func buildFixtureReport(t *testing.T, source *fixtureSource, runID string) Report {
	t.Helper()
	built, err := Build(context.Background(), source, runID, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return built
}

func TestRepeatGroupK1HasNoSpread(t *testing.T) {
	source := goldenFixture(t)
	source.runs = source.runs[:1]
	built := buildFixtureReport(t, source, fixtureRun0)
	if built.Repeats == nil || built.Repeats.K != 1 {
		t.Fatalf("repeats = %+v", built.Repeats)
	}
	metric := built.Repeats.Metrics["accuracy"]
	if metric.Mean == nil || *metric.Mean != 0.75 {
		t.Fatalf("k=1 accuracy mean = %v", metric.Mean)
	}
	if metric.Stddev != nil {
		t.Fatalf("k=1 stddev = %v, want nil", metric.Stddev)
	}
	if len(metric.PerRun) != 1 || metric.PerRun[0].Value == nil ||
		*metric.PerRun[0].Value != 0.75 {
		t.Fatalf("k=1 per-run = %+v", metric.PerRun)
	}
	if built.Headline.Accuracy.Aggregation != aggregationSingleRun {
		t.Fatalf("k=1 headline aggregation = %q", built.Headline.Accuracy.Aggregation)
	}
	if built.Headline.Accuracy.Repeat != nil {
		t.Fatal("k=1 headline carries repeat spread")
	}
}

func TestRepeatStatsKnownValues(t *testing.T) {
	source := goldenFixture(t)
	built := buildFixtureReport(t, source, fixtureRun1)
	accuracy := built.Repeats.Metrics["accuracy"]
	if accuracy.Mean == nil || math.Abs(*accuracy.Mean-2.0/3.0) > 1e-12 {
		t.Fatalf("accuracy mean = %v", accuracy.Mean)
	}
	if accuracy.Min == nil || *accuracy.Min != 0.5 {
		t.Fatalf("accuracy min = %v", accuracy.Min)
	}
	if accuracy.Max == nil || *accuracy.Max != 0.75 {
		t.Fatalf("accuracy max = %v", accuracy.Max)
	}
	if accuracy.Stddev == nil || math.Abs(*accuracy.Stddev-0.14433756729740643) > 1e-12 {
		t.Fatalf("accuracy stddev = %v", accuracy.Stddev)
	}
	wantPerRun := []float64{0.75, 0.75, 0.5}
	if len(accuracy.PerRun) != len(wantPerRun) {
		t.Fatalf("per-run = %+v", accuracy.PerRun)
	}
	for index, want := range wantPerRun {
		if accuracy.PerRun[index].RepeatIndex != index {
			t.Fatalf("per-run[%d] index = %d", index, accuracy.PerRun[index].RepeatIndex)
		}
		if accuracy.PerRun[index].Value == nil || *accuracy.PerRun[index].Value != want {
			t.Fatalf("per-run[%d] value = %v, want %v", index,
				accuracy.PerRun[index].Value, want)
		}
	}
	if built.Headline.Accuracy.Numerator != nil || built.Headline.Accuracy.Denominator != nil {
		t.Fatal("repeat headline carried a pooled binomial denominator")
	}
}

func TestRepeatMembersLoadInRepeatIndexOrder(t *testing.T) {
	source := goldenFixture(t)
	source.runs = []RunRow{source.runs[2], source.runs[0], source.runs[1]}
	built := buildFixtureReport(t, source, fixtureRun1)
	for index, run := range built.Runs {
		if run.RepeatIndex == nil || *run.RepeatIndex != index {
			t.Fatalf("runs[%d] repeat index = %v", index, run.RepeatIndex)
		}
	}
	for index, perRun := range built.Headline.Accuracy.Repeat.PerRun {
		if perRun.RepeatIndex != index {
			t.Fatalf("per-run[%d] repeat index = %d", index, perRun.RepeatIndex)
		}
	}
}

func TestRepeatGroupMissingSiblingFails(t *testing.T) {
	source := goldenFixture(t)
	source.runs = []RunRow{source.runs[0], source.runs[2]}
	_, err := Build(context.Background(), source, fixtureRun0, Options{
		DatasetDir: devDatasetDir(t),
		Clock:      fixtureClock(t),
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("missing sibling error = %v", err)
	}
}

func TestRepeatGroupInconsistentSiblingFails(t *testing.T) {
	mutations := map[string]func(*RunRow){
		"model":           func(run *RunRow) { run.Model = "other-model" },
		"prompt":          func(run *RunRow) { run.PromptVersion = "v2" },
		"seed":            func(run *RunRow) { run.Seed++ },
		"suite":           func(run *RunRow) { run.Suite = "other" },
		"dataset_version": func(run *RunRow) { run.DatasetVersion = "other" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			source := goldenFixture(t)
			mutate(&source.runs[2])
			_, err := Build(context.Background(), source, fixtureRun1, Options{
				DatasetDir: devDatasetDir(t),
				Clock:      fixtureClock(t),
			})
			if err == nil || !strings.Contains(err.Error(), "inconsistent") {
				t.Fatalf("inconsistent sibling error = %v", err)
			}
		})
	}
}

func TestRepeatGroupDifferentGitSHASharedRun(t *testing.T) {
	source := goldenFixture(t)
	source.runs[2].GitSHA = strings.Repeat("b", 40)
	built := buildFixtureReport(t, source, fixtureRun1)
	found := false
	for _, warning := range built.Warnings {
		if strings.Contains(warning, "different git SHAs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v", built.Warnings)
	}
}
