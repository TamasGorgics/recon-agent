package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/eval/approver"
)

// ErrReadModel reports missing, malformed, or ambiguous operational data. The
// pipeline turns it into a fail-closed ErrE2EFailed result; it is never a
// silent approval or rejection.
var ErrReadModel = errors.New("eval/e2e: invalid operational read model")

// Queryer is the read-only pgx subset the read model needs. *pgxpool.Pool and
// pgx.Tx both satisfy it.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Loader is the narrow operational read surface used by the pipeline. Every
// method reads production/eval-sandbox tables only; none touches eval_truth or
// a ground-truth document.
type Loader interface {
	approver.RecordSource
	ProposalState(ctx context.Context, proposalID string) (ProposalState, error)
	LoadCaseFacts(ctx context.Context, caseID string) (approver.CaseFacts, error)
	LoadProposalFacts(ctx context.Context, proposalID string) (approver.ProposalFacts, error)
}

// ProposalState is the observable proposal projection needed to route a resume.
type ProposalState struct {
	ProposalID      string
	CaseID          string
	Type            string
	Status          string
	PolicyTier      *string
	PatternVerified *string
	Verification    []byte
	ExpiresAt       *time.Time
	LedgerTxnID     string
}

// Verifier projects proposals.verification / proposals.pattern_verified into
// the persisted verifier block. It is nil when the verifier never evaluated
// the proposal.
func (s ProposalState) Verifier() *Verifier {
	evaluated := s.PatternVerified != nil || hasJSON(s.Verification)
	if !evaluated {
		return nil
	}
	verifier := &Verifier{Evaluated: true, Passed: s.PatternVerified != nil}
	if s.PatternVerified != nil {
		verifier.Pattern = *s.PatternVerified
	}
	return verifier
}

// ReadModel is the pgx-backed Loader. It is safe for concurrent use when the
// underlying pool is.
type ReadModel struct {
	q Queryer
}

// NewReadModel returns a read model bound to q. q must be non-nil.
func NewReadModel(q Queryer) (*ReadModel, error) {
	if q == nil {
		return nil, errors.New("eval/e2e: read model queryer is required")
	}
	return &ReadModel{q: q}, nil
}

var _ Loader = (*ReadModel)(nil)

// ProposalState loads the routing status and policy/verifier facts for one
// proposal. applied_proposals is read for the ledger txn, not for truth.
func (m *ReadModel) ProposalState(ctx context.Context, proposalID string) (ProposalState, error) {
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return ProposalState{}, fmt.Errorf("%w: proposal id is not a canonical UUID", ErrReadModel)
	}
	var (
		state        ProposalState
		verification []byte
	)
	err := m.q.QueryRow(ctx,
		`SELECT p.id::text, p.case_id::text, p.type, p.status, p.risk_tier,
		        p.pattern_verified, p.verification, p.expires_at,
		        COALESCE(ap.ledger_txn_id::text, '')
		   FROM proposals p
		   LEFT JOIN applied_proposals ap ON ap.proposal_id = p.id
		  WHERE p.id = $1::uuid`, proposalID).
		Scan(&state.ProposalID, &state.CaseID, &state.Type, &state.Status,
			&state.PolicyTier, &state.PatternVerified, &verification,
			&state.ExpiresAt, &state.LedgerTxnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProposalState{}, fmt.Errorf("%w: proposal %s does not exist", ErrReadModel, proposalID)
	}
	if err != nil {
		return ProposalState{}, fmt.Errorf("%w: load proposal %s: %v", ErrReadModel, proposalID, err)
	}
	if hasJSON(verification) {
		state.Verification = append([]byte(nil), verification...)
	}
	return state, nil
}

