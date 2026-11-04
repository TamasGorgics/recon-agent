// Package money provides exact monetary values for the reconciliation agent.
//
// A Money value is an int64 amount in minor units paired with an ISO 4217
// currency. The currency exponent comes from the embedded ISO 4217 table
// (JPY 0, USD 2, KWD 3), never from a hardcoded division. All arithmetic is
// checked: currency mismatches and int64 overflow return typed errors, and
// the package never wraps around.
//
// Decimal strings are accepted only at parse boundaries (ParseDecimal) and
// converted with exact decimal arithmetic. FX rates are exact decimals with a
// maximum scale of 12 and are applied only through Convert. Rounding is
// centralised in Round, which is half-even by default. Allocate splits an
// amount across weighted parts and assigns the residual to a named part.
//
// JSON is {"amount_minor": <integer>, "currency": "<ISO code>"}; fractional or
// string amounts are rejected. The package has no database, clock, or network
// dependency, so it is safe to use from pure generation and parsing code.
package money
