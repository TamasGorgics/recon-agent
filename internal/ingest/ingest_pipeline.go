package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// preparedRecord is a canonical record after contract validation and value
// date derivation. It is immutable for the duration of the transaction.
type preparedRecord struct {
	record    Record
	valueDate time.Time
	raw       []byte
	rawHash   string
}

// preparedFile is one parser result after local validation. controlErr is a
// FIN-50 quarantine reason; structural parser-contract violations are returned
// as ErrInvalidRecord instead and never create a batch row.
type preparedFile struct {
	records     []preparedRecord
	declared    Declared
	statements  []Statement
	periodStart *time.Time
	periodEnd   *time.Time
	totalMinor  int64
	controlErr  string
}

// Ingest runs one file through the pipeline:
//
//	validate request -> parse -> validate/derive -> [tx: file-hash no-op,
//	FIN-50 control totals, scope/currency checks, FIN-55 continuity,
//	all-or-nothing insert or quarantine + audit].
//
// A control-total, row-count, currency, scope, restatement, or continuity
// failure is a quarantined batch (Result.StatusQuarantined), not an error. A
// parse failure or a parser-contract violation happens before any batch row
// exists and is returned as ErrParse/ErrInvalidRecord.
func (i *Ingester) Ingest(ctx context.Context, req Request) (Result, error) {
	if strings.TrimSpace(req.Source) == "" {
		return Result{}, fmt.Errorf("%w: source is required", ErrInvalidRequest)
	}
	if req.Parser == nil {
		return Result{}, fmt.Errorf("%w: parser is required", ErrInvalidRequest)
	}
	calendar, ok := i.sources.Calendar(req.Source)
	if !ok {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownSource, req.Source)
	}

	parsed, err := req.Parser.Parse(bytes.NewReader(req.Data))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s: %w", ErrParse, fileNameOrPlaceholder(req.FileName), err)
	}
	prepared, err := prepareFile(parsed, calendar)
	if err != nil {
		return Result{}, err
	}

	fileHash := sha256Hex(req.Data)
	var result Result
	err = db.WithTx(ctx, i.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		res, err := i.ingestTx(ctx, tx, req, fileHash, prepared)
		if err != nil {
			return err
		}
		result = res
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("ingest: %s: %w", fileNameOrPlaceholder(req.FileName), err)
	}
	return result, nil
}

