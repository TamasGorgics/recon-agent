package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// canonicalUUIDPattern is the lowercase 8-4-4-4-12 form used by the rest of the
// approval path. Rejecting other spellings here keeps a malformed id from
// reaching Postgres as a 22P02 error.
var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// DB is the narrow database surface the service needs. Both *pgxpool.Pool and
// pgx.Tx satisfy it; the caller owns the transaction and may roll back the
// verification with the rest of its work.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Service loads verified-source rows through the agent read views, dispatches
// the pure evaluator for the claimed pattern, and persists only
// proposals.verification and proposals.pattern_verified. It is safe for
// concurrent use: all state is immutable after NewService.
type Service struct {
	db   DB
	opts Options
}

// NewService validates opts (applying defaults) and returns a service bound to
// db. The caller must pass the same transaction handle used for the rest of its
// proposal work; Verify never begins or commits a transaction itself.
func NewService(db DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: database is required", ErrInvalidInput)
	}
	resolved, err := resolveOptions(opts)
	if err != nil {
		return nil, err
	}
	if resolved.RoundingCapMinor != nil {
		caps := make(map[string]int64, len(resolved.RoundingCapMinor))
		for currency, cap := range resolved.RoundingCapMinor {
			caps[currency] = cap
		}
		resolved.RoundingCapMinor = caps
	}
	return &Service{db: db, opts: resolved}, nil
}

// proposalRow is the minimal proposal projection the verifier loads. Payload is
// untrusted proposal data, not verifier input until each field is decoded
// strictly.
type proposalRow struct {
	ID             string
	CaseID         string
	Type           string
	Payload        json.RawMessage
	PatternClaimed *string
	Evidence       []string
	Status         string
}

// Verify loads the proposal and its case's uniquely cited open break, dispatches
// the claimed pattern, persists the result (verification plus pattern_verified
// only on a pass), and returns it.
//
// A break that cannot be resolved uniquely (zero or multiple matching open
// breaks) returns a failed Result with reason break_unresolved and writes
// nothing: without a unique break there is no authoritative source set to
// verify. Every other evaluator failure is persisted with pattern_verified NULL.
func (s *Service) Verify(ctx context.Context, proposalID string) (Result, error) {
	if s == nil || s.db == nil {
		return Result{}, fmt.Errorf("%w: service is not initialized", ErrInvalidInput)
	}
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return Result{}, fmt.Errorf("%w: %q is not a canonical lowercase UUID", ErrInvalidProposalID, proposalID)
	}

	proposal, err := s.loadProposal(ctx, proposalID)
	if err != nil {
		return Result{}, err
	}
	if proposal.Status != "proposed" {
		return Result{}, fmt.Errorf("%w: proposal %s is %s", ErrNotVerifiable, proposalID, proposal.Status)
	}

	claimed := ""
	if proposal.PatternClaimed != nil {
		claimed = *proposal.PatternClaimed
	}
	if result, ok := claimFailure(claimed); !ok {
		return result, s.persist(ctx, proposalID, result)
	}
	if result, ok := proposalTypeFailure(proposal.Type, claimed); !ok {
		return result, s.persist(ctx, proposalID, result)
	}

	// A proposal may only be verified against a unique open break attached to
	// its case whose refs are all cited in its evidence. This runs before any
	// source load; without it a proposal could be "verified" against unrelated
	// evidence rows. Ambiguity fails closed with no write.
	resolved, breakFailure, err := s.resolveBreak(ctx, proposal.CaseID, proposal.Evidence, claimed)
	if err != nil {
		return Result{}, err
	}
	if breakFailure != nil {
		return *breakFailure, nil
	}

	var result Result
	switch claimed {
	case PatternFeeVariance:
		result, err = s.verifyFeeVariance(ctx, proposal, resolved, claimed)
	case PatternRounding:
		result, err = s.verifyRounding(ctx, proposal, resolved, claimed)
	case PatternFXRounding:
		result, err = s.verifyFXRounding(ctx, proposal, resolved, claimed)
	case PatternReferenceTypo:
		result, err = s.verifyReferenceTypo(ctx, proposal, resolved, claimed)
	default:
		// claimFailure has already rejected every value outside the closed
		// pattern set; this is defence in depth.
		result = failure(claimed, claimed, Details{}, ReasonUnknownPattern)
	}
	if err != nil {
		return Result{}, err
	}
	return result, s.persist(ctx, proposalID, result)
}

