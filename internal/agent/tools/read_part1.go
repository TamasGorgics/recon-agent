package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Part-1 read tools: get_case, lookup_ledger_txn, lookup_external_record and
// search_external_records. Every query is static SQL with $n parameters against
// the v_agent_* views only; the queryer is always the agent-pool read surface.

const (
	sqlGetCase = `
		SELECT id, priority, value_minor, currency, status, version, attempts,
		       escalation_reason, created_at, resolved_at, resolution_type
		  FROM v_agent_cases
		 WHERE id = $1`

	sqlCaseBreaks = `
		SELECT b.id, b.signature, b.scope_id, b.side, b.ledger_txn_ids,
		       b.external_refs, b.delta_minor, b.currency, b.status, b.detected_at
		  FROM v_agent_case_breaks cb
		  JOIN v_agent_breaks b ON b.id = cb.break_id
		 WHERE cb.case_id = $1
		 ORDER BY b.detected_at DESC, b.id
		 LIMIT 51`

	sqlLedgerTxnByID = `
		SELECT id, kind, value_date, created_at, booked_at, description,
		       period_id, adjusts_period_id, reversal_of
		  FROM v_agent_ledger_txns
		 WHERE id = $1
		 LIMIT 1`

	sqlLedgerEntriesByTxn = `
		SELECT id, txn_id, account_id, account_code, account_name, direction,
		       amount_minor, currency, posted_at, external_ref
		  FROM v_agent_ledger_entries
		 WHERE txn_id = $1
		 ORDER BY posted_at, id`

	sqlLedgerTxnsByExternalRef = `
		SELECT DISTINCT t.id, t.kind, t.value_date, t.created_at, t.booked_at,
		       t.description, t.period_id, t.adjusts_period_id, t.reversal_of
		  FROM v_agent_ledger_txns t
		  JOIN v_agent_ledger_entries e ON e.txn_id = t.id
		 WHERE e.external_ref = $1
		 ORDER BY t.value_date DESC, t.created_at DESC, t.id
		 LIMIT 51`

	sqlLedgerEntriesForTxns = `
		SELECT e.id, e.txn_id, e.account_id, e.account_code, e.account_name,
		       e.direction, e.amount_minor, e.currency, e.posted_at, e.external_ref
		  FROM v_agent_ledger_entries e
		  JOIN v_agent_ledger_txns t ON t.id = e.txn_id
		 WHERE e.txn_id = ANY($1::text[])
		 ORDER BY t.value_date DESC, t.created_at DESC, t.id, e.posted_at, e.id
		 LIMIT 201`

	sqlExternalRecord = `
		SELECT id, source, source_record_id, kind, amount_minor, currency,
		       fee_minor, occurred_at, settled_at, value_date, batch_id, raw, raw_hash
		  FROM v_agent_external_records
		 WHERE source = $1 AND source_record_id = $2`

	sqlSearchExternalRecords = `
		SELECT id, source, source_record_id, kind, amount_minor, currency,
		       fee_minor, occurred_at, settled_at, value_date, batch_id, raw, raw_hash
		  FROM v_agent_external_records
		 WHERE currency = $1
		   AND amount_minor = $2
		   AND value_date BETWEEN $3::date AND $4::date
		   AND ($5::text IS NULL
		        OR source_record_id = $5
		        OR raw->>'reference' = $5
		        OR raw->>'bank_reference' = $5)
		 ORDER BY value_date DESC, source, source_record_id
		 LIMIT 51`
)

const (
	maxCaseBreaks             = 50
	maxLedgerTxns             = 50
	maxLedgerEntries          = 200
	maxSearchRows             = 50
	maxSearchWindowDays       = 30 // inclusive 31-day window
	maxExternalReferenceChars = 128
)

