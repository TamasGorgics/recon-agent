package verify

import (
	"math"
	"testing"
	"time"
)

func TestVerifyRoundingPass(t *testing.T) {
	in := validRoundingInput()
	result := VerifyRounding(in, DefaultOptions())
	assertPass(t, result, PatternRounding, PatternRounding)
	if got, want := result.Details.ExpectedMinor, int64(1_000); got != want {
		t.Fatalf("ExpectedMinor = %d, want %d", got, want)
	}
	if got, want := result.Details.ActualMinor, int64(999); got != want {
		t.Fatalf("ActualMinor = %d, want %d", got, want)
	}
}

func TestVerifyRoundingFailures(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*RoundingInput)
		opts        Options
		wantReasons []string
	}{
		{
			name:        "missing claim",
			mutate:      func(in *RoundingInput) { in.Claimed = "" },
			wantReasons: []string{ReasonNoPattern},
		},
		{
			name:        "unknown claim",
			mutate:      func(in *RoundingInput) { in.Claimed = "mystery" },
			wantReasons: []string{ReasonUnknownPattern},
		},
		{
			name:        "other claim",
			mutate:      func(in *RoundingInput) { in.Claimed = PatternOther },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "wrong claim",
			mutate:      func(in *RoundingInput) { in.Claimed = PatternFXRounding },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "missing source",
			mutate:      func(in *RoundingInput) { in.External.Source = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "missing external id",
			mutate:      func(in *RoundingInput) { in.External.SourceRecordID = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "missing ledger id",
			mutate:      func(in *RoundingInput) { in.Ledger.ID = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "mixed currency",
			mutate:      func(in *RoundingInput) { in.Ledger.Currency = "EUR" },
			wantReasons: []string{ReasonCurrencyMismatch},
		},
		{
			name: "invalid currency",
			mutate: func(in *RoundingInput) {
				in.External.Currency = "usd"
				in.Ledger.Currency = "usd"
			},
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative external amount",
			mutate:      func(in *RoundingInput) { in.External.AmountMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative ledger amount",
			mutate:      func(in *RoundingInput) { in.Ledger.NetMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "zero occurred at",
			mutate:      func(in *RoundingInput) { in.External.OccurredAt = time.Time{} },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name: "zero delta",
			mutate: func(in *RoundingInput) {
				in.External.AmountMinor = 1_000
				in.Ledger.NetMinor = 1_000
			},
			wantReasons: []string{ReasonNotRounding},
		},
		{
			name: "negative delta",
			mutate: func(in *RoundingInput) {
				in.External.AmountMinor = 999
				in.Ledger.NetMinor = 1_000
			},
			wantReasons: []string{ReasonNotRounding},
		},
		{
			name: "cap exceeded by two",
			mutate: func(in *RoundingInput) {
				in.External.AmountMinor = 1_001
				in.Lines = balancedLines(2, "USD")
			},
			wantReasons: []string{ReasonCapExceeded},
		},
		{
			name: "cap zero off by one",
			mutate: func(in *RoundingInput) {
				in.Lines = balancedLines(1, "USD")
			},
			opts:        Options{RoundingCapMinor: map[string]int64{"USD": 0}},
			wantReasons: []string{ReasonCapExceeded},
		},
		{
			name: "rounded major values differ",
			mutate: func(in *RoundingInput) {
				in.External.AmountMinor = 1_150
				in.Ledger.NetMinor = 1_149
				in.Lines = balancedLines(1, "USD")
			},
			wantReasons: []string{ReasonNotRounding},
		},
		{
			name: "half-even tie disagrees",
			mutate: func(in *RoundingInput) {
				in.External.AmountMinor = 1_051
				in.Ledger.NetMinor = 1_050
				in.Lines = balancedLines(1, "USD")
			},
			wantReasons: []string{ReasonNotRounding},
		},
		{
			name: "adjustment wrong amount",
			mutate: func(in *RoundingInput) {
				in.Lines = balancedLines(2, "USD")
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment empty",
			mutate: func(in *RoundingInput) {
				in.Lines = nil
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment wrong currency",
			mutate: func(in *RoundingInput) {
				in.Lines[1].Currency = "EUR"
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment negative line",
			mutate: func(in *RoundingInput) {
				in.Lines[0].AmountMinor = -1
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment unknown direction",
			mutate: func(in *RoundingInput) {
				in.Lines[0].Direction = "sideways"
			},
			wantReasons: []string{ReasonAdjustmentMismatch},
		},
		{
			name: "adjustment overflow",
			mutate: func(in *RoundingInput) {
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
			mutate:      func(in *RoundingInput) {},
			opts:        Options{RoundingCapMinor: map[string]int64{"USD": -1}},
			wantReasons: []string{ReasonInvalidOptions},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validRoundingInput()
			if test.mutate != nil {
				test.mutate(&in)
			}
			result := VerifyRounding(in, test.opts)
			assertFailure(t, result, PatternRounding, in.Claimed, test.wantReasons...)
		})
	}
}

func TestVerifyRoundingCapBoundaries(t *testing.T) {
	in := validRoundingInput()
	in.External.AmountMinor = 1_002
	in.Ledger.NetMinor = 1_000
	in.Lines = balancedLines(2, "USD")
	assertPass(t, VerifyRounding(in, Options{RoundingCapMinor: map[string]int64{"USD": 2}}), PatternRounding, PatternRounding)

	in = validRoundingInput()
	in.External.AmountMinor = 1_003
	in.Ledger.NetMinor = 1_000
	in.Lines = balancedLines(3, "USD")
	assertFailure(t, VerifyRounding(in, Options{RoundingCapMinor: map[string]int64{"USD": 2}}), PatternRounding, PatternRounding, ReasonCapExceeded)
}

func TestVerifyRoundingCurrencies(t *testing.T) {
	// A one-minor-unit difference can never be a rounding difference when the
	// currency has no fractional major unit.
	in := validRoundingInput()
	in.External.Currency = "JPY"
	in.External.AmountMinor = 1_000
	in.Ledger.Currency = "JPY"
	in.Ledger.NetMinor = 999
	in.Lines = balancedLines(1, "JPY")
	assertFailure(t, VerifyRounding(in, DefaultOptions()), PatternRounding, PatternRounding, ReasonNotRounding)

	// KWD has three fractional digits, so 10.000 and 9.999 agree at the major
	// unit and the one-minor residual is within cap.
	in = validRoundingInput()
	in.External.Currency = "KWD"
	in.External.AmountMinor = 10_000
	in.Ledger.Currency = "KWD"
	in.Ledger.NetMinor = 9_999
	in.Lines = balancedLines(1, "KWD")
	assertPass(t, VerifyRounding(in, DefaultOptions()), PatternRounding, PatternRounding)

	// Half-even tie down: 10.50 rounds to 10 (even) and 10.49 rounds to 10.
	in = validRoundingInput()
	in.External.AmountMinor = 1_050
	in.Ledger.NetMinor = 1_049
	in.Lines = balancedLines(1, "USD")
	assertPass(t, VerifyRounding(in, DefaultOptions()), PatternRounding, PatternRounding)
}
