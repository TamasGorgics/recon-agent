package api

import (
	"net/http"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// AuthKind is the route's authentication mode. It is explicit rather than
// inferred from an empty role so a signature-authenticated callback can never
// fall through to the public branch.
type AuthKind uint8

const (
	// AuthPublic serves the request with no authentication (health, metrics).
	AuthPublic AuthKind = iota
	// AuthAPIKey requires a static Bearer API key with the route's role.
	AuthAPIKey
	// AuthSignature requires the route-specific signature middleware.
	AuthSignature
)

// String renders a short diagnostic label for the auth mode. It is not the
// OpenAPI x-auth-kind value; the Slack operation uses "slack-signature".
func (k AuthKind) String() string {
	switch k {
	case AuthPublic:
		return "public"
	case AuthAPIKey:
		return "api-key"
	case AuthSignature:
		return "signature"
	default:
		return "unknown"
	}
}

// Route is one row of the single route registry. Method+Pattern are mounted by
// the chi router and compared with api/openapi.yaml by the contract test. Auth
// selects the middleware; Role is non-empty exactly for AuthAPIKey routes.
type Route struct {
	Method  string
	Pattern string
	Role    config.Role
	Auth    AuthKind
	Handler http.HandlerFunc
}

// Route patterns. They are exported constants so the OpenAPI contract test and
// handler tests share the same spelling, including "{id}".
const (
	RouteHealthz           = "/healthz"
	RouteMetrics           = "/metrics"
	RouteIngest            = "/v1/ingest/files"
	RouteReconcile         = "/v1/reconcile/run"
	RouteCases             = "/v1/cases"
	RouteCaseFile          = "/v1/cases/{id}"
	RouteProposals         = "/v1/proposals"
	RouteProposalDecision  = "/v1/proposals/{id}/decision"
	RouteSlackInteractions = "/v1/slack/interactions"
	RouteSwitches          = "/v1/admin/switches"
)

// routeTable is the single source of truth for method, path, auth kind,
// required role, and handler. Order affects only the Allow header for 405
// responses.
func (a *API) routeTable() []Route {
	return []Route{
		{Method: http.MethodGet, Pattern: RouteHealthz, Auth: AuthPublic, Handler: a.handleHealthz},
		{Method: http.MethodGet, Pattern: RouteMetrics, Auth: AuthPublic, Handler: a.handleMetrics},
		{Method: http.MethodPost, Pattern: RouteIngest, Auth: AuthAPIKey, Role: config.RoleAdmin, Handler: a.handleIngest},
		{Method: http.MethodPost, Pattern: RouteReconcile, Auth: AuthAPIKey, Role: config.RoleAdmin, Handler: a.handleReconcile},
		{Method: http.MethodGet, Pattern: RouteCases, Auth: AuthAPIKey, Role: config.RoleViewer, Handler: a.handleListCases},
		{Method: http.MethodGet, Pattern: RouteCaseFile, Auth: AuthAPIKey, Role: config.RoleViewer, Handler: a.handleCaseFile},
		{Method: http.MethodGet, Pattern: RouteProposals, Auth: AuthAPIKey, Role: config.RoleViewer, Handler: a.handleListProposals},
		{Method: http.MethodPost, Pattern: RouteProposalDecision, Auth: AuthAPIKey, Role: config.RoleApprover, Handler: a.handleProposalDecision},
		{Method: http.MethodPost, Pattern: RouteSlackInteractions, Auth: AuthSignature, Handler: a.handleSlackInteraction},
		{Method: http.MethodPost, Pattern: RouteSwitches, Auth: AuthAPIKey, Role: config.RoleAdmin, Handler: a.handleSetSwitch},
	}
}

// allowedMethods returns the comma-separated Allow value for a path that
// matched a route pattern but not the request method. It understands the single
// "{id}" parameter; unknown paths return "".
func (a *API) allowedMethods(path string) string {
	methods := make([]string, 0, 1)
	for _, route := range a.routeTable() {
		if patternMatches(route.Pattern, path) {
			methods = append(methods, route.Method)
		}
	}
	return strings.Join(methods, ", ")
}

// patternMatches compares a chi pattern with a concrete request path. The only
// supported parameter form is a whole-segment "{name}".
func patternMatches(pattern, path string) bool {
	patternParts := splitPath(pattern)
	pathParts := splitPath(path)
	if len(patternParts) != len(pathParts) {
		return false
	}
	for i, part := range patternParts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			if pathParts[i] == "" {
				return false
			}
			continue
		}
		if part != pathParts[i] {
			return false
		}
	}
	return true
}

func splitPath(value string) []string {
	trimmed := strings.TrimPrefix(value, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}
