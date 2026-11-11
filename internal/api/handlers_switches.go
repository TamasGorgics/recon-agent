package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// switchRequestJSON uses RawMessage for enabled so a missing, null, or
// non-boolean value is reported as invalid_request rather than invalid_json.
type switchRequestJSON struct {
	Name    *string         `json:"name"`
	Enabled json.RawMessage `json:"enabled"`
	Reason  *string         `json:"reason"`
}

// maxSwitchReasonRunes caps the operator-supplied switch reason.
const maxSwitchReasonRunes = 500

// handleSetSwitch implements POST /v1/admin/switches.
func (a *API) handleSetSwitch(w http.ResponseWriter, r *http.Request) {
	var request switchRequestJSON
	if err := decodeJSONBody(w, r, &request, a.cfg.MaxJSONBytes); err != nil {
		a.writeClientError(w, r, err)
		return
	}

	name := ""
	if request.Name != nil {
		name = strings.TrimSpace(*request.Name)
	}
	if name != SwitchAgent && name != SwitchAutoApply {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"name must be agent_enabled or auto_apply_enabled"))
		return
	}
	enabled, ok := parseJSONBool(request.Enabled)
	if !ok {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"enabled is required as a JSON boolean"))
		return
	}
	reason := ""
	if request.Reason != nil {
		reason = strings.TrimSpace(*request.Reason)
	}
	if reason == "" {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"reason is required"))
		return
	}
	if utf8.RuneCountInString(reason) > maxSwitchReasonRunes {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"reason must be at most 500 characters"))
		return
	}

	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		a.unauthorized(w, r)
		return
	}
	result, err := a.switches.SetSwitch(r.Context(), SwitchSetRequest{
		Name:    name,
		Enabled: enabled,
		Reason:  reason,
		ActorID: principal.KeyID,
		TraceID: telemetry.TraceID(r.Context()),
	})
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// parseJSONBool accepts exactly a JSON true or false literal.
func parseJSONBool(raw json.RawMessage) (bool, bool) {
	switch strings.TrimSpace(string(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}
