package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

// Typed loader and persistence errors. Callers fail closed when any of these
// is returned.
var (
	// ErrInvalidConfig reports a policy document that does not satisfy the
	// strict schema or the locked validation rules.
	ErrInvalidConfig = errors.New("policy: invalid config")
	// ErrEmptyConfig reports an empty or comment-only policy document.
	ErrEmptyConfig = errors.New("policy: empty config")
	// ErrConfigHashMismatch reports that the stored YAML for a hash differs
	// from the bytes being loaded. The stored row is never overwritten.
	ErrConfigHashMismatch = errors.New("policy: config hash mismatch")
	// ErrConfigVersionMissing reports that a hash exists but its version row
	// cannot be read back after an insert conflict.
	ErrConfigVersionMissing = errors.New("policy: config version missing")
	// ErrConfigMismatch reports that LoadedConfig.Config is not exactly the
	// document parsed from LoadedConfig.YAML. NewEngine never trusts a Config
	// supplied alongside bytes it did not parse itself.
	ErrConfigMismatch = errors.New("policy: config does not match yaml")
)

// SchemaVersion is the only supported policy document schema version.
const SchemaVersion = 1

// PolicyYAML is the filename of the committed policy document.
const PolicyYAML = "policy.yaml"

// Known proposal types and verifier patterns. The policy loader restricts the
// configurable sets to these closed vocabularies.
const (
	TypeLink        ProposalType = "link"
	TypeAdjustment  ProposalType = "adjustment"
	TypeWriteOff    ProposalType = "write_off"
	TypeDuplicate   ProposalType = "duplicate"
	TypeRequestInfo ProposalType = "request_info"
)

// Pattern names produced by deterministic verifiers (M4-05). "other" is known
// but can never be auto-applied.
const (
	PatternFeeVariance   = "fee_variance"
	PatternRounding      = "rounding"
	PatternFXRounding    = "fx_rounding"
	PatternReferenceTypo = "reference_typo"
	PatternOther         = "other"
)

// Threshold is an exact decimal YAML scalar. Floats are never accepted into a
// float64: the scalar text is parsed by shopspring/decimal, so 0.8 is exact.
type Threshold struct {
	value decimal.Decimal
}

// UnmarshalYAML parses one scalar decimal exactly. Non-scalar values and
// values that are not finite decimals are rejected.
func (t *Threshold) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode {
		return errors.New("threshold must be a decimal scalar")
	}
	text := strings.TrimSpace(node.Value)
	if text == "" {
		return errors.New("threshold must not be empty")
	}
	value, err := decimal.NewFromString(text)
	if err != nil {
		return fmt.Errorf("threshold %q is not a decimal: %w", text, err)
	}
	t.value = value
	return nil
}

// Value returns the exact decimal value.
func (t Threshold) Value() decimal.Decimal { return t.value }

// String renders the exact decimal value.
func (t Threshold) String() string { return t.value.String() }

// AutoApplyConfig is the T0 gate policy.
type AutoApplyConfig struct {
	MaxValueMinor   map[string]int64 `yaml:"max_value_minor"`
	RequireVerifier bool             `yaml:"require_verifier"`
	AllowedTypes    []string         `yaml:"allowed_types"`
	AllowedPatterns []string         `yaml:"allowed_patterns"`
}

// ConfidenceConfig controls the confidence downgrade.
type ConfidenceConfig struct {
	DowngradeBelow Threshold `yaml:"downgrade_below"`
}

// CapConfig is one per-currency minor-unit cap map.
type CapConfig struct {
	MaxValueMinor map[string]int64 `yaml:"max_value_minor"`
}

// Config is the parsed risk-policy document. All money values are literal
// minor units; no cap is ever multiplied or reinterpreted.
type Config struct {
	SchemaVersion    int              `yaml:"schema_version"`
	AutoApply        AutoApplyConfig  `yaml:"auto_apply"`
	Confidence       ConfidenceConfig `yaml:"confidence"`
	SingleApproval   CapConfig        `yaml:"single_approval"`
	WriteOff         CapConfig        `yaml:"write_off"`
	ProposalTTLHours int              `yaml:"proposal_ttl_hours"`
	FourEyes         bool             `yaml:"four_eyes"`
}

