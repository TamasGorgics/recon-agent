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
	if got, want := cfg.LLM.Provider, DefaultLLMProvider; got != want {
		t.Errorf("LLM.Provider = %q, want %q", got, want)
	}
	if got, want := cfg.LLM.Model, DefaultLLMModel; got != want {
		t.Errorf("LLM.Model = %q, want %q", got, want)
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
	if cfg.LLM.APIKey != "" {
		t.Errorf("LLM.APIKey = %q, want empty", cfg.LLM.APIKey)
	}
	if got, want := cfg.LLM.BaseURL, DefaultLLMBaseURL; got != want {
		t.Errorf("LLM.BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.LLM.Timeout, DefaultLLMTimeout; got != want {
		t.Errorf("LLM.Timeout = %s, want %s", got, want)
	}
	if got, want := cfg.LLM.MaxAttempts, DefaultLLMMaxAttempts; got != want {
		t.Errorf("LLM.MaxAttempts = %d, want %d", got, want)
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
	if got, want := cfg.LLM.Model, DefaultLLMModel; got != want {
		t.Errorf("LLM.Model = %q, want YAML value %q", got, want)
	}
}

func TestEnvOverridesAllFields(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDatabaseURL, "postgres://env:env@db:5432/env_recon?sslmode=disable")
	t.Setenv(EnvDatabaseTestURL, "postgres://env:env@db:5432/env_recon_test?sslmode=disable")
	t.Setenv(EnvLogLevel, "error")
	t.Setenv(EnvOpenCodeAPIKey, "env-opencode-key")
	t.Setenv(EnvOpenCodeBaseURL, "https://example.test/v1")
	t.Setenv(EnvOpenCodeModel, "opencode-go/glm-5.3-flash")

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
	if got, want := cfg.LLM.APIKey, "env-opencode-key"; got != want {
		t.Errorf("LLM.APIKey = %q, want %q", got, want)
	}
	if got, want := cfg.LLM.Model, "glm-5.3-flash"; got != want {
		t.Errorf("LLM.Model = %q, want normalized %q", got, want)
	}
	if got, want := cfg.LLM.BaseURL, "https://example.test/v1"; got != want {
		t.Errorf("LLM.BaseURL = %q, want %q", got, want)
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
