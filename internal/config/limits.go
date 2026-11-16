package config

import (
	"fmt"
	"maps"
	"sort"
	"strings"
)

// Auto-apply limit defaults (M4-09). Daily caps are literal minor units and
// are twenty times the corresponding M4-04 per-proposal auto-apply caps:
// USD/EUR 10,000; SEK 100,000; JPY 10,000. The break-rate anomaly threshold is
// 10% (1,000 basis points) over at least 20 events in the UTC day.
const (
	DefaultAutoApplyDailyCapUSD int64 = 10_000
	DefaultAutoApplyDailyCapEUR int64 = 10_000
	DefaultAutoApplyDailyCapSEK int64 = 100_000
	DefaultAutoApplyDailyCapJPY int64 = 10_000

	DefaultAutoApplyBreakRateThresholdBPS = 1_000
	DefaultAutoApplyBreakRateMinSample    = 20

	MinAutoApplyBreakRateThresholdBPS = 1
	MaxAutoApplyBreakRateThresholdBPS = 10_000
)

// builtInAutoApplyDailyCaps is the fallback for currencies missing from
// auto_apply_limits.daily_max_value_minor. It is copied into every
// WithDefaults result; callers never receive the map itself.
var builtInAutoApplyDailyCaps = map[string]int64{
	"USD": DefaultAutoApplyDailyCapUSD,
	"EUR": DefaultAutoApplyDailyCapEUR,
	"SEK": DefaultAutoApplyDailyCapSEK,
	"JPY": DefaultAutoApplyDailyCapJPY,
}

// AutoApplyLimitsConfig holds the FIN-53 daily and break-rate limits. It is
// YAML-only: there is no environment binding, and policy.yaml is untouched.
type AutoApplyLimitsConfig struct {
	DailyMaxValueMinor    map[string]int64 `yaml:"daily_max_value_minor"`
	BreakRateThresholdBPS int              `yaml:"break_rate_threshold_bps"`
	BreakRateMinSample    int              `yaml:"break_rate_min_sample"`
}

// DefaultAutoApplyLimitsConfig returns a fully populated copy of the built-in
// limits. The map is independent, so mutating the result cannot change the
// package defaults.
func DefaultAutoApplyLimitsConfig() AutoApplyLimitsConfig {
	return AutoApplyLimitsConfig{}.WithDefaults()
}

// WithDefaults returns a copy with every missing currency filled from the
// built-in table and a zero threshold or sample replaced by its default. An
// explicit zero is therefore indistinguishable from an omitted field and means
// "use the default", not "invalid". It deliberately preserves explicit
// non-default entries, including zero and negative caps and out-of-range
// thresholds, so Validate can reject them. Every call returns a fresh map.
func (c AutoApplyLimitsConfig) WithDefaults() AutoApplyLimitsConfig {
	c.DailyMaxValueMinor = maps.Clone(c.DailyMaxValueMinor)
	if c.DailyMaxValueMinor == nil {
		c.DailyMaxValueMinor = make(map[string]int64, len(builtInAutoApplyDailyCaps))
	}
	for currency, cap := range builtInAutoApplyDailyCaps {
		if _, ok := c.DailyMaxValueMinor[currency]; !ok {
			c.DailyMaxValueMinor[currency] = cap
		}
	}
	if c.BreakRateThresholdBPS == 0 {
		c.BreakRateThresholdBPS = DefaultAutoApplyBreakRateThresholdBPS
	}
	if c.BreakRateMinSample == 0 {
		c.BreakRateMinSample = DefaultAutoApplyBreakRateMinSample
	}
	return c
}

// Validate checks the FIN-53 limits after applying defaults. Currency keys
// must be upper-case ISO 4217-shaped codes, caps must be positive, the
// threshold must be between 1 and 10,000 basis points, and the minimum sample
// must be positive.
func (c AutoApplyLimitsConfig) Validate() error {
	c = c.WithDefaults()
	currencies := make([]string, 0, len(c.DailyMaxValueMinor))
	for currency := range c.DailyMaxValueMinor {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	for _, currency := range currencies {
		if !currencyCodePattern.MatchString(currency) {
			return fmt.Errorf("config: auto_apply_limits.daily_max_value_minor: %q is not an ISO 4217 code", currency)
		}
		if cap := c.DailyMaxValueMinor[currency]; cap <= 0 {
			return fmt.Errorf("config: auto_apply_limits.daily_max_value_minor.%s: %d must be positive", currency, cap)
		}
	}
	if c.BreakRateThresholdBPS < MinAutoApplyBreakRateThresholdBPS || c.BreakRateThresholdBPS > MaxAutoApplyBreakRateThresholdBPS {
		return fmt.Errorf("config: auto_apply_limits.break_rate_threshold_bps must be between %d and %d, got %d",
			MinAutoApplyBreakRateThresholdBPS, MaxAutoApplyBreakRateThresholdBPS, c.BreakRateThresholdBPS)
	}
	if c.BreakRateMinSample <= 0 {
		return fmt.Errorf("config: auto_apply_limits.break_rate_min_sample must be positive, got %d", c.BreakRateMinSample)
	}
	return nil
}

// Canonical renders the limits block in a stable order for diagnostics or
// hashing.
func (c AutoApplyLimitsConfig) Canonical() string {
	c = c.WithDefaults()
	var builder strings.Builder
	fmt.Fprintf(&builder, "break_rate_threshold_bps=%d;", c.BreakRateThresholdBPS)
	fmt.Fprintf(&builder, "break_rate_min_sample=%d;", c.BreakRateMinSample)
	for _, currency := range sortedDailyCaps(c.DailyMaxValueMinor) {
		fmt.Fprintf(&builder, "daily_max_value_minor.%s=%d;", currency, c.DailyMaxValueMinor[currency])
	}
	return builder.String()
}

// String renders the limits block as supplied, with sorted currency keys so
// Config.String does not depend on map iteration order.
func (c AutoApplyLimitsConfig) String() string {
	currencies := sortedDailyCaps(c.DailyMaxValueMinor)
	parts := make([]string, 0, len(currencies))
	for _, currency := range currencies {
		parts = append(parts, fmt.Sprintf("%s:%d", currency, c.DailyMaxValueMinor[currency]))
	}
	capText := "nil"
	if c.DailyMaxValueMinor != nil {
		capText = "{" + strings.Join(parts, ",") + "}"
	}
	return fmt.Sprintf("AutoApplyLimitsConfig{DailyMaxValueMinor:%s, BreakRateThresholdBPS:%d, BreakRateMinSample:%d}",
		capText, c.BreakRateThresholdBPS, c.BreakRateMinSample)
}

func sortedDailyCaps(caps map[string]int64) []string {
	currencies := make([]string, 0, len(caps))
	for currency := range caps {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	return currencies
}
