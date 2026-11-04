// covers: FIN-01, FIN-03
package money

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestCurrencies(t *testing.T) {
	required := map[string]int{
		"USD": 2,
		"EUR": 2,
		"SEK": 2,
		"GBP": 2,
		"JPY": 0,
		"KWD": 3,
	}
	currencies := Currencies()
	if len(currencies) < len(required) {
		t.Fatalf("Currencies() returned %d entries, want at least %d", len(currencies), len(required))
	}
	seen := make(map[string]int, len(currencies))
	for i, c := range currencies {
		if !c.IsValid() {
			t.Fatalf("Currencies()[%d] is not a valid currency", i)
		}
		code := c.Code()
		if len(code) != 3 {
			t.Fatalf("Currencies()[%d].Code() = %q, want three letters", i, code)
		}
		for _, r := range code {
			if r < 'A' || r > 'Z' {
				t.Fatalf("Currencies()[%d].Code() = %q, want upper-case ISO 4217 letters", i, code)
			}
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("Currencies() returned %q more than once", code)
		}
		seen[code] = c.Exponent()
		if exp := c.Exponent(); exp < 0 || exp > 4 {
			t.Fatalf("%s exponent = %d, want 0..4", code, exp)
		}
		if got := c.String(); got != code {
			t.Fatalf("Currency(%s).String() = %q, want %q", code, got, code)
		}
		if i > 0 && currencies[i-1].Code() >= code {
			t.Fatalf("Currencies() is not sorted: %q before %q", currencies[i-1].Code(), code)
		}
	}
	for code, want := range required {
		got, ok := seen[code]
		if !ok {
			t.Fatalf("Currencies() is missing required currency %s", code)
		}
		if got != want {
			t.Fatalf("%s exponent = %d, want %d", code, got, want)
		}
	}
	currencies[0] = Currency{}
	if !Currencies()[0].IsValid() {
		t.Fatal("Currencies() returned a slice sharing package state")
	}
}

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		name         string
		code         string
		wantExponent int
		wantErr      error
	}{
		{"usd", "USD", 2, nil},
		{"eur", "EUR", 2, nil},
		{"sek", "SEK", 2, nil},
		{"gbp", "GBP", 2, nil},
		{"jpy", "JPY", 0, nil},
		{"kwd", "KWD", 3, nil},
		{"lower-case rejected", "usd", 0, ErrUnknownCurrency},
		{"empty rejected", "", 0, ErrUnknownCurrency},
		{"unknown rejected", "ZZZ", 0, ErrUnknownCurrency},
		{"two letters rejected", "US", 0, ErrUnknownCurrency},
		{"padded rejected", " USD", 0, ErrUnknownCurrency},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCurrency(tc.code)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseCurrency(%q) error = %v, want %v", tc.code, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got.Code() != tc.code {
				t.Errorf("ParseCurrency(%q).Code() = %q", tc.code, got.Code())
			}
			if got.Exponent() != tc.wantExponent {
				t.Errorf("ParseCurrency(%q).Exponent() = %d, want %d", tc.code, got.Exponent(), tc.wantExponent)
			}
		})
	}
}

func TestMustParseCurrency(t *testing.T) {
	if got := MustParseCurrency("JPY"); got.Exponent() != 0 {
		t.Fatalf("MustParseCurrency(%q).Exponent() = %d, want 0", "JPY", got.Exponent())
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustParseCurrency did not panic on an unknown code")
		}
	}()
	MustParseCurrency("NOPE")
}

func TestPackageCurrencies(t *testing.T) {
	tests := []struct {
		got      Currency
		wantCode string
		wantExp  int
	}{
		{USD, "USD", 2},
		{EUR, "EUR", 2},
		{SEK, "SEK", 2},
		{GBP, "GBP", 2},
		{JPY, "JPY", 0},
		{KWD, "KWD", 3},
	}
	for _, tc := range tests {
		if !tc.got.IsValid() {
			t.Fatalf("%s package value is not valid", tc.wantCode)
		}
		if tc.got.Code() != tc.wantCode || tc.got.Exponent() != tc.wantExp {
			t.Fatalf("%s package value = (%q, %d), want (%q, %d)",
				tc.wantCode, tc.got.Code(), tc.got.Exponent(), tc.wantCode, tc.wantExp)
		}
	}
}

