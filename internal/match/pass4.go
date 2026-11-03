package match

import (
	"context"
	"sort"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// Fuzzy scoring weights (basis points of the 0..10000 confidence). The total
// maximum is 5000 + 3000 + 2000 = 10000.
const (
	fuzzyAmountWeightBPS = 5_000
	fuzzyDateWeightBPS   = 3_000
	fuzzyHintWeightBPS   = 2_000
)

// FuzzyPass is matching pass 4: amount + value-date window + counterparty hint,
// scored deterministically in basis points (no floating point). A pair is a
// candidate only when the absolute amount delta is within the configured fee
// tolerance and the value dates are within FuzzyDateWindowDays.
//
// The best-scoring ledger transaction matches only when there is a single clear
// winner: a score below FuzzyThresholdBPS means no match, and an exact tie
// between two candidates also means no match. The confidence is stored in
// matches.confidence by the engine.
type FuzzyPass struct {
	Config config.MatchConfig
}

// Name returns the stable pass name used in run stats.
func (FuzzyPass) Name() string { return "fuzzy" }

// Candidates implements Pass.
func (p FuzzyPass) Candidates(_ context.Context, rc *RunContext) ([]Candidate, error) {
	cfg := p.Config.WithDefaults()
	external := rc.UnmatchedExternal()
	ledger := rc.UnmatchedLedger()

	// Bucket unmatched ledger transactions by UTC value date. Day buckets make
	// the scan proportional to the in-window ledger population, not the whole
	// scope.
	byDay := make(map[string][]LedgerTxn, len(ledger))
	for _, txn := range ledger {
		if txn.Currency != rc.Scope().Currency {
			continue
		}
		key := dateOnly(txn.ValueDate).Format("2006-01-02")
		byDay[key] = append(byDay[key], txn)
	}

	candidates := make([]Candidate, 0)
	for _, record := range external {
		best, ok := fuzzyBest(record, cfg, byDay)
		if !ok {
			continue
		}
		candidates = append(candidates, best)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].External.ID < candidates[j].External.ID
	})
	return candidates, nil
}

// fuzzyBest returns the single clear best ledger candidate for record, if any.
func fuzzyBest(record ExternalRecord, cfg config.MatchConfig, byDay map[string][]LedgerTxn) (Candidate, bool) {
	tolerance := cfg.FeeTolerance(record.Currency)
	window := cfg.FuzzyDateWindowDays
	recordDate := dateOnly(record.ValueDate)

	bestScore := -1
	var bestTxn LedgerTxn
	tied := false

	for offset := -window; offset <= window; offset++ {
		key := recordDate.AddDate(0, 0, offset).Format("2006-01-02")
		for _, txn := range byDay[key] {
			if txn.Currency != record.Currency {
				continue
			}
			delta, ok := absDelta(record.AmountMinor, txn.AmountMinor)
			if !ok {
				// The true delta is outside int64 and therefore outside any
				// configured tolerance: not a candidate.
				continue
			}
			if delta > tolerance {
				continue
			}
			days := dayDistance(record.ValueDate, txn.ValueDate)
			if days > window {
				continue
			}
			score, ok := fuzzyScoreBPS(delta, tolerance, days, window, record.Reference, txn.Reference)
			if !ok {
				continue
			}
			switch {
			case score > bestScore:
				bestScore = score
				bestTxn = txn
				tied = false
			case score == bestScore:
				tied = true
			}
		}
	}

	if bestScore < cfg.FuzzyThresholdBPS || tied {
		return Candidate{}, false
	}
	return Candidate{
		External: record,
		Legs: []Leg{{
			LedgerTxnID: bestTxn.ID,
			AmountMinor: bestTxn.AmountMinor,
			Currency:    bestTxn.Currency,
		}},
		Rule:          RuleFuzzy,
		ConfidenceBPS: ConfidenceBPS(bestScore),
	}, true
}

// fuzzyScoreBPS scores one pair. Integer arithmetic keeps the score exact and
// order-independent (FIN-01: no float in a money path). ok is false when a
// weighted product would overflow; the caller then treats the pair as
// non-matching instead of using a wrapped score.
func fuzzyScoreBPS(delta, tolerance int64, days, window int, recordReference, ledgerReference string) (int, bool) {
	amountScore := 0
	if delta == 0 {
		amountScore = fuzzyAmountWeightBPS
	} else if tolerance > 0 {
		remaining, ok := subInt64(tolerance, delta)
		if !ok {
			return 0, false
		}
		scaled, ok := mulDivInt64(int64(fuzzyAmountWeightBPS), remaining, tolerance)
		if !ok {
			return 0, false
		}
		amountScore = int(scaled)
	}

	dateScore := 0
	if days == 0 {
		dateScore = fuzzyDateWeightBPS
	} else if window > 0 {
		remaining, ok := subInt64(int64(window), int64(days))
		if !ok {
			return 0, false
		}
		scaled, ok := mulDivInt64(int64(fuzzyDateWeightBPS), remaining, int64(window))
		if !ok {
			return 0, false
		}
		dateScore = int(scaled)
	}

	hintScore := 0
	if recordReference != "" && recordReference == ledgerReference {
		hintScore = fuzzyHintWeightBPS
	}
	return amountScore + dateScore + hintScore, true
}
