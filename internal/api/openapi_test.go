// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/TamasGorgics/recon-agent/internal/approval"
)

type openAPIOperation struct {
	Security      *[]map[string][]string `yaml:"security"`
	XRequiredRole string                 `yaml:"x-required-role"`
	Responses     map[string]yaml.Node   `yaml:"responses"`
}

type openAPIDocument struct {
	OpenAPI    string                                 `yaml:"openapi"`
	Paths      map[string]map[string]openAPIOperation `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]struct {
			Type   string `yaml:"type"`
			Scheme string `yaml:"scheme"`
		} `yaml:"securitySchemes"`
		Schemas map[string]yaml.Node `yaml:"schemas"`
	} `yaml:"components"`
}

var httpMethods = map[string]struct{}{
	"get": {}, "post": {}, "put": {}, "delete": {}, "patch": {},
	"options": {}, "head": {}, "trace": {},
}

// openAPIStatusSets is the exact set of statuses each operation can return.
// Authentication contributes 401/403 to protected operations, body decoding
// contributes 400/413/415 where a body is read, and service failures contribute
// 500. Public health handlers have no failure path and never return 500.
var openAPIStatusSets = map[string][]string{
	"GET " + RouteHealthz:           {"200"},
	"GET " + RouteMetrics:           {"200"},
	"POST " + RouteIngest:           {"200", "400", "401", "403", "413", "415", "500"},
	"POST " + RouteReconcile:        {"200", "400", "401", "403", "404", "413", "415", "500"},
	"GET " + RouteCases:             {"200", "400", "401", "403", "500"},
	"GET " + RouteCaseFile:          {"200", "400", "401", "403", "404", "500"},
	"GET " + RouteProposals:         {"200", "400", "401", "403", "500"},
	"POST " + RouteProposalDecision: {"200", "400", "401", "403", "404", "409", "413", "415", "500"},
	"POST " + RouteSwitches:         {"200", "400", "401", "403", "413", "415", "500"},
}

func loadOpenAPI(t *testing.T) openAPIDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var document openAPIDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return document
}

func TestOpenAPIRouteContract(t *testing.T) {
	document := loadOpenAPI(t)
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", document.OpenAPI)
	}
	scheme, ok := document.Components.SecuritySchemes["bearerAuth"]
	if !ok || scheme.Type != "http" || scheme.Scheme != "bearer" {
		t.Fatalf("bearerAuth scheme = %+v, want http bearer", scheme)
	}

	h := newHarness(t, nil)
	routerRoutes := make(map[string]map[string]Route)
	for _, route := range h.api.routeTable() {
		if routerRoutes[route.Pattern] == nil {
			routerRoutes[route.Pattern] = make(map[string]Route)
		}
		routerRoutes[route.Pattern][route.Method] = route
	}

	if len(document.Paths) != len(routerRoutes) {
		t.Fatalf("OpenAPI has %d paths, router has %d", len(document.Paths), len(routerRoutes))
	}
	for pattern, methods := range routerRoutes {
		item, ok := document.Paths[pattern]
		if !ok {
			t.Fatalf("router pattern %q is missing from OpenAPI", pattern)
		}
		for method := range item {
			if _, isMethod := httpMethods[method]; !isMethod {
				t.Fatalf("path %q has non-method key %q", pattern, method)
			}
		}
		if len(item) != len(methods) {
			t.Fatalf("path %q documents %d methods, router has %d", pattern, len(item), len(methods))
		}
		for method, route := range methods {
			operation, ok := item[methodLower(method)]
			if !ok {
				t.Fatalf("router route %s %s is missing from OpenAPI", method, pattern)
			}
			assertOpenAPIOperation(t, route, operation)
			assertOpenAPIStatusSet(t, route, operation)
		}
	}
	for pattern := range document.Paths {
		if _, ok := routerRoutes[pattern]; !ok {
			t.Fatalf("OpenAPI path %q is not implemented by the router", pattern)
		}
	}
}

func assertOpenAPIOperation(t *testing.T, route Route, operation openAPIOperation) {
	t.Helper()
	if route.Role == "" {
		if operation.Security == nil || len(*operation.Security) != 0 {
			t.Fatalf("public route %s %s must have security: []", route.Method, route.Pattern)
		}
		if operation.XRequiredRole != "none" {
			t.Fatalf("public route %s %s x-required-role = %q", route.Method, route.Pattern, operation.XRequiredRole)
		}
		for _, status := range []string{"401", "403"} {
			if _, ok := operation.Responses[status]; ok {
				t.Fatalf("public route %s %s documents %s", route.Method, route.Pattern, status)
			}
		}
		return
	}

	if operation.Security == nil || len(*operation.Security) != 1 {
		t.Fatalf("protected route %s %s must declare bearer security", route.Method, route.Pattern)
	}
	bearer, ok := (*operation.Security)[0]["bearerAuth"]
	if !ok || len(bearer) != 0 {
		t.Fatalf("protected route %s %s security = %+v", route.Method, route.Pattern, *operation.Security)
	}
	if operation.XRequiredRole != string(route.Role) {
		t.Fatalf("route %s %s x-required-role = %q, want %q", route.Method, route.Pattern, operation.XRequiredRole, route.Role)
	}
	for _, status := range []string{"200", "400", "401", "403", "500"} {
		if _, ok := operation.Responses[status]; !ok {
			t.Fatalf("route %s %s is missing the %s response", route.Method, route.Pattern, status)
		}
	}
}

// assertOpenAPIStatusSet requires the documented response set to equal the
// handler's status set exactly, in both directions.
func assertOpenAPIStatusSet(t *testing.T, route Route, operation openAPIOperation) {
	t.Helper()
	want, ok := openAPIStatusSets[route.Method+" "+route.Pattern]
	if !ok || len(want) == 0 {
		t.Fatalf("no status contract for route %s %s", route.Method, route.Pattern)
	}
	if len(operation.Responses) != len(want) {
		t.Fatalf("%s %s documents %d statuses %v, want %v",
			route.Method, route.Pattern, len(operation.Responses), responseStatuses(operation), want)
	}
	for _, status := range want {
		if _, ok := operation.Responses[status]; !ok {
			t.Fatalf("%s %s is missing documented status %s", route.Method, route.Pattern, status)
		}
	}
}

func responseStatuses(operation openAPIOperation) []string {
	statuses := make([]string, 0, len(operation.Responses))
	for status := range operation.Responses {
		statuses = append(statuses, status)
	}
	return statuses
}

// TestOpenAPIStatusSetsAreReachable proves the statuses that differ from the
// common auth/error set are produced by the real router, not only listed in
// the status table above.
func TestOpenAPIStatusSetsAreReachable(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{RouteReconcile, RouteSwitches} {
		response := h.expectStatus(http.MethodPost, path, "", "text/plain", testAdminKey, http.StatusUnsupportedMediaType)
		_ = response.Body.Close()

		small := newHarness(t, func(opts *Options) { opts.Config.MaxJSONBytes = 8 })
		response = small.expectStatus(http.MethodPost, path, strings.Repeat("x", 64), "application/json", testAdminKey, http.StatusRequestEntityTooLarge)
		_ = response.Body.Close()
	}

	decisionPath := "/v1/proposals/" + testDecisionProposalID + "/decision"
	response := h.expectStatus(http.MethodPost, decisionPath, "", "text/plain", testApproverKey, http.StatusUnsupportedMediaType)
	_ = response.Body.Close()
	small := newHarness(t, func(opts *Options) { opts.Config.MaxJSONBytes = 8 })
	response = small.expectStatus(http.MethodPost, decisionPath, strings.Repeat("x", 64), "application/json", testApproverKey, http.StatusRequestEntityTooLarge)
	_ = response.Body.Close()

	decisionStatuses := []struct {
		name   string
		body   string
		err    error
		status int
	}{
		{"invalid decision", `{"decision":"maybe"}`, nil, http.StatusBadRequest},
		{"proposal not found", `{"decision":"approved"}`, approval.ErrProposalNotFound, http.StatusNotFound},
		{"decision conflict", `{"decision":"approved"}`, approval.ErrDecisionConflict, http.StatusConflict},
		{"success", `{"decision":"approved"}`, nil, http.StatusOK},
		{"internal", `{"decision":"approved"}`, errors.New("decision backend unavailable"), http.StatusInternalServerError},
	}
	for _, test := range decisionStatuses {
		t.Run(test.name, func(t *testing.T) {
			decisionHarness := newHarness(t, nil)
			decisionHarness.decisions.err = test.err
			response := decisionHarness.expectStatus(http.MethodPost, decisionPath,
				test.body, "application/json", testApproverKey, test.status)
			_ = response.Body.Close()
		})
	}

	for _, path := range []string{RouteHealthz, RouteMetrics} {
		response := h.expectStatus(http.MethodGet, path, "", "", "", http.StatusOK)
		_ = response.Body.Close()
	}
}

func methodLower(method string) string {
	switch method {
	case "GET":
		return "get"
	case "POST":
		return "post"
	default:
		return method
	}
}

// TestOpenAPICaseFileV2Schema pins the case-file v2 contract: the version is
// 2, the trace section is present, and every chain link and missing marker is
// documented. The route/status contract is covered by TestOpenAPIRouteContract.
//
// covers: FIN-32
func TestOpenAPICaseFileV2Schema(t *testing.T) {
	document := loadOpenAPI(t)

	caseFile := decodeSchema(t, document, "CaseFile")
	if !containsString(caseFile.Required, "schema_version") || !containsString(caseFile.Required, "trace") {
		t.Fatalf("CaseFile required = %v", caseFile.Required)
	}
	var version struct {
		Const int `yaml:"const"`
	}
	decodeNode(t, caseFile.Properties["schema_version"], &version)
	if version.Const != 2 {
		t.Fatalf("CaseFile schema_version const = %d, want 2", version.Const)
	}
	var traceRef struct {
		Ref string `yaml:"$ref"`
	}
	decodeNode(t, caseFile.Properties["trace"], &traceRef)
	if traceRef.Ref != "#/components/schemas/CaseFileTrace" {
		t.Fatalf("CaseFile trace ref = %q", traceRef.Ref)
	}

	trace := decodeSchema(t, document, "CaseFileTrace")
	for _, key := range []string{"case", "chains", "missing"} {
		if !containsString(trace.Required, key) {
			t.Fatalf("CaseFileTrace required = %v, missing %q", trace.Required, key)
		}
	}

	chain := decodeSchema(t, document, "TraceChain")
	wantChainFields := []string{
		"ledger_txn_id", "applied_proposal", "proposal", "case", "breaks",
		"agent_run", "verification", "policy", "approvals", "executor",
		"ledger_txn", "ledger_entries", "missing",
	}
	for _, key := range wantChainFields {
		if !containsString(chain.Required, key) {
			t.Fatalf("TraceChain required = %v, missing %q", chain.Required, key)
		}
	}
	var ledgerTxnID struct {
		Types []string `yaml:"type"`
	}
	decodeNode(t, chain.Properties["ledger_txn_id"], &ledgerTxnID)
	if !containsString(ledgerTxnID.Types, "null") {
		t.Fatalf("ledger_txn_id types = %v, want nullable", ledgerTxnID.Types)
	}

	policy := decodeSchema(t, document, "TracePolicy")
	var configYAML struct {
		Description string `yaml:"description"`
	}
	decodeNode(t, policy.Properties["config_yaml"], &configYAML)
	if !strings.Contains(configYAML.Description, "not secrets") {
		t.Fatalf("config_yaml description = %q, want the not-secrets statement", configYAML.Description)
	}

	markers := decodeSchema(t, document, "TraceMissingMarker")
	wantMarkers := []string{
		"applied_proposal", "proposal", "case", "breaks", "agent_run",
		"agent_run.trace_id", "verification", "policy", "policy_config",
		"approvals", "executor.audit", "ledger_txn", "ledger_entries",
	}
	if len(markers.Enum) != len(wantMarkers) {
		t.Fatalf("marker enum = %v, want %v", markers.Enum, wantMarkers)
	}
	for i, marker := range wantMarkers {
		if markers.Enum[i] != marker {
			t.Fatalf("marker enum[%d] = %q, want %q", i, markers.Enum[i], marker)
		}
	}
}

type openAPISchema struct {
	Required   []string             `yaml:"required"`
	Properties map[string]yaml.Node `yaml:"properties"`
	Enum       []string             `yaml:"enum"`
}

func decodeSchema(t *testing.T, document openAPIDocument, name string) openAPISchema {
	t.Helper()
	node, ok := document.Components.Schemas[name]
	if !ok {
		t.Fatalf("OpenAPI is missing schema %q", name)
	}
	var schema openAPISchema
	decodeNode(t, node, &schema)
	return schema
}

func decodeNode(t *testing.T, node yaml.Node, out any) {
	t.Helper()
	if err := node.Decode(out); err != nil {
		t.Fatalf("decode OpenAPI schema node: %v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
