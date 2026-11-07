package dataset

import (
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/injections"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// covers: FIN-13
func TestDefaultConfigLocks(t *testing.T) {
	cases := []struct {
		split      Split
		seed       int64
		days       int
		difficulty string
		total      int
		batchApps  int
	}{
		{Dev, 1, 35, synth.DifficultyRealistic, 138, 6},
		{Test, 2, 35, synth.DifficultyRealistic, 345, 15},
	}
	for _, tc := range cases {
		t.Run(string(tc.split), func(t *testing.T) {
			cfg, err := DefaultConfig(tc.split)
			if err != nil {
				t.Fatalf("DefaultConfig: %v", err)
			}
			if cfg.Seed != tc.seed || cfg.Days != tc.days || cfg.Difficulty != tc.difficulty {
				t.Fatalf("seed/days/difficulty = %d/%d/%s, want %d/%d/%s",
					cfg.Seed, cfg.Days, cfg.Difficulty, tc.seed, tc.days, tc.difficulty)
			}
			if cfg.Modes.Total != tc.total {
				t.Fatalf("modes total = %d, want %d", cfg.Modes.Total, tc.total)
			}
			sum := 0
			perMode := (tc.total - tc.batchApps) / (len(modes.AllModes) - 1)
			for _, mode := range modes.AllModes {
				want := perMode
				if mode == modes.BatchComposite {
					want = tc.batchApps
				}
				if cfg.Modes.Counts[mode] != want {
					t.Fatalf("%s applications = %d, want %d", mode, cfg.Modes.Counts[mode], want)
				}
				sum += cfg.Modes.Counts[mode]
			}
			if sum != tc.total {
				t.Fatalf("application counts sum = %d, want %d", sum, tc.total)
			}
			assertLedgerAllowlist(t, cfg)
		})
	}

	t.Run("injection", func(t *testing.T) {
		cfg, err := DefaultConfig(Injection)
		if err != nil {
			t.Fatalf("DefaultConfig: %v", err)
		}
		if cfg.Seed != 3 || cfg.Days != 16 || cfg.Difficulty != synth.DifficultyAdversarial {
			t.Fatalf("seed/days/difficulty = %d/%d/%s, want 3/16/adversarial",
				cfg.Seed, cfg.Days, cfg.Difficulty)
		}
		if cfg.Modes.Total != 30 || len(cfg.Modes.Counts) != 1 ||
			cfg.Modes.Counts[modes.InjectionMemo] != 30 {
			t.Fatalf("injection mode mix = total %d counts %v", cfg.Modes.Total, cfg.Modes.Counts)
		}
		assertLedgerAllowlist(t, cfg)
	})

	if _, err := DefaultConfig(Split("holdout")); err == nil {
		t.Fatal("DefaultConfig accepted an unknown split")
	}
}

func assertLedgerAllowlist(t *testing.T, cfg Config) {
	t.Helper()
	for _, currency := range cfg.Synth.CurrenciesSorted() {
		lower := strings.ToLower(currency)
		for _, code := range []string{
			"clearing_psp_a_" + lower,
			"fees_expense_" + lower,
			"refunds_expense_" + lower,
		} {
			if !cfg.Ledger.AllowsAdjustment(code) {
				t.Fatalf("adjustment allowlist is missing %q", code)
			}
		}
	}
}

func TestConfigValidationRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"split", func(c *Config) { c.Split = Split("holdout") }, "split"},
		{"negative seed", func(c *Config) { c.Seed = -1 }, "seed"},
		{"zero days", func(c *Config) { c.Days = 0 }, "days"},
		{"too many days", func(c *Config) { c.Days = 3661 }, "days"},
		{"difficulty", func(c *Config) { c.Difficulty = "brutal" }, "difficulty"},
		{"mode total", func(c *Config) { c.Modes.Total++ }, "modes"},
		{"synth currencies", func(c *Config) { c.Synth.Currencies = nil }, "synth"},
		{"adjustment allowlist", func(c *Config) {
			filtered := c.Ledger.AdjustmentAccounts[:0]
			for _, code := range c.Ledger.AdjustmentAccounts {
				if code != "fees_expense_usd" {
					filtered = append(filtered, code)
				}
			}
			c.Ledger.AdjustmentAccounts = filtered
		}, "ledger.adjustment_accounts"},
		{"missing psp calendar", func(c *Config) {
			c.Sources = map[string]config.SourceConfig{
				synth.SourceBank: config.DefaultSynthIngestSources()[synth.SourceBank],
			}
		}, "psp_a calendar"},
		{"duplicate template", func(c *Config) {
			c.Templates = []injections.Template{
				{Name: "same.txt", Text: "first"},
				{Name: "same.txt", Text: "second"},
			}
		}, "templates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DefaultConfig(Test)
			if err != nil {
				t.Fatalf("DefaultConfig: %v", err)
			}
			tc.mutate(&cfg)
			err = cfg.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestConfigValidateAcceptsDefaults(t *testing.T) {
	for _, split := range Splits() {
		cfg, err := DefaultConfig(split)
		if err != nil {
			t.Fatalf("DefaultConfig(%s): %v", split, err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(%s): %v", split, err)
		}
	}
}
