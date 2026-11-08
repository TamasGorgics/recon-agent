package llm

import "fmt"

// SnapshotSchema is the persisted pricing-snapshot schema name. Versioned so a
// future registry schema can change the snapshot shape without ambiguity.
const SnapshotSchema = "llm-pricing/v1"

// Snapshot is a point-in-time copy of the embedded pricing registry. Monetary
// rates are decimal strings, never JSON numbers, so a stored eval run can be
// reproduced without floating-point rounding.
type Snapshot struct {
	Schema    string                   `json:"schema"`
	Source    string                   `json:"source"`
	FetchedAt string                   `json:"fetched_at"`
	Models    map[string]ModelSnapshot `json:"models"`
}

// ModelSnapshot is one model's per-million-token rates.
type ModelSnapshot struct {
	InputPerMillion     string `json:"input_per_million"`
	OutputPerMillion    string `json:"output_per_million"`
	CacheReadPerMillion string `json:"cache_read_per_million"`
	Currency            string `json:"currency"`
}

// PricingSnapshot returns a snapshot of the embedded default registry. Callers
// capture it once per eval run; later pricing changes never rewrite history.
func PricingSnapshot() Snapshot { return defaultRegistry.Snapshot() }

// Snapshot returns a point-in-time copy of the registry.
func (r *Registry) Snapshot() Snapshot {
	snapshot := Snapshot{
		Schema:    fmt.Sprintf("llm-pricing/v%d", r.SchemaVersion),
		Source:    r.SourceURL,
		FetchedAt: r.FetchedAt,
		Models:    make(map[string]ModelSnapshot, len(r.models)),
	}
	for id, model := range r.models {
		snapshot.Models[id] = ModelSnapshot{
			InputPerMillion:     model.Prices.Input.String(),
			OutputPerMillion:    model.Prices.Output.String(),
			CacheReadPerMillion: model.Prices.CacheRead.String(),
			Currency:            "USD",
		}
	}
	return snapshot
}
