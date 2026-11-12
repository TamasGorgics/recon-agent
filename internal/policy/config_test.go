package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// committedPolicyFile is the path to the repository-root policy document from
// this package's test working directory.
const committedPolicyFile = "../../policy.yaml"

const validPolicyDoc = `schema_version: 1

auto_apply:
  max_value_minor:
    USD: 500
    EUR: 500
    SEK: 5000
    JPY: 500
  require_verifier: true
  allowed_types: [link, adjustment]
  allowed_patterns: [fee_variance, rounding, fx_rounding, reference_typo]

confidence:
  downgrade_below: 0.8

single_approval:
  max_value_minor:
    USD: 50000
    EUR: 50000
    SEK: 500000
    JPY: 50000

write_off:
  max_value_minor:
    USD: 500
    EUR: 500
    SEK: 5000
    JPY: 500

proposal_ttl_hours: 48
four_eyes: true
`

func replaceOnce(t *testing.T, doc, old, replacement string) string {
	t.Helper()
	if !strings.Contains(doc, old) {
		t.Fatalf("test policy document does not contain %q", old)
	}
	return strings.Replace(doc, old, replacement, 1)
}

// TestLoadCommittedPolicy pins the committed defaults from the policy
// contract. The root policy.yaml is the source of truth for production.
//
// covers: FIN-13, FIN-34
func TestLoadCommittedPolicy(t *testing.T) {
	raw, err := os.ReadFile(committedPolicyFile)
	if err != nil {
		t.Fatalf("read committed policy: %v", err)
	}
	loaded, err := LoadFile(committedPolicyFile)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := loaded.Hash; got != HashYAML(raw) {
		t.Errorf("Hash = %q, want sha256 of the committed bytes", got)
	}
	if len(loaded.Hash) != 64 || strings.ToLower(loaded.Hash) != loaded.Hash {
		t.Fatalf("Hash = %q, want 64 lowercase hex", loaded.Hash)
	}
	if string(loaded.YAML) != string(raw) {
		t.Fatal("LoadedConfig.YAML differs from the file bytes")
	}

	cfg := loaded.Config
	if cfg.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", cfg.SchemaVersion)
	}
	wantAuto := map[string]int64{"USD": 500, "EUR": 500, "SEK": 5000, "JPY": 500}
	wantSingle := map[string]int64{"USD": 50000, "EUR": 50000, "SEK": 500000, "JPY": 50000}
	wantWriteOff := map[string]int64{"USD": 500, "EUR": 500, "SEK": 5000, "JPY": 500}
	assertCaps(t, "auto_apply", cfg.AutoApply.MaxValueMinor, wantAuto)
	assertCaps(t, "single_approval", cfg.SingleApproval.MaxValueMinor, wantSingle)
	assertCaps(t, "write_off", cfg.WriteOff.MaxValueMinor, wantWriteOff)
	if !cfg.AutoApply.RequireVerifier {
		t.Error("AutoApply.RequireVerifier = false, want true")
	}
	if got, want := strings.Join(cfg.AutoApply.AllowedTypes, ","), "link,adjustment"; got != want {
		t.Errorf("AllowedTypes = %q, want %q", got, want)
	}
	if got, want := strings.Join(cfg.AutoApply.AllowedPatterns, ","), "fee_variance,rounding,fx_rounding,reference_typo"; got != want {
		t.Errorf("AllowedPatterns = %q, want %q", got, want)
	}
	if got := cfg.Confidence.DowngradeBelow.String(); got != "0.8" {
		t.Errorf("Confidence.DowngradeBelow = %q, want 0.8", got)
	}
	if got := cfg.ProposalTTL(); got != 48*time.Hour {
		t.Errorf("ProposalTTL = %s, want 48h", got)
	}
	if !cfg.FourEyes {
		t.Error("FourEyes = false, want true")
	}
}

func assertCaps(t *testing.T, name string, got, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d currencies, want %d", name, len(got), len(want))
	}
	for currency, wantCap := range want {
		if gotCap, ok := got[currency]; !ok || gotCap != wantCap {
			t.Errorf("%s[%s] = %d (present %t), want %d", name, currency, gotCap, ok, wantCap)
		}
	}
}

