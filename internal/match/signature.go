package match

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// BreakSide is the break classification used by BreakCandidate.Signature. The
// values are the four sides D-5 names; M1-13 and M1-14 write breaks from them.
type BreakSide string

// Break sides from M1 spec D-5.
const (
	SideLedgerOnly     BreakSide = "ledger_only"
	SideExternalOnly   BreakSide = "external_only"
	SideAmountMismatch BreakSide = "amount_mismatch"
	SideDuplicate      BreakSide = "duplicate"
)

// RecordRef identifies one external record by its stable source key.
type RecordRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

// BreakCandidate is the canonical input to a break signature. It deliberately
// excludes run ID, detection time, and status, so re-running the matcher over
// unchanged data produces the same signature and ON CONFLICT (signature) DO
// NOTHING deduplicates breaks (D-5).
type BreakCandidate struct {
	ScopeID         string
	Side            BreakSide
	LedgerTxnIDs    []string
	ExternalRecords []RecordRef
	Currency        string
	DeltaMinor      int64
}

type canonicalBreak struct {
	ScopeID         string      `json:"scope_id"`
	Side            BreakSide   `json:"side"`
	LedgerTxnIDs    []string    `json:"ledger_txn_ids"`
	ExternalRecords []RecordRef `json:"external_records"`
	Currency        string      `json:"currency"`
	DeltaMinor      int64       `json:"delta_minor"`
}

// Signature returns the canonical SHA-256 hex digest of the candidate. Ledger
// IDs and external (source, source_record_id) pairs are sorted and de-duplicated
// on copies, so field-order permutations of the input slices are stable and the
// caller's slices are never mutated. The canonical JSON uses a fixed field
// order; run ID, detection time, and status are not part of the input.
func (c BreakCandidate) Signature() string {
	ledgerIDs := append([]string(nil), c.LedgerTxnIDs...)
	sort.Strings(ledgerIDs)
	ledgerIDs = dedupeStrings(ledgerIDs)

	records := append([]RecordRef(nil), c.ExternalRecords...)
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Source != records[j].Source {
			return records[i].Source < records[j].Source
		}
		return records[i].SourceRecordID < records[j].SourceRecordID
	})
	records = dedupeRecordRefs(records)

	canonical, err := json.Marshal(canonicalBreak{
		ScopeID:         c.ScopeID,
		Side:            c.Side,
		LedgerTxnIDs:    ledgerIDs,
		ExternalRecords: records,
		Currency:        c.Currency,
		DeltaMinor:      c.DeltaMinor,
	})
	if err != nil {
		// The canonical struct contains only strings and an int64, so json
		// cannot fail. Keep the signature total rather than returning an error
		// callers would have to thread through every break writer.
		panic("match: canonical break encoding failed: " + err.Error())
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func dedupeRecordRefs(values []RecordRef) []RecordRef {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		last := out[len(out)-1]
		if value.Source != last.Source || value.SourceRecordID != last.SourceRecordID {
			out = append(out, value)
		}
	}
	return out
}
