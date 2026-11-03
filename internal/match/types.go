package match

import (
	"context"
	"sort"
	"time"
)

// Rule names stored in matches.rule. M1-12 owns the first two; M1-13 adds the
// composite and fuzzy values. Duplicates never produce a match rule: they only
// produce BreakCandidate rows with SideDuplicate.
const (
	// RuleExact is pass 1: same reference, amount, and currency.
	RuleExact = "exact"
	// RuleReferenceTolerance is pass 2: same reference and currency, with the
	// absolute amount delta within the configured fee tolerance.
	RuleReferenceTolerance = "reference_tolerance"
	// RuleCompositeBatch is pass 3 with declared settlement batch membership.
	RuleCompositeBatch = "composite_batch"
	// RuleCompositeSubset is pass 3's bounded subset-sum fallback.
	RuleCompositeSubset = "composite"
	// RuleFuzzy is pass 4: amount + value-date window + counterparty hint.
	RuleFuzzy = "fuzzy"
	// RuleDuplicate names pass 5 in run stats. It never links.
	RuleDuplicate = "duplicate"
)

// ConfidenceBPS returns the confidence as basis points (10000 = 1.0). The
// database stores confidence as NUMERIC(5,4); match uses integers so no money
// path ever touches floating point (FIN-01).
type ConfidenceBPS int

// Confidence values used by the deterministic passes.
const (
	ConfidenceExact     ConfidenceBPS = 10_000
	ConfidenceTolerance ConfidenceBPS = 9_900
	ConfidenceComposite ConfidenceBPS = 9_800
	ConfidenceMax       ConfidenceBPS = 10_000
)

// Scope is one reconciliation scope: a ledger account paired with one external
// source and currency (spec section 5). Engine.Run reconciles exactly one scope,
// matching the singular match_runs.scope_id.
type Scope struct {
	ID              string
	LedgerAccountID string
	Source          string
	Currency        string
}

// Window is an inclusive value-date window. Both bounds are required business
// dates; the matcher never reads the wall clock to derive them (FIN-42).
type Window struct {
	From time.Time
	To   time.Time
}

// Valid reports whether the window has both bounds and To is not before From.
func (w Window) Valid() bool {
	return !w.From.IsZero() && !w.To.IsZero() && !dateOnly(w.To).Before(dateOnly(w.From))
}

// Contains reports whether valueDate falls inside the window. Comparisons use
// the UTC business date, never a timestamp's time-of-day.
func (w Window) Contains(valueDate time.Time) bool {
	date := dateOnly(valueDate)
	return !date.Before(dateOnly(w.From)) && !date.After(dateOnly(w.To))
}

// ExternalRecord is one unmatched source record loaded for a run. Reference is
// the source's matching reference: raw->>'reference' for psp_a,
// raw->>'bank_reference' for bank_x, and source_record_id as the fallback.
//
// Batch is the source's declared settlement batch (external_records.batch_id).
// M1-13 pass 3 prefers declared batch membership before subset-sum.
// OccurredAt is the source event time; duplicate detection uses it for
// same-amount/same-time charges.
type ExternalRecord struct {
	ID             string
	Source         string
	SourceRecordID string
	Kind           string
	AmountMinor    int64
	FeeMinor       int64
	Currency       string
	ValueDate      time.Time
	OccurredAt     time.Time
	Reference      string
	Batch          string
}

// LedgerTxn is one unmatched ledger transaction loaded for a run. AmountMinor
// is the signed net amount on the scope's ledger account (debit positive,
// credit negative), which is the convention external records use.
type LedgerTxn struct {
	ID          string
	Kind        string
	AmountMinor int64
	Currency    string
	ValueDate   time.Time
	Reference   string
}

// Leg is one ledger transaction claimed by a candidate match. A 1:1 match has
// one leg; a composite match has N.
type Leg struct {
	LedgerTxnID string
	AmountMinor int64
	Currency    string
}

// Candidate is a match proposal produced by a Pass before it is inserted.
// Legs must reference unmatched ledger transactions; Evaluate enforces that and
// the unique indexes are the final backstop.
type Candidate struct {
	External      ExternalRecord
	Legs          []Leg
	Rule          string
	ConfidenceBPS ConfidenceBPS
}

