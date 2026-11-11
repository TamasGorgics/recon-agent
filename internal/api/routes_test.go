package api

import (
	"net/http"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

func TestRouteTable(t *testing.T) {
	h := newHarness(t, nil)
	want := []Route{
		{Method: http.MethodGet, Pattern: RouteHealthz, Role: ""},
		{Method: http.MethodGet, Pattern: RouteMetrics, Role: ""},
		{Method: http.MethodPost, Pattern: RouteIngest, Role: config.RoleAdmin},
		{Method: http.MethodPost, Pattern: RouteReconcile, Role: config.RoleAdmin},
		{Method: http.MethodGet, Pattern: RouteCases, Role: config.RoleViewer},
		{Method: http.MethodGet, Pattern: RouteCaseFile, Role: config.RoleViewer},
		{Method: http.MethodGet, Pattern: RouteProposals, Role: config.RoleViewer},
		{Method: http.MethodPost, Pattern: RouteSwitches, Role: config.RoleAdmin},
	}
	got := h.api.routeTable()
	if len(got) != len(want) {
		t.Fatalf("route count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Method != want[i].Method || got[i].Pattern != want[i].Pattern || got[i].Role != want[i].Role {
			t.Fatalf("route %d = %s %s %q, want %s %s %q",
				i, got[i].Method, got[i].Pattern, got[i].Role,
				want[i].Method, want[i].Pattern, want[i].Role)
		}
		if got[i].Handler == nil {
			t.Fatalf("route %s %s has a nil handler", got[i].Method, got[i].Pattern)
		}
	}
}

func TestNotFoundEnvelope(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, "/v1/does-not-exist", "", "", testAdminKey, http.StatusNotFound)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeNotFound)
}

func TestMethodNotAllowedEnvelopeAndAllow(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodPost, RouteCases, `{}`, "application/json", testAdminKey, http.StatusMethodNotAllowed)
	defer func() { _ = response.Body.Close() }()
	if got := response.Header.Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", got)
	}
	assertErrorCode(t, response, codeMethodNotAllowed)
}

func TestAllowedMethodsForParameterizedPath(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodPost, "/v1/cases/"+testCaseID, `{}`, "application/json", testAdminKey, http.StatusMethodNotAllowed)
	defer func() { _ = response.Body.Close() }()
	if got := response.Header.Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", got)
	}
}

func TestHealthzAndMetricsBodies(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, RouteHealthz, "", "", "", http.StatusOK)
	if got := response.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("healthz content type = %q", got)
	}
	if body := bodyOf(t, response); body != "{\"status\":\"ok\"}\n" {
		t.Fatalf("healthz body = %q", body)
	}

	response = h.expectStatus(http.MethodGet, RouteMetrics, "", "", "", http.StatusOK)
	if got := response.Header.Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics content type = %q", got)
	}
	if body := bodyOf(t, response); body != metricsBody {
		t.Fatalf("metrics body = %q, want the static stub", body)
	}
}