// cloneConfig deep-copies every map and slice. An Engine is built from a
// freshly parsed document and then stores this clone, so neither the loaded
// Config nor the caller's option slices can alias engine state.
func cloneConfig(c Config) Config {
	out := c
	out.AutoApply.MaxValueMinor = maps.Clone(c.AutoApply.MaxValueMinor)
	out.AutoApply.AllowedTypes = slices.Clone(c.AutoApply.AllowedTypes)
	out.AutoApply.AllowedPatterns = slices.Clone(c.AutoApply.AllowedPatterns)
	out.SingleApproval.MaxValueMinor = maps.Clone(c.SingleApproval.MaxValueMinor)
	out.WriteOff.MaxValueMinor = maps.Clone(c.WriteOff.MaxValueMinor)
	return out
}

// LoadedConfig is one policy document plus the exact bytes and SHA-256 that
// identify it. Hash is sha256(YAML) rendered lowercase hex.
type LoadedConfig struct {
	Config Config
	Hash   string
	YAML   []byte
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// LoadFile reads the policy document once, then parses and hashes those exact
// bytes through LoadBytes.
func LoadFile(path string) (LoadedConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return LoadedConfig{}, fmt.Errorf("policy: read %q: %w", path, err)
	}
	loaded, err := LoadBytes(raw)
	if err != nil {
		return LoadedConfig{}, fmt.Errorf("policy: load %q: %w", path, err)
	}
	return loaded, nil
}

// LoadBytes is the single parse path. It hashes the raw bytes first, then
// decodes one strict YAML document, then validates the policy. Comments,
// whitespace, and line endings are content and therefore part of the hash.
func LoadBytes(raw []byte) (LoadedConfig, error) {
	hash := HashYAML(raw)
	cfg, err := parseConfig(raw)
	if err != nil {
		return LoadedConfig{}, err
	}
	return LoadedConfig{
		Config: cfg,
		Hash:   hash,
		YAML:   append([]byte(nil), raw...),
	}, nil
}

// parseConfig is the one strict YAML decode path. NewEngine calls it again on
// the exact bytes that were hashed, so the engine can never apply a Config that
// differs from the document named by its hash.
func parseConfig(raw []byte) (Config, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Config{}, fmt.Errorf("%w: document is empty", ErrEmptyConfig)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("%w: document is empty", ErrEmptyConfig)
		}
		return Config{}, fmt.Errorf("%w: parse: %v", ErrInvalidConfig, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("%w: parse trailing document: %v", ErrInvalidConfig, err)
		}
		return Config{}, fmt.Errorf("%w: document must contain exactly one YAML document", ErrInvalidConfig)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return cfg, nil
}

// HashYAML returns sha256(raw) as lowercase hex. It never normalizes input.
func HashYAML(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Validate enforces the strict policy contract. Every rule fails closed: a
// malformed or ambiguous document must never produce an implicit default.
func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version %d, want %d", c.SchemaVersion, SchemaVersion)
	}
	if !c.AutoApply.RequireVerifier {
		return errors.New("auto_apply.require_verifier must be true")
	}
	if !c.FourEyes {
		return errors.New("four_eyes must be present and true")
	}
	if c.ProposalTTLHours <= 0 {
		return fmt.Errorf("proposal_ttl_hours %d, want > 0", c.ProposalTTLHours)
	}
	if int64(c.ProposalTTLHours) > maxTTLHours {
		return fmt.Errorf("proposal_ttl_hours %d overflows time.Duration (max %d hours)",
			c.ProposalTTLHours, maxTTLHours)
	}
	if err := validateTypes(c.AutoApply.AllowedTypes); err != nil {
		return err
	}
	if err := validatePatterns(c.AutoApply.AllowedPatterns); err != nil {
		return err
	}
	if err := validateCaps(c.AutoApply.MaxValueMinor, c.SingleApproval.MaxValueMinor, c.WriteOff.MaxValueMinor); err != nil {
		return err
	}
	threshold := c.Confidence.DowngradeBelow.Value()
	if !threshold.IsPositive() || threshold.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("confidence.downgrade_below %s, want (0, 1]", threshold.String())
	}
	return nil
}

