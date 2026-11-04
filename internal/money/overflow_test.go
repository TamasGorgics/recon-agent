// covers: FIN-04
package money

import (
	"errors"
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

func TestAddOverflow(t *testing.T) {
	tests := []struct {
		name string
		a    int64
		b    int64
	}{
		{"max plus one", math.MaxInt64, 1},
		{"max plus max", math.MaxInt64, math.MaxInt64},
		{"min minus one", math.MinInt64, -1},
		{"min plus min", math.MinInt64, math.MinInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FromMinor(tc.a, USD).Add(FromMinor(tc.b, USD)); !errors.Is(err, ErrOverflow) {
				t.Fatalf("Add(%d, %d) error = %v, want ErrOverflow", tc.a, tc.b, err)
			}
		})
	}

	if got, err := FromMinor(math.MaxInt64-1, USD).Add(FromMinor(1, USD)); err != nil || got.AmountMinor() != math.MaxInt64 {
		t.Fatalf("Add(max-1, 1) = (%d, %v), want (%d, nil)", got.AmountMinor(), err, int64(math.MaxInt64))
	}
	if got, err := FromMinor(math.MinInt64+1, USD).Add(FromMinor(-1, USD)); err != nil || got.AmountMinor() != math.MinInt64 {
		t.Fatalf("Add(min+1, -1) = (%d, %v), want (%d, nil)", got.AmountMinor(), err, int64(math.MinInt64))
	}
}

func TestSubOverflow(t *testing.T) {
	tests := []struct {
		name string
		a    int64
		b    int64
	}{
		{"max minus minus one", math.MaxInt64, -1},
		{"max minus min", math.MaxInt64, math.MinInt64},
		{"min minus one", math.MinInt64, 1},
		{"min minus max", math.MinInt64, math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FromMinor(tc.a, USD).Sub(FromMinor(tc.b, USD)); !errors.Is(err, ErrOverflow) {
				t.Fatalf("Sub(%d, %d) error = %v, want ErrOverflow", tc.a, tc.b, err)
			}
		})
	}

	if got, err := FromMinor(math.MaxInt64, USD).Sub(FromMinor(math.MaxInt64, USD)); err != nil || !got.IsZero() {
		t.Fatalf("Sub(max, max) = (%d, %v), want (0, nil)", got.AmountMinor(), err)
	}
}

func TestNegOverflow(t *testing.T) {
	if _, err := FromMinor(math.MinInt64, USD).Neg(); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Neg(MinInt64) error = %v, want ErrOverflow", err)
	}
	got, err := FromMinor(math.MinInt64+1, USD).Neg()
	if err != nil {
		t.Fatalf("Neg(MinInt64+1) error = %v", err)
	}
	if got.AmountMinor() != math.MaxInt64 {
		t.Fatalf("Neg(MinInt64+1) = %d, want %d", got.AmountMinor(), int64(math.MaxInt64))
	}
	got, err = FromMinor(math.MaxInt64, USD).Neg()
	if err != nil {
		t.Fatalf("Neg(MaxInt64) error = %v", err)
	}
	if got.AmountMinor() != -math.MaxInt64 {
		t.Fatalf("Neg(MaxInt64) = %d, want %d", got.AmountMinor(), -int64(math.MaxInt64))
	}
}

func TestRoundOverflow(t *testing.T) {
	tests := []struct {
		name string
		in   string
		exp  int
		mode RoundMode
	}{
		{"above max", "92233720368547758.08", 2, RoundHalfEven},
		{"below min", "-92233720368547758.09", 2, RoundHalfEven},
		{"integer above max", "9223372036854775808", 0, RoundHalfEven},
		{"integer below min", "-9223372036854775809", 0, RoundHalfEven},
		{"scientific above max", "1e19", 0, RoundHalfEven},
		{"scientific far above max", "1e20", 0, RoundHalfEven},
		{"scientific below min", "-1e19", 0, RoundHalfEven},
		{"half even rounds above max", "92233720368547758.075", 2, RoundHalfEven},
		{"half up rounds above max", "92233720368547758.075", 2, RoundHalfUp},
		{"half even rounds below min", "-92233720368547758.095", 2, RoundHalfEven},
		{"away from zero above max", "92233720368547758.071", 2, RoundAwayFromZero},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Round(decimal.RequireFromString(tc.in), tc.exp, tc.mode); !errors.Is(err, ErrOverflow) {
				t.Fatalf("Round(%s, %d, %d) error = %v, want ErrOverflow", tc.in, tc.exp, tc.mode, err)
			}
		})
	}
}

func TestRoundExponentOutOfRange(t *testing.T) {
	if _, err := Round(decimal.NewFromInt(1), maxTargetExponent+1, RoundHalfEven); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Round with exponent %d error = %v, want ErrOverflow", maxTargetExponent+1, err)
	}
	if _, err := Round(decimal.NewFromInt(1), -maxTargetExponent-1, RoundHalfEven); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Round with exponent %d error = %v, want ErrOverflow", -maxTargetExponent-1, err)
	}
}

func TestConvertOverflow(t *testing.T) {
	tests := []struct {
		name   string
		amount int64
		from   Currency
		rate   string
		to     Currency
	}{
		{"max double", math.MaxInt64, JPY, "2", JPY},
		{"min double", math.MinInt64, JPY, "2", JPY},
		{"max barely above one", math.MaxInt64, JPY, "1.000000000001", JPY},
		{"max double usd", math.MaxInt64, USD, "2", USD},
		{"min double kwd", math.MinInt64, KWD, "2", KWD},
		{"max large rate", math.MaxInt64, USD, "1000", JPY},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Convert(FromMinor(tc.amount, tc.from), mustRate(t, tc.rate), tc.to, RoundHalfEven); !errors.Is(err, ErrOverflow) {
				t.Fatalf("Convert(%d %s, %s, %s) error = %v, want ErrOverflow", tc.amount, tc.from, tc.rate, tc.to, err)
			}
		})
	}
}
