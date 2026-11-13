package verify

import (
	"math"
	"math/big"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// VerifyFXRounding validates an FX rounding adjustment against the stored
// rate. The external target amount is the provider's half-even rounded value;
// the ledger target amount is the ledger's truncated value. The verifier checks
// that one positive integer source-minor amount explains both observations
// under the exact stored rate, then requires the residual to equal the balanced
// adjustment and stay within the target currency's rounding cap.
//
// Rates is the service-scoped candidate set for the relevant base/quote pair
// and source. The pure input has no base-currency field, so a mixed pair set is
// rejected as ambiguous rather than guessed. The latest as_of <= the external
// UTC instant wins; a tie fails closed.
func VerifyFXRounding(in FXRoundingInput, opts Options) Result {
	details := Details{Currency: in.External.Currency}
	if r, ok := checkClaim(in.Claimed, PatternFXRounding, details); !ok {
		return r
	}
	opts, err := resolveOptions(opts)
	if err != nil {
		return failure(PatternFXRounding, in.Claimed, details, ReasonInvalidOptions)
	}

	var reasons []string
	if in.External.Source == "" || in.External.SourceRecordID == "" || in.Ledger.ID == "" {
		reasons = append(reasons, ReasonSourceAmbiguous)
	}
	if !validCurrency(in.External.Currency) || !validCurrency(in.Ledger.Currency) {
		reasons = append(reasons, ReasonInvalidInput)
	} else if in.External.Currency != in.Ledger.Currency {
		reasons = append(reasons, ReasonCurrencyMismatch)
	}
	if in.External.AmountMinor <= 0 || in.Ledger.NetMinor < 0 {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if in.External.OccurredAt.IsZero() {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if len(reasons) > 0 {
		return failure(PatternFXRounding, in.Claimed, details, reasons...)
	}

	target, err := money.ParseCurrency(in.External.Currency)
	if err != nil {
		return failure(PatternFXRounding, in.Claimed, details, ReasonInvalidInput)
	}

	candidates := make([]FXRate, 0, len(in.Rates))
	for _, candidate := range in.Rates {
		if candidate.Source == in.External.Source && candidate.Quote == in.External.Currency {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return failure(PatternFXRounding, in.Claimed, details, ReasonMissingRate)
	}
	base := candidates[0].Base
	for _, candidate := range candidates[1:] {
		if candidate.Base != base {
			return failure(PatternFXRounding, in.Claimed, details, ReasonAmbiguousRate)
		}
	}

	var selected *FXRate
	var latest time.Time
	tied := false
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.AsOf.After(in.External.OccurredAt) {
			continue
		}
		switch {
		case selected == nil || candidate.AsOf.After(latest):
			selected = candidate
			latest = candidate.AsOf
			tied = false
		case candidate.AsOf.Equal(latest):
			tied = true
		}
	}
	if selected == nil {
		return failure(PatternFXRounding, in.Claimed, details, ReasonMissingRate)
	}
	if tied {
		return failure(PatternFXRounding, in.Claimed, details, ReasonAmbiguousRate)
	}

	details.Base = selected.Base
	details.Quote = selected.Quote
	details.AsOf = selected.AsOf.UTC().Format(time.RFC3339Nano)
	details.ExpectedMinor = in.External.AmountMinor
	details.ActualMinor = in.Ledger.NetMinor

	if selected.Base == "" || selected.Quote == "" || selected.Base == selected.Quote ||
		selected.Quote != in.External.Currency || !validCurrency(selected.Base) || !validCurrency(selected.Quote) ||
		selected.AsOf.IsZero() {
		return failure(PatternFXRounding, in.Claimed, details, ReasonInvalidRate)
	}
	rate, err := money.ParseRate(selected.Rate)
	if err != nil || !rate.Decimal().IsPositive() {
		return failure(PatternFXRounding, in.Claimed, details, ReasonInvalidRate)
	}
	baseCurrency, err := money.ParseCurrency(selected.Base)
	if err != nil {
		return failure(PatternFXRounding, in.Claimed, details, ReasonInvalidRate)
	}

	// target_minor = source_minor * rate * 10^(targetExp-baseExp)
	scale := rate.Decimal().Shift(int32(target.Exponent() - baseCurrency.Exponent()))
	if !fxConversionExplainable(in.External.AmountMinor, in.Ledger.NetMinor, scale) {
		return failure(PatternFXRounding, in.Claimed, details, ReasonConversionMismatch)
	}

	residual, ok := subInt64(in.External.AmountMinor, in.Ledger.NetMinor)
	if !ok {
		return failure(PatternFXRounding, in.Claimed, details, ReasonAmountOverflow)
	}
	absResidual, ok := absInt64(residual)
	if !ok {
		return failure(PatternFXRounding, in.Claimed, details, ReasonAmountOverflow)
	}
	if absResidual > opts.capFor(in.External.Currency) {
		reasons = append(reasons, ReasonCapExceeded)
	}
	reasons = append(reasons, validAdjustment(in.Lines, in.External.Currency, residual)...)
	if len(reasons) > 0 {
		return failure(PatternFXRounding, in.Claimed, details, reasons...)
	}
	return success(PatternFXRounding, in.Claimed, details)
}

// fxConversionExplainable reports whether one positive integer source-minor
// amount S can produce both observations:
//
//	external = round_half_even(S * k)   (the provider's rounded target)
//	ledger   = trunc(S * k)             (the ledger's truncated target)
//
// k = rate * 10^(targetExp-baseExp) is a positive terminating decimal. The
// verifier intersects the exact half-even rounding interval for external with
// the [ledger, ledger+1) truncation interval, then checks whether that interval
// contains S*k for a positive integer S. The intersection and the S bounds are
// computed with exact decimal/big.Int arithmetic; no float or approximate
// division is involved.
func fxConversionExplainable(external, ledger int64, k decimal.Decimal) bool {
	if !k.IsPositive() || external < 0 || ledger < 0 {
		return false
	}
	// A half-even rounded value can only be the truncated value or one more,
	// and fx_rounding must explain a positive residual.
	if external <= ledger || external-ledger > 1 {
		return false
	}

	// Truncation interval: ledger <= T < ledger+1.
	lo := decimal.NewFromInt(ledger)
	loInclusive := true
	hi := decimal.NewFromInt(ledger + 1)
	hiInclusive := false

	// Half-even rounding interval for external. At an exact .5 tie an even
	// result includes both endpoints and an odd result excludes both.
	half := decimal.New(5, -1)
	roundLo := decimal.NewFromInt(external).Sub(half)
	roundHi := decimal.NewFromInt(external).Add(half)
	even := external%2 == 0
	if roundLo.GreaterThan(lo) {
		lo = roundLo
		loInclusive = even
	}
	if roundHi.LessThan(hi) {
		hi = roundHi
		hiInclusive = even
	}

	// The source amount is positive, so T = S*k must be strictly positive.
	if lo.LessThanOrEqual(decimal.Zero) {
		lo = decimal.Zero
		loInclusive = false
	}
	if hi.LessThanOrEqual(decimal.Zero) {
		return false
	}
	switch lo.Cmp(hi) {
	case 1:
		return false
	case 0:
		if !loInclusive || !hiInclusive {
			return false
		}
	}

	return hasPositiveIntegerSource(lo, loInclusive, hi, hiInclusive, k)
}

// hasPositiveIntegerSource reports whether some integer S >= 1 satisfies the
// bounded interval lo <= S*k <= hi (with the supplied endpoint inclusivity).
//
// Both endpoints and k are terminating decimals. Scaling by 10^scale (scale is
// at least 1 so half-unit endpoints become integers) and then by 2 turns the
// comparison into integer arithmetic against K2 = 2*k*10^scale. The smallest
// and largest admissible S follow from exact floor/ceiling division.
func hasPositiveIntegerSource(lo decimal.Decimal, loInclusive bool, hi decimal.Decimal, hiInclusive bool, k decimal.Decimal) bool {
	scale := 1
	if exponent := k.Exponent(); exponent < -1 {
		scale = int(-exponent)
	}
	factor := decimal.New(1, int32(scale))
	scaledK := k.Mul(factor)
	if !scaledK.IsInteger() {
		return false
	}
	k2 := new(big.Int).Mul(scaledK.BigInt(), big.NewInt(2))
	if k2.Sign() <= 0 {
		return false
	}
	loScaled := lo.Mul(factor).Mul(decimal.NewFromInt(2))
	hiScaled := hi.Mul(factor).Mul(decimal.NewFromInt(2))
	if !loScaled.IsInteger() || !hiScaled.IsInteger() {
		return false
	}
	loInt := loScaled.BigInt()
	hiInt := hiScaled.BigInt()
	if loInt.Sign() < 0 {
		loInt = big.NewInt(0)
	}
	if hiInt.Sign() <= 0 {
		return false
	}

	var min *big.Int
	if loInclusive {
		min = ceilDiv(loInt, k2)
	} else {
		min = floorDiv(loInt, k2)
		min.Add(min, big.NewInt(1))
	}
	if min.Sign() <= 0 {
		min = big.NewInt(1)
	}
	var max *big.Int
	if hiInclusive {
		max = floorDiv(hiInt, k2)
	} else {
		max = ceilDiv(hiInt, k2)
		max.Sub(max, big.NewInt(1))
	}

	maxInt64 := big.NewInt(math.MaxInt64)
	if min.Cmp(maxInt64) > 0 {
		return false
	}
	if max.Cmp(maxInt64) > 0 {
		max.Set(maxInt64)
	}
	return max.Sign() > 0 && min.Cmp(max) <= 0
}

// floorDiv divides non-negative a by positive b and truncates towards zero,
// which is floor for non-negative operands.
func floorDiv(a, b *big.Int) *big.Int {
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(a, b, remainder)
	return quotient
}

// ceilDiv divides non-negative a by positive b and rounds up.
func ceilDiv(a, b *big.Int) *big.Int {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(a, b, remainder)
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}
