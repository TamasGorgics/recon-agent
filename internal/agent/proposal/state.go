package proposal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// CaseState is the projected case state. ResolutionType is empty while the
// case is unresolved.
type CaseState struct {
	ID             string
	Status         string
	ResolutionType string
}

// BreakState is the projected state of one break attached to the case.
// LedgerTxnIDs and ExternalRefs are sorted canonical values.
type BreakState struct {
	ID           string
	Signature    string
	Side         string
	Status       string
	DeltaMinor   int64
	Currency     string
	LedgerTxnIDs []string
	ExternalRefs []string
}

// RecordState is the projection of one cited evidence record. Fields holds
// only the canonical projection fields for the ref's kind; narrative text and
// wall-clock fields are excluded.
type RecordState struct {
	Ref    string
	Kind   string
	Fields map[string]any
}

// State is the canonical snapshot hashed onto a proposal. M4-06 reuses this
// shape and the Canonical/Hash functions.
type State struct {
	Case    CaseState
	Breaks  []BreakState
	Records []RecordState
}

// ResolveState loads the case, its attached breaks, and the projection of
// every evidence ref through the agent read surface. Unknown or unresolvable
// refs return an error so the caller can reject the proposal before insert.
func ResolveState(ctx context.Context, q tools.Queryer, caseID string, evidence []string) (State, error) {
	if q == nil {
		return State{}, errors.New("proposal: resolve state: no queryer")
	}
	normalized, err := tools.ParseUUID(caseID)
	if err != nil {
		return State{}, fmt.Errorf("proposal: resolve state case id: %w", err)
	}
	normalized = strings.ToLower(normalized)

	caseState, err := resolveCase(ctx, q, normalized)
	if err != nil {
		return State{}, err
	}
	breaks, err := resolveBreaks(ctx, q, normalized)
	if err != nil {
		return State{}, err
	}
	refs := tools.SortedUnique(evidence)
	records := make([]RecordState, 0, len(refs))
	for _, ref := range refs {
		if err := tools.ValidateRef(ref); err != nil {
			return State{}, fmt.Errorf("%w: %v", ErrInvalidEvidence, err)
		}
		record, err := resolveRecord(ctx, q, ref)
		if err != nil {
			return State{}, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Ref < records[j].Ref })

	return State{Case: caseState, Breaks: breaks, Records: records}, nil
}

func resolveCase(ctx context.Context, q tools.Queryer, caseID string) (CaseState, error) {
	var (
		state          CaseState
		resolutionType *string
	)
	err := q.QueryRow(ctx,
		`SELECT id, status, resolution_type FROM v_agent_cases WHERE id = $1`,
		caseID).Scan(&state.ID, &state.Status, &resolutionType)
	if err != nil {
		return CaseState{}, fmt.Errorf("proposal: resolve case %s: %w", caseID, err)
	}
	state.ResolutionType = tools.StringOrEmpty(resolutionType)
	return state, nil
}

type storedBreakExternalRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

func resolveBreaks(ctx context.Context, q tools.Queryer, caseID string) ([]BreakState, error) {
	rows, err := q.Query(ctx,
		`SELECT b.id, b.signature, b.side, b.status, b.delta_minor, b.currency,
		        b.ledger_txn_ids, b.external_refs
		   FROM v_agent_case_breaks cb
		   JOIN v_agent_breaks b ON b.id = cb.break_id
		  WHERE cb.case_id = $1
		  ORDER BY b.id`,
		caseID)
	if err != nil {
		return nil, fmt.Errorf("proposal: resolve breaks: %w", err)
	}
	breaks, err := tools.CollectRows(rows, func(row pgx.Rows) (BreakState, error) {
		var (
			state    BreakState
			external []byte
		)
		if err := row.Scan(&state.ID, &state.Signature, &state.Side, &state.Status,
			&state.DeltaMinor, &state.Currency, &state.LedgerTxnIDs, &external); err != nil {
			return BreakState{}, err
		}
		converted, err := canonicalBreakExternalRefs(external)
		if err != nil {
			return BreakState{}, err
		}
		state.ExternalRefs = converted
		state.LedgerTxnIDs = tools.SortedUnique(state.LedgerTxnIDs)
		return state, nil
	})
	if err != nil {
		return nil, fmt.Errorf("proposal: resolve breaks: %w", err)
	}
	sort.Slice(breaks, func(i, j int) bool { return breaks[i].ID < breaks[j].ID })
	return breaks, nil
}

