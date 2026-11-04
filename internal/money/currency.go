package money

import (
	"fmt"
	"sort"
)

// Currency is an ISO 4217 currency code paired with its minor-unit exponent.
// The zero value is not a valid currency; construct values with ParseCurrency,
// MustParseCurrency, or Currencies.
type Currency struct {
	code     string
	exponent int8
}

// currencyTable is the embedded ISO 4217 subset supported by the agent. The
// M1-03 currencies seed is generated from Currencies(), so this table is the
// single source of truth for codes and exponents.
var currencyTable = map[string]int8{
	"AED": 2,
	"AUD": 2,
	"BGN": 2,
	"BHD": 3,
	"BRL": 2,
	"CAD": 2,
	"CHF": 2,
	"CLP": 0,
	"CNY": 2,
	"COP": 2,
	"CZK": 2,
	"DKK": 2,
	"EGP": 2,
	"EUR": 2,
	"GBP": 2,
	"HKD": 2,
	"HUF": 2,
	"IDR": 2,
	"ILS": 2,
	"INR": 2,
	"ISK": 0,
	"JOD": 3,
	"JPY": 0,
	"KES": 2,
	"KRW": 0,
	"KWD": 3,
	"MXN": 2,
	"MYR": 2,
	"NGN": 2,
	"NOK": 2,
	"NZD": 2,
	"OMR": 3,
	"PHP": 2,
	"PKR": 2,
	"PLN": 2,
	"QAR": 2,
	"RON": 2,
	"SAR": 2,
	"SEK": 2,
	"SGD": 2,
	"THB": 2,
	"TND": 3,
	"TRY": 2,
	"TWD": 2,
	"UAH": 2,
	"USD": 2,
	"VND": 0,
	"ZAR": 2,
}

// Currency values for the codes required by the M1-01 contract.
var (
	USD = MustParseCurrency("USD")
	EUR = MustParseCurrency("EUR")
	SEK = MustParseCurrency("SEK")
	GBP = MustParseCurrency("GBP")
	JPY = MustParseCurrency("JPY")
	KWD = MustParseCurrency("KWD")
)

// ParseCurrency returns the currency for an exact, upper-case ISO 4217 code.
// Unknown codes and lower-case variants are rejected; callers must not guess.
func ParseCurrency(code string) (Currency, error) {
	exponent, ok := currencyTable[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return Currency{code: code, exponent: exponent}, nil
}

// MustParseCurrency is ParseCurrency for package-level constants and tests.
// It panics on an unknown code.
func MustParseCurrency(code string) Currency {
	currency, err := ParseCurrency(code)
	if err != nil {
		panic(err)
	}
	return currency
}

// Currencies returns every supported currency sorted by code. The returned
// slice is a copy; mutating it does not affect the package.
func Currencies() []Currency {
	currencies := make([]Currency, 0, len(currencyTable))
	for code, exponent := range currencyTable {
		currencies = append(currencies, Currency{code: code, exponent: exponent})
	}
	sort.Slice(currencies, func(i, j int) bool { return currencies[i].code < currencies[j].code })
	return currencies
}

// Code returns the ISO 4217 code.
func (c Currency) Code() string { return c.code }

// Exponent returns the number of decimal digits in one major unit.
func (c Currency) Exponent() int { return int(c.exponent) }

// String returns the ISO 4217 code.
func (c Currency) String() string { return c.code }

// IsValid reports whether c came from the embedded ISO 4217 table.
func (c Currency) IsValid() bool {
	exponent, ok := currencyTable[c.code]
	return ok && exponent == c.exponent
}
