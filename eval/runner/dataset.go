package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/score"
)

// resolvedDataset is the fail-closed result of manifest loading and rebuilding.
type resolvedDataset struct {
	Dir      string
	Manifest dataset.Manifest
	Config   dataset.Config
	Dataset  *dataset.Dataset
}

// loadDataset loads the committed manifest, rebuilds the split from the
// configured builder, and fails closed on any version/split/seed/days/
// difficulty/truth-schema drift. It never touches the database.
func loadDataset(opts Options, deps Deps) (*resolvedDataset, error) {
	dir := strings.TrimSpace(opts.DatasetDir)
	if dir == "" {
		root := strings.TrimSpace(deps.ModuleRoot)
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
		return nil, fmt.Errorf("eval/runner: load committed manifest: %w", err)
	}
	if manifest.Split != opts.Split {
		return nil, usagef("committed manifest split %q does not match --split %q",
			manifest.Split, opts.Split)
	}
	if opts.Seed != nil && *opts.Seed != manifest.Seed {
		return nil, usagef("--seed %d does not match the committed manifest seed %d",
			*opts.Seed, manifest.Seed)
	}

	buildConfig := deps.BuildConfig
	if buildConfig == nil {
		buildConfig = func(manifest dataset.Manifest) (dataset.Config, error) {
			return dataset.DefaultConfig(manifest.Split)
		}
	}
	cfg, err := buildConfig(manifest)
	if err != nil {
		return nil, fmt.Errorf("eval/runner: build dataset config for split %q: %w", manifest.Split, err)
	}
	ds, err := dataset.Build(cfg)
	if err != nil {
		return nil, fmt.Errorf("eval/runner: rebuild dataset split %q: %w", manifest.Split, err)
	}
	if err := checkDatasetDrift(manifest, ds.Manifest); err != nil {
		return nil, err
	}
	if err := score.ValidateTruth(ds.Truth); err != nil {
		return nil, fmt.Errorf("eval/runner: validate ground truth: %w", err)
	}
	return &resolvedDataset{Dir: dir, Manifest: manifest, Config: cfg, Dataset: ds}, nil
}

// checkDatasetDrift fails closed when the rebuilt dataset differs from the
// committed manifest. This catches generator/config/default drift and any
// mutated or uncommitted dataset version.
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
		return fmt.Errorf("eval/runner: committed manifest drift (refusing to run): %s",
			strings.Join(mismatches, "; "))
	}
	return nil
}

// FindModuleRoot walks up from the current working directory looking for
// go.mod. If none is found it returns the current working directory so a
// command run outside a module still has a documented, explicit fallback.
func FindModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("eval/runner: determine working directory: %w", err)
	}
	return findModuleRootFrom(dir), nil
}

func findModuleRootFrom(dir string) string {
	original := dir
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return original
		}
		dir = parent
	}
}
