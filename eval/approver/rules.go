package approver

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Stable rule ids. The value is part of the e2e result schema, so it must not
// change without a schema/version change.
const (
	// RuleExactMatch is every exact-match predicate passing.
	RuleExactMatch = "exact_match"
	// RuleT0AutoApproved is a T0/auto-approved request that bypasses decisions.
	RuleT0AutoApproved = "t0_auto_approved"
	// RuleNoProposal is an abstain because the pipeline has no proposal.
	RuleNoProposal = "no_proposal"
	// RuleTypeMismatch is an unknown proposal type, unknown observable break
	// kind, or an incompatible type/break-kind pair.
	RuleTypeMismatch = "type_mismatch"
	// RuleAmountMismatch is an amount that does not reconcile.
	RuleAmountMismatch = "amount_mismatch"
	// RuleCurrencyMismatch is a proposal or cited-record currency mismatch.
	RuleCurrencyMismatch = "currency_mismatch"
	// RuleLedgerRefsMismatch is a missing, unknown, out-of-scope, duplicated,
	// or non-reconciling cited ledger reference.
	RuleLedgerRefsMismatch = "ledger_refs_mismatch"
	// RuleExternalRefsMismatch is a missing, unknown, out-of-scope,
	// duplicated, malformed, or non-reconciling cited external reference.
	RuleExternalRefsMismatch = "external_refs_mismatch"
	// RuleCounterpartyMismatch is an empty or non-matching counterparty.
	RuleCounterpartyMismatch = "counterparty_mismatch"
	// RuleResolutionMismatch is an empty, non-normalized, unknown, or
	// type-inconsistent resolution.
	RuleResolutionMismatch = "resolution_mismatch"
)

const reasonExactMatch = "observable case facts and cited records match exactly"

var (
	canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	currencyPattern      = regexp.MustCompile(`^[A-Z]{3}$`)
)

// proposalTypeSpec is the observable meaning of one proposal type. Only
// resolution types reach this package; request_info is routed to waiting_info
// and never to the approval decision path.
type proposalTypeSpec struct {
	resolution      string
	ledgerAffecting bool
}

var proposalTypes = map[string]proposalTypeSpec{
	"link":       {resolution: "link"},
	"adjustment": {resolution: "adjustment", ledgerAffecting: true},
	"write_off":  {resolution: "write_off", ledgerAffecting: true},
	"duplicate":  {resolution: "duplicate"},
}

// breakKindTypes is the authoritative compatibility table from spec 4.1.3.
// The observable break kind decides which resolution types can be exact. An
// unknown break kind or an unlisted proposal type rejects rather than guesses.
var breakKindTypes = map[string]map[string]struct{}{
	"fee_variance":     allowedTypes("adjustment"),
	"fx_rounding":      allowedTypes("adjustment"),
	"rounding":         allowedTypes("adjustment", "write_off"),
	"reference_typo":   allowedTypes("link"),
	"duplicate_charge": allowedTypes("duplicate"),
	"other":            allowedTypes("adjustment"),
}

func allowedTypes(types ...string) map[string]struct{} {
	allowed := make(map[string]struct{}, len(types))
	for _, proposalType := range types {
		allowed[proposalType] = struct{}{}
	}
	return allowed
}

// ruleMismatch is a predicate failure: a deterministic rejection, not a
// pipeline error.
type ruleMismatch struct {
	rule   string
	reason string
}

func (m *ruleMismatch) Error() string { return m.reason }

func mismatch(rule, reason string) error {
	return &ruleMismatch{rule: rule, reason: reason}
}

// rule is one entry in the data-driven exact-match table. The slice order is
// the frozen evaluation order; each Check returns nil to pass, a *ruleMismatch
// to reject, or any other error to fail the pipeline closed.
type rule struct {
	ID    string
	Check func(context.Context, *evaluation) error
}

// ruleTable is the authoritative predicate table. The first failing check
// rejects with the rule id it returns.
var ruleTable = []rule{
	{ID: RuleTypeMismatch, Check: checkType},
	{ID: RuleAmountMismatch, Check: checkAmount},
	{ID: RuleLedgerRefsMismatch, Check: checkLedgerRefs},
	{ID: RuleExternalRefsMismatch, Check: checkExternalRefs},
	{ID: RuleCounterpartyMismatch, Check: checkCounterparty},
	{ID: RuleResolutionMismatch, Check: checkResolution},
}

