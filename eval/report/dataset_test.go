package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
)

func TestLoadDatasetMatchesCommittedVersion(t *testing.T) {
	_, manifest := devDataset(t)
	resolved, err := loadDataset(RunRow{
		Split:          dataset.Dev,
		DatasetVersion: manifest.EvalDatasetVersion,
	}, Options{DatasetDir: devDatasetDir(t)})
	if err != nil {
		t.Fatalf("loadDataset: %v", err)
	}
	if resolved.Manifest.Split != dataset.Dev {
		t.Fatalf("split = %q", resolved.Manifest.Split)
	}
	if len(resolved.Scenarios) != len(resolved.Truth.Scenarios) || len(resolved.Scenarios) == 0 {
		t.Fatalf("scenario map = %d, truth = %d",
			len(resolved.Scenarios), len(resolved.Truth.Scenarios))
	}
}

func TestLoadDatasetVersionMismatchFailsClosed(t *testing.T) {
	_, err := loadDataset(RunRow{
		Split:          dataset.Dev,
		DatasetVersion: "0000000000000000000000000000000000000000000000000000000000000000",
	}, Options{DatasetDir: devDatasetDir(t)})
	if err == nil || !strings.Contains(err.Error(), "dataset version drift") {
		t.Fatalf("version mismatch error = %v", err)
	}
}

func TestLoadDatasetSplitMismatchFailsClosed(t *testing.T) {
	_, manifest := devDataset(t)
	_, err := loadDataset(RunRow{
		Split:          dataset.Injection,
		DatasetVersion: manifest.EvalDatasetVersion,
	}, Options{DatasetDir: devDatasetDir(t)})
	if err == nil || !strings.Contains(err.Error(), "does not match run split") {
		t.Fatalf("split mismatch error = %v", err)
	}
}

func TestLoadDatasetMissingManifestFailsClosed(t *testing.T) {
	_, err := loadDataset(RunRow{
		Split:          dataset.Dev,
		DatasetVersion: "unused",
	}, Options{DatasetDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "load committed manifest") {
		t.Fatalf("missing manifest error = %v", err)
	}
}

func TestLoadDatasetRebuildDriftFailsClosed(t *testing.T) {
	committed, err := dataset.LoadManifest(devDatasetDir(t))
	if err != nil {
		t.Fatalf("load committed manifest: %v", err)
	}
	committed.Seed++
	version, err := dataset.ComputeEvalDatasetVersion(committed)
	if err != nil {
		t.Fatalf("recompute version: %v", err)
	}
	committed.EvalDatasetVersion = version
	data, err := json.MarshalIndent(committed, "", "  ")
	if err != nil {
		t.Fatalf("marshal mutated manifest: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write mutated manifest: %v", err)
	}
	_, err = loadDataset(RunRow{
		Split:          dataset.Dev,
		DatasetVersion: version,
	}, Options{DatasetDir: dir})
	if err == nil || !strings.Contains(err.Error(), "committed manifest drift") {
		t.Fatalf("rebuild drift error = %v", err)
	}
}