func validateTypes(types []string) error {
	if len(types) == 0 {
		return errors.New("auto_apply.allowed_types must not be empty")
	}
	seen := make(map[string]struct{}, len(types))
	for _, proposalType := range types {
		switch ProposalType(proposalType) {
		case TypeLink, TypeAdjustment, TypeWriteOff, TypeDuplicate, TypeRequestInfo:
		default:
			return fmt.Errorf("auto_apply.allowed_types contains unknown type %q", proposalType)
		}
		if proposalType == string(TypeRequestInfo) {
			return errors.New("auto_apply.allowed_types must not contain request_info")
		}
		if _, ok := seen[proposalType]; ok {
			return fmt.Errorf("auto_apply.allowed_types contains duplicate %q", proposalType)
		}
		seen[proposalType] = struct{}{}
	}
	return nil
}

func validatePatterns(patterns []string) error {
	if len(patterns) == 0 {
		return errors.New("auto_apply.allowed_patterns must not be empty")
	}
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		switch pattern {
		case PatternFeeVariance, PatternRounding, PatternFXRounding, PatternReferenceTypo, PatternOther:
		default:
			return fmt.Errorf("auto_apply.allowed_patterns contains unknown pattern %q", pattern)
		}
		if pattern == PatternOther {
			return errors.New("auto_apply.allowed_patterns must not contain other")
		}
		if _, ok := seen[pattern]; ok {
			return fmt.Errorf("auto_apply.allowed_patterns contains duplicate %q", pattern)
		}
		seen[pattern] = struct{}{}
	}
	return nil
}

func validateCaps(auto, single, writeOff map[string]int64) error {
	if len(auto) == 0 {
		return errors.New("auto_apply.max_value_minor must not be empty")
	}
	for name, caps := range map[string]map[string]int64{
		"auto_apply.max_value_minor":      auto,
		"single_approval.max_value_minor": single,
		"write_off.max_value_minor":       writeOff,
	} {
		if len(caps) != len(auto) {
			return fmt.Errorf("%s has %d currencies, want %d (all cap maps share one currency set)",
				name, len(caps), len(auto))
		}
		for currency, cap := range caps {
			if !currencyPattern.MatchString(currency) {
				return fmt.Errorf("%s currency %q must match ^[A-Z]{3}$", name, currency)
			}
			if cap <= 0 {
				return fmt.Errorf("%s[%s] %d, want > 0", name, currency, cap)
			}
			if _, ok := auto[currency]; !ok {
				return fmt.Errorf("%s adds currency %q absent from auto_apply.max_value_minor", name, currency)
			}
		}
	}
	for currency := range auto {
		if !currencyPattern.MatchString(currency) {
			return fmt.Errorf("auto_apply.max_value_minor currency %q must match ^[A-Z]{3}$", currency)
		}
	}
	return nil
}

// maxTTLHours is the largest whole-hour TTL that does not overflow
// time.Duration. Validate rejects anything larger so a configured lifetime can
// never wrap negative.
const maxTTLHours int64 = math.MaxInt64 / int64(time.Hour)

// ProposalTTL returns the configured proposal lifetime. M4-04 never writes
// expires_at; M4-11 consumes this value. It returns zero rather than a negative
// duration for an invalid Config constructed outside LoadBytes/NewEngine.
func (c Config) ProposalTTL() time.Duration {
	hours := int64(c.ProposalTTLHours)
	if hours <= 0 || hours > maxTTLHours {
		return 0
	}
	return time.Duration(hours) * time.Hour
}

// String renders the non-secret policy surface with deterministic ordering.
func (c Config) String() string {
	return fmt.Sprintf(
		"Policy{schema_version:%d, auto_apply:{max_value_minor:%s, require_verifier:%t, allowed_types:%v, allowed_patterns:%v}, confidence:{downgrade_below:%s}, single_approval:{max_value_minor:%s}, write_off:{max_value_minor:%s}, proposal_ttl_hours:%d, four_eyes:%t}",
		c.SchemaVersion,
		renderCaps(c.AutoApply.MaxValueMinor),
		c.AutoApply.RequireVerifier,
		c.AutoApply.AllowedTypes,
		c.AutoApply.AllowedPatterns,
		c.Confidence.DowngradeBelow.String(),
		renderCaps(c.SingleApproval.MaxValueMinor),
		renderCaps(c.WriteOff.MaxValueMinor),
		c.ProposalTTLHours,
		c.FourEyes,
	)
}

func renderCaps(caps map[string]int64) string {
	currencies := make([]string, 0, len(caps))
	for currency := range caps {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	var b strings.Builder
	b.WriteByte('{')
	for i, currency := range currencies {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s:%d", currency, caps[currency])
	}
	b.WriteByte('}')
	return b.String()
}
