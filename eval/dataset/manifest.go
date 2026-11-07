package dataset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/injections"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

const (
	// ManifestSchemaVersion is the committed manifest format version.
	ManifestSchemaVersion = "1"
	// TruthSchemaVersion is the ground-truth JSON schema version.
	TruthSchemaVersion = "1"
	// manifestName is the committed per-split manifest file name.
	manifestName = "manifest.json"
	// evalGroundTruthName is the eval wrapper file name inside a generated
	// world. The raw synth ground_truth.json is intentionally not included.
	evalGroundTruthName = "ground_truth.json"
)

// Manifest is eval/datasets/<split>/manifest.json. It carries enough
// provenance to reproduce and compare a generated world without committing the
// world itself.
type Manifest struct {
	SchemaVersion            string            `json:"schema_version"`
	EvalDatasetVersion       string            `json:"eval_dataset_version,omitempty"`
	GeneratorVersion         string            `json:"generator_version"`
	GeneratorDatasetVersion  string            `json:"generator_dataset_version"`
	Split                    Split             `json:"split"`
	Seed                     int64             `json:"seed"`
	Days                     int               `json:"days"`
	Difficulty               string            `json:"difficulty"`
	SynthConfigCanonical     string            `json:"synth_config_canonical"`
	ModesConfigCanonical     string            `json:"modes_config_canonical"`
	Noise                    modes.Noise       `json:"noise"`
	SourceCalendarsCanonical string            `json:"source_calendars_canonical"`
	InjectionTemplateHashes  map[string]string `json:"injection_template_hashes"`
	TruthSchemaVersion       string            `json:"truth_schema_version"`
	Counts                   synth.Counts      `json:"counts"`
	ApplicationCounts        map[string]int    `json:"application_counts"`
	ModeCounts               map[string]int    `json:"mode_counts"`
	DifficultyCounts         map[string]int    `json:"difficulty_counts"`
	Files                    map[string]string `json:"files"`
}

// ComputeEvalDatasetVersion returns SHA-256 over the canonical compact JSON of
// every manifest field except eval_dataset_version. The ground truth does not
// carry this value, so there is no circular hash.
func ComputeEvalDatasetVersion(manifest Manifest) (string, error) {
	canonical := manifest
	canonical.EvalDatasetVersion = ""
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("eval/dataset: canonical manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// manifestBytes renders the committed pretty-printed form with a trailing
// newline. encoding/json sorts map keys, so the bytes are deterministic.
func manifestBytes(manifest Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("eval/dataset: render manifest: %w", err)
	}
	return append(data, '\n'), nil
}

// WriteManifest writes <dir>/manifest.json for tests and the -update workflow.
// Generated worlds are never written here.
func (d *Dataset) WriteManifest(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("eval/dataset: manifest directory is required")
	}
	data, err := manifestBytes(d.Manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("eval/dataset: create manifest directory: %w", err)
	}
	path := filepath.Join(dir, manifestName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("eval/dataset: write %s: %w", path, err)
	}
	return nil
}

// LoadManifest loads and validates a committed manifest. dir may be the split
// directory or the manifest file itself.
func LoadManifest(dir string) (Manifest, error) {
	path := dir
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		path = filepath.Join(dir, manifestName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("eval/dataset: read manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("eval/dataset: decode manifest %s: %w", path, err)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, fmt.Errorf("eval/dataset: manifest %s: %w", path, err)
	}
	return manifest, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("schema_version %q, want %q", manifest.SchemaVersion, ManifestSchemaVersion)
	}
	if !manifest.Split.Valid() {
		return fmt.Errorf("unknown split %q", manifest.Split)
	}
	if manifest.Seed < 0 {
		return errors.New("seed must not be negative")
	}
	if manifest.Days <= 0 {
		return errors.New("days must be positive")
	}
	if manifest.Difficulty == "" {
		return errors.New("difficulty is required")
	}
	if manifest.GeneratorVersion == "" {
		return errors.New("generator_version is required")
	}
	if manifest.GeneratorDatasetVersion == "" {
		return errors.New("generator_dataset_version is required")
	}
	if manifest.TruthSchemaVersion != TruthSchemaVersion {
		return fmt.Errorf("truth_schema_version %q, want %q", manifest.TruthSchemaVersion, TruthSchemaVersion)
	}
	if len(manifest.Files) == 0 {
		return errors.New("files must not be empty")
	}
	for path, hash := range manifest.Files {
		if path == "" || filepath.IsAbs(path) || strings.Contains(path, "..") {
			return fmt.Errorf("invalid file path %q", path)
		}
		if len(hash) != sha256.Size*2 {
			return fmt.Errorf("file %q hash is not SHA-256 hex", path)
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return fmt.Errorf("file %q hash is not hex: %w", path, err)
		}
	}
	want, err := ComputeEvalDatasetVersion(manifest)
	if err != nil {
		return err
	}
	if manifest.EvalDatasetVersion != want {
		return fmt.Errorf("eval_dataset_version %q does not match canonical content %q",
			manifest.EvalDatasetVersion, want)
	}
	return nil
}

// canonicalModesConfig mirrors synth/modes.Config's unexported canonical form:
// total plus present mode counts in name order, zero counts omitted. The eval
// manifest must stay sensitive to the application mix even though the modes
// package does not export the helper.
func canonicalModesConfig(cfg modes.Config) string {
	parts := make([]string, 0, len(cfg.Counts))
	for name, count := range cfg.Counts {
		if count == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, count))
	}
	sort.Strings(parts)
	return fmt.Sprintf("total=%d;%s", cfg.Total, strings.Join(parts, ","))
}

// canonicalSourceCalendars renders a stable representation of every source
// calendar. synth's generator dataset_version deliberately excludes sources,
// so the eval manifest hashes them separately (Q-37).
func canonicalSourceCalendars(sources map[string]config.SourceConfig) string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		source := sources[name]
		parts = append(parts, fmt.Sprintf(
			"%s{business_days=%s;cutoff=%s;holidays=%s;timezone=%s;untrusted_fields=%s}",
			name,
			strings.Join(sortedCopy(source.BusinessDays), ","),
			source.Cutoff,
			strings.Join(sortedCopy(source.Holidays), ","),
			source.Timezone,
			strings.Join(sortedCopy(source.UntrustedFields), ","),
		))
	}
	return strings.Join(parts, ";")
}

// injectionTemplateHashes returns the SHA-256 of each template's trimmed text,
// keyed by file name. The loader trims template files, so hashing the text is
// the stable content identity shared by generator and manifest.
func injectionTemplateHashes(templates []injections.Template) map[string]string {
	hashes := make(map[string]string, len(templates))
	for _, template := range templates {
		sum := sha256.Sum256([]byte(template.Text))
		hashes[template.Name] = hex.EncodeToString(sum[:])
	}
	return hashes
}

func sortedCopy(values []string) []string {
	copied := make([]string, len(values))
	copy(copied, values)
	sort.Strings(copied)
	return copied
}
