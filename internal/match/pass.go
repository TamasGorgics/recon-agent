package match

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// ErrInvalidCandidate reports a pass proposal that does not refer to the run
// snapshot or that carries an invalid leg/rule/confidence. It is a programming
// error in a pass, not a data problem.
var ErrInvalidCandidate = errors.New("match: invalid candidate")

// DefaultPasses returns the ordered M1 pass chain: exact, reference tolerance,
// composite, fuzzy, duplicates. Passes 3 to 5 are M1-13.
func DefaultPasses(cfg config.MatchConfig) []Pass {
	cfg = cfg.WithDefaults()
	return []Pass{
		ExactPass{},
		ReferenceTolerancePass{FeeToleranceMinor: cfg.FeeToleranceMinor},
		CompositePass{Config: cfg},
		FuzzyPass{Config: cfg},
		DuplicatePass{},
	}
}

// BreakPass is an optional pass capability: besides match candidates it can
// propose BreakCandidates for records it deliberately refused to match
// (composite ambiguity/caps, duplicates). Break candidates are evaluated after
// every match pass has claimed its records, so a later fuzzy match cannot leave
// a stale break behind.
type BreakPass interface {
	Pass
	BreakCandidates(ctx context.Context, rc *RunContext) ([]BreakCandidate, error)
}

// Evaluate runs the passes in order over rc and returns the accepted candidates
// plus a per-pass accepted count. Break candidates are discarded; Engine uses
// EvaluateWithBreaks. It does not touch the database.
//
// Every proposal is normalized against the run snapshot so a pass cannot
// fabricate an external record, ledger transaction, amount, or currency. When
// two proposals in the same pass claim the same external record or ledger
// transaction, all of them are dropped as ambiguous; this keeps the accepted
// set independent of proposal order. Claims accepted by an earlier pass are
// invisible to later passes, which is how pass 1 takes precedence over pass 2.
func Evaluate(ctx context.Context, rc *RunContext, passes []Pass) ([]Candidate, map[string]int, error) {
	candidates, _, counts, err := EvaluateWithBreaks(ctx, rc, passes)
	return candidates, counts, err
}

// EvaluateWithBreaks is Evaluate plus the deterministic break candidates
// proposed by passes implementing BreakPass. Break candidates are de-duplicated
// by their canonical Signature (D-5) and sorted by it, so a run's break set does
// not depend on proposal or input order.
func EvaluateWithBreaks(ctx context.Context, rc *RunContext, passes []Pass) ([]Candidate, []BreakCandidate, map[string]int, error) {
	if rc == nil {
		return nil, nil, nil, fmt.Errorf("%w: nil RunContext", ErrInvalidCandidate)
	}
	counts := make(map[string]int, len(passes))
	accepted := make([]Candidate, 0)

	for _, pass := range passes {
		if pass == nil {
			return nil, nil, nil, fmt.Errorf("%w: nil Pass", ErrInvalidCandidate)
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		raw, err := pass.Candidates(ctx, rc)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("match: pass %s: %w", pass.Name(), err)
		}

		proposals := make([]Candidate, 0, len(raw))
		externalClaims := make(map[string]int, len(raw))
		ledgerClaims := make(map[string]int, len(raw))
		for i, candidate := range raw {
			normalized, err := rc.normalize(candidate)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("match: pass %s candidate %d: %w", pass.Name(), i, err)
			}
			if rc.isUsed(normalized) {
				continue
			}
			proposals = append(proposals, normalized)
			externalClaims[normalized.External.ID]++
			for _, leg := range normalized.Legs {
				ledgerClaims[leg.LedgerTxnID]++
			}
		}

		passAccepted := 0
		for _, candidate := range proposals {
			ambiguous := externalClaims[candidate.External.ID] != 1
			for _, leg := range candidate.Legs {
				if ledgerClaims[leg.LedgerTxnID] != 1 {
					ambiguous = true
				}
			}
			if ambiguous {
				continue
			}
			rc.claim(candidate)
			accepted = append(accepted, candidate)
			passAccepted++
		}
		counts[pass.Name()] = passAccepted
	}

	// Break candidates are proposed only after every match pass has run and
	// every claim is final.
	breaks := make([]BreakCandidate, 0)
	seenBreaks := make(map[string]struct{})
	for _, pass := range passes {
		breakPass, ok := pass.(BreakPass)
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		raw, err := breakPass.BreakCandidates(ctx, rc)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("match: pass %s breaks: %w", pass.Name(), err)
		}
		for i, candidate := range raw {
			if err := rc.normalizeBreak(candidate); err != nil {
				return nil, nil, nil, fmt.Errorf("match: pass %s break %d: %w", pass.Name(), i, err)
			}
			signature := candidate.Signature()
			if _, duplicate := seenBreaks[signature]; duplicate {
				continue
			}
			seenBreaks[signature] = struct{}{}
			breaks = append(breaks, candidate)
		}
	}
	sort.SliceStable(breaks, func(i, j int) bool {
		return breaks[i].Signature() < breaks[j].Signature()
	})

	sort.SliceStable(accepted, func(i, j int) bool {
		return accepted[i].External.ID < accepted[j].External.ID
	})
	return accepted, breaks, counts, nil
}