// requiredRuleIDs are the predicates every valid table must expose. The
// amount/currency rule also emits currency_mismatch; the reference rules
// enforce the cited records' amounts and currencies.
var requiredRuleIDs = []string{
	RuleTypeMismatch,
	RuleAmountMismatch,
	RuleLedgerRefsMismatch,
	RuleExternalRefsMismatch,
	RuleCounterpartyMismatch,
	RuleResolutionMismatch,
}

func validateRuleTable(table []rule) error {
	if len(table) == 0 {
		return fmt.Errorf("%w: empty", ErrRuleTable)
	}
	seen := make(map[string]struct{}, len(table))
	for _, entry := range table {
		if strings.TrimSpace(entry.ID) == "" {
			return fmt.Errorf("%w: rule with an empty id", ErrRuleTable)
		}
		if entry.Check == nil {
			return fmt.Errorf("%w: rule %q has no check", ErrRuleTable, entry.ID)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return fmt.Errorf("%w: duplicate rule %q", ErrRuleTable, entry.ID)
		}
		seen[entry.ID] = struct{}{}
	}
	for _, id := range requiredRuleIDs {
		if _, ok := seen[id]; !ok {
			return fmt.Errorf("%w: missing required rule %q", ErrRuleTable, id)
		}
	}
	return nil
}

// evaluation carries the inputs shared by every predicate and lazily resolves
// the records cited by the proposal. A resolution error is a pipeline error,
// never a rejection.
type evaluation struct {
	caseFacts CaseFacts
	proposal  ProposalFacts
	records   RecordSource

	ledger       map[string]LedgerRecord
	ledgerLoaded bool
	ledgerErr    error

	external       map[string]ExternalRecord
	externalLoaded bool
	externalErr    error
}

func (e *evaluation) ledgerRecords(ctx context.Context) (map[string]LedgerRecord, error) {
	if e.ledgerLoaded {
		return e.ledger, e.ledgerErr
	}
	e.ledgerLoaded = true
	e.ledger = make(map[string]LedgerRecord)
	if len(e.proposal.LedgerRefs) == 0 {
		return e.ledger, nil
	}
	if isNilValue(e.records) {
		e.ledgerErr = errRecordSource
		return nil, e.ledgerErr
	}
	records, err := e.records.Ledger(ctx, e.proposal.LedgerRefs)
	if err != nil {
		e.ledgerErr = fmt.Errorf("eval/approver: load cited ledger records: %w", err)
		return nil, e.ledgerErr
	}
	for _, record := range records {
		if record.ID != "" {
			e.ledger[record.ID] = record
		}
	}
	return e.ledger, nil
}

func (e *evaluation) externalRecords(ctx context.Context) (map[string]ExternalRecord, error) {
	if e.externalLoaded {
		return e.external, e.externalErr
	}
	e.externalLoaded = true
	e.external = make(map[string]ExternalRecord)
	if len(e.proposal.ExternalRefs) == 0 {
		return e.external, nil
	}
	if isNilValue(e.records) {
		e.externalErr = errRecordSource
		return nil, e.externalErr
	}
	records, err := e.records.External(ctx, e.proposal.ExternalRefs)
	if err != nil {
		e.externalErr = fmt.Errorf("eval/approver: load cited external records: %w", err)
		return nil, e.externalErr
	}
	for _, record := range records {
		if record.ID != "" {
			e.external[record.ID] = record
		}
	}
	return e.external, nil
}

// checkType applies predicate 1: a known proposal type compatible with the
// observable break kind.
func checkType(_ context.Context, e *evaluation) error {
	if _, known := proposalTypes[e.proposal.Type]; !known {
		return mismatch(RuleTypeMismatch, "proposal type is not a known resolution type")
	}
	allowed, known := breakKindTypes[e.caseFacts.BreakKind]
	if !known {
		return mismatch(RuleTypeMismatch, "observable break kind is not in the rule table")
	}
	if _, compatible := allowed[e.proposal.Type]; !compatible {
		return mismatch(RuleTypeMismatch, "proposal type is not compatible with the observable break kind")
	}
	return nil
}

