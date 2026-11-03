package match

import (
	"math"
	"math/bits"
)

// Matcher amounts come from the database and source files as int64 minor
// units, so they can span the whole int64 range. Every operation that can leave
// that range is checked here: an overflow is a "no candidate" signal for the
// caller, never a wrapped value (FIN-04, M1-13 exact-sum property).

// addInt64 returns a+b. ok is false when the sum would overflow int64.
func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// subInt64 returns a-b. ok is false when the difference would overflow int64.
func subInt64(a, b int64) (int64, bool) {
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	return a - b, true
}

// absInt64 returns |v|. ok is false for math.MinInt64, whose magnitude is one
// greater than math.MaxInt64 and therefore not representable.
func absInt64(v int64) (int64, bool) {
	if v == math.MinInt64 {
		return 0, false
	}
	if v < 0 {
		return -v, true
	}
	return v, true
}

// absDelta returns |a-b|. ok is false when the difference or its magnitude
// cannot be represented as int64; callers treat that as a non-match.
func absDelta(a, b int64) (int64, bool) {
	delta, ok := subInt64(a, b)
	if !ok {
		return 0, false
	}
	return absInt64(delta)
}

// mulDivInt64 returns floor(a*b/c) for non-negative a and b and positive c. The
// product uses a 128-bit intermediate so a large but valid fuzzy tolerance
// cannot wrap the weighted score. ok is false for invalid operands or when the
// quotient does not fit int64.
func mulDivInt64(a, b, c int64) (int64, bool) {
	if a < 0 || b < 0 || c <= 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return 0, false
	}
	quotient, _ := bits.Div64(hi, lo, uint64(c))
	if quotient > math.MaxInt64 {
		return 0, false
	}
	return int64(quotient), true
}
