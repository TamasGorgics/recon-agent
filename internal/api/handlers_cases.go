package api

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// caseStatuses is the complete case.status CHECK set.
var caseStatuses = map[string]struct{}{
	"open":             {},
	"in_agent":         {},
	"proposal_pending": {},
	"waiting_info":     {},
	"escalated":        {},
	"resolved":         {},
}

// handleListCases implements GET /v1/cases.
func (a *API) handleListCases(w http.ResponseWriter, r *http.Request) {
	values, clientErr := parseQuery(r)
	if clientErr != nil {
		a.writeClientError(w, r, clientErr)
		return
	}
	if _, present := values["type"]; present {
		a.writeError(w, r, http.StatusBadRequest, codeUnsupportedFilter,
			"the type filter is not supported")
		return
	}
	for key := range values {
		switch key {
		case "status", "limit", "cursor":
		default:
			a.writeClientError(w, r, invalidQueryError())
			return
		}
	}

	query := CaseListQuery{Limit: defaultListLimit}
	if raw, present := values["status"]; present {
		status, ok := singleQueryValue(raw)
		if !ok {
			a.writeClientError(w, r, invalidQueryError())
			return
		}
		if _, known := caseStatuses[status]; !known {
			a.writeClientError(w, r, invalidQueryError())
			return
		}
		query.Status = status
	}
	limit, cursor, clientErr := parsePageParams(values)
	if clientErr != nil {
		a.writeClientError(w, r, clientErr)
		return
	}
	query.Limit = limit
	query.Cursor = cursor

	page, err := a.caseList.ListCases(r.Context(), query)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	items := page.Items
	if items == nil {
		items = make([]CaseSummary, 0)
	}
	for i := range items {
		if items[i].BreakIDs == nil {
			items[i].BreakIDs = []string{}
		}
	}
	writeJSON(w, http.StatusOK, CaseList{Items: items, NextCursor: page.NextCursor})
}

// handleCaseFile implements GET /v1/cases/{id}.
func (a *API) handleCaseFile(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "id")
	if !uuidPattern.MatchString(caseID) {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"case id must be a UUID"))
		return
	}
	document, err := a.caseFile.GetCaseFile(r.Context(), caseID)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	document.SchemaVersion = 1
	document.Case.BreakIDs = nonNilStrings(document.Case.BreakIDs)
	document.Breaks = nonNilBreaks(document.Breaks)
	document.Proposals = nonNilProposals(document.Proposals)
	document.AgentRuns = nonNilAgentRuns(document.AgentRuns)
	writeJSON(w, http.StatusOK, document)
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilBreaks(values []Break) []Break {
	if values == nil {
		return []Break{}
	}
	return values
}

func nonNilProposals(values []ProposalDetail) []ProposalDetail {
	if values == nil {
		return []ProposalDetail{}
	}
	return values
}

func nonNilAgentRuns(values []AgentRun) []AgentRun {
	if values == nil {
		return []AgentRun{}
	}
	return values
}

func parseQuery(r *http.Request) (url.Values, *clientError) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, invalidQueryError()
	}
	return values, nil
}

func singleQueryValue(values []string) (string, bool) {
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

func invalidQueryError() *clientError {
	return newClientError(http.StatusBadRequest, codeInvalidQuery,
		"one or more query parameters are invalid or not supported")
}

// parsePageParams validates the shared limit/cursor parameters.
func parsePageParams(values url.Values) (int, *listCursor, *clientError) {
	limit, err := parseListLimit(values["limit"])
	if err != nil {
		return 0, nil, invalidQueryError()
	}
	cursorValues, present := values["cursor"]
	if !present {
		return limit, nil, nil
	}
	raw, ok := singleQueryValue(cursorValues)
	if !ok {
		return 0, nil, invalidQueryError()
	}
	cursor, err := decodeCursor(raw)
	if err != nil {
		return 0, nil, invalidQueryError()
	}
	return limit, &cursor, nil
}
