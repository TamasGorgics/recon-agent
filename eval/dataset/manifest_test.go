package dataset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/injections"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

var updateGolden = flag.Bool("update", false, "update committed eval manifests")

// TestCommittedManifests asserts that eval/datasets/<split>/manifest.json is a
// byte-identical rebuild with the current generator/config. Run with -update
// only when intentionally regenerating.
func TestCommittedManifests(t *testing.T) {
	for _, split := range Splits() {
		t.Run(string(split), func(t *testing.T) {
			built := cachedDataset(t, split)
			dir := filepath.Join("..", "datasets", string(split))
			if *updateGolden {
				if err := built.WriteManifest(dir); err != nil {
					t.Fatalf("write manifest: %v", err)
				}
				return
			}
			committed, err := LoadManifest(dir)
			if err != nil {
				t.Fatalf("load committed manifest (run go test ./eval/dataset -run TestCommittedManifests -update to generate): %v", err)
			}
			want, err := manifestBytes(built.Manifest)
			if err != nil {
				t.Fatalf("render built manifest: %v", err)
			}
			got, err := manifestBytes(committed)
			if err != nil {
				t.Fatalf("render committed manifest: %v", err)
			}
			if !bytes.Equal(want, got) {
				t.Fatalf("committed manifest drifted from a fresh build\nwant:\n%s\ngot:\n%s", want, got)
			}
		})
	}
}

func TestCommittedManifestHygieneAndBudget(t *testing.T) {
	root := filepath.Join("..", "datasets")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("eval datasets directory: %v", err)
	}
	splits := make(map[string]struct{}, len(Splits()))
	for _, split := range Splits() {
		splits[string(split)] = struct{}{}
	}
	totalBytes := int64(0)
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case len(parts) == 1 && parts[0] == "README.md" && !entry.IsDir():
			return nil
		case len(parts) == 1 && !entry.IsDir():
			t.Fatalf("unexpected committed file eval/datasets/%s", rel)
		case len(parts) == 1 && entry.IsDir():
			if _, ok := splits[parts[0]]; !ok {
				t.Fatalf("unexpected split directory eval/datasets/%s", rel)
			}
			return nil
		case len(parts) == 2 && entry.IsDir():
			t.Fatalf("unexpected nested directory eval/datasets/%s", rel)
		case len(parts) == 2 && parts[1] == manifestName:
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > 128*1024 {
				t.Fatalf("manifest %s is %d bytes, budget is 128 KiB", rel, info.Size())
			}
			totalBytes += info.Size()
			files = append(files, rel)
			return nil
		default:
			t.Fatalf("unexpected committed world file eval/datasets/%s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk datasets: %v", err)
	}
	if totalBytes > 256*1024 {
		t.Fatalf("committed manifests total %d bytes, budget is 256 KiB", totalBytes)
	}
	sort.Strings(files)
	want := []string{"dev/manifest.json", "injection/manifest.json", "test/manifest.json"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("committed manifests = %v, want %v", files, want)
	}
}

