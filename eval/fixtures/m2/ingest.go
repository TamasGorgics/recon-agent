package m2

import "github.com/TamasGorgics/recon-agent/internal/config"

// IngestConfig returns the ingest configuration for the fixture's eval-only
// source. The read tools use it to label the synthetic memo and reference
// fields as untrusted, so the model-visible transcript wraps them and the
// injection detector scans them. The production synth sources are unchanged.
func IngestConfig() config.IngestConfig {
	return config.IngestConfig{Sources: map[string]config.SourceConfig{
		fixtureSource: {
			Timezone:        "UTC",
			Cutoff:          "00:00",
			UntrustedFields: []string{"memo", "reference"},
		},
	}}
}
