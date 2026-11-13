package verify

import (
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// VerifyFeeVariance recomputes a PSP fee from the unique applicable schedule
// and confirms that the ledger fee and the balanced adjustment agree.
//
// It fails closed with missing_schedule/ambiguous_schedule when the schedule
// selection is not unique, fee_mismatch when the booked ledger fee differs
// from the half-even scheduled fee, and adjustment_mismatch when the proposed
// lines do not equal the positive fee delta. The generator's historical
// half-up arithmetic is deliberately not used: FIN-06 requires half-even.
func VerifyFeeVariance(in FeeVarianceInput, opts Options) Result {
	details := Details{Currency: in.External.Currency}
	if r, ok := checkClaim(in.Claimed, PatternFeeVariance, details); !ok {
		return r
	}
	if _, err := resolveOptions(opts); err != nil {
		return failure(PatternFeeVariance, in.Claimed, details, ReasonInvalidOptions)
	}

	var reasons []string
	if in.External.Source == "" || in.External.SourceRecordID == "" || in.Ledger.ID == "" {
		reasons = append(reasons, ReasonSourceAmbiguous)
	}
	if in.External.Kind != "charge" {
		reasons = append(reasons, ReasonPatternMismatch)
	}
	if !validCurrency(in.External.Currency) || !validCurrency(in.Ledger.Currency) {
		reasons = append(reasons, ReasonInvalidInput)
	} else if in.External.Currency != in.Ledger.Currency {
		reasons = append(reasons, ReasonCurrencyMismatch)
	}
	if in.External.AmountMinor <= 0 || in.External.FeeMinor < 0 || in.Ledger.FeeMinor < 0 {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if in.External.OccurredAt.IsZero() {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if len(reasons) > 0 {
		return failure(PatternFeeVariance, in.Claimed, details, reasons...)
	}

	if _, err := money.ParseCurrency(in.External.Currency); err != nil {
		return failure(PatternFeeVariance, in.Claimed, details, ReasonInvalidInput)
	}
	eventDate := utcDate(in.External.OccurredAt)

	var matches []FeeSchedule
	invalidRange := false
	for _, schedule := range in.Schedules {
		if schedule.Source != in.External.Source ||
			schedule.Kind != in.External.Kind ||
			schedule.Currency != in.External.Currency {
			continue
		}
		if schedule.ValidFrom.IsZero() {
			invalidRange = true
			continue
		}
		validFrom := utcDate(schedule.ValidFrom)
		if !schedule.ValidTo.IsZero() && utcDate(schedule.ValidTo).Before(validFrom) {
			invalidRange = true
			continue
		}
		if eventDate.Before(validFrom) {
			continue
		}
		if !schedule.ValidTo.IsZero() && eventDate.After(utcDate(schedule.ValidTo)) {
			continue
		}
		matches = append(matches, schedule)
	}
	if invalidRange {
		return failure(PatternFeeVariance, in.Claimed, details, ReasonInvalidInput)
	}
	switch len(matches) {
	case 0:
		return failure(PatternFeeVariance, in.Claimed, details, ReasonMissingSchedule)
	case 1:
	default:
		return failure(PatternFeeVariance, in.Claimed, details, ReasonAmbiguousSchedule)
	}

	schedule := matches[0]
	details.ScheduleID = schedule.ID
	if schedule.PctBPS < 0 || schedule.FixedMinor < 0 {
		return failure(PatternFeeVariance, in.Claimed, details, ReasonInvalidInput)
	}

	gross, ok := addInt64(in.External.AmountMinor, in.External.FeeMinor)
	if !ok {
		return failure(PatternFeeVariance, in.Claimed, details, ReasonAmountOverflow)
	}
	// gross * pct_bps / 10000 terminates at four decimal places, so the
	// decimal quotient is exact before the central half-even rounding.
	percentage := decimal.NewFromInt(gross).
		Mul(decimal.NewFromInt(int64(schedule.PctBPS))).
		Div(decimal.NewFromInt(10_000))
	// percentage is already expressed in minor units (gross_minor * bps /
	// 10000), so the target exponent is 0. Scaling by the currency exponent
	// again would multiply every fee by 10^exponent.
	roundedFee, err := money.Round(percentage, 0, money.RoundHalfEven)
	if err != nil {
		return failure(PatternFeeVariance, in.Claimed, details, ReasonAmountOverflow)
	}
	expectedFee, ok := addInt64(roundedFee, schedule.FixedMinor)
	if !ok {
		return failure(PatternFeeVariance, in.Claimed, details, ReasonAmountOverflow)
	}
	details.ExpectedMinor = expectedFee
	details.ActualMinor = in.Ledger.FeeMinor

	if in.Ledger.FeeMinor != expectedFee {
		reasons = append(reasons, ReasonFeeMismatch)
	}
	delta, ok := subInt64(in.External.FeeMinor, in.Ledger.FeeMinor)
	if !ok {
		reasons = append(reasons, ReasonAmountOverflow)
	} else if delta <= 0 {
		reasons = append(reasons, ReasonAdjustmentMismatch)
	} else {
		reasons = append(reasons, validAdjustment(in.Lines, in.External.Currency, delta)...)
	}
	if len(reasons) > 0 {
		return failure(PatternFeeVariance, in.Claimed, details, reasons...)
	}
	return success(PatternFeeVariance, in.Claimed, details)
}
