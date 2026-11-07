package dataset

import (
	"fmt"
	"sync"
	"testing"
)

var (
	fixtureOnce    sync.Once
	fixtureBySplit map[Split]*Dataset
	fixtureErr     error
)

// cachedDataset builds every split once per test process and returns an
// immutable shared fixture. Build is deterministic, so sharing is safe as long
// as tests only read the result.
func cachedDataset(t *testing.T, split Split) *Dataset {
	t.Helper()
	fixtureOnce.Do(func() {
		fixtureBySplit = make(map[Split]*Dataset, len(Splits()))
		for _, candidate := range Splits() {
			config, err := DefaultConfig(candidate)
			if err != nil {
				fixtureErr = fmt.Errorf("default config %s: %w", candidate, err)
				return
			}
			built, err := Build(config)
			if err != nil {
				fixtureErr = fmt.Errorf("build %s: %w", candidate, err)
				return
			}
			fixtureBySplit[candidate] = built
		}
	})
	if fixtureErr != nil {
		t.Fatalf("cached eval fixture: %v", fixtureErr)
	}
	built := fixtureBySplit[split]
	if built == nil {
		t.Fatalf("no cached fixture for split %q", split)
	}
	return built
}
