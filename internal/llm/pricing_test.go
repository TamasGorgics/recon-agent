package llm

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const fallbackFixture = `
schema_version: 1
source_url: https://example.test/models
fetched_at: "2026-11-06"
models:
  - provider: opencode-go
    id: primary
    selector: opencode-go/primary
    context_limit: 1000
    output_limit: 100
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: false
      structured_output: false
      cache: false
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: false
  - provider: opencode-go
    id: backup1
    selector: opencode-go/backup1
    context_limit: 1000
    output_limit: 100
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: false
      structured_output: false
      cache: false
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: true
  - provider: opencode-go
    id: backup2
    selector: opencode-go/backup2
    context_limit: 1000
    output_limit: 100
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: false
      structured_output: false
      cache: false
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: true
  - provider: opencode-go
    id: backup3
    selector: opencode-go/backup3
    context_limit: 1000
    output_limit: 100
    capabilities:
      tools: true
      tool_choice: ["auto"]
      temperature: true
      reasoning: false
      structured_output: false
      cache: false
    prices_per_million:
      input: "1"
      output: "2"
      cache_read: "0"
      cache_write_5m: "0"
      cache_write_1h: "0"
    allowed_fallback: true
`

func TestPricingExactCosts(t *testing.T) {
	// covers: FIN-01
	registry := DefaultRegistry()
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) // Wednesday, off-peak

	tests := []struct {
		name   string
		model  string
		tokens Tokens
		want   string
	}{
		{"deepseek text", "deepseek-v4.1-flash", Tokens{Input: 38, Output: 16}, "0.0000153"},
		{"deepseek tool with cache", "deepseek-v4.1-flash", Tokens{Input: 28, CacheRead: 256, Output: 24}, "0.000019368"},
		{"glm tool", "glm-5.3-flash", Tokens{Input: 148, Output: 6}, "0.0000252"},
		{"cache write zero", "deepseek-v4.1-flash", Tokens{CacheWrite5m: 100, CacheWrite1h: 100}, "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := registry.ComputeCost(tt.model, tt.tokens, at)
			if err != nil {
				t.Fatalf("ComputeCost: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("ComputeCost = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPricingPeakBoundaries(t *testing.T) {
	model, err := DefaultRegistry().Get("deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	day := func(y int, m time.Month, d, hour, minute, sec int) time.Time {
		return time.Date(y, m, d, hour, minute, sec, 0, time.UTC)
	}
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"monday 00:59:59", day(2026, 11, 2, 0, 59, 59), "1"},
		{"monday 01:00:00", day(2026, 11, 2, 1, 0, 0), "2"},
		{"monday 03:59:59", day(2026, 11, 2, 3, 59, 59), "2"},
		{"monday 04:00:00", day(2026, 11, 2, 4, 0, 0), "1"},
		{"monday 05:59:59", day(2026, 11, 2, 5, 59, 59), "1"},
		{"monday 06:00:00", day(2026, 11, 2, 6, 0, 0), "2"},
		{"friday 09:59:59", day(2026, 11, 6, 9, 59, 59), "2"},
		{"friday 10:00:00", day(2026, 11, 6, 10, 0, 0), "1"},
		{"saturday 02:00:00", day(2026, 11, 7, 2, 0, 0), "1"},
		{"sunday 07:00:00", day(2026, 11, 8, 7, 0, 0), "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := model.PeakMultiplierAt(tt.at).String(); got != tt.want {
				t.Fatalf("PeakMultiplierAt = %s, want %s", got, tt.want)
			}
		})
	}
	peaked, err := DefaultRegistry().ComputeCost("deepseek-v4.1-flash", Tokens{Input: 38, Output: 16}, day(2026, 11, 2, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := peaked.String(), "0.0000306"; got != want {
		t.Fatalf("peak cost = %s, want %s", got, want)
	}
}

func TestPricingUnknownModel(t *testing.T) {
	if _, err := DefaultRegistry().ComputeCost("no-such-model", Tokens{Input: 1}, time.Now()); err == nil {
		t.Fatal("ComputeCost accepted an unknown model")
	} else if !strings.Contains(err.Error(), ErrUnknownModel.Error()) {
		t.Fatalf("ComputeCost error = %v, want %v", err, ErrUnknownModel)
	}
	if DefaultRegistry().AllowedFallback("no-such-model") {
		t.Fatal("AllowedFallback accepted an unknown model")
	}
}

func TestPricingSnapshotConsistency(t *testing.T) {
	var embedded pricingDocument
	if err := yaml.Unmarshal(pricingYAML, &embedded); err != nil {
		t.Fatalf("unmarshal embedded pricing: %v", err)
	}
	data, err := os.ReadFile("testdata/models_snapshot.json")
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snapshot pricingDocument
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if !reflect.DeepEqual(embedded, snapshot) {
		t.Fatalf("embedded pricing.yaml differs from reviewed snapshot\nembedded: %+v\nsnapshot: %+v", embedded, snapshot)
	}
	registry := DefaultRegistry()
	for _, entry := range snapshot.Models {
		model, err := registry.Get(entry.ID)
		if err != nil {
			t.Fatalf("registry missing %q: %v", entry.ID, err)
		}
		if model.ContextLimit != entry.ContextLimit || model.OutputLimit != entry.OutputLimit {
			t.Errorf("%s limits = %d/%d, want %d/%d", entry.ID, model.ContextLimit, model.OutputLimit, entry.ContextLimit, entry.OutputLimit)
		}
		if model.AllowedFallback != entry.AllowedFallback {
			t.Errorf("%s allowed_fallback = %v, want %v", entry.ID, model.AllowedFallback, entry.AllowedFallback)
		}
	}
}

func TestPricingMalformedTable(t *testing.T) {
	valid := fallbackFixture
	tests := []struct {
		name string
		data string
	}{
		{"schema", strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1)},
		{"source", strings.Replace(valid, "source_url: https://example.test/models", `source_url: ""`, 1)},
		{"date", strings.Replace(valid, `fetched_at: "2026-11-06"`, `fetched_at: "not-a-date"`, 1)},
		{"negative price", strings.Replace(valid, `input: "1"`, `input: "-1"`, 1)},
		{"bad price", strings.Replace(valid, `input: "1"`, `input: "one"`, 1)},
		{"bad window", strings.Replace(string(pricingYAML), `start: "01:00"`, `start: "25:00"`, 1)},
		{"bad day", strings.Replace(string(pricingYAML), `["Mon", "Tue", "Wed", "Thu", "Fri"]`, `["Funday"]`, 1)},
		{"duplicate", strings.Replace(valid, "id: backup1\n    selector: opencode-go/backup1", "id: primary\n    selector: opencode-go/primary", 1)},
		{"bad tool choice", strings.Replace(valid, `tool_choice: ["auto"]`, `tool_choice: ["sometimes"]`, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadRegistry([]byte(tt.data)); err == nil {
				t.Fatal("LoadRegistry accepted a malformed table")
			}
		})
	}
}

func TestFallbackAllowlist(t *testing.T) {
	registry, err := LoadRegistry([]byte(fallbackFixture))
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if !registry.AllowedFallback("backup1") || !registry.AllowedFallback("opencode-go/backup2") {
		t.Fatal("allowed fallbacks were rejected")
	}
	if registry.AllowedFallback("primary") {
		t.Fatal("AllowedFallback accepted a disallowed model")
	}
	if err := registry.ValidateFallback("primary", []string{"backup1", "opencode-go/backup2"}); err != nil {
		t.Fatalf("ValidateFallback(valid) = %v", err)
	}
	tests := []struct {
		name      string
		primary   string
		fallbacks []string
		want      string
	}{
		{"unknown primary", "missing", nil, "unknown"},
		{"unknown fallback", "primary", []string{"missing"}, "unknown"},
		{"disallowed", "primary", []string{"primary"}, "equals"},
		{"duplicate", "primary", []string{"backup1", "backup1"}, "duplicated"},
		{"too many", "primary", []string{"backup1", "backup2", "backup3", "backup1"}, "maximum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := registry.ValidateFallback(tt.primary, tt.fallbacks)
			if err == nil {
				t.Fatalf("ValidateFallback(%q, %v) = nil", tt.primary, tt.fallbacks)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateFallback error = %v, want %q", err, tt.want)
			}
		})
	}
	if err := registry.ValidateFallback("primary", []string{"primary"}); err == nil {
		t.Fatal("ValidateFallback accepted the primary as a fallback")
	}
	if DefaultRegistry().AllowedFallback("glm-5.3-flash") {
		t.Fatal("GLM must not be allowed as a fallback before the eval gate")
	}
}
