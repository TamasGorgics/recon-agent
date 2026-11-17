package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval"
)

const testDecisionProposalID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

func TestProposalDecisionForwardsPrincipalAndPath(t *testing.T) {
	h := newHarness(t, nil)
	decidedAt := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	h.decisions.result = approval.DecisionResult{
		ProposalID: testDecisionProposalID,
		Status:     "approved",
		Decision:   approval.DecisionApproved,
		ApproverID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		DecidedAt:  decidedAt,
		Replayed:   false,
	}
	body := `{"decision":"approved","reason":"  checked evidence  "}`
	response := h.request(http.MethodPost, "/v1/proposals/"+testDecisionProposalID+"/decision",
		body, "application/json", testApproverKey, nil)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if h.decisions.calls != 1 {
		t.Fatalf("decision service calls = %d, want 1", h.decisions.calls)
	}
	if h.decisions.gotProposalID != testDecisionProposalID {
		t.Fatalf("proposal id = %q, want %q", h.decisions.gotProposalID, testDecisionProposalID)
	}
	if h.decisions.gotDecision != approval.DecisionApproved {
		t.Fatalf("decision = %q, want approved", h.decisions.gotDecision)
	}
	if h.decisions.gotAPIKeyID != "dev-approver" {
		t.Fatalf("api key id = %q, want dev-approver", h.decisions.gotAPIKeyID)
	}
	if h.decisions.gotSource != "api" {
		t.Fatalf("source = %q, want api", h.decisions.gotSource)
	}

	var result ProposalDecisionResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ProposalID != testDecisionProposalID || result.Status != "approved" ||
		result.Decision != "approved" || result.ApproverID != "cccccccc-cccc-4ccc-8ccc-cccccccccccc" ||
		!result.DecidedAt.Equal(decidedAt) || result.Replayed {
		t.Fatalf("result = %+v", result)
	}
}

func TestProposalDecisionAcceptsOptionalReason(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodPost, "/v1/proposals/"+testDecisionProposalID+"/decision",
		`{"decision":"rejected"}`, "application/json", testApproverKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	if h.decisions.calls != 1 || h.decisions.gotDecision != approval.DecisionRejected {
		t.Fatalf("decision call = %d/%q", h.decisions.calls, h.decisions.gotDecision)
	}
}

func TestProposalDecisionInvalidRequests(t *testing.T) {
	longReason := strings.Repeat("x", maxDecisionReasonRunes+1)
	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"malformed json", `{`, "application/json", http.StatusBadRequest, codeInvalidJSON},
		{"missing decision", `{}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"null decision", `{"decision":null}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"non-string decision", `{"decision":1}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"unknown decision", `{"decision":"maybe"}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"uppercase decision", `{"decision":"APPROVED"}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"non-string reason", `{"decision":"approved","reason":5}`, "application/json", http.StatusBadRequest, codeInvalidJSON},
		{"oversized reason", `{"decision":"approved","reason":"` + longReason + `"}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"unknown field", `{"decision":"approved","actor":"me"}`, "application/json", http.StatusBadRequest, codeInvalidJSON},
		{"wrong content type", `{"decision":"approved"}`, "text/plain", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		{"empty body", ``, "application/json", http.StatusBadRequest, codeInvalidJSON},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.expectStatus(http.MethodPost, "/v1/proposals/"+testDecisionProposalID+"/decision",
				test.body, test.contentType, testApproverKey, test.wantStatus)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, test.wantCode)
			if h.decisions.calls != 0 {
				t.Fatal("decision service was called for an invalid request")
			}
		})
	}
}

func TestProposalDecisionErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"invalid decision", approval.ErrInvalidDecision, http.StatusBadRequest, codeInvalidRequest},
		{"invalid request", approval.ErrInvalidRequest, http.StatusBadRequest, codeInvalidRequest},
		{"approver not found", approval.ErrApproverNotFound, http.StatusForbidden, codeForbidden},
		{"approver role", approval.ErrApproverRole, http.StatusForbidden, codeForbidden},
		{"approver inactive", approval.ErrApproverInactive, http.StatusForbidden, codeForbidden},
		{"agent actor", approval.ErrAgentActor, http.StatusForbidden, codeForbidden},
		{"proposal not found", approval.ErrProposalNotFound, http.StatusNotFound, codeNotFound},
		{"not pending", approval.ErrNotPending, http.StatusConflict, codeConflict},
		{"expired", approval.ErrExpired, http.StatusConflict, codeConflict},
		{"decision conflict", approval.ErrDecisionConflict, http.StatusConflict, codeConflict},
		{"co-maker", approval.ErrCoMaker, http.StatusConflict, codeConflict},
		{"internal", errInternalForTest{}, http.StatusInternalServerError, codeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.decisions.err = test.err
			response := h.expectStatus(http.MethodPost, "/v1/proposals/"+testDecisionProposalID+"/decision",
				`{"decision":"approved"}`, "application/json", testApproverKey, test.wantStatus)
			body := bodyOf(t, response)
			assertErrorCodeFromText(t, body, test.wantCode)
			if strings.Contains(body, "secret-detail") {
				t.Fatalf("service error leaked: %s", body)
			}
		})
	}
}

func TestProposalDecisionViewerForbiddenBeforeBodyParsing(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodPost, "/v1/proposals/"+testDecisionProposalID+"/decision",
		`{not json`, "application/json", testViewerKey, http.StatusForbidden)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeForbidden)
	if h.decisions.calls != 0 {
		t.Fatal("decision service was called for a forbidden request")
	}
}
