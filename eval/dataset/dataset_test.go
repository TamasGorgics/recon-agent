package dataset

import (
	"bytes"
	"testing"

	"github.com/TamasGorgics/recon-agent/synth/modes"
)

func TestBuildIsDeterministic(t *testing.T) {
	for _, split := range Splits() {
		t.Run(string(split), func(t *testing.T) {
			cfg, err := DefaultConfig(split)
			if err != nil {
				t.Fatalf("DefaultConfig: %v", err)
			}
			first, err := Build(cfg)
			if err != nil {
				t.Fatalf("first Build: %v", err)
			}
			second, err := Build(cfg)
			if err != nil {
				t.Fatalf("second Build: %v", err)
			}
			if first.Manifest.EvalDatasetVersion != second.Manifest.EvalDatasetVersion {
				t.Fatalf("eval_dataset_version differs: %s vs %s",
					first.Manifest.EvalDatasetVersion, second.Manifest.EvalDatasetVersion)
			}
			wantManifest, err := manifestBytes(first.Manifest)
			if err != nil {
				t.Fatalf("first manifest bytes: %v", err)
			}
			gotManifest, err := manifestBytes(second.Manifest)
			if err != nil {
				t.Fatalf("second manifest bytes: %v", err)
			}
			if !bytes.Equal(wantManifest, gotManifest) {
				t.Fatal("manifest bytes differ between two builds")
			}
			if len(first.Files) != len(second.Files) {
				t.Fatalf("file count differs: %d vs %d", len(first.Files), len(second.Files))
			}
			for index := range first.Files {
				left, right := first.Files[index], second.Files[index]
				if left.Path != right.Path {
					t.Fatalf("file %d path %q vs %q", index, left.Path, right.Path)
				}
				if !bytes.Equal(left.Data, right.Data) {
					t.Fatalf("file %s bytes differ between two builds", left.Path)
				}
			}
			wantTruth, err := jsonDocument(first.Truth)
			if err != nil {
				t.Fatalf("first truth bytes: %v", err)
			}
			gotTruth, err := jsonDocument(second.Truth)
			if err != nil {
				t.Fatalf("second truth bytes: %v", err)
			}
			if !bytes.Equal(wantTruth, gotTruth) {
				t.Fatal("ground-truth bytes differ between two builds")
			}
		})
	}
}

func TestScenariosStartNonNil(t *testing.T) {
	truth := cachedDataset(t, Test).Truth
	if len(truth.Scenarios) == 0 {
		t.Fatal("ground truth has no scenarios")
	}
	for _, scenario := range truth.Scenarios {
		if scenario.LedgerTxnIDs == nil || scenario.ExternalRecords == nil ||
			scenario.Amounts == nil || scenario.Accounts == nil {
			t.Fatalf("scenario %s has a nil required array", scenario.ScenarioID)
		}
	}
}

func TestSplitsAreDisjoint(t *testing.T) {
	type splitTruth struct {
		seed  int64
		ids   map[string]struct{}
		count int
	}
	truths := make(map[Split]splitTruth, len(Splits()))
	for _, split := range Splits() {
		built := cachedDataset(t, split)
		ids := make(map[string]struct{}, len(built.Truth.Scenarios))
		for _, scenario := range built.Truth.Scenarios {
			if _, duplicate := ids[scenario.ScenarioID]; duplicate {
				t.Fatalf("%s contains duplicate scenario id %s", split, scenario.ScenarioID)
			}
			ids[scenario.ScenarioID] = struct{}{}
		}
		truths[split] = splitTruth{seed: built.Manifest.Seed, ids: ids, count: len(ids)}
	}
	for _, left := range Splits() {
		for _, right := range Splits() {
			if left >= right {
				continue
			}
			if truths[left].seed == truths[right].seed {
				t.Fatalf("%s and %s share seed %d", left, right, truths[left].seed)
			}
			for id := range truths[left].ids {
				if _, overlap := truths[right].ids[id]; overlap {
					t.Fatalf("%s and %s share scenario id %s", left, right, id)
				}
			}
		}
	}
}

func TestTestSplitCoverage(t *testing.T) {
	built := cachedDataset(t, Test)
	for _, mode := range modes.AllModes {
		if count := built.Manifest.ModeCounts[mode]; count < 30 {
			t.Fatalf("test mode %s has %d truth rows, want >= 30", mode, count)
		}
	}
	if got := built.Manifest.ModeCounts[modes.BatchComposite]; got != 30 {
		t.Fatalf("test batch_composite truth rows = %d, want 30 from 15 applications", got)
	}
	if got := built.Manifest.ApplicationCounts[modes.BatchComposite]; got != 15 {
		t.Fatalf("test batch_composite applications = %d, want 15", got)
	}
	sum := 0
	for _, count := range built.Manifest.ModeCounts {
		sum += count
	}
	if sum != len(built.Truth.Scenarios) {
		t.Fatalf("mode counts sum = %d, truth scenarios = %d", sum, len(built.Truth.Scenarios))
	}
	difficultySum := 0
	for _, count := range built.Manifest.DifficultyCounts {
		difficultySum += count
	}
	if difficultySum != len(built.Truth.Scenarios) {
		t.Fatalf("difficulty counts sum = %d, truth scenarios = %d",
			difficultySum, len(built.Truth.Scenarios))
	}
	if got := built.Manifest.ApplicationCounts[modes.CleanMode]; got != 0 {
		t.Fatalf("clean application count = %d, want 0", got)
	}
}

func TestDefaultCountsAndSelection(t *testing.T) {
	dev := cachedDataset(t, Dev)
	if dev.Manifest.ApplicationCounts[modes.CleanMode] != 0 {
		t.Fatal("dev clean application count is not zero")
	}
	if sumIntMap(dev.Manifest.ApplicationCounts) != 138 {
		t.Fatalf("dev applications = %d, want 138", sumIntMap(dev.Manifest.ApplicationCounts))
	}
	if sumIntMap(dev.Manifest.ModeCounts) != len(dev.Truth.Scenarios) {
		t.Fatal("dev mode counts do not sum to truth scenarios")
	}
	if dev.Manifest.ApplicationCounts[modes.BatchComposite] != 6 ||
		dev.Manifest.ModeCounts[modes.BatchComposite] != 12 {
		t.Fatalf("dev batch applications/truth = %d/%d, want 6/12",
			dev.Manifest.ApplicationCounts[modes.BatchComposite],
			dev.Manifest.ModeCounts[modes.BatchComposite])
	}

	injection := cachedDataset(t, Injection)
	if sumIntMap(injection.Manifest.ApplicationCounts) != 30 {
		t.Fatalf("injection applications = %d, want 30",
			sumIntMap(injection.Manifest.ApplicationCounts))
	}
	if len(injection.Truth.Scenarios) != 30 {
		t.Fatalf("injection truth scenarios = %d, want 30", len(injection.Truth.Scenarios))
	}
	for _, scenario := range injection.Truth.Scenarios {
		if scenario.Mode != modes.InjectionMemo {
			t.Fatalf("injection split selected mode %q", scenario.Mode)
		}
	}
	if injection.Manifest.ModeCounts[modes.CleanMode] != 0 {
		t.Fatalf("injection clean mode count = %d, want 0", injection.Manifest.ModeCounts[modes.CleanMode])
	}
	if injection.Manifest.Counts.LedgerTxns <= len(injection.Truth.Scenarios) {
		t.Fatal("injection world does not retain the unscored clean generator rows")
	}
}

func sumIntMap(values map[string]int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}
