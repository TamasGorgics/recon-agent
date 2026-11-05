package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var part2HistorySignaturePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const part2SettlementBatchSchema = `{
	"type": "object",
	"additionalProperties": false,
	"required": ["batch_id"],
	"properties": {
		"batch_id": {
			"type": "string",
			"pattern": "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
		}
	}
}`

const part2FXRateSchema = `{
	"type": "object",
	"additionalProperties": false,
	"required": ["base", "quote", "date"],
	"properties": {
		"base": {"type": "string", "pattern": "^[A-Z]{3}$"},
		"quote": {"type": "string", "pattern": "^[A-Z]{3}$"},
		"date": {"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$"}
	}
}`

const part2FeeScheduleSchema = `{
	"type": "object",
	"additionalProperties": false,
	"required": ["source", "kind", "currency", "date"],
	"properties": {
		"source": {"type": "string", "minLength": 1, "pattern": "^[a-z][a-z0-9_]*$"},
		"kind": {"type": "string", "minLength": 1},
		"currency": {"type": "string", "pattern": "^[A-Z]{3}$"},
		"date": {"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$"}
	}
}`

const part2HistorySchema = `{
	"type": "object",
	"additionalProperties": false,
	"required": ["signature"],
	"properties": {
		"signature": {"type": "string", "pattern": "^[0-9a-f]{64}$"}
	}
}`

