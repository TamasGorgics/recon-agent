package proposal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// buildPayload returns the canonical stored payload for a request. Lines are
// sorted by (currency, account_code, direction, amount_minor) so reordering
// cannot create a second idempotency key.
func buildPayload(req recordRequest) (map[string]any, error) {
	switch req.kind {
	case "link":
		if req.linkSet {
			ids := append([]string(nil), req.ledgerTxnIDs...)
			sort.Strings(ids)
			return map[string]any{
				"ledger_txn_ids": ids,
				"external_ref":   req.externalRef,
			}, nil
		}
		return map[string]any{
			"ledger_txn_id": req.ledgerTxnID,
			"external_ref":  req.externalRef,
		}, nil
	case "adjustment", "write_off":
		lines := make([]map[string]any, 0, len(req.lines))
		for _, line := range req.lines {
			lines = append(lines, map[string]any{
				"account_code": line.AccountCode,
				"direction":    line.Direction,
				"amount_minor": line.AmountMinor,
				"currency":     line.Currency,
			})
		}
		sort.Slice(lines, func(i, j int) bool {
			return lineSortKey(lines[i]) < lineSortKey(lines[j])
		})
		return map[string]any{"lines": lines}, nil
	case "duplicate":
		payload := map[string]any{"external_ref": req.externalRef}
		if req.duplicateOf != "" {
			payload["duplicate_of"] = req.duplicateOf
		}
		return payload, nil
	case "request_info":
		return map[string]any{"question": req.question}, nil
	default:
		return nil, fmt.Errorf("proposal: unknown proposal type %q", req.kind)
	}
}

func lineSortKey(line map[string]any) string {
	return fmt.Sprintf("%v\x00%v\x00%v\x00%v",
		line["currency"], line["account_code"], line["direction"], line["amount_minor"])
}

// idempotencyKey is SHA-256 hex over the canonical key object. Confidence is
// the exact four-fractional-digit string that is stored.
func idempotencyKey(caseID, runID, kind string, payload map[string]any, rationale string, evidence []string, confidence, pattern string) (string, error) {
	input := map[string]any{
		"case_id":         caseID,
		"run_id":          runID,
		"type":            kind,
		"payload":         payload,
		"rationale":       rationale,
		"evidence":        tools.SortedUnique(evidence),
		"confidence":      confidence,
		"pattern_claimed": pattern,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("proposal: encode idempotency input: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