// RegisterPart1 registers the part-1 read tools. It is composed explicitly by
// the agent loop; the package has no init-time registration.
func RegisterPart1(registry *Registry, deps Deps) error {
	if registry == nil {
		return errors.New("tools: RegisterPart1: nil registry")
	}
	part1Tools := []Tool{
		&getCaseTool{deps: deps},
		&lookupLedgerTxnTool{deps: deps},
		&lookupExternalRecordTool{deps: deps},
		&searchExternalRecordsTool{deps: deps},
	}
	for _, tool := range part1Tools {
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

// part1Queryer prefers the per-run agent queryer and falls back to the Deps
// queryer for callers that register tools outside a run.
func part1Queryer(run *RunContext, deps Deps) (Queryer, error) {
	if run != nil && run.Agent != nil {
		return run.Agent, nil
	}
	if deps.Agent != nil {
		return deps.Agent, nil
	}
	return nil, fmt.Errorf("%w: no agent queryer configured", ErrInternal)
}

func decodePart1Args(data json.RawMessage, dst any) error {
	if err := DecodeArgs(data, dst); err != nil {
		return wrapInvalidArguments("arguments are invalid")
	}
	return nil
}

type getCaseRequest struct {
	CaseID string `json:"case_id"`
}

type getCaseTool struct {
	deps Deps
}

func (t *getCaseTool) Name() string { return "get_case" }

func (t *getCaseTool) Description() string {
	return "Fetch a reconciliation case with its current state and break details."
}

func (t *getCaseTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["case_id"],
		"properties": {
			"case_id": {
				"type": "string",
				"pattern": "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
			}
		}
	}`)
}

func (t *getCaseTool) Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error) {
	var req getCaseRequest
	if err := decodePart1Args(args, &req); err != nil {
		return Result{}, err
	}
	caseID, err := ParseUUID(req.CaseID)
	if err != nil {
		return Result{}, err
	}
	caseID = strings.ToLower(caseID)

	queryer, err := part1Queryer(run, t.deps)
	if err != nil {
		return Result{}, err
	}

	var (
		result       caseResult
		createdAt    time.Time
		resolvedAt   *time.Time
		escalation   *string
		resolutionTy *string
	)
	err = queryer.QueryRow(ctx, sqlGetCase, caseID).Scan(
		&result.ID, &result.Priority, &result.ValueMinor, &result.Currency,
		&result.Status, &result.Version, &result.Attempts, &escalation,
		&createdAt, &resolvedAt, &resolutionTy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, NotFound("case not found")
	}
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_case query: %w", err)
	}
	result.EscalationReason = escalation
	result.CreatedAt = FormatTimestamp(createdAt)
	result.ResolvedAt = OptionalTimestamp(resolvedAt)
	result.ResolutionType = resolutionTy
	result.Breaks = []caseBreakResult{}

	rows, err := queryer.Query(ctx, sqlCaseBreaks, caseID)
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_case breaks query: %w", err)
	}
	breakRows, err := CollectRows(rows, scanCaseBreak)
	if err != nil {
		return Result{}, err
	}
	truncated := len(breakRows) > maxCaseBreaks
	if truncated {
		breakRows = breakRows[:maxCaseBreaks]
	}

	refs := make([]string, 0, 1+len(breakRows)*2)
	caseRef, err := CaseRef(result.ID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: stored case id: %v", ErrInternal, err)
	}
	refs = append(refs, caseRef)
	for _, breakRow := range breakRows {
		result.Breaks = append(result.Breaks, breakRow.result())
		breakRefs, err := breakRow.evidenceRefs()
		if err != nil {
			return Result{}, err
		}
		refs = append(refs, breakRefs...)
	}

	return Result{Data: result, Refs: refs, Truncated: truncated}, nil
}

type caseExternalRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

type caseResult struct {
	ID               string            `json:"id"`
	Priority         int               `json:"priority"`
	ValueMinor       int64             `json:"value_minor"`
	Currency         string            `json:"currency"`
	Status           string            `json:"status"`
	Version          int               `json:"version"`
	Attempts         int               `json:"attempts"`
	EscalationReason *string           `json:"escalation_reason"`
	CreatedAt        string            `json:"created_at"`
	ResolvedAt       *string           `json:"resolved_at"`
	ResolutionType   *string           `json:"resolution_type"`
	Breaks           []caseBreakResult `json:"breaks"`
}

type caseBreakRow struct {
	ID           string
	Signature    string
	ScopeID      string
	Side         string
	LedgerTxnIDs []string
	ExternalRefs []caseExternalRef
	DeltaMinor   int64
	Currency     string
	Status       string
	DetectedAt   time.Time
}

type caseBreakResult struct {
	ID           string            `json:"id"`
	Signature    string            `json:"signature"`
	ScopeID      string            `json:"scope_id"`
	Side         string            `json:"side"`
	LedgerTxnIDs []string          `json:"ledger_txn_ids"`
	ExternalRefs []caseExternalRef `json:"external_refs"`
	DeltaMinor   int64             `json:"delta_minor"`
	Currency     string            `json:"currency"`
	Status       string            `json:"status"`
	DetectedAt   string            `json:"detected_at"`
}

func (b caseBreakRow) result() caseBreakResult {
	return caseBreakResult{
		ID:           b.ID,
		Signature:    b.Signature,
		ScopeID:      b.ScopeID,
		Side:         b.Side,
		LedgerTxnIDs: b.LedgerTxnIDs,
		ExternalRefs: b.ExternalRefs,
		DeltaMinor:   b.DeltaMinor,
		Currency:     b.Currency,
		Status:       b.Status,
		DetectedAt:   FormatTimestamp(b.DetectedAt),
	}
}

func (b caseBreakRow) evidenceRefs() ([]string, error) {
	refs := make([]string, 0, len(b.LedgerTxnIDs)+len(b.ExternalRefs))
	for _, txnID := range b.LedgerTxnIDs {
		ref, err := LedgerTxnRef(txnID)
		if err != nil {
			return nil, fmt.Errorf("%w: break %s: %v", ErrInternal, b.ID, err)
		}
		refs = append(refs, ref)
	}
	for _, external := range b.ExternalRefs {
		ref, err := ExternalRef(external.Source, external.SourceRecordID)
		if err != nil {
			return nil, fmt.Errorf("%w: break %s: %v", ErrInternal, b.ID, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func scanCaseBreak(rows pgx.Rows) (caseBreakRow, error) {
	var (
		breakRow caseBreakRow
		refsJSON []byte
		detected time.Time
	)
	if err := rows.Scan(
		&breakRow.ID, &breakRow.Signature, &breakRow.ScopeID, &breakRow.Side,
		&breakRow.LedgerTxnIDs, &refsJSON, &breakRow.DeltaMinor,
		&breakRow.Currency, &breakRow.Status, &detected,
	); err != nil {
		return caseBreakRow{}, err
	}
	if len(refsJSON) > 0 {
		if err := json.Unmarshal(refsJSON, &breakRow.ExternalRefs); err != nil {
			return caseBreakRow{}, fmt.Errorf("decode break external_refs: %w", err)
		}
	}
	if breakRow.LedgerTxnIDs == nil {
		breakRow.LedgerTxnIDs = []string{}
	}
	if breakRow.ExternalRefs == nil {
		breakRow.ExternalRefs = []caseExternalRef{}
	}
	breakRow.DetectedAt = detected
	return breakRow, nil
}

type lookupLedgerTxnRequest struct {
	TxnID       *string `json:"txn_id"`
	ExternalRef *string `json:"external_ref"`
}

type lookupLedgerTxnTool struct {
	deps Deps
}

func (t *lookupLedgerTxnTool) Name() string { return "lookup_ledger_txn" }

func (t *lookupLedgerTxnTool) Description() string {
	return "Fetch one ledger transaction or the transactions sharing an external reference, with their entries."
}

func (t *lookupLedgerTxnTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"oneOf": [
			{
				"type": "object",
				"additionalProperties": false,
				"minProperties": 1,
				"maxProperties": 1,
				"properties": {
					"txn_id": {
						"type": "string",
						"pattern": "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
					}
				}
			},
			{
				"type": "object",
				"additionalProperties": false,
				"minProperties": 1,
				"maxProperties": 1,
				"properties": {
					"external_ref": {
						"type": "string",
						"minLength": 1,
						"maxLength": 512
					}
				}
			}
		],
		"properties": {
			"txn_id": {
				"type": "string",
				"pattern": "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
			},
			"external_ref": {
				"type": "string",
				"minLength": 1,
				"maxLength": 512
			}
		}
	}`)
}

