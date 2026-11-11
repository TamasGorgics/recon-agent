package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

const testScopeID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

func TestReconcileValidRequest(t *testing.T) {
	h := newHarness(t, nil)
	h.matcher.result = match.RunResult{
		RunID:   "run-1",
		ScopeID: testScopeID,
		Stats: match.RunStats{
			ExternalCandidates: 2,
			LedgerCandidates:   2,
			Matched:            1,
			UnmatchedExternal:  1,
			UnmatchedLedger:    0,
			Breaks:             1,
			BreaksBySide:       map[string]int{"ledger_only": 1},
			ByPass:             map[string]int{"exact": 1},
		},
		Breaks: []match.BreakCandidate{{ScopeID: testScopeID, Side: match.SideLedgerOnly}},
	}
	h.cases.result = cases.CreateBreaksResult{
		CreatedBreaks:  1,
		ExistingBreaks: 0,
		Cases:          []cases.Case{{ID: testCaseID}},
	}
	body := `{"scope_id":"` + testScopeID + `","from":"2026-11-01","to":"2026-11-07"}`
	response := h.expectStatus(http.MethodPost, RouteReconcile, body, "application/json", testAdminKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()

	var result ReconcileResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.RunID != "run-1" || result.ScopeID != testScopeID || result.Breaks != 1 ||
		result.BreaksCreated != 1 || result.BreaksExisting != 0 || result.CasesCreated != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Stats.ExternalCandidates != 2 {
		t.Fatalf("stats = %+v", result.Stats)
	}
	if h.matcher.calls != 1 {
		t.Fatalf("matcher calls = %d, want 1", h.matcher.calls)
	}
	wantFrom := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	if !h.matcher.got.Window.From.Equal(wantFrom) || !h.matcher.got.Window.To.Equal(wantTo) {
		t.Fatalf("window = %+v, want %v..%v", h.matcher.got.Window, wantFrom, wantTo)
	}
	if h.cases.calls != 1 {
		t.Fatalf("CreateBreaks calls = %d, want 1", h.cases.calls)
	}
	if h.cases.got.MatchRunID != "run-1" || h.cases.got.ScopeID != testScopeID || len(h.cases.got.Breaks) != 1 {
		t.Fatalf("CreateBreaks request = %+v", h.cases.got)
	}
}

func TestReconcileZeroBreaksSkipsCreateBreaks(t *testing.T) {
	h := newHarness(t, nil)
	h.matcher.result = match.RunResult{RunID: "run-2", ScopeID: testScopeID}
	body := `{"scope_id":"` + testScopeID + `","from":"2026-11-01","to":"2026-11-07"}`
	response := h.expectStatus(http.MethodPost, RouteReconcile, body, "application/json", testAdminKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var result ReconcileResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Breaks != 0 || result.BreaksCreated != 0 || result.CasesCreated != 0 {
		t.Fatalf("result = %+v", result)
	}
	if h.cases.calls != 0 {
		t.Fatal("CreateBreaks was called with zero breaks")
	}
	if result.Stats.BreaksBySide == nil || result.Stats.ByPass == nil {
		t.Fatal("stats maps should marshal as {}, not null")
	}
}

func TestReconcileValidationFailures(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"missing scope", `{"from":"2026-11-01","to":"2026-11-07"}`, codeInvalidScopeID},
		{"bad scope", `{"scope_id":"not-a-uuid","from":"2026-11-01","to":"2026-11-07"}`, codeInvalidScopeID},
		{"missing from", `{"scope_id":"` + testScopeID + `","to":"2026-11-07"}`, codeInvalidDateRange},
		{"bad from", `{"scope_id":"` + testScopeID + `","from":"2026-1-1","to":"2026-11-07"}`, codeInvalidDateRange},
		{"impossible date", `{"scope_id":"` + testScopeID + `","from":"2026-02-30","to":"2026-11-07"}`, codeInvalidDateRange},
		{"bad to", `{"scope_id":"` + testScopeID + `","from":"2026-11-01","to":"07/11/2026"}`, codeInvalidDateRange},
		{"inverted", `{"scope_id":"` + testScopeID + `","from":"2026-11-08","to":"2026-11-07"}`, codeInvalidDateRange},
		{"unknown field", `{"scope_id":"` + testScopeID + `","from":"2026-11-01","to":"2026-11-07","dry_run":true}`, codeInvalidJSON},
		{"trailing value", `{"scope_id":"` + testScopeID + `","from":"2026-11-01","to":"2026-11-07"} {}`, codeInvalidJSON},
		{"duplicate field", `{"scope_id":"` + testScopeID + `","scope_id":"` + testScopeID + `","from":"2026-11-01","to":"2026-11-07"}`, codeInvalidJSON},
		{"empty body", ``, codeInvalidJSON},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.expectStatus(http.MethodPost, RouteReconcile, test.body, "application/json", testAdminKey, http.StatusBadRequest)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, test.wantCode)
			if h.matcher.calls != 0 {
				t.Fatal("matcher was called for an invalid request")
			}
		})
	}
}

func TestReconcileContentTypeRequired(t *testing.T) {
	h := newHarness(t, nil)
	body := `{"scope_id":"` + testScopeID + `","from":"2026-11-01","to":"2026-11-07"}`
	response := h.expectStatus(http.MethodPost, RouteReconcile, body, "text/plain", testAdminKey, http.StatusUnsupportedMediaType)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeUnsupportedMedia)
}

func TestReconcileScopeNotFound(t *testing.T) {
	h := newHarness(t, nil)
	h.matcher.err = match.ErrScopeNotFound
	body := `{"scope_id":"` + testScopeID + `","from":"2026-11-01","to":"2026-11-07"}`
	response := h.expectStatus(http.MethodPost, RouteReconcile, body, "application/json", testAdminKey, http.StatusNotFound)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeNotFound)
}

func TestReconcileBreakCreationFailureIsGeneric500(t *testing.T) {
	h := newHarness(t, nil)
	h.matcher.result = match.RunResult{
		RunID:   "run-after-matcher",
		ScopeID: testScopeID,
		Breaks:  []match.BreakCandidate{{ScopeID: testScopeID, Side: match.SideLedgerOnly}},
	}
	h.cases.err = errInternalForTest{}
	body := `{"scope_id":"` + testScopeID + `","from":"2026-11-01","to":"2026-11-07"}`
	response := h.expectStatus(http.MethodPost, RouteReconcile, body, "application/json", testAdminKey, http.StatusInternalServerError)
	bodyText := bodyOf(t, response)
	if strings.Contains(bodyText, "secret-detail") || strings.Contains(bodyText, "run-after-matcher") {
		t.Fatalf("partial state or cause leaked: %s", bodyText)
	}
	assertErrorCodeFromText(t, bodyText, codeInternal)
	if !strings.Contains(h.logs.String(), "run-after-matcher") {
		t.Fatal("server log should include the committed matcher run ID")
	}
}

func assertErrorCodeFromText(t *testing.T, body, want string) {
	t.Helper()
	var envelope errorEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Error.Code != want {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, want)
	}
}
