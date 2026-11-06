package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SynthConfig controls the deterministic synthetic dataset (M1-09). Every
// field is part of the dataset's config hash, so changing it changes the
// dataset version even when the seed stays the same.
//
// Amounts are integer minor units (FIN-05). Fee rates are integer basis points
// (FIN-06); the generator never uses floating point.
type SynthConfig struct {
	// Currencies is the ordered set of currencies in the dataset. The
	// generator sorts it, so map iteration order cannot change the output.
	Currencies []string `yaml:"currencies"`
	// BaseDate is the first calendar day of the dataset, YYYY-MM-DD. It is
	// fixed so generating on a different wall-clock day is byte-identical.
	BaseDate string `yaml:"base_date"`
	// Customers and Merchants are the fake party counts.
	Customers int `yaml:"customers"`
	Merchants int `yaml:"merchants"`
	// ChargesPerDay is the number of card charges per currency per day.
	ChargesPerDay int `yaml:"charges_per_day"`
	// FeeBps and FixedFeeMinor define the psp_a charge fee schedule
	// (fee = gross*pct_bps/10000 + fixed_minor, rounded half up).
	FeeBps        map[string]int   `yaml:"fee_bps"`
	FixedFeeMinor map[string]int64 `yaml:"fixed_fee_minor"`
	// PSPFeeMinor is the amount of the standalone psp_a fee row per currency
	// per day. BankFeeMinor is the bank_x account fee per currency per day.
	PSPFeeMinor  map[string]int64 `yaml:"psp_fee_minor"`
	BankFeeMinor map[string]int64 `yaml:"bank_fee_minor"`
	// OpeningBalanceMinor is the first bank statement's opening balance.
	OpeningBalanceMinor map[string]int64 `yaml:"opening_balance_minor"`
	// Sources configures the per-source settlement calendars used by
	// `recon synth load`. It is not part of the generated file bytes.
	Sources map[string]SourceConfig `yaml:"sources"`
}

// DefaultSynthConfig returns the M1-09 generator defaults. Each call returns a
// fresh value; maps and slices are copied so callers cannot mutate package
// state.
func DefaultSynthConfig() SynthConfig {
	return SynthConfig{
		Currencies:    []string{"EUR", "JPY", "SEK", "USD"},
		BaseDate:      "2026-01-05",
		Customers:     12,
		Merchants:     4,
		ChargesPerDay: 2,
		FeeBps: map[string]int{
			"EUR": 290,
			"JPY": 290,
			"SEK": 290,
			"USD": 290,
		},
		FixedFeeMinor: map[string]int64{
			"EUR": 25,
			"JPY": 30,
			"SEK": 100,
			"USD": 30,
		},
		PSPFeeMinor: map[string]int64{
			"EUR": 100,
			"JPY": 100,
			"SEK": 1000,
			"USD": 100,
		},
		BankFeeMinor: map[string]int64{
			"EUR": 50,
			"JPY": 50,
			"SEK": 500,
			"USD": 50,
		},
		OpeningBalanceMinor: map[string]int64{
			"EUR": 900_000,
			"JPY": 5_000_000,
			"SEK": 10_000_000,
			"USD": 1_000_000,
		},
		Sources: DefaultSynthIngestSources(),
	}
}

// DefaultSynthIngestSources returns the per-source calendars the generated
// files are built for. They match docs/formats.md and are also the fallback
// when `recon synth load` runs without an ingest section in YAML.
func DefaultSynthIngestSources() map[string]SourceConfig {
	return map[string]SourceConfig{
		"psp_a": {
			Timezone:        "America/New_York",
			Cutoff:          "17:00",
			UntrustedFields: []string{"memo"},
		},
		"bank_x": {
			Timezone:        "Europe/Stockholm",
			Cutoff:          "15:30",
			UntrustedFields: []string{"description"},
		},
	}
}

// DefaultUntrustedFields returns the fallback per-source untrusted-field map
// the read tools use when no ingest config supplies the source. It includes the
// generated sources plus the eval-only M2 fixture source psp_m2, whose synthetic
// memo/reference text must still be wrapped and scanned.
func DefaultUntrustedFields() map[string][]string {
	sources := DefaultSynthIngestSources()
	fields := make(map[string][]string, len(sources)+1)
	for name, source := range sources {
		if len(source.UntrustedFields) == 0 {
			continue
		}
		fields[name] = append([]string(nil), source.UntrustedFields...)
	}
	fields["psp_m2"] = []string{"memo", "reference"}
	return fields
}