// RegisterPart2 registers the M2-06 read tools. M2-09 composes RegisterPart1
// and RegisterPart2; the registry itself registers nothing implicitly.
func RegisterPart2(registry *Registry, deps Deps) error {
	if registry == nil {
		return fmt.Errorf("%w: nil registry", ErrInvalidTool)
	}
	tools := []Tool{
		part2SettlementBatchTool{deps: deps},
		part2FXRateTool{},
		part2FeeScheduleTool{},
		part2HistoryTool{},
	}
	for _, tool := range tools {
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

// part2RunQueryer returns the agent-pool query surface or a generic internal
// error. Tool bodies never receive the app pool.
func part2RunQueryer(run *RunContext) (Queryer, error) {
	if run == nil || run.Agent == nil {
		return nil, fmt.Errorf("%w: run context has no agent queryer", ErrInternal)
	}
	return run.Agent, nil
}

// ---------------------------------------------------------------------------
// get_settlement_batch
// ---------------------------------------------------------------------------

type part2SettlementBatchTool struct {
	deps Deps
}

func (part2SettlementBatchTool) Name() string { return "get_settlement_batch" }

func (part2SettlementBatchTool) Description() string {
	return "Return a settlement batch header, up to 50 member records, and up to 50 statement balances."
}

func (part2SettlementBatchTool) InputSchema() json.RawMessage {
	return json.RawMessage(part2SettlementBatchSchema)
}

type part2SettlementBatchData struct {
	ID                 string                        `json:"id"`
	Source             string                        `json:"source"`
	PeriodStart        *string                       `json:"period_start"`
	PeriodEnd          *string                       `json:"period_end"`
	DeclaredTotalMinor int64                         `json:"declared_total_minor"`
	Currency           string                        `json:"currency"`
	Status             string                        `json:"status"`
	QuarantineReason   *string                       `json:"quarantine_reason"`
	RowCount           int                           `json:"row_count"`
	DeclaredRowCount   int                           `json:"declared_row_count"`
	Records            []part2SettlementBatchRecord  `json:"records"`
	StatementBalances  []part2StatementBalanceRecord `json:"statement_balances"`
}

type part2SettlementBatchRecord struct {
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
	Raw             json.RawMessage `json:"raw"`
	UntrustedFields []string        `json:"untrusted_fields"`
}

type part2StatementBalanceRecord struct {
	ID           string `json:"id"`
	ScopeID      string `json:"scope_id"`
	AsOfDate     string `json:"as_of_date"`
	OpeningMinor int64  `json:"opening_minor"`
	ClosingMinor int64  `json:"closing_minor"`
	Currency     string `json:"currency"`
	BatchID      string `json:"batch_id"`
}

func (t part2SettlementBatchTool) Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error) {
	var request struct {
		BatchID string `json:"batch_id"`
	}
	if err := DecodeArgs(args, &request); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	batchID, err := ParseUUID(request.BatchID)
	if err != nil {
		return Result{}, err
	}
	// The v_agent_* views expose UUIDs as text; normalise before comparing so
	// an upper-case canonical UUID matches the lower-case stored id.
	batchID = strings.ToLower(batchID)
	queryer, err := part2RunQueryer(run)
	if err != nil {
		return Result{}, err
	}

	var (
		id               string
		source           string
		periodStart      *time.Time
		periodEnd        *time.Time
		declaredTotal    int64
		currency         string
		status           string
		quarantineReason *string
		rowCount         int
		declaredRows     int
	)
	err = queryer.QueryRow(ctx,
		`SELECT id, source, period_start, period_end, declared_total_minor,
		        currency, status, quarantine_reason, row_count, declared_row_count
		   FROM v_agent_settlement_batches
		  WHERE id = $1`, batchID).
		Scan(&id, &source, &periodStart, &periodEnd, &declaredTotal, &currency,
			&status, &quarantineReason, &rowCount, &declaredRows)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, NotFound("settlement batch not found")
	}
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_settlement_batch header: %w", err)
	}

	batchRef, err := BatchRef(id)
	if err != nil {
		return Result{}, err
	}
	refs := []string{batchRef}
	data := part2SettlementBatchData{
		ID:                 id,
		Source:             source,
		PeriodStart:        OptionalDate(periodStart),
		PeriodEnd:          OptionalDate(periodEnd),
		DeclaredTotalMinor: declaredTotal,
		Currency:           currency,
		Status:             status,
		QuarantineReason:   quarantineReason,
		RowCount:           rowCount,
		DeclaredRowCount:   declaredRows,
		Records:            []part2SettlementBatchRecord{},
		StatementBalances:  []part2StatementBalanceRecord{},
	}
	// A quarantined batch is header-only: its rows were rejected and must not
	// be surfaced as member evidence.
	if status == "quarantined" {
		return Result{Data: data, Refs: refs}, nil
	}

	rows, err := queryer.Query(ctx,
		`SELECT id, source, source_record_id, kind, amount_minor, currency,
		        fee_minor, occurred_at, settled_at, value_date, raw
		   FROM v_agent_external_records
		  WHERE batch_id = $1
		  ORDER BY source, source_record_id
		  LIMIT 51`, batchID)
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_settlement_batch records: %w", err)
	}
	records, err := CollectRows(rows, part2ScanSettlementRecord(t.deps.UntrustedFields))
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_settlement_batch records: %w", err)
	}
	truncated := false
	if len(records) > 50 {
		records = records[:50]
		truncated = true
	}
	for _, record := range records {
		ref, err := ExternalRef(record.Source, record.SourceRecordID)
		if err != nil {
			return Result{}, err
		}
		refs = append(refs, ref)
	}
	data.Records = records

	balanceRows, err := queryer.Query(ctx,
		`SELECT id, scope_id, as_of_date, opening_minor, closing_minor,
		        currency, batch_id
		   FROM v_agent_statement_balances
		  WHERE batch_id = $1
		  ORDER BY as_of_date, id
		  LIMIT 51`, batchID)
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_settlement_batch statement balances: %w", err)
	}
	balances, err := CollectRows(balanceRows, func(rows pgx.Rows) (part2StatementBalanceRecord, error) {
		var (
			record    part2StatementBalanceRecord
			asOf      time.Time
			batchText string
		)
		if err := rows.Scan(&record.ID, &record.ScopeID, &asOf,
			&record.OpeningMinor, &record.ClosingMinor, &record.Currency,
			&batchText); err != nil {
			return part2StatementBalanceRecord{}, err
		}
		record.AsOfDate = FormatDate(asOf)
		record.BatchID = batchText
		return record, nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_settlement_batch statement balances: %w", err)
	}
	if len(balances) > 50 {
		balances = balances[:50]
		truncated = true
	}
	data.StatementBalances = balances

	return Result{Data: data, Refs: refs, Truncated: truncated}, nil
}

