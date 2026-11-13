package verify

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()
	if opts.ReferenceWindowDays != DefaultReferenceWindowDays {
		t.Fatalf("ReferenceWindowDays = %d, want %d", opts.ReferenceWindowDays, DefaultReferenceWindowDays)
	}
	if opts.RoundingCapMinor != nil {
		t.Fatalf("RoundingCapMinor = %#v, want nil", opts.RoundingCapMinor)
	}
	if err := opts.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if got, want := (Options{}).withDefaults().ReferenceWindowDays, DefaultReferenceWindowDays; got != want {
		t.Fatalf("zero Options window = %d, want %d", got, want)
	}
}

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"zero", Options{}, ""},
		{"nil caps", Options{RoundingCapMinor: nil}, ""},
		{"empty caps", Options{RoundingCapMinor: map[string]int64{}}, ""},
		{"caps", Options{RoundingCapMinor: map[string]int64{"USD": 0, "JPY": 2, "KWD": 3}}, ""},
		{"window min", Options{ReferenceWindowDays: 1}, ""},
		{"window max", Options{ReferenceWindowDays: 365}, ""},
		{"negative cap", Options{RoundingCapMinor: map[string]int64{"USD": -1}}, "negative"},
		{"bad currency", Options{RoundingCapMinor: map[string]int64{"usd": 1}}, "ISO 4217"},
		{"negative window", Options{ReferenceWindowDays: -1}, "between 1 and 365"},
		{"window too large", Options{ReferenceWindowDays: 366}, "between 1 and 365"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.opts.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, test.wantErr)
			}
			if !strings.Contains(err.Error(), ErrInvalidOptions.Error()) {
				t.Fatalf("Validate() = %v, want it to wrap ErrInvalidOptions", err)
			}
		})
	}
}

