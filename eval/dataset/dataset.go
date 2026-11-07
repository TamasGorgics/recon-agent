package dataset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/injections"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// Dataset is one built split: the committed manifest, the versioned ground
// truth, and the transient world files.
type Dataset struct {
	Manifest Manifest
	Truth    GroundTruth
	Files    []synth.File
}

// Build generates one split deterministically. It does not read the wall clock,
// the database, or the network.
func Build(cfg Config) (*Dataset, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	templates, err := resolveTemplates(cfg.Templates)
	if err != nil {
		return nil, err
	}
	if cfg.Modes.Counts[modes.InjectionMemo] > 0 && len(templates) == 0 {
		return nil, errors.New("eval/dataset: injection_memo needs at least one template")
	}
	sources := cfg.resolvedSources()
	clean, err := synth.New(clock.NewFake(cfg.GeneratedAt)).Generate(synth.Options{
		Seed:       cfg.Seed,
		Days:       cfg.Days,
		Difficulty: cfg.Difficulty,
		Config:     cfg.Synth,
	})
	if err != nil {
		return nil, fmt.Errorf("eval/dataset: generate clean world: %w", err)
	}
	result, err := modes.ApplyWithInjectionTemplates(clean, cfg.Modes, sources, templates)
	if err != nil {
		return nil, fmt.Errorf("eval/dataset: apply modes: %w", err)
	}
	world, err := newEvalWorld(result)
	if err != nil {
		return nil, err
	}
	selected, err := selectScenarios(cfg.Split, result.Truth.Scenarios)
	if err != nil {
		return nil, err
	}
	truth := GroundTruth{
		SchemaVersion:           TruthSchemaVersion,
		GeneratorDatasetVersion: result.Manifest.DatasetVersion,
		Scenarios:               make([]ScenarioTruth, 0, len(selected)),
	}
	for _, raw := range selected {
		scenario, err := buildScenarioTruth(raw, world, templates)
		if err != nil {
			return nil, err
		}
		truth.Scenarios = append(truth.Scenarios, scenario)
	}
	sort.Slice(truth.Scenarios, func(i, j int) bool {
		return truth.Scenarios[i].ScenarioID < truth.Scenarios[j].ScenarioID
	})
	if err := ValidateGroundTruth(truth); err != nil {
		return nil, err
	}
	truthBytes, err := jsonDocument(truth)
	if err != nil {
		return nil, err
	}
	files, err := assembleFiles(result.Files, truthBytes)
	if err != nil {
		return nil, err
	}
	manifest, err := buildManifest(cfg, result, truth, files, templates, sources)
	if err != nil {
		return nil, err
	}
	if err := validateManifest(manifest); err != nil {
		return nil, fmt.Errorf("eval/dataset: built manifest: %w", err)
	}
	return &Dataset{Manifest: manifest, Truth: truth, Files: files}, nil
}

// resolveTemplates returns the explicit template set or the embedded one.
func resolveTemplates(templates []injections.Template) ([]injections.Template, error) {
	if len(templates) > 0 {
		copied := append([]injections.Template(nil), templates...)
		if err := validateTemplates(copied); err != nil {
			return nil, fmt.Errorf("eval/dataset: templates: %w", err)
		}
		return copied, nil
	}
	loaded, err := injections.All()
	if err != nil {
		return nil, fmt.Errorf("eval/dataset: load embedded injection templates: %w", err)
	}
	return loaded, nil
}

// assembleFiles replaces the raw generator manifest and ground truth with the
// eval manifest (committed separately) and eval ground truth, then sorts and
// rejects duplicate or unsafe paths.
func assembleFiles(generated []synth.File, groundTruth []byte) ([]synth.File, error) {
	files := make([]synth.File, 0, len(generated))
	for _, file := range generated {
		switch file.Path {
		case "manifest.json", evalGroundTruthName:
			continue
		}
		files = append(files, synth.File{Path: file.Path, Data: file.Data})
	}
	files = append(files, synth.File{Path: evalGroundTruthName, Data: groundTruth})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for index, file := range files {
		if file.Path == "" || filepath.IsAbs(file.Path) || strings.Contains(file.Path, "..") {
			return nil, fmt.Errorf("eval/dataset: invalid output path %q", file.Path)
		}
		if index > 0 && files[index-1].Path == file.Path {
			return nil, fmt.Errorf("eval/dataset: duplicate output path %q", file.Path)
		}
	}
	return files, nil
}

func buildManifest(cfg Config, result *modes.Result, truth GroundTruth, files []synth.File,
	templates []injections.Template, sources map[string]config.SourceConfig) (Manifest, error) {
	hashes, err := fileHashes(files)
	if err != nil {
		return Manifest{}, err
	}
	modeCounts := zeroModeCounts()
	difficultyCounts := make(map[string]int)
	for _, scenario := range truth.Scenarios {
		modeCounts[scenario.Mode]++
		difficultyCounts[scenario.Difficulty]++
	}
	applicationCounts := zeroModeCounts()
	for name, count := range cfg.Modes.Counts {
		applicationCounts[name] = count
	}
	manifest := Manifest{
		SchemaVersion:            ManifestSchemaVersion,
		GeneratorVersion:         result.Manifest.GeneratorVersion,
		GeneratorDatasetVersion:  result.Manifest.DatasetVersion,
		Split:                    cfg.Split,
		Seed:                     cfg.Seed,
		Days:                     cfg.Days,
		Difficulty:               cfg.Difficulty,
		SynthConfigCanonical:     cfg.Synth.Canonical(cfg.Days, cfg.Difficulty),
		ModesConfigCanonical:     canonicalModesConfig(cfg.Modes),
		Noise:                    result.Manifest.Noise,
		SourceCalendarsCanonical: canonicalSourceCalendars(sources),
		InjectionTemplateHashes:  injectionTemplateHashes(templates),
		TruthSchemaVersion:       TruthSchemaVersion,
		Counts:                   result.Manifest.Counts,
		ApplicationCounts:        applicationCounts,
		ModeCounts:               modeCounts,
		DifficultyCounts:         difficultyCounts,
		Files:                    hashes,
	}
	version, err := ComputeEvalDatasetVersion(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.EvalDatasetVersion = version
	return manifest, nil
}

func zeroModeCounts() map[string]int {
	counts := make(map[string]int, len(modes.AllModes)+1)
	counts[modes.CleanMode] = 0
	for _, mode := range modes.AllModes {
		counts[mode] = 0
	}
	return counts
}

func fileHashes(files []synth.File) (map[string]string, error) {
	hashes := make(map[string]string, len(files))
	for _, file := range files {
		if _, duplicate := hashes[file.Path]; duplicate {
			return nil, fmt.Errorf("eval/dataset: duplicate output path %q", file.Path)
		}
		sum := sha256.Sum256(file.Data)
		hashes[file.Path] = hex.EncodeToString(sum[:])
	}
	return hashes, nil
}

// WriteWorld writes the transient world files under dir. Tests and M3-02 use
// this; the repository never commits the output.
func (d *Dataset) WriteWorld(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("eval/dataset: world directory is required")
	}
	for _, file := range d.Files {
		full := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("eval/dataset: create directory for %s: %w", file.Path, err)
		}
		if err := os.WriteFile(full, file.Data, 0o644); err != nil {
			return fmt.Errorf("eval/dataset: write %s: %w", file.Path, err)
		}
	}
	return nil
}

func jsonDocument(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("eval/dataset: render document: %w", err)
	}
	return append(data, '\n'), nil
}