func canonicalBreakExternalRefs(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var stored []storedBreakExternalRef
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("proposal: decode break external_refs: %w", err)
	}
	refs := make([]string, 0, len(stored))
	for _, item := range stored {
		ref, err := tools.ExternalRef(item.Source, item.SourceRecordID)
		if err != nil {
			return nil, fmt.Errorf("proposal: break external ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return tools.SortedUnique(refs), nil
}

func resolveRecord(ctx context.Context, q tools.Queryer, ref string) (RecordState, error) {
	switch {
	case strings.HasPrefix(ref, "case:"):
		return resolveCaseRecord(ctx, q, ref)
	case strings.HasPrefix(ref, "ledger_txn:"):
		return resolveLedgerTxnRecord(ctx, q, ref)
	case strings.HasPrefix(ref, "ext:"):
		return resolveExternalRecord(ctx, q, ref)
	case strings.HasPrefix(ref, "batch:"):
		return resolveBatchRecord(ctx, q, ref)
	case strings.HasPrefix(ref, "fx:"):
		return resolveFXRecord(ctx, q, ref)
	case strings.HasPrefix(ref, "fee_schedule:"):
		return resolveFeeScheduleRecord(ctx, q, ref)
	default:
		return RecordState{}, fmt.Errorf("%w: unknown ref prefix", ErrInvalidEvidence)
	}
}

func resolveCaseRecord(ctx context.Context, q tools.Queryer, ref string) (RecordState, error) {
	var (
		id, status     string
		resolutionType *string
	)
	err := q.QueryRow(ctx,
		`SELECT id, status, resolution_type FROM v_agent_cases WHERE id = $1`,
		strings.TrimPrefix(ref, "case:")).Scan(&id, &status, &resolutionType)
	if err != nil {
		return RecordState{}, fmt.Errorf("proposal: resolve %s: %w", ref, err)
	}
	return RecordState{Ref: ref, Kind: "case", Fields: map[string]any{
		"id":              id,
		"status":          status,
		"resolution_type": tools.StringOrEmpty(resolutionType),
	}}, nil
}

func resolveLedgerTxnRecord(ctx context.Context, q tools.Queryer, ref string) (RecordState, error) {
	txnID := strings.TrimPrefix(ref, "ledger_txn:")
	var (
		kind      string
		valueDate time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT kind, value_date FROM v_agent_ledger_txns WHERE id = $1`, txnID).
		Scan(&kind, &valueDate)
	if err != nil {
		return RecordState{}, fmt.Errorf("proposal: resolve %s: %w", ref, err)
	}
	rows, err := q.Query(ctx,
		`SELECT id, account_code, direction, amount_minor, currency
		   FROM v_agent_ledger_entries
		  WHERE txn_id = $1
		  ORDER BY id`, txnID)
	if err != nil {
		return RecordState{}, fmt.Errorf("proposal: resolve %s entries: %w", ref, err)
	}
	entries, err := tools.CollectRows(rows, func(row pgx.Rows) (map[string]any, error) {
		var (
			entryID, accountCode, direction, currency string
			amountMinor                               int64
		)
		if err := row.Scan(&entryID, &accountCode, &direction, &amountMinor, &currency); err != nil {
			return nil, err
		}
		return map[string]any{
			"account_code": accountCode,
			"direction":    direction,
			"amount_minor": amountMinor,
			"currency":     currency,
		}, nil
	})
	if err != nil {
		return RecordState{}, fmt.Errorf("proposal: resolve %s entries: %w", ref, err)
	}
	return RecordState{Ref: ref, Kind: "ledger_txn", Fields: map[string]any{
		"kind":       kind,
		"value_date": tools.FormatDate(valueDate),
		"entries":    entries,
	}}, nil
}

func resolveExternalRecord(ctx context.Context, q tools.Queryer, ref string) (RecordState, error) {
	rest := strings.TrimPrefix(ref, "ext:")
	source, recordID, _ := strings.Cut(rest, ":")
	var (
		kind      string
		amount    int64
		currency  string
		feeMinor  int64
		valueDate time.Time
		rawHash   string
		actualID  string
		actualSrc string
		actualRec string
	)
	err := q.QueryRow(ctx,
		`SELECT id, source, source_record_id, kind, amount_minor, currency,
		        fee_minor, value_date, raw_hash
		   FROM v_agent_external_records
		  WHERE source = $1 AND source_record_id = $2`,
		source, recordID).
		Scan(&actualID, &actualSrc, &actualRec, &kind, &amount, &currency,
			&feeMinor, &valueDate, &rawHash)
	if err != nil {
		return RecordState{}, fmt.Errorf("proposal: resolve %s: %w", ref, err)
	}
	return RecordState{Ref: ref, Kind: "ext", Fields: map[string]any{
		"source":           actualSrc,
		"source_record_id": actualRec,
		"kind":             kind,
		"amount_minor":     amount,
		"currency":         currency,
		"fee_minor":        feeMinor,
		"value_date":       tools.FormatDate(valueDate),
		"raw_hash":         rawHash,
	}}, nil
}

func resolveBatchRecord(ctx context.Context, q tools.Queryer, ref string) (RecordState, error) {
	batchID := strings.TrimPrefix(ref, "batch:")
	var (
		id, source, currency, status string
		declaredTotal                int64
		rowCount, declaredRows       int
	)
	err := q.QueryRow(ctx,
		`SELECT id, source, declared_total_minor, currency, status, row_count,
		        declared_row_count
		   FROM v_agent_settlement_batches
		  WHERE id = $1`, batchID).
		Scan(&id, &source, &declaredTotal, &currency, &status, &rowCount, &declaredRows)
	if err != nil {
		return RecordState{}, fmt.Errorf("proposal: resolve %s: %w", ref, err)
	}
	return RecordState{Ref: ref, Kind: "batch", Fields: map[string]any{
		"id":                   id,
		"source":               source,
		"declared_total_minor": declaredTotal,
		"currency":             currency,
		"status":               status,
		"row_count":            rowCount,
		"declared_row_count":   declaredRows,
	}}, nil
}

func resolveFXRecord(ctx context.Context, q tools.Queryer, ref string) (RecordState, error) {
	rest := strings.TrimPrefix(ref, "fx:")
	pair, date, _ := strings.Cut(rest, "@")
	base, quote, _ := strings.Cut(pair, "/")
	if _, err := tools.ParseDate(date); err != nil {
		return RecordState{}, fmt.Errorf("%w: fx ref date: %v", ErrInvalidEvidence, err)
	}
	var (
		rate string
		asOf time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT base, quote, rate, as_of
		   FROM v_agent_fx_rates
		  WHERE base = $1 AND quote = $2
		    AND as_of < ($3::date + interval '1 day')
		  ORDER BY as_of DESC, id DESC
		  LIMIT 1`, base, quote, date).
		Scan(&base, &quote, &rate, &asOf)
	if err != nil {
		return RecordState{}, fmt.Errorf("proposal: resolve %s: %w", ref, err)
	}
	return RecordState{Ref: ref, Kind: "fx", Fields: map[string]any{
		"base":  base,
		"quote": quote,
		"date":  date,
		"rate":  rate,
		"as_of": tools.FormatTimestamp(asOf),
	}}, nil
}

func resolveFeeScheduleRecord(ctx context.Context, q tools.Queryer, ref string) (RecordState, error) {
	scheduleID := strings.TrimPrefix(ref, "fee_schedule:")
	var (
		id, source, kind, currency string
		pctBPS, fixedMinor         int
		validFrom                  time.Time
		validTo                    *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT id, source, kind, currency, pct_bps, fixed_minor, valid_from, valid_to
		   FROM v_agent_fee_schedules
		  WHERE id = $1`, scheduleID).
		Scan(&id, &source, &kind, &currency, &pctBPS, &fixedMinor, &validFrom, &validTo)
	if err != nil {
		return RecordState{}, fmt.Errorf("proposal: resolve %s: %w", ref, err)
	}
	fields := map[string]any{
		"id":          id,
		"source":      source,
		"kind":        kind,
		"currency":    currency,
		"pct_bps":     pctBPS,
		"fixed_minor": fixedMinor,
		"valid_from":  tools.FormatDate(validFrom),
	}
	if validTo != nil {
		fields["valid_to"] = tools.FormatDate(*validTo)
	}
	return RecordState{Ref: ref, Kind: "fee_schedule", Fields: fields}, nil
}
