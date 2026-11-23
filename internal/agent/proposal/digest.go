package proposal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// NormalizeConfidence renders a stored or decoded confidence value as the fixed
// four-decimal-place string the digest covers. It accepts a decimal string so a
// stored NUMERIC(5,4) column (for example 0.8500) and a tool-argument literal
// (for example 0.85) hash identically.
func NormalizeConfidence(value string) (string, error) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("proposal: confidence is not a number")
	}
	return parsed.StringFixed(4), nil
}

// Digest returns the lowercase hex SHA-256 of the canonical proposal document:
//
//	{type, payload, rationale, evidence(sorted unique), confidence(4dp),
//	 pattern_claimed}
//
// state_hash, idempotency_key, run_id, timestamps and id are deliberately
// excluded. Object keys are sorted at every level, arrays inside payload keep
// their stored order, no insignificant whitespace or HTML escaping is emitted,
// and pattern_claimed serializes null and "" as distinct values.
func Digest(f Fields) (string, error) {
	evidence := tools.SortedUnique(f.Evidence)
	document := map[string]any{
		"type":            f.Type,
		"payload":         f.Payload,
		"rationale":       f.Rationale,
		"evidence":        evidence,
		"confidence":      f.Confidence,
		"pattern_claimed": f.PatternClaimed,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("proposal: encode digest input: %w", err)
	}
	canonical, err := tools.CanonicalJSON(encoded)
	if err != nil {
		return "", fmt.Errorf("proposal: canonicalize digest input: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
