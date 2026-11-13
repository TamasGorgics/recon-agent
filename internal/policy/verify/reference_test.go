package verify

import (
	"math"
	"testing"
	"time"
)

func TestVerifyReferenceTypoPass(t *testing.T) {
	in := validReferenceInput()
	result := VerifyReferenceTypo(in, DefaultOptions())
	assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)
	if got, want := result.Details.Candidates, 1; got != want {
		t.Fatalf("Candidates = %d, want %d", got, want)
	}
	if got, want := result.Details.ExpectedMinor, int64(1_000); got != want {
		t.Fatalf("ExpectedMinor = %d, want %d", got, want)
	}
	if got, want := result.Details.ActualMinor, int64(1_000); got != want {
		t.Fatalf("ActualMinor = %d, want %d", got, want)
	}
}

func TestVerifyReferenceTypoFailures(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*ReferenceTypoInput)
		opts        Options
		wantReasons []string
	}{
		{
			name:        "missing claim",
			mutate:      func(in *ReferenceTypoInput) { in.Claimed = "" },
			wantReasons: []string{ReasonNoPattern},
		},
		{
			name:        "unknown claim",
			mutate:      func(in *ReferenceTypoInput) { in.Claimed = "mystery" },
			wantReasons: []string{ReasonUnknownPattern},
		},
		{
			name:        "other claim",
			mutate:      func(in *ReferenceTypoInput) { in.Claimed = PatternOther },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "wrong claim",
			mutate:      func(in *ReferenceTypoInput) { in.Claimed = PatternRounding },
			wantReasons: []string{ReasonPatternMismatch},
		},
		{
			name:        "missing source",
			mutate:      func(in *ReferenceTypoInput) { in.External.Source = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "missing external id",
			mutate:      func(in *ReferenceTypoInput) { in.External.SourceRecordID = "" },
			wantReasons: []string{ReasonSourceAmbiguous},
		},
		{
			name:        "invalid currency",
			mutate:      func(in *ReferenceTypoInput) { in.External.Currency = "usd" },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "zero amount",
			mutate:      func(in *ReferenceTypoInput) { in.External.AmountMinor = 0 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "negative amount",
			mutate:      func(in *ReferenceTypoInput) { in.External.AmountMinor = -1 },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "zero occurred at",
			mutate:      func(in *ReferenceTypoInput) { in.External.OccurredAt = time.Time{} },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "nil proposed set",
			mutate:      func(in *ReferenceTypoInput) { in.ProposedLedgerIDs = nil },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "empty proposed set",
			mutate:      func(in *ReferenceTypoInput) { in.ProposedLedgerIDs = []string{} },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "blank proposed ids only",
			mutate:      func(in *ReferenceTypoInput) { in.ProposedLedgerIDs = []string{" ", ""} },
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "no candidates",
			mutate:      func(in *ReferenceTypoInput) { in.Candidates = nil },
			wantReasons: []string{ReasonNoCandidate},
		},
		{
			name: "candidate wrong amount",
			mutate: func(in *ReferenceTypoInput) {
				in.Candidates[0].AmountMinor = 999
			},
			wantReasons: []string{ReasonNoCandidate},
		},
		{
			name: "candidate wrong currency",
			mutate: func(in *ReferenceTypoInput) {
				in.Candidates[0].Currency = "EUR"
			},
			wantReasons: []string{ReasonNoCandidate},
		},
		{
			name: "candidate outside window",
			mutate: func(in *ReferenceTypoInput) {
				in.Candidates[0].ValueDate = in.External.OccurredAt.AddDate(0, 0, 4)
			},
			wantReasons: []string{ReasonNoCandidate},
		},
		{
			name: "second candidate fails",
			mutate: func(in *ReferenceTypoInput) {
				other := in.Candidates[0]
				other.LedgerTxnID = "ledger-ref-2"
				in.Candidates = append(in.Candidates, other)
			},
			wantReasons: []string{ReasonMultipleCandidates},
		},
		{
			name: "second candidate fails even when proposed",
			mutate: func(in *ReferenceTypoInput) {
				other := in.Candidates[0]
				other.LedgerTxnID = "ledger-ref-2"
				in.Candidates = append(in.Candidates, other)
				in.ProposedLedgerIDs = []string{"ledger-ref-1", "ledger-ref-2"}
			},
			wantReasons: []string{ReasonMultipleCandidates},
		},
		{
			name: "candidate mismatch",
			mutate: func(in *ReferenceTypoInput) {
				in.ProposedLedgerIDs = []string{"ledger-other"}
			},
			wantReasons: []string{ReasonCandidateMismatch},
		},
		{
			name: "proposed set has an extra ledger",
			mutate: func(in *ReferenceTypoInput) {
				in.ProposedLedgerIDs = []string{"ledger-ref-1", "ledger-ref-2"}
			},
			wantReasons: []string{ReasonCandidateMismatch},
		},
		{
			name: "candidate missing id",
			mutate: func(in *ReferenceTypoInput) {
				in.Candidates[0].LedgerTxnID = ""
			},
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name: "candidate missing value date",
			mutate: func(in *ReferenceTypoInput) {
				in.Candidates[0].ValueDate = time.Time{}
			},
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name: "candidate invalid currency",
			mutate: func(in *ReferenceTypoInput) {
				in.Candidates[0].Currency = "usd"
			},
			wantReasons: []string{ReasonInvalidInput},
		},
		{
			name:        "invalid options window",
			mutate:      func(in *ReferenceTypoInput) {},
			opts:        Options{ReferenceWindowDays: -1},
			wantReasons: []string{ReasonInvalidOptions},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validReferenceInput()
			if test.mutate != nil {
				test.mutate(&in)
			}
			result := VerifyReferenceTypo(in, test.opts)
			assertFailure(t, result, PatternReferenceTypo, in.Claimed, test.wantReasons...)
		})
	}
}

