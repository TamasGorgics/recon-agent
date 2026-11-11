package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// apiTestKey is a distinctive, valid plaintext used for secret-leak checks.
const apiTestKey = "unit-test-api-key-0123456789-abcdef"

func TestParseAPIKeysValid(t *testing.T) {
	clearEnv(t)
	raw := `[{"id":"dev-admin","role":"admin","key":"` + apiTestKey + `"},
	          {"id":"dev-viewer","role":"viewer","key":"another-valid-api-key-0123456789"}]`
	keys, err := ParseAPIKeys(raw)
	if err != nil {
		t.Fatalf("ParseAPIKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(keys))
	}
	if keys[0].ID != "dev-admin" || keys[0].Role != RoleAdmin {
		t.Fatalf("first key = %+v", keys[0])
	}
	if got, want := keys[0].Digest, sha256.Sum256([]byte(apiTestKey)); got != want {
		t.Fatalf("digest = %x, want %x", got, want)
	}
	if keys[1].Role != RoleViewer {
		t.Fatalf("second role = %q", keys[1].Role)
	}
}

func TestParseAPIKeysEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "[]"} {
		keys, err := ParseAPIKeys(raw)
		if err != nil {
			t.Fatalf("ParseAPIKeys(%q): %v", raw, err)
		}
		if len(keys) != 0 {
			t.Fatalf("ParseAPIKeys(%q) = %d keys, want 0", raw, len(keys))
		}
	}
}

func TestParseAPIKeysRejectsInvalid(t *testing.T) {
	valid := `{"id":"id-%d","role":"viewer","key":"0123456789abcdef0123456789abcdef-%d"}`
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"malformed json", `[{"id":`, "JSON array"},
		{"not array", `{"id":"a","role":"viewer","key":"` + apiTestKey + `"}`, "JSON array"},
		{"trailing value", `[` + fmt.Sprintf(valid, 1, 1) + `] []`, "exactly one"},
		{"missing id", `[{"id":"","role":"viewer","key":"` + apiTestKey + `"}]`, "id is required"},
		{"whitespace id", `[{"id":" a","role":"viewer","key":"` + apiTestKey + `"}]`, "surrounding whitespace"},
		{"control id", `[{"id":"a\u0001","role":"viewer","key":"` + apiTestKey + `"}]`, "visible ASCII"},
		{"duplicate id", `[` + fmt.Sprintf(valid, 1, 1) + `,` + fmt.Sprintf(valid, 1, 2) + `]`, "duplicate id"},
		{"invalid role", `[{"id":"a","role":"root","key":"` + apiTestKey + `"}]`, "role must be"},
		{"missing role", `[{"id":"a","key":"` + apiTestKey + `"}]`, "role must be"},
		{"short key", `[{"id":"a","role":"viewer","key":"short"}]`, "at least 32"},
		{"whitespace key", `[{"id":"a","role":"viewer","key":"` + apiTestKey + ` "}]`, "leading or trailing"},
		{"duplicate key", `[` + fmt.Sprintf(valid, 1, 1) + `,` + fmt.Sprintf(valid, 2, 1) + `]`, "duplicate key material"},
		{"unknown field", `[{"id":"a","role":"viewer","key":"` + apiTestKey + `","admin":true}]`, "JSON array"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseAPIKeys(test.raw)
			if err == nil {
				t.Fatal("ParseAPIKeys accepted an invalid value")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want it to contain %q", err, test.want)
			}
			if strings.Contains(err.Error(), apiTestKey) {
				t.Fatalf("error leaked key material: %q", err)
			}
			if strings.Contains(err.Error(), "0123456789abcdef") {
				t.Fatalf("error leaked key material: %q", err)
			}
		})
	}
}

func TestLoadAPIKeysFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKeys, `[{"id":"dev-admin","role":"admin","key":"`+apiTestKey+`"}]`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.API.Keys) != 1 {
		t.Fatalf("len(cfg.API.Keys) = %d, want 1", len(cfg.API.Keys))
	}
	if got, want := cfg.API.Keys[0].Digest, sha256.Sum256([]byte(apiTestKey)); got != want {
		t.Fatalf("digest = %x, want %x", got, want)
	}
}

func TestLoadMalformedAPIKeysAborts(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKeys, `[{"id":"dev","role":"admin","key":"`+apiTestKey+`"}] []`)
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted trailing API_KEYS data")
	}
	if strings.Contains(err.Error(), apiTestKey) {
		t.Fatalf("Load error leaked key material: %q", err)
	}
}

func TestLoadRejectsAPIKeysInYAML(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "keys.yaml")
	if err := os.WriteFile(path, []byte("api:\n  keys:\n    - id: dev\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("LoadFile accepted api.keys in YAML")
	}
	if !strings.Contains(err.Error(), "keys") {
		t.Fatalf("error = %v, want the unknown api.keys field", err)
	}
}