func (t *lookupLedgerTxnTool) Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error) {
	var req lookupLedgerTxnRequest
	if err := decodePart1Args(args, &req); err != nil {
		return Result{}, err
	}
	if (req.TxnID == nil) == (req.ExternalRef == nil) {
		return Result{}, wrapInvalidArguments("exactly one of txn_id or external_ref is required")
	}
	if req.ExternalRef != nil && *req.ExternalRef == "" {
		return Result{}, wrapInvalidArguments("external_ref must not be empty")
	}
	queryer, err := part1Queryer(run, t.deps)
	if err != nil {
		return Result{}, err
	}

	if req.TxnID != nil {
		return lookupLedgerTxnByID(ctx, queryer, *req.TxnID)
	}
	return lookupLedgerTxnsByExternalRef(ctx, queryer, *req.ExternalRef)
}

func lookupLedgerTxnByID(ctx context.Context, queryer Queryer, rawID string) (Result, error) {
	txnID, err := ParseUUID(rawID)
	if err != nil {
		return Result{}, err
	}
	txnID = strings.ToLower(txnID)

	headerRows, err := queryer.Query(ctx, sqlLedgerTxnByID, txnID)
	if err != nil {
		return Result{}, fmt.Errorf("tools: lookup_ledger_txn header query: %w", err)
	}
	headers, err := CollectRows(headerRows, scanLedgerTxnHeader)
	if err != nil {
		return Result{}, err
	}
	if len(headers) == 0 {
		return Result{}, NotFound("ledger transaction not found")
	}

	entryRows, err := queryer.Query(ctx, sqlLedgerEntriesByTxn, txnID)
	if err != nil {
		return Result{}, fmt.Errorf("tools: lookup_ledger_txn entries query: %w", err)
	}
	entries, err := CollectRows(entryRows, scanLedgerEntry)
	if err != nil {
		return Result{}, err
	}

	txn := headers[0].result(entries)
	ref, err := LedgerTxnRef(txn.ID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: stored ledger txn id: %v", ErrInternal, err)
	}
	return Result{
		Data: ledgerLookupResult{Transactions: []ledgerTxnResult{txn}},
		Refs: []string{ref},
	}, nil
}