// Pass is one ordered matching pass. Passes are pure functions over the
// RunContext snapshot: they must not touch the database or the wall clock. The
// engine runs them in order, claims the accepted candidates, and hands the next
// pass only the remaining unmatched records.
type Pass interface {
	// Name is the stable pass name used in per-run stats, for example "exact".
	Name() string
	// Candidates returns proposals for this pass. Evaluate validates and
	// de-duplicates them.
	Candidates(ctx context.Context, rc *RunContext) ([]Candidate, error)
}

// RunContext is the immutable run metadata plus the candidate snapshot and the
// claims accepted so far. Pass implementations read UnmatchedExternal and
// UnmatchedLedger; only Evaluate mutates claims.
type RunContext struct {
	scope  Scope
	window Window
	now    time.Time

	external     []ExternalRecord
	ledger       []LedgerTxn
	externalByID map[string]ExternalRecord
	ledgerByID   map[string]LedgerTxn

	usedExternal map[string]struct{}
	usedLedger   map[string]struct{}
}

// NewRunContext copies and sorts the candidate slices by ID, so shuffling the
// input order cannot change pass results. The returned context has no claims.
func NewRunContext(scope Scope, window Window, now time.Time, external []ExternalRecord, ledger []LedgerTxn) *RunContext {
	externalCopy := append([]ExternalRecord(nil), external...)
	ledgerCopy := append([]LedgerTxn(nil), ledger...)
	sort.SliceStable(externalCopy, func(i, j int) bool { return externalCopy[i].ID < externalCopy[j].ID })
	sort.SliceStable(ledgerCopy, func(i, j int) bool { return ledgerCopy[i].ID < ledgerCopy[j].ID })

	externalByID := make(map[string]ExternalRecord, len(externalCopy))
	for _, record := range externalCopy {
		externalByID[record.ID] = record
	}
	ledgerByID := make(map[string]LedgerTxn, len(ledgerCopy))
	for _, txn := range ledgerCopy {
		ledgerByID[txn.ID] = txn
	}

	return &RunContext{
		scope:        scope,
		window:       window,
		now:          now,
		external:     externalCopy,
		ledger:       ledgerCopy,
		externalByID: externalByID,
		ledgerByID:   ledgerByID,
		usedExternal: make(map[string]struct{}),
		usedLedger:   make(map[string]struct{}),
	}
}

// Scope returns the run's reconciliation scope.
func (rc *RunContext) Scope() Scope { return rc.scope }

// Window returns the run's inclusive value-date window.
func (rc *RunContext) Window() Window { return rc.window }

// Now returns the injected run timestamp.
func (rc *RunContext) Now() time.Time { return rc.now }

// External returns every loaded external record in stable ID order.
func (rc *RunContext) External() []ExternalRecord {
	return append([]ExternalRecord(nil), rc.external...)
}

// Ledger returns every loaded ledger transaction in stable ID order.
func (rc *RunContext) Ledger() []LedgerTxn {
	return append([]LedgerTxn(nil), rc.ledger...)
}

// UnmatchedExternal returns the external records not claimed by an earlier
// pass, in stable ID order.
func (rc *RunContext) UnmatchedExternal() []ExternalRecord {
	out := make([]ExternalRecord, 0, len(rc.external))
	for _, record := range rc.external {
		if _, used := rc.usedExternal[record.ID]; used {
			continue
		}
		out = append(out, record)
	}
	return out
}

// UnmatchedLedger returns the ledger transactions not claimed by an earlier
// pass, in stable ID order.
func (rc *RunContext) UnmatchedLedger() []LedgerTxn {
	out := make([]LedgerTxn, 0, len(rc.ledger))
	for _, txn := range rc.ledger {
		if _, used := rc.usedLedger[txn.ID]; used {
			continue
		}
		out = append(out, txn)
	}
	return out
}

func (rc *RunContext) claim(candidate Candidate) {
	rc.usedExternal[candidate.External.ID] = struct{}{}
	for _, leg := range candidate.Legs {
		rc.usedLedger[leg.LedgerTxnID] = struct{}{}
	}
}

func (rc *RunContext) usedLedgerCount() int { return len(rc.usedLedger) }

func dateOnly(value time.Time) time.Time {
	utc := value.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}
