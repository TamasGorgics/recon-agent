package modes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/synth"
)

// Errors returned before any world mutation is rendered.
var (
	// ErrInvalidWorld reports a dataset or world that cannot host a mode.
	ErrInvalidWorld = errors.New("synth/modes: invalid world")
	// ErrInsufficientData reports a mix that needs more unused charge or
	// refund rows than the clean dataset contains.
	ErrInsufficientData = errors.New("synth/modes: insufficient clean scenarios")
)

// Mode applies one failure mode to a World and appends its ground-truth
// scenario. Apply must be deterministic for a given world state.
type Mode interface {
	// Name is the mode string used in ground truth and the manifest.
	Name() string
	// Apply mutates the world and emits exactly one scenario.
	Apply(w *World) error
}

// Manifest is the failure-mode manifest. It embeds the M1-09 manifest so every
// existing field keeps its name, and adds the per-mode and per-difficulty
// counts required by M1-10 plus the documented noise knobs.
type Manifest struct {
	synth.Manifest
	ModeCounts       map[string]int `json:"mode_counts"`
	DifficultyCounts map[string]int `json:"difficulty_counts"`
	Noise            Noise          `json:"noise"`
}

// Result is one mutated dataset. Files contains the same paths M1-09 renders,
// so synth.Load can load it without a modes-specific loader.
type Result struct {
	Manifest Manifest
	Ledger   synth.LedgerDocument
	Truth    synth.GroundTruth
	Files    []synth.File
}

// Apply mutates a clean dataset with the configured mode mix. The input dataset
// is never mutated: the world owns copies of every slice it changes. It uses
// the default psp_a/bank_x calendars for late_settlement; ApplyWithSources
// accepts the generator's configured calendars instead.
func Apply(dataset *synth.Dataset, cfg Config) (*Result, error) {
	return ApplyWithSources(dataset, cfg, nil)
}

// ApplyWithSources is Apply with an explicit per-source calendar map (the
// generator's config.SynthConfig.Sources). An empty map falls back to
// config.DefaultSynthIngestSources().
func ApplyWithSources(dataset *synth.Dataset, cfg Config,
	sources map[string]config.SourceConfig) (*Result, error) {
	if dataset == nil {
		return nil, fmt.Errorf("%w: dataset is required", ErrInvalidWorld)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		sources = config.DefaultSynthIngestSources()
	}
	world, err := newWorld(dataset, sources)
	if err != nil {
		return nil, err
	}
	registry := registry()
	for _, name := range PartAModes {
		mode := registry[name]
		count := cfg.Counts[name]
		for i := 0; i < count; i++ {
			if err := mode.Apply(world); err != nil {
				return nil, fmt.Errorf("synth/modes: apply %s: %w", name, err)
			}
		}
	}
	if err := applyNoise(world); err != nil {
		return nil, err
	}
	return world.render(cfg)
}

// Generate builds one clean dataset with synth and then applies the mix. It
// keeps generation DB-free and clock-free: the clock is the generator's
// injected clock, used only by a later synth.Load.
func Generate(clk clock.Clock, opts synth.Options, cfg Config) (*Result, error) {
	dataset, err := synth.New(clk).Generate(opts)
	if err != nil {
		return nil, err
	}
	return ApplyWithSources(dataset, cfg, opts.Config.Sources)
}

// Write writes every rendered file under dir, overwriting existing bytes.
func (r *Result) Write(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("synth/modes: output directory is required")
	}
	for _, file := range r.Files {
		full := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("synth/modes: create directory for %s: %w", file.Path, err)
		}
		if err := os.WriteFile(full, file.Data, 0o644); err != nil {
			return fmt.Errorf("synth/modes: write %s: %w", file.Path, err)
		}
	}
	return nil
}

// registry returns one Mode per part-A name. It is a function so the package
// has no mutable package-level mode state.
func registry() map[string]Mode {
	return map[string]Mode{
		DuplicateCharge:   modeFunc{name: DuplicateCharge, apply: applyDuplicateCharge},
		FeeVariance:       modeFunc{name: FeeVariance, apply: applyFeeVariance},
		FXRounding:        modeFunc{name: FXRounding, apply: applyFXRounding},
		LateSettlement:    modeFunc{name: LateSettlement, apply: applyLateSettlement},
		MissingSettlement: modeFunc{name: MissingSettlement, apply: applyMissingSettlement},
		PartialRefund:     modeFunc{name: PartialRefund, apply: applyPartialRefund},
	}
}

type modeFunc struct {
	name  string
	apply func(*World) error
}

func (m modeFunc) Name() string { return m.name }

func (m modeFunc) Apply(w *World) error { return m.apply(w) }

// ModeNames returns PartAModes sorted ascending. The slice is a copy.
func ModeNames() []string {
	names := append([]string(nil), PartAModes...)
	sort.Strings(names)
	return names
}