// LoadCaseFacts projects the observable case facts. BreakKind comes from the
// break side only; the pipeline overlays proposals.pattern_verified after
// approval.Request because that is the verifier's observable pattern fact.
// Counterparty is the reconciliation scope source. DeltaMinor is the case's
// absolute value_minor (the sum of absolute break deltas).
func (m *ReadModel) LoadCaseFacts(ctx context.Context, caseID string) (approver.CaseFacts, error) {
	if !canonicalUUIDPattern.MatchString(caseID) {
		return approver.CaseFacts{}, fmt.Errorf("%w: case id is not a canonical UUID", ErrReadModel)
	}
	rows, err := m.q.Query(ctx,
		`SELECT c.id::text, c.value_minor, c.currency,
		        b.scope_id::text, b.side, rs.source
		   FROM cases c
		   JOIN case_breaks cb ON cb.case_id = c.id
		   JOIN breaks b ON b.id = cb.break_id
		   JOIN reconciliation_scopes rs ON rs.id = b.scope_id
		  WHERE c.id = $1::uuid
		  ORDER BY b.id`, caseID)
	if err != nil {
		return approver.CaseFacts{}, fmt.Errorf("%w: load case facts %s: %v", ErrReadModel, caseID, err)
	}
	defer rows.Close()

	var (
		facts        approver.CaseFacts
		scopeID      string
		source       string
		sides        []string
		first        = true
		caseCurrency string
	)
	for rows.Next() {
		var (
			rowCaseID   string
			value       int64
			currency    string
			breakScope  string
			side        string
			breakSource string
		)
		if err := rows.Scan(&rowCaseID, &value, &currency, &breakScope, &side, &breakSource); err != nil {
			return approver.CaseFacts{}, fmt.Errorf("%w: scan case facts %s: %v", ErrReadModel, caseID, err)
		}
		if first {
			facts.CaseID = rowCaseID
			facts.DeltaMinor = value
			facts.Currency = currency
			caseCurrency = currency
			scopeID = breakScope
			source = breakSource
			first = false
		} else if breakScope != scopeID || breakSource != source || currency != caseCurrency {
			return approver.CaseFacts{}, fmt.Errorf(
				"%w: case %s breaks span multiple scopes or currencies", ErrReadModel, caseID)
		}
		sides = append(sides, side)
	}
	if err := rows.Err(); err != nil {
		return approver.CaseFacts{}, fmt.Errorf("%w: iterate case facts %s: %v", ErrReadModel, caseID, err)
	}
	if first {
		return approver.CaseFacts{}, fmt.Errorf("%w: case %s has no attached break", ErrReadModel, caseID)
	}
	facts.ScopeID = scopeID
	facts.Counterparty = source
	facts.BreakKind = breakKindForSides(sides)
	return facts, nil
}

// LoadProposalFacts projects the stored proposal into the approver's observable
// shape. Resolution is derived from proposals.type, never from a stored
// resolution column. Cited ledger refs are canonical txn UUIDs; cited external
// refs are canonical ext:<source>:<id> strings.
func (m *ReadModel) LoadProposalFacts(ctx context.Context, proposalID string) (approver.ProposalFacts, error) {
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return approver.ProposalFacts{}, fmt.Errorf("%w: proposal id is not a canonical UUID", ErrReadModel)
	}
	var (
		facts        approver.ProposalFacts
		payload      []byte
		caseID       string
		caseCurrency string
		counterparty string
	)
	err := m.q.QueryRow(ctx,
		`SELECT p.id::text, p.case_id::text, p.type, p.payload, c.currency,
		        COALESCE((SELECT rs.source
		                    FROM case_breaks cb
		                    JOIN breaks b ON b.id = cb.break_id
		                    JOIN reconciliation_scopes rs ON rs.id = b.scope_id
		                   WHERE cb.case_id = c.id
		                   ORDER BY b.id
		                   LIMIT 1), '')
		   FROM proposals p
		   JOIN cases c ON c.id = p.case_id
		  WHERE p.id = $1::uuid`, proposalID).
		Scan(&facts.ProposalID, &caseID, &facts.Type, &payload,
			&caseCurrency, &counterparty)
	if errors.Is(err, pgx.ErrNoRows) {
		return approver.ProposalFacts{}, fmt.Errorf("%w: proposal %s does not exist", ErrReadModel, proposalID)
	}
	if err != nil {
		return approver.ProposalFacts{}, fmt.Errorf("%w: load proposal facts %s: %v", ErrReadModel, proposalID, err)
	}
	facts.Resolution = resolutionForType(facts.Type)
	facts.Counterparty = counterparty
	facts.Currency = caseCurrency

	switch facts.Type {
	case "adjustment", "write_off":
		amount, currency, err := adjustmentPayloadFacts(payload)
		if err != nil {
			return approver.ProposalFacts{}, fmt.Errorf("%w: proposal %s payload: %v", ErrReadModel, proposalID, err)
		}
		facts.AmountMinor = amount
		if currency != "" {
			facts.Currency = currency
		}
	case "link", "duplicate":
		refs, err := citedRefs(payload)
		if err != nil {
			return approver.ProposalFacts{}, fmt.Errorf("%w: proposal %s payload: %v", ErrReadModel, proposalID, err)
		}
		facts.LedgerRefs = refs.ledgerRefs
		facts.ExternalRefs = refs.externalRefs
		amount, currency := m.externalAmount(ctx, refs.externalPrimary)
		facts.AmountMinor = amount
		if currency != "" {
			facts.Currency = currency
		}
	case "request_info":
		// The lifecycle never routes request_info to the approver.
	default:
		// Unknown types are left with a zero amount; the approver rejects them
		// with type_mismatch rather than guessing.
	}
	return facts, nil
}

