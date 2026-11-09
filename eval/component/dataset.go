package component

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/score"
)

// resolvedDataset is the fail-closed result of loading, rebuilding and
// verifying the committed dataset split before the matcher sandbox is created.
type resolvedDataset struct {
	Dir      string
	Manifest dataset.Manifest
	Config   dataset.Config
	Dataset  *dataset.Dataset
}

// loadDataset mirrors the runner/report fail-closed dataset checks without
// importing eval/runner: manifest split must match the requested split, the
// rebuilt manifest must match the committed version/split/seed/days/difficulty/
// truth schema, and the ground truth must validate for scoring.
func loadDataset(opts Options) (*resolvedDataset, error) {
	dir := strings.TrimSpace(opts.DatasetDir)
	if dir == "" {
		root := strings.TrimSpace(opts.ModuleRoot)
		if root == "" {
			var err error
			root, err = FindModuleRoot()
			if err != nil {
				return nil, err
			}
		}
		dir = filepath.Join(root, "eval", "datasets", string(opts.Split))
	}

	manifest, err := dataset.LoadManifest(dir)
	if err != nil {
		return nil, fmt.Errorf("eval/component: load committed manifest: %w", err)
	}
	if manifest.Split != opts.Split {
		return nil, fmt.Errorf(
			"eval/component: committed manifest split %q does not match --split %q",
			manifest.Split, opts.Split)
	}

	buildConfig := opts.BuildConfig
	if buildConfig == nil {
		buildConfig = func(manifest dataset.Manifest) (dataset.Config, error) {
			return dataset.DefaultConfig(manifest.Split)
		}
	}
	cfg, err := buildConfig(manifest)
	if err != nil {
		return nil, fmt.Errorf("eval/component: build dataset config for split %q: %w",
			manifest.Split, err)
	}
	ds, err := dataset.Build(cfg)
	if err != nil {
		return nil, fmt.Errorf("eval/component: rebuild dataset split %q: %w",
			manifest.Split, err)
	}
	if err := checkDatasetDrift(manifest, ds.Manifest); err != nil {
		return nil, err
	}
	if err := score.ValidateTruth(ds.Truth); err != nil {
		return nil, fmt.Errorf("eval/component: validate ground truth: %w", err)
	}
	return &resolvedDataset{Dir: dir, Manifest: manifest, Config: cfg, Dataset: ds}, nil
}

// checkDatasetDrift fails closed when the rebuilt dataset differs from the
// committed manifest. This is the same drift set the runner applies.
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
		return fmt.Errorf("eval/component: committed manifest drift (refusing to score): %s",
			strings.Join(mismatches, "; "))
	}
	return nil
}

// FindModuleRoot walks up from the current working directory looking for
// go.mod. It returns the current directory as a documented fallback when no
// module root is found.
func FindModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("eval/component: determine working directory: %w", err)
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
