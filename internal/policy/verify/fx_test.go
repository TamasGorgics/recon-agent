package verify

import (
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

func TestVerifyFXRoundingPass(t *testing.T) {
	in := validFXInput()
	result := VerifyFXRounding(in, DefaultOptions())
	assertPass(t, result, PatternFXRounding, PatternFXRounding)
	if got, want := result.Details.ExpectedMinor, int64(91); got != want {
		t.Fatalf("ExpectedMinor = %d, want %d", got, want)
	}
	if got, want := result.Details.ActualMinor, int64(90); got != want {
		t.Fatalf("ActualMinor = %d, want %d", got, want)
	}
	if got, want := result.Details.Base, "USD"; got != want {
		t.Fatalf("Base = %q, want %q", got, want)
	}
	if got, want := result.Details.Quote, "EUR"; got != want {
		t.Fatalf("Quote = %q, want %q", got, want)
	}
	if got, want := result.Details.AsOf, in.Rates[0].AsOf.UTC().Format(time.RFC3339Nano); got != want {
		t.Fatalf("AsOf = %q, want %q", got, want)
	}
}

func TestVerifyFXRoundingFailures(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*FXRoundingInput)
		opts        Options
		wantReasons []string
	}{
		{
			name:        "missing claim",
			mutate:      func(in *FXRoundingInput) { in.Claimed = "" },
			wantReasons: []string{ReasonNoPattern},
		},
		{
			name:        "unknown claim",
			mutate:      func(in *FXRoundingInput) { in.Claimed = "mystery" },
			wantReasons: []string{ReasonUnknownPattern},
		},
		{
			name:        "other claim",
			mutate:      func(in *FXRoundingInput) { in.Claimed = PatternOther },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "wrong claim",
			mutate:      func(in *FXRoundingInput) { in.Claimed = PatternFeeVariance },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "missing source",
			mutate:      func(in *FXRoundingInput) { in.External.Source = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "missing external id",
			mutate:      func(in *FXRoundingInput) { in.External.SourceRecordID = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "missing ledger id",
			mutate:      func(in *FXRoundingInput) { in.Ledger.ID = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "mixed currency",
			mutate:      func(in *FXRoundingInput) { in.Ledger.Currency = "USD" },
			wantReasons: []string{ReasonCurrencyMismatch},
		},
		{
			name: "invalid currency",
			mutate: func(in *FXRoundingInput) {
				in.External.Currency = "eur"
				in.Ledger.Currency = "eur"
			},
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "zero external amount",
			mutate:      func(in *FXRoundingInput) { in.External.AmountMinor = 0 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative external amount",
			mutate:      func(in *FXRoundingInput) { in.External.AmountMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative ledger amount",
			mutate:      func(in *FXRoundingInput) { in.Ledger.NetMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "zero occurred at",
			mutate:      func(in *FXRoundingInput) { in.External.OccurredAt = time.Time{} },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "no rates",
			mutate:      func(in *FXRoundingInput) { in.Rates = nil },
			wantReasons: []string{ReasonMissingRate},
		},
		{
			name: "rate is in the future",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].AsOf = in.External.OccurredAt.Add(time.Hour)
			},
			wantReasons: []string{ReasonMissingRate},
		},
		{
			name: "wrong source",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Source = "bank_x"
			},
			wantReasons: []string{ReasonMissingRate},
		},
		{
			name: "wrong quote",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Quote = "USD"
			},
			wantReasons: []string{ReasonMissingRate},
		},
		{
			name: "mixed base pair",
			mutate: func(in *FXRoundingInput) {
				other := in.Rates[0]
				other.Base = "GBP"
				other.AsOf = in.Rates[0].AsOf.Add(-time.Hour)
				in.Rates = append(in.Rates, other)
			},
			wantReasons: []string{ReasonAmbiguousRate},
		},
		{
			name: "tied as_of",
			mutate: func(in *FXRoundingInput) {
				other := in.Rates[0]
				other.Rate = "0.8"
				in.Rates = append(in.Rates, other)
			},
			wantReasons: []string{ReasonAmbiguousRate},
		},
		{
			name: "empty rate",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Rate = ""
			},
			wantReasons: []string{ReasonInvalidRate},
		},
		{
			name: "malformed rate",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Rate = "not-a-number"
			},
			wantReasons: []string{ReasonInvalidRate},
		},
		{
			name: "zero rate",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Rate = "0"
			},
			wantReasons: []string{ReasonInvalidRate},
		},
		{
			name: "negative rate",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Rate = "-0.9"
			},
			wantReasons: []string{ReasonInvalidRate},
		},
		{
			name: "rate more than twelve decimals",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Rate = "1.0000000000001"
			},
			wantReasons: []string{ReasonInvalidRate},
		},
		{
			name: "invalid base currency",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Base = "US"
			},
			wantReasons: []string{ReasonInvalidRate},
		},
		{
			name: "base equals quote",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Base = "EUR"
				in.Rates[0].Quote = "EUR"
			},
			wantReasons: []string{ReasonInvalidRate},
		},
		{
			name: "zero as_of",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].AsOf = time.Time{}
			},
			wantReasons: []string{ReasonInvalidRate},
		},
		{
			name: "zero residual",
			mutate: func(in *FXRoundingInput) {
				in.External.AmountMinor = 90
				in.Ledger.NetMinor = 90
				in.Lines = balancedLines(0, "EUR")
			},
			wantReasons: []string{ReasonConversionMismatch},
		},
		{
			name: "residual greater than one",
			mutate: func(in *FXRoundingInput) {
				in.External.AmountMinor = 92
				in.Ledger.NetMinor = 90
			},
			wantReasons: []string{ReasonConversionMismatch},
		},
		{
			name: "external below ledger",
			mutate: func(in *FXRoundingInput) {
				in.External.AmountMinor = 90
				in.Ledger.NetMinor = 91
			},
			wantReasons: []string{ReasonConversionMismatch},
		},
		{
			name: "no integer source at rate 0.95",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Rate = "0.95"
			},
			wantReasons: []string{ReasonConversionMismatch},
		},
		{
			name: "half-up observation at half-even tie",
			mutate: func(in *FXRoundingInput) {
				in.Rates[0].Rate = "0.5"
				in.External.AmountMinor = 1
				in.Ledger.NetMinor = 0
			},
			wantReasons: []string{ReasonConversionMismatch},
		},
		{
			name: "cap zero off by one",
			mutate: func(in *FXRoundingInput) {
				in.Lines = balancedLines(1, "EUR")
			},
			opts:        Options{RoundingCapMinor: map[string]int64{"EUR": 0}},
			wantReasons: []string{ReasonCapExceeded},
		},
		{
			name: "adjustment wrong amount",
			mutate: func(in *FXRoundingInput) {
				in.Lines = balancedLines(2, "EUR")
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment empty",
			mutate: func(in *FXRoundingInput) {
				in.Lines = nil
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment wrong currency",
			mutate: func(in *FXRoundingInput) {
				in.Lines[1].Currency = "USD"
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment overflow",
			mutate: func(in *FXRoundingInput) {
				in.Lines = []AdjustmentLine{
					{AccountCode: "clearing", Direction: "debit", AmountMinor: math.MaxInt64, Currency: "EUR"},
					{AccountCode: "clearing-2", Direction: "debit", AmountMinor: math.MaxInt64, Currency: "EUR"},
					{AccountCode: "fees", Direction: "credit", AmountMinor: math.MaxInt64, Currency: "EUR"},
					{AccountCode: "fees-2", Direction: "credit", AmountMinor: math.MaxInt64, Currency: "EUR"},
				}
			},
			wantReasons: []string{ReasonAdjustmentMismatch, ReasonAmountOverflow},
		},
		{
			name:        "invalid options",
			mutate:      func(in *FXRoundingInput) {},
			opts:        Options{ReferenceWindowDays: 366},
			wantReasons: []string{ReasonInvalidOptions},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validFXInput()
			if test.mutate != nil {
				test.mutate(&in)
			}
			result := VerifyFXRounding(in, test.opts)
			assertFailure(t, result, PatternFXRounding, in.Claimed, test.wantReasons...)
		})
	}
}

