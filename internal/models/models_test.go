package models

import (
	"os"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/llm"
)

func committedPath(t *testing.T) string {
	t.Helper()
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	return path
}

func readCommitted(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(committedPath(t))
	if err != nil {
		t.Fatalf("read models.yaml: %v", err)
	}
	return string(data)
}

func mustParse(t *testing.T, text string) Document {
	t.Helper()
	doc, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

func TestCommittedRegistryValidatesAgainstPricing(t *testing.T) {
	doc, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if doc.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	primary, ok := doc.ByRole(RolePrimary)
	if !ok || primary != "deepseek-v4.1-flash" {
		t.Fatalf("primary = %q/%t, want deepseek-v4.1-flash", primary, ok)
	}
	second, ok := doc.ByRole(RoleSecond)
	if !ok || second != "glm-5.3-flash" {
		t.Fatalf("second = %q/%t, want glm-5.3-flash", second, ok)
	}
	if doc.Eval.DefaultModel != primary || doc.Eval.DefaultPrompt != "v1" {
		t.Fatalf("eval defaults = %+v, want primary/v1", doc.Eval)
	}
	for _, entry := range doc.Models {
		if entry.AllowedFallback {
			t.Fatalf("model %q allows automatic fallback; both eval models must be explicit", entry.ID)
		}
	}

	text := readCommitted(t)
	for _, forbidden := range []string{"prices_per_million", "context_limit", "output_limit", "capabilities"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("models.yaml duplicates %q; prices and capabilities stay in internal/llm/pricing.yaml", forbidden)
		}
	}
}

func TestStrictParseRejectsUnknownFields(t *testing.T) {
	tests := map[string]string{
		"top-level price table": "schema_version: 1\nsource: internal/llm/pricing.yaml\nprices_per_million: {}\nmodels: []\neval: {}\n",
		"model price field":     "schema_version: 1\nsource: internal/llm/pricing.yaml\nmodels:\n  - id: deepseek-v4.1-flash\n    role: primary\n    allowed_fallback: false\n    prices_per_million:\n      input: \"0.15\"\neval:\n  default_model: deepseek-v4.1-flash\n  default_prompt: v1\n",
		"model capability":      "schema_version: 1\nsource: internal/llm/pricing.yaml\nmodels:\n  - id: deepseek-v4.1-flash\n    role: primary\n    allowed_fallback: false\n    capabilities:\n      tools: true\neval: {}\n",
	}
	for name, text := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(text))
			if err == nil {
				t.Fatal("Parse accepted a duplicated/unknown field")
			}
			if strings.Contains(name, "price") && !strings.Contains(err.Error(), "prices_per_million") {
				t.Fatalf("error does not name prices_per_million: %v", err)
			}
		})
	}
}

func TestParseRejectsDuplicateKeysAndMultipleDocuments(t *testing.T) {
	if _, err := Parse([]byte("schema_version: 1\nschema_version: 2\n")); err == nil {
		t.Fatal("Parse accepted a duplicate key")
	}
	if _, err := Parse([]byte("schema_version: 1\n---\nschema_version: 1\n")); err == nil {
		t.Fatal("Parse accepted multiple YAML documents")
	}
}

func TestDriftRemovedOrRenamedPricingModelFails(t *testing.T) {
	pricing, err := os.ReadFile("../llm/pricing.yaml")
	if err != nil {
		t.Fatalf("read pricing.yaml: %v", err)
	}
	renamed := strings.ReplaceAll(string(pricing), "glm-5.3-flash", "glm-5.3-flash-renamed")
	registry, err := llm.LoadRegistry([]byte(renamed))
	if err != nil {
		t.Fatalf("LoadRegistry(renamed): %v", err)
	}
	_, err = LoadAndValidate(committedPath(t), registry)
	if err == nil {
		t.Fatal("drift: renamed pricing id was accepted")
	}
	if !strings.Contains(err.Error(), "glm-5.3-flash") || !strings.Contains(err.Error(), "not in") {
		t.Fatalf("drift error does not name the missing id: %v", err)
	}
}

func TestDriftAllowedFallbackChangeFails(t *testing.T) {
	pricing, err := os.ReadFile("../llm/pricing.yaml")
	if err != nil {
		t.Fatalf("read pricing.yaml: %v", err)
	}
	// The glm model is the only one whose tail is cache_write_1h directly
	// followed by allowed_fallback (deepseek has a peak block in between).
	needle := "cache_write_1h: \"0\"\n    allowed_fallback: false"
	if strings.Count(string(pricing), needle) != 1 {
		t.Fatalf("pricing.yaml tail is not unique (%d matches); update this drift test",
			strings.Count(string(pricing), needle))
	}
	mutated := strings.Replace(string(pricing), needle,
		"cache_write_1h: \"0\"\n    allowed_fallback: true", 1)
	registry, err := llm.LoadRegistry([]byte(mutated))
	if err != nil {
		t.Fatalf("LoadRegistry(mutated): %v", err)
	}
	_, err = LoadAndValidate(committedPath(t), registry)
	if err == nil {
		t.Fatal("drift: allowed_fallback change was accepted")
	}
	if !strings.Contains(err.Error(), "allowed_fallback") || !strings.Contains(err.Error(), "glm-5.3-flash") {
		t.Fatalf("drift error does not name model and field: %v", err)
	}
}

func TestValidateRejectsBadRoles(t *testing.T) {
	base := readCommitted(t)
	tests := map[string]struct {
		text string
		want string
	}{
		"two primary": {
			strings.Replace(base, "role: second", "role: primary", 1),
			"role",
		},
		"missing second": {
			strings.Replace(base, "  - id: glm-5.3-flash\n    role: second\n    allowed_fallback: false\n", "", 1),
			`exactly one "second"`,
		},
		"unknown role": {
			strings.Replace(base, "role: second", "role: fallback", 1),
			"role",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			doc := mustParse(t, test.text)
			if err := doc.Validate(llm.DefaultRegistry()); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestValidateRejectsBadDefaults(t *testing.T) {
	base := readCommitted(t)
	tests := map[string]struct {
		text string
		want string
	}{
		"unlisted default model": {
			strings.Replace(base, "default_model: deepseek-v4.1-flash", "default_model: gpt-unknown", 1),
			"eval.default_model",
		},
		"unknown default prompt": {
			strings.Replace(base, "default_prompt: v1", "default_prompt: v99", 1),
			"eval.default_prompt",
		},
		"wrong source": {
			strings.Replace(base, "source: internal/llm/pricing.yaml", "source: internal/llm/other.yaml", 1),
			"source",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			doc := mustParse(t, test.text)
			if err := doc.Validate(llm.DefaultRegistry()); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestValidateRejectsDuplicateModel(t *testing.T) {
	base := readCommitted(t)
	text := strings.Replace(base, "  - id: glm-5.3-flash",
		"  - id: deepseek-v4.1-flash", 1)
	doc := mustParse(t, text)
	if err := doc.Validate(llm.DefaultRegistry()); err == nil ||
		!strings.Contains(err.Error(), "more than once") {
		t.Fatalf("Validate = %v, want duplicate error", err)
	}
}
