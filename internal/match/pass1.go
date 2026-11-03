package match

import (
	"context"
	"sort"
)

// ExactPass is matching pass 1: same reference, amount, and currency. It is
// deterministic: a group matches only when exactly one unmatched external
// record and one unmatched ledger transaction share the exact key, so duplicate
// references are left for later passes instead of being linked arbitrarily.
type ExactPass struct{}

// Name returns the stable pass name used in run stats.
func (ExactPass) Name() string { return RuleExact }

type exactKey struct {
	reference string
	amount    int64
	currency  string
}

// Candidates implements Pass.
func (ExactPass) Candidates(_ context.Context, rc *RunContext) ([]Candidate, error) {
	externalByKey := make(map[exactKey][]ExternalRecord)
	for _, record := range rc.UnmatchedExternal() {
		if record.Reference == "" {
			continue
		}
		key := exactKey{reference: record.Reference, amount: record.AmountMinor, currency: record.Currency}
		externalByKey[key] = append(externalByKey[key], record)
	}
	ledgerByKey := make(map[exactKey][]LedgerTxn)
	for _, txn := range rc.UnmatchedLedger() {
		if txn.Reference == "" {
			continue
		}
		key := exactKey{reference: txn.Reference, amount: txn.AmountMinor, currency: txn.Currency}
		ledgerByKey[key] = append(ledgerByKey[key], txn)
	}

	keys := make([]exactKey, 0, len(externalByKey))
	for key, records := range externalByKey {
		if len(records) == 1 && len(ledgerByKey[key]) == 1 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].reference != keys[j].reference {
			return keys[i].reference < keys[j].reference
		}
		if keys[i].amount != keys[j].amount {
			return keys[i].amount < keys[j].amount
		}
		return keys[i].currency < keys[j].currency
	})

	candidates := make([]Candidate, 0, len(keys))
	for _, key := range keys {
		record := externalByKey[key][0]
		txn := ledgerByKey[key][0]
		candidates = append(candidates, Candidate{
			External: record,
			Legs: []Leg{{
				LedgerTxnID: txn.ID,
				AmountMinor: txn.AmountMinor,
				Currency:    txn.Currency,
			}},
			Rule:          RuleExact,
			ConfidenceBPS: ConfidenceExact,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].External.ID < candidates[j].External.ID
	})
	return candidates, nil
}
