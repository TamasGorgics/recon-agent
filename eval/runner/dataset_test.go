package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

func smallDatasetConfig(t *testing.T) dataset.Config {
	t.Helper()
	cfg, err := dataset.DefaultConfig(dataset.Dev)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Days = 3
	cfg.Modes = modes.Config{
		Total:  2,
		Counts: map[string]int{modes.FeeVariance: 1, modes.ReferenceTypo: 1},
	}
	return cfg
}

func TestLoadDatasetAndFailClosedDrift(t *testing.T) {
	cfg := smallDatasetConfig(t)
	ds, err := dataset.Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dir := t.TempDir()
	if err := ds.WriteManifest(dir); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	opts := Options{Split: dataset.Dev, DatasetDir: dir}
	rd, err := loadDataset(opts, Deps{BuildConfig: func(dataset.Manifest) (dataset.Config, error) {
		return cfg, nil
	}})
	if err != nil {
		t.Fatalf("loadDataset: %v", err)
	}
	if rd.Manifest.EvalDatasetVersion != ds.Manifest.EvalDatasetVersion {
		t.Fatalf("loaded version %q, want %q", rd.Manifest.EvalDatasetVersion, ds.Manifest.EvalDatasetVersion)
	}

	drifted := cfg
	drifted.Days++
	_, err = loadDataset(opts, Deps{BuildConfig: func(dataset.Manifest) (dataset.Config, error) {
		return drifted, nil
	}})
	if err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("drift error = %v, want drift", err)
	}

	wrongSplit := opts
	wrongSplit.Split = dataset.Test
	if _, err := loadDataset(wrongSplit, Deps{}); !IsUsage(err) {
		t.Fatalf("split mismatch = %v, want usage", err)
	}

	wrongSeed := opts
	seed := int64(ds.Manifest.Seed + 1)
	wrongSeed.Seed = &seed
	if _, err := loadDataset(wrongSeed, Deps{}); !IsUsage(err) {
		t.Fatalf("seed mismatch = %v, want usage", err)
	}
}

func TestExpectedScoreableCount(t *testing.T) {
	truth := dataset.GroundTruth{Scenarios: []dataset.ScenarioTruth{
		{ScenarioID: "a", Mode: modes.FeeVariance, BreakSignature: "sig"},
		{ScenarioID: "b", Mode: modes.CleanMode, BreakSignature: ""},
		{ScenarioID: "c", Mode: modes.InjectionMemo, BreakSignature: ""},
	}}
	// The generator puts injection_memo applications in dev/test too, and the
	// sandbox creates a synthetic case for each one regardless of split.
	if got := expectedScoreableCount(truth); got != 2 {
		t.Fatalf("scoreable = %d, want 2", got)
	}
}

func TestFindModuleRootFrom(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := findModuleRootFrom(nested); got != root {
		t.Fatalf("root = %q, want %q", got, root)
	}
	if got := findModuleRootFrom(t.TempDir()); got == "" {
		t.Fatal("fallback root is empty")
	}
}

func TestBestEffortGitSHA(t *testing.T) {
	if sha := bestEffortGitSHA(t.TempDir()); sha != "" {
		t.Fatalf("non-git sha = %q, want empty", sha)
	}
	if sha := bestEffortGitSHA(""); sha != "" {
		t.Fatalf("empty root sha = %q", sha)
	}
}

func TestBuildRunConfigDecimalStrings(t *testing.T) {
	opts := Options{
		Suite:       "agent",
		Split:       dataset.Test,
		Repeat:      2,
		Concurrency: 4,
		LLM:         "cassette",
		MaxCostUSD:  decimal.RequireFromString("5.00"),
	}
	raw, err := buildRunConfig(opts, 1)
	if err != nil {
		t.Fatalf("buildRunConfig: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	evalSection, _ := document["eval"].(map[string]any)
	if evalSection["max_cost_usd"] != "5" {
		t.Fatalf("max_cost_usd = %#v, want string \"5\"", evalSection["max_cost_usd"])
	}
	pricing, _ := document["pricing"].(map[string]any)
	if pricing["schema"] != "llm-pricing/v1" {
		t.Fatalf("pricing schema = %#v", pricing["schema"])
	}
	models, _ := pricing["models"].(map[string]any)
	model, _ := models[config.DefaultLLMModel].(map[string]any)
	if model == nil {
		t.Fatalf("pricing model missing: %#v", models)
	}
	for key, value := range model {
		if key == "currency" {
			continue
		}
		if _, ok := value.(string); !ok {
			t.Fatalf("%s = %T, want decimal string", key, value)
		}
	}
}
