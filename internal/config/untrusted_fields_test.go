package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIngestUntrustedFieldsConfig proves the per-source config can declare the
// free-text columns that M1-08 keeps in raw for spec section 9 handling.
func TestIngestUntrustedFieldsConfig(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "ingest:\n" +
		"  sources:\n" +
		"    psp_a:\n" +
		"      timezone: UTC\n" +
		"      cutoff: \"17:00\"\n" +
		"      untrusted_fields: [memo]\n" +
		"    bank_x:\n" +
		"      timezone: UTC\n" +
		"      cutoff: \"15:30\"\n" +
		"      untrusted_fields: [description]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if err := cfg.Ingest.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	psp := cfg.Ingest.Sources["psp_a"]
	if got, want := strings.Join(psp.UntrustedFields, ","), "memo"; got != want {
		t.Errorf("psp_a untrusted fields = %q, want %q", got, want)
	}
	bank := cfg.Ingest.Sources["bank_x"]
	if got, want := strings.Join(bank.UntrustedFields, ","), "description"; got != want {
		t.Errorf("bank_x untrusted fields = %q, want %q", got, want)
	}
	pspFields, err := psp.UntrustedFieldSet()
	if err != nil {
		t.Fatalf("psp_a UntrustedFieldSet: %v", err)
	}
	if _, ok := pspFields["memo"]; !ok {
		t.Errorf("psp_a field set = %v, want memo", pspFields)
	}
	bankFields, err := bank.UntrustedFieldSet()
	if err != nil {
		t.Fatalf("bank_x UntrustedFieldSet: %v", err)
	}
	if _, ok := bankFields["description"]; !ok {
		t.Errorf("bank_x field set = %v, want description", bankFields)
	}
}

// TestConfiguredUntrustedFields pins the projection both cmd/worker and the
// agent CLI/agentrun pass to the production runner.
func TestConfiguredUntrustedFields(t *testing.T) {
	cfg := Config{Ingest: IngestConfig{Sources: map[string]SourceConfig{
		"psp_a":  {Timezone: "UTC", Cutoff: "17:00", UntrustedFields: []string{"memo"}},
		"bank_x": {Timezone: "UTC", Cutoff: "15:30"},
		"psp_m2": {Timezone: "UTC", Cutoff: "00:00", UntrustedFields: []string{"memo", "reference"}},
	}}}
	got := cfg.ConfiguredUntrustedFields()
	if fields := strings.Join(got["psp_a"], ","); fields != "memo" {
		t.Errorf("psp_a fields = %q, want memo", fields)
	}
	if _, ok := got["bank_x"]; ok {
		t.Errorf("bank_x with no untrusted fields was projected: %v", got["bank_x"])
	}
	if fields := strings.Join(got["psp_m2"], ","); fields != "memo,reference" {
		t.Errorf("psp_m2 fields = %q, want memo,reference", fields)
	}
	got["psp_a"][0] = "mutated"
	if cfg.Ingest.Sources["psp_a"].UntrustedFields[0] != "memo" {
		t.Fatal("ConfiguredUntrustedFields aliases the config slice")
	}
	if fields := (Config{}).ConfiguredUntrustedFields(); fields != nil {
		t.Errorf("empty config fields = %v, want nil", fields)
	}
}

// TestDefaultUntrustedFieldsIncludesPSPM2 pins the eval-only fallback used when
// no ingest config names psp_m2: its memo and reference must be labelled.
func TestDefaultUntrustedFieldsIncludesPSPM2(t *testing.T) {
	fields := DefaultUntrustedFields()
	if got := strings.Join(fields["psp_m2"], ","); got != "memo,reference" {
		t.Fatalf("psp_m2 default fields = %q, want memo,reference", got)
	}
	if got := strings.Join(fields["psp_a"], ","); got != "memo" {
		t.Errorf("psp_a default fields = %q, want memo", got)
	}
	if got := strings.Join(fields["bank_x"], ","); got != "description" {
		t.Errorf("bank_x default fields = %q, want description", got)
	}
}

func TestUntrustedFieldSetValidation(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   string
	}{
		{"empty list", nil, ""},
		{"valid", []string{"memo", "reference_note"}, ""},
		{"empty name", []string{""}, "empty"},
		{"whitespace only", []string{"  "}, "empty"},
		{"surrounding whitespace", []string{" memo"}, "whitespace"},
		{"duplicate", []string{"memo", "memo"}, "duplicated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := SourceConfig{
				Timezone:        "UTC",
				Cutoff:          "17:00",
				UntrustedFields: tc.fields,
			}
			_, err := source.UntrustedFieldSet()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("UntrustedFieldSet(%v) = %v, want nil", tc.fields, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("UntrustedFieldSet(%v) accepted invalid fields", tc.fields)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("UntrustedFieldSet(%v) error = %v, want %q", tc.fields, err, tc.want)
			}
		})
	}
}

// TestIngestValidateRejectsBadUntrustedFields checks Validate wires the field
// set through, so a bad config fails at startup rather than at ingest time.
func TestIngestValidateRejectsBadUntrustedFields(t *testing.T) {
	cfg := IngestConfig{Sources: map[string]SourceConfig{
		"psp_a": {Timezone: "UTC", Cutoff: "17:00", UntrustedFields: []string{"memo", "memo"}},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted duplicate untrusted fields")
	}
	if !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("Validate error = %v, want it to mention untrusted fields", err)
	}
}
