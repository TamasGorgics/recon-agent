package match

import (
	"context"
	"sort"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// Canonical external record kinds used by passes 3 to 5.
const (
	kindCharge = "charge"
	kindPayout = "payout"
)

// CompositePass is matching pass 3: one external payout against N ledger
// transactions whose signed sum equals the payout exactly.
//
// Declared settlement batch membership is tried first. The batch is the
// source's external_records.batch_id; its members are the other external
// records in the same batch, and their references select the ledger legs. If
// the declared membership is absent, ambiguous, or does not sum exactly, the
// bounded subset-sum fallback runs (subset.go). Two equally valid subsets are
// ambiguous and produce no match. A pool or search that exceeds a configured
// cap produces a break candidate, never a partial match.
type CompositePass struct {
	Config config.MatchConfig
}

// Name returns the stable pass name used in run stats.
func (CompositePass) Name() string { return "composite" }

// Candidates implements Pass.
func (p CompositePass) Candidates(_ context.Context, rc *RunContext) ([]Candidate, error) {
	cfg := p.Config.WithDefaults()
	ledger := rc.UnmatchedLedger()
	byReference := ledgerByReference(ledger)
	members := batchMembers(rc.External())

	candidates := make([]Candidate, 0)
	for _, record := range rc.UnmatchedExternal() {
		if record.Kind != kindPayout {
			continue
		}
		if candidate, matched, blocked := declaredCandidate(record, cfg, members, byReference); matched {
			candidates = append(candidates, candidate)
			continue
		} else if blocked {
			// Declared membership exists but exceeds the leg cap: a break, not
			// a fallback guess from a different subset.
			continue
		}
		if candidate, ok := subsetCandidate(record, cfg, ledger); ok {
			candidates = append(candidates, candidate)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].External.ID < candidates[j].External.ID
	})
	return candidates, nil
}

// BreakCandidates implements BreakPass. Every payout still unmatched after all
// passes is a break: this is the conservative outcome for ambiguous subsets,
// exceeded caps, and payouts with no candidate at all.
func (CompositePass) BreakCandidates(_ context.Context, rc *RunContext) ([]BreakCandidate, error) {
	breaks := make([]BreakCandidate, 0)
	for _, record := range rc.UnmatchedExternal() {
		if record.Kind != kindPayout {
			continue
		}
		breaks = append(breaks, BreakCandidate{
			ScopeID:         rc.Scope().ID,
			Side:            SideExternalOnly,
			ExternalRecords: []RecordRef{{Source: record.Source, SourceRecordID: record.SourceRecordID}},
			Currency:        record.Currency,
		})
	}
	return breaks, nil
}

// ledgerByReference indexes unmatched ledger transactions by reference. A
// reference mapping to more than one transaction is ambiguous and never used
// for declared membership.
func ledgerByReference(ledger []LedgerTxn) map[string][]LedgerTxn {
	byReference := make(map[string][]LedgerTxn, len(ledger))
	for _, txn := range ledger {
		if txn.Reference == "" {
			continue
		}
		byReference[txn.Reference] = append(byReference[txn.Reference], txn)
	}
	return byReference
}

// batchMembers groups every external record in the run snapshot by its declared
// batch. The snapshot (not only the unmatched remainder) is used because a
// batch declaration stays valid after its member records were matched.
func batchMembers(external []ExternalRecord) map[string][]ExternalRecord {
	members := make(map[string][]ExternalRecord)
	for _, record := range external {
		if record.Batch == "" {
			continue
		}
		members[record.Batch] = append(members[record.Batch], record)
	}
	return members
}

// declaredCandidate resolves a payout through its declared batch membership.
// matched reports a valid exact candidate; blocked reports that the declaration
// exists but is too large for the configured leg cap, in which case the caller
// must not fall back to subset-sum.
func declaredCandidate(record ExternalRecord, cfg config.MatchConfig, members map[string][]ExternalRecord, byReference map[string][]LedgerTxn) (candidate Candidate, matched, blocked bool) {
	if record.Batch == "" {
		return Candidate{}, false, false
	}
	references := make([]string, 0)
	seen := make(map[string]struct{})
	for _, member := range members[record.Batch] {
		if member.ID == record.ID || member.Reference == "" {
			continue
		}
		if _, duplicate := seen[member.Reference]; duplicate {
			continue
		}
		seen[member.Reference] = struct{}{}
		references = append(references, member.Reference)
	}
	if len(references) == 0 {
		return Candidate{}, false, false
	}
	sort.Strings(references)

	if len(references) > cfg.CompositeMaxLegs {
		return Candidate{}, false, true
	}
	legs := make([]Leg, 0, len(references))
	var sum int64
	for _, reference := range references {
		txns := byReference[reference]
		if len(txns) != 1 {
			return Candidate{}, false, false
		}
		txn := txns[0]
		if txn.Currency != record.Currency {
			return Candidate{}, false, false
		}
		legs = append(legs, Leg{LedgerTxnID: txn.ID, AmountMinor: txn.AmountMinor, Currency: txn.Currency})
		next, ok := addInt64(sum, txn.AmountMinor)
		if !ok {
			// The true leg sum is outside int64 and can never equal the
			// external total exactly; refuse the declared batch and let the
			// bounded subset fallback (or a break) decide.
			return Candidate{}, false, false
		}
		sum = next
	}
	if sum != record.AmountMinor || len(legs) == 0 {
		return Candidate{}, false, false
	}
	return Candidate{
		External:      record,
		Legs:          legs,
		Rule:          RuleCompositeBatch,
		ConfidenceBPS: ConfidenceComposite,
	}, true, false
}
