package money

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"
)

// RoundMode selects how a fractional minor unit is resolved.
type RoundMode uint8

// Rounding modes. RoundHalfEven is the default for every FX and fee
// calculation.
const (
	// RoundHalfEven rounds a half towards the nearest even integer.
	RoundHalfEven RoundMode = iota
	// RoundHalfUp rounds a half away from zero.
	RoundHalfUp
	// RoundTowardZero truncates the fractional part.
	RoundTowardZero
	// RoundAwayFromZero rounds any non-zero fractional part away from zero.
	RoundAwayFromZero
)

// maxTargetExponent bounds the target minor-unit exponent accepted by Round.
// Currency exponents are 0..4; the wider bound keeps the function usable for
// fee and reporting scales without allowing pathological allocations.
const maxTargetExponent = 1000

var (
	decimalOne = decimal.NewFromInt(1)
	decimalTwo = decimal.NewFromInt(2)
)

// Round scales d by 10^exponent and rounds the result to an int64 using mode.
// It is the single rounding point for FX and fee calculations: the result is
// exact, half-even by default, and returns ErrOverflow instead of wrapping.
func Round(d decimal.Decimal, exponent int, mode RoundMode) (int64, error) {
	if mode > RoundAwayFromZero {
		return 0, fmt.Errorf("%w: %d", ErrInvalidRoundingMode, mode)
	}
	if exponent < -maxTargetExponent || exponent > maxTargetExponent {
		return 0, fmt.Errorf("%w: target exponent %d is outside [-%d, %d]",
			ErrOverflow, exponent, maxTargetExponent, maxTargetExponent)
	}
	combined := int64(d.Exponent()) + int64(exponent)
	if combined > math.MaxInt32 || combined < math.MinInt32 {
		return 0, fmt.Errorf("%w: scaled exponent %d does not fit in int32", ErrOverflow, combined)
	}
	scaled := d.Shift(int32(exponent))
	if scaled.IsZero() {
		return 0, nil
	}

	truncated := scaled.Truncate(0)
	fraction := scaled.Sub(truncated)
	sign := decimal.NewFromInt(int64(fraction.Sign()))

	var result decimal.Decimal
	switch mode {
	case RoundTowardZero:
		result = truncated
	case RoundAwayFromZero:
		result = truncated
		if !fraction.IsZero() {
			result = result.Add(sign)
		}
	case RoundHalfUp, RoundHalfEven:
		switch fraction.Abs().Mul(decimalTwo).Cmp(decimalOne) {
		case -1:
			result = truncated
		case 1:
			result = truncated.Add(sign)
		default:
			if mode == RoundHalfUp || truncated.Coefficient().Bit(0) == 1 {
				result = truncated.Add(sign)
			} else {
				result = truncated
			}
		}
	default:
		return 0, fmt.Errorf("%w: %d", ErrInvalidRoundingMode, mode)
	}

	if result.IsZero() {
		return 0, nil
	}
	// An integer decimal with a positive exponent is a small coefficient
	// times a power of ten. Materialising more than 19 digits cannot fit in
	// int64, and this bound keeps a hostile exponent from allocating.
	if exp := result.Exponent(); exp > 19 {
		return 0, fmt.Errorf("%w: rounded value is outside the int64 minor-unit range", ErrOverflow)
	}
	minor := result.BigInt()
	if !minor.IsInt64() {
		return 0, fmt.Errorf("%w: rounded value is outside the int64 minor-unit range", ErrOverflow)
	}
	return minor.Int64(), nil
}
