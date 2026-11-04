package money

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/shopspring/decimal"
)

// Typed sentinel errors. Callers branch with errors.Is; errors may wrap them
// with additional context.
var (
	// ErrUnknownCurrency is returned when a Currency is not in the embedded
	// ISO 4217 table.
	ErrUnknownCurrency = errors.New("money: unknown currency")
	// ErrCurrencyMismatch is returned when arithmetic mixes two currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when an int64 operation would wrap around.
	ErrOverflow = errors.New("money: overflow")
	// ErrInvalidAmount is returned when a decimal string cannot be parsed.
	ErrInvalidAmount = errors.New("money: invalid amount")
	// ErrExcessPrecision is returned when a decimal string has more fractional
	// digits than the currency's exponent can represent exactly.
	ErrExcessPrecision = errors.New("money: excess precision")
	// ErrInvalidRate is returned when an FX rate is malformed or has a scale
	// greater than 12 decimal places.
	ErrInvalidRate = errors.New("money: invalid rate")
	// ErrInvalidRoundingMode is returned for an unknown RoundMode.
	ErrInvalidRoundingMode = errors.New("money: invalid rounding mode")
	// ErrInvalidAllocation is returned for invalid Allocate weights or a
	// residual index outside the result.
	ErrInvalidAllocation = errors.New("money: invalid allocation")
	// ErrInvalidJSON is returned when a JSON money value is malformed, has
	// missing or unknown fields, or uses a non-integer amount.
	ErrInvalidJSON = errors.New("money: invalid JSON")
)

// Money is an exact amount in minor units paired with its currency. The zero
// value has no currency and is not valid for arithmetic, conversion, or JSON.
type Money struct {
	amount   int64
	currency Currency
}

// FromMinor returns a Money for an integer amount in minor units. The currency
// must come from ParseCurrency, MustParseCurrency, or Currencies; an invalid
// currency is reported by any later operation that needs it.
func FromMinor(amount int64, currency Currency) Money {
	return Money{amount: amount, currency: currency}
}

// AmountMinor returns the amount in minor units.
func (m Money) AmountMinor() int64 { return m.amount }

// Currency returns the currency paired with the amount.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.amount == 0 }

// check verifies that the currency is part of the embedded ISO 4217 table.
func (m Money) check() error {
	if !m.currency.IsValid() {
		return fmt.Errorf("%w: %q", ErrUnknownCurrency, m.currency.code)
	}
	return nil
}

// checkPair verifies both operands and that they use the same currency.
func (m Money) checkPair(other Money) error {
	if err := m.check(); err != nil {
		return err
	}
	if err := other.check(); err != nil {
		return err
	}
	if m.currency != other.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.currency.code, other.currency.code)
	}
	return nil
}

// Add returns m + other. It fails with ErrCurrencyMismatch when the currencies
// differ and with ErrOverflow when the int64 sum would wrap.
func (m Money) Add(other Money) (Money, error) {
	if err := m.checkPair(other); err != nil {
		return Money{}, err
	}
	if other.amount > 0 && m.amount > math.MaxInt64-other.amount {
		return Money{}, fmt.Errorf("%w: %d + %d", ErrOverflow, m.amount, other.amount)
	}
	if other.amount < 0 && m.amount < math.MinInt64-other.amount {
		return Money{}, fmt.Errorf("%w: %d + %d", ErrOverflow, m.amount, other.amount)
	}
	return FromMinor(m.amount+other.amount, m.currency), nil
}

// Sub returns m - other. It fails with ErrCurrencyMismatch when the currencies
// differ and with ErrOverflow when the int64 difference would wrap.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.checkPair(other); err != nil {
		return Money{}, err
	}
	if other.amount > 0 && m.amount < math.MinInt64+other.amount {
		return Money{}, fmt.Errorf("%w: %d - %d", ErrOverflow, m.amount, other.amount)
	}
	if other.amount < 0 && m.amount > math.MaxInt64+other.amount {
		return Money{}, fmt.Errorf("%w: %d - %d", ErrOverflow, m.amount, other.amount)
	}
	return FromMinor(m.amount-other.amount, m.currency), nil
}

// Neg returns -m. Negating math.MinInt64 fails with ErrOverflow.
func (m Money) Neg() (Money, error) {
	if err := m.check(); err != nil {
		return Money{}, err
	}
	if m.amount == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: cannot negate %d", ErrOverflow, m.amount)
	}
	return FromMinor(-m.amount, m.currency), nil
}

// Cmp compares m and other and returns -1, 0, or 1. It fails with
// ErrCurrencyMismatch when the currencies differ.
func (m Money) Cmp(other Money) (int, error) {
	if err := m.checkPair(other); err != nil {
		return 0, err
	}
	switch {
	case m.amount < other.amount:
		return -1, nil
	case m.amount > other.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

// ParseDecimal converts an exact decimal string into minor units for currency.
// It rejects exponent notation, malformed input, values with more fractional
// digits than the currency exponent can represent exactly, and values outside
// the int64 minor-unit range. Trailing fractional zeros are allowed because
// they do not change the exact value.
func ParseDecimal(s string, currency Currency) (Money, error) {
	if !currency.IsValid() {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, currency.code)
	}
	if s == "" {
		return Money{}, fmt.Errorf("%w: empty amount", ErrInvalidAmount)
	}
	if strings.ContainsAny(s, "eE") {
		return Money{}, fmt.Errorf("%w: exponent notation %q is not accepted", ErrInvalidAmount, s)
	}
	value, err := decimal.NewFromString(s)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	scaled := value.Shift(int32(currency.exponent))
	if !scaled.IsInteger() {
		return Money{}, fmt.Errorf("%w: %q has more than %d decimal places for %s",
			ErrExcessPrecision, s, currency.Exponent(), currency.code)
	}
	minor := scaled.BigInt()
	if !minor.IsInt64() {
		return Money{}, fmt.Errorf("%w: %q is outside the int64 minor-unit range", ErrOverflow, s)
	}
	return FromMinor(minor.Int64(), currency), nil
}

// DecimalString formats the amount as an exact decimal string using the
// currency exponent, for example "10.00" for 1000 USD minor units.
func (m Money) DecimalString() string {
	return decimal.NewFromInt(m.amount).
		Shift(-int32(m.currency.Exponent())).
		StringFixed(int32(m.currency.Exponent()))
}

// String formats the money as "<decimal> <CODE>", for example "10.00 USD".
func (m Money) String() string {
	return m.DecimalString() + " " + m.currency.code
}