func TestVerifyFXRoundingLatestRateAndBoundaries(t *testing.T) {
	in := validFXInput()
	earlier := in.Rates[0]
	earlier.Rate = "0.8"
	earlier.AsOf = in.Rates[0].AsOf.Add(-time.Hour)
	in.Rates = append([]FXRate{earlier}, in.Rates...)
	result := VerifyFXRounding(in, DefaultOptions())
	assertPass(t, result, PatternFXRounding, PatternFXRounding)
	if got, want := result.Details.AsOf, in.Rates[1].AsOf.UTC().Format(time.RFC3339Nano); got != want {
		t.Fatalf("AsOf = %q, want latest %q", got, want)
	}

	// as_of exactly equal to the external instant is applicable.
	in = validFXInput()
	in.Rates[0].AsOf = in.External.OccurredAt
	assertPass(t, VerifyFXRounding(in, DefaultOptions()), PatternFXRounding, PatternFXRounding)

	// A rate one nanosecond after the external instant is not applicable.
	in = validFXInput()
	in.Rates[0].AsOf = in.External.OccurredAt.Add(time.Nanosecond)
	assertFailure(t, VerifyFXRounding(in, DefaultOptions()), PatternFXRounding, PatternFXRounding, ReasonMissingRate)
}

func TestVerifyFXRoundingHalfEvenInterval(t *testing.T) {
	// rate 0.5: source 3 minor -> 1.5 target, half-even rounds to 2 (even)
	// and truncation is 1.
	in := validFXInput()
	in.Rates[0].Rate = "0.5"
	in.External.AmountMinor = 2
	in.Ledger.NetMinor = 1
	in.Lines = balancedLines(1, "EUR")
	assertPass(t, VerifyFXRounding(in, DefaultOptions()), PatternFXRounding, PatternFXRounding)

	// rate 0.5: source 2 minor -> 1.0 target, rounded and truncated are both
	// 1. There is no positive residual, so this is not a verifiable fx
	// rounding recharge.
	in = validFXInput()
	in.Rates[0].Rate = "0.5"
	in.External.AmountMinor = 1
	in.Ledger.NetMinor = 1
	in.Lines = balancedLines(0, "EUR")
	assertFailure(t, VerifyFXRounding(in, DefaultOptions()), PatternFXRounding, PatternFXRounding, ReasonConversionMismatch)
}