// checkAmount applies predicate 2's top-level amount/currency checks. The
// cited-record amounts are checked by the reference rules so that a missing
// reference reports a reference rule, not an amount rule.
func checkAmount(_ context.Context, e *evaluation) error {
	if !currencyPattern.MatchString(e.proposal.Currency) ||
		e.proposal.Currency != e.caseFacts.Currency {
		return mismatch(RuleCurrencyMismatch, "proposal currency does not match the observable case currency")
	}
	if e.proposal.AmountMinor <= 0 {
		return mismatch(RuleAmountMismatch, "proposal amount must be positive")
	}
	spec, known := proposalTypes[e.proposal.Type]
	if !known {
		return mismatch(RuleTypeMismatch, "proposal type is not a known resolution type")
	}
	if spec.ledgerAffecting && e.proposal.AmountMinor != e.caseFacts.DeltaMinor {
		return mismatch(RuleAmountMismatch, "proposal amount does not match the observable discrepancy")
	}
	return nil
}

// checkLedgerRefs applies predicate 3: every cited ledger transaction exists,
// belongs to the case scope, and reconciles exactly to the proposed effect.
func checkLedgerRefs(ctx context.Context, e *evaluation) error {
	if len(e.proposal.LedgerRefs) == 0 {
		if e.proposal.Type == "link" {
			return mismatch(RuleLedgerRefsMismatch, "link proposal has no cited ledger reference")
		}
		return nil
	}
	records, err := e.ledgerRecords(ctx)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(e.proposal.LedgerRefs))
	var total int64
	for _, id := range e.proposal.LedgerRefs {
		if !canonicalUUIDPattern.MatchString(id) {
			return mismatch(RuleLedgerRefsMismatch, "cited ledger reference is not a canonical transaction id")
		}
		if _, duplicate := seen[id]; duplicate {
			return mismatch(RuleLedgerRefsMismatch, "cited ledger references contain a duplicate")
		}
		seen[id] = struct{}{}

		record, ok := records[id]
		if !ok {
			return mismatch(RuleLedgerRefsMismatch, "cited ledger reference does not exist")
		}
		if record.ScopeID != e.caseFacts.ScopeID {
			return mismatch(RuleLedgerRefsMismatch, "cited ledger reference belongs to a different scope")
		}
		if record.AmountMinor <= 0 {
			return mismatch(RuleLedgerRefsMismatch, "cited ledger reference has a non-positive amount")
		}
		if record.Currency != e.proposal.Currency {
			return mismatch(RuleCurrencyMismatch, "cited ledger reference currency does not match the proposal")
		}
		if total > math.MaxInt64-record.AmountMinor {
			return mismatch(RuleLedgerRefsMismatch, "cited ledger amounts overflow")
		}
		total += record.AmountMinor
	}
	if total != e.proposal.AmountMinor {
		return mismatch(RuleLedgerRefsMismatch, "cited ledger records do not reconcile to the proposed effect")
	}
	return nil
}