func TestParseDecimal(t *testing.T) {
	tests := []struct {
		name     string
		amount   string
		currency Currency
		want     int64
		wantErr  error
	}{
		{"usd two decimals", "10.00", USD, 1000, nil},
		{"usd integer", "10", USD, 1000, nil},
		{"usd one minor unit", "0.01", USD, 1, nil},
		{"usd negative one minor unit", "-0.01", USD, -1, nil},
		{"usd plus sign", "+1.5", USD, 150, nil},
		{"usd trailing zeros", "10.000", USD, 1000, nil},
		{"jpy integer", "10", JPY, 10, nil},
		{"jpy zero", "0", JPY, 0, nil},
		{"jpy trailing zeros", "100.00", JPY, 100, nil},
		{"kwd three decimals", "1.234", KWD, 1234, nil},
		{"kwd negative one minor unit", "-0.001", KWD, -1, nil},
		{"negative zero", "-0", USD, 0, nil},
		{"negative zero decimals", "-0.00", USD, 0, nil},
		{"usd excess precision", "10.001", USD, 0, ErrExcessPrecision},
		{"kwd excess precision", "1.2345", KWD, 0, ErrExcessPrecision},
		{"jpy fraction", "100.5", JPY, 0, ErrExcessPrecision},
		{"exponent notation", "1e3", USD, 0, ErrInvalidAmount},
		{"upper exponent notation", "1E3", USD, 0, ErrInvalidAmount},
		{"empty", "", USD, 0, ErrInvalidAmount},
		{"leading space", " 1.00", USD, 0, ErrInvalidAmount},
		{"trailing space", "1.00 ", USD, 0, ErrInvalidAmount},
		{"text", "abc", USD, 0, ErrInvalidAmount},
		{"double dot", "1.2.3", USD, 0, ErrInvalidAmount},
		{"max int64", "92233720368547758.07", USD, math.MaxInt64, nil},
		{"min int64", "-92233720368547758.08", USD, math.MinInt64, nil},
		{"max int64 kwd", "9223372036854775.807", KWD, math.MaxInt64, nil},
		{"min int64 kwd", "-9223372036854775.808", KWD, math.MinInt64, nil},
		{"overflow above max", "92233720368547758.08", USD, 0, ErrOverflow},
		{"overflow below min", "-92233720368547758.09", USD, 0, ErrOverflow},
		{"very long fraction", "0." + strings.Repeat("0", 5000) + "1", USD, 0, ErrExcessPrecision},
		{"very long integer", strings.Repeat("9", 5000), USD, 0, ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDecimal(tc.amount, tc.currency)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseDecimal(%q, %s) error = %v, want %v", tc.amount, tc.currency, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got.AmountMinor() != tc.want {
				t.Errorf("ParseDecimal(%q, %s) = %d minor, want %d", tc.amount, tc.currency, got.AmountMinor(), tc.want)
			}
			if got.Currency() != tc.currency {
				t.Errorf("ParseDecimal(%q, %s) currency = %s", tc.amount, tc.currency, got.Currency())
			}
		})
	}
}

func TestParseDecimalUnknownCurrency(t *testing.T) {
	if _, err := ParseDecimal("1.00", Currency{}); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("ParseDecimal with zero Currency error = %v, want ErrUnknownCurrency", err)
	}
}

func TestDecimalStringRoundTrip(t *testing.T) {
	tests := []struct {
		minor    int64
		currency Currency
		want     string
	}{
		{0, USD, "0.00"},
		{1000, USD, "10.00"},
		{-1, USD, "-0.01"},
		{5, USD, "0.05"},
		{10, JPY, "10"},
		{0, JPY, "0"},
		{1234, KWD, "1.234"},
		{-1, KWD, "-0.001"},
		{math.MaxInt64, USD, "92233720368547758.07"},
		{math.MinInt64, USD, "-92233720368547758.08"},
	}
	for _, tc := range tests {
		t.Run(tc.want+" "+tc.currency.Code(), func(t *testing.T) {
			m := FromMinor(tc.minor, tc.currency)
			if got := m.DecimalString(); got != tc.want {
				t.Fatalf("DecimalString() = %q, want %q", got, tc.want)
			}
			if got, want := m.String(), tc.want+" "+tc.currency.Code(); got != want {
				t.Fatalf("String() = %q, want %q", got, want)
			}
			got, err := ParseDecimal(m.DecimalString(), tc.currency)
			if err != nil {
				t.Fatalf("ParseDecimal(%q) error = %v", m.DecimalString(), err)
			}
			if got != m {
				t.Fatalf("parse(format(%d %s)) = %d %s", tc.minor, tc.currency, got.AmountMinor(), got.Currency())
			}
		})
	}
}
