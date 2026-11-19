package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCasesConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// covers: FIN-41
func TestCasesMissingSettlementSLADefault(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Cases.MissingSettlementSLABusinessDays, DefaultMissingSettlementSLABusinessDays; got != want {
		t.Fatalf("default SLA = %d, want %d", got, want)
	}
	if got, want := DefaultMissingSettlementSLABusinessDays, 5; got != want {
		t.Fatalf("DefaultMissingSettlementSLABusinessDays = %d, want %d", got, want)
	}
	if err := cfg.Cases.Validate(); err != nil {
		t.Fatalf("default Cases.Validate: %v", err)
	}
}

// covers: FIN-41
func TestCasesMissingSettlementSLAFromYAML(t *testing.T) {
	clearEnv(t)
	path := writeCasesConfig(t, "cases:\n  missing_settlement_sla_business_days: 12\n")
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Cases.MissingSettlementSLABusinessDays, 12; got != want {
		t.Fatalf("SLA = %d, want %d", got, want)
	}
	if err := cfg.Cases.Validate(); err != nil {
		t.Fatalf("Cases.Validate: %v", err)
	}

	// Every boundary value in the inclusive range is accepted.
	for _, days := range []int{1, 365} {
		cfg := DefaultCasesConfig()
		cfg.MissingSettlementSLABusinessDays = days
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Cases.Validate(%d) = %v, want nil", days, err)
		}
	}
}

// covers: FIN-41
func TestCasesMissingSettlementSLAValidation(t *testing.T) {
	for _, days := range []int{-1, 0, 366, 1000} {
		t.Run(fmt.Sprintf("%d", days), func(t *testing.T) {
			cfg := CasesConfig{MissingSettlementSLABusinessDays: days}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Cases.Validate(%d) = nil, want an error", days)
			}
			if !strings.Contains(err.Error(), MissingSettlementSLAKey) {
				t.Errorf("error = %v, want it to name %q", err, MissingSettlementSLAKey)
			}
			if !strings.Contains(err.Error(), "between 1 and 365") {
				t.Errorf("error = %v, want it to name the 1..365 range", err)
			}
		})
	}
}

// covers: FIN-41
func TestCasesMissingSettlementSLANonIntegerFails(t *testing.T) {
	for _, value := range []string{"five", "1.5", "true"} {
		t.Run(value, func(t *testing.T) {
			clearEnv(t)
			path := writeCasesConfig(t, "cases:\n  missing_settlement_sla_business_days: "+value+"\n")
			_, err := LoadFile(path)
			if err == nil {
				t.Fatalf("LoadFile accepted non-integer SLA %q", value)
			}
			if !strings.Contains(err.Error(), MissingSettlementSLAKey) {
				t.Fatalf("error = %v, want it to name %q", err, MissingSettlementSLAKey)
			}
		})
	}
}

// covers: FIN-41
func TestCasesRejectsMaxAttemptsKey(t *testing.T) {
	clearEnv(t)
	path := writeCasesConfig(t, "cases:\n  missing_settlement_sla_business_days: 5\n  max_attempts: 3\n")
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("LoadFile accepted a cases.max_attempts key; the attempt cap stays a constant")
	}
	if !strings.Contains(err.Error(), "max_attempts") {
		t.Fatalf("error = %v, want it to name the rejected key", err)
	}
}

// covers: FIN-41
func TestValidateAPIRuntimeIncludesCases(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDatabaseAppURL, "postgres://recon:recon@localhost:5432/recon?sslmode=disable")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.API.Keys = []APIKey{{ID: "test", Role: RoleAdmin}}
	cfg.Slack = SlackConfig{BotToken: "test-bot-token", SigningSecret: "test-signing-secret"}
	cfg.Cases.MissingSettlementSLABusinessDays = 0
	if err := cfg.ValidateAPIRuntime(); err == nil {
		t.Fatal("ValidateAPIRuntime accepted an out-of-range SLA")
	} else if !strings.Contains(err.Error(), MissingSettlementSLAKey) {
		t.Fatalf("error = %v, want it to name %q", err, MissingSettlementSLAKey)
	}
}