func TestOptionsCapFallback(t *testing.T) {
	// A nil map falls back to one minor unit per currency.
	if got, want := (Options{}).capFor("USD"), DefaultRoundingCapMinor; got != want {
		t.Fatalf("nil capFor(USD) = %d, want %d", got, want)
	}
	// An explicitly empty map behaves the same way.
	if got, want := (Options{RoundingCapMinor: map[string]int64{}}).capFor("USD"), DefaultRoundingCapMinor; got != want {
		t.Fatalf("empty capFor(USD) = %d, want %d", got, want)
	}
	// An explicit zero is a real cap, not a fallback.
	if got := (Options{RoundingCapMinor: map[string]int64{"USD": 0}}).capFor("USD"); got != 0 {
		t.Fatalf("explicit capFor(USD) = %d, want 0", got)
	}
	if got := (Options{RoundingCapMinor: map[string]int64{"USD": 5}}).capFor("USD"); got != 5 {
		t.Fatalf("custom capFor(USD) = %d, want 5", got)
	}
	if got, want := (Options{RoundingCapMinor: map[string]int64{"USD": 5}}).capFor("EUR"), DefaultRoundingCapMinor; got != want {
		t.Fatalf("absent capFor(EUR) = %d, want %d", got, want)
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg := config.VerifierConfig{
		RoundingCapMinor:    map[string]int64{"USD": 2, "JPY": 0},
		ReferenceWindowDays: 7,
	}
	opts := OptionsFromConfig(cfg)
	if got, want := opts.ReferenceWindowDays, 7; got != want {
		t.Fatalf("ReferenceWindowDays = %d, want %d", got, want)
	}
	if !reflect.DeepEqual(opts.RoundingCapMinor, cfg.RoundingCapMinor) {
		t.Fatalf("RoundingCapMinor = %#v, want %#v", opts.RoundingCapMinor, cfg.RoundingCapMinor)
	}
	cfg.RoundingCapMinor["USD"] = 99
	if got, want := opts.RoundingCapMinor["USD"], int64(2); got != want {
		t.Fatalf("OptionsFromConfig aliased the config map: USD = %d, want %d", got, want)
	}

	zero := OptionsFromConfig(config.VerifierConfig{})
	if zero.ReferenceWindowDays != DefaultReferenceWindowDays {
		t.Fatalf("zero config window = %d, want %d", zero.ReferenceWindowDays, DefaultReferenceWindowDays)
	}
	if zero.RoundingCapMinor != nil {
		t.Fatalf("zero config caps = %#v, want nil", zero.RoundingCapMinor)
	}
	if err := zero.Validate(); err != nil {
		t.Fatalf("zero config options Validate() = %v", err)
	}
}

func TestFailureReasonsAreSortedAndDeduplicated(t *testing.T) {
	in := validFeeInput()
	in.External.Source = ""
	in.External.SourceRecordID = ""
	in.Ledger.ID = ""
	in.External.Kind = "refund"
	in.External.Currency = "usd"
	in.Ledger.Currency = "usd"
	in.External.AmountMinor = 0
	in.External.FeeMinor = -1
	in.Ledger.FeeMinor = -1
	in.External.OccurredAt = testTime
	result := VerifyFeeVariance(in, DefaultOptions())
	assertFailure(t, result, PatternFeeVariance, in.Claimed,
		ReasonInvalidInput, ReasonPatternMismatch, ReasonSourceAmbiguous)
}

func TestVerificationConversion(t *testing.T) {
	pass := VerifyRounding(validRoundingInput(), DefaultOptions())
	verification, err := pass.Verification()
	if err != nil {
		t.Fatalf("Verification: %v", err)
	}
	if verification.Pattern != PatternRounding || !verification.Passed {
		t.Fatalf("policy verification = %+v, want passing rounding", verification)
	}
	var details Details
	if err := json.Unmarshal(verification.Details, &details); err != nil {
		t.Fatalf("unmarshal verification details: %v", err)
	}
	if details.SchemaVersion != SchemaVersion || details.Claimed != PatternRounding {
		t.Fatalf("details = %+v, want schema 1 and claimed rounding", details)
	}

	// A hand-built result with no schema version is normalized to version 1.
	raw, err := (Result{Pattern: PatternRounding}).Verification()
	if err != nil {
		t.Fatalf("Verification zero: %v", err)
	}
	if !strings.Contains(string(raw.Details), `"schema_version":1`) {
		t.Fatalf("verification zero details = %s, want schema_version 1", raw.Details)
	}
}

func TestReasonVocabulary(t *testing.T) {
	codes := ReasonCodes()
	if !reflect.DeepEqual(codes, sortedCopy(codes)) {
		t.Fatalf("ReasonCodes() is not sorted: %v", codes)
	}
	seen := make(map[string]struct{}, len(codes))
	for _, code := range codes {
		if code == "" {
			t.Fatal("ReasonCodes() contains an empty code")
		}
		if _, ok := seen[code]; ok {
			t.Fatalf("ReasonCodes() contains duplicate %q", code)
		}
		seen[code] = struct{}{}
		if !IsReasonCode(code) {
			t.Fatalf("IsReasonCode(%q) = false", code)
		}
	}
	for _, required := range []string{
		ReasonNoPattern, ReasonPatternMismatch, ReasonUnknownPattern, ReasonSourceAmbiguous,
		ReasonCurrencyMismatch, ReasonAmountOverflow, ReasonAdjustmentMismatch,
		ReasonMissingSchedule, ReasonAmbiguousSchedule, ReasonFeeMismatch,
		ReasonMissingRate, ReasonAmbiguousRate, ReasonConversionMismatch,
		ReasonCapExceeded, ReasonNotRounding, ReasonNoCandidate,
		ReasonMultipleCandidates, ReasonCandidateMismatch,
		ReasonBreakUnresolved,
	} {
		if !IsReasonCode(required) {
			t.Fatalf("required reason %q missing from the closed vocabulary", required)
		}
	}
	if IsReasonCode("free text reason") {
		t.Fatal("IsReasonCode accepted an unknown code")
	}
}

func sortedCopy(values []string) []string {
	copied := append([]string(nil), values...)
	for i := 1; i < len(copied); i++ {
		for j := i; j > 0 && copied[j] < copied[j-1]; j-- {
			copied[j], copied[j-1] = copied[j-1], copied[j]
		}
	}
	return copied
}