func TestAPIConfigValidate(t *testing.T) {
	if err := DefaultAPIConfig().Validate(); err != nil {
		t.Fatalf("default API config is invalid: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*APIConfig)
		want   string
	}{
		{"missing addr", func(c *APIConfig) { c.Addr = " " }, "api.addr"},
		{"zero read timeout", func(c *APIConfig) { c.ReadTimeout = 0 }, "api.read_timeout"},
		{"negative write timeout", func(c *APIConfig) { c.WriteTimeout = -time.Second }, "api.write_timeout"},
		{"small header cap", func(c *APIConfig) { c.MaxHeaderBytes = 4095 }, "max_header_bytes"},
		{"zero json cap", func(c *APIConfig) { c.MaxJSONBytes = 0 }, "max_json_bytes"},
		{"upload below json", func(c *APIConfig) { c.MaxUploadBytes = c.MaxJSONBytes - 1 }, "max_upload_bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultAPIConfig()
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid API block")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestValidateAPIRuntime(t *testing.T) {
	clearEnv(t)
	cfg := defaults()
	cfg.Database.AppURL = "postgres://app:app@localhost:5432/recon?sslmode=disable"
	cfg.API.Keys = []APIKey{{ID: "dev", Role: RoleAdmin}}
	if err := cfg.ValidateAPIRuntime(); err != nil {
		t.Fatalf("ValidateAPIRuntime: %v", err)
	}
	if cfg.Database.URL != "" || cfg.LLM.APIKey != "" || cfg.Database.AgentURL != "" {
		t.Fatal("test config unexpectedly needs superuser/agent/LLM fields")
	}

	missingApp := cfg
	missingApp.Database.AppURL = ""
	if err := missingApp.ValidateAPIRuntime(); err == nil || !strings.Contains(err.Error(), "database.app_url") {
		t.Fatalf("missing app URL error = %v", err)
	}

	noKeys := cfg
	noKeys.API.Keys = nil
	if err := noKeys.ValidateAPIRuntime(); err == nil || !strings.Contains(err.Error(), "API_KEYS") {
		t.Fatalf("missing keys error = %v", err)
	}

	badAPI := cfg
	badAPI.API.WriteTimeout = 0
	if err := badAPI.ValidateAPIRuntime(); err == nil || !strings.Contains(err.Error(), "api.write_timeout") {
		t.Fatalf("invalid API block error = %v", err)
	}
}

func TestRoleOrdering(t *testing.T) {
	if !RoleAdmin.Allows(RoleViewer) || !RoleApprover.Allows(RoleViewer) {
		t.Fatal("admin/approver must satisfy viewer")
	}
	if RoleViewer.Allows(RoleAdmin) || RoleApprover.Allows(RoleAdmin) {
		t.Fatal("viewer/approver must not satisfy admin")
	}
	if !RoleViewer.Allows(RoleViewer) {
		t.Fatal("viewer must satisfy viewer")
	}
}

func TestAPIKeyAndAPIConfigFormattingRedactsDigest(t *testing.T) {
	key := APIKey{ID: "ops-admin", Role: RoleAdmin, Digest: sha256.Sum256([]byte(apiTestKey))}
	api := APIConfig{Addr: ":8080", Keys: []APIKey{key}}
	digestHex := fmt.Sprintf("%x", key.Digest)
	digestBytes := fmt.Sprintf("%v", key.Digest)
	digestGo := fmt.Sprintf("%#v", key.Digest)
	values := map[string]any{
		"key value":   key,
		"key pointer": &key,
		"api value":   api,
		"api pointer": &api,
		"key slice":   []APIKey{key},
	}
	for name, value := range values {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(format, value)
			for _, secret := range []string{digestHex, digestBytes, digestGo, apiTestKey} {
				if strings.Contains(out, secret) {
					t.Fatalf("%s %s leaked API key material: %s", name, format, out)
				}
			}
		}
	}
	for name, out := range map[string]string{
		"APIKey.GoString":    key.GoString(),
		"APIConfig.GoString": api.GoString(),
	} {
		for _, secret := range []string{digestHex, digestBytes, digestGo, apiTestKey} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s leaked API key material: %s", name, out)
			}
		}
	}
}

func TestConfigFormattingRedactsSecrets(t *testing.T) {
	keys, err := ParseAPIKeys(`[{"id":"ops-admin","role":"admin","key":"` + apiTestKey + `"}]`)
	if err != nil {
		t.Fatalf("ParseAPIKeys: %v", err)
	}
	cfg := Config{
		Database: DatabaseConfig{
			URL:      "postgres://recon:db-password-secret@localhost:5432/recon?sslmode=disable",
			TestURL:  "postgres://recon:db-password-secret@localhost:5432/recon_test?sslmode=disable",
			AppURL:   "postgres://recon:db-password-secret@localhost:5432/recon?sslmode=disable",
			AgentURL: "postgres://recon_agent:db-password-secret@localhost:5432/recon?sslmode=disable",
		},
		LLM: LLMConfig{
			Provider: "opencode",
			BaseURL:  "https://example.invalid",
			APIKey:   "llm-api-secret-value",
			Model:    "test-model",
		},
		API: APIConfig{Addr: ":8080", Keys: keys},
	}
	digest := fmt.Sprintf("%x", keys[0].Digest)
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(format, cfg)
		for _, secret := range []string{digest, "db-password-secret", "llm-api-secret-value", apiTestKey} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s leaked %q: %s", format, secret, out)
			}
		}
	}
	for _, value := range []any{cfg.API, keys[0]} {
		out := fmt.Sprintf("%+v", value)
		if strings.Contains(out, digest) || strings.Contains(out, apiTestKey) {
			t.Fatalf("formatted value leaked key material: %s", out)
		}
	}
}