func lookupLedgerTxnsByExternalRef(ctx context.Context, queryer Queryer, externalRef string) (Result, error) {
	headerRows, err := queryer.Query(ctx, sqlLedgerTxnsByExternalRef, externalRef)
	if err != nil {
		return Result{}, fmt.Errorf("tools: lookup_ledger_txn external_ref query: %w", err)
	}
	headers, err := CollectRows(headerRows, scanLedgerTxnHeader)
	if err != nil {
		return Result{}, err
	}
	if len(headers) == 0 {
		return Result{}, NotFound("no ledger transactions match that external reference")
	}
	truncated := len(headers) > maxLedgerTxns
	if truncated {
		headers = headers[:maxLedgerTxns]
	}

	ids := make([]string, 0, len(headers))
	refs := make([]string, 0, len(headers))
	for _, header := range headers {
		ids = append(ids, header.ID)
		ref, err := LedgerTxnRef(header.ID)
		if err != nil {
			return Result{}, fmt.Errorf("%w: stored ledger txn id: %v", ErrInternal, err)
		}
		refs = append(refs, ref)
	}

	entryRows, err := queryer.Query(ctx, sqlLedgerEntriesForTxns, ids)
	if err != nil {
		return Result{}, fmt.Errorf("tools: lookup_ledger_txn entries query: %w", err)
	}
	entries, err := CollectRows(entryRows, scanLedgerEntry)
	if err != nil {
		return Result{}, err
	}
	if len(entries) > maxLedgerEntries {
		entries = entries[:maxLedgerEntries]
		truncated = true
	}

	byTxn := make(map[string][]ledgerEntryResult, len(headers))
	for _, entry := range entries {
		byTxn[entry.TxnID] = append(byTxn[entry.TxnID], entry)
	}
	transactions := make([]ledgerTxnResult, 0, len(headers))
	for _, header := range headers {
		txnEntries := byTxn[header.ID]
		if txnEntries == nil {
			txnEntries = []ledgerEntryResult{}
		}
		transactions = append(transactions, header.result(txnEntries))
	}

	return Result{
		Data:      ledgerLookupResult{Transactions: transactions},
		Refs:      refs,
		Truncated: truncated,
	}, nil
}