// checkExternalRefs applies predicate 4: every cited external record exists,
// belongs to the case scope, and reconciles exactly to the proposed effect.
func checkExternalRefs(ctx context.Context, e *evaluation) error {
	if len(e.proposal.ExternalRefs) == 0 {
		if e.proposal.Type == "link" || e.proposal.Type == "duplicate" {
			return mismatch(RuleExternalRefsMismatch, "proposal type requires a cited external reference")
		}
		return nil
	}
	records, err := e.externalRecords(ctx)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(e.proposal.ExternalRefs))
	var total int64
	for _, ref := range e.proposal.ExternalRefs {
		if !validExternalRef(ref) {
			return mismatch(RuleExternalRefsMismatch, "cited external reference is malformed")
		}
		if _, duplicate := seen[ref]; duplicate {
			return mismatch(RuleExternalRefsMismatch, "cited external references contain a duplicate")
		}
		seen[ref] = struct{}{}

		record, ok := records[ref]
		if !ok {
			return mismatch(RuleExternalRefsMismatch, "cited external reference does not exist")
		}
		if record.ScopeID != e.caseFacts.ScopeID {
			return mismatch(RuleExternalRefsMismatch, "cited external reference belongs to a different scope")
		}
		if record.AmountMinor <= 0 {
			return mismatch(RuleExternalRefsMismatch, "cited external reference has a non-positive amount")
		}
		if record.Currency != e.proposal.Currency {
			return mismatch(RuleCurrencyMismatch, "cited external reference currency does not match the proposal")
		}
		if e.proposal.Type == "duplicate" {
			// A duplicate cites the duplicate and optionally its original;
			// both records carry the same one-record amount.
			if record.AmountMinor != e.proposal.AmountMinor {
				return mismatch(RuleExternalRefsMismatch, "cited external record amount does not match the duplicate amount")
			}
			continue
		}
		if total > math.MaxInt64-record.AmountMinor {
			return mismatch(RuleExternalRefsMismatch, "cited external amounts overflow")
		}
		total += record.AmountMinor
	}
	if e.proposal.Type != "duplicate" && total != e.proposal.AmountMinor {
		return mismatch(RuleExternalRefsMismatch, "cited external records do not reconcile to the proposed effect")
	}
	return nil
}

// checkCounterparty applies predicate 5: a non-empty proposal counterparty
// equals every cited external record's counterparty, or the observable case
// counterparty when the proposal cites no external record.
func checkCounterparty(ctx context.Context, e *evaluation) error {
	if e.proposal.Counterparty == "" || e.proposal.Counterparty != strings.TrimSpace(e.proposal.Counterparty) {
		return mismatch(RuleCounterpartyMismatch, "proposal counterparty is empty or not normalized")
	}
	if len(e.proposal.ExternalRefs) == 0 {
		if e.caseFacts.Counterparty == "" ||
			e.caseFacts.Counterparty != strings.TrimSpace(e.caseFacts.Counterparty) {
			return mismatch(RuleCounterpartyMismatch, "observable case counterparty is missing or not normalized")
		}
		if e.proposal.Counterparty != e.caseFacts.Counterparty {
			return mismatch(RuleCounterpartyMismatch, "proposal counterparty does not match the observable case counterparty")
		}
		return nil
	}

	records, err := e.externalRecords(ctx)
	if err != nil {
		return err
	}
	for _, ref := range e.proposal.ExternalRefs {
		record, ok := records[ref]
		if !ok {
			return mismatch(RuleExternalRefsMismatch, "cited external reference does not exist")
		}
		if record.Counterparty == "" ||
			record.Counterparty != strings.TrimSpace(record.Counterparty) ||
			record.Counterparty != e.proposal.Counterparty {
			return mismatch(RuleCounterpartyMismatch, "proposal counterparty does not match a cited external record")
		}
	}
	return nil
}

// checkResolution applies predicate 6: the resolution is non-empty,
// normalized, and consistent with the proposal type.
func checkResolution(_ context.Context, e *evaluation) error {
	spec, known := proposalTypes[e.proposal.Type]
	if !known {
		return mismatch(RuleTypeMismatch, "proposal type is not a known resolution type")
	}
	if e.proposal.Resolution == "" ||
		e.proposal.Resolution != strings.TrimSpace(e.proposal.Resolution) ||
		e.proposal.Resolution != strings.ToLower(e.proposal.Resolution) ||
		e.proposal.Resolution != spec.resolution {
		return mismatch(RuleResolutionMismatch, "proposal resolution is not normalized or is inconsistent with its type")
	}
	return nil
}

// validExternalRef reports whether ref is a canonical ext:<source>:<id> ref.
func validExternalRef(ref string) bool {
	if ref == "" || ref != strings.TrimSpace(ref) || strings.ContainsAny(ref, " \t\r\n") {
		return false
	}
	rest, ok := strings.CutPrefix(ref, "ext:")
	if !ok {
		return false
	}
	source, recordID, ok := strings.Cut(rest, ":")
	return ok && source != "" && recordID != ""
}