// loadProposal reads only the verifier-relevant columns. Payload and evidence
// stay opaque bytes; evidence must be an array of canonical refs, otherwise the
// proposal data is unusable.
func (s *Service) loadProposal(ctx context.Context, proposalID string) (proposalRow, error) {
	var (
		proposal    proposalRow
		payload     []byte
		evidenceRaw []byte
	)
	err := s.db.QueryRow(ctx,
		`SELECT id::text, case_id::text, type, payload, pattern_claimed, evidence, status
		   FROM proposals
		  WHERE id = $1::uuid`,
		proposalID).
		Scan(&proposal.ID, &proposal.CaseID, &proposal.Type, &payload,
			&proposal.PatternClaimed, &evidenceRaw, &proposal.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return proposalRow{}, fmt.Errorf("%w: %s", ErrProposalNotFound, proposalID)
	}
	if err != nil {
		return proposalRow{}, fmt.Errorf("verify: load proposal %s: %w", proposalID, err)
	}
	if err := json.Unmarshal(evidenceRaw, &proposal.Evidence); err != nil {
		return proposalRow{}, fmt.Errorf("%w: decode proposal evidence: %v", ErrInvalidInput, err)
	}
	proposal.Payload = payload
	return proposal, nil
}

// claimFailure classifies the untrusted claim before any source work. It returns
// (zero, true) only for the four supported patterns.
func claimFailure(claimed string) (Result, bool) {
	switch claimed {
	case "":
		return failure("", "", Details{}, ReasonNoPattern), false
	case PatternOther:
		// other has no verifier and can never reach T0.
		return failure(PatternOther, claimed, Details{}, ReasonPatternMismatch), false
	case PatternFeeVariance, PatternFXRounding, PatternRounding, PatternReferenceTypo:
		return Result{}, true
	default:
		return failure(claimed, claimed, Details{}, ReasonUnknownPattern), false
	}
}

// proposalTypeFailure enforces the spec's type pairing: adjustment patterns
// need an adjustment proposal and reference_typo needs a link.
func proposalTypeFailure(proposalType, claimed string) (Result, bool) {
	want := "adjustment"
	if claimed == PatternReferenceTypo {
		want = "link"
	}
	if proposalType == want {
		return Result{}, true
	}
	return failure(claimed, claimed, Details{}, ReasonPatternMismatch), false
}

type breakExternalRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

// resolvedBreak is the unique case-attached unresolved break the proposal is
// verified against. ScopeID and Currency come from the break row; the canonical
// external refs are decoded once during resolution.
type resolvedBreak struct {
	ID           string
	ScopeID      string
	Currency     string
	LedgerTxnIDs []string
	ExternalRefs []breakExternalRef
}

