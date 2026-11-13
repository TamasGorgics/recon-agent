package config

import (
	"fmt"
	"sort"
	"strings"
)

// Verifier tuning defaults. Verifier tuning is YAML-only: it has no
// environment binding and no secret value, so .env.example does not change
// for M4-05.
const (
	// DefaultVerifierRoundingCapMinor is the per-currency cap used when a
	// currency is absent from verifier.rounding_cap_minor. Zero is a valid
	// explicit cap (no rounding tolerance); nil and empty maps both fall back
	// to this value per currency.
	DefaultVerifierRoundingCapMinor int64 = 1
	// DefaultVerifierReferenceWindowDays is the inclusive absolute value-date
	// window used when verifier.reference_window_days is blank or zero.
	DefaultVerifierReferenceWindowDays = 3
	// MinVerifierReferenceWindowDays and MaxVerifierReferenceWindowDays bound
	// the reference window after defaults are applied.
	MinVerifierReferenceWindowDays = 1
	MaxVerifierReferenceWindowDays = 365
)

// VerifierConfig holds deterministic verifier tuning (M4-05). It is not a
// policy document: policy.yaml stays frozen, and the verifier block lives in
// the root CONFIG_FILE (or the built-in defaults).
type VerifierConfig struct {
	RoundingCapMinor    map[string]int64 `yaml:"rounding_cap_minor"`
	ReferenceWindowDays int              `yaml:"reference_window_days"`
}

// DefaultVerifierConfig returns the built-in verifier defaults. The cap map is
// deliberately nil: a nil map means the per-currency fallback applies, which
// keeps the defaults independent of the embedded currency table.
func DefaultVerifierConfig() VerifierConfig {
	return VerifierConfig{ReferenceWindowDays: DefaultVerifierReferenceWindowDays}
}

// WithDefaults fills a blank/zero reference window with its default. It keeps
// the rounding-cap map exactly as supplied: nil and empty maps both resolve to
// the per-currency fallback at lookup time, while an explicit entry (including
// zero) is preserved. Negative values are preserved for Validate to reject.
func (c VerifierConfig) WithDefaults() VerifierConfig {
	if c.ReferenceWindowDays == 0 {
		c.ReferenceWindowDays = DefaultVerifierReferenceWindowDays
	}
	return c
}

// Validate checks the verifier block. It applies defaults first, so a blank
// window is valid and resolves to 3; an explicitly invalid negative or
// out-of-range window is still rejected. Currency keys must be upper-case ISO
// 4217-shaped codes and caps must be non-negative.
func (c VerifierConfig) Validate() error {
	c = c.WithDefaults()
	for currency, cap := range c.RoundingCapMinor {
		if !currencyCodePattern.MatchString(currency) {
			return fmt.Errorf("config: verifier.rounding_cap_minor: %q is not an ISO 4217 code", currency)
		}
		if cap < 0 {
			return fmt.Errorf("config: verifier.rounding_cap_minor.%s: %d is negative", currency, cap)
		}
	}
	if c.ReferenceWindowDays < MinVerifierReferenceWindowDays || c.ReferenceWindowDays > MaxVerifierReferenceWindowDays {
		return fmt.Errorf("config: verifier.reference_window_days must be between %d and %d, got %d",
			MinVerifierReferenceWindowDays, MaxVerifierReferenceWindowDays, c.ReferenceWindowDays)
	}
	return nil
}

// Canonical renders the verifier block in a stable order. It is useful for
// diagnostics and for hashing a run configuration; no verifier table is
// keyed by it at M4-05.
func (c VerifierConfig) Canonical() string {
	c = c.WithDefaults()
	currencies := make([]string, 0, len(c.RoundingCapMinor))
	for currency := range c.RoundingCapMinor {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	var builder strings.Builder
	builder.WriteString("reference_window_days=")
	fmt.Fprintf(&builder, "%d;", c.ReferenceWindowDays)
	for _, currency := range currencies {
		fmt.Fprintf(&builder, "rounding_cap_minor.%s=%d;", currency, c.RoundingCapMinor[currency])
	}
	return builder.String()
}

// String renders the verifier block with sorted currency keys so Config.String
// is stable across map iteration order.
func (c VerifierConfig) String() string {
	currencies := make([]string, 0, len(c.RoundingCapMinor))
	for currency := range c.RoundingCapMinor {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	parts := make([]string, 0, len(currencies))
	for _, currency := range currencies {
		parts = append(parts, fmt.Sprintf("%s:%d", currency, c.RoundingCapMinor[currency]))
	}
	capText := "nil"
	if c.RoundingCapMinor != nil {
		capText = "{" + strings.Join(parts, ",") + "}"
	}
	return fmt.Sprintf("VerifierConfig{RoundingCapMinor:%s, ReferenceWindowDays:%d}", capText, c.ReferenceWindowDays)
}
