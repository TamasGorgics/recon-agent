package config

import (
	"fmt"
	"regexp"
	"sort"
)

// MatchConfig holds the deterministic matcher's fee tolerance per currency
// (M1-12). Pass 2 matches the same reference when the absolute amount delta is
// within tolerance; pass 1 (exact) still requires equal amounts.
//
// FeeToleranceMinor is keyed by ISO 4217 currency and expressed in that
// currency's minor units. A nil map selects DefaultMatchConfig; an explicitly
// empty map means zero tolerance for every currency.
type MatchConfig struct {
	FeeToleranceMinor map[string]int64 `yaml:"fee_tolerance_minor"`
}

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

// DefaultMatchConfig returns a fresh copy of the default tolerance map so
// callers cannot mutate the package default.
func DefaultMatchConfig() MatchConfig {
	tolerance := make(map[string]int64, len(DefaultFeeToleranceMinor))
	for currency, minor := range DefaultFeeToleranceMinor {
		tolerance[currency] = minor
	}
	return MatchConfig{FeeToleranceMinor: tolerance}
}

// FeeTolerance returns the configured tolerance for currency, or zero when the
// currency is absent.
func (c MatchConfig) FeeTolerance(currency string) int64 {
	if c.FeeToleranceMinor == nil {
		return DefaultMatchConfig().FeeToleranceMinor[currency]
	}
	return c.FeeToleranceMinor[currency]
}

// Validate rejects negative tolerances and malformed currency codes.
func (c MatchConfig) Validate() error {
	for currency, minor := range c.FeeToleranceMinor {
		if !currencyCodePattern.MatchString(currency) {
			return fmt.Errorf("config: match.fee_tolerance_minor: %q is not an ISO 4217 code", currency)
		}
		if minor < 0 {
			return fmt.Errorf("config: match.fee_tolerance_minor.%s: %d is negative", currency, minor)
		}
	}
	return nil
}

// Canonical renders the tolerance map in a stable currency order. The matcher
// hashes it into match_runs.config_hash.
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
	return canonical
}