// resolveBreak returns the unique active (open or in_case) break whose canonical
// external/ledger refs are all cited in evidence, or a failed Result when the
// case does not have exactly one such break.
func (s *Service) resolveBreak(ctx context.Context, caseID string, evidence []string, claimed string) (*resolvedBreak, *Result, error) {
	cited := make(map[string]struct{}, len(evidence))
	for _, ref := range evidence {
		cited[ref] = struct{}{}
	}

	rows, err := s.db.Query(ctx,
		`SELECT b.id::text, b.scope_id::text, b.currency, b.status,
		        b.ledger_txn_ids, b.external_refs
		   FROM v_agent_case_breaks cb
		   JOIN v_agent_breaks b ON b.id = cb.break_id
		  WHERE cb.case_id = $1
		  ORDER BY b.id`,
		caseID)
	if err != nil {
		return nil, nil, fmt.Errorf("verify: load case breaks: %w", err)
	}
	defer rows.Close()

	matches := 0
	var resolved *resolvedBreak
	for rows.Next() {
		var (
			id, scopeID, currency, status string
			ledgerIDs                     []string
			externalRaw                   []byte
		)
		if err := rows.Scan(&id, &scopeID, &currency, &status, &ledgerIDs, &externalRaw); err != nil {
			return nil, nil, fmt.Errorf("verify: scan case break: %w", err)
		}
		if status != "open" && status != "in_case" {
			continue
		}
		externalRefs, err := decodeBreakExternalRefs(externalRaw)
		if err != nil {
			return nil, nil, err
		}
		refs, err := canonicalBreakRefs(ledgerIDs, externalRefs)
		if err != nil {
			return nil, nil, err
		}
		if len(refs) == 0 {
			continue
		}
		allCited := true
		for _, ref := range refs {
			if _, ok := cited[ref]; !ok {
				allCited = false
				break
			}
		}
		if allCited {
			matches++
			if matches > 1 {
				break
			}
			resolved = &resolvedBreak{
				ID:           id,
				ScopeID:      scopeID,
				Currency:     currency,
				LedgerTxnIDs: ledgerIDs,
				ExternalRefs: externalRefs,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("verify: load case breaks: %w", err)
	}
	if matches == 1 {
		return resolved, nil, nil
	}
	result := failure(claimed, claimed, Details{}, ReasonBreakUnresolved)
	return nil, &result, nil
}

// decodeBreakExternalRefs decodes breaks.external_refs. An empty or JSON null
// value means the break has no external side.
func decodeBreakExternalRefs(externalRaw []byte) ([]breakExternalRef, error) {
	if len(externalRaw) == 0 {
		return nil, nil
	}
	var external []breakExternalRef
	if err := json.Unmarshal(externalRaw, &external); err != nil {
		return nil, fmt.Errorf("verify: decode break external refs: %w", err)
	}
	return external, nil
}

// canonicalBreakRefs converts a break's stored ledger ids and external
// (source, source_record_id) objects into canonical evidence refs.
func canonicalBreakRefs(ledgerIDs []string, external []breakExternalRef) ([]string, error) {
	refs := make([]string, 0, len(ledgerIDs)+len(external))
	for _, ledgerID := range ledgerIDs {
		ref, err := tools.LedgerTxnRef(ledgerID)
		if err != nil {
			return nil, fmt.Errorf("verify: break ledger txn: %w", err)
		}
		refs = append(refs, ref)
	}
	for _, item := range external {
		ref, err := tools.ExternalRef(item.Source, item.SourceRecordID)
		if err != nil {
			return nil, fmt.Errorf("verify: break external ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// adjustmentSources is the loaded source set for fee_variance and rounding.
// failure is non-nil when a source row is missing or ambiguous.
type adjustmentSources struct {
	external  ExternalRecord
	ledger    LedgerTxn
	schedules []FeeSchedule
	failure   *Result
}

// adjustmentRefs returns the resolved break's exact external and ledger txn
// refs. An adjustment is only verifiable against the break pair, never against
// whatever ext:/ledger_txn: refs happen to appear in evidence: without this a
// fabricated adjustment could cite an unrelated ledger txn. A break with zero
// or multiple refs of either kind is ambiguous. Extra refs cited in evidence
// are ignored.
func adjustmentRefs(resolved *resolvedBreak) (string, string, []string) {
	if resolved == nil || len(resolved.ExternalRefs) != 1 || len(resolved.LedgerTxnIDs) != 1 {
		return "", "", []string{ReasonSourceAmbiguous}
	}
	externalRef, err := tools.ExternalRef(resolved.ExternalRefs[0].Source, resolved.ExternalRefs[0].SourceRecordID)
	if err != nil {
		return "", "", []string{ReasonSourceAmbiguous}
	}
	ledgerRef, err := tools.LedgerTxnRef(resolved.LedgerTxnIDs[0])
	if err != nil {
		return "", "", []string{ReasonSourceAmbiguous}
	}
	return externalRef, ledgerRef, nil
}

// breakScope is the reconciliation scope projection the loaders need.
type breakScope struct {
	LedgerAccountID string
	Source          string
	Currency        string
}

// loadBreakScope reads the break's reconciliation scope. A missing scope or an
// unusable row is a failed Result, not an I/O error.
func (s *Service) loadBreakScope(ctx context.Context, scopeID, pattern, claimed string) (breakScope, *Result, error) {
	var scope breakScope
	err := s.db.QueryRow(ctx,
		`SELECT ledger_account_id::text, source, currency
		   FROM reconciliation_scopes
		  WHERE id = $1::uuid`,
		scopeID).
		Scan(&scope.LedgerAccountID, &scope.Source, &scope.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return breakScope{}, failurePtr(pattern, claimed, "", ReasonSourceAmbiguous), nil
	}
	if err != nil {
		return breakScope{}, nil, fmt.Errorf("verify: load break scope: %w", err)
	}
	return scope, nil, nil
}

// loadAdjustmentSources binds the load to the resolved break's exact source
// pair, loads the break scope's ledger account, and (for fee_variance) every
// schedule for the external's source/kind/currency. The scope source/currency
// must match the loaded external record; entry-level scope agreement is
// enforced while deriving the ledger net.
func (s *Service) loadAdjustmentSources(ctx context.Context, resolved *resolvedBreak, pattern, claimed string, withSchedules bool) (adjustmentSources, error) {
	externalRef, ledgerRef, reasons := adjustmentRefs(resolved)
	if len(reasons) > 0 {
		return adjustmentSources{failure: failurePtr(pattern, claimed, "", reasons...)}, nil
	}

	scope, failure, err := s.loadBreakScope(ctx, resolved.ScopeID, pattern, claimed)
	if err != nil {
		return adjustmentSources{}, err
	}
	if failure != nil {
		return adjustmentSources{failure: failure}, nil
	}

	external, failure, err := s.loadExternal(ctx, externalRef, pattern, claimed)
	if err != nil {
		return adjustmentSources{}, err
	}
	if failure != nil {
		return adjustmentSources{failure: failure}, nil
	}
	if external.Source != scope.Source {
		return adjustmentSources{failure: failurePtr(pattern, claimed, external.Currency, ReasonSourceAmbiguous)}, nil
	}
	if external.Currency != scope.Currency {
		return adjustmentSources{failure: failurePtr(pattern, claimed, external.Currency, ReasonCurrencyMismatch)}, nil
	}

	ledger, failure, err := s.loadLedger(ctx, ledgerRef, scope.LedgerAccountID, scope.Currency, pattern, claimed)
	if err != nil {
		return adjustmentSources{}, err
	}
	if failure != nil {
		failure.Details.Currency = external.Currency
		return adjustmentSources{failure: failure}, nil
	}

	sources := adjustmentSources{external: external, ledger: ledger}
	if withSchedules {
		schedules, err := s.loadFeeSchedules(ctx, external)
		if err != nil {
			return adjustmentSources{}, err
		}
		sources.schedules = schedules
	}
	return sources, nil
}

// loadExternal loads one external record by canonical ref. A missing row is a
// failed Result, not an I/O error.
func (s *Service) loadExternal(ctx context.Context, ref, pattern, claimed string) (ExternalRecord, *Result, error) {
	rest := strings.TrimPrefix(ref, "ext:")
	source, recordID, ok := strings.Cut(rest, ":")
	if !ok || source == "" || recordID == "" {
		return ExternalRecord{}, failurePtr(pattern, claimed, "", ReasonSourceAmbiguous), nil
	}
	var external ExternalRecord
	err := s.db.QueryRow(ctx,
		`SELECT source, source_record_id, kind, amount_minor, currency, fee_minor,
		        occurred_at, value_date
		   FROM v_agent_external_records
		  WHERE source = $1 AND source_record_id = $2`,
		source, recordID).
		Scan(&external.Source, &external.SourceRecordID, &external.Kind,
			&external.AmountMinor, &external.Currency, &external.FeeMinor,
			&external.OccurredAt, &external.ValueDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExternalRecord{}, failurePtr(pattern, claimed, "", ReasonSourceAmbiguous), nil
	}
	if err != nil {
		return ExternalRecord{}, nil, fmt.Errorf("verify: load external record %s: %w", ref, err)
	}
	return external, nil, nil
}

// ledgerEntry is one ledger entry projection used to derive the fee and the
// scope-account net.
type ledgerEntry struct {
	AccountID   string
	AccountCode string
	Direction   string
	Currency    string
	AmountMinor int64
}

// loadLedger loads one cited ledger txn and its entries, then derives the booked
// fee and the signed net on the break scope's ledger account. Entries on any
// other account are outside the scope and never contribute to the net.
func (s *Service) loadLedger(ctx context.Context, ref, ledgerAccountID, scopeCurrency, pattern, claimed string) (LedgerTxn, *Result, error) {
	txnID := strings.TrimPrefix(ref, "ledger_txn:")
	var (
		id        string
		valueDate time.Time
	)
	err := s.db.QueryRow(ctx,
		`SELECT id::text, value_date FROM v_agent_ledger_txns WHERE id = $1`,
		txnID).Scan(&id, &valueDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return LedgerTxn{}, failurePtr(pattern, claimed, "", ReasonSourceAmbiguous), nil
	}
	if err != nil {
		return LedgerTxn{}, nil, fmt.Errorf("verify: load ledger txn %s: %w", ref, err)
	}

	rows, err := s.db.Query(ctx,
		`SELECT account_id::text, account_code, direction, amount_minor, currency
		   FROM v_agent_ledger_entries
		  WHERE txn_id = $1
		  ORDER BY id`,
		txnID)
	if err != nil {
		return LedgerTxn{}, nil, fmt.Errorf("verify: load ledger entries %s: %w", ref, err)
	}
	defer rows.Close()

	entries := make([]ledgerEntry, 0, 8)
	for rows.Next() {
		var entry ledgerEntry
		if err := rows.Scan(&entry.AccountID, &entry.AccountCode, &entry.Direction, &entry.AmountMinor, &entry.Currency); err != nil {
			return LedgerTxn{}, nil, fmt.Errorf("verify: scan ledger entry %s: %w", ref, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return LedgerTxn{}, nil, fmt.Errorf("verify: load ledger entries %s: %w", ref, err)
	}

	ledger, failure := deriveLedgerTxn(pattern, claimed, id, valueDate, ledgerAccountID, scopeCurrency, entries, pattern == PatternFeeVariance)
	if failure != nil {
		return LedgerTxn{}, failure, nil
	}
	return ledger, nil, nil
}

// deriveLedgerTxn computes LedgerTxn.FeeMinor from the single debit entry on a
// fees_expense_* account and NetMinor from the entries on the scope's ledger
// account only (debits minus credits). fee_variance requires the fee entry;
// rounding and fx_rounding do not use it. An absent scope-account entry, a
// scope-account entry in another currency, or a mixed-currency set is
// source_ambiguous; clearing entries on other accounts are ignored.
func deriveLedgerTxn(pattern, claimed, txnID string, valueDate time.Time, ledgerAccountID, scopeCurrency string, entries []ledgerEntry, requireFee bool) (LedgerTxn, *Result) {
	var (
		feeMinor    int64
		feeCurrency string
		feeSeen     bool
		scopeSeen   bool
		netMinor    int64
	)
	for _, entry := range entries {
		switch {
		case isFeeAccount(entry.AccountCode):
			if entry.Direction != "debit" || feeSeen || entry.AmountMinor <= 0 {
				return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonSourceAmbiguous)
			}
			feeSeen = true
			feeMinor = entry.AmountMinor
			feeCurrency = entry.Currency
		case entry.AccountID == ledgerAccountID:
			if entry.AmountMinor <= 0 {
				return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonInvalidInput)
			}
			if entry.Currency != scopeCurrency {
				return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonSourceAmbiguous)
			}
			scopeSeen = true
			switch entry.Direction {
			case "debit":
				next, ok := addInt64(netMinor, entry.AmountMinor)
				if !ok {
					return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonAmountOverflow)
				}
				netMinor = next
			case "credit":
				next, ok := subInt64(netMinor, entry.AmountMinor)
				if !ok {
					return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonAmountOverflow)
				}
				netMinor = next
			default:
				return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonSourceAmbiguous)
			}
		}
	}
	if !scopeSeen {
		return LedgerTxn{}, failurePtr(pattern, claimed, "", ReasonSourceAmbiguous)
	}
	if feeSeen && feeCurrency != scopeCurrency {
		return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonCurrencyMismatch)
	}
	if requireFee && !feeSeen {
		return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonSourceAmbiguous)
	}
	if valueDate.IsZero() {
		return LedgerTxn{}, failurePtr(pattern, claimed, scopeCurrency, ReasonInvalidInput)
	}
	fee := int64(0)
	if feeSeen {
		fee = feeMinor
	}
	return LedgerTxn{
		ID:        txnID,
		ValueDate: valueDate,
		Currency:  scopeCurrency,
		FeeMinor:  fee,
		NetMinor:  netMinor,
	}, nil
}

