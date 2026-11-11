package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// malformedSecretDSNs are DSNs whose parse error from pgx echoes the raw input
// unless the caller replaces the cause. Every password carries secretMarker.
const secretMarker = "ADVERSARY-SECRET-PASSWORD"

func malformedSecretDSNs() map[string]string {
	return map[string]string{
		"url invalid port":             "postgres://recon_app:" + secretMarker + "@localhost:notaport/recon",
		"url invalid host escape":      "postgres://recon_app:" + secretMarker + "@host%zz:5432/recon",
		"url unterminated bracket":     "postgres://recon_app:" + secretMarker + "@[::1/recon",
		"keyword unterminated quote":   "host=localhost user=recon_app password='first " + secretMarker,
		"keyword missing value":        "host=localhost user=recon_app password=" + secretMarker + " extra",
		"url malformed query password": "postgres://localhost/recon?sslmode=bogus&password=" + secretMarker,
	}
}

func assertNoSecret(t *testing.T, operation string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted a malformed DSN", operation)
	}
	text := err.Error()
	if strings.Contains(text, secretMarker) {
		t.Fatalf("%s error leaked the DSN password: %v", operation, err)
	}
	for _, fragment := range []string{"postgres://", "password=", "notaport", "host%zz"} {
		if strings.Contains(text, fragment) {
			t.Fatalf("%s error echoed raw DSN material %q: %v", operation, fragment, err)
		}
	}
}

func TestOpenRoleParseErrorsDoNotLeakDSN(t *testing.T) {
	for name, dsn := range malformedSecretDSNs() {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := db.OpenRole(ctx, db.RoleApp, dsn)
			assertNoSecret(t, "OpenRole", err)
		})
	}
}

func TestNewPoolParseErrorsDoNotLeakDSN(t *testing.T) {
	for name, dsn := range malformedSecretDSNs() {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := db.NewPool(ctx, dsn)
			assertNoSecret(t, "NewPool", err)
		})
	}
}
