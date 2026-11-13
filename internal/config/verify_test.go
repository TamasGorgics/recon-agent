package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeVerifierYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestVerifierConfigDefaults(t *testing.T) {
	cfg := DefaultVerifierConfig()
	if got, want := cfg.ReferenceWindowDays, DefaultVerifierReferenceWindowDays; got != want {
		t.Errorf("ReferenceWindowDays = %d, want %d", got, want)
	}
	if cfg.RoundingCapMinor != nil {
		t.Errorf("RoundingCapMinor = %#v, want nil (per-currency fallback)", cfg.RoundingCapMinor)
	}
	if got, want := DefaultVerifierRoundingCapMinor, int64(1); got != want {
		t.Errorf("DefaultVerifierRoundingCapMinor = %d, want %d", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	if cfg.WithDefaults().ReferenceWindowDays != DefaultVerifierReferenceWindowDays {
		t.Errorf("WithDefaults changed a default window")
	}
}

func TestVerifierConfigWithDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   VerifierConfig
		want int
	}{
		{"zero window", VerifierConfig{}, DefaultVerifierReferenceWindowDays},
		{"negative window preserved", VerifierConfig{ReferenceWindowDays: -1}, -1},
		{"explicit window preserved", VerifierConfig{ReferenceWindowDays: 14}, 14},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.in.WithDefaults()
			if got.ReferenceWindowDays != test.want {
				t.Fatalf("ReferenceWindowDays = %d, want %d", got.ReferenceWindowDays, test.want)
			}
			if (test.in.RoundingCapMinor == nil) != (got.RoundingCapMinor == nil) {
				t.Fatalf("WithDefaults changed nil-ness of RoundingCapMinor: %#v -> %#v", test.in.RoundingCapMinor, got.RoundingCapMinor)
			}
		})
	}

	t.Run("nil and empty maps stay distinct", func(t *testing.T) {
		empty := VerifierConfig{RoundingCapMinor: map[string]int64{}}
		got := empty.WithDefaults()
		if got.RoundingCapMinor == nil {
			t.Fatal("WithDefaults converted an explicit empty map to nil")
		}
		if len(got.RoundingCapMinor) != 0 {
			t.Fatalf("WithDefaults populated an explicitly empty map: %#v", got.RoundingCapMinor)
		}
	})
}

func TestVerifierConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     VerifierConfig
		wantErr string
	}{
		{"defaults", VerifierConfig{}, ""},
		{"window zero defaults", VerifierConfig{ReferenceWindowDays: 0}, ""},
		{"window min", VerifierConfig{ReferenceWindowDays: 1}, ""},
		{"window max", VerifierConfig{ReferenceWindowDays: 365}, ""},
		{"window negative", VerifierConfig{ReferenceWindowDays: -1}, "between 1 and 365"},
		{"window too large", VerifierConfig{ReferenceWindowDays: 366}, "between 1 and 365"},
		{"nil caps", VerifierConfig{RoundingCapMinor: nil}, ""},
		{"empty caps", VerifierConfig{RoundingCapMinor: map[string]int64{}}, ""},
		{"zero cap", VerifierConfig{RoundingCapMinor: map[string]int64{"USD": 0}}, ""},
		{"multiple caps", VerifierConfig{RoundingCapMinor: map[string]int64{"USD": 1, "JPY": 0, "KWD": 3}}, ""},
		{"negative cap", VerifierConfig{RoundingCapMinor: map[string]int64{"USD": -1}}, "is negative"},
		{"lower case key", VerifierConfig{RoundingCapMinor: map[string]int64{"usd": 1}}, "not an ISO 4217 code"},
		{"too short key", VerifierConfig{RoundingCapMinor: map[string]int64{"US": 1}}, "not an ISO 4217 code"},
		{"too long key", VerifierConfig{RoundingCapMinor: map[string]int64{"USDX": 1}}, "not an ISO 4217 code"},
		{"space key", VerifierConfig{RoundingCapMinor: map[string]int64{" USD": 1}}, "not an ISO 4217 code"},
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

func TestVerifierConfigCanonicalAndString(t *testing.T) {
	cfg := VerifierConfig{
		RoundingCapMinor:    map[string]int64{"USD": 2, "JPY": 0, "EUR": 1},
		ReferenceWindowDays: 0,
	}
	if got, want := cfg.Canonical(), "reference_window_days=3;rounding_cap_minor.EUR=1;rounding_cap_minor.JPY=0;rounding_cap_minor.USD=2;"; got != want {
		t.Fatalf("Canonical() = %q, want %q", got, want)
	}
	if got, want := cfg.String(), "VerifierConfig{RoundingCapMinor:{EUR:1,JPY:0,USD:2}, ReferenceWindowDays:0}"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got, want := (VerifierConfig{}).String(), "VerifierConfig{RoundingCapMinor:nil, ReferenceWindowDays:0}"; got != want {
		t.Fatalf("zero String() = %q, want %q", got, want)
	}

	// Canonical must not depend on map iteration order.
	for i := 0; i < 100; i++ {
		if got := cfg.Canonical(); got != cfg.Canonical() {
			t.Fatalf("Canonical() is unstable: %q != %q", got, cfg.Canonical())
		}
	}
}

func TestLoadFileVerifierConfig(t *testing.T) {
	clearEnv(t)
	path := writeVerifierYAML(t, `
database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
verifier:
  rounding_cap_minor:
    USD: 2
    JPY: 0
  reference_window_days: 7
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Verifier.ReferenceWindowDays, 7; got != want {
		t.Errorf("Verifier.ReferenceWindowDays = %d, want %d", got, want)
	}
	if got, want := cfg.Verifier.RoundingCapMinor["USD"], int64(2); got != want {
		t.Errorf("Verifier.RoundingCapMinor[USD] = %d, want %d", got, want)
	}
	if got, want := cfg.Verifier.RoundingCapMinor["JPY"], int64(0); got != want {
		t.Errorf("Verifier.RoundingCapMinor[JPY] = %d, want %d", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestLoadFileVerifierZeroWindowUsesDefault(t *testing.T) {
	clearEnv(t)
	path := writeVerifierYAML(t, `
verifier:
  reference_window_days: 0
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Verifier.ReferenceWindowDays, DefaultVerifierReferenceWindowDays; got != want {
		t.Errorf("Verifier.ReferenceWindowDays = %d, want %d", got, want)
	}
}

func TestLoadFileVerifierUnknownKeyRejected(t *testing.T) {
	clearEnv(t)
	path := writeVerifierYAML(t, `
verifier:
  rounding_cap_minor:
    USD: 1
  extra: nope
`)
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted an unknown verifier key")
	} else if !strings.Contains(err.Error(), "extra") {
		t.Fatalf("LoadFile error = %v, want it to mention the unknown key", err)
	}
}

func TestLoadFileVerifierInvalidTypeRejected(t *testing.T) {
	clearEnv(t)
	path := writeVerifierYAML(t, `
verifier:
  rounding_cap_minor:
    USD: one
`)
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted a non-integer cap")
	}
}

func TestLoadUsesConfigFileForVerifier(t *testing.T) {
	clearEnv(t)
	path := writeVerifierYAML(t, `
verifier:
  rounding_cap_minor:
    EUR: 3
  reference_window_days: 9
`)
	t.Setenv(EnvConfigFile, path)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Verifier.RoundingCapMinor["EUR"], int64(3); got != want {
		t.Errorf("Verifier.RoundingCapMinor[EUR] = %d, want %d", got, want)
	}
	if got, want := cfg.Verifier.ReferenceWindowDays, 9; got != want {
		t.Errorf("Verifier.ReferenceWindowDays = %d, want %d", got, want)
	}
}

func TestConfigStringIncludesVerifierButNoSecrets(t *testing.T) {
	cfg := defaults()
	cfg.Database.URL = "postgres://user:secret@localhost:5432/recon"
	cfg.LLM.APIKey = "super-secret-key"
	cfg.Verifier = VerifierConfig{
		RoundingCapMinor:    map[string]int64{"USD": 2},
		ReferenceWindowDays: 5,
	}
	text := cfg.String()
	if !strings.Contains(text, "Verifier:VerifierConfig{RoundingCapMinor:{USD:2}, ReferenceWindowDays:5}") {
		t.Fatalf("Config.String = %q, want it to include the verifier block", text)
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("Config.String leaked a secret: %q", text)
	}
}

func TestLoadDefaultsVerifier(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Verifier.ReferenceWindowDays, DefaultVerifierReferenceWindowDays; got != want {
		t.Errorf("Verifier.ReferenceWindowDays = %d, want %d", got, want)
	}
	if cfg.Verifier.RoundingCapMinor != nil {
		t.Errorf("Verifier.RoundingCapMinor = %#v, want nil", cfg.Verifier.RoundingCapMinor)
	}
}