// normalizeBreak pins a break candidate to this run's scope and validates its
// side/currency, so a pass cannot attribute a break to another scope.
func (rc *RunContext) normalizeBreak(candidate BreakCandidate) error {
	if candidate.ScopeID != "" && candidate.ScopeID != rc.scope.ID {
		return fmt.Errorf("%w: break scope %q does not match run scope %q",
			ErrInvalidCandidate, candidate.ScopeID, rc.scope.ID)
	}
	switch candidate.Side {
	case SideLedgerOnly, SideExternalOnly, SideAmountMismatch, SideDuplicate:
	default:
		return fmt.Errorf("%w: break side %q is not a D-5 side", ErrInvalidCandidate, candidate.Side)
	}
	if candidate.Currency != rc.scope.Currency {
		return fmt.Errorf("%w: break currency %q does not match scope %q",
			ErrInvalidCandidate, candidate.Currency, rc.scope.Currency)
	}
	return nil
}

func (rc *RunContext) normalize(candidate Candidate) (Candidate, error) {
	external, ok := rc.externalByID[candidate.External.ID]
	if !ok {
		return Candidate{}, fmt.Errorf("%w: unknown external record %q", ErrInvalidCandidate, candidate.External.ID)
	}
	if external.Currency != rc.scope.Currency {
		return Candidate{}, fmt.Errorf("%w: external %q currency %q does not match scope %q",
			ErrInvalidCandidate, external.ID, external.Currency, rc.scope.Currency)
	}
	if candidate.Rule == "" {
		return Candidate{}, fmt.Errorf("%w: external %q has an empty rule", ErrInvalidCandidate, external.ID)
	}
	if candidate.ConfidenceBPS < 0 || candidate.ConfidenceBPS > ConfidenceMax {
		return Candidate{}, fmt.Errorf("%w: external %q confidence %d is outside 0..10000",
			ErrInvalidCandidate, external.ID, candidate.ConfidenceBPS)
	}
	if len(candidate.Legs) == 0 {
		return Candidate{}, fmt.Errorf("%w: external %q has no legs", ErrInvalidCandidate, external.ID)
	}

	seen := make(map[string]struct{}, len(candidate.Legs))
	legs := make([]Leg, 0, len(candidate.Legs))
	for _, leg := range candidate.Legs {
		txn, ok := rc.ledgerByID[leg.LedgerTxnID]
		if !ok {
			return Candidate{}, fmt.Errorf("%w: unknown ledger txn %q", ErrInvalidCandidate, leg.LedgerTxnID)
		}
		if txn.Currency != rc.scope.Currency {
			return Candidate{}, fmt.Errorf("%w: ledger txn %q currency %q does not match scope %q",
				ErrInvalidCandidate, txn.ID, txn.Currency, rc.scope.Currency)
		}
		if leg.AmountMinor != txn.AmountMinor {
			return Candidate{}, fmt.Errorf("%w: ledger txn %q leg amount %d does not match %d",
				ErrInvalidCandidate, txn.ID, leg.AmountMinor, txn.AmountMinor)
		}
		if _, duplicate := seen[txn.ID]; duplicate {
			return Candidate{}, fmt.Errorf("%w: ledger txn %q appears twice", ErrInvalidCandidate, txn.ID)
		}
		seen[txn.ID] = struct{}{}
		legs = append(legs, Leg{LedgerTxnID: txn.ID, AmountMinor: txn.AmountMinor, Currency: txn.Currency})
	}
	sort.SliceStable(legs, func(i, j int) bool { return legs[i].LedgerTxnID < legs[j].LedgerTxnID })

	return Candidate{
		External:      external,
		Legs:          legs,
		Rule:          candidate.Rule,
		ConfidenceBPS: candidate.ConfidenceBPS,
	}, nil
}

func (rc *RunContext) isUsed(candidate Candidate) bool {
	if _, used := rc.usedExternal[candidate.External.ID]; used {
		return true
	}
	for _, leg := range candidate.Legs {
		if _, used := rc.usedLedger[leg.LedgerTxnID]; used {
			return true
		}
	}
	return false
}
