package config

import (
	"fmt"
	"regexp"
	"sort"
)

// MatchConfig holds the deterministic matcher's configuration (M1-12 passes 1
// and 2, M1-13 passes 3 to 5).
//
// FeeToleranceMinor is keyed by ISO 4217 currency and expressed in that
// currency's minor units. A nil map selects DefaultMatchConfig; an explicitly
// empty map means zero tolerance for every currency.
//
// Zero values for the M1-13 fields select the documented defaults (see
// WithDefaults); negative values are rejected by Validate.
type MatchConfig struct {
	FeeToleranceMinor map[string]int64 `yaml:"fee_tolerance_minor"`

	// CompositeMaxLegs caps the number of ledger legs in one composite match.
	CompositeMaxLegs int `yaml:"composite_max_legs"`
	// CompositeMaxNodes caps the subset-sum search nodes per external record.
	CompositeMaxNodes int `yaml:"composite_max_nodes"`
	// CompositeCandidateLimit caps the same-sign, in-window ledger pool that
	// subset-sum considers. A larger pool yields a break, never a guess.
	CompositeCandidateLimit int `yaml:"composite_candidate_limit"`
	// CompositeDateWindowDays bounds leg value dates around the external date.
	CompositeDateWindowDays int `yaml:"composite_date_window_days"`

	// FuzzyThresholdBPS is the minimum score (0..10000) for a fuzzy match.
	FuzzyThresholdBPS int `yaml:"fuzzy_threshold_bps"`
	// FuzzyDateWindowDays is the maximum value-date distance for fuzzy pairs.
	FuzzyDateWindowDays int `yaml:"fuzzy_date_window_days"`
}

// M1-13 default caps and scores. They are deliberately conservative: an
// unresolved record becomes a break instead of an arbitrary link.
const (
	DefaultCompositeMaxLegs        = 8
	DefaultCompositeMaxNodes       = 100_000
	DefaultCompositeCandidateLimit = 256
	DefaultCompositeDateWindowDays = 7
	DefaultFuzzyThresholdBPS       = 8_000
	DefaultFuzzyDateWindowDays     = 3
)

// DefaultFeeToleranceMinor is the matcher tolerance used when YAML does not
// override match.fee_tolerance_minor. The values are deliberately small: a
// tolerance match is a real match recorded with its delta, not a way to hide a
// material amount difference.
var DefaultFeeToleranceMinor = map[string]int64{
	"USD": 100,
	"EUR": 100,
	"GBP": 100,
	"SEK": 1_000,
}

var currencyCodePattern = regexp.MustCompile(`^[A-Z]{3}$`)

// DefaultMatchConfig returns a complete match configuration with fresh maps so
// callers cannot mutate the package defaults.
func DefaultMatchConfig() MatchConfig {
	return MatchConfig{}.WithDefaults()
}

// WithDefaults fills nil maps and zero-valued M1-13 caps with their defaults.
// Negative values are preserved for Validate to reject; an explicitly empty
// tolerance map stays empty (zero tolerance).
func (c MatchConfig) WithDefaults() MatchConfig {
	if c.FeeToleranceMinor == nil {
		c.FeeToleranceMinor = make(map[string]int64, len(DefaultFeeToleranceMinor))
		for currency, minor := range DefaultFeeToleranceMinor {
			c.FeeToleranceMinor[currency] = minor
		}
	}
	if c.CompositeMaxLegs == 0 {
		c.CompositeMaxLegs = DefaultCompositeMaxLegs
	}
	if c.CompositeMaxNodes == 0 {
		c.CompositeMaxNodes = DefaultCompositeMaxNodes
	}
	if c.CompositeCandidateLimit == 0 {
		c.CompositeCandidateLimit = DefaultCompositeCandidateLimit
	}
	if c.CompositeDateWindowDays == 0 {
		c.CompositeDateWindowDays = DefaultCompositeDateWindowDays
	}
	if c.FuzzyThresholdBPS == 0 {
		c.FuzzyThresholdBPS = DefaultFuzzyThresholdBPS
	}
	if c.FuzzyDateWindowDays == 0 {
		c.FuzzyDateWindowDays = DefaultFuzzyDateWindowDays
	}
	return c
}

// FeeTolerance returns the configured tolerance for currency, or zero when the
// currency is absent.
func (c MatchConfig) FeeTolerance(currency string) int64 {
	if c.FeeToleranceMinor == nil {
		return DefaultMatchConfig().FeeToleranceMinor[currency]
	}
	return c.FeeToleranceMinor[currency]
}

// Validate rejects negative tolerances/caps and malformed values.
func (c MatchConfig) Validate() error {
	for currency, minor := range c.FeeToleranceMinor {
		if !currencyCodePattern.MatchString(currency) {
			return fmt.Errorf("config: match.fee_tolerance_minor: %q is not an ISO 4217 code", currency)
		}
		if minor < 0 {
			return fmt.Errorf("config: match.fee_tolerance_minor.%s: %d is negative", currency, minor)
		}
	}
	for name, value := range map[string]int{
		"composite_max_legs":         c.CompositeMaxLegs,
		"composite_max_nodes":        c.CompositeMaxNodes,
		"composite_candidate_limit":  c.CompositeCandidateLimit,
		"composite_date_window_days": c.CompositeDateWindowDays,
		"fuzzy_threshold_bps":        c.FuzzyThresholdBPS,
		"fuzzy_date_window_days":     c.FuzzyDateWindowDays,
	} {
		if value < 0 {
			return fmt.Errorf("config: match.%s: %d is negative", name, value)
		}
	}
	if c.FuzzyThresholdBPS > 10_000 {
		return fmt.Errorf("config: match.fuzzy_threshold_bps: %d exceeds 10000", c.FuzzyThresholdBPS)
	}
	return nil
}

// Canonical renders the configuration in a stable order. The matcher hashes it
// into match_runs.config_hash.
func (c MatchConfig) Canonical() string {
	currencies := make([]string, 0, len(c.FeeToleranceMinor))
	for currency := range c.FeeToleranceMinor {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	canonical := ""
	for _, currency := range currencies {
		canonical += fmt.Sprintf("%s=%d;", currency, c.FeeToleranceMinor[currency])
	}
	canonical += fmt.Sprintf("composite_max_legs=%d;", c.CompositeMaxLegs)
	canonical += fmt.Sprintf("composite_max_nodes=%d;", c.CompositeMaxNodes)
	canonical += fmt.Sprintf("composite_candidate_limit=%d;", c.CompositeCandidateLimit)
	canonical += fmt.Sprintf("composite_date_window_days=%d;", c.CompositeDateWindowDays)
	canonical += fmt.Sprintf("fuzzy_threshold_bps=%d;", c.FuzzyThresholdBPS)
	canonical += fmt.Sprintf("fuzzy_date_window_days=%d;", c.FuzzyDateWindowDays)
	return canonical
}
