package main

import (
	"context"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// TestRunStartupErrorRedactsDSNAndAPIKey proves the process returns a startup
// error that is safe to print to stderr: neither the malformed DSN password nor
// the configured API key may appear.
func TestRunStartupErrorRedactsDSNAndAPIKey(t *testing.T) {
	const (
		dbSecret = "startup-secret-password-fragment"
		apiKey   = "startup-api-key-0123456789-abcdef"
	)
	t.Setenv(config.EnvConfigFile, "")
	t.Setenv(config.EnvDatabaseAppURL,
		"postgres://recon_app:"+dbSecret+"@localhost:notaport/recon")
	t.Setenv(config.EnvAPIKeys, `[{"id":"ops","role":"admin","key":"`+apiKey+`"}]`)
	t.Setenv(config.EnvLogLevel, "info")

	err := run(context.Background())
	if err == nil {
		t.Fatal("run accepted a malformed database DSN")
	}
	text := err.Error()
	for _, secret := range []string{dbSecret, apiKey, "postgres://", "password="} {
		if strings.Contains(text, secret) {
			t.Fatalf("startup error leaked %q: %v", secret, err)
		}
	}
}