func isFeeAccount(code string) bool {
	return code == "fees_expense" || strings.HasPrefix(code, "fees_expense_")
}

// loadFeeSchedules returns every schedule candidate for the external record's
// source/kind/currency in deterministic ID order. The pure evaluator performs
// the inclusive date selection and fails closed on zero/multiple matches.
func (s *Service) loadFeeSchedules(ctx context.Context, external ExternalRecord) ([]FeeSchedule, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id::text, source, kind, currency, pct_bps, fixed_minor, valid_from, valid_to
		   FROM v_agent_fee_schedules
		  WHERE source = $1 AND kind = $2 AND currency = $3
		  ORDER BY id`,
		external.Source, external.Kind, external.Currency)
	if err != nil {
		return nil, fmt.Errorf("verify: load fee schedules: %w", err)
	}
	defer rows.Close()

	schedules := make([]FeeSchedule, 0, 4)
	for rows.Next() {
		var (
			schedule FeeSchedule
			validTo  *time.Time
		)
		if err := rows.Scan(&schedule.ID, &schedule.Source, &schedule.Kind,
			&schedule.Currency, &schedule.PctBPS, &schedule.FixedMinor,
			&schedule.ValidFrom, &validTo); err != nil {
			return nil, fmt.Errorf("verify: scan fee schedule: %w", err)
		}
		if validTo != nil {
			schedule.ValidTo = *validTo
		}
		schedules = append(schedules, schedule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("verify: load fee schedules: %w", err)
	}
	return schedules, nil
}

// verifyFeeVariance loads the adjustment sources and schedules, decodes the
// proposal lines strictly, and calls the pure evaluator.
func (s *Service) verifyFeeVariance(ctx context.Context, proposal proposalRow, resolved *resolvedBreak, claimed string) (Result, error) {
	sources, err := s.loadAdjustmentSources(ctx, resolved, PatternFeeVariance, claimed, true)
	if err != nil {
		return Result{}, err
	}
	if sources.failure != nil {
		return *sources.failure, nil
	}
	lines, err := decodeAdjustmentLines(proposal.Payload)
	if err != nil {
		return failure(PatternFeeVariance, claimed, Details{Currency: sources.external.Currency}, ReasonInvalidInput), nil
	}
	return VerifyFeeVariance(FeeVarianceInput{
		Claimed:   claimed,
		External:  sources.external,
		Ledger:    sources.ledger,
		Schedules: sources.schedules,
		Lines:     lines,
	}, s.opts), nil
}

// verifyRounding loads the adjustment sources, decodes the proposal lines
// strictly, and calls the pure evaluator.
func (s *Service) verifyRounding(ctx context.Context, proposal proposalRow, resolved *resolvedBreak, claimed string) (Result, error) {
	sources, err := s.loadAdjustmentSources(ctx, resolved, PatternRounding, claimed, false)
	if err != nil {
		return Result{}, err
	}
	if sources.failure != nil {
		return *sources.failure, nil
	}
	lines, err := decodeAdjustmentLines(proposal.Payload)
	if err != nil {
		return failure(PatternRounding, claimed, Details{Currency: sources.external.Currency}, ReasonInvalidInput), nil
	}
	return VerifyRounding(RoundingInput{
		Claimed:  claimed,
		External: sources.external,
		Ledger:   sources.ledger,
		Lines:    lines,
	}, s.opts), nil
}

// discoverFXBase finds the single base currency for the external's source and
// target currency at or before the occurrence instant. Zero candidates is a
// missing rate; more than one base is ambiguous because the pure evaluator has
// no base-currency field with which to choose.
func (s *Service) discoverFXBase(ctx context.Context, external ExternalRecord, claimed string) (string, *Result, error) {
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT base
		   FROM v_agent_fx_rates
		  WHERE source = $1 AND quote = $2 AND as_of <= $3
		  ORDER BY base`,
		external.Source, external.Currency, external.OccurredAt)
	if err != nil {
		return "", nil, fmt.Errorf("verify: discover fx base: %w", err)
	}
	defer rows.Close()

	bases := make([]string, 0, 2)
	for rows.Next() {
		var base string
		if err := rows.Scan(&base); err != nil {
			return "", nil, fmt.Errorf("verify: scan fx base: %w", err)
		}
		bases = append(bases, base)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("verify: discover fx base: %w", err)
	}
	switch len(bases) {
	case 0:
		return "", failurePtr(PatternFXRounding, claimed, external.Currency, ReasonMissingRate), nil
	case 1:
		return bases[0], nil, nil
	default:
		return "", failurePtr(PatternFXRounding, claimed, external.Currency, ReasonAmbiguousRate), nil
	}
}

