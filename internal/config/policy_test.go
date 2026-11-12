package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicyYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestPolicyFileDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Policy.File, DefaultPolicyFile; got != want {
		t.Errorf("Policy.File = %q, want %q", got, want)
	}
	if got, want := EnvPolicyFile, "POLICY_FILE"; got != want {
		t.Errorf("EnvPolicyFile = %q, want %q", got, want)
	}
	if got, want := DefaultPolicyFile, "policy.yaml"; got != want {
		t.Errorf("DefaultPolicyFile = %q, want %q", got, want)
	}
}

func TestPolicyFileEnvOverridesDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPolicyFile, "env-policy.yaml")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Policy.File, "env-policy.yaml"; got != want {
		t.Errorf("Policy.File = %q, want environment value %q", got, want)
	}
}

func TestPolicyFileYAMLAndEnvPrecedence(t *testing.T) {
	clearEnv(t)
	path := writePolicyYAML(t, "policy:\n  file: yaml-policy.yaml\n")
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Policy.File, "yaml-policy.yaml"; got != want {
		t.Errorf("Policy.File = %q, want YAML value %q", got, want)
	}

	t.Setenv(EnvPolicyFile, "env-wins.yaml")
	cfg, err = LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile with env: %v", err)
	}
	if got, want := cfg.Policy.File, "env-wins.yaml"; got != want {
		t.Errorf("Policy.File = %q, want environment value %q", got, want)
	}
}

func TestPolicyFileUnknownKeyRejected(t *testing.T) {
	clearEnv(t)
	path := writePolicyYAML(t, "policy:\n  file: policy.yaml\n  extra: nope\n")
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted an unknown policy key")
	}
}

func TestPolicyFileEmptyRejected(t *testing.T) {
	clearEnv(t)
	cfg := defaults()
	cfg.Database.URL = "postgres://user:pass@localhost:5432/recon?sslmode=disable"
	cfg.Policy.File = "  "
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted an empty policy.file")
	}
	if !strings.Contains(err.Error(), "policy.file") {
		t.Fatalf("Validate error = %v, want it to name policy.file", err)
	}
}

func TestPolicyFileRenderedWithoutSecrets(t *testing.T) {
	clearEnv(t)
	cfg := defaults()
	cfg.Database.URL = "postgres://user:secret@localhost:5432/recon?sslmode=disable"
	if strings.Contains(cfg.String(), "secret") {
		t.Fatal("Config.String leaked a secret")
	}
	if !strings.Contains(cfg.String(), "Policy:{File:"+DefaultPolicyFile+"}") {
		t.Fatalf("Config.String = %q, want it to include the policy file", cfg.String())
	}
}
