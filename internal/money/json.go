package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MarshalJSON encodes Money as {"amount_minor": <integer>, "currency": "<ISO>"}.
// The amount is always an integer number of minor units; an invalid currency
// is an error rather than an empty string.
func (m Money) MarshalJSON() ([]byte, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
	}{
		AmountMinor: m.amount,
		Currency:    m.currency.code,
	})
}

// UnmarshalJSON strictly decodes {"amount_minor": <integer>, "currency":
// "<ISO>"}. Unknown or missing fields, fractional or string amounts, an
// unknown currency, and trailing data are rejected. The receiver is unchanged
// on error.
func (m *Money) UnmarshalJSON(data []byte) error {
	if m == nil {
		return fmt.Errorf("%w: nil receiver", ErrInvalidJSON)
	}
	var raw struct {
		AmountMinor *int64  `json:"amount_minor"`
		Currency    *string `json:"currency"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after money object", ErrInvalidJSON)
	}
	if raw.AmountMinor == nil {
		return fmt.Errorf("%w: missing amount_minor", ErrInvalidJSON)
	}
	if raw.Currency == nil {
		return fmt.Errorf("%w: missing currency", ErrInvalidJSON)
	}
	currency, err := ParseCurrency(*raw.Currency)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	*m = FromMinor(*raw.AmountMinor, currency)
	return nil
}
