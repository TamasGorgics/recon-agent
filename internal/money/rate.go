package money

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// rateScale is the maximum number of decimal places stored in an FX rate. It
// matches fx_rates.rate NUMERIC(24,12).
const rateScale int32 = 12

// Rate is an exact FX rate with at most 12 decimal places. Zero and negative
// values are representable; Convert gives them their mathematical meaning.
// The zero value is a valid zero rate.
type Rate struct {
	value decimal.Decimal
}

// ParseRate parses an exact decimal FX rate and rejects more than 12 decimal
// places (including trailing-zero representations that carry no extra value).
func ParseRate(s string) (Rate, error) {
	if s == "" {
		return Rate{}, fmt.Errorf("%w: empty rate", ErrInvalidRate)
	}
	value, err := decimal.NewFromString(s)
	if err != nil {
		return Rate{}, fmt.Errorf("%w: %q", ErrInvalidRate, s)
	}
	return RateFromDecimal(value)
}

// RateFromDecimal builds a Rate from an exact decimal value and rejects more
// than 12 decimal places.
func RateFromDecimal(value decimal.Decimal) (Rate, error) {
	if !value.Equal(value.Truncate(rateScale)) {
		return Rate{}, fmt.Errorf("%w: %s has more than %d decimal places", ErrInvalidRate, value, rateScale)
	}
	return Rate{value: value}, nil
}

// MustParseRate is ParseRate for constants and tests. It panics on an invalid
// rate.
func MustParseRate(s string) Rate {
	rate, err := ParseRate(s)
	if err != nil {
		panic(err)
	}
	return rate
}

// Decimal returns the exact rate value.
func (r Rate) Decimal() decimal.Decimal { return r.value }

// String returns the canonical decimal representation of the rate.
func (r Rate) String() string { return r.value.String() }

// IsZero reports whether the rate is exactly zero.
func (r Rate) IsZero() bool { return r.value.IsZero() }

// Convert converts m into currency to using the exact FX rate. The result is
// rounded once, centrally, with mode. It fails with ErrUnknownCurrency for an
// invalid source or target and with ErrOverflow when the converted amount does
// not fit in int64 minor units.
func Convert(m Money, rate Rate, to Currency, mode RoundMode) (Money, error) {
	if err := m.check(); err != nil {
		return Money{}, err
	}
	if !to.IsValid() {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, to.code)
	}
	converted := decimal.NewFromInt(m.amount).
		Shift(-int32(m.currency.Exponent())).
		Mul(rate.value)
	minor, err := Round(converted, to.Exponent(), mode)
	if err != nil {
		return Money{}, fmt.Errorf("money: convert %s to %s: %w", m.currency.code, to.code, err)
	}
	return FromMinor(minor, to), nil
}
