package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval"
)

// decisionRequestJSON uses RawMessage for decision so a missing, null, or
// non-string value is reported as invalid_request rather than invalid_json.
// Reason is optional and reserved: the frozen approval.Decide contract has no
// reason parameter yet, so it is validated but not persisted.
type decisionRequestJSON struct {
	Decision json.RawMessage `json:"decision"`
	Reason   *string         `json:"reason"`
}

const (
	// decisionSourceAPI is the approvals.channel and audit source for a
	// decision made through the HTTP API.
	decisionSourceAPI = "api"
	// maxDecisionReasonRunes matches the state machine's audit-reason bound.
	// The value is validated here even though M4-11 does not forward it.
	maxDecisionReasonRunes = 2000
)

// handleProposalDecision implements POST /v1/proposals/{id}/decision. The
// authenticated API key is mapped to an approvers row by the approval service;
// the key value itself never leaves the authentication middleware.
func (a *API) handleProposalDecision(w http.ResponseWriter, r *http.Request) {
	var request decisionRequestJSON
	if err := decodeJSONBody(w, r, &request, a.cfg.MaxJSONBytes); err != nil {
		a.writeClientError(w, r, err)
		return
	}

	decision, ok := parseDecision(request.Decision)
	if !ok {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"decision must be approved or rejected"))
		return
	}
	reason := ""
	if request.Reason != nil {
		reason = strings.TrimSpace(*request.Reason)
	}
	if utf8.RuneCountInString(reason) > maxDecisionReasonRunes {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"reason must be at most 2000 characters"))
		return
	}

	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		a.unauthorized(w, r)
		return
	}
	result, err := a.decisions.DecideByAPIKey(r.Context(), chi.URLParam(r, "id"), decision, principal.KeyID, decisionSourceAPI)
	if err != nil {
		a.writeDecisionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ProposalDecisionResult{
		ProposalID: result.ProposalID,
		Status:     result.Status,
		Decision:   string(result.Decision),
		ApproverID: result.ApproverID,
		DecidedAt:  result.DecidedAt,
		Replayed:   result.Replayed,
	})
}

// parseDecision accepts exactly one bare JSON string equal to an approval
// decision. A number, object, array, null, or any other spelling is invalid.
func parseDecision(raw json.RawMessage) (approval.Decision, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	decision := approval.Decision(value)
	if !decision.Valid() {
		return "", false
	}
	return decision, true
}

// writeDecisionError maps approval-service sentinels onto the public envelope
// and logs only unexpected server-side causes.
func (a *API) writeDecisionError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := mapDecisionError(err)
	if status >= http.StatusInternalServerError {
		a.logError(r, "proposal decision failed", err)
	}
	a.writeError(w, r, status, code, message)
}

func mapDecisionError(err error) (int, string, string) {
	switch {
	case errors.Is(err, approval.ErrInvalidDecision),
		errors.Is(err, approval.ErrInvalidRequest):
		return http.StatusBadRequest, codeInvalidRequest, "the decision request is invalid"
	case errors.Is(err, approval.ErrApproverNotFound),
		errors.Is(err, approval.ErrApproverRole),
		errors.Is(err, approval.ErrApproverInactive),
		errors.Is(err, approval.ErrAgentActor):
		return http.StatusForbidden, codeForbidden, messageForbidden
	case errors.Is(err, approval.ErrProposalNotFound):
		return http.StatusNotFound, codeNotFound, messageNotFound
	case errors.Is(err, approval.ErrNotPending),
		errors.Is(err, approval.ErrExpired),
		errors.Is(err, approval.ErrDecisionConflict),
		errors.Is(err, approval.ErrCoMaker):
		return http.StatusConflict, codeConflict, "the proposal cannot accept this decision"
	default:
		return http.StatusInternalServerError, codeInternal, messageInternal
	}
}