// Ledger resolves the cited ledger transactions to their signed effect on the
// reconciliation scope's clearing account. Debit is positive, credit negative;
// the approver requires a positive amount.
func (m *ReadModel) Ledger(ctx context.Context, ids []string) ([]approver.LedgerRecord, error) {
	if len(ids) == 0 {
		return []approver.LedgerRecord{}, nil
	}
	rows, err := m.q.Query(ctx,
		`SELECT lt.id::text, rs.id::text, le.currency,
		        sum(CASE WHEN le.direction = 'debit'
		                 THEN le.amount_minor ELSE -le.amount_minor END)::bigint
		   FROM ledger_txns lt
		   JOIN ledger_entries le ON le.txn_id = lt.id
		   JOIN reconciliation_scopes rs ON rs.ledger_account_id = le.account_id
		  WHERE lt.id::text = ANY($1::text[])
		  GROUP BY lt.id, rs.id, le.currency
		  ORDER BY lt.id, rs.id, le.currency`, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: load cited ledger records: %v", ErrReadModel, err)
	}
	defer rows.Close()

	records := make([]approver.LedgerRecord, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var record approver.LedgerRecord
		if err := rows.Scan(&record.ID, &record.ScopeID, &record.Currency, &record.AmountMinor); err != nil {
			return nil, fmt.Errorf("%w: scan cited ledger record: %v", ErrReadModel, err)
		}
		if _, duplicate := seen[record.ID]; duplicate {
			return nil, fmt.Errorf("%w: ledger txn %s has an ambiguous clearing effect", ErrReadModel, record.ID)
		}
		seen[record.ID] = struct{}{}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate cited ledger records: %v", ErrReadModel, err)
	}
	return records, nil
}

