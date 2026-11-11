package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

type principalKey struct{}

// Principal is the authenticated API key identity.
type Principal struct {
	KeyID string
	Role  config.Role
}

// PrincipalFromContext returns the authenticated principal, if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok
}

// authenticate parses exactly one Bearer token, compares its SHA-256 digest
// against every configured key with a constant-time comparison, then enforces
// the route's required role. Authentication and authorization happen before
// the handler parses a body.
func (a *API) authenticate(required config.Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Values("Authorization"))
		if !ok {
			a.unauthorized(w, r)
			return
		}
		digest := sha256.Sum256([]byte(token))
		matched := -1
		for i := range a.keys {
			equal := subtle.ConstantTimeCompare(digest[:], a.keys[i].Digest[:])
			if equal == 1 && matched == -1 {
				matched = i
			}
		}
		if matched < 0 {
			a.unauthorized(w, r)
			return
		}
		principal := Principal{KeyID: a.keys[matched].ID, Role: a.keys[matched].Role}
		if !principal.Role.Allows(required) {
			a.writeError(w, r, http.StatusForbidden, codeForbidden, messageForbidden)
			return
		}
		if state := requestStateFrom(r.Context()); state != nil {
			state.principal = principal
		}
		ctx := context.WithValue(r.Context(), principalKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="recon"`)
	a.writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, messageUnauthenticated)
}

// bearerToken accepts exactly one "Bearer <token>" header value. The scheme is
// case-insensitive; the token is non-empty and contains no whitespace or comma.
func bearerToken(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	value := values[0]
	scheme, rest, found := strings.Cut(value, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	if rest == "" || strings.TrimSpace(rest) != rest || strings.ContainsAny(rest, " \t\r\n,") {
		return "", false
	}
	return rest, true
}
