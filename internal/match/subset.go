package match

import (
	"sort"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// subsetCandidate runs pass 3's bounded subset-sum fallback for one payout.
//
// The pool is deterministic and small by construction: unmatched ledger
// transactions in the scope currency, with the same sign as the payout, at most
// the payout magnitude, and a value date within CompositeDateWindowDays. The
// pool size is capped by CompositeCandidateLimit, the subset size by
// CompositeMaxLegs, and the search by CompositeMaxNodes. Exceeding any cap means
// no match; the payout becomes a break candidate in BreakCandidates.
func subsetCandidate(record ExternalRecord, cfg config.MatchConfig, ledger []LedgerTxn) (Candidate, bool) {
	target, ok := absInt64(record.AmountMinor)
	if !ok {
		// The payout magnitude is not representable, so no exact subset can be
		// proven; leave it for a break rather than wrapping the target.
		return Candidate{}, false
	}
	if target == 0 {
		return Candidate{}, false
	}

	pool := make([]LedgerTxn, 0)
	for _, txn := range ledger {
		if txn.Currency != record.Currency || txn.AmountMinor == 0 {
			continue
		}
		if (record.AmountMinor > 0) != (txn.AmountMinor > 0) {
			continue
		}
		amount, ok := absInt64(txn.AmountMinor)
		if !ok {
			// |MinInt64| exceeds every representable positive target, so this
			// transaction cannot be part of an exact subset.
			continue
		}
		if amount > target {
			continue
		}
		if dayDistance(txn.ValueDate, record.ValueDate) > cfg.CompositeDateWindowDays {
			continue
		}
		pool = append(pool, txn)
	}
	if len(pool) == 0 || len(pool) > cfg.CompositeCandidateLimit {
		return Candidate{}, false
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].ID < pool[j].ID })

	amounts := make([]int64, len(pool))
	for i, txn := range pool {
		amount, ok := absInt64(txn.AmountMinor)
		if !ok {
			// The pool filter above already excludes MinInt64; keep this guard
			// total so a future pool change cannot introduce a wrapped amount.
			return Candidate{}, false
		}
		amounts[i] = amount
	}

	solutions := subsetSum(amounts, target, cfg.CompositeMaxLegs, cfg.CompositeMaxNodes)
	// Two or more valid subsets are ambiguous: never pick one arbitrarily.
	if len(solutions) != 1 {
		return Candidate{}, false
	}
	legs := make([]Leg, 0, len(solutions[0]))
	for _, index := range solutions[0] {
		txn := pool[index]
		legs = append(legs, Leg{LedgerTxnID: txn.ID, AmountMinor: txn.AmountMinor, Currency: txn.Currency})
	}
	sort.SliceStable(legs, func(i, j int) bool { return legs[i].LedgerTxnID < legs[j].LedgerTxnID })
	return Candidate{
		External:      record,
		Legs:          legs,
		Rule:          RuleCompositeSubset,
		ConfidenceBPS: ConfidenceComposite,
	}, true
}

// subsetSum returns up to two index subsets of amounts that sum to target. The
// search is a depth-first include/exclude walk in pool order, pruned by the
// remaining suffix sum and bounded by maxLegs and maxNodes. Every amount is
// positive and paths longer than maxLegs are pruned, so a completed search is
// exhaustive within the configured caps.
//
// The node budget is a hard failure: if the walk exhausts maxNodes, subsetSum
// returns nil even when it already found a solution. A truncated search cannot
// prove that subset is unique, so callers must turn any cap hit into a break,
// never a partial or best-effort match.
func subsetSum(amounts []int64, target int64, maxLegs, maxNodes int) [][]int {
	if len(amounts) == 0 || target <= 0 {
		return nil
	}
	// suffix[i] is the exact sum of amounts[i:] when exactSuffix[i] is true.
	// Once a suffix sum leaves int64 it is an upper bound that exceeds every
	// representable remaining target; exactSuffix records that so the pruning
	// test can simply be skipped instead of relying on a clamped value.
	suffix := make([]int64, len(amounts)+1)
	exactSuffix := make([]bool, len(amounts)+1)
	exactSuffix[len(amounts)] = true
	for i := len(amounts) - 1; i >= 0; i-- {
		if !exactSuffix[i+1] {
			continue
		}
		sum, ok := addInt64(suffix[i+1], amounts[i])
		if !ok {
			continue
		}
		suffix[i] = sum
		exactSuffix[i] = true
	}
	if exactSuffix[0] && suffix[0] < target {
		return nil
	}

	solutions := make([][]int, 0, 2)
	nodes := 0
	capped := false
	var walk func(index int, remaining int64, chosen []int)
	walk = func(index int, remaining int64, chosen []int) {
		if capped || len(solutions) >= 2 {
			return
		}
		nodes++
		if nodes > maxNodes {
			capped = true
			return
		}
		if remaining == 0 {
			if len(chosen) >= 2 {
				solutions = append(solutions, append([]int(nil), chosen...))
			}
			return
		}
		if index >= len(amounts) || len(chosen) >= maxLegs || remaining < 0 {
			return
		}
		if exactSuffix[index] && suffix[index] < remaining {
			return
		}
		chosen = append(chosen, index)
		if next, ok := subInt64(remaining, amounts[index]); ok {
			walk(index+1, next, chosen)
		}
		chosen = chosen[:len(chosen)-1]
		walk(index+1, remaining, chosen)
	}
	walk(0, target, nil)
	if capped {
		// Do not return a prefix of the solution set: without a completed
		// search, one discovered subset may hide a second valid one.
		return nil
	}
	return solutions
}

// dayDistance returns the whole-day distance between two value dates.
func dayDistance(first, second time.Time) int {
	firstDate := dateOnly(first)
	secondDate := dateOnly(second)
	days := int(secondDate.Sub(firstDate).Hours() / 24)
	if days < 0 {
		days = -days
	}
	return days
}