type ledgerLookupResult struct {
	Transactions []ledgerTxnResult `json:"transactions"`
}

type ledgerTxnRow struct {
	ID              string
	Kind            string
	ValueDate       time.Time
	CreatedAt       time.Time
	BookedAt        time.Time
	Description     string
	PeriodID        string
	AdjustsPeriodID *string
	ReversalOf      *string
}

type ledgerTxnResult struct {
	ID              string              `json:"id"`
	Kind            string              `json:"kind"`
	ValueDate       string              `json:"value_date"`
	CreatedAt       string              `json:"created_at"`
	BookedAt        string              `json:"booked_at"`
	Description     string              `json:"description"`
	PeriodID        string              `json:"period_id"`
	AdjustsPeriodID *string             `json:"adjusts_period_id"`
	ReversalOf      *string             `json:"reversal_of"`
	Entries         []ledgerEntryResult `json:"entries"`
}

func (t ledgerTxnRow) result(entries []ledgerEntryResult) ledgerTxnResult {
	if entries == nil {
		entries = []ledgerEntryResult{}
	}
	return ledgerTxnResult{
		ID:              t.ID,
		Kind:            t.Kind,
		ValueDate:       FormatDate(t.ValueDate),
		CreatedAt:       FormatTimestamp(t.CreatedAt),
		BookedAt:        FormatTimestamp(t.BookedAt),
		Description:     t.Description,
		PeriodID:        t.PeriodID,
		AdjustsPeriodID: t.AdjustsPeriodID,
		ReversalOf:      t.ReversalOf,
		Entries:         entries,
	}
}

func scanLedgerTxnHeader(rows pgx.Rows) (ledgerTxnRow, error) {
	var header ledgerTxnRow
	if err := rows.Scan(
		&header.ID, &header.Kind, &header.ValueDate, &header.CreatedAt,
		&header.BookedAt, &header.Description, &header.PeriodID,
		&header.AdjustsPeriodID, &header.ReversalOf,
	); err != nil {
		return ledgerTxnRow{}, err
	}
	return header, nil
}

type ledgerEntryResult struct {
	ID          string  `json:"id"`
	TxnID       string  `json:"txn_id"`
	AccountID   string  `json:"account_id"`
	AccountCode string  `json:"account_code"`
	AccountName string  `json:"account_name"`
	Direction   string  `json:"direction"`
	AmountMinor int64   `json:"amount_minor"`
	Currency    string  `json:"currency"`
	PostedAt    string  `json:"posted_at"`
	ExternalRef *string `json:"external_ref"`
}

func scanLedgerEntry(rows pgx.Rows) (ledgerEntryResult, error) {
	var (
		entry    ledgerEntryResult
		postedAt time.Time
	)
	if err := rows.Scan(
		&entry.ID, &entry.TxnID, &entry.AccountID, &entry.AccountCode,
		&entry.AccountName, &entry.Direction, &entry.AmountMinor,
		&entry.Currency, &postedAt, &entry.ExternalRef,
	); err != nil {
		return ledgerEntryResult{}, err
	}
	entry.PostedAt = FormatTimestamp(postedAt)
	return entry, nil
}