func TestEvalVersionCoversManifestFields(t *testing.T) {
	base := cloneManifest(cachedDataset(t, Test).Manifest)
	baseVersion := base.EvalDatasetVersion
	mutations := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"file hash", func(m *Manifest) {
			m.Files = cloneStringMap(m.Files)
			m.Files["ledger.json"] = strings.Repeat("0", 64)
		}},
		{"mode count", func(m *Manifest) {
			m.ModeCounts = cloneIntMap(m.ModeCounts)
			m.ModeCounts[modes.FeeVariance]++
		}},
		{"application count", func(m *Manifest) {
			m.ApplicationCounts = cloneIntMap(m.ApplicationCounts)
			m.ApplicationCounts[modes.FeeVariance]++
		}},
		{"difficulty count", func(m *Manifest) {
			m.DifficultyCounts = cloneIntMap(m.DifficultyCounts)
			m.DifficultyCounts["realistic"]++
		}},
		{"synth config", func(m *Manifest) { m.SynthConfigCanonical += ";mutated" }},
		{"modes config", func(m *Manifest) { m.ModesConfigCanonical += ";mutated" }},
		{"source calendars", func(m *Manifest) { m.SourceCalendarsCanonical += ";mutated" }},
		{"template hash", func(m *Manifest) {
			m.InjectionTemplateHashes = cloneStringMap(m.InjectionTemplateHashes)
			m.InjectionTemplateHashes["mutated.txt"] = strings.Repeat("1", 64)
		}},
		{"truth schema version", func(m *Manifest) { m.TruthSchemaVersion = "2" }},
		{"noise", func(m *Manifest) { m.Noise.UnrelatedBreaks++ }},
		{"counts", func(m *Manifest) { m.Counts.LedgerTxns++ }},
		{"seed", func(m *Manifest) { m.Seed++ }},
		{"days", func(m *Manifest) { m.Days++ }},
		{"difficulty", func(m *Manifest) { m.Difficulty = synth.DifficultyEasy }},
		{"generator version", func(m *Manifest) { m.GeneratorVersion = "0.0.0" }},
		{"generator dataset version", func(m *Manifest) { m.GeneratorDatasetVersion = strings.Repeat("2", 64) }},
		{"split", func(m *Manifest) { m.Split = Dev }},
		{"manifest schema", func(m *Manifest) { m.SchemaVersion = "2" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := cloneManifest(base)
			mutation.mutate(&changed)
			version, err := ComputeEvalDatasetVersion(changed)
			if err != nil {
				t.Fatalf("ComputeEvalDatasetVersion: %v", err)
			}
			if version == baseVersion {
				t.Fatalf("%s mutation did not change eval_dataset_version", mutation.name)
			}
		})
	}
}

func TestEvalVersionChangesWithTemplateAndCalendar(t *testing.T) {
	base := cachedDataset(t, Dev)
	cfg, err := DefaultConfig(Dev)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}

	t.Run("template", func(t *testing.T) {
		templates, err := injections.All()
		if err != nil {
			t.Fatalf("load templates: %v", err)
		}
		mutated := append([]injections.Template(nil), templates...)
		mutated[0].Text += " [mutated template]"
		cfg.Templates = mutated
		built, err := Build(cfg)
		if err != nil {
			t.Fatalf("Build with mutated template: %v", err)
		}
		if built.Manifest.GeneratorDatasetVersion != base.Manifest.GeneratorDatasetVersion {
			t.Fatal("template mutation changed generator_dataset_version; generator seed/config changed")
		}
		if built.Manifest.EvalDatasetVersion == base.Manifest.EvalDatasetVersion {
			t.Fatal("template mutation did not change eval_dataset_version")
		}
	})
	t.Run("calendar", func(t *testing.T) {
		cfg, err := DefaultConfig(Dev)
		if err != nil {
			t.Fatalf("DefaultConfig: %v", err)
		}
		sources := make(map[string]config.SourceConfig, len(cfg.Sources))
		for name, source := range cfg.Sources {
			source.BusinessDays = append([]string(nil), source.BusinessDays...)
			source.Holidays = append([]string(nil), source.Holidays...)
			source.UntrustedFields = append([]string(nil), source.UntrustedFields...)
			sources[name] = source
		}
		psp := sources[synth.SourcePSP]
		psp.Cutoff = "16:00"
		sources[synth.SourcePSP] = psp
		cfg.Sources = sources
		built, err := Build(cfg)
		if err != nil {
			t.Fatalf("Build with mutated calendar: %v", err)
		}
		if built.Manifest.GeneratorDatasetVersion != base.Manifest.GeneratorDatasetVersion {
			t.Fatal("calendar mutation changed generator_dataset_version; generator seed/config changed")
		}
		if built.Manifest.SourceCalendarsCanonical == base.Manifest.SourceCalendarsCanonical {
			t.Fatal("calendar mutation did not change the calendar canonical")
		}
		if built.Manifest.EvalDatasetVersion == base.Manifest.EvalDatasetVersion {
			t.Fatal("calendar mutation did not change eval_dataset_version")
		}
	})
}

