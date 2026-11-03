package modes

import (
	"fmt"
	"sort"
	"strings"
)

// Mode names implemented by M1-10 (spec section 10.1, part A). M1-11 adds the
// remaining six; Config rejects them as unknown until then.
const (
	DuplicateCharge   = "duplicate_charge"
	FeeVariance       = "fee_variance"
	FXRounding        = "fx_rounding"
	LateSettlement    = "late_settlement"
	MissingSettlement = "missing_settlement"
	PartialRefund     = "partial_refund"
)

// CleanMode names untouched M1-09 scenarios in manifest mode_counts.
const CleanMode = "clean"

// PartAModes is the stable, ascending mode order used for application and
// manifest counts. Iteration order is part of the deterministic contract.
var PartAModes = []string{
	DuplicateCharge,
	FeeVariance,
	FXRounding,
	LateSettlement,
	MissingSettlement,
	PartialRefund,
}

// Config selects how many failure scenarios each mode adds. Total must equal
// the sum of Counts; a mismatch is a typed ConfigError. Counts may omit a mode
// (equivalent to zero).
type Config struct {
	Total  int
	Counts map[string]int
}

// ConfigError is a typed invalid mix/count configuration error. Callers use
// errors.As to distinguish configuration mistakes from data problems.
type ConfigError struct {
	Field  string
	Reason string
}

// Error implements error.
func (e *ConfigError) Error() string {
	return fmt.Sprintf("synth/modes: config %s: %s", e.Field, e.Reason)
}

// Validate checks the mix and counts before the world is touched.
func (c Config) Validate() error {
	if c.Total < 0 {
		return &ConfigError{Field: "total", Reason: "must not be negative"}
	}
	if c.Total == 0 {
		return &ConfigError{Field: "total", Reason: "must be positive"}
	}
	names := make([]string, 0, len(c.Counts))
	for name := range c.Counts {
		names = append(names, name)
	}
	sort.Strings(names)
	sum := 0
	for _, name := range names {
		count := c.Counts[name]
		if !isPartAMode(name) {
			return &ConfigError{Field: "counts." + name, Reason: "unknown mode"}
		}
		if count < 0 {
			return &ConfigError{Field: "counts." + name, Reason: "must not be negative"}
		}
		sum += count
	}
	if sum != c.Total {
		return &ConfigError{
			Field:  "counts",
			Reason: fmt.Sprintf("sum %d does not match total %d", sum, c.Total),
		}
	}
	return nil
}

// DefaultConfig spreads total scenarios as evenly as possible over the six
// part-A modes, in PartAModes order. A non-positive total is returned as an
// invalid Config so Validate reports it.
func DefaultConfig(total int) Config {
	counts := make(map[string]int, len(PartAModes))
	if total <= 0 {
		return Config{Total: total, Counts: counts}
	}
	base := total / len(PartAModes)
	remainder := total % len(PartAModes)
	for i, name := range PartAModes {
		counts[name] = base
		if i < remainder {
			counts[name]++
		}
	}
	return Config{Total: total, Counts: counts}
}

// canonical renders the effective mix as a stable string for the dataset
// config hash. Zero counts are omitted, so an explicit zero and an absent key
// produce the same dataset.
func (c Config) canonical() string {
	parts := make([]string, 0, len(c.Counts))
	for name, count := range c.Counts {
		if count == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, count))
	}
	sort.Strings(parts)
	return fmt.Sprintf("total=%d;%s", c.Total, strings.Join(parts, ","))
}

func isPartAMode(name string) bool {
	for _, mode := range PartAModes {
		if mode == name {
			return true
		}
	}
	return false
}
