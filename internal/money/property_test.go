// covers: FIN-01, FIN-04, FIN-06
package money

import (
	"errors"
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
	"pgregory.net/rapid"
)

var propertyCurrencies = []Currency{USD, EUR, SEK, GBP, JPY, KWD}

func drawCurrency(rt *rapid.T, label string) Currency {
	return propertyCurrencies[rapid.IntRange(0, len(propertyCurrencies)-1).Draw(rt, label)]
}

func TestAddCommutative(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cur := drawCurrency(rt, "currency")
		a := rapid.Int64().Draw(rt, "a")
		b := rapid.Int64().Draw(rt, "b")

		left, leftErr := FromMinor(a, cur).Add(FromMinor(b, cur))
		right, rightErr := FromMinor(b, cur).Add(FromMinor(a, cur))
		if (leftErr == nil) != (rightErr == nil) {
			rt.Fatalf("Add(%d, %d) error = %v but Add(%d, %d) error = %v", a, b, leftErr, b, a, rightErr)
		}
		if leftErr != nil {
			if !errors.Is(leftErr, ErrOverflow) {
				rt.Fatalf("Add(%d, %d) error = %v, want ErrOverflow", a, b, leftErr)
			}
			return
		}
		if left != right {
			rt.Fatalf("Add(%d, %d) = %d but Add(%d, %d) = %d", a, b, left.AmountMinor(), b, a, right.AmountMinor())
		}
	})
}

func TestAddAssociative(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cur := drawCurrency(rt, "currency")
		a := rapid.Int64().Draw(rt, "a")
		b := rapid.Int64().Draw(rt, "b")
		c := rapid.Int64().Draw(rt, "c")

		ab, errAB := FromMinor(a, cur).Add(FromMinor(b, cur))
		bc, errBC := FromMinor(b, cur).Add(FromMinor(c, cur))
		if errAB != nil || errBC != nil {
			for _, err := range []error{errAB, errBC} {
				if err != nil && !errors.Is(err, ErrOverflow) {
					rt.Fatalf("intermediate Add error = %v, want ErrOverflow", err)
				}
			}
			return
		}
		left, leftErr := ab.Add(FromMinor(c, cur))
		right, rightErr := FromMinor(a, cur).Add(bc)
		if leftErr != nil || rightErr != nil {
			for _, err := range []error{leftErr, rightErr} {
				if err != nil && !errors.Is(err, ErrOverflow) {
					rt.Fatalf("outer Add error = %v, want ErrOverflow", err)
				}
			}
			return
		}
		if left != right {
			rt.Fatalf("(%d+%d)+%d = %d but %d+(%d+%d) = %d",
				a, b, c, left.AmountMinor(), a, b, c, right.AmountMinor())
		}
	})
}

func TestParseFormatIdentity(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cur := drawCurrency(rt, "currency")
		amount := rapid.Int64().Draw(rt, "amount")
		m := FromMinor(amount, cur)
		formatted := m.DecimalString()
		back, err := ParseDecimal(formatted, cur)
		if err != nil {
			rt.Fatalf("ParseDecimal(%q, %s) error = %v", formatted, cur, err)
		}
		if back != m {
			rt.Fatalf("parse(format(%d %s)) = %d %s", amount, cur, back.AmountMinor(), back.Currency())
		}
	})
}

func TestAllocateSumsToTotal(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cur := drawCurrency(rt, "currency")
		amount := rapid.Int64().Draw(rt, "amount")
		n := rapid.IntRange(1, 8).Draw(rt, "parts")
		weights := make([]int64, n)
		var weightSum int64
		for i := range weights {
			weights[i] = rapid.Int64Range(0, 1_000_000).Draw(rt, "weight")
			weightSum += weights[i]
		}
		if weightSum == 0 {
			weights[0] = 1
			weightSum = 1
		}
		residualTo := rapid.IntRange(0, n-1).Draw(rt, "residualTo")
		m := FromMinor(amount, cur)

		parts, err := Allocate(m, weights, residualTo)
		if err != nil {
			rt.Fatalf("Allocate(%d, %v, %d) error = %v", amount, weights, residualTo, err)
		}
		var total big.Int
		for _, p := range parts {
			total.Add(&total, big.NewInt(p.AmountMinor()))
		}
		if total.Cmp(big.NewInt(amount)) != 0 {
			rt.Fatalf("Allocate parts sum to %s, want %d", total.String(), amount)
		}

		weightTotal := big.NewInt(weightSum)
		for i, weight := range weights {
			share := new(big.Int).Mul(big.NewInt(amount), big.NewInt(weight))
			share.Quo(share, weightTotal)
			diff := new(big.Int).Sub(big.NewInt(parts[i].AmountMinor()), share)
			if i == residualTo {
				if diff.CmpAbs(big.NewInt(int64(n))) >= 0 {
					rt.Fatalf("residual part %d differs from its exact share by %s, want less than %d", i, diff.String(), n)
				}
				continue
			}
			if diff.Sign() != 0 {
				rt.Fatalf("part %d = %d, want exact truncated share %s", i, parts[i].AmountMinor(), share.String())
			}
		}
	})
}

func TestConvertWithinOneMinor(t *testing.T) {
	modes := []RoundMode{RoundHalfEven, RoundHalfUp, RoundTowardZero, RoundAwayFromZero}
	rapid.Check(t, func(rt *rapid.T) {
		from := drawCurrency(rt, "from")
		to := drawCurrency(rt, "to")
		amount := rapid.Int64().Draw(rt, "amount")
		coefficient := rapid.Int64Range(-1_000_000_000, 1_000_000_000).Draw(rt, "coefficient")
		exponent := rapid.IntRange(-12, 0).Draw(rt, "exponent")
		rate, err := RateFromDecimal(decimal.New(coefficient, int32(exponent)))
		if err != nil {
			rt.Fatalf("RateFromDecimal(%d, %d) error = %v", coefficient, exponent, err)
		}
		mode := modes[rapid.IntRange(0, len(modes)-1).Draw(rt, "mode")]

		got, err := Convert(FromMinor(amount, from), rate, to, mode)
		exact := decimal.NewFromInt(amount).
			Shift(-int32(from.Exponent())).
			Mul(rate.Decimal()).
			Shift(int32(to.Exponent()))
		if err != nil {
			if !errors.Is(err, ErrOverflow) {
				rt.Fatalf("Convert(%d %s, %s, %s, %d) error = %v, want ErrOverflow", amount, from, rate, to, mode, err)
			}
			return
		}
		diff := decimal.NewFromInt(got.AmountMinor()).Sub(exact).Abs()
		if diff.GreaterThan(decimal.NewFromInt(1)) {
			rt.Fatalf("Convert(%d %s, %s, %s, %d) = %d, exact = %s, difference %s exceeds one minor unit",
				amount, from, rate, to, mode, got.AmountMinor(), exact, diff)
		}
	})
}
