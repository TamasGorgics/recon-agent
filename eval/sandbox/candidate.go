package sandbox

import (
	"fmt"
	"strings"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// scopeRow is one reconciliation scope read from the clone.
type scopeRow struct {
	ID       string
	Source   string
	Currency string
}

func scopeKey(source, currency string) string {
	return source + "\x00" + currency
}

// scopeIndex indexes scopes by source and currency. The schema's
// UNIQUE(source, currency) makes each key unique.
func scopeIndex(scopes []scopeRow) map[string]scopeRow {
	index := make(map[string]scopeRow, len(scopes))
	for _, scope := range scopes {
		index[scopeKey(scope.Source, scope.Currency)] = scope
	}
	return index
}

// truthScope resolves the single reconciliation scope a truth scenario belongs
// to: the external record's source when one is present, otherwise the source
// encoded in the clearing account.
func truthScope(truth dataset.ScenarioTruth, scopes map[string]scopeRow) (scopeRow, error) {
	if len(truth.Amounts) == 0 {
		return scopeRow{}, fmt.Errorf("%w: scenario %s has no amounts",
			ErrInvalidLoad, truth.ScenarioID)
	}
	currency := truth.Amounts[0].Currency
	source := ""
	if len(truth.ExternalRecords) > 0 {
		source = truth.ExternalRecords[0].Source
		for _, ref := range truth.ExternalRecords {
			if ref.Source != source {
				return scopeRow{}, fmt.Errorf("%w: scenario %s mixes external sources",
					ErrInvalidLoad, truth.ScenarioID)
			}
		}
	}
	if source == "" {
		lower := strings.ToLower(currency)
		for _, account := range truth.Accounts {
			if !strings.HasPrefix(account, "clearing_") || !strings.HasSuffix(account, "_"+lower) {
				continue
			}
			candidate := strings.TrimSuffix(strings.TrimPrefix(account, "clearing_"), "_"+lower)
			if candidate == "" {
				continue
			}
			if _, ok := scopes[scopeKey(candidate, currency)]; ok {
				source = candidate
				break
			}
		}
	}
	if source == "" {
		return scopeRow{}, fmt.Errorf("%w: scenario %s has no resolvable source",
			ErrInvalidLoad, truth.ScenarioID)
	}
	scope, ok := scopes[scopeKey(source, currency)]
	if !ok {
		return scopeRow{}, fmt.Errorf("%w: scenario %s has no scope %s/%s",
			ErrInvalidLoad, truth.ScenarioID, source, currency)
	}
	return scope, nil
}

// reconstructTruthCandidate rebuilds the canonical break candidate that
// produced truth.BreakSignature. The generator's truth carries references and
// amounts but not the side/delta pair, so try the four break sides with deltas
// derived from the scenario amounts. Exactly one candidate must hash to the
// committed signature; zero or multiple fail closed.
func reconstructTruthCandidate(truth dataset.ScenarioTruth, scopes map[string]scopeRow) (match.BreakCandidate, error) {
	if truth.BreakSignature == "" {
		return match.BreakCandidate{}, fmt.Errorf("%w: scenario %s has an empty signature",
			ErrNoTruthCandidate, truth.ScenarioID)
	}
	scope, err := truthScope(truth, scopes)
	if err != nil {
		return match.BreakCandidate{}, err
	}
	currency := truth.Amounts[0].Currency

	ledgerOptions := uniqueLedgerOptions(truth.LedgerTxnIDs)
	recordOptions := uniqueRecordOptions(truth.ExternalRecords)
	deltaOptions := truthDeltaOptions(truth.Amounts)

	matches := make([]match.BreakCandidate, 0, 1)
	for _, side := range []match.BreakSide{
		match.SideLedgerOnly,
		match.SideExternalOnly,
		match.SideAmountMismatch,
		match.SideDuplicate,
	} {
		for _, delta := range deltaOptions {
			for _, ledger := range ledgerOptions {
				for _, records := range recordOptions {
					if len(ledger) == 0 && len(records) == 0 {
						continue
					}
					candidate := match.BreakCandidate{
						ScopeID:         scope.ID,
						Side:            side,
						LedgerTxnIDs:    ledger,
						ExternalRecords: records,
						Currency:        currency,
						DeltaMinor:      delta,
					}
					if candidate.Signature() != truth.BreakSignature {
						continue
					}
					matches = appendUniqueCandidate(matches, candidate)
				}
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return match.BreakCandidate{}, fmt.Errorf(
			"%w: scenario %s signature %s could not be reconstructed",
			ErrNoTruthCandidate, truth.ScenarioID, truth.BreakSignature)
	default:
		return match.BreakCandidate{}, fmt.Errorf(
			"%w: scenario %s signature %s has %d reconstructions",
			ErrAmbiguousMapping, truth.ScenarioID, truth.BreakSignature, len(matches))
	}
}

// syntheticCandidate derives the eval-only external-only break used for an
// injection_memo scenario with no production break.
func syntheticCandidate(truth dataset.ScenarioTruth, scopes map[string]scopeRow) (match.BreakCandidate, error) {
	if truth.Mode != modes.InjectionMemo {
		return match.BreakCandidate{}, fmt.Errorf("%w: scenario %s is not injection_memo",
			ErrInvalidLoad, truth.ScenarioID)
	}
	if len(truth.ExternalRecords) == 0 {
		return match.BreakCandidate{}, fmt.Errorf("%w: injection scenario %s has no external records",
			ErrInvalidLoad, truth.ScenarioID)
	}
	if len(truth.Amounts) == 0 {
		return match.BreakCandidate{}, fmt.Errorf("%w: injection scenario %s has no amounts",
			ErrInvalidLoad, truth.ScenarioID)
	}
	scope, err := truthScope(truth, scopes)
	if err != nil {
		return match.BreakCandidate{}, err
	}
	return match.BreakCandidate{
		ScopeID:         scope.ID,
		Side:            match.SideExternalOnly,
		ExternalRecords: recordRefs(truth.ExternalRecords),
		Currency:        truth.Amounts[0].Currency,
		DeltaMinor:      0,
	}, nil
}

func uniqueLedgerOptions(ids []string) [][]string {
	var out [][]string
	seen := make(map[string]struct{}, 2)
	for _, option := range [][]string{append([]string(nil), ids...), nil} {
		key := strings.Join(option, ",")
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, option)
	}
	return out
}

func uniqueRecordOptions(refs []synth.RecordRef) [][]match.RecordRef {
	var out [][]match.RecordRef
	seen := make(map[string]struct{}, 2)
	for _, option := range [][]synth.RecordRef{refs, nil} {
		converted := recordRefs(option)
		key := recordsKey(converted)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, converted)
	}
	return out
}

func recordRefs(refs []synth.RecordRef) []match.RecordRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]match.RecordRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, match.RecordRef{Source: ref.Source, SourceRecordID: ref.SourceRecordID})
	}
	return out
}

func recordsKey(refs []match.RecordRef) string {
	var builder strings.Builder
	for _, ref := range refs {
		builder.WriteString(ref.Source)
		builder.WriteByte('\x00')
		builder.WriteString(ref.SourceRecordID)
		builder.WriteByte('\x01')
	}
	return builder.String()
}

func truthDeltaOptions(amounts []synth.Amount) []int64 {
	seen := map[int64]struct{}{0: {}}
	add := func(value int64) { seen[value] = struct{}{} }
	for _, amount := range amounts {
		add(amount.AmountMinor)
		add(-amount.AmountMinor)
	}
	for i := range amounts {
		for j := range amounts {
			add(amounts[i].AmountMinor - amounts[j].AmountMinor)
		}
	}
	out := make([]int64, 0, len(seen))
	for delta := range seen {
		out = append(out, delta)
	}
	return out
}

func appendUniqueCandidate(candidates []match.BreakCandidate, candidate match.BreakCandidate) []match.BreakCandidate {
	for _, existing := range candidates {
		if existing.Signature() == candidate.Signature() {
			return candidates
		}
	}
	return append(candidates, candidate)
}
