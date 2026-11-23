package api

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/metrics"
)

// frozenFamilies is the spec §4 contract that every process exposes while idle.
var frozenFamilies = []string{
	metrics.MetricBuildInfo,
	metrics.MetricMatchRunsTotal,
	metrics.MetricMatchesTotal,
	metrics.MetricBreaksTotal,
	metrics.MetricCaseAgeSeconds,
	metrics.MetricQueueDepth,
	metrics.MetricAgentRunsTotal,
	metrics.MetricAgentCostUSDTotal,
	metrics.MetricApprovalLatencySeconds,
	metrics.MetricErrorsTotal,
	metrics.MetricOutboxLagSeconds,
	metrics.MetricOutboxDeadLettersTotal,
	metrics.MetricAutoApplyCapUsageRatio,
	metrics.MetricTieoutUnexplainedMinor,
	metrics.MetricIntegrityJobStatus,
}

// failingGatherer simulates a total registry failure for the 500 path.
type failingGatherer struct{}

func (failingGatherer) Gather() ([]*dto.MetricFamily, error) {
	return nil, errors.New("collector backend unavailable")
}

func TestRouteTable(t *testing.T) {
	h := newHarness(t, nil)
	want := []Route{
		{Method: http.MethodGet, Pattern: RouteHealthz, Auth: AuthPublic},
		{Method: http.MethodGet, Pattern: RouteMetrics, Auth: AuthPublic},
		{Method: http.MethodPost, Pattern: RouteIngest, Auth: AuthAPIKey, Role: config.RoleAdmin},
		{Method: http.MethodPost, Pattern: RouteReconcile, Auth: AuthAPIKey, Role: config.RoleAdmin},
		{Method: http.MethodGet, Pattern: RouteCases, Auth: AuthAPIKey, Role: config.RoleViewer},
		{Method: http.MethodGet, Pattern: RouteCaseFile, Auth: AuthAPIKey, Role: config.RoleViewer},
		{Method: http.MethodGet, Pattern: RouteProposals, Auth: AuthAPIKey, Role: config.RoleViewer},
		{Method: http.MethodPost, Pattern: RouteProposalDecision, Auth: AuthAPIKey, Role: config.RoleApprover},
		{Method: http.MethodPost, Pattern: RouteSlackInteractions, Auth: AuthSignature},
		{Method: http.MethodPost, Pattern: RouteSwitches, Auth: AuthAPIKey, Role: config.RoleAdmin},
	}
	got := h.api.routeTable()
	if len(got) != len(want) {
		t.Fatalf("route count = %d, want %d", len(got), len(want))
	}
	seen := make(map[string]struct{}, len(got))
	for i := range want {
		if got[i].Method != want[i].Method || got[i].Pattern != want[i].Pattern ||
			got[i].Role != want[i].Role || got[i].Auth != want[i].Auth {
			t.Fatalf("route %d = %s %s role=%q auth=%s, want %s %s role=%q auth=%s",
				i, got[i].Method, got[i].Pattern, got[i].Role, got[i].Auth,
				want[i].Method, want[i].Pattern, want[i].Role, want[i].Auth)
		}
		if got[i].Handler == nil {
			t.Fatalf("route %s %s has a nil handler", got[i].Method, got[i].Pattern)
		}
		key := got[i].Method + " " + got[i].Pattern
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("route %s is registered twice", key)
		}
		seen[key] = struct{}{}
		switch got[i].Auth {
		case AuthAPIKey:
			if got[i].Role == "" {
				t.Fatalf("API-key route %s %s has an empty role", got[i].Method, got[i].Pattern)
			}
		case AuthPublic, AuthSignature:
			if got[i].Role != "" {
				t.Fatalf("%s route %s %s has role %q", got[i].Auth, got[i].Method, got[i].Pattern, got[i].Role)
			}
		default:
			t.Fatalf("route %s %s has unknown auth kind %d", got[i].Method, got[i].Pattern, got[i].Auth)
		}
	}
}

func TestSignatureRouteRejectsAPICredentialsAndAcceptsSignature(t *testing.T) {
	h := newHarness(t, nil)
	for name, mutate := range map[string]func(*http.Request){
		"no credentials": func(*http.Request) {},
		"admin API key": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testAdminKey)
		},
		"approver API key": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testApproverKey)
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := h.request(http.MethodPost, RouteSlackInteractions,
				"payload=%7B%7D", "application/x-www-form-urlencoded", "", mutate)
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.StatusCode)
			}
			assertErrorCode(t, response, codeUnauthenticated)
		})
	}

	response := h.request(http.MethodPost, RouteSlackInteractions,
		"payload=%7B%7D", "application/x-www-form-urlencoded", "",
		func(r *http.Request) { r.Header.Set("X-Test-Signature", "signed") })
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("signed status = %d, want 200", response.StatusCode)
	}
}

