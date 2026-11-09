package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/score"
)

// resolvedDataset is the fail-closed result of loading, rebuilding and
// verifying the committed dataset for a run. It intentionally duplicates the
// runner's drift checks instead of importing eval/runner, which must not
// depend on eval/report (and vice versa).
type resolvedDataset struct {
	Dir       string
	Manifest  dataset.Manifest
	Config    dataset.Config
	Truth     dataset.GroundTruth
	Scenarios map[string]dataset.ScenarioTruth
}

func loadDataset(run RunRow, opts Options) (*resolvedDataset, error) {
	dir, err := datasetDir(run.Split, opts)
	if err != nil {
		return nil, err
	}
	manifest, err := dataset.LoadManifest(dir)
	if err != nil {
		return nil, fmt.Errorf("eval/report: load committed manifest: %w", err)
	}
	if manifest.Split != run.Split {
		return nil, fmt.Errorf(
			"eval/report: committed manifest split %q does not match run split %q",
			manifest.Split, run.Split)
	}
	if manifest.EvalDatasetVersion != run.DatasetVersion {
		return nil, fmt.Errorf(
			"eval/report: dataset version drift: committed manifest %q does not match run %q",
			manifest.EvalDatasetVersion, run.DatasetVersion)
	}
	cfg, err := dataset.DefaultConfig(manifest.Split)
	if err != nil {
		return nil, fmt.Errorf("eval/report: build dataset config for split %q: %w",
			manifest.Split, err)
	}
	ds, err := dataset.Build(cfg)
	if err != nil {
		return nil, fmt.Errorf("eval/report: rebuild dataset split %q: %w",
			manifest.Split, err)
	}
	if err := checkDatasetDrift(manifest, ds.Manifest); err != nil {
		return nil, err
	}
	if err := score.ValidateTruth(ds.Truth); err != nil {
		return nil, fmt.Errorf("eval/report: validate ground truth: %w", err)
	}
	scenarios := make(map[string]dataset.ScenarioTruth, len(ds.Truth.Scenarios))
	for _, scenario := range ds.Truth.Scenarios {
		if _, exists := scenarios[scenario.ScenarioID]; exists {
			return nil, fmt.Errorf("eval/report: duplicate truth scenario %q",
				scenario.ScenarioID)
		}
		scenarios[scenario.ScenarioID] = scenario
	}
	return &resolvedDataset{
		Dir:       dir,
		Manifest:  manifest,
		Config:    cfg,
		Truth:     ds.Truth,
		Scenarios: scenarios,
	}, nil
}

func datasetDir(split dataset.Split, opts Options) (string, error) {
	dir := strings.TrimSpace(opts.DatasetDir)
	if dir != "" {
		return dir, nil
	}
	root := strings.TrimSpace(opts.ModuleRoot)
	if root == "" {
		var err error
		root, err = FindModuleRoot()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(root, "eval", "datasets", string(split)), nil
}

// FindModuleRoot walks up from the current working directory looking for
// go.mod. It returns the current directory as a documented fallback when no
// module root is found.
func FindModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("eval/report: determine working directory: %w", err)
	}
	original := dir
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil && !info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return original, nil
		}
		dir = parent
	}
}

// checkDatasetDrift fails closed when the rebuilt dataset differs from the
// committed manifest. This is the same drift set the runner applies (version,
// split, seed, days, difficulty, truth schema).
func checkDatasetDrift(committed, rebuilt dataset.Manifest) error {
	mismatches := make([]string, 0, 6)
	if rebuilt.EvalDatasetVersion != committed.EvalDatasetVersion {
		mismatches = append(mismatches, fmt.Sprintf("eval_dataset_version %q != %q",
			rebuilt.EvalDatasetVersion, committed.EvalDatasetVersion))
	}
	if rebuilt.Split != committed.Split {
		mismatches = append(mismatches, fmt.Sprintf("split %q != %q", rebuilt.Split, committed.Split))
	}
	if rebuilt.Seed != committed.Seed {
		mismatches = append(mismatches, fmt.Sprintf("seed %d != %d", rebuilt.Seed, committed.Seed))
	}
	if rebuilt.Days != committed.Days {
		mismatches = append(mismatches, fmt.Sprintf("days %d != %d", rebuilt.Days, committed.Days))
	}
	if rebuilt.Difficulty != committed.Difficulty {
		mismatches = append(mismatches, fmt.Sprintf("difficulty %q != %q",
			rebuilt.Difficulty, committed.Difficulty))
	}
	if rebuilt.TruthSchemaVersion != committed.TruthSchemaVersion {
		mismatches = append(mismatches, fmt.Sprintf("truth_schema_version %q != %q",
			rebuilt.TruthSchemaVersion, committed.TruthSchemaVersion))
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("eval/report: committed manifest drift (refusing to report): %s",
			strings.Join(mismatches, "; "))
	}
	return nil
}
