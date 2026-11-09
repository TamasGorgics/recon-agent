package component

import (
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

func miniatureConfig(t *testing.T) dataset.Config {
	t.Helper()
	cfg, err := dataset.DefaultConfig(dataset.Dev)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Seed = 7306
	cfg.Days = 7
	cfg.Difficulty = synth.DifficultyEasy
	cfg.Synth.ChargesPerDay = 1
	counts := make(map[string]int, len(modes.AllModes))
	total := 0
	for _, mode := range modes.AllModes {
		counts[mode] = 1
		total++
	}
	cfg.Modes = modes.Config{Total: total, Counts: counts}
	return cfg
}

func miniatureDataset(t *testing.T) (*dataset.Dataset, dataset.Config) {
	t.Helper()
	cfg := miniatureConfig(t)
	ds, err := dataset.Build(cfg)
	if err != nil {
		t.Fatalf("Build miniature dataset: %v", err)
	}
	return ds, cfg
}

func writeMiniatureManifest(t *testing.T, ds *dataset.Dataset) string {
	t.Helper()
	dir := t.TempDir()
	if err := ds.WriteManifest(dir); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	return dir
}

func TestLoadDatasetSucceedsWithBuildConfig(t *testing.T) {
	ds, cfg := miniatureDataset(t)
	dir := writeMiniatureManifest(t, ds)
	resolved, err := loadDataset(Options{
		Split:      dataset.Dev,
		DatasetDir: dir,
		BuildConfig: func(dataset.Manifest) (dataset.Config, error) {
			return cfg, nil
		},
	})
	if err != nil {
		t.Fatalf("loadDataset: %v", err)
	}
	if resolved.Manifest.EvalDatasetVersion != ds.Manifest.EvalDatasetVersion {
		t.Fatalf("manifest version = %q", resolved.Manifest.EvalDatasetVersion)
	}
	if len(resolved.Dataset.Truth.Scenarios) != len(ds.Truth.Scenarios) {
		t.Fatalf("truth scenarios = %d, want %d",
			len(resolved.Dataset.Truth.Scenarios), len(ds.Truth.Scenarios))
	}
}

func TestLoadDatasetFailsClosedOnDrift(t *testing.T) {
	ds, _ := miniatureDataset(t)
	dir := writeMiniatureManifest(t, ds)
	_, err := loadDataset(Options{Split: dataset.Dev, DatasetDir: dir})
	if err == nil || !strings.Contains(err.Error(), "committed manifest drift") {
		t.Fatalf("drift error = %v", err)
	}
}

func TestLoadDatasetFailsClosedOnSplitMismatch(t *testing.T) {
	ds, cfg := miniatureDataset(t)
	dir := writeMiniatureManifest(t, ds)
	_, err := loadDataset(Options{
		Split:      dataset.Test,
		DatasetDir: dir,
		BuildConfig: func(dataset.Manifest) (dataset.Config, error) {
			return cfg, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match --split") {
		t.Fatalf("split mismatch error = %v", err)
	}
}

func TestLoadDatasetFailsClosedOnMissingManifest(t *testing.T) {
	_, err := loadDataset(Options{Split: dataset.Dev, DatasetDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "load committed manifest") {
		t.Fatalf("missing manifest error = %v", err)
	}
}