// loadFXRates returns every row for the exact base/quote/source pair whose as_of
// is at or before the external occurrence instant. The pure evaluator selects
// the latest row and fails closed on a timestamp tie.
func (s *Service) loadFXRates(ctx context.Context, base, quote, source string, asOf time.Time, claimed, currency string) ([]FXRate, *Result, error) {
	rows, err := s.db.Query(ctx,
		`SELECT base, quote, source, rate, as_of
		   FROM v_agent_fx_rates
		  WHERE base = $1 AND quote = $2 AND source = $3 AND as_of <= $4
		  ORDER BY id`,
		base, quote, source, asOf)
	if err != nil {
		return nil, nil, fmt.Errorf("verify: load fx rates: %w", err)
	}
	defer rows.Close()

	rates := make([]FXRate, 0, 4)
	for rows.Next() {
		var rate FXRate
		if err := rows.Scan(&rate.Base, &rate.Quote, &rate.Source, &rate.Rate, &rate.AsOf); err != nil {
			return nil, nil, fmt.Errorf("verify: scan fx rate: %w", err)
		}
		rates = append(rates, rate)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("verify: load fx rates: %w", err)
	}
	if len(rates) == 0 {
		return nil, failurePtr(PatternFXRounding, claimed, currency, ReasonMissingRate), nil
	}
	return rates, nil, nil
}

