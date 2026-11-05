package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/money"
)

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// IsUUID reports whether value is a canonical 8-4-4-4-12 hex UUID.
func IsUUID(value string) bool {
	return uuidPattern.MatchString(value)
}

// ParseUUID validates a UUID before it is used as a $n::uuid parameter, so a
// malformed id never reaches Postgres as a 22P02 error.
func ParseUUID(value string) (string, error) {
	if !IsUUID(value) {
		return "", fmt.Errorf("%w: %q is not a UUID", ErrInvalidArguments, value)
	}
	return value, nil
}

// ParseDate parses a strict YYYY-MM-DD business date as UTC midnight.
func ParseDate(value string) (time.Time, error) {
	if !datePattern.MatchString(value) {
		return time.Time{}, fmt.Errorf("%w: %q is not a YYYY-MM-DD date", ErrInvalidArguments, value)
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %q is not a valid date", ErrInvalidArguments, value)
	}
	return parsed, nil
}

// ParseCurrency validates an upper-case ISO 4217 code against the money
// package's supported set.
func ParseCurrency(value string) (money.Currency, error) {
	currency, err := money.ParseCurrency(value)
	if err != nil {
		return money.Currency{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	return currency, nil
}

// FormatDate renders a business date as YYYY-MM-DD in UTC.
func FormatDate(value time.Time) string {
	return value.UTC().Format("2006-01-02")
}

// FormatTimestamp renders a timestamp as RFC3339 in UTC.
func FormatTimestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

// DecodeArgs decodes a tool request into a typed struct with unknown fields
// rejected. The schema remains the primary contract; this is defense in depth
// for handlers.
func DecodeArgs(data json.RawMessage, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("tools: decode args: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("tools: decode args: trailing data")
	}
	return nil
}

// DecodeJSONB decodes a JSONB column into dst.
func DecodeJSONB(data []byte, dst any) error {
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("tools: decode JSONB: %w", err)
	}
	return nil
}

// EncodeJSON serializes a value deterministically (encoding/json sorts map
// keys) for a JSONB column or a result envelope.
func EncodeJSON(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("tools: encode JSON: %w", err)
	}
	return data, nil
}

// SortedUnique returns a sorted copy without duplicates. The result is never
// nil.
func SortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

// CollectRows scans every row of a query result with scan and returns the
// rows, closing the result set. It is the shared row-scanning helper for the
// read tools; callers still own the per-tool row caps and truncation flag.
func CollectRows[T any](rows pgx.Rows, scan func(pgx.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	values := make([]T, 0)
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("tools: scan row: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tools: iterate rows: %w", err)
	}
	return values, nil
}

// ListUntrustedFields returns the sorted unique fields of an external record
// that must be treated as untrusted. It starts from the configured list for
// the record's source (falling back to config.DefaultSynthIngestSources when
// the configured map is empty), keeps only fields present with a non-null
// value, and adds reference/bank_reference when those keys are present.
func ListUntrustedFields(source string, raw map[string]any, configured map[string][]string) []string {
	fields := []string{}
	configuredFields := configured[source]
	if len(configured) == 0 {
		if defaults, ok := config.DefaultSynthIngestSources()[source]; ok {
			configuredFields = defaults.UntrustedFields
		}
	}
	for _, field := range configuredFields {
		if value, present := raw[field]; present && value != nil {
			fields = append(fields, field)
		}
	}
	for _, field := range []string{"reference", "bank_reference"} {
		if _, present := raw[field]; present {
			fields = append(fields, field)
		}
	}
	return SortedUnique(fields)
}

// StringOrEmpty dereferences an optional string for JSON output.
func StringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// OptionalTimestamp formats an optional timestamp as RFC3339 UTC or null.
func OptionalTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := FormatTimestamp(*value)
	return &formatted
}

// OptionalDate formats an optional business date as YYYY-MM-DD or null.
func OptionalDate(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := FormatDate(*value)
	return &formatted
}

// TrimmedString returns value with surrounding whitespace removed; it is a
// small helper for request fields that must be non-empty after trimming.
func TrimmedString(value string) string {
	return strings.TrimSpace(value)
}
