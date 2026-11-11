package api

import "net/http"

// proposalStatuses is the complete proposals.status CHECK set.
var proposalStatuses = map[string]struct{}{
	"proposed":         {},
	"pending_approval": {},
	"approved":         {},
	"auto_approved":    {},
	"applied":          {},
	"apply_failed":     {},
	"rejected":         {},
	"expired":          {},
	"superseded":       {},
}

// handleListProposals implements GET /v1/proposals. It returns summary fields
// only; payload/evidence/rationale never cross this endpoint.
func (a *API) handleListProposals(w http.ResponseWriter, r *http.Request) {
	values, clientErr := parseQuery(r)
	if clientErr != nil {
		a.writeClientError(w, r, clientErr)
		return
	}
	for key := range values {
		switch key {
		case "status", "case_id", "limit", "cursor":
		default:
			a.writeClientError(w, r, invalidQueryError())
			return
		}
	}

	query := ProposalListQuery{Limit: defaultListLimit}
	if raw, present := values["status"]; present {
		status, ok := singleQueryValue(raw)
		if !ok {
			a.writeClientError(w, r, invalidQueryError())
			return
		}
		if status == "pending" {
			status = "pending_approval"
		}
		if _, known := proposalStatuses[status]; !known {
			a.writeClientError(w, r, invalidQueryError())
			return
		}
		query.Status = status
	}
	if raw, present := values["case_id"]; present {
		caseID, ok := singleQueryValue(raw)
		if !ok || !uuidPattern.MatchString(caseID) {
			a.writeClientError(w, r, invalidQueryError())
			return
		}
		query.CaseID = caseID
	}
	limit, cursor, clientErr := parsePageParams(values)
	if clientErr != nil {
		a.writeClientError(w, r, clientErr)
		return
	}
	query.Limit = limit
	query.Cursor = cursor

	page, err := a.proposals.ListProposals(r.Context(), query)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	items := page.Items
	if items == nil {
		items = make([]ProposalSummary, 0)
	}
	writeJSON(w, http.StatusOK, ProposalList{Items: items, NextCursor: page.NextCursor})
}
