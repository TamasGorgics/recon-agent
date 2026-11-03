// covers: FIN-05
package money

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestMarshalJSON(t *testing.T) {
	tests := []struct {
		name  string
		money Money
		want  string
	}{
		{"usd", mustParseDecimal(t, "10.00", USD), `{"amount_minor":1000,"currency":"USD"}`},
		{"jpy zero", FromMinor(0, JPY), `{"amount_minor":0,"currency":"JPY"}`},
		{"kwd negative", FromMinor(-1234, KWD), `{"amount_minor":-1234,"currency":"KWD"}`},
		{"max", FromMinor(math.MaxInt64, USD), `{"amount_minor":9223372036854775807,"currency":"USD"}`},
		{"min", FromMinor(math.MinInt64, USD), `{"amount_minor":-9223372036854775808,"currency":"USD"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.money)
			if err != nil {
				t.Fatalf("Marshal error = %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMarshalJSONInvalidCurrency(t *testing.T) {
	if _, err := json.Marshal(FromMinor(1, Currency{})); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Marshal invalid currency error = %v, want ErrUnknownCurrency", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	tests := []Money{
		FromMinor(0, USD),
		FromMinor(1000, USD),
		FromMinor(-1, KWD),
		FromMinor(10, JPY),
		FromMinor(math.MaxInt64, SEK),
		FromMinor(math.MinInt64, GBP),
	}
	for _, want := range tests {
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%d %s) error = %v", want.AmountMinor(), want.Currency(), err)
		}
		var got Money
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", data, err)
		}
		if got != want {
			t.Fatalf("JSON round trip = %d %s, want %d %s", got.AmountMinor(), got.Currency(), want.AmountMinor(), want.Currency())
		}
	}
}

func TestUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Money
		wantErr error
	}{
		{"usd", `{"amount_minor":1000,"currency":"USD"}`, FromMinor(1000, USD), nil},
		{"negative kwd", `{"amount_minor":-5,"currency":"KWD"}`, FromMinor(-5, KWD), nil},
		{"jpy zero", `{"amount_minor":0,"currency":"JPY"}`, FromMinor(0, JPY), nil},
		{"empty object", `{}`, Money{}, ErrInvalidJSON},
		{"missing currency", `{"amount_minor":100}`, Money{}, ErrInvalidJSON},
		{"missing amount", `{"currency":"USD"}`, Money{}, ErrInvalidJSON},
		{"null amount", `{"amount_minor":null,"currency":"USD"}`, Money{}, ErrInvalidJSON},
		{"fractional amount", `{"amount_minor":1.5,"currency":"USD"}`, Money{}, ErrInvalidJSON},
		{"string amount", `{"amount_minor":"100","currency":"USD"}`, Money{}, ErrInvalidJSON},
		{"exponent amount", `{"amount_minor":1e2,"currency":"USD"}`, Money{}, ErrInvalidJSON},
		{"unknown currency", `{"amount_minor":100,"currency":"ZZZ"}`, Money{}, ErrInvalidJSON},
		{"lower-case currency", `{"amount_minor":100,"currency":"usd"}`, Money{}, ErrInvalidJSON},
		{"unknown field", `{"amount_minor":100,"currency":"USD","extra":true}`, Money{}, ErrInvalidJSON},
		{"amount overflow", `{"amount_minor":9223372036854775808,"currency":"USD"}`, Money{}, ErrInvalidJSON},
		{"null document", `null`, Money{}, ErrInvalidJSON},
		{"array document", `[]`, Money{}, ErrInvalidJSON},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Money
			err := json.Unmarshal([]byte(tc.in), &got)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Unmarshal(%s) error = %v, want %v", tc.in, err, tc.wantErr)
			}
			if err != nil {
				if got != (Money{}) {
					t.Fatalf("Unmarshal(%s) partially assigned %d %s", tc.in, got.AmountMinor(), got.Currency())
				}
				return
			}
			if got != tc.want {
				t.Fatalf("Unmarshal(%s) = %d %s, want %d %s", tc.in, got.AmountMinor(), got.Currency(), tc.want.AmountMinor(), tc.want.Currency())
			}
		})
	}
}

func TestUnmarshalJSONTrailingData(t *testing.T) {
	var m Money
	if err := json.Unmarshal([]byte(`{"amount_minor":100,"currency":"USD"} junk`), &m); err == nil {
		t.Fatal("Unmarshal accepted trailing data")
	}
	if m != (Money{}) {
		t.Fatalf("Unmarshal partially assigned %d %s", m.AmountMinor(), m.Currency())
	}
}

func TestUnmarshalJSONUnknownCurrencySentinel(t *testing.T) {
	var m Money
	err := json.Unmarshal([]byte(`{"amount_minor":100,"currency":"ZZZ"}`), &m)
	if !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Unmarshal unknown currency error = %v, want it to wrap ErrUnknownCurrency", err)
	}
	if !strings.Contains(err.Error(), "ZZZ") {
		t.Fatalf("Unmarshal unknown currency error = %v, want it to name the code", err)
	}
}

func TestUnmarshalJSONNilReceiver(t *testing.T) {
	var m *Money
	if err := m.UnmarshalJSON([]byte(`{"amount_minor":1,"currency":"USD"}`)); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("nil receiver error = %v, want ErrInvalidJSON", err)
	}
}

func TestUnmarshalJSONDoesNotPartiallyAssign(t *testing.T) {
	original := FromMinor(42, USD)
	got := original
	err := json.Unmarshal([]byte(`{"amount_minor":1,"currency":"EUR","bogus":1}`), &got)
	if err == nil {
		t.Fatal("Unmarshal accepted an unknown field")
	}
	if got != original {
		t.Fatalf("Unmarshal changed the receiver on error: %d %s", got.AmountMinor(), got.Currency())
	}
}
