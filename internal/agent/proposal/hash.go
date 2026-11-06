package proposal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Canonical returns deterministic JSON for state. Keys are sorted by
// encoding/json, breaks by id, records by ref, and every ref/id list inside a
// break is sorted. The projection excludes narrative, wall-clock and
// cases.version fields.
func Canonical(state State) ([]byte, error) {
	return json.Marshal(canonicalValue(state))
}

// Hash returns the lowercase hex SHA-256 of Canonical(state).
func Hash(state State) (string, error) {
	canonical, err := Canonical(state)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalValue(state State) map[string]any {
	breaks := append([]BreakState(nil), state.Breaks...)
	sort.Slice(breaks, func(i, j int) bool { return breaks[i].ID < breaks[j].ID })
	breakValues := make([]any, 0, len(breaks))
	for _, item := range breaks {
		breakValues = append(breakValues, map[string]any{
			"id":             item.ID,
			"signature":      item.Signature,
			"side":           item.Side,
			"status":         item.Status,
			"delta_minor":    item.DeltaMinor,
			"currency":       item.Currency,
			"ledger_txn_ids": sortedRefs(item.LedgerTxnIDs),
			"external_refs":  sortedRefs(item.ExternalRefs),
		})
	}

	records := append([]RecordState(nil), state.Records...)
	sort.Slice(records, func(i, j int) bool { return records[i].Ref < records[j].Ref })
	recordValues := make([]any, 0, len(records))
	for _, item := range records {
		fields := item.Fields
		if fields == nil {
			fields = map[string]any{}
		}
		recordValues = append(recordValues, map[string]any{
			"ref":    item.Ref,
			"kind":   item.Kind,
			"fields": fields,
		})
	}

	return map[string]any{
		"case": map[string]any{
			"id":              state.Case.ID,
			"status":          state.Case.Status,
			"resolution_type": state.Case.ResolutionType,
		},
		"breaks":  breakValues,
		"records": recordValues,
	}
}

func sortedRefs(values []string) []string {
	sorted := append([]string{}, values...)
	sort.Strings(sorted)
	return sorted
}
