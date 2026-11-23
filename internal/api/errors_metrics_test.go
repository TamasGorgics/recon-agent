package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/metrics"
)

// TestWriteErrorCountsOnly5xx proves the API's error metric fires on a
// server-side failure with the bounded internal code and stays silent for a
// client validation response.
func TestWriteErrorCountsOnly5xx(t *testing.T) {
	processMetrics, err := metrics.New(metrics.Options{Component: "api"})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	h := newHarness(t, func(opts *Options) { opts.Metrics = processMetrics })
	h.caseList.err = errors.New("database unavailable")

	response := h.expectStatus(http.MethodGet, RouteCases, "", "", testViewerKey, http.StatusInternalServerError)
	_ = response.Body.Close()

	before := gatherValue(t, processMetrics, "internal")
	if before != 1 {
		t.Fatalf("api/internal errors = %v, want 1", before)
	}

	// A 400 validation response is not counted (spec 6.10).
	response = h.expectStatus(http.MethodGet, RouteCases+"?status=bogus", "", "", testViewerKey, http.StatusBadRequest)
	_ = response.Body.Close()
	if got := gatherValue(t, processMetrics, "internal"); got != before {
		t.Fatalf("4xx changed api/internal errors: %v -> %v", before, got)
	}
	if got := gatherValue(t, processMetrics, unknownMetricCode); got != 0 {
		t.Fatalf("4xx created an unknown-coded error series: %v", got)
	}
}

const unknownMetricCode = "unknown"

// gatherValue reads one api error counter by code from the registry.
func gatherValue(t *testing.T, m *metrics.Metrics, code string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != metrics.MetricErrorsTotal {
			continue
		}
		for _, metric := range family.GetMetric() {
			var component, gotCode string
			for _, label := range metric.GetLabel() {
				switch label.GetName() {
				case "component":
					component = label.GetValue()
				case "code":
					gotCode = label.GetValue()
				}
			}
			if component == "api" && gotCode == code {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
