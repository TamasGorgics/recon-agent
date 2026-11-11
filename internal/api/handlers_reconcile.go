package api

import (
	"net/http"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// reconcileRequest is the strict POST /v1/reconcile/run body. Pointers detect a
// missing or null field; the decoder rejects unknown fields and duplicate keys.
type reconcileRequest struct {
	ScopeID *string `json:"scope_id"`
	From    *string `json:"from"`
	To      *string `json:"to"`
}

// handleReconcile implements POST /v1/reconcile/run. The matcher run and the
// break/case persistence are separate transactions; a failure after the run
// returns a generic 500 and logs the run ID, never a partial success.
func (a *API) handleReconcile(w http.ResponseWriter, r *http.Request) {
	var request reconcileRequest
	if err := decodeJSONBody(w, r, &request, a.cfg.MaxJSONBytes); err != nil {
		a.writeClientError(w, r, err)
		return
	}
	scopeID, err := validateScopeID(request.ScopeID)
	if err != nil {
		a.writeClientError(w, r, err)
		return
	}
	from, err := validateDateField("from", request.From)
	if err != nil {
		a.writeClientError(w, r, err)
		return
	}
	to, err := validateDateField("to", request.To)
	if err != nil {
		a.writeClientError(w, r, err)
		return
	}
	if to.Before(from) {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidDateRange,
			"from must be on or before to"))
		return
	}

	result, runErr := a.matcher.Run(r.Context(), match.RunRequest{
		ScopeID: scopeID,
		Window:  match.Window{From: from, To: to},
	})
	if runErr != nil {
		a.writeServiceError(w, r, runErr)
		return
	}

	created := 0
	existing := 0
	casesCreated := 0
	if len(result.Breaks) > 0 {
		breakResult, breakErr := a.cases.CreateBreaks(r.Context(), cases.CreateBreaksRequest{
			ScopeID:    result.ScopeID,
			MatchRunID: result.RunID,
			Breaks:     result.Breaks,
		})
		if breakErr != nil {
			// The matcher run is already committed. Do not claim atomicity and
			// do not expose the partial state or the internal cause.
			a.logger.Error("reconcile: break creation failed after matcher run",
				"request_id", RequestIDFromContext(r.Context()),
				"trace_id", traceIDFromContext(r.Context()),
				"run_id", result.RunID,
				"error", breakErr.Error(),
			)
			a.writeError(w, r, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
		created = breakResult.CreatedBreaks
		existing = breakResult.ExistingBreaks
		casesCreated = len(breakResult.Cases)
	}

	stats := result.Stats
	if stats.BreaksBySide == nil {
		stats.BreaksBySide = map[string]int{}
	}
	if stats.ByPass == nil {
		stats.ByPass = map[string]int{}
	}
	writeJSON(w, http.StatusOK, ReconcileResult{
		RunID:          result.RunID,
		ScopeID:        result.ScopeID,
		Stats:          stats,
		Breaks:         len(result.Breaks),
		BreaksCreated:  created,
		BreaksExisting: existing,
		CasesCreated:   casesCreated,
	})
}

func validateScopeID(value *string) (string, *clientError) {
	if value == nil || *value == "" {
		return "", newClientError(http.StatusBadRequest, codeInvalidScopeID, "scope_id is required")
	}
	if !uuidPattern.MatchString(*value) {
		return "", newClientError(http.StatusBadRequest, codeInvalidScopeID, "scope_id must be a UUID")
	}
	return *value, nil
}

func validateDateField(name string, value *string) (time.Time, *clientError) {
	if value == nil || *value == "" {
		return time.Time{}, newClientError(http.StatusBadRequest, codeInvalidDateRange,
			name+" is required as YYYY-MM-DD")
	}
	parsed, ok := parseISODate(*value)
	if !ok {
		return time.Time{}, newClientError(http.StatusBadRequest, codeInvalidDateRange,
			name+" must be a YYYY-MM-DD date")
	}
	return parsed, nil
}

// parseISODate accepts exactly a zero-padded YYYY-MM-DD calendar date.
func parseISODate(value string) (time.Time, bool) {
	if len(value) != 10 || value[4] != '-' || value[7] != '-' {
		return time.Time{}, false
	}
	for i := 0; i < len(value); i++ {
		if i == 4 || i == 7 {
			continue
		}
		if value[i] < '0' || value[i] > '9' {
			return time.Time{}, false
		}
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}