func part2ScanSettlementRecord(configured map[string][]string) func(pgx.Rows) (part2SettlementBatchRecord, error) {
	return func(rows pgx.Rows) (part2SettlementBatchRecord, error) {
		var (
			record     part2SettlementBatchRecord
			occurredAt time.Time
			settledAt  *time.Time
			valueDate  time.Time
			raw        []byte
		)
		if err := rows.Scan(&record.ID, &record.Source, &record.SourceRecordID,
			&record.Kind, &record.AmountMinor, &record.Currency, &record.FeeMinor,
			&occurredAt, &settledAt, &valueDate, &raw); err != nil {
			return part2SettlementBatchRecord{}, err
		}
		record.OccurredAt = FormatTimestamp(occurredAt)
		record.SettledAt = OptionalTimestamp(settledAt)
		record.ValueDate = FormatDate(valueDate)
		if len(raw) == 0 {
			raw = []byte(`{}`)
		}
		// Keep the raw payload byte-for-byte JSON so large integers are not
		// round-tripped through float64; decode only to find untrusted keys.
		var rawValue any
		if err := DecodeJSONB(raw, &rawValue); err != nil {
			return part2SettlementBatchRecord{}, err
		}
		rawObject, _ := rawValue.(map[string]any)
		record.Raw = json.RawMessage(raw)
		record.UntrustedFields = ListUntrustedFields(record.Source, rawObject, configured)
		return record, nil
	}
}

// ---------------------------------------------------------------------------
// get_fx_rate
// ---------------------------------------------------------------------------

type part2FXRateTool struct{}

func (part2FXRateTool) Name() string { return "get_fx_rate" }

func (part2FXRateTool) Description() string {
	return "Return the latest FX rate on or before the end of the requested UTC date as an exact decimal string."
}

func (part2FXRateTool) InputSchema() json.RawMessage {
	return json.RawMessage(part2FXRateSchema)
}

type part2FXRateData struct {
	Base   string `json:"base"`
	Quote  string `json:"quote"`
	Rate   string `json:"rate"`
	AsOf   string `json:"as_of"`
	Source string `json:"source"`
}

func (part2FXRateTool) Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error) {
	var request struct {
		Base  string `json:"base"`
		Quote string `json:"quote"`
		Date  string `json:"date"`
	}
	if err := DecodeArgs(args, &request); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	base, err := ParseCurrency(request.Base)
	if err != nil {
		return Result{}, err
	}
	quote, err := ParseCurrency(request.Quote)
	if err != nil {
		return Result{}, err
	}
	if base.Code() == quote.Code() {
		return Result{}, fmt.Errorf("%w: base and quote must differ", ErrInvalidArguments)
	}
	date, err := ParseDate(request.Date)
	if err != nil {
		return Result{}, err
	}
	queryer, err := part2RunQueryer(run)
	if err != nil {
		return Result{}, err
	}

	// Q-D: latest row strictly before date+1 day; no lower bound.
	endOfDate := date.AddDate(0, 0, 1)
	var (
		rowBase  string
		rowQuote string
		rate     string
		asOf     time.Time
		source   string
	)
	err = queryer.QueryRow(ctx,
		`SELECT base, quote, rate, as_of, source
		   FROM v_agent_fx_rates
		  WHERE base = $1 AND quote = $2 AND as_of < $3::timestamptz
		  ORDER BY as_of DESC, id DESC
		  LIMIT 1`, base.Code(), quote.Code(), endOfDate).
		Scan(&rowBase, &rowQuote, &rate, &asOf, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, NotFound("fx rate not found")
	}
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_fx_rate: %w", err)
	}

	ref, err := FXRef(base.Code(), quote.Code(), request.Date)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Data: part2FXRateData{
			Base:   rowBase,
			Quote:  rowQuote,
			Rate:   rate,
			AsOf:   FormatTimestamp(asOf),
			Source: source,
		},
		Refs: []string{ref},
	}, nil
}