func TestVerifyReferenceTypoWindowEdges(t *testing.T) {
	tests := []struct {
		name     string
		days     int
		window   int
		wantPass bool
	}{
		{"three days before", -3, 3, true},
		{"four days before", -4, 3, false},
		{"three days after", 3, 3, true},
		{"four days after", 4, 3, false},
		{"one day inside custom window", 1, 1, true},
		{"two days outside custom window", 2, 1, false},
		{"same day", 0, 3, true},
		{"maximum window", 365, 365, true},
		{"over maximum window", 365, 366, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validReferenceInput()
			in.Candidates[0].ValueDate = in.External.OccurredAt.AddDate(0, 0, test.days)
			opts := Options{ReferenceWindowDays: test.window}
			result := VerifyReferenceTypo(in, opts)
			if test.wantPass {
				assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)
			} else {
				if test.window > MaxReferenceWindowDays {
					assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonInvalidOptions)
					return
				}
				assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonNoCandidate)
			}
		})
	}
}

func TestVerifyReferenceTypoUsesValueDateWhenSet(t *testing.T) {
	in := validReferenceInput()
	in.External.ValueDate = in.External.OccurredAt
	in.External.OccurredAt = in.External.OccurredAt.AddDate(0, 0, 90)
	in.Candidates[0].ValueDate = in.External.ValueDate.AddDate(0, 0, 3)
	assertPass(t, VerifyReferenceTypo(in, DefaultOptions()), PatternReferenceTypo, PatternReferenceTypo)

	// Without a value date the same input falls back to OccurredAt and the
	// candidate is 87 days outside the window.
	in.External.ValueDate = time.Time{}
	assertFailure(t, VerifyReferenceTypo(in, DefaultOptions()), PatternReferenceTypo, PatternReferenceTypo, ReasonNoCandidate)
}

func TestVerifyReferenceTypoProposedSetNormalization(t *testing.T) {
	for _, proposed := range [][]string{
		{"ledger-ref-1"},
		{" ledger-ref-1 "},
		{"ledger-ref-1", "ledger-ref-1"},
		{"", "ledger-ref-1"},
	} {
		in := validReferenceInput()
		in.ProposedLedgerIDs = proposed
		assertPass(t, VerifyReferenceTypo(in, DefaultOptions()), PatternReferenceTypo, PatternReferenceTypo)
	}
}

func TestVerifyReferenceTypoCandidateOrderDeterminism(t *testing.T) {
	in := validReferenceInput()
	first := in.Candidates[0]
	other := first
	other.LedgerTxnID = "ledger-ref-2"
	in.Candidates = []Candidate{first, other}
	forward := resultJSON(t, VerifyReferenceTypo(in, DefaultOptions()))

	in.Candidates = []Candidate{other, first}
	backward := resultJSON(t, VerifyReferenceTypo(in, DefaultOptions()))
	if forward != backward {
		t.Fatalf("candidate order changed the result:\n%s\n%s", forward, backward)
	}
	if got, want := VerifyReferenceTypo(in, DefaultOptions()).Details.Candidates, 2; got != want {
		t.Fatalf("Candidates = %d, want %d", got, want)
	}
}

func TestVerifyReferenceTypoMaxAmount(t *testing.T) {
	in := validReferenceInput()
	in.External.AmountMinor = math.MaxInt64
	in.Candidates[0].AmountMinor = math.MaxInt64
	assertPass(t, VerifyReferenceTypo(in, DefaultOptions()), PatternReferenceTypo, PatternReferenceTypo)
}
