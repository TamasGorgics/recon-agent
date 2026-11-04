package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const dateLayout = "2006-01-02"

// hashLine is the canonical shape of one entry for request hashing. Field order
// is fixed so encoding/json emits a stable object.
type hashLine struct {
	AccountID   string         `json:"account_id"`
	Direction   string         `json:"direction"`
	AmountMinor int64          `json:"amount_minor"`
	Currency    string         `json:"currency"`
	ExternalRef string         `json:"external_ref,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type hashPayload struct {
	Kind        string   `json:"kind"`
	ValueDate   string   `json:"value_date"`
	Description string   `json:"description"`
	ReversalOf  string   `json:"reversal_of,omitempty"`
	Lines       []string `json:"lines"`
}

// requestHash returns the canonical SHA-256 hex digest of the request payload.
// Lines are sorted, so line order never changes identity; timestamps and the
// caller-supplied transaction id are deliberately excluded because a retry
// generated them again (D-7). Kind, value date, description, reversal link, and
// every line field are included, so any meaningful payload change under the
// same idempotency key is a conflict (FIN-12).
func requestHash(req PostRequest) (string, error) {
	lines := make([]string, 0, len(req.Lines))
	for i, line := range req.Lines {
		encoded, err := json.Marshal(hashLine{
			AccountID:   line.AccountID,
			Direction:   string(line.Direction),
			AmountMinor: line.AmountMinor,
			Currency:    line.Currency,
			ExternalRef: line.ExternalRef,
			Metadata:    line.Metadata,
		})
		if err != nil {
			return "", fmt.Errorf("%w: line %d metadata is not JSON-encodable: %w", ErrInvalidLine, i, err)
		}
		lines = append(lines, string(encoded))
	}
	sort.Strings(lines)

	payload, err := json.Marshal(hashPayload{
		Kind:        string(req.Kind),
		ValueDate:   req.ValueDate.UTC().Format(dateLayout),
		Description: req.Description,
		ReversalOf:  req.ReversalOf,
		Lines:       lines,
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode request hash payload: %w", ErrInvalidRequest, err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