// verifyFXRounding loads the cited source pair, derives the base/quote pair
// solely from the stored rate table for the external source and target
// currency, loads every rate at or before the occurrence instant, decodes the
// proposal lines strictly, and calls the pure evaluator. Any cited fx: evidence
// ref is untrusted and is ignored entirely, including its date: the pair is the
// unique stored base for the external source/quote, or the verifier fails
// closed.
func (s *Service) verifyFXRounding(ctx context.Context, proposal proposalRow, resolved *resolvedBreak, claimed string) (Result, error) {
	sources, err := s.loadAdjustmentSources(ctx, resolved, PatternFXRounding, claimed, false)
	if err != nil {
		return Result{}, err
	}
	if sources.failure != nil {
		return *sources.failure, nil
	}
	external := sources.external
	details := Details{Currency: external.Currency}

	base, discoverFailure, err := s.discoverFXBase(ctx, external, claimed)
	if err != nil {
		return Result{}, err
	}
	if discoverFailure != nil {
		return *discoverFailure, nil
	}
	quote := external.Currency

	rates, rateFailure, err := s.loadFXRates(ctx, base, quote, external.Source, external.OccurredAt, claimed, external.Currency)
	if err != nil {
		return Result{}, err
	}
	if rateFailure != nil {
		return *rateFailure, nil
	}
	lines, err := decodeAdjustmentLines(proposal.Payload)
	if err != nil {
		return failure(PatternFXRounding, claimed, details, ReasonInvalidInput), nil
	}
	return VerifyFXRounding(FXRoundingInput{
		Claimed:  claimed,
		External: external,
		Ledger:   sources.ledger,
		Rates:    rates,
		Lines:    lines,
	}, s.opts), nil
}

// referencePayload is the strict shape of a reference_typo proposal payload.
// Exactly one of the singular and plural forms must be present, and external_ref
// must name the break's single external record.
type referencePayload struct {
	ExternalRef  *string   `json:"external_ref"`
	LedgerTxnID  *string   `json:"ledger_txn_id"`
	LedgerTxnIDs *[]string `json:"ledger_txn_ids"`
}

