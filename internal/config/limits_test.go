package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoApplyLimitsDefaults(t *testing.T) {
	cfg := DefaultAutoApplyLimitsConfig()
	wantCaps := map[string]int64{
		"USD": DefaultAutoApplyDailyCapUSD,
		"EUR": DefaultAutoApplyDailyCapEUR,
		"SEK": DefaultAutoApplyDailyCapSEK,
		"JPY": DefaultAutoApplyDailyCapJPY,
	}
	if len(cfg.DailyMaxValueMinor) != len(wantCaps) {
		t.Fatalf("DailyMaxValueMinor = %#v, want %d currencies", cfg.DailyMaxValueMinor, len(wantCaps))
	}
	for currency, want := range wantCaps {
		if got := cfg.DailyMaxValueMinor[currency]; got != want {
			t.Errorf("DailyMaxValueMinor[%s] = %d, want %d", currency, got, want)
		}
	}
	if got, want := cfg.BreakRateThresholdBPS, DefaultAutoApplyBreakRateThresholdBPS; got != want {
		t.Errorf("BreakRateThresholdBPS = %d, want %d", got, want)
	}
	if got, want := cfg.BreakRateMinSample, DefaultAutoApplyBreakRateMinSample; got != want {
		t.Errorf("BreakRateMinSample = %d, want %d", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	if got := cfg.WithDefaults(); len(got.DailyMaxValueMinor) != len(wantCaps) {
		t.Errorf("WithDefaults() added currencies: %#v", got.DailyMaxValueMinor)
	}

	// The returned map must be independent of the package defaults.
	cfg.DailyMaxValueMinor["USD"] = 1
	if got := DefaultAutoApplyLimitsConfig().DailyMaxValueMinor["USD"]; got != DefaultAutoApplyDailyCapUSD {
		t.Fatalf("mutating a returned default changed the package table: USD = %d", got)
	}
}

func TestAutoApplyLimitsWithDefaults(t *testing.T) {
	t.Run("partial map fills missing currencies", func(t *testing.T) {
		in := AutoApplyLimitsConfig{
			DailyMaxValueMinor:    map[string]int64{"USD": 123, "GBP": 456},
			BreakRateThresholdBPS: 500,
			BreakRateMinSample:    7,
		}
		got := in.WithDefaults()
		if got.DailyMaxValueMinor["USD"] != 123 || got.DailyMaxValueMinor["GBP"] != 456 {
			t.Fatalf("explicit caps were not preserved: %#v", got.DailyMaxValueMinor)
		}
		if got.DailyMaxValueMinor["EUR"] != DefaultAutoApplyDailyCapEUR {
			t.Fatalf("EUR fallback = %d", got.DailyMaxValueMinor["EUR"])
		}
		if got.BreakRateThresholdBPS != 500 || got.BreakRateMinSample != 7 {
			t.Fatalf("explicit tuning was not preserved: %+v", got)
		}
		if in.DailyMaxValueMinor["EUR"] != 0 {
			t.Fatal("WithDefaults mutated its receiver map")
		}
	})

	t.Run("zero values take defaults", func(t *testing.T) {
		got := (AutoApplyLimitsConfig{}).WithDefaults()
		if got.BreakRateThresholdBPS != DefaultAutoApplyBreakRateThresholdBPS {
			t.Errorf("BreakRateThresholdBPS = %d, want default", got.BreakRateThresholdBPS)
		}
		if got.BreakRateMinSample != DefaultAutoApplyBreakRateMinSample {
			t.Errorf("BreakRateMinSample = %d, want default", got.BreakRateMinSample)
		}
		if len(got.DailyMaxValueMinor) != 4 {
			t.Errorf("DailyMaxValueMinor = %#v, want four built-ins", got.DailyMaxValueMinor)
		}
	})

	t.Run("explicit zero cap is preserved for Validate", func(t *testing.T) {
		got := (AutoApplyLimitsConfig{DailyMaxValueMinor: map[string]int64{"USD": 0}}).WithDefaults()
		if cap := got.DailyMaxValueMinor["USD"]; cap != 0 {
			t.Fatalf("USD cap = %d, want the explicit zero preserved", cap)
		}
		if err := got.Validate(); err == nil {
			t.Fatal("Validate accepted an explicit zero cap")
		}
	})
}

func TestAutoApplyLimitsValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     AutoApplyLimitsConfig
		wantErr string
	}{
		{"defaults", AutoApplyLimitsConfig{}, ""},
		{"threshold min", AutoApplyLimitsConfig{BreakRateThresholdBPS: 1}, ""},
		{"threshold max", AutoApplyLimitsConfig{BreakRateThresholdBPS: 10_000}, ""},
		{"threshold negative", AutoApplyLimitsConfig{BreakRateThresholdBPS: -1}, "between 1 and 10000"},
		{"threshold too large", AutoApplyLimitsConfig{BreakRateThresholdBPS: 10_001}, "between 1 and 10000"},
		{"min sample negative", AutoApplyLimitsConfig{BreakRateMinSample: -1}, "must be positive"},
		{"negative cap", AutoApplyLimitsConfig{DailyMaxValueMinor: map[string]int64{"USD": -1}}, "must be positive"},
		{"zero cap", AutoApplyLimitsConfig{DailyMaxValueMinor: map[string]int64{"USD": 0}}, "must be positive"},
		{"lower case key", AutoApplyLimitsConfig{DailyMaxValueMinor: map[string]int64{"usd": 1}}, "not an ISO 4217 code"},
		{"too short key", AutoApplyLimitsConfig{DailyMaxValueMinor: map[string]int64{"US": 1}}, "not an ISO 4217 code"},
		{"too long key", AutoApplyLimitsConfig{DailyMaxValueMinor: map[string]int64{"USDX": 1}}, "not an ISO 4217 code"},
		{"space key", AutoApplyLimitsConfig{DailyMaxValueMinor: map[string]int64{" USD": 1}}, "not an ISO 4217 code"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestAutoApplyLimitsCanonicalAndString(t *testing.T) {
	cfg := AutoApplyLimitsConfig{
		DailyMaxValueMinor:    map[string]int64{"USD": 2, "JPY": 0, "EUR": 1},
		BreakRateThresholdBPS: 0,
		BreakRateMinSample:    0,
	}
	if got, want := cfg.Canonical(), "break_rate_threshold_bps=1000;break_rate_min_sample=20;daily_max_value_minor.EUR=1;daily_max_value_minor.JPY=0;daily_max_value_minor.SEK=100000;daily_max_value_minor.USD=2;"; got != want {
		t.Fatalf("Canonical() = %q, want %q", got, want)
	}
	wantString := "AutoApplyLimitsConfig{DailyMaxValueMinor:{EUR:1,JPY:0,USD:2}, BreakRateThresholdBPS:0, BreakRateMinSample:0}"
	if got := cfg.String(); got != wantString {
		t.Fatalf("String() = %q, want %q", got, wantString)
	}
	if got, want := (AutoApplyLimitsConfig{}).String(), "AutoApplyLimitsConfig{DailyMaxValueMinor:nil, BreakRateThresholdBPS:0, BreakRateMinSample:0}"; got != want {
		t.Fatalf("zero String() = %q, want %q", got, want)
	}

	for i := 0; i < 100; i++ {
		if got := cfg.Canonical(); got != cfg.Canonical() {
			t.Fatalf("Canonical() is unstable: %q != %q", got, cfg.Canonical())
		}
	}
}

func writeLimitsYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadFileAutoApplyLimits(t *testing.T) {
	clearEnv(t)
	path := writeLimitsYAML(t, `
database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
auto_apply_limits:
  daily_max_value_minor:
    USD: 25
  break_rate_threshold_bps: 250
  break_rate_min_sample: 40
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := cfg.AutoApplyLimits.DailyMaxValueMinor["USD"]; got != 25 {
		t.Errorf("USD cap = %d, want 25", got)
	}
	if got, want := cfg.AutoApplyLimits.DailyMaxValueMinor["EUR"], DefaultAutoApplyDailyCapEUR; got != want {
		t.Errorf("EUR fallback = %d, want %d", got, want)
	}
	if got, want := cfg.AutoApplyLimits.BreakRateThresholdBPS, 250; got != want {
		t.Errorf("BreakRateThresholdBPS = %d, want %d", got, want)
	}
	if got, want := cfg.AutoApplyLimits.BreakRateMinSample, 40; got != want {
		t.Errorf("BreakRateMinSample = %d, want %d", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestLoadFileAutoApplyLimitsStrict(t *testing.T) {
	clearEnv(t)
	t.Run("unknown key", func(t *testing.T) {
		path := writeLimitsYAML(t, `
auto_apply_limits:
  daily_max_value_minor:
    USD: 10
  extra: nope
`)
		if _, err := LoadFile(path); err == nil {
			t.Fatal("LoadFile accepted an unknown auto_apply_limits key")
		} else if !strings.Contains(err.Error(), "extra") {
			t.Fatalf("LoadFile error = %v, want it to mention the unknown key", err)
		}
	})

	t.Run("invalid type", func(t *testing.T) {
		path := writeLimitsYAML(t, `
auto_apply_limits:
  daily_max_value_minor:
    USD: ten
`)
		if _, err := LoadFile(path); err == nil {
			t.Fatal("LoadFile accepted a non-integer cap")
		}
	})
}

func TestLoadDefaultsAutoApplyLimits(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.AutoApplyLimits.BreakRateThresholdBPS, DefaultAutoApplyBreakRateThresholdBPS; got != want {
		t.Errorf("BreakRateThresholdBPS = %d, want %d", got, want)
	}
	if got, want := cfg.AutoApplyLimits.DailyMaxValueMinor["SEK"], DefaultAutoApplyDailyCapSEK; got != want {
		t.Errorf("SEK cap = %d, want %d", got, want)
	}
}

func TestConfigStringIncludesAutoApplyLimits(t *testing.T) {
	cfg := defaults()
	cfg.Database.URL = "postgres://user:secret@localhost:5432/recon"
	cfg.LLM.APIKey = "super-secret-key"
	cfg.AutoApplyLimits = AutoApplyLimitsConfig{
		DailyMaxValueMinor:    map[string]int64{"USD": 12},
		BreakRateThresholdBPS: 750,
		BreakRateMinSample:    30,
	}
	text := cfg.String()
	want := "AutoApplyLimits:AutoApplyLimitsConfig{DailyMaxValueMinor:{USD:12}, BreakRateThresholdBPS:750, BreakRateMinSample:30}"
	if !strings.Contains(text, want) {
		t.Fatalf("Config.String = %q, want it to include %q", text, want)
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("Config.String leaked a secret: %q", text)
	}
}
