package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configEnvNames returns every environment variable read by the config package,
// including CONFIG_FILE which selects the YAML file.
func configEnvNames() []string {
	names := []string{EnvConfigFile}
	for _, binding := range envBindings {
		names = append(names, binding.name)
	}
	return names
}

// clearEnv isolates a test from the developer's shell environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range configEnvNames() {
		t.Setenv(name, "")
	}
}

func TestLoadFileValid(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Database.URL, "postgres://recon:recon@localhost:5432/recon?sslmode=disable"; got != want {
		t.Errorf("Database.URL = %q, want %q", got, want)
	}
	if got, want := cfg.Database.TestURL, "postgres://recon:recon@localhost:5432/recon_test?sslmode=disable"; got != want {
		t.Errorf("Database.TestURL = %q, want %q", got, want)
	}
	if got, want := cfg.Log.Level, "debug"; got != want {
		t.Errorf("Log.Level = %q, want %q", got, want)
	}
	if got, want := cfg.LLM.AnthropicAPIKey, "fake-anthropic-key"; got != want {
		t.Errorf("LLM.AnthropicAPIKey = %q, want %q", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestLoadFileUnknownKey(t *testing.T) {
	clearEnv(t)
	_, err := LoadFile("testdata/unknown-key.yaml")
	if err == nil {
		t.Fatal("LoadFile accepted an unknown key")
	}
	if !strings.Contains(err.Error(), "dsn") {
		t.Fatalf("LoadFile error = %v, want it to mention the unknown key %q", err, "dsn")
	}
}

func TestLoadFileInvalidType(t *testing.T) {
	clearEnv(t)
	_, err := LoadFile("testdata/invalid-type.yaml")
	if err == nil {
		t.Fatal("LoadFile accepted an invalid type")
	}
}

func TestValidateRequiresDatabaseURL(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadFile("testdata/missing-required.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a config without Database.URL")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Log.Level, DefaultLogLevel; got != want {
		t.Errorf("Log.Level = %q, want default %q", got, want)
	}
	if cfg.Database.URL != "" {
		t.Errorf("Database.URL = %q, want empty", cfg.Database.URL)
	}
	if cfg.Database.TestURL != "" {
		t.Errorf("Database.TestURL = %q, want empty", cfg.Database.TestURL)
	}
	if cfg.LLM.AnthropicAPIKey != "" {
		t.Errorf("LLM.AnthropicAPIKey = %q, want empty", cfg.LLM.AnthropicAPIKey)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted the default config without Database.URL")
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDatabaseURL, "postgres://env:env@db:5432/env_recon?sslmode=disable")
	t.Setenv(EnvLogLevel, "warn")

	cfg, err := LoadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Database.URL, "postgres://env:env@db:5432/env_recon?sslmode=disable"; got != want {
		t.Errorf("Database.URL = %q, want environment value %q", got, want)
	}
	if got, want := cfg.Log.Level, "warn"; got != want {
		t.Errorf("Log.Level = %q, want environment value %q", got, want)
	}
	if got, want := cfg.Database.TestURL, "postgres://recon:recon@localhost:5432/recon_test?sslmode=disable"; got != want {
		t.Errorf("Database.TestURL = %q, want YAML value %q", got, want)
	}
	if got, want := cfg.LLM.AnthropicAPIKey, "fake-anthropic-key"; got != want {
		t.Errorf("LLM.AnthropicAPIKey = %q, want YAML value %q", got, want)
	}
}

func TestEnvOverridesAllFields(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDatabaseURL, "postgres://env:env@db:5432/env_recon?sslmode=disable")
	t.Setenv(EnvDatabaseTestURL, "postgres://env:env@db:5432/env_recon_test?sslmode=disable")
	t.Setenv(EnvLogLevel, "error")
	t.Setenv(EnvAnthropicAPIKey, "env-anthropic-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Database.URL, "postgres://env:env@db:5432/env_recon?sslmode=disable"; got != want {
		t.Errorf("Database.URL = %q, want %q", got, want)
	}
	if got, want := cfg.Database.TestURL, "postgres://env:env@db:5432/env_recon_test?sslmode=disable"; got != want {
		t.Errorf("Database.TestURL = %q, want %q", got, want)
	}
	if got, want := cfg.Log.Level, "error"; got != want {
		t.Errorf("Log.Level = %q, want %q", got, want)
	}
	if got, want := cfg.LLM.AnthropicAPIKey, "env-anthropic-key"; got != want {
		t.Errorf("LLM.AnthropicAPIKey = %q, want %q", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestLoadUsesConfigFileEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvConfigFile, "testdata/valid.yaml")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Database.URL, "postgres://recon:recon@localhost:5432/recon?sslmode=disable"; got != want {
		t.Errorf("Database.URL = %q, want %q", got, want)
	}
}

func TestLoadMissingConfigFile(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvConfigFile, filepath.Join(t.TempDir(), "missing.yaml"))
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a CONFIG_FILE that does not exist")
	}
}

func TestEnvExampleParity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	listed := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf(".env.example line %q is not KEY=VALUE", line)
		}
		listed[key] = true
	}

	for _, name := range configEnvNames() {
		if !listed[name] {
			t.Errorf(".env.example is missing %s", name)
		}
	}
}
