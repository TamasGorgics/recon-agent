package api

import "net/http"

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

// handleMetrics serves the process registry. The handler is public and keeps
// the frozen text/plain content type; total gather failure is a 500.
func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	a.metricsHandler.ServeHTTP(w, r)
}
