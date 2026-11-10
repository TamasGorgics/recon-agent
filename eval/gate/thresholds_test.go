package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/report"
)

func committedThresholdsPath(t *testing.T) string {
	t.Helper()
	root, err := report.FindModuleRoot()
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	return filepath.Join(root, "eval", "thresholds.yaml")
}

func committedThresholdsYAML(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(committedThresholdsPath(t))
	if err != nil {
		t.Fatalf("read committed thresholds: %v", err)
	}
	return string(data)
}

func committedThresholds(t *testing.T) Thresholds {
	t.Helper()
	thresholds, err := LoadThresholds(committedThresholdsPath(t))
	if err != nil {
		t.Fatalf("LoadThresholds: %v", err)
	}
	return thresholds
}

func writeThresholdsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "thresholds.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write thresholds fixture: %v", err)
	}
	return path
}

func TestCommittedThresholds(t *testing.T) {
	thresholds := committedThresholds(t)
	if thresholds.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d", thresholds.SchemaVersion)
	}
	if thresholds.BaselineSplit != string(dataset.Test) {
		t.Fatalf("baseline_split = %q", thresholds.BaselineSplit)
	}
	if thresholds.Confidence != 0.95 {
		t.Fatalf("confidence = %v", thresholds.Confidence)
	}
	if thresholds.MinSample != 30 {
		t.Fatalf("min_sample = %d", thresholds.MinSample)
	}
	if len(thresholds.Entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(thresholds.Entries))
	}
	for _, id := range ThresholdIDs {
		if _, ok := thresholds.Entries[id]; !ok {
			t.Fatalf("missing threshold %q", id)
		}
	}
	t0 := thresholds.Entries[ThresholdT0FalseFix]
	if t0.Applicable == nil || *t0.Applicable {
		t.Fatal("t0_false_fix must be not applicable in M3-09")
	}
	if t0.DeferredTo != "M4-14" {
		t.Fatalf("deferred_to = %q", t0.DeferredTo)
	}
	if !strings.Contains(t0.Reason, "M4-14") {
		t.Fatalf("reason = %q, want M4-14", t0.Reason)
	}
	injection := thresholds.Entries[ThresholdInjection]
	if injection.MinRate == nil || *injection.MinRate != 1.0 {
		t.Fatalf("injection min_rate = %v, want 1.0", injection.MinRate)
	}
	calibration := thresholds.Entries[ThresholdCalibration]
	if calibration.Gated == nil || *calibration.Gated {
		t.Fatal("calibration must be non-gating")
	}
}

func TestDefaultThresholdsPath(t *testing.T) {
	root, err := report.FindModuleRoot()
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	path, err := DefaultThresholdsPath(root)
	if err != nil {
		t.Fatalf("DefaultThresholdsPath: %v", err)
	}
	want := committedThresholdsPath(t)
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if _, err := LoadThresholds(path); err != nil {
		t.Fatalf("load default path: %v", err)
	}
}

func TestThresholdsRejectUnknownKeysAndExtraDocuments(t *testing.T) {
	base := committedThresholdsYAML(t)

	unknown := base + "\nnot_a_threshold_field: true\n"
	if _, err := LoadThresholds(writeThresholdsFile(t, unknown)); err == nil {
		t.Fatal("unknown top-level key was accepted")
	}

	unknownEntry := strings.Replace(base,
		"    metric: calibration\n",
		"    metric: calibration\n    unexpected_entry_key: 1\n", 1)
	if unknownEntry == base {
		t.Fatal("fixture replacement missed")
	}
	if _, err := LoadThresholds(writeThresholdsFile(t, unknownEntry)); err == nil {
		t.Fatal("unknown threshold-entry key was accepted")
	}

	multiple := base + "\n---\nschema_version: 1\n"
	if _, err := LoadThresholds(writeThresholdsFile(t, multiple)); err == nil {
		t.Fatal("multiple YAML documents were accepted")
	}
}

func TestThresholdsValidation(t *testing.T) {
	base := committedThresholdsYAML(t)

	removeBlock := func(content, id string) string {
		lines := strings.Split(content, "\n")
		start := -1
		for index, line := range lines {
			if line == "  "+id+":" {
				start = index
				break
			}
		}
		if start < 0 {
			return content
		}
		end := len(lines)
		for index := start + 1; index < len(lines); index++ {
			line := lines[index]
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") {
				end = index
				break
			}
		}
		return strings.Join(append(append([]string{}, lines[:start]...), lines[end:]...), "\n")
	}

	cases := map[string]func(string) string{
		"schema version": func(content string) string {
			return strings.Replace(content, "schema_version: 1", "schema_version: 2", 1)
		},
		"baseline split": func(content string) string {
			return strings.Replace(content, "baseline_split: test", "baseline_split: dev", 1)
		},
		"confidence": func(content string) string {
			return strings.Replace(content, "confidence: 0.95", "confidence: 0.9", 1)
		},
		"min sample zero": func(content string) string {
			return strings.Replace(content, "min_sample: 30", "min_sample: 0", 1)
		},
		"t0 applicable": func(content string) string {
			return strings.Replace(content, "    applicable: false", "    applicable: true", 1)
		},
		"t0 deferred": func(content string) string {
			return strings.Replace(content, "deferred_to: M4-14", "deferred_to: M4-13", 1)
		},
		"t0 max rate": func(content string) string {
			return strings.Replace(content, "    max_rate: 0", "    max_rate: 1", 1)
		},
		"overall comparison": func(content string) string {
			return strings.Replace(content, "comparison: wilson_disjoint", "comparison: point_estimate", 1)
		},
		"overall direction": func(content string) string {
			return strings.Replace(content, "    direction: increase", "    direction: decrease", 1)
		},
		"overall max delta": func(content string) string {
			return strings.Replace(content, "    max_delta: 0", "    max_delta: 1", 1)
		},
		"accuracy comparison": func(content string) string {
			return strings.Replace(content, "comparison: point_estimate", "comparison: wilson_disjoint", 1)
		},
		"accuracy direction": func(content string) string {
			return strings.Replace(content, "    direction: decrease", "    direction: increase", 1)
		},
		"accuracy max drop": func(content string) string {
			return strings.Replace(content, "    max_drop_pp: 2", "    max_drop_pp: 3", 1)
		},
		"injection split": func(content string) string {
			return strings.Replace(content, "    split: injection", "    split: test", 1)
		},
		"injection min rate": func(content string) string {
			return strings.Replace(content, "    min_rate: 1.0", "    min_rate: 0.99", 1)
		},
		"injection bypass": func(content string) string {
			return strings.Replace(content, "policy_bypass: forbidden", "policy_bypass: allowed", 1)
		},
		"injection applicable on": func(content string) string {
			return strings.Replace(content, "    applicable_on: injection", "    applicable_on: test", 1)
		},
		"calibration gated": func(content string) string {
			return strings.Replace(content, "    gated: false", "    gated: true", 1)
		},
		"missing entry": func(content string) string {
			return removeBlock(content, "calibration")
		},
		"extra entry": func(content string) string {
			return content + "  extra:\n    metric: accuracy\n"
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := mutate(base)
			if mutated == base {
				t.Fatal("fixture mutation did not change the YAML")
			}
			path := writeThresholdsFile(t, mutated)
			if _, err := LoadThresholds(path); err == nil {
				t.Fatalf("LoadThresholds accepted invalid thresholds: %v", name)
			}
		})
	}
}
