package verify

import (
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// VerifyRounding confirms that the external and ledger amounts differ only by
// a small, rounding-sized amount in the same currency, agree after central
// half-even rounding, and are explained by a balanced adjustment.
func VerifyRounding(in RoundingInput, opts Options) Result {
	details := Details{Currency: in.External.Currency}
	if r, ok := checkClaim(in.Claimed, PatternRounding, details); !ok {
		return r
	}
	opts, err := resolveOptions(opts)
	if err != nil {
		return failure(PatternRounding, in.Claimed, details, ReasonInvalidOptions)
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
	if in.External.AmountMinor < 0 || in.Ledger.NetMinor < 0 {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if in.External.OccurredAt.IsZero() {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if len(reasons) > 0 {
		return failure(PatternRounding, in.Claimed, details, reasons...)
	}

	currency, err := money.ParseCurrency(in.External.Currency)
	if err != nil {
		return failure(PatternRounding, in.Claimed, details, ReasonInvalidInput)
	}
	details.ExpectedMinor = in.External.AmountMinor
	details.ActualMinor = in.Ledger.NetMinor

	delta, ok := subInt64(in.External.AmountMinor, in.Ledger.NetMinor)
	if !ok {
		return failure(PatternRounding, in.Claimed, details, ReasonAmountOverflow)
	}
	if delta <= 0 {
		// A rounding recharge increases the ledger side to match the external
		// side. Zero is a no-op and a negative delta would need the opposite
		// direction, which this pattern does not describe.
		return failure(PatternRounding, in.Claimed, details, ReasonNotRounding)
	}
	absDelta, ok := absInt64(delta)
	if !ok {
		return failure(PatternRounding, in.Claimed, details, ReasonAmountOverflow)
	}
	if absDelta > opts.capFor(in.External.Currency) {
		return failure(PatternRounding, in.Claimed, details, ReasonCapExceeded)
	}

	externalRounded, ok := roundToMajor(in.External.AmountMinor, currency.Exponent())
	if !ok {
		return failure(PatternRounding, in.Claimed, details, ReasonAmountOverflow)
	}
	ledgerRounded, ok := roundToMajor(in.Ledger.NetMinor, currency.Exponent())
	if !ok {
		return failure(PatternRounding, in.Claimed, details, ReasonAmountOverflow)
	}
	if externalRounded != ledgerRounded {
		return failure(PatternRounding, in.Claimed, details, ReasonNotRounding)
	}

	if reasons = validAdjustment(in.Lines, in.External.Currency, absDelta); len(reasons) > 0 {
		return failure(PatternRounding, in.Claimed, details, reasons...)
	}
	return success(PatternRounding, in.Claimed, details)
}

// roundToMajor rounds an integer minor-unit amount to the nearest whole major
// unit using the central half-even function. The amount is first expressed at
// the currency exponent (minor -> major decimal) and then rounded to an
// integer. Rounding the raw minor integer with money.Round(minor, exponent)
// would only scale it by 10^exponent and could never make two distinct minor
// amounts agree.
func roundToMajor(minor int64, exponent int) (int64, bool) {
	major := decimal.NewFromInt(minor).Shift(-int32(exponent))
	rounded, err := money.Round(major, 0, money.RoundHalfEven)
	if err != nil {
		return 0, false
	}
	return rounded, true
}
