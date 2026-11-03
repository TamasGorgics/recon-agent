package modes

import (
	"fmt"
	"sort"
	"strings"
)

// Mode names implemented by M1-10 (part A) and M1-11 (part B); spec section
// 10.1.
const (
	DuplicateCharge   = "duplicate_charge"
	FeeVariance       = "fee_variance"
	FXRounding        = "fx_rounding"
	LateSettlement    = "late_settlement"
	MissingSettlement = "missing_settlement"
	PartialRefund     = "partial_refund"

	Chargeback     = "chargeback"
	BatchComposite = "batch_composite"
	ReferenceTypo  = "reference_typo"
	UnknownDeposit = "unknown_deposit"
	InjectionMemo  = "injection_memo"
	Ambiguous      = "ambiguous"
)

// CleanMode names untouched M1-09 scenarios in manifest mode_counts.
const CleanMode = "clean"

// PartAModes is the stable, ascending part-A order. PartBModes follows the
// same convention and AllModes is the single deterministic application order.
var PartAModes = []string{
	DuplicateCharge,
	FeeVariance,
	FXRounding,
	LateSettlement,
	MissingSettlement,
	PartialRefund,
}

// PartBModes is the M1-11 set in the same stable ascending order.
var PartBModes = []string{
	Ambiguous,
	BatchComposite,
	Chargeback,
	InjectionMemo,
	ReferenceTypo,
	UnknownDeposit,
}

// AllModes is PartAModes followed by PartBModes: the single deterministic
// application order for every M1 failure mode.
var AllModes = append(append([]string(nil), PartAModes...), PartBModes...)

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
		if !isKnownMode(name) {
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
// part-A modes, in PartAModes order. It is kept stable for M1-10 callers; use
// DefaultAllConfig to enable all twelve modes.
func DefaultConfig(total int) Config {
	return defaultConfig(total, PartAModes)
}

// DefaultAllConfig spreads total scenarios as evenly as possible over all
// twelve modes, in AllModes order. A batch_composite count emits two
// ground-truth scenarios (one composite link and one missing charge), so its
// manifest count is twice its configured count.
func DefaultAllConfig(total int) Config {
	return defaultConfig(total, AllModes)
}

func defaultConfig(total int, names []string) Config {
	counts := make(map[string]int, len(names))
	if total <= 0 {
		return Config{Total: total, Counts: counts}
	}
	base := total / len(names)
	remainder := total % len(names)
	for i, name := range names {
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

func isKnownMode(name string) bool {
	for _, mode := range AllModes {
		if mode == name {
			return true
		}
	}
	return false
}