func (i *Ingester) ingestTx(ctx context.Context, tx pgx.Tx, req Request, fileHash string, prepared preparedFile) (Result, error) {
	// Serialize same-source ingestion so the duplicate-record pre-count and
	// the FIN-55 continuity reads cannot race another ingest of this source.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "ingest:"+req.Source); err != nil {
		return Result{}, fmt.Errorf("acquire source ingestion lock: %w", err)
	}

	// FIN-51: identical file bytes are a no-op regardless of the first
	// batch's final status. No new rows and no audit entry.
	var existingID, existingStatus string
	err := tx.QueryRow(ctx,
		`SELECT id::text, status FROM settlement_batches WHERE file_hash = $1`, fileHash).
		Scan(&existingID, &existingStatus)
	switch {
	case err == nil:
		return Result{BatchID: existingID, Status: Status(existingStatus), NoOp: true}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Result{}, fmt.Errorf("read existing batch by file hash: %w", err)
	}

	// A quarantined batch still carries the declared currency, so the code
	// must exist before any quarantine row can persist (FK to currencies).
	var currencyExists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM currencies WHERE code = $1)`, prepared.declared.Currency).
		Scan(&currencyExists); err != nil {
		return Result{}, fmt.Errorf("check declared currency: %w", err)
	}
	if !currencyExists {
		return Result{}, fmt.Errorf("%w: declared currency %q is not configured", ErrInvalidRecord, prepared.declared.Currency)
	}

	reason := prepared.controlErr
	var scopeID string
	if reason == "" {
		err = tx.QueryRow(ctx,
			`SELECT id::text FROM reconciliation_scopes WHERE source = $1 AND currency = $2`,
			req.Source, prepared.declared.Currency).
			Scan(&scopeID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			reason = fmt.Sprintf("unknown reconciliation scope for source %q and currency %q",
				req.Source, prepared.declared.Currency)
		case err != nil:
			return Result{}, fmt.Errorf("read reconciliation scope: %w", err)
		}
	}
	if reason == "" {
		reason = recordCurrencyMismatch(prepared)
	}
	if reason == "" {
		reason = statementCurrencyMismatch(prepared)
	}
	if reason == "" {
		reason, err = continuityReason(ctx, tx, scopeID, prepared.statements)
		if err != nil {
			return Result{}, err
		}
	}

	if reason != "" {
		return i.persistQuarantine(ctx, tx, req, fileHash, prepared, reason)
	}

	inserted, skipIDs, err := anticipatedInserted(ctx, tx, req.Source, prepared.records)
	if err != nil {
		return Result{}, err
	}
	batchID, noOp, status, err := insertBatch(ctx, tx, req, fileHash, prepared, "", inserted)
	if err != nil {
		return Result{}, err
	}
	if noOp {
		return Result{BatchID: batchID, Status: status, NoOp: true}, nil
	}

	actual, err := insertRecords(ctx, tx, req.Source, batchID, prepared.records)
	if err != nil {
		return Result{}, err
	}
	if actual != inserted {
		return Result{}, fmt.Errorf("inserted %d records but anticipated %d; duplicate constraint raced", actual, inserted)
	}
	if err := insertStatements(ctx, tx, scopeID, batchID, prepared.statements); err != nil {
		return Result{}, err
	}

	skipped := len(prepared.records) - inserted
	after := map[string]any{
		"source":               req.Source,
		"file_name":            req.FileName,
		"file_hash":            fileHash,
		"declared_total_minor": prepared.declared.TotalMinor,
		"declared_row_count":   prepared.declared.RowCount,
		"parsed_row_count":     len(prepared.records),
		"inserted_row_count":   inserted,
		"skipped_row_count":    skipped,
		"statement_count":      len(prepared.statements),
	}
	if skipped > 0 {
		after["skip_reason"] = skippedSkipReason(skipIDs)
	}
	if err := i.appendAudit(ctx, tx, ActionIngest, batchID, after); err != nil {
		return Result{}, err
	}
	return Result{
		BatchID:  batchID,
		Status:   StatusIngested,
		Parsed:   len(prepared.records),
		Inserted: inserted,
		Skipped:  skipped,
	}, nil
}

// persistQuarantine writes the quarantined batch plus its single audit entry.
// No external_records or statement_balances row is ever written for it.
func (i *Ingester) persistQuarantine(ctx context.Context, tx pgx.Tx, req Request, fileHash string, prepared preparedFile, reason string) (Result, error) {
	batchID, noOp, status, err := insertBatch(ctx, tx, req, fileHash, prepared, reason, 0)
	if err != nil {
		return Result{}, err
	}
	if noOp {
		return Result{BatchID: batchID, Status: status, NoOp: true}, nil
	}
	after := map[string]any{
		"source":               req.Source,
		"file_name":            req.FileName,
		"file_hash":            fileHash,
		"reason":               reason,
		"declared_total_minor": prepared.declared.TotalMinor,
		"declared_row_count":   prepared.declared.RowCount,
		"parsed_row_count":     len(prepared.records),
	}
	if err := i.appendAudit(ctx, tx, ActionQuarantine, batchID, after); err != nil {
		return Result{}, err
	}
	return Result{
		BatchID:          batchID,
		Status:           StatusQuarantined,
		Parsed:           len(prepared.records),
		QuarantineReason: reason,
	}, nil
}

func (i *Ingester) appendAudit(ctx context.Context, tx pgx.Tx, action, batchID string, after map[string]any) error {
	_, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      i.actor,
		Action:     action,
		EntityType: EntityBatch,
		EntityID:   batchID,
		After:      after,
		Timestamp:  i.clock.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	return nil
}

// insertBatch inserts the final batch row. It returns noOp=true when another
// transaction won the file_hash race. rowCount is the number of external
// records this batch will insert (always zero for quarantine).
func insertBatch(ctx context.Context, tx pgx.Tx, req Request, fileHash string, prepared preparedFile, quarantineReason string, rowCount int) (batchID string, noOp bool, status Status, err error) {
	status = StatusIngested
	var reasonArg any
	if quarantineReason != "" {
		status = StatusQuarantined
		reasonArg = quarantineReason
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO settlement_batches
		     (source, period_start, period_end, declared_total_minor, currency,
		      file_hash, status, quarantine_reason, row_count, declared_row_count)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (file_hash) DO NOTHING
		 RETURNING id::text`,
		req.Source, prepared.periodStart, prepared.periodEnd, prepared.declared.TotalMinor,
		prepared.declared.Currency, fileHash, string(status), reasonArg, rowCount,
		prepared.declared.RowCount).
		Scan(&batchID)
	if errors.Is(err, pgx.ErrNoRows) {
		var existing string
		if readErr := tx.QueryRow(ctx,
			`SELECT id::text, status FROM settlement_batches WHERE file_hash = $1`, fileHash).
			Scan(&batchID, &existing); readErr != nil {
			return "", false, "", fmt.Errorf("read batch after file-hash conflict: %w", readErr)
		}
		return batchID, true, Status(existing), nil
	}
	if err != nil {
		return "", false, "", fmt.Errorf("insert settlement batch: %w", err)
	}
	return batchID, false, status, nil
}

