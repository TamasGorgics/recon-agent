package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
)

func TestListCasesDefaultsAndFilters(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, RouteCases, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var list CaseList
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Items == nil {
		t.Fatal("items must be [] not null")
	}
	if h.caseList.got.Limit != defaultListLimit || h.caseList.got.Status != "" || h.caseList.got.Cursor != nil {
		t.Fatalf("query = %+v", h.caseList.got)
	}

	h = newHarness(t, nil)
	h.caseList.page = CaseListPage{
		Items:      []CaseSummary{{ID: testCaseID, BreakIDs: nil}},
		NextCursor: "next",
	}
	response = h.expectStatus(http.MethodGet, RouteCases+"?status=open&limit=2", "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.caseList.got.Status != "open" || h.caseList.got.Limit != 2 {
		t.Fatalf("query = %+v", h.caseList.got)
	}
	if list.NextCursor != "next" || len(list.Items) != 1 || list.Items[0].BreakIDs == nil {
		t.Fatalf("list = %+v", list)
	}
}

func TestListCasesCursorRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 11, 10, 0, 0, 123000000, time.UTC)
	cursor := encodeCursor(created, testCaseID)
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, RouteCases+"?cursor="+cursor, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if h.caseList.got.Cursor == nil || !h.caseList.got.Cursor.CreatedAt.Equal(created) || h.caseList.got.Cursor.ID != testCaseID {
		t.Fatalf("cursor = %+v", h.caseList.got.Cursor)
	}
}

func TestListCasesInvalidQuery(t *testing.T) {
	tests := []struct {
		query    string
		wantCode string
	}{
		{"?status=weird", codeInvalidQuery},
		{"?status=", codeInvalidQuery},
		{"?limit=0", codeInvalidQuery},
		{"?limit=201", codeInvalidQuery},
		{"?limit=abc", codeInvalidQuery},
		{"?limit=", codeInvalidQuery},
		{"?cursor=not-base64", codeInvalidQuery},
		{"?cursor=", codeInvalidQuery},
		{"?unknown=1", codeInvalidQuery},
		{"?type=adjustment", codeUnsupportedFilter},
		{"?type=", codeUnsupportedFilter},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.expectStatus(http.MethodGet, RouteCases+test.query, "", "", testViewerKey, http.StatusBadRequest)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, test.wantCode)
			if h.caseList.calls != 0 {
				t.Fatal("store was queried for an invalid query")
			}
		})
	}
}

func TestCaseFileShapeAndNormalization(t *testing.T) {
	h := newHarness(t, nil)
	h.caseFile.document = CaseFile{
		Case: CaseFileCase{ID: testCaseID, BreakIDs: nil},
	}
	response := h.expectStatus(http.MethodGet, "/v1/cases/"+testCaseID, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var document CaseFile
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if document.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", document.SchemaVersion)
	}
	if document.Breaks == nil || document.Proposals == nil || document.AgentRuns == nil || document.Case.BreakIDs == nil {
		t.Fatalf("empty sections must be [] not null: %+v", document)
	}
	if h.caseFile.got != testCaseID {
		t.Fatalf("case id = %q", h.caseFile.got)
	}
}

func TestCaseFileRequestBody(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, "/v1/cases/"+testCaseID, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"schema_version", "case", "breaks", "proposals", "agent_runs"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response missing %q", key)
		}
	}
}

func TestCaseFileInvalidAndMissing(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, "/v1/cases/not-a-uuid", "", "", testViewerKey, http.StatusBadRequest)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeInvalidRequest)

	h = newHarness(t, nil)
	h.caseFile.err = cases.ErrCaseNotFound
	response = h.expectStatus(http.MethodGet, "/v1/cases/"+testCaseID, "", "", testViewerKey, http.StatusNotFound)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeNotFound)
}

func TestListProposalsFiltersAndAlias(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, RouteProposals+"?status=pending&case_id="+testCaseID+"&limit=5", "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if h.proposals.got.Status != "pending_approval" {
		t.Fatalf("status = %q, want pending_approval", h.proposals.got.Status)
	}
	if h.proposals.got.CaseID != testCaseID || h.proposals.got.Limit != 5 {
		t.Fatalf("query = %+v", h.proposals.got)
	}

	var list ProposalList
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Items == nil {
		t.Fatal("items must be [] not null")
	}
}

func TestListProposalsSummaryOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.proposals.page = ProposalListPage{Items: []ProposalSummary{{
		ID:         "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		CaseID:     testCaseID,
		Type:       "adjustment",
		Status:     "pending_approval",
		Confidence: 0.87,
	}}}
	response := h.expectStatus(http.MethodGet, RouteProposals, "", "", testViewerKey, http.StatusOK)
	body := bodyOf(t, response)
	for _, forbidden := range []string{"payload", "evidence", "rationale", "policy_decision", "verification"} {
		if strings.Contains(body, `"`+forbidden+`"`) {
			t.Fatalf("proposal list leaked %q: %s", forbidden, body)
		}
	}
}

func TestListProposalsInvalidQuery(t *testing.T) {
	tests := []string{
		"?status=weird",
		"?status=",
		"?case_id=not-a-uuid",
		"?limit=0",
		"?cursor=bad",
		"?type=adjustment",
	}
	for _, query := range tests {
		t.Run(query, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.expectStatus(http.MethodGet, RouteProposals+query, "", "", testViewerKey, http.StatusBadRequest)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, codeInvalidQuery)
			if h.proposals.calls != 0 {
				t.Fatal("store was queried for an invalid query")
			}
		})
	}
}