func TestVerifyFXRoundingBruteForceInterval(t *testing.T) {
	// Compare the exact interval helper with exhaustive exact source-minor
	// search for small rates and small target amounts. Rates are >= 0.1 and
	// external <= 20, so any witness has source <= 205.
	rates := []string{"0.1", "0.25", "0.5", "0.75", "0.9", "1.25", "2", "3.3"}
	for _, rateText := range rates {
		k, err := decimal.NewFromString(rateText)
		if err != nil {
			t.Fatalf("parse %q: %v", rateText, err)
		}
		for external := int64(0); external <= 20; external++ {
			for ledger := int64(0); ledger < external; ledger++ {
				want := false
				for source := int64(1); source <= 1_000; source++ {
					target := decimal.NewFromInt(source).Mul(k)
					rounded, err := money.Round(target, 0, money.RoundHalfEven)
					if err != nil {
						t.Fatalf("round: %v", err)
					}
					truncated := target.Truncate(0).BigInt().Int64()
					if rounded == external && truncated == ledger {
						want = true
						break
					}
				}
				got := fxConversionExplainable(external, ledger, k)
				if got != want {
					t.Fatalf("fxConversionExplainable(%d, %d, %s) = %v, want %v", external, ledger, rateText, got, want)
				}
			}
		}
	}
}
