package sandbox

import (
	"sync"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

var (
	miniMu     sync.Mutex
	miniBySeed map[int64]*dataset.Dataset
	miniCfg    map[int64]dataset.Config
)

// miniDataset builds one small, deterministic dataset per seed. It exercises
// every failure mode once (batch_composite emits two truth rows), which keeps
// integration tests fast while covering the full mapping surface.
func miniDataset(t *testing.T, seed int64) (*dataset.Dataset, dataset.Config) {
	t.Helper()
	miniMu.Lock()
	defer miniMu.Unlock()
	if miniBySeed == nil {
		miniBySeed = make(map[int64]*dataset.Dataset)
		miniCfg = make(map[int64]dataset.Config)
	}
	if built, ok := miniBySeed[seed]; ok {
		return built, miniCfg[seed]
	}
	cfg, err := dataset.DefaultConfig(dataset.Dev)
	if err != nil {
		t.Fatalf("mini dataset default config: %v", err)
	}
	cfg.Seed = seed
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
	built, err := dataset.Build(cfg)
	if err != nil {
		t.Fatalf("build mini dataset seed %d: %v", seed, err)
	}
	miniBySeed[seed] = built
	miniCfg[seed] = cfg
	return built, cfg
}

// allScopes returns the deterministic scope set for a mini config without a
// database.
func allScopes(seed int64, synthConfig config.SynthConfig) map[string]scopeRow {
	scopes := make(map[string]scopeRow)
	for _, currency := range synthConfig.CurrenciesSorted() {
		for _, source := range []string{synth.SourcePSP, synth.SourceBank} {
			scopes[scopeKey(source, currency)] = scopeRow{
				ID:       deterministicScopeID(seed, source, currency),
				Source:   source,
				Currency: currency,
			}
		}
	}
	return scopes
}
