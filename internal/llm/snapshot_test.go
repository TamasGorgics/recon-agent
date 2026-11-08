package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// covers: FIN-05
// The snapshot must carry money as decimal strings, never fractional JSON
// numbers, so an eval run's pricing history cannot be rounded by a float.
func TestPricingSnapshotDecimalStrings(t *testing.T) {
	snapshot := PricingSnapshot()
	if snapshot.Schema != SnapshotSchema {
		t.Fatalf("schema = %q, want %q", snapshot.Schema, SnapshotSchema)
	}
	if !strings.HasPrefix(snapshot.Source, "https://") || snapshot.FetchedAt == "" {
		t.Fatalf("source/fetched_at missing: %+v", snapshot)
	}
	const modelID = "deepseek-v4.1-flash"
	model, ok := snapshot.Models[modelID]
	if !ok {
		t.Fatalf("model snapshot missing for %q", modelID)
	}
	for name, value := range map[string]string{
		"input":      model.InputPerMillion,
		"output":     model.OutputPerMillion,
		"cache_read": model.CacheReadPerMillion,
	} {
		if value == "" {
			t.Fatalf("%s rate is empty", name)
		}
		if strings.ContainsAny(value, "eE") {
			t.Fatalf("%s rate %q uses exponent notation", name, value)
		}
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var envelope struct {
		Models map[string]map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	for modelID, rates := range envelope.Models {
		for name, value := range rates {
			if name == "currency" {
				continue
			}
			if _, ok := value.(string); !ok {
				t.Fatalf("%s.%s is %T, want a decimal string", modelID, name, value)
			}
		}
	}
}
