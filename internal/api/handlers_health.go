package api

import "net/http"

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

const metricsBody = `# HELP recon_build_info Build information for the reconciliation API.
# TYPE recon_build_info gauge
recon_build_info{component="api"} 1
`

func (a *API) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(metricsBody))
}
