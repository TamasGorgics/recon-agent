package proposal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Canonical returns deterministic JSON for state. Keys are sorted by
// encoding/json, breaks by id, records by ref, every ref/id list inside a
// break is sorted, and arrays inside record fields (notably ledger entries) are
// ordered by their canonical JSON bytes. The projection excludes narrative,
// wall-clock and cases.version fields.
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
			"fields": canonicalFields(fields),
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

// canonicalFields recursively normalizes a record's projected fields. Map keys
// are ordered by encoding/json; array order is replaced by a canonical sort so
// DB row order (for example ledger entry order) cannot change the hash. Arrays
// of bytes are opaque values and are left alone.
func canonicalFields(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, child := range typed {
			normalized[key] = canonicalFields(child)
		}
		return normalized
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array:
		if reflected.Kind() == reflect.Slice && reflected.Type().Elem().Kind() == reflect.Uint8 {
			return value
		}
		items := make([]any, reflected.Len())
		for i := 0; i < reflected.Len(); i++ {
			items[i] = canonicalFields(reflected.Index(i).Interface())
		}
		return sortCanonicalItems(items)
	case reflect.Map:
		if reflected.Type().Key().Kind() != reflect.String {
			return value
		}
		normalized := make(map[string]any, reflected.Len())
		iter := reflected.MapRange()
		for iter.Next() {
			normalized[iter.Key().String()] = canonicalFields(iter.Value().Interface())
		}
		return normalized
	default:
		return value
	}
}

// sortCanonicalItems orders array items by their canonical JSON encoding. Two
// items that encode identically are interchangeable, so the sorted byte
// sequence is deterministic even when they came from different DB rows.
func sortCanonicalItems(items []any) []any {
	type keyedItem struct {
		key   string
		value any
	}
	keyed := make([]keyedItem, len(items))
	for i, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			encoded = []byte(fmt.Sprintf("%v", item))
		}
		keyed[i] = keyedItem{key: string(encoded), value: item}
	}
	sort.SliceStable(keyed, func(i, j int) bool { return keyed[i].key < keyed[j].key })
	ordered := make([]any, len(keyed))
	for i := range keyed {
		ordered[i] = keyed[i].value
	}
	return ordered
}

func sortedRefs(values []string) []string {
	sorted := append([]string{}, values...)
	sort.Strings(sorted)
	return sorted
}
