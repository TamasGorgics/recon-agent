package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultLedgerConfig(t *testing.T) {
	cfg := defaults()
	if len(cfg.Ledger.AdjustmentAccounts) != len(DefaultAdjustmentAccounts) {
		t.Fatalf("default adjustment accounts = %v, want %v", cfg.Ledger.AdjustmentAccounts, DefaultAdjustmentAccounts)
	}
	for _, code := range DefaultAdjustmentAccounts {
		if !cfg.Ledger.AllowsAdjustment(code) {
			t.Errorf("default ledger config does not allow adjustment account %q", code)
		}
	}
	if cfg.Ledger.AllowsAdjustment("clearing_usd") {
		t.Error("default ledger config allows a generic clearing account")
	}
	if len(cfg.Ledger.CustomerFundsAccounts) != 0 {
		t.Errorf("default customer-funds accounts = %v, want empty", cfg.Ledger.CustomerFundsAccounts)
	}

	// The returned slice is a copy; mutating it must not change the package
	// default.
	cfg.Ledger.AdjustmentAccounts[0] = "mutated"
	if DefaultAdjustmentAccounts[0] == "mutated" {
		t.Fatal("DefaultLedgerConfig shares its backing array with DefaultAdjustmentAccounts")
	}
}

func TestLedgerConfigMembership(t *testing.T) {
	cfg := LedgerConfig{
		AdjustmentAccounts:    []string{"suspense", "rounding"},
		CustomerFundsAccounts: []string{"customer_funds_usd"},
	}
	if !cfg.AllowsAdjustment("suspense") || !cfg.AllowsAdjustment("rounding") {
		t.Error("allowlist does not contain a configured account")
	}
	if cfg.AllowsAdjustment("suspense ") || cfg.AllowsAdjustment("Suspense") {
		t.Error("allowlist matching must be exact")
	}
	if !cfg.IsCustomerFunds("customer_funds_usd") {
		t.Error("customer-funds list does not contain a configured account")
	}
	if cfg.IsCustomerFunds("clearing_usd") {
		t.Error("customer-funds list matched an unrelated account")
	}
}

func TestLoadFileLedgerSection(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "ledger:\n" +
		"  adjustment_accounts: [suspense]\n" +
		"  customer_funds_accounts: [customer_funds_usd]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := cfg.Ledger.AdjustmentAccounts; len(got) != 1 || got[0] != "suspense" {
		t.Fatalf("adjustment accounts = %v, want [suspense]", got)
	}
	if got := cfg.Ledger.CustomerFundsAccounts; len(got) != 1 || got[0] != "customer_funds_usd" {
		t.Fatalf("customer-funds accounts = %v, want [customer_funds_usd]", got)
	}
}