func TestSignatureRouteMethodNotAllowed(t *testing.T) {
	h := newHarness(t, nil)
	response := h.request(http.MethodGet, RouteSlackInteractions, "", "", "",
		func(r *http.Request) { r.Header.Set("X-Test-Signature", "signed") })
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.StatusCode)
	}
	if got := response.Header.Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
}

func TestNewRequiresSignatureRouteWiring(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "missing middleware", mutate: func(opts *Options) { opts.SignatureAuth = nil }},
		{name: "missing handler", mutate: func(opts *Options) { opts.SlackInteractions = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs := &bytes.Buffer{}
			opts := defaultTestOptions(logs)
			test.mutate(&opts)
			if _, err := New(opts); err == nil {
				t.Fatal("New accepted an incomplete signature route wiring")
			}
		})
	}
}

func TestAuthKindString(t *testing.T) {
	want := map[AuthKind]string{
		AuthPublic:    "public",
		AuthAPIKey:    "api-key",
		AuthSignature: "signature",
		AuthKind(99):  "unknown",
	}
	for kind, text := range want {
		if got := kind.String(); got != text {
			t.Fatalf("AuthKind(%d).String() = %q, want %q", kind, got, text)
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
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain; version=0.0.4; charset=utf-8") {
		t.Fatalf("metrics content type = %q", got)
	}
	body := bodyOf(t, response)
	for _, name := range frozenFamilies {
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Fatalf("metrics body is missing HELP for %s:\n%s", name, body)
		}
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Fatalf("metrics body is missing TYPE for %s:\n%s", name, body)
		}
	}
	if !strings.Contains(body, `recon_build_info{component="api"} 1`) {
		t.Fatalf("metrics body is missing the api build info:\n%s", body)
	}
	for _, secret := range []string{"postgres://", "startup-secret", "xoxb-"} {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics body leaked %q:\n%s", secret, body)
		}
	}
}

func TestMetricsTotalGatherFailureReturns500(t *testing.T) {
	h := newHarness(t, nil)
	h.api.metricsHandler = metrics.Handler(failingGatherer{}, h.api.logger)

	response := h.expectStatus(http.MethodGet, RouteMetrics, "", "", "", http.StatusInternalServerError)
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("failure content type = %q, want text/plain", got)
	}
	body := bodyOf(t, response)
	if !strings.Contains(body, "metrics: gather failed") {
		t.Fatalf("failure body = %q, want the bounded gather error", body)
	}
	if strings.Contains(body, "collector backend unavailable") {
		t.Fatalf("failure body leaked the collector error: %q", body)
	}
}

func TestMetricsRegistriesAreIsolated(t *testing.T) {
	first := newHarness(t, nil)
	second := newHarness(t, nil)

	firstBody := bodyOf(t, first.expectStatus(http.MethodGet, RouteMetrics, "", "", "", http.StatusOK))
	if strings.Contains(firstBody, `code="internal"} 1`) {
		t.Fatalf("fresh registry already has a produced error:\n%s", firstBody)
	}

	first.api.metrics.ObserveError("api", "internal")
	firstBody = bodyOf(t, first.expectStatus(http.MethodGet, RouteMetrics, "", "", "", http.StatusOK))
	if !strings.Contains(firstBody, `recon_errors_total{code="internal",component="api"} 1`) {
		t.Fatalf("first registry is missing its observation:\n%s", firstBody)
	}
	secondBody := bodyOf(t, second.expectStatus(http.MethodGet, RouteMetrics, "", "", "", http.StatusOK))
	if strings.Contains(secondBody, `recon_errors_total{code="internal",component="api"} 1`) {
		t.Fatalf("second API shared the first registry:\n%s", secondBody)
	}
}

func TestMetricsOptionUsesProvidedRegistry(t *testing.T) {
	processMetrics, err := metrics.New(metrics.Options{Component: "api"})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	processMetrics.ObserveError("api", "internal")
	h := newHarness(t, func(opts *Options) { opts.Metrics = processMetrics })

	body := bodyOf(t, h.expectStatus(http.MethodGet, RouteMetrics, "", "", "", http.StatusOK))
	if !strings.Contains(body, `recon_errors_total{code="internal",component="api"} 1`) {
		t.Fatalf("provided registry was not served:\n%s", body)
	}
}