// decodeReferencePayload decodes the payload strictly, requires external_ref and
// exactly one of ledger_txn_id and ledger_txn_ids, rejects non-canonical refs and
// lowercase UUIDs, and returns the canonical external ref plus a sorted unique
// ledger set.
func decodeReferencePayload(payload json.RawMessage) (string, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded referencePayload
	if err := decoder.Decode(&decoded); err != nil {
		return "", nil, fmt.Errorf("payload: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", nil, errors.New("payload has trailing data")
	}
	if decoded.ExternalRef == nil {
		return "", nil, errors.New("payload must have external_ref")
	}
	externalRef := *decoded.ExternalRef
	if err := tools.ValidateRef(externalRef); err != nil || !strings.HasPrefix(externalRef, "ext:") {
		return "", nil, fmt.Errorf("payload external_ref %q is not a canonical external reference", externalRef)
	}
	if (decoded.LedgerTxnID == nil) == (decoded.LedgerTxnIDs == nil) {
		return "", nil, errors.New("payload must have exactly one of ledger_txn_id or ledger_txn_ids")
	}
	var ids []string
	if decoded.LedgerTxnID != nil {
		ids = []string{*decoded.LedgerTxnID}
	} else {
		ids = *decoded.LedgerTxnIDs
	}
	if len(ids) == 0 {
		return "", nil, errors.New("payload ledger set is empty")
	}
	unique := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !canonicalUUIDPattern.MatchString(id) {
			return "", nil, fmt.Errorf("payload ledger id %q is not a canonical lowercase UUID", id)
		}
		unique[id] = struct{}{}
	}
	normalized := make([]string, 0, len(unique))
	for id := range unique {
		normalized = append(normalized, id)
	}
	sort.Strings(normalized)
	return externalRef, normalized, nil
}

// sameLedgerSet reports whether two ID lists name the same non-empty set.
func sameLedgerSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	for _, id := range left {
		id = strings.TrimSpace(id)
		if id != "" {
			leftSet[id] = struct{}{}
		}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, id := range right {
		id = strings.TrimSpace(id)
		if id != "" {
			rightSet[id] = struct{}{}
		}
	}
	if len(leftSet) == 0 || len(leftSet) != len(rightSet) {
		return false
	}
	for id := range leftSet {
		if _, ok := rightSet[id]; !ok {
			return false
		}
	}
	return true
}