// Validate checks the generator configuration before any output is written.
func (c SynthConfig) Validate() error {
	if len(c.Currencies) == 0 {
		return errors.New("config: synth.currencies is required")
	}
	seen := make(map[string]struct{}, len(c.Currencies))
	for _, code := range c.Currencies {
		if len(code) != 3 || strings.ToUpper(code) != code {
			return fmt.Errorf("config: synth.currencies: %q is not an ISO 4217 code", code)
		}
		if _, duplicate := seen[code]; duplicate {
			return fmt.Errorf("config: synth.currencies: %q is duplicated", code)
		}
		seen[code] = struct{}{}
	}
	if _, err := time.Parse("2006-01-02", c.BaseDate); err != nil {
		return fmt.Errorf("config: synth.base_date %q is not YYYY-MM-DD", c.BaseDate)
	}
	if c.Customers <= 0 {
		return errors.New("config: synth.customers must be positive")
	}
	if c.Merchants <= 0 {
		return errors.New("config: synth.merchants must be positive")
	}
	if c.ChargesPerDay <= 0 {
		return errors.New("config: synth.charges_per_day must be positive")
	}
	for _, code := range c.Currencies {
		if bps, ok := c.FeeBps[code]; ok && (bps < 0 || bps > 10_000) {
			return fmt.Errorf("config: synth.fee_bps.%s must be between 0 and 10000", code)
		}
		if fixed, ok := c.FixedFeeMinor[code]; ok && fixed < 0 {
			return fmt.Errorf("config: synth.fixed_fee_minor.%s must not be negative", code)
		}
		if fee, ok := c.PSPFeeMinor[code]; ok && fee <= 0 {
			return fmt.Errorf("config: synth.psp_fee_minor.%s must be positive", code)
		}
		if fee, ok := c.BankFeeMinor[code]; ok && fee <= 0 {
			return fmt.Errorf("config: synth.bank_fee_minor.%s must be positive", code)
		}
		if balance, ok := c.OpeningBalanceMinor[code]; ok && balance <= 0 {
			return fmt.Errorf("config: synth.opening_balance_minor.%s must be positive", code)
		}
	}
	for _, name := range sortedSourceNames(c.Sources) {
		if err := c.Sources[name].Validate(name); err != nil {
			return fmt.Errorf("config: synth.sources: %w", err)
		}
	}
	return nil
}

// Canonical renders the effective generation configuration as a stable string.
// days and difficulty are part of the dataset definition, so they are included
// in the hash even though they are CLI arguments rather than YAML fields.
func (c SynthConfig) Canonical(days int, difficulty string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "base_date=%s;days=%d;difficulty=%s;customers=%d;merchants=%d;charges_per_day=%d;",
		c.BaseDate, days, difficulty, c.Customers, c.Merchants, c.ChargesPerDay)
	b.WriteString("currencies=")
	b.WriteString(strings.Join(sortedStrings(c.Currencies), ","))
	b.WriteString(";fee_bps=")
	writeInt64Map(&b, intMapToInt64(c.FeeBps))
	b.WriteString(";fixed_fee_minor=")
	writeInt64Map(&b, c.FixedFeeMinor)
	b.WriteString(";psp_fee_minor=")
	writeInt64Map(&b, c.PSPFeeMinor)
	b.WriteString(";bank_fee_minor=")
	writeInt64Map(&b, c.BankFeeMinor)
	b.WriteString(";opening_balance_minor=")
	writeInt64Map(&b, c.OpeningBalanceMinor)
	return b.String()
}

// CurrenciesSorted returns the configured currency codes in ascending order.
func (c SynthConfig) CurrenciesSorted() []string {
	codes := append([]string(nil), c.Currencies...)
	sort.Strings(codes)
	return codes
}

func sortedStrings(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}

func intMapToInt64(values map[string]int) map[string]int64 {
	out := make(map[string]int64, len(values))
	for key, value := range values {
		out[key] = int64(value)
	}
	return out
}

func writeInt64Map(b *strings.Builder, values map[string]int64) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(values[key], 10))
	}
}