// anticipatedInserted returns how many of the file's unique record IDs are new
// for this source. It locks the source so no other ingest can insert between
// this count and the record inserts.
func anticipatedInserted(ctx context.Context, tx pgx.Tx, source string, records []preparedRecord) (int, map[string]struct{}, error) {
	ids := make([]string, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, rec := range records {
		id := rec.record.SourceRecordID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	existing := make(map[string]struct{}, len(ids))
	if len(ids) > 0 {
		rows, err := tx.Query(ctx,
			`SELECT source_record_id
			   FROM external_records
			  WHERE source = $1 AND source_record_id = ANY($2::text[])`,
			source, ids)
		if err != nil {
			return 0, nil, fmt.Errorf("read existing external records: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, nil, fmt.Errorf("scan existing external record: %w", err)
			}
			existing[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("read existing external records: %w", err)
		}
		rows.Close()
	}
	return len(ids) - len(existing), existing, nil
}

// insertRecords inserts the file's records in file order. ON CONFLICT keeps
// FIN-51 dedupe safe even if the pre-count raced; the caller verifies that the
// actual count still matches what the batch row recorded.
func insertRecords(ctx context.Context, tx pgx.Tx, source, batchID string, records []preparedRecord) (int, error) {
	inserted := 0
	for _, rec := range records {
		var settledAt any
		if rec.record.SettledAt != nil {
			settledAt = rec.record.SettledAt.UTC()
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO external_records
			     (source, source_record_id, kind, amount_minor, currency, fee_minor,
			      occurred_at, settled_at, value_date, batch_id, raw, raw_hash)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::uuid, $11::jsonb, $12)
			 ON CONFLICT (source, source_record_id) DO NOTHING`,
			source, rec.record.SourceRecordID, string(rec.record.Kind),
			rec.record.AmountMinor, rec.record.Currency, rec.record.FeeMinor,
			rec.record.OccurredAt.UTC(), settledAt, rec.valueDate, batchID,
			string(rec.raw), rec.rawHash)
		if err != nil {
			return 0, fmt.Errorf("insert external record %q: %w", rec.record.SourceRecordID, err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

func insertStatements(ctx context.Context, tx pgx.Tx, scopeID, batchID string, statements []Statement) error {
	for _, statement := range statements {
		if _, err := tx.Exec(ctx,
			`INSERT INTO statement_balances
			     (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
			 VALUES ($1::uuid, $2, $3, $4, $5, $6::uuid)`,
			scopeID, DateOnly(statement.AsOfDate), statement.OpeningMinor,
			statement.ClosingMinor, statement.Currency, batchID); err != nil {
			return fmt.Errorf("insert statement balance %s: %w",
				statement.AsOfDate.Format("2006-01-02"), err)
		}
	}
	return nil
}

// balanceNode is one statement in the FIN-55 chain, either already stored or
// arriving in this file.
type balanceNode struct {
	asOf     time.Time
	opening  int64
	closing  int64
	incoming bool
}

// continuityReason merges the stored and incoming statements by date and
// checks every adjacent pair. New rows may be backfills, so both the previous
// closing and the next opening must line up. Duplicate dates are restatements.
func continuityReason(ctx context.Context, tx pgx.Tx, scopeID string, statements []Statement) (string, error) {
	rows, err := tx.Query(ctx,
		`SELECT as_of_date, opening_minor, closing_minor
		   FROM statement_balances
		  WHERE scope_id = $1::uuid
		  ORDER BY as_of_date`, scopeID)
	if err != nil {
		return "", fmt.Errorf("read statement balances: %w", err)
	}
	nodes := make([]balanceNode, 0, len(statements))
	for rows.Next() {
		var node balanceNode
		if err := rows.Scan(&node.asOf, &node.opening, &node.closing); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan statement balance: %w", err)
		}
		node.asOf = DateOnly(node.asOf)
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("read statement balances: %w", err)
	}
	rows.Close()

	for _, statement := range statements {
		nodes = append(nodes, balanceNode{
			asOf:     DateOnly(statement.AsOfDate),
			opening:  statement.OpeningMinor,
			closing:  statement.ClosingMinor,
			incoming: true,
		})
	}
	sort.SliceStable(nodes, func(a, b int) bool { return nodes[a].asOf.Before(nodes[b].asOf) })

	for idx := 1; idx < len(nodes); idx++ {
		prev, current := nodes[idx-1], nodes[idx]
		if prev.asOf.Equal(current.asOf) {
			if prev.incoming || current.incoming {
				return fmt.Sprintf("restatement: statement balance for %s already exists",
					current.asOf.Format("2006-01-02")), nil
			}
			return "", fmt.Errorf("duplicate stored statement balances for %s",
				current.asOf.Format("2006-01-02"))
		}
		if prev.closing != current.opening {
			return fmt.Sprintf(
				"continuity: statement %s opens at %d but the previous closing balance is %d",
				current.asOf.Format("2006-01-02"), current.opening, prev.closing), nil
		}
	}
	return "", nil
}

func recordCurrencyMismatch(prepared preparedFile) string {
	for _, rec := range prepared.records {
		if rec.record.Currency != prepared.declared.Currency {
			return fmt.Sprintf("currency mismatch: record %q is %s but the batch is %s",
				rec.record.SourceRecordID, rec.record.Currency, prepared.declared.Currency)
		}
	}
	return ""
}

func statementCurrencyMismatch(prepared preparedFile) string {
	for _, statement := range prepared.statements {
		if statement.Currency != prepared.declared.Currency {
			return fmt.Sprintf("currency mismatch: statement %s is %s but the batch is %s",
				statement.AsOfDate.Format("2006-01-02"), statement.Currency, prepared.declared.Currency)
		}
	}
	return ""
}

func skippedSkipReason(skipIDs map[string]struct{}) string {
	if len(skipIDs) == 0 {
		return "duplicate (source, source_record_id) record skipped (FIN-51)"
	}
	return "duplicate (source, source_record_id) record already present from an earlier file (FIN-51)"
}

// prepareFile validates the parser contract, derives value dates, and computes
// the FIN-50 control verdict. Structural violations return ErrInvalidRecord
// (nothing is persisted); a total/count mismatch becomes controlErr so the
// caller can persist a quarantined batch.
func prepareFile(parsed ParseResult, calendar Calendar) (preparedFile, error) {
	prepared := preparedFile{
		declared: parsed.Declared,
	}
	if strings.TrimSpace(prepared.declared.Currency) == "" {
		return preparedFile{}, fmt.Errorf("%w: declared currency is empty", ErrInvalidRecord)
	}
	if prepared.declared.RowCount < 0 {
		return preparedFile{}, fmt.Errorf("%w: declared row count %d is negative",
			ErrInvalidRecord, prepared.declared.RowCount)
	}
	if parsed.PeriodStart.IsZero() != parsed.PeriodEnd.IsZero() {
		return preparedFile{}, fmt.Errorf("%w: period_start and period_end must both be set or both be zero",
			ErrInvalidRecord)
	}

	prepared.records = make([]preparedRecord, len(parsed.Records))
	for idx, record := range parsed.Records {
		preparedRecord, err := prepareRecord(record, calendar)
		if err != nil {
			return preparedFile{}, err
		}
		prepared.records[idx] = preparedRecord
	}

	prepared.statements = make([]Statement, len(parsed.Statements))
	for idx, statement := range parsed.Statements {
		if statement.AsOfDate.IsZero() {
			return preparedFile{}, fmt.Errorf("%w: statement %d has no as-of date", ErrInvalidRecord, idx)
		}
		if strings.TrimSpace(statement.Currency) == "" {
			return preparedFile{}, fmt.Errorf("%w: statement %s has no currency",
				ErrInvalidRecord, statement.AsOfDate.Format("2006-01-02"))
		}
		statement.AsOfDate = DateOnly(statement.AsOfDate)
		prepared.statements[idx] = statement
	}
	sort.SliceStable(prepared.statements, func(a, b int) bool {
		return prepared.statements[a].AsOfDate.Before(prepared.statements[b].AsOfDate)
	})

	prepared.periodStart, prepared.periodEnd = periodBounds(parsed, prepared)
	if prepared.periodStart != nil && prepared.periodEnd != nil && prepared.periodEnd.Before(*prepared.periodStart) {
		return preparedFile{}, fmt.Errorf("%w: period_end is before period_start", ErrInvalidRecord)
	}

	for _, rec := range prepared.records {
		sum, overflow := addInt64(prepared.totalMinor, rec.record.AmountMinor)
		if overflow {
			prepared.controlErr = "control total overflow: the record sum does not fit in int64"
			break
		}
		prepared.totalMinor = sum
	}
	if prepared.controlErr == "" {
		if len(prepared.records) != prepared.declared.RowCount {
			prepared.controlErr = fmt.Sprintf(
				"row count mismatch: parser returned %d records but the trailer declared %d",
				len(prepared.records), prepared.declared.RowCount)
		} else if prepared.totalMinor != prepared.declared.TotalMinor {
			prepared.controlErr = fmt.Sprintf(
				"control total mismatch: parsed sum is %d but the trailer declared %d",
				prepared.totalMinor, prepared.declared.TotalMinor)
		}
	}
	return prepared, nil
}

func prepareRecord(record Record, calendar Calendar) (preparedRecord, error) {
	if strings.TrimSpace(record.SourceRecordID) == "" {
		return preparedRecord{}, fmt.Errorf("%w: source record id is empty", ErrInvalidRecord)
	}
	if !record.Kind.Valid() {
		return preparedRecord{}, fmt.Errorf("%w: record %q has unknown kind %q",
			ErrInvalidRecord, record.SourceRecordID, record.Kind)
	}
	if strings.TrimSpace(record.Currency) == "" {
		return preparedRecord{}, fmt.Errorf("%w: record %q has no currency",
			ErrInvalidRecord, record.SourceRecordID)
	}
	if record.OccurredAt.IsZero() {
		return preparedRecord{}, fmt.Errorf("%w: record %q has no occurred_at",
			ErrInvalidRecord, record.SourceRecordID)
	}
	if record.FeeMinor < 0 {
		return preparedRecord{}, fmt.Errorf("%w: record %q has negative fee_minor %d",
			ErrInvalidRecord, record.SourceRecordID, record.FeeMinor)
	}

	raw := record.Raw
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if !json.Valid(raw) {
		return preparedRecord{}, fmt.Errorf("%w: record %q raw payload is not valid JSON",
			ErrInvalidRecord, record.SourceRecordID)
	}
	rawHash := record.RawHash
	if rawHash == "" {
		rawHash = sha256Hex(raw)
	} else if !isSHA256Hex(rawHash) {
		return preparedRecord{}, fmt.Errorf("%w: record %q raw_hash is not a SHA-256 hex digest",
			ErrInvalidRecord, record.SourceRecordID)
	}

	valueDate := record.ValueDate
	if valueDate.IsZero() {
		valueDate = calendar.ValueDate(record.OccurredAt)
	} else {
		valueDate = DateOnly(valueDate)
	}
	return preparedRecord{record: record, valueDate: valueDate, raw: raw, rawHash: rawHash}, nil
}

// periodBounds uses the parser's declared period when present, otherwise the
// min/max of record value dates and statement dates.
func periodBounds(parsed ParseResult, prepared preparedFile) (start, end *time.Time) {
	if !parsed.PeriodStart.IsZero() {
		startValue := DateOnly(parsed.PeriodStart)
		endValue := DateOnly(parsed.PeriodEnd)
		return &startValue, &endValue
	}
	var min, max time.Time
	consider := func(value time.Time) {
		if min.IsZero() || value.Before(min) {
			min = value
		}
		if max.IsZero() || value.After(max) {
			max = value
		}
	}
	for _, rec := range prepared.records {
		consider(rec.valueDate)
	}
	for _, statement := range prepared.statements {
		consider(statement.AsOfDate)
	}
	if min.IsZero() {
		return nil, nil
	}
	return &min, &max
}

func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, true
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, true
	}
	return a + b, false
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func fileNameOrPlaceholder(name string) string {
	if strings.TrimSpace(name) == "" {
		return "<unnamed file>"
	}
	return name
}