// ---------------------------------------------------------------------------
// get_fee_schedule
// ---------------------------------------------------------------------------

type part2FeeScheduleTool struct{}

func (part2FeeScheduleTool) Name() string { return "get_fee_schedule" }

func (part2FeeScheduleTool) Description() string {
	return "Return the fee schedule in force for a source, kind, currency and UTC date."
}

func (part2FeeScheduleTool) InputSchema() json.RawMessage {
	return json.RawMessage(part2FeeScheduleSchema)
}

type part2FeeScheduleData struct {
	ID         string  `json:"id"`
	Source     string  `json:"source"`
	Kind       string  `json:"kind"`
	Currency   string  `json:"currency"`
	PctBPS     int     `json:"pct_bps"`
	FixedMinor int64   `json:"fixed_minor"`
	ValidFrom  string  `json:"valid_from"`
	ValidTo    *string `json:"valid_to"`
}

func (part2FeeScheduleTool) Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error) {
	var request struct {
		Source   string `json:"source"`
		Kind     string `json:"kind"`
		Currency string `json:"currency"`
		Date     string `json:"date"`
	}
	if err := DecodeArgs(args, &request); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	if !sourcePattern.MatchString(request.Source) {
		return Result{}, wrapInvalidArguments("source %q is not a configured-source-shaped token", request.Source)
	}
	if TrimmedString(request.Kind) == "" {
		return Result{}, wrapInvalidArguments("kind is required")
	}
	currency, err := ParseCurrency(request.Currency)
	if err != nil {
		return Result{}, err
	}
	date, err := ParseDate(request.Date)
	if err != nil {
		return Result{}, err
	}
	queryer, err := part2RunQueryer(run)
	if err != nil {
		return Result{}, err
	}

	var (
		id          string
		source      string
		kind        string
		rowCurrency string
		pctBPS      int
		fixedMinor  int64
		validFrom   time.Time
		validTo     *time.Time
	)
	err = queryer.QueryRow(ctx,
		`SELECT id, source, kind, currency, pct_bps, fixed_minor,
		        valid_from, valid_to
		   FROM v_agent_fee_schedules
		  WHERE source = $1
		    AND kind = $2
		    AND currency = $3
		    AND valid_from <= $4::date
		    AND (valid_to IS NULL OR valid_to >= $4::date)
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, request.Source, request.Kind, currency.Code(), FormatDate(date)).
		Scan(&id, &source, &kind, &rowCurrency, &pctBPS, &fixedMinor, &validFrom, &validTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, NotFound("fee schedule not found")
	}
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_fee_schedule: %w", err)
	}

	ref, err := FeeScheduleRef(id)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Data: part2FeeScheduleData{
			ID:         id,
			Source:     source,
			Kind:       kind,
			Currency:   rowCurrency,
			PctBPS:     pctBPS,
			FixedMinor: fixedMinor,
			ValidFrom:  FormatDate(validFrom),
			ValidTo:    OptionalDate(validTo),
		},
		Refs: []string{ref},
	}, nil
}

// ---------------------------------------------------------------------------
// get_history
// ---------------------------------------------------------------------------

type part2HistoryTool struct{}

func (part2HistoryTool) Name() string { return "get_history" }

func (part2HistoryTool) Description() string {
	return "Return up to 20 resolved cases structurally similar to a break signature (same scope, side, currency and exact delta)."
}

func (part2HistoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(part2HistorySchema)
}

type part2HistoryMatch struct {
	ScopeID    string `json:"scope_id"`
	Side       string `json:"side"`
	Currency   string `json:"currency"`
	DeltaMinor int64  `json:"delta_minor"`
}

type part2HistoryCase struct {
	CaseID         string `json:"case_id"`
	ValueMinor     int64  `json:"value_minor"`
	Currency       string `json:"currency"`
	ResolutionType string `json:"resolution_type"`
	ResolvedAt     string `json:"resolved_at"`
	MatchingBreaks int    `json:"matching_breaks"`
}

type part2HistoryData struct {
	Signature string             `json:"signature"`
	Match     part2HistoryMatch  `json:"match"`
	Cases     []part2HistoryCase `json:"cases"`
}

func (part2HistoryTool) Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error) {
	var request struct {
		Signature string `json:"signature"`
	}
	if err := DecodeArgs(args, &request); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	if !part2HistorySignaturePattern.MatchString(request.Signature) {
		return Result{}, wrapInvalidArguments("signature must be 64 lowercase hex characters")
	}
	queryer, err := part2RunQueryer(run)
	if err != nil {
		return Result{}, err
	}

	var (
		breakID  string
		scopeID  string
		side     string
		currency string
		delta    int64
	)
	err = queryer.QueryRow(ctx,
		`SELECT id, scope_id, side, currency, delta_minor
		   FROM v_agent_breaks
		  WHERE signature = $1`, request.Signature).
		Scan(&breakID, &scopeID, &side, &currency, &delta)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, NotFound("break signature not found")
	}
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_history break: %w", err)
	}

	var currentCaseID string
	err = queryer.QueryRow(ctx,
		`SELECT case_id FROM v_agent_case_breaks WHERE break_id = $1`, breakID).
		Scan(&currentCaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		currentCaseID = ""
	} else if err != nil {
		return Result{}, fmt.Errorf("tools: get_history current case: %w", err)
	}

	var exclude any
	if currentCaseID != "" {
		exclude = currentCaseID
	}

	rows, err := queryer.Query(ctx,
		`SELECT case_id, case_value_minor, case_currency, resolution_type,
		        resolved_at, count(*) AS matching_breaks
		   FROM v_agent_history
		  WHERE scope_id = $1
		    AND side = $2
		    AND break_currency = $3
		    AND delta_minor = $4
		    AND ($5::text IS NULL OR case_id <> $5)
		  GROUP BY case_id, case_value_minor, case_currency, resolution_type, resolved_at
		  ORDER BY resolved_at DESC, case_id
		  LIMIT 21`,
		scopeID, side, currency, delta, exclude)
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_history cases: %w", err)
	}
	cases, err := CollectRows(rows, func(rows pgx.Rows) (part2HistoryCase, error) {
		var (
			record         part2HistoryCase
			resolutionType *string
			resolvedAt     time.Time
			matchingBreaks int64
		)
		if err := rows.Scan(&record.CaseID, &record.ValueMinor, &record.Currency,
			&resolutionType, &resolvedAt, &matchingBreaks); err != nil {
			return part2HistoryCase{}, err
		}
		record.ResolutionType = StringOrEmpty(resolutionType)
		record.ResolvedAt = FormatTimestamp(resolvedAt)
		record.MatchingBreaks = int(matchingBreaks)
		return record, nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("tools: get_history cases: %w", err)
	}
	truncated := false
	if len(cases) > 20 {
		cases = cases[:20]
		truncated = true
	}

	return Result{
		Data: part2HistoryData{
			Signature: request.Signature,
			Match: part2HistoryMatch{
				ScopeID:    scopeID,
				Side:       side,
				Currency:   currency,
				DeltaMinor: delta,
			},
			Cases: cases,
		},
		Truncated: truncated,
	}, nil
}