// loadReferenceCandidates loads the eligible ledger candidates for the break's
// reconciliation scope. A candidate amount is the signed net on the scope's
// ledger account only (debits minus credits), never every clearing_* entry. It
// must equal the external amount in the scope currency, have a value date inside
// the inclusive window around the external reference date, and have no active
// match leg. Base tables are used because the verifier runs as the app role,
// which has SELECT on them.
func (s *Service) loadReferenceCandidates(ctx context.Context, scope breakScope, external ExternalRecord, claimed string) ([]Candidate, *Result, error) {
	if scope.Source != external.Source {
		return nil, failurePtr(PatternReferenceTypo, claimed, external.Currency, ReasonSourceAmbiguous), nil
	}
	if scope.Currency != external.Currency {
		return nil, failurePtr(PatternReferenceTypo, claimed, external.Currency, ReasonCurrencyMismatch), nil
	}

	rows, err := s.db.Query(ctx,
		`SELECT t.id::text, t.value_date,
		        SUM(CASE WHEN e.direction = 'debit' THEN e.amount_minor
		                 ELSE -e.amount_minor END)::bigint
		   FROM ledger_txns t
		   JOIN ledger_entries e ON e.txn_id = t.id
		  WHERE e.account_id = $1::uuid
		    AND e.currency = $3
		    AND t.value_date BETWEEN $4::date - $5::int AND $4::date + $5::int
		    AND NOT EXISTS (
		        SELECT 1 FROM match_legs ml
		         WHERE ml.ledger_txn_id = t.id AND ml.status = 'active')
		  GROUP BY t.id, t.value_date
		 HAVING SUM(CASE WHEN e.direction = 'debit' THEN e.amount_minor
		                 ELSE -e.amount_minor END) = $2
		  ORDER BY t.id`,
		scope.LedgerAccountID, external.AmountMinor, scope.Currency,
		referenceDate(external).UTC().Format("2006-01-02"), s.opts.ReferenceWindowDays)
	if err != nil {
		return nil, nil, fmt.Errorf("verify: load reference candidates: %w", err)
	}
	defer rows.Close()

	candidates := make([]Candidate, 0, 4)
	for rows.Next() {
		candidate := Candidate{Currency: scope.Currency}
		if err := rows.Scan(&candidate.LedgerTxnID, &candidate.ValueDate, &candidate.AmountMinor); err != nil {
			return nil, nil, fmt.Errorf("verify: scan reference candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("verify: load reference candidates: %w", err)
	}
	return candidates, nil, nil
}

// verifyReferenceTypo binds the link to the resolved break: the payload's
// external_ref must equal the break's single external ref, the loaded record's
// currency must equal the break currency, and a break that names ledger txns
// must have exactly that proposed set. Only an external-only break may propose
// the scope candidate discovered by amount/date. It then decodes the proposed
// ledger set strictly and calls the pure evaluator.
func (s *Service) verifyReferenceTypo(ctx context.Context, proposal proposalRow, resolved *resolvedBreak, claimed string) (Result, error) {
	payloadExternalRef, proposed, err := decodeReferencePayload(proposal.Payload)
	if err != nil {
		return failure(PatternReferenceTypo, claimed, Details{}, ReasonInvalidInput), nil
	}
	if len(resolved.ExternalRefs) != 1 {
		return failure(PatternReferenceTypo, claimed, Details{}, ReasonSourceAmbiguous), nil
	}
	breakExternalRef, err := tools.ExternalRef(resolved.ExternalRefs[0].Source, resolved.ExternalRefs[0].SourceRecordID)
	if err != nil {
		return failure(PatternReferenceTypo, claimed, Details{}, ReasonSourceAmbiguous), nil
	}
	if payloadExternalRef != breakExternalRef {
		return failure(PatternReferenceTypo, claimed, Details{}, ReasonSourceAmbiguous), nil
	}

	external, externalFailure, err := s.loadExternal(ctx, breakExternalRef, PatternReferenceTypo, claimed)
	if err != nil {
		return Result{}, err
	}
	if externalFailure != nil {
		return *externalFailure, nil
	}
	details := Details{Currency: external.Currency}
	if external.Currency != resolved.Currency {
		return failure(PatternReferenceTypo, claimed, details, ReasonCurrencyMismatch), nil
	}
	if len(resolved.LedgerTxnIDs) > 0 && !sameLedgerSet(resolved.LedgerTxnIDs, proposed) {
		return failure(PatternReferenceTypo, claimed, details, ReasonCandidateMismatch), nil
	}

	scope, scopeFailure, err := s.loadBreakScope(ctx, resolved.ScopeID, PatternReferenceTypo, claimed)
	if err != nil {
		return Result{}, err
	}
	if scopeFailure != nil {
		return *scopeFailure, nil
	}
	candidates, candidateFailure, err := s.loadReferenceCandidates(ctx, scope, external, claimed)
	if err != nil {
		return Result{}, err
	}
	if candidateFailure != nil {
		return *candidateFailure, nil
	}
	return VerifyReferenceTypo(ReferenceTypoInput{
		Claimed:           claimed,
		External:          external,
		Candidates:        candidates,
		ProposedLedgerIDs: proposed,
	}, s.opts), nil
}

// decodeAdjustmentLines strictly decodes payload.lines. An empty array, an empty
// object, an unknown field, a non-integer amount, or an invalid direction is
// malformed proposal data and becomes a failed Result (invalid_input) in the
// caller.
func decodeAdjustmentLines(payload json.RawMessage) ([]AdjustmentLine, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, fmt.Errorf("payload is not an object: %w", err)
	}
	rawLines, ok := object["lines"]
	if !ok {
		return nil, errors.New("payload has no lines")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawLines, &items); err != nil {
		return nil, fmt.Errorf("payload lines are not an array: %w", err)
	}
	if len(items) == 0 {
		return nil, errors.New("payload lines are empty")
	}
	lines := make([]AdjustmentLine, 0, len(items))
	for _, raw := range items {
		var item struct {
			AccountCode string `json:"account_code"`
			Direction   string `json:"direction"`
			AmountMinor int64  `json:"amount_minor"`
			Currency    string `json:"currency"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&item); err != nil {
			return nil, fmt.Errorf("payload line: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, errors.New("payload line has trailing data")
		}
		if item.AccountCode == "" {
			return nil, errors.New("payload line has no account_code")
		}
		if item.Direction != "debit" && item.Direction != "credit" {
			return nil, fmt.Errorf("payload line direction %q", item.Direction)
		}
		if item.AmountMinor <= 0 {
			return nil, errors.New("payload line amount_minor must be positive")
		}
		if !validCurrency(item.Currency) {
			return nil, fmt.Errorf("payload line currency %q", item.Currency)
		}
		lines = append(lines, AdjustmentLine{
			AccountCode: item.AccountCode,
			Direction:   item.Direction,
			AmountMinor: item.AmountMinor,
			Currency:    item.Currency,
		})
	}
	return lines, nil
}

// persist writes exactly the two verifier columns. pattern_verified is NULL for
// every non-pass; a concurrent status change (zero rows) rolls the caller's use
// of the result back with ErrNotVerifiable.
func (s *Service) persist(ctx context.Context, proposalID string, result Result) error {
	verification, err := result.Verification()
	if err != nil {
		return fmt.Errorf("verify: encode result: %w", err)
	}
	var verified *string
	if result.Passed {
		pattern := result.Pattern
		verified = &pattern
	}
	tag, err := s.db.Exec(ctx,
		`UPDATE proposals
		    SET verification = $2::jsonb, pattern_verified = $3
		  WHERE id = $1::uuid AND status = 'proposed'`,
		proposalID, string(verification.Details), verified)
	if err != nil {
		return fmt.Errorf("verify: persist verification: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: proposal %s changed status", ErrNotVerifiable, proposalID)
	}
	return nil
}

// failurePtr is the pointer form used by loaders.
func failurePtr(pattern, claimed, currency string, reasons ...string) *Result {
	result := failure(pattern, claimed, Details{Currency: currency}, reasons...)
	return &result
}
