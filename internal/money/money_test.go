// covers: FIN-01, FIN-02, FIN-03, FIN-04, FIN-06
package money

import (
	"errors"
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

// mustParseDecimal parses amount in currency or fails the test.
func mustParseDecimal(t *testing.T, amount string, currency Currency) Money {
	t.Helper()
	m, err := ParseDecimal(amount, currency)
	if err != nil {
		t.Fatalf("ParseDecimal(%q, %s): %v", amount, currency, err)
	}
	return m
}

// mustRate parses rate or fails the test.
func mustRate(t *testing.T, rate string) Rate {
	t.Helper()
	r, err := ParseRate(rate)
	if err != nil {
		t.Fatalf("ParseRate(%q): %v", rate, err)
	}
	return r
}

func TestFromMinor(t *testing.T) {
	m := FromMinor(1234, KWD)
	if got := m.AmountMinor(); got != 1234 {
		t.Errorf("AmountMinor() = %d, want 1234", got)
	}
	if got := m.Currency(); got != KWD {
		t.Errorf("Currency() = %s, want KWD", got)
	}
	if m.IsZero() {
		t.Error("IsZero() = true for 1234 minor")
	}
	if !FromMinor(0, JPY).IsZero() {
		t.Error("IsZero() = false for zero")
	}
}

func TestAdd(t *testing.T) {
	tests := []struct {
		name string
		a    int64
		b    int64
		cur  Currency
		want int64
	}{
		{"positive", 100, 250, USD, 350},
		{"negative operands", -100, -250, USD, -350},
		{"opposite signs", 100, -250, USD, -150},
		{"zero identity", 0, 0, JPY, 0},
		{"jpy", 1, 2, JPY, 3},
		{"kwd", 1, 2, KWD, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromMinor(tc.a, tc.cur).Add(FromMinor(tc.b, tc.cur))
			if err != nil {
				t.Fatalf("Add error = %v", err)
			}
			if got.AmountMinor() != tc.want {
				t.Errorf("Add = %d, want %d", got.AmountMinor(), tc.want)
			}
			if got.Currency() != tc.cur {
				t.Errorf("Add currency = %s, want %s", got.Currency(), tc.cur)
			}
		})
	}
}

func TestSub(t *testing.T) {
	tests := []struct {
		name string
		a    int64
		b    int64
		cur  Currency
		want int64
	}{
		{"positive", 250, 100, USD, 150},
		{"to negative", 100, 250, USD, -150},
		{"negative operands", -100, -250, USD, 150},
		{"zero", 5, 5, KWD, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromMinor(tc.a, tc.cur).Sub(FromMinor(tc.b, tc.cur))
			if err != nil {
				t.Fatalf("Sub error = %v", err)
			}
			if got.AmountMinor() != tc.want {
				t.Errorf("Sub = %d, want %d", got.AmountMinor(), tc.want)
			}
		})
	}
}

func TestArithmeticCurrencyMismatch(t *testing.T) {
	a := FromMinor(100, USD)
	b := FromMinor(100, EUR)
	if _, err := a.Add(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add mismatch error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := a.Sub(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sub mismatch error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := a.Cmp(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Cmp mismatch error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestArithmeticInvalidCurrency(t *testing.T) {
	bad := FromMinor(1, Currency{})
	good := FromMinor(1, USD)
	if _, err := bad.Add(good); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Add invalid currency error = %v, want ErrUnknownCurrency", err)
	}
	if _, err := bad.Sub(good); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Sub invalid currency error = %v, want ErrUnknownCurrency", err)
	}
	if _, err := bad.Neg(); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Neg invalid currency error = %v, want ErrUnknownCurrency", err)
	}
	if _, err := bad.Cmp(good); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Cmp invalid currency error = %v, want ErrUnknownCurrency", err)
	}
	if _, err := good.Add(bad); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Add second invalid currency error = %v, want ErrUnknownCurrency", err)
	}
	if _, err := good.Sub(bad); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Sub second invalid currency error = %v, want ErrUnknownCurrency", err)
	}
	if _, err := good.Cmp(bad); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Cmp second invalid currency error = %v, want ErrUnknownCurrency", err)
	}
}

func TestNeg(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want int64
	}{
		{"positive", 5, -5},
		{"negative", -5, 5},
		{"zero", 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromMinor(tc.in, USD).Neg()
			if err != nil {
				t.Fatalf("Neg error = %v", err)
			}
			if got.AmountMinor() != tc.want {
				t.Errorf("Neg = %d, want %d", got.AmountMinor(), tc.want)
			}
		})
	}
}