// covers: FIN-13, FIN-34
func TestLoadInvalidDocuments(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{"unknown top-level key", validPolicyDoc + "unknown_key: true\n"},
		{"unknown nested key", replaceOnce(t, validPolicyDoc, "  require_verifier: true\n", "  require_verifier: true\n  unknown_gate: true\n")},
		{"duplicate key", replaceOnce(t, validPolicyDoc, "schema_version: 1\n", "schema_version: 1\nschema_version: 1\n")},
		{"duplicate nested key", replaceOnce(t, validPolicyDoc, "    USD: 500\n", "    USD: 500\n    USD: 500\n")},
		{"trailing document", validPolicyDoc + "---\nschema_version: 1\n"},
		{"missing schema_version", replaceOnce(t, validPolicyDoc, "schema_version: 1\n\n", "")},
		{"schema_version 2", replaceOnce(t, validPolicyDoc, "schema_version: 1", "schema_version: 2")},
		{"unknown allowed type", replaceOnce(t, validPolicyDoc, "[link, adjustment]", "[link, bogus]")},
		{"duplicate allowed type", replaceOnce(t, validPolicyDoc, "[link, adjustment]", "[link, link]")},
		{"request_info allowed", replaceOnce(t, validPolicyDoc, "[link, adjustment]", "[link, request_info]")},
		{"empty allowed types", replaceOnce(t, validPolicyDoc, "[link, adjustment]", "[]")},
		{"unknown allowed pattern", replaceOnce(t, validPolicyDoc, "[fee_variance, rounding, fx_rounding, reference_typo]", "[fee_variance, bogus]")},
		{"other allowed pattern", replaceOnce(t, validPolicyDoc, "[fee_variance, rounding, fx_rounding, reference_typo]", "[fee_variance, other]")},
		{"duplicate allowed pattern", replaceOnce(t, validPolicyDoc, "[fee_variance, rounding, fx_rounding, reference_typo]", "[fee_variance, fee_variance]")},
		{"empty allowed patterns", replaceOnce(t, validPolicyDoc, "[fee_variance, rounding, fx_rounding, reference_typo]", "[]")},
		{"require_verifier false", replaceOnce(t, validPolicyDoc, "require_verifier: true", "require_verifier: false")},
		{"four_eyes false", replaceOnce(t, validPolicyDoc, "four_eyes: true", "four_eyes: false")},
		{"missing four_eyes", replaceOnce(t, validPolicyDoc, "four_eyes: true\n", "")},
		{"zero cap", replaceOnce(t, validPolicyDoc, "    USD: 50000", "    USD: 0")},
		{"negative cap", replaceOnce(t, validPolicyDoc, "    EUR: 50000", "    EUR: -1")},
		{"mismatched currency sets", replaceOnce(t, validPolicyDoc, "    EUR: 500\n", "")},
		{"malformed currency key", replaceOnce(t, validPolicyDoc, "    JPY: 50000", "    jpy: 50000")},
		{"downgrade below zero", replaceOnce(t, validPolicyDoc, "downgrade_below: 0.8", "downgrade_below: 0")},
		{"negative downgrade", replaceOnce(t, validPolicyDoc, "downgrade_below: 0.8", "downgrade_below: -0.1")},
		{"downgrade above one", replaceOnce(t, validPolicyDoc, "downgrade_below: 0.8", "downgrade_below: 1.1")},
		{"non-scalar downgrade", replaceOnce(t, validPolicyDoc, "downgrade_below: 0.8", "downgrade_below: [0.8]")},
		{"ttl zero", replaceOnce(t, validPolicyDoc, "proposal_ttl_hours: 48", "proposal_ttl_hours: 0")},
		{"ttl negative", replaceOnce(t, validPolicyDoc, "proposal_ttl_hours: 48", "proposal_ttl_hours: -1")},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.doc))
			if err == nil {
				t.Fatal("LoadBytes accepted an invalid policy document")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// covers: FIN-34
func TestLoadEmptyDocuments(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":        "",
		"whitespace":   " \n\t\n",
		"comment only": "# no policy here\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadBytes([]byte(doc)); !errors.Is(err, ErrEmptyConfig) {
				t.Fatalf("LoadBytes error = %v, want ErrEmptyConfig", err)
			}
		})
	}
}