type lookupExternalRecordRequest struct {
	Source   string `json:"source"`
	RecordID string `json:"record_id"`
}

type lookupExternalRecordTool struct {
	deps Deps
}

func (t *lookupExternalRecordTool) Name() string { return "lookup_external_record" }

func (t *lookupExternalRecordTool) Description() string {
	return "Fetch one normalized external payment record, including its raw fields and untrusted-field list."
}

func (t *lookupExternalRecordTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["source", "record_id"],
		"properties": {
			"source": {
				"type": "string",
				"pattern": "^[a-z][a-z0-9_]*$",
				"maxLength": 64
			},
			"record_id": {
				"type": "string",
				"minLength": 1,
				"maxLength": 512
			}
		}
	}`)
}

func (t *lookupExternalRecordTool) Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error) {
	var req lookupExternalRecordRequest
	if err := decodePart1Args(args, &req); err != nil {
		return Result{}, err
	}
	if req.Source == "" || req.RecordID == "" {
		return Result{}, wrapInvalidArguments("source and record_id are required")
	}
	queryer, err := part1Queryer(run, t.deps)
	if err != nil {
		return Result{}, err
	}

	rows, err := queryer.Query(ctx, sqlExternalRecord, req.Source, req.RecordID)
	if err != nil {
		return Result{}, fmt.Errorf("tools: lookup_external_record query: %w", err)
	}
	records, err := CollectRows(rows, scanExternalRecord)
	if err != nil {
		return Result{}, err
	}
	if len(records) == 0 {
		return Result{}, NotFound("external record not found")
	}
	record := records[0]
	result, err := newExternalRecordResult(record, t.deps)
	if err != nil {
		return Result{}, err
	}
	refs, err := externalRecordRefs(record)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: result, Refs: refs}, nil
}

type externalRecordRow struct {
	ID             string
	Source         string
	SourceRecordID string
	Kind           string
	AmountMinor    int64
	Currency       string
	FeeMinor       int64
	OccurredAt     time.Time
	SettledAt      *time.Time
	ValueDate      time.Time
	BatchID        string
	Raw            []byte
	RawHash        string
}

type externalRecordResult struct {
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	SourceRecordID  string          `json:"source_record_id"`
	Kind            string          `json:"kind"`
	AmountMinor     int64           `json:"amount_minor"`
	Currency        string          `json:"currency"`
	FeeMinor        int64           `json:"fee_minor"`
	OccurredAt      string          `json:"occurred_at"`
	SettledAt       *string         `json:"settled_at"`
	ValueDate       string          `json:"value_date"`
	BatchID         string          `json:"batch_id"`
	Raw             json.RawMessage `json:"raw"`
	RawHash         string          `json:"raw_hash"`
	UntrustedFields []string        `json:"untrusted_fields"`
}

func scanExternalRecord(rows pgx.Rows) (externalRecordRow, error) {
	var record externalRecordRow
	if err := rows.Scan(
		&record.ID, &record.Source, &record.SourceRecordID, &record.Kind,
		&record.AmountMinor, &record.Currency, &record.FeeMinor,
		&record.OccurredAt, &record.SettledAt, &record.ValueDate,
		&record.BatchID, &record.Raw, &record.RawHash,
	); err != nil {
		return externalRecordRow{}, err
	}
	return record, nil
}

func newExternalRecordResult(record externalRecordRow, deps Deps) (externalRecordResult, error) {
	raw := record.Raw
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var rawFields map[string]any
	if err := DecodeJSONB(raw, &rawFields); err != nil {
		return externalRecordResult{}, fmt.Errorf("%w: stored raw JSON", ErrInternal)
	}
	return externalRecordResult{
		ID:              record.ID,
		Source:          record.Source,
		SourceRecordID:  record.SourceRecordID,
		Kind:            record.Kind,
		AmountMinor:     record.AmountMinor,
		Currency:        record.Currency,
		FeeMinor:        record.FeeMinor,
		OccurredAt:      FormatTimestamp(record.OccurredAt),
		SettledAt:       OptionalTimestamp(record.SettledAt),
		ValueDate:       FormatDate(record.ValueDate),
		BatchID:         record.BatchID,
		Raw:             json.RawMessage(raw),
		RawHash:         record.RawHash,
		UntrustedFields: ListUntrustedFields(record.Source, rawFields, deps.UntrustedFields),
	}, nil
}

func externalRecordRefs(record externalRecordRow) ([]string, error) {
	ext, err := ExternalRef(record.Source, record.SourceRecordID)
	if err != nil {
		return nil, fmt.Errorf("%w: stored external record id: %v", ErrInternal, err)
	}
	batch, err := BatchRef(record.BatchID)
	if err != nil {
		return nil, fmt.Errorf("%w: stored batch id: %v", ErrInternal, err)
	}
	return []string{ext, batch}, nil
}

type searchExternalRecordsRequest struct {
	AmountMinor int64   `json:"amount_minor"`
	Currency    string  `json:"currency"`
	From        string  `json:"from"`
	To          string  `json:"to"`
	Reference   *string `json:"reference"`
}

type searchExternalRecordsTool struct {
	deps Deps
}

func (t *searchExternalRecordsTool) Name() string { return "search_external_records" }

func (t *searchExternalRecordsTool) Description() string {
	return "Search external records by integer amount, currency, an inclusive value-date window of at most 31 days, and optionally an exact reference."
}

func (t *searchExternalRecordsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["amount_minor", "currency", "from", "to"],
		"properties": {
			"amount_minor": {"type": "integer"},
			"currency": {"type": "string", "pattern": "^[A-Z]{3}$"},
			"from": {"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$"},
			"to": {"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$"},
			"reference": {"type": "string", "minLength": 1, "maxLength": 128}
		}
	}`)
}