func TestCmp(t *testing.T) {
	tests := []struct {
		name string
		a    int64
		b    int64
		want int
	}{
		{"less", -5, 5, -1},
		{"equal", 7, 7, 0},
		{"greater", 5, -5, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromMinor(tc.a, USD).Cmp(FromMinor(tc.b, USD))
			if err != nil {
				t.Fatalf("Cmp error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Cmp = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		name    string
		rate    string
		want    string
		wantErr error
	}{
		{"integer", "1", "1", nil},
		{"twelve places", "0.123456789012", "0.123456789012", nil},
		{"trailing zero beyond twelve", "0.1234567890120", "0.123456789012", nil},
		{"thirteen places", "0.1234567890123", "", ErrInvalidRate},
		{"scientific too precise", "1e-13", "", ErrInvalidRate},
		{"scientific at twelve", "1e-12", "0.000000000001", nil},
		{"zero", "0", "0", nil},
		{"negative", "-0.5", "-0.5", nil},
		{"empty", "", "", ErrInvalidRate},
		{"text", "abc", "", ErrInvalidRate},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRate(tc.rate)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseRate(%q) error = %v, want %v", tc.rate, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got.String() != tc.want {
				t.Errorf("ParseRate(%q).String() = %q, want %q", tc.rate, got.String(), tc.want)
			}
		})
	}
}

func TestMustParseRate(t *testing.T) {
	if got := MustParseRate("1.5").String(); got != "1.5" {
		t.Fatalf("MustParseRate(%q).String() = %q, want %q", "1.5", got, "1.5")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustParseRate did not panic on a rate with more than 12 decimal places")
		}
	}()
	MustParseRate("1.2345678901234")
}

func TestRateFromDecimal(t *testing.T) {
	tooPrecise := decimal.RequireFromString("0.1234567890123")
	if _, err := RateFromDecimal(tooPrecise); !errors.Is(err, ErrInvalidRate) {
		t.Fatalf("RateFromDecimal(too precise) error = %v, want ErrInvalidRate", err)
	}
	ok, err := RateFromDecimal(decimal.RequireFromString("0.123456789012"))
	if err != nil {
		t.Fatalf("RateFromDecimal: %v", err)
	}
	if got, want := ok.String(), "0.123456789012"; got != want {
		t.Errorf("RateFromDecimal String() = %q, want %q", got, want)
	}
	if ok.IsZero() {
		t.Error("IsZero() = true for a non-zero rate")
	}
	if !MustParseRate("0").IsZero() {
		t.Error("IsZero() = false for a zero rate")
	}
}

func TestRound(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		exp     int
		mode    RoundMode
		want    int64
		wantErr error
	}{
		{"zero", "0", 0, RoundHalfEven, 0, nil},
		{"positive below half", "1.4", 0, RoundHalfEven, 1, nil},
		{"positive half to even down", "2.5", 0, RoundHalfEven, 2, nil},
		{"positive half to even up", "1.5", 0, RoundHalfEven, 2, nil},
		{"negative half to even down", "-2.5", 0, RoundHalfEven, -2, nil},
		{"negative half to even up", "-1.5", 0, RoundHalfEven, -2, nil},
		{"zero point five to zero", "0.5", 0, RoundHalfEven, 0, nil},
		{"half up positive", "1.5", 0, RoundHalfUp, 2, nil},
		{"half up negative", "-1.5", 0, RoundHalfUp, -2, nil},
		{"toward zero positive", "1.9", 0, RoundTowardZero, 1, nil},
		{"toward zero negative", "-1.9", 0, RoundTowardZero, -1, nil},
		{"away from zero positive", "1.1", 0, RoundAwayFromZero, 2, nil},
		{"away from zero negative", "-1.1", 0, RoundAwayFromZero, -2, nil},
		{"three decimals exact", "1.234", 3, RoundHalfEven, 1234, nil},
		{"three decimals half even down", "1.2345", 3, RoundHalfEven, 1234, nil},
		{"three decimals half even up", "1.2355", 3, RoundHalfEven, 1236, nil},
		{"three decimals half up", "1.2345", 3, RoundHalfUp, 1235, nil},
		{"negative three decimals half even", "-1.2345", 3, RoundHalfEven, -1234, nil},
		{"negative three decimals half up", "-1.2345", 3, RoundHalfUp, -1235, nil},
		{"negative three decimals away", "-1.2341", 3, RoundAwayFromZero, -1235, nil},
		{"negative target exponent", "1234", -2, RoundHalfEven, 12, nil},
		{"max int64 exact", "92233720368547758.07", 2, RoundHalfEven, math.MaxInt64, nil},
		{"min int64 exact", "-92233720368547758.08", 2, RoundHalfEven, math.MinInt64, nil},
		{"invalid mode", "1", 0, RoundMode(99), 0, ErrInvalidRoundingMode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Round(decimal.RequireFromString(tc.in), tc.exp, tc.mode)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Round(%s, %d, %d) error = %v, want %v", tc.in, tc.exp, tc.mode, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got != tc.want {
				t.Errorf("Round(%s, %d, %d) = %d, want %d", tc.in, tc.exp, tc.mode, got, tc.want)
			}
		})
	}
}