func TestGroundTruthDoesNotCarryEvalVersion(t *testing.T) {
	built := cachedDataset(t, Test)
	var truthFile []byte
	for _, file := range built.Files {
		if file.Path == evalGroundTruthName {
			truthFile = file.Data
			break
		}
	}
	if truthFile == nil {
		t.Fatal("generated world has no eval ground_truth.json")
	}
	if bytes.Contains(truthFile, []byte("eval_dataset_version")) {
		t.Fatal("ground_truth.json carries eval_dataset_version (circular hash)")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(truthFile, &document); err != nil {
		t.Fatalf("decode ground truth: %v", err)
	}
	if _, ok := document["generator_dataset_version"]; !ok {
		t.Fatal("ground_truth.json is missing generator_dataset_version")
	}
	if len(document) != 3 {
		t.Fatalf("ground_truth.json has %d top-level fields, want 3", len(document))
	}
}

func TestManifestCountsAndHashesMatchWorld(t *testing.T) {
	built := cachedDataset(t, Test)
	if len(built.Manifest.Files) != len(built.Files) {
		t.Fatalf("manifest hashes %d files, world has %d", len(built.Manifest.Files), len(built.Files))
	}
	for _, file := range built.Files {
		hash, ok := built.Manifest.Files[file.Path]
		if !ok {
			t.Fatalf("world file %s is not hashed in the manifest", file.Path)
		}
		sum := sha256.Sum256(file.Data)
		if hash != hex.EncodeToString(sum[:]) {
			t.Fatalf("world file %s hash mismatch", file.Path)
		}
	}
	var ledger synth.LedgerDocument
	for _, file := range built.Files {
		if file.Path == "ledger.json" {
			if err := json.Unmarshal(file.Data, &ledger); err != nil {
				t.Fatalf("decode ledger.json: %v", err)
			}
		}
	}
	counts := built.Manifest.Counts
	if counts.Accounts != len(ledger.Accounts) || counts.Customers != len(ledger.Customers) ||
		counts.Merchants != len(ledger.Merchants) || counts.FeeSchedules != len(ledger.FeeSchedules) ||
		counts.LedgerTxns != len(ledger.Transactions) {
		t.Fatalf("manifest reference counts do not match ledger.json: %+v vs %d/%d/%d/%d/%d",
			counts, len(ledger.Accounts), len(ledger.Customers), len(ledger.Merchants),
			len(ledger.FeeSchedules), len(ledger.Transactions))
	}
	externalRows := 0
	for _, file := range built.Files {
		switch {
		case strings.HasPrefix(file.Path, synth.SourcePSP+"/"):
			records, err := parseExternalFile(file, synth.SourcePSP, pspColumns)
			if err != nil {
				t.Fatalf("parse %s: %v", file.Path, err)
			}
			externalRows += len(records)
		case strings.HasPrefix(file.Path, synth.SourceBank+"/"):
			records, err := parseExternalFile(file, synth.SourceBank, bankColumns)
			if err != nil {
				t.Fatalf("parse %s: %v", file.Path, err)
			}
			externalRows += len(records)
		}
	}
	if counts.ExternalRecords != externalRows {
		t.Fatalf("manifest external_records = %d, world rows = %d", counts.ExternalRecords, externalRows)
	}
	selectedLinks := 0
	for _, scenario := range built.Truth.Scenarios {
		selectedLinks += len(scenario.ExternalRecords)
	}
	if counts.GroundTruthLinks != selectedLinks {
		t.Fatalf("manifest ground_truth_links = %d, selected truth links = %d",
			counts.GroundTruthLinks, selectedLinks)
	}
}

func cloneManifest(manifest Manifest) Manifest {
	cloned := manifest
	cloned.Files = cloneStringMap(manifest.Files)
	cloned.InjectionTemplateHashes = cloneStringMap(manifest.InjectionTemplateHashes)
	cloned.ModeCounts = cloneIntMap(manifest.ModeCounts)
	cloned.ApplicationCounts = cloneIntMap(manifest.ApplicationCounts)
	cloned.DifficultyCounts = cloneIntMap(manifest.DifficultyCounts)
	return cloned
}

func cloneStringMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneIntMap(values map[string]int) map[string]int {
	cloned := make(map[string]int, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
