package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// CanonicalJSON returns the canonical encoding of one JSON value: object keys
// are sorted lexicographically at every level, arrays keep their order, output
// carries no insignificant whitespace, and HTML-sensitive characters are not
// escaped. Input is decoded with json.Decoder.UseNumber so numeric literals are
// preserved exactly instead of being converted to float64; null and "" remain
// distinct values.
//
// The function is used both when recording a tool result and when replaying a
// loaded tool_calls.result. Callers that hash a database column should feed the
// JSONB textual representation (for example, `result::text`): PostgreSQL
// normalises JSONB numbers and escapes, and canonicalising that same text on
// both paths keeps the hash stable across a database round-trip.
func CanonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("tools: canonicalize JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("tools: canonicalize JSON: trailing data")
		}
		return nil, fmt.Errorf("tools: canonicalize JSON: %w", err)
	}

	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("tools: canonicalize JSON: %w", err)
	}
	// json.Encoder terminates the document with a newline; it is not part of
	// the canonical value.
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), nil
}

// CanonicalJSONHash returns the lowercase hex SHA-256 of CanonicalJSON(raw).
func CanonicalJSONHash(raw []byte) (string, error) {
	canonical, err := CanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
