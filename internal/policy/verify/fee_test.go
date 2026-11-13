package verify

import (
	"math"
	"testing"
	"time"
)

func TestVerifyFeeVariancePass(t *testing.T) {
	in := validFeeInput()
	result := VerifyFeeVariance(in, DefaultOptions())
	assertPass(t, result, PatternFeeVariance, PatternFeeVariance)
	if got, want := result.Details.ExpectedMinor, int64(42); got != want {
		t.Fatalf("ExpectedMinor = %d, want %d", got, want)
	}
	if got, want := result.Details.ActualMinor, int64(42); got != want {
		t.Fatalf("ActualMinor = %d, want %d", got, want)
	}
	if got, want := result.Details.ScheduleID, "schedule-1"; got != want {
		t.Fatalf("ScheduleID = %q, want %q", got, want)
	}
	if got, want := result.Details.Currency, "USD"; got != want {
		t.Fatalf("Currency = %q, want %q", got, want)
	}
}

func TestVerifyFeeVarianceFailures(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*FeeVarianceInput)
		opts        Options
		wantReasons []string
	}{
		{
			name:        "missing claim",
			mutate:      func(in *FeeVarianceInput) { in.Claimed = "" },
			wantReasons: []string{ReasonNoPattern},
		},
		{
			name:        "unknown claim",
			mutate:      func(in *FeeVarianceInput) { in.Claimed = "mystery" },
			wantReasons: []string{ReasonUnknownPattern},
		},
		{
			name:        "other claim",
			mutate:      func(in *FeeVarianceInput) { in.Claimed = PatternOther },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "wrong claim",
			mutate:      func(in *FeeVarianceInput) { in.Claimed = PatternRounding },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "missing source",
			mutate:      func(in *FeeVarianceInput) { in.External.Source = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "missing external id",
			mutate:      func(in *FeeVarianceInput) { in.External.SourceRecordID = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "missing ledger id",
			mutate:      func(in *FeeVarianceInput) { in.Ledger.ID = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "wrong kind",
			mutate:      func(in *FeeVarianceInput) { in.External.Kind = "refund" },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "mixed currency",
			mutate:      func(in *FeeVarianceInput) { in.Ledger.Currency = "EUR" },
			wantReasons: []string{ReasonCurrencyMismatch},
		},
		{
			name: "invalid currency",
			mutate: func(in *FeeVarianceInput) {
				in.External.Currency = "usd"
				in.Ledger.Currency = "usd"
			},
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "zero external amount",
			mutate:      func(in *FeeVarianceInput) { in.External.AmountMinor = 0 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative external amount",
			mutate:      func(in *FeeVarianceInput) { in.External.AmountMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative external fee",
			mutate:      func(in *FeeVarianceInput) { in.External.FeeMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative ledger fee",
			mutate:      func(in *FeeVarianceInput) { in.Ledger.FeeMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "zero occurred at",
			mutate:      func(in *FeeVarianceInput) { in.External.OccurredAt = time.Time{} },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "missing schedule",
			mutate:      func(in *FeeVarianceInput) { in.Schedules = nil },
			wantReasons: []string{ReasonMissingSchedule},
		},
		{
			name: "schedule starts after event",
			mutate: func(in *FeeVarianceInput) {
				in.Schedules[0].ValidFrom = testTime.AddDate(0, 0, 1)
			},
			wantReasons: []string{ReasonMissingSchedule},
		},
		{
			name: "schedule ends before event",
			mutate: func(in *FeeVarianceInput) {
				in.Schedules[0].ValidTo = testTime.AddDate(0, 0, -1)
			},
			wantReasons: []string{ReasonMissingSchedule},
		},
		{
			name: "ambiguous schedule",
			mutate: func(in *FeeVarianceInput) {
				duplicate := in.Schedules[0]
				duplicate.ID = "schedule-2"
				in.Schedules = append(in.Schedules, duplicate)
			},
			wantReasons: []string{ReasonAmbiguousSchedule},
		},
		{
			name: "schedule wrong source",
			mutate: func(in *FeeVarianceInput) {
				in.Schedules[0].Source = "bank_x"
			},
			wantReasons: []string{ReasonMissingSchedule},
		},
		{
			name: "schedule wrong kind",
			mutate: func(in *FeeVarianceInput) {
				in.Schedules[0].Kind = "fee"
			},
			wantReasons: []string{ReasonMissingSchedule},
		},
		{
			name: "schedule wrong currency",
			mutate: func(in *FeeVarianceInput) {
				in.Schedules[0].Currency = "EUR"
			},
			wantReasons: []string{ReasonMissingSchedule},
		},
		{
			name: "invalid schedule range",
			mutate: func(in *FeeVarianceInput) {
				in.Schedules[0].ValidFrom = testTime.AddDate(0, 0, 2)
				in.Schedules[0].ValidTo = testTime
			},
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name: "zero valid from",
			mutate: func(in *FeeVarianceInput) {
				in.Schedules[0].ValidFrom = time.Time{}
			},
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative pct",
			mutate:      func(in *FeeVarianceInput) { in.Schedules[0].PctBPS = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative fixed",
			mutate:      func(in *FeeVarianceInput) { in.Schedules[0].FixedMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name: "fee mismatch",
			mutate: func(in *FeeVarianceInput) {
				in.Ledger.FeeMinor = 43
				in.Lines = balancedLines(19_957, "USD")
			},
			wantReasons: []string{ReasonFeeMismatch},
		},
		{
			name: "zero delta",
			mutate: func(in *FeeVarianceInput) {
				in.External.FeeMinor = 40
				in.Ledger.FeeMinor = 40
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "negative delta",
			mutate: func(in *FeeVarianceInput) {
				in.External.FeeMinor = 38
				in.Ledger.FeeMinor = 40
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment wrong amount",
			mutate: func(in *FeeVarianceInput) {
				in.Lines = balancedLines(19_957, "USD")
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment empty",
			mutate: func(in *FeeVarianceInput) {
				in.Lines = nil
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment wrong currency",
			mutate: func(in *FeeVarianceInput) {
				in.Lines[1].Currency = "EUR"
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment negative line",
			mutate: func(in *FeeVarianceInput) {
				in.Lines[0].AmountMinor = -1
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment unknown direction",
			mutate: func(in *FeeVarianceInput) {
				in.Lines[0].Direction = "sideways"
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment only debit",
			mutate: func(in *FeeVarianceInput) {
				in.Lines = in.Lines[:1]
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "gross overflow",
			mutate: func(in *FeeVarianceInput) {
				in.External.AmountMinor = math.MaxInt64
				in.External.FeeMinor = 1
			},
			wantReasons: []string{ReasonAmountOverflow},
		},
		{
			name: "expected fee overflow",
			mutate: func(in *FeeVarianceInput) {
				in.External.AmountMinor = math.MaxInt64 - 1
				in.External.FeeMinor = 1
				in.Schedules[0].PctBPS = 10_000
			},
			wantReasons: []string{ReasonAmountOverflow},
		},
		{
			name: "adjustment sum overflow",
			mutate: func(in *FeeVarianceInput) {
				in.Lines = []AdjustmentLine{
					{AccountCode: "clearing", Direction: "debit", AmountMinor: math.MaxInt64, Currency: "USD"},
					{AccountCode: "clearing-2", Direction: "debit", AmountMinor: math.MaxInt64, Currency: "USD"},
					{AccountCode: "fees", Direction: "credit", AmountMinor: math.MaxInt64, Currency: "USD"},
					{AccountCode: "fees-2", Direction: "credit", AmountMinor: math.MaxInt64, Currency: "USD"},
				}
			},
			wantReasons: []string{ReasonAdjustmentMismatch, ReasonAmountOverflow},
		},
		{
			name:        "invalid options",
			mutate:      func(in *FeeVarianceInput) {},
			opts:        Options{RoundingCapMinor: map[string]int64{"USD": -1}},
			wantReasons: []string{ReasonInvalidOptions},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validFeeInput()
			if test.mutate != nil {
				test.mutate(&in)
			}
			result := VerifyFeeVariance(in, test.opts)
			assertFailure(t, result, PatternFeeVariance, in.Claimed, test.wantReasons...)
		})
	}
}

func TestVerifyFeeVarianceScheduleDateEdges(t *testing.T) {
	in := validFeeInput()
	in.Schedules[0].ValidFrom = utcDate(testTime)
	in.Schedules[0].ValidTo = utcDate(testTime)
	assertPass(t, VerifyFeeVariance(in, DefaultOptions()), PatternFeeVariance, PatternFeeVariance)

	in = validFeeInput()
	in.Schedules[0].ValidTo = time.Time{} // open-ended
	assertPass(t, VerifyFeeVariance(in, DefaultOptions()), PatternFeeVariance, PatternFeeVariance)

	in = validFeeInput()
	in.Schedules[0].ValidFrom = testTime.AddDate(0, 0, 1)
	assertFailure(t, VerifyFeeVariance(in, DefaultOptions()), PatternFeeVariance, PatternFeeVariance, ReasonMissingSchedule)

	in = validFeeInput()
	in.Schedules[0].ValidTo = testTime.AddDate(0, 0, -1)
	assertFailure(t, VerifyFeeVariance(in, DefaultOptions()), PatternFeeVariance, PatternFeeVariance, ReasonMissingSchedule)
}

func TestVerifyFeeVarianceHalfEvenBoundaries(t *testing.T) {
	// gross_minor * 1bp / 10000 = 0.5 minor units; half-even rounds down to 0
	// (half-up would round to 1).
	even := validFeeInput()
	even.External.AmountMinor = 4_999
	even.External.FeeMinor = 1
	even.Schedules[0].PctBPS = 1
	even.Schedules[0].FixedMinor = 0
	even.Ledger.FeeMinor = 0
	even.Lines = balancedLines(1, "USD")
	result := VerifyFeeVariance(even, DefaultOptions())
	assertPass(t, result, PatternFeeVariance, PatternFeeVariance)
	if got, want := result.Details.ExpectedMinor, int64(0); got != want {
		t.Fatalf("ExpectedMinor = %d, want %d (half-even tie down)", got, want)
	}

	// gross_minor * 1bp / 10000 = 1.5 minor units; half-even rounds up to 2.
	odd := validFeeInput()
	odd.External.AmountMinor = 14_999
	odd.External.FeeMinor = 3
	odd.Schedules[0].PctBPS = 1
	odd.Schedules[0].FixedMinor = 0
	odd.Ledger.FeeMinor = 2
	odd.Lines = balancedLines(1, "USD")
	result = VerifyFeeVariance(odd, DefaultOptions())
	assertPass(t, result, PatternFeeVariance, PatternFeeVariance)
	if got, want := result.Details.ExpectedMinor, int64(2); got != want {
		t.Fatalf("ExpectedMinor = %d, want %d (half-even tie up)", got, want)
	}
}

func TestVerifyFeeVarianceJPYAndKWDExponents(t *testing.T) {
	tests := []struct {
		name     string
		currency string
		amount   int64
		fee      int64
		wantFee  int64
		delta    int64
	}{
		{"JPY zero exponent", "JPY", 100_000, 20_000, 12, 19_988},
		{"KWD three exponents", "KWD", 100_000, 20_000, 12, 19_988},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validFeeInput()
			in.External.Currency = test.currency
			in.External.AmountMinor = test.amount
			in.External.FeeMinor = test.fee
			in.Ledger.Currency = test.currency
			in.Ledger.FeeMinor = test.wantFee
			in.Schedules[0].Currency = test.currency
			in.Schedules[0].FixedMinor = 0
			in.Lines = balancedLines(test.delta, test.currency)
			result := VerifyFeeVariance(in, DefaultOptions())
			assertPass(t, result, PatternFeeVariance, PatternFeeVariance)
			if got, want := result.Details.ExpectedMinor, test.wantFee; got != want {
				t.Fatalf("ExpectedMinor = %d, want %d", got, want)
			}
		})
	}
}

func TestVerifyFeeVarianceScheduleDateUsesUTC(t *testing.T) {
	in := validFeeInput()
	// 2026-01-15 23:30 in UTC+5 is 2026-01-15 18:30 UTC, still inside the
	// January 15 schedule day.
	zone := time.FixedZone("UTC+5", 5*60*60)
	in.External.OccurredAt = time.Date(2026, time.January, 15, 23, 30, 0, 0, zone)
	in.Schedules[0].ValidFrom = utcDate(testTime)
	in.Schedules[0].ValidTo = utcDate(testTime)
	assertPass(t, VerifyFeeVariance(in, DefaultOptions()), PatternFeeVariance, PatternFeeVariance)
}