// covers: FIN-34
func TestHashYAMLContentAddressing(t *testing.T) {
	raw := []byte(validPolicyDoc)
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])
	if got := HashYAML(raw); got != want {
		t.Fatalf("HashYAML = %q, want %q", got, want)
	}
	if HashYAML(raw) != HashYAML(append([]byte(nil), raw...)) {
		t.Fatal("HashYAML is not deterministic for identical bytes")
	}

	variants := map[string][]byte{
		"comment":    []byte(validPolicyDoc + "# a comment is content\n"),
		"whitespace": []byte(validPolicyDoc + "\n"),
		"crlf":       []byte(strings.ReplaceAll(validPolicyDoc, "\n", "\r\n")),
	}
	for name, variant := range variants {
		if got := HashYAML(variant); got == want {
			t.Errorf("%s change did not change the hash", name)
		}
	}

	loaded, err := LoadBytes(raw)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if loaded.Hash != want {
		t.Fatalf("loaded hash = %q, want %q", loaded.Hash, want)
	}
}

// covers: FIN-13, FIN-34
func TestConfigAccessors(t *testing.T) {
	loaded, err := LoadFile(committedPolicyFile)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	engine, err := NewEngine(loaded, Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	for currency, want := range map[string]int64{"USD": 500, "EUR": 500, "SEK": 5000, "JPY": 500} {
		if got, ok := engine.AutoApplyCap(currency); !ok || got != want {
			t.Errorf("AutoApplyCap(%s) = %d/%t, want %d/true", currency, got, ok, want)
		}
	}
	for currency, want := range map[string]int64{"USD": 50000, "EUR": 50000, "SEK": 500000, "JPY": 50000} {
		if got, ok := engine.SingleApprovalCap(currency); !ok || got != want {
			t.Errorf("SingleApprovalCap(%s) = %d/%t, want %d/true", currency, got, ok, want)
		}
	}
	for currency, want := range map[string]int64{"USD": 500, "EUR": 500, "SEK": 5000, "JPY": 500} {
		if got, ok := engine.WriteOffCap(currency); !ok || got != want {
			t.Errorf("WriteOffCap(%s) = %d/%t, want %d/true", currency, got, ok, want)
		}
	}
	for _, currency := range []string{"usd", "GBP", ""} {
		if _, ok := engine.AutoApplyCap(currency); ok {
			t.Errorf("AutoApplyCap(%q) = true, want exact-case miss", currency)
		}
	}
	if got := engine.ProposalTTL(); got != 48*time.Hour {
		t.Errorf("ProposalTTL = %s, want 48h", got)
	}
	if got := engine.ConfigHash(); got != loaded.Hash {
		t.Errorf("ConfigHash = %q, want %q", got, loaded.Hash)
	}
}

// covers: FIN-34
func TestConfigStringDeterministic(t *testing.T) {
	loaded, err := LoadBytes([]byte(validPolicyDoc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	got := loaded.Config.String()
	for _, want := range []string{
		"schema_version:1",
		"{EUR:500,JPY:500,SEK:5000,USD:500}",
		"downgrade_below:0.8",
		"proposal_ttl_hours:48",
		"four_eyes:true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Config.String() = %q, want it to contain %q", got, want)
		}
	}
}

func TestLoadFileMissing(t *testing.T) {
	if _, err := LoadFile("testdata/does-not-exist.yaml"); err == nil {
		t.Fatal("LoadFile accepted a missing file")
	}
}

func TestNewEngineRejectsInconsistentLoadedConfig(t *testing.T) {
	loaded, err := LoadBytes([]byte(validPolicyDoc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	badHash := loaded
	badHash.Hash = strings.Repeat("f", 64)
	if _, err := NewEngine(badHash, Options{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewEngine mismatch error = %v, want ErrInvalidConfig", err)
	}

	tampered := loaded
	tampered.Config.FourEyes = false
	if _, err := NewEngine(tampered, Options{}); !errors.Is(err, ErrConfigMismatch) {
		t.Fatalf("NewEngine tampered config error = %v, want ErrConfigMismatch", err)
	}
}

// TestNewEngineRequiresConfigToEqualParsedYAML is the FIN-34 freeze boundary:
// a valid Config that is not exactly the document named by the hash must never
// reach a decision.
func TestNewEngineRequiresConfigToEqualParsedYAML(t *testing.T) {
	loaded, err := LoadBytes([]byte(validPolicyDoc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if _, err := NewEngine(loaded, Options{}); err != nil {
		t.Fatalf("NewEngine(consistent) error = %v", err)
	}

	tests := []struct {
		name   string
		loaded func() LoadedConfig
	}{
		{
			name: "different cap map",
			loaded: func() LoadedConfig {
				drift := loaded
				drift.Config.AutoApply.MaxValueMinor = map[string]int64{"USD": 100000, "EUR": 500, "SEK": 5000, "JPY": 500}
				return drift
			},
		},
		{
			name: "different TTL",
			loaded: func() LoadedConfig {
				drift := loaded
				drift.Config.ProposalTTLHours = 24
				return drift
			},
		},
		{
			name: "different allowed types slice",
			loaded: func() LoadedConfig {
				drift := loaded
				drift.Config.AutoApply.AllowedTypes = []string{"link"}
				return drift
			},
		},
		{
			name: "matching yaml is a different document",
			loaded: func() LoadedConfig {
				other, err := LoadBytes([]byte(replaceOnce(t, validPolicyDoc, "proposal_ttl_hours: 48", "proposal_ttl_hours: 24")))
				if err != nil {
					t.Fatalf("LoadBytes other: %v", err)
				}
				return LoadedConfig{Config: loaded.Config, Hash: other.Hash, YAML: other.YAML}
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewEngine(tc.loaded(), Options{}); !errors.Is(err, ErrConfigMismatch) {
				t.Fatalf("NewEngine error = %v, want ErrConfigMismatch", err)
			}
		})
	}

	// A hash can address bytes that are not a policy document at all; the
	// Config supplied alongside must not be trusted.
	fakeYAML := []byte("# comment only, not a policy\n")
	forged := LoadedConfig{Config: loaded.Config, Hash: HashYAML(fakeYAML), YAML: fakeYAML}
	if _, err := NewEngine(forged, Options{}); !errors.Is(err, ErrEmptyConfig) {
		t.Fatalf("NewEngine forged empty YAML error = %v, want ErrEmptyConfig", err)
	}
}

// TestProposalTTLOverflow pins the duration boundary so a large configured TTL
// can never wrap negative.
func TestProposalTTLOverflow(t *testing.T) {
	maxDoc := replaceOnce(t, validPolicyDoc, "proposal_ttl_hours: 48", "proposal_ttl_hours: "+strconv.FormatInt(maxTTLHours, 10))
	loaded, err := LoadBytes([]byte(maxDoc))
	if err != nil {
		t.Fatalf("LoadBytes(max) error = %v", err)
	}
	want := time.Duration(maxTTLHours) * time.Hour
	if got := loaded.Config.ProposalTTL(); got != want || got <= 0 {
		t.Fatalf("ProposalTTL(max) = %s, want positive %s", got, want)
	}

	for name, hours := range map[string]string{
		"max plus one": strconv.FormatInt(maxTTLHours+1, 10),
		"max int64":    strconv.FormatInt(math.MaxInt64, 10),
	} {
		doc := replaceOnce(t, validPolicyDoc, "proposal_ttl_hours: 48", "proposal_ttl_hours: "+hours)
		if _, err := LoadBytes([]byte(doc)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s: LoadBytes error = %v, want ErrInvalidConfig", name, err)
		}
	}

	// Directly constructed invalid Configs must never yield a negative TTL.
	for _, hours := range []int{-1, 0, int(maxTTLHours) + 1, math.MaxInt} {
		if got := (Config{ProposalTTLHours: hours}).ProposalTTL(); got != 0 {
			t.Errorf("Config{ProposalTTLHours:%d}.ProposalTTL() = %s, want 0", hours, got)
		}
	}
}

func TestEnsureVersionRejectsInvalidInput(t *testing.T) {
	loaded, err := LoadBytes([]byte(validPolicyDoc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	fixed := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	if err := loaded.EnsureVersion(t.Context(), nil, fixed); !errors.Is(err, ErrInvalidVersionInput) {
		t.Fatalf("nil tx error = %v, want ErrInvalidVersionInput", err)
	}
}