func TestConvert(t *testing.T) {
	tests := []struct {
		name    string
		amount  int64
		from    Currency
		rate    string
		to      Currency
		mode    RoundMode
		want    int64
		wantErr error
	}{
		{"usd to eur", 10000, USD, "1.10", EUR, RoundHalfEven, 11000, nil},
		{"jpy to usd", 100, JPY, "0.0067", USD, RoundHalfEven, 67, nil},
		{"usd to jpy", 1000, USD, "150", JPY, RoundHalfEven, 1500, nil},
		{"kwd to usd", 1234, KWD, "3.25", USD, RoundHalfEven, 401, nil},
		{"usd to kwd", 40105, USD, "0.249", KWD, RoundHalfEven, 99861, nil},
		{"identity", 1234, USD, "1", USD, RoundHalfEven, 1234, nil},
		{"zero rate", 1234, USD, "0", EUR, RoundHalfEven, 0, nil},
		{"negative rate", 1000, USD, "-1.5", EUR, RoundHalfEven, -1500, nil},
		{"half even down", 5, JPY, "0.1", JPY, RoundHalfEven, 0, nil},
		{"half even up", 15, JPY, "0.1", JPY, RoundHalfEven, 2, nil},
		{"half up", 5, JPY, "0.1", JPY, RoundHalfUp, 1, nil},
		{"toward zero", 9, JPY, "0.5", JPY, RoundTowardZero, 4, nil},
		{"away from zero", 1, JPY, "0.5", JPY, RoundAwayFromZero, 1, nil},
		{"max int64 unchanged", math.MaxInt64, USD, "1", USD, RoundHalfEven, math.MaxInt64, nil},
		{"invalid target", 100, USD, "1", Currency{}, RoundHalfEven, 0, ErrUnknownCurrency},
		{"invalid source", 100, Currency{}, "1", USD, RoundHalfEven, 0, ErrUnknownCurrency},
		{"invalid mode", 100, USD, "1", EUR, RoundMode(99), 0, ErrInvalidRoundingMode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Convert(FromMinor(tc.amount, tc.from), mustRate(t, tc.rate), tc.to, tc.mode)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Convert error = %v, want %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got.AmountMinor() != tc.want {
				t.Errorf("Convert = %d minor, want %d", got.AmountMinor(), tc.want)
			}
			if got.Currency() != tc.to {
				t.Errorf("Convert currency = %s, want %s", got.Currency(), tc.to)
			}
		})
	}
}

func TestAllocate(t *testing.T) {
	tests := []struct {
		name       string
		minor      int64
		weights    []int64
		residualTo int
		want       []int64
		wantErr    error
	}{
		{"thirds residual first", 1000, []int64{1, 1, 1}, 0, []int64{334, 333, 333}, nil},
		{"thirds residual last", 1000, []int64{1, 1, 1}, 2, []int64{333, 333, 334}, nil},
		{"weighted", 1000, []int64{1, 2, 1}, 1, []int64{250, 500, 250}, nil},
		{"negative amount", -1000, []int64{1, 1, 1}, 1, []int64{-333, -334, -333}, nil},
		{"two units residual", 1001, []int64{1, 1, 1}, 2, []int64{333, 333, 335}, nil},
		{"zero total", 0, []int64{1, 1, 1}, 1, []int64{0, 0, 0}, nil},
		{"zero weight", 100, []int64{0, 1, 1}, 0, []int64{0, 50, 50}, nil},
		{"weights exceeding int64 sum", 100, []int64{math.MaxInt64, math.MaxInt64}, 0, []int64{50, 50}, nil},
		{"empty", 100, nil, 0, nil, ErrInvalidAllocation},
		{"all zero weights", 100, []int64{0, 0}, 0, nil, ErrInvalidAllocation},
		{"negative weight", 100, []int64{1, -1}, 0, nil, ErrInvalidAllocation},
		{"residual below range", 100, []int64{1, 1}, -1, nil, ErrInvalidAllocation},
		{"residual above range", 100, []int64{1, 1}, 2, nil, ErrInvalidAllocation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := Allocate(FromMinor(tc.minor, USD), tc.weights, tc.residualTo)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Allocate error = %v, want %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if len(parts) != len(tc.want) {
				t.Fatalf("Allocate returned %d parts, want %d", len(parts), len(tc.want))
			}
			for i, want := range tc.want {
				if parts[i].AmountMinor() != want {
					t.Errorf("parts[%d] = %d, want %d", i, parts[i].AmountMinor(), want)
				}
				if parts[i].Currency() != USD {
					t.Errorf("parts[%d] currency = %s, want USD", i, parts[i].Currency())
				}
			}
			var sum int64
			for _, p := range parts {
				sum += p.AmountMinor()
			}
			if sum != tc.minor {
				t.Errorf("Allocate parts sum to %d, want %d", sum, tc.minor)
			}
		})
	}
}

func TestAllocateInvalidCurrency(t *testing.T) {
	if _, err := Allocate(FromMinor(100, Currency{}), []int64{1}, 0); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Allocate invalid currency error = %v, want ErrUnknownCurrency", err)
	}
}