func (t *searchExternalRecordsTool) Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error) {
	var req searchExternalRecordsRequest
	if err := decodePart1Args(args, &req); err != nil {
		return Result{}, err
	}
	currency, err := ParseCurrency(req.Currency)
	if err != nil {
		return Result{}, err
	}
	from, err := ParseDate(req.From)
	if err != nil {
		return Result{}, err
	}
	to, err := ParseDate(req.To)
	if err != nil {
		return Result{}, err
	}
	if from.After(to) {
		return Result{}, wrapInvalidArguments("from must be on or before to")
	}
	if to.Sub(from) > maxSearchWindowDays*24*time.Hour {
		return Result{}, wrapInvalidArguments("the date window must be at most 31 calendar days")
	}
	if req.Reference != nil {
		if *req.Reference == "" || utf8.RuneCountInString(*req.Reference) > maxExternalReferenceChars {
			return Result{}, wrapInvalidArguments("reference must be between 1 and 128 characters")
		}
	}
	queryer, err := part1Queryer(run, t.deps)
	if err != nil {
		return Result{}, err
	}

	var reference any
	if req.Reference != nil {
		reference = *req.Reference
	}
	rows, err := queryer.Query(ctx, sqlSearchExternalRecords,
		currency.Code(), req.AmountMinor, FormatDate(from), FormatDate(to), reference)
	if err != nil {
		return Result{}, fmt.Errorf("tools: search_external_records query: %w", err)
	}
	records, err := CollectRows(rows, scanExternalRecord)
	if err != nil {
		return Result{}, err
	}
	truncated := len(records) > maxSearchRows
	if truncated {
		records = records[:maxSearchRows]
	}

	data := make([]externalRecordResult, 0, len(records))
	refs := make([]string, 0, len(records)*2)
	for _, record := range records {
		row, err := newExternalRecordResult(record, t.deps)
		if err != nil {
			return Result{}, err
		}
		data = append(data, row)
		recordRefs, err := externalRecordRefs(record)
		if err != nil {
			return Result{}, err
		}
		refs = append(refs, recordRefs...)
	}
	return Result{Data: data, Refs: refs, Truncated: truncated}, nil
}
