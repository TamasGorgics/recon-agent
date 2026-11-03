package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// zeroPrevHash is the genesis prev_hash: 32 zero bytes (SHA-256 width).
func zeroPrevHash() []byte { return make([]byte, sha256.Size) }

// sha256Sum returns the raw 32-byte digest of b.
func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// formatTimestamp is the canonical timestamp encoding in the hash input. It
// truncates to microsecond precision so a value that round-trips through
// Postgres TIMESTAMPTZ hashes identically on Verify.
func formatTimestamp(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

// canonicalJSON returns compact JSON with object keys sorted lexicographically
// and no insignificant whitespace. It decodes JSON with UseNumber first so
// integer amounts do not pass through float64.
func canonicalJSON(v any) ([]byte, error) {
	normalized, err := normalizeJSON(v)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("audit: encode canonical json: %w", err)
	}
	return encoded, nil
}

// normalizeJSON converts v (a Go value or JSON bytes) into the generic decoded
// form that json.Marshal renders canonically. JSON is decoded with UseNumber
// so integer literals round-trip exactly.
func normalizeJSON(v any) (any, error) {
	if v == nil {
		return nil, nil
	}

	var raw []byte
	switch value := v.(type) {
	case json.RawMessage:
		raw = []byte(value)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("audit: encode json payload: %w", err)
		}
		raw = encoded
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("audit: decode json payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("audit: decode json payload: trailing data")
		}
		return nil, fmt.Errorf("audit: decode json payload: %w", err)
	}
	return decoded, nil
}

// computeRowHash hashes one audit row. The input is canonical JSON over the
// hashed columns: seq, ts, actor_type, actor_id, action, entity_type,
// entity_id, before, after, trace_id, and prev_hash. Peers are validated
// elsewhere; this function only enforces the 32-byte prev_hash invariant.
func computeRowHash(
	seq int64,
	ts time.Time,
	actorType, actorID, action, entityType, entityID string,
	before, after any,
	traceID *string,
	prevHash []byte,
) ([]byte, error) {
	if len(prevHash) != sha256.Size {
		return nil, fmt.Errorf("audit: prev_hash length = %d, want %d", len(prevHash), sha256.Size)
	}
	input, err := canonicalJSON(map[string]any{
		"seq":         seq,
		"ts":          formatTimestamp(ts),
		"actor_type":  actorType,
		"actor_id":    actorID,
		"action":      action,
		"entity_type": entityType,
		"entity_id":   entityID,
		"before":      before,
		"after":       after,
		"trace_id":    traceID,
		"prev_hash":   hex.EncodeToString(prevHash),
	})
	if err != nil {
		return nil, err
	}
	return sha256Sum(input), nil
}
