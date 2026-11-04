package match

import (
	"context"
	"sort"
)

// ReferenceTolerancePass is matching pass 2: same reference and currency, with
// the absolute amount delta within the configured per-currency fee tolerance.
// The delta is not booked by M1-12; it is the tolerance-match evidence M1-15's
// seeded-bug test relies on.
//
// Like pass 1 it links only when exactly one unmatched external record and one
// unmatched ledger transaction share the reference; otherwise the pair is
// ambiguous and left for later passes or breaks.
type ReferenceTolerancePass struct {
	FeeToleranceMinor map[string]int64
}

// Name returns the stable pass name used in run stats.
func (ReferenceTolerancePass) Name() string { return RuleReferenceTolerance }

// Candidates implements Pass.
func (p ReferenceTolerancePass) Candidates(_ context.Context, rc *RunContext) ([]Candidate, error) {
	externalByRef := make(map[string][]ExternalRecord)
	for _, record := range rc.UnmatchedExternal() {
		if record.Reference == "" {
			continue
		}
		externalByRef[record.Reference] = append(externalByRef[record.Reference], record)
	}
	ledgerByRef := make(map[string][]LedgerTxn)
	for _, txn := range rc.UnmatchedLedger() {
		if txn.Reference == "" {
			continue
		}
		ledgerByRef[txn.Reference] = append(ledgerByRef[txn.Reference], txn)
	}

	references := make([]string, 0, len(externalByRef))
	for reference, records := range externalByRef {
		if len(records) != 1 || len(ledgerByRef[reference]) != 1 {
			continue
		}
		delta, ok := absDelta(records[0].AmountMinor, ledgerByRef[reference][0].AmountMinor)
		if !ok {
			// The true delta does not fit int64, so it can never be within a
			// configured tolerance: leave the pair unmatched.
			continue
		}
		if delta <= p.tolerance(rc.Scope().Currency) {
			references = append(references, reference)
		}
	}
	sort.Strings(references)

	candidates := make([]Candidate, 0, len(references))
	for _, reference := range references {
		record := externalByRef[reference][0]
		txn := ledgerByRef[reference][0]
		candidates = append(candidates, Candidate{
			External: record,
			Legs: []Leg{{
				LedgerTxnID: txn.ID,
				AmountMinor: txn.AmountMinor,
				Currency:    txn.Currency,
			}},
			Rule:          RuleReferenceTolerance,
			ConfidenceBPS: ConfidenceTolerance,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].External.ID < candidates[j].External.ID
	})
	return candidates, nil
}

func (p ReferenceTolerancePass) tolerance(currency string) int64 {
	tolerance := p.FeeToleranceMinor[currency]
	if tolerance < 0 {
		return 0
	}
	return tolerance
}
