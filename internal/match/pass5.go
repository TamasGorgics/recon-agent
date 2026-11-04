package match

import (
	"context"
	"sort"
	"strconv"
	"time"
)

// DuplicatePass is matching pass 5. It never produces a match: repeated source
// record ids and same-amount/same-time charges are reported as duplicate break
// candidates for a human or the agent to resolve (spec section 5, D-5). Both
// signals are grouped over records still unmatched after passes 1 to 4, and the
// groups are visited in sorted key order so the break set is deterministic.
type DuplicatePass struct{}

// Name returns the stable pass name used in run stats.
func (DuplicatePass) Name() string { return RuleDuplicate }

// Candidates implements Pass. Duplicates never auto-link.
func (DuplicatePass) Candidates(context.Context, *RunContext) ([]Candidate, error) {
	return nil, nil
}

// BreakCandidates implements BreakPass.
func (DuplicatePass) BreakCandidates(_ context.Context, rc *RunContext) ([]BreakCandidate, error) {
	unmatched := rc.UnmatchedExternal()
	breaks := make([]BreakCandidate, 0)

	// Repeated external ids: same source and source_record_id. The database
	// unique key normally prevents this, but a snapshot assembled from mixed
	// batches, replays, or future sources may still contain it.
	byID := make(map[string][]ExternalRecord)
	for _, record := range unmatched {
		key := record.Source + "\x00" + record.SourceRecordID
		byID[key] = append(byID[key], record)
	}
	for _, key := range sortedKeys(byID) {
		group := byID[key]
		if len(group) < 2 {
			continue
		}
		breaks = append(breaks, duplicateBreak(rc.Scope().ID, group))
	}

	// Same amount at the same instant for charges: the classic duplicate charge.
	byAmountTime := make(map[string][]ExternalRecord)
	for _, record := range unmatched {
		if record.Kind != kindCharge || record.OccurredAt.IsZero() {
			continue
		}
		key := record.Currency + "|" + strconv.FormatInt(record.AmountMinor, 10) + "|" +
			record.OccurredAt.UTC().Format(time.RFC3339Nano)
		byAmountTime[key] = append(byAmountTime[key], record)
	}
	for _, key := range sortedKeys(byAmountTime) {
		group := byAmountTime[key]
		if len(group) < 2 {
			continue
		}
		breaks = append(breaks, duplicateBreak(rc.Scope().ID, group))
	}

	sort.SliceStable(breaks, func(i, j int) bool {
		return breaks[i].Signature() < breaks[j].Signature()
	})
	return breaks, nil
}

func duplicateBreak(scopeID string, group []ExternalRecord) BreakCandidate {
	refs := make([]RecordRef, 0, len(group))
	for _, record := range group {
		refs = append(refs, RecordRef{Source: record.Source, SourceRecordID: record.SourceRecordID})
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].Source != refs[j].Source {
			return refs[i].Source < refs[j].Source
		}
		return refs[i].SourceRecordID < refs[j].SourceRecordID
	})
	return BreakCandidate{
		ScopeID:         scopeID,
		Side:            SideDuplicate,
		ExternalRecords: refs,
		Currency:        group[0].Currency,
	}
}

func sortedKeys[T any](values map[string][]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