// External resolves canonical ext:<source>:<id> refs to external records. The
// record's scope comes from (source, currency) via reconciliation_scopes; the
// counterparty is the scope source, never untrusted raw text.
func (m *ReadModel) External(ctx context.Context, refs []string) ([]approver.ExternalRecord, error) {
	if len(refs) == 0 {
		return []approver.ExternalRecord{}, nil
	}
	sources := make([]string, 0, len(refs))
	recordIDs := make([]string, 0, len(refs))
	for _, ref := range refs {
		source, recordID, ok := parseExternalRef(ref)
		if !ok {
			return nil, fmt.Errorf("%w: external ref %q is malformed", ErrReadModel, ref)
		}
		sources = append(sources, source)
		recordIDs = append(recordIDs, recordID)
	}
	rows, err := m.q.Query(ctx,
		`SELECT er.source, er.source_record_id, er.amount_minor, er.currency,
		        COALESCE(rs.id::text, '')
		   FROM unnest($1::text[], $2::text[]) AS wanted(source, record_id)
		   JOIN external_records er
		     ON er.source = wanted.source AND er.source_record_id = wanted.record_id
		   LEFT JOIN reconciliation_scopes rs
		     ON rs.source = er.source AND rs.currency = er.currency
		  ORDER BY er.source, er.source_record_id`, sources, recordIDs)
	if err != nil {
		return nil, fmt.Errorf("%w: load cited external records: %v", ErrReadModel, err)
	}
	defer rows.Close()

	records := make([]approver.ExternalRecord, 0, len(refs))
	for rows.Next() {
		var (
			source   string
			recordID string
			amount   int64
			currency string
			scopeID  string
		)
		if err := rows.Scan(&source, &recordID, &amount, &currency, &scopeID); err != nil {
			return nil, fmt.Errorf("%w: scan cited external record: %v", ErrReadModel, err)
		}
		records = append(records, approver.ExternalRecord{
			ID:           externalRef(source, recordID),
			ScopeID:      scopeID,
			AmountMinor:  amount,
			Currency:     currency,
			Counterparty: source,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate cited external records: %v", ErrReadModel, err)
	}
	return records, nil
}

// externalAmount returns the cited external record's amount/currency for a
// link or duplicate proposal. A missing record yields zero and the case
// currency: the approver's external-ref predicate then rejects the proposal.
func (m *ReadModel) externalAmount(ctx context.Context, ref string) (int64, string) {
	source, recordID, ok := parseExternalRef(ref)
	if !ok {
		return 0, ""
	}
	var (
		amount   int64
		currency string
	)
	err := m.q.QueryRow(ctx,
		`SELECT amount_minor, currency
		   FROM external_records
		  WHERE source = $1 AND source_record_id = $2`, source, recordID).
		Scan(&amount, &currency)
	if err != nil {
		return 0, ""
	}
	return amount, currency
}

type citedRefSet struct {
	ledgerRefs      []string
	externalRefs    []string
	externalPrimary string
}

type storedPayload struct {
	LedgerTxnID  *string  `json:"ledger_txn_id"`
	LedgerTxnIDs []string `json:"ledger_txn_ids"`
	ExternalRef  string   `json:"external_ref"`
	DuplicateOf  string   `json:"duplicate_of"`
}

func citedRefs(payload []byte) (citedRefSet, error) {
	var stored storedPayload
	if err := json.Unmarshal(payload, &stored); err != nil {
		return citedRefSet{}, err
	}
	set := citedRefSet{}
	if stored.LedgerTxnID != nil && *stored.LedgerTxnID != "" {
		set.ledgerRefs = []string{*stored.LedgerTxnID}
	}
	set.ledgerRefs = append(set.ledgerRefs, stored.LedgerTxnIDs...)
	if stored.ExternalRef != "" {
		set.externalRefs = append(set.externalRefs, stored.ExternalRef)
		set.externalPrimary = stored.ExternalRef
	}
	if stored.DuplicateOf != "" {
		if stored.DuplicateOf != stored.ExternalRef {
			set.externalRefs = append(set.externalRefs, stored.DuplicateOf)
		}
		if set.externalPrimary == "" {
			set.externalPrimary = stored.DuplicateOf
		}
	}
	return set, nil
}

type adjustmentPayloadLine struct {
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// adjustmentPayloadFacts returns the debit total and the single debit currency
// (empty when debits mix currencies or are absent).
func adjustmentPayloadFacts(payload []byte) (int64, string, error) {
	var stored struct {
		Lines []adjustmentPayloadLine `json:"lines"`
	}
	if err := json.Unmarshal(payload, &stored); err != nil {
		return 0, "", err
	}
	var (
		total      int64
		currencies []string
	)
	for _, line := range stored.Lines {
		if line.Direction != "debit" || line.AmountMinor <= 0 {
			continue
		}
		total += line.AmountMinor
		if line.Currency != "" && !containsString(currencies, line.Currency) {
			currencies = append(currencies, line.Currency)
		}
	}
	if len(currencies) == 1 {
		return total, currencies[0], nil
	}
	return total, "", nil
}

// resolutionForType is the only resolution mapping: the stored proposal type.
func resolutionForType(proposalType string) string {
	switch proposalType {
	case "link", "adjustment", "write_off", "duplicate":
		return proposalType
	default:
		return ""
	}
}

// breakKindForSides maps observable break sides into the approver's break-kind
// vocabulary. A verified pattern overrides this after Request.
func breakKindForSides(sides []string) string {
	kind := "other"
	for _, side := range sides {
		switch side {
		case "duplicate":
			return "duplicate_charge"
		case "amount_mismatch":
			kind = "fee_variance"
		}
	}
	return kind
}

// parseExternalRef splits a canonical ext:<source>:<record_id> ref.
func parseExternalRef(ref string) (string, string, bool) {
	if ref == "" || ref != strings.TrimSpace(ref) || strings.ContainsAny(ref, " \t\r\n") {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(ref, "ext:")
	if !ok {
		return "", "", false
	}
	source, recordID, ok := strings.Cut(rest, ":")
	if !ok || source == "" || recordID == "" {
		return "", "", false
	}
	return source, recordID, true
}

func externalRef(source, recordID string) string {
	return "ext:" + source + ":" + recordID
}

func hasJSON(document []byte) bool {
	trimmed := strings.TrimSpace(string(document))
	return trimmed != "" && trimmed != "null"
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
