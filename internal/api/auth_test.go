package api

import (
	"net/http"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

const testCaseID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
		ok     bool
	}{
		{"valid", []string{"Bearer abc"}, "abc", true},
		{"case insensitive scheme", []string{"bearer abc"}, "abc", true},
		{"missing", nil, "", false},
		{"two headers", []string{"Bearer a", "Bearer b"}, "", false},
		{"no scheme", []string{"abc"}, "", false},
		{"empty token", []string{"Bearer "}, "", false},
		{"extra space", []string{"Bearer  abc"}, "", false},
		{"trailing space", []string{"Bearer abc "}, "", false},
		{"comma", []string{"Bearer a,b"}, "", false},
		{"basic", []string{"Basic abc"}, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := bearerToken(test.values)
			if ok != test.ok || got != test.want {
				t.Fatalf("bearerToken(%v) = %q, %v; want %q, %v", test.values, got, ok, test.want, test.ok)
			}
		})
	}
}

type protectedCase struct {
	method      string
	path        string
	body        string
	contentType string
	required    config.Role
}

func protectedCases(t *testing.T) []protectedCase {
	t.Helper()
	ingestBody, ingestType := multipartBody(t, formPart{name: "source", value: "psp_a"}, formPart{name: "file", filename: "psp.csv", value: "data"})
	return []protectedCase{
		{http.MethodPost, RouteIngest, ingestBody, ingestType, config.RoleAdmin},
		{http.MethodPost, RouteReconcile, `{"scope_id":"` + testCaseID + `","from":"2026-11-01","to":"2026-11-07"}`, "application/json", config.RoleAdmin},
		{http.MethodGet, RouteCases, "", "", config.RoleViewer},
		{http.MethodGet, "/v1/cases/" + testCaseID, "", "", config.RoleViewer},
		{http.MethodGet, RouteProposals, "", "", config.RoleViewer},
		{http.MethodPost, RouteSwitches, `{"name":"agent_enabled","enabled":false,"reason":"pause"}`, "application/json", config.RoleAdmin},
	}
}

func TestRoleMatrixAllowsAndDenies(t *testing.T) {
	actors := []struct {
		name   string
		role   config.Role
		secret string
	}{
		{"viewer", config.RoleViewer, testViewerKey},
		{"approver", config.RoleApprover, testApproverKey},
		{"admin", config.RoleAdmin, testAdminKey},
	}
	for _, route := range protectedCases(t) {
		for _, actor := range actors {
			t.Run(route.method+" "+route.path+"/"+actor.name, func(t *testing.T) {
				h := newHarness(t, nil)
				want := http.StatusOK
				if !actor.role.Allows(route.required) {
					want = http.StatusForbidden
				}
				response := h.request(route.method, route.path, route.body, route.contentType, actor.secret, nil)
				defer func() { _ = response.Body.Close() }()
				if response.StatusCode != want {
					t.Fatalf("status = %d, want %d", response.StatusCode, want)
				}
				if want == http.StatusForbidden {
					assertErrorCode(t, response, codeForbidden)
				}
			})
		}
	}
}

func TestAuthFailuresOnEveryProtectedRoute(t *testing.T) {
	authFailures := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"missing", nil},
		{"malformed scheme", func(r *http.Request) { r.Header.Set("Authorization", "Token abc") }},
		{"empty token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }},
		{"two headers", func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer "+testAdminKey)
			r.Header.Add("Authorization", "Bearer "+testViewerKey)
		}},
		{"unknown key", func(r *http.Request) { r.Header.Set("Authorization", "Bearer unknown-key-0123456789abcdef") }},
	}
	for _, route := range protectedCases(t) {
		for _, failure := range authFailures {
			t.Run(route.method+" "+route.path+"/"+failure.name, func(t *testing.T) {
				h := newHarness(t, nil)
				response := h.request(route.method, route.path, route.body, route.contentType, "", failure.mutate)
				defer func() { _ = response.Body.Close() }()
				if response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", response.StatusCode)
				}
				if got := response.Header.Get("WWW-Authenticate"); got == "" {
					t.Fatal("401 response is missing WWW-Authenticate")
				}
				assertErrorCode(t, response, codeUnauthenticated)
			})
		}
	}
}

func TestRoleCheckHappensBeforeBodyParsing(t *testing.T) {
	h := newHarness(t, nil)
	// A viewer on an admin route with a malformed body must get 403, not 400.
	response := h.expectStatus(http.MethodPost, RouteSwitches, `{not json`, "application/json", testViewerKey, http.StatusForbidden)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeForbidden)
	if h.switches.calls != 0 {
		t.Fatal("switch service was called for a forbidden request")
	}
}

func TestPublicRoutesIgnoreInvalidAuthorization(t *testing.T) {
	for _, path := range []string{RouteHealthz, RouteMetrics} {
		for _, header := range []string{"", "Bearer " + testAdminKey, "Basic nope"} {
			t.Run(path+"/"+header, func(t *testing.T) {
				h := newHarness(t, nil)
				response := h.request(http.MethodGet, path, "", "", "", func(r *http.Request) {
					if header != "" {
						r.Header.Set("Authorization", header)
					}
				})
				defer func() { _ = response.Body.Close() }()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", response.StatusCode)
				}
			})
		}
	}
}

func TestAuthenticatedViewerGets404ForMissingCase(t *testing.T) {
	h := newHarness(t, nil)
	h.caseFile.err = cases.ErrCaseNotFound
	response := h.expectStatus(http.MethodGet, "/v1/cases/"+testCaseID, "", "", testViewerKey, http.StatusNotFound)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeNotFound)
}
