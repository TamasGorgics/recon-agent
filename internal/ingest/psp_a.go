package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// pspColumns is the exact, ordered psp_a header.
var pspColumns = []string{
	"record_id",
	"type",
	"occurred_at",
	"settled_at",
	"gross",
	"fee",
	"net",
	"currency",
	"reference",
	"batch_id",
	"memo",
}

var (
	pspRequiredMetadata = []string{"rows", "total", "currency", "batch"}
	pspOptionalMetadata = []string{"period_start", "period_end"}
)

// PSPParser parses `psp_a` settlement files into canonical records. It is
// stateless; one parser value may be reused across files and goroutines.
type PSPParser struct{}

// NewPSPParser returns a parser for the psp_a format described in
// docs/formats.md.
func NewPSPParser() *PSPParser { return &PSPParser{} }

// Source returns the configured source name this parser implements.
func (p *PSPParser) Source() string { return SourcePSP }

// UntrustedFields returns the free-text columns kept in Record.Raw. The values
// must never be treated as instructions (spec section 9).
func (p *PSPParser) UntrustedFields() []string { return []string{UntrustedPSPMemo} }

// pspRaw is the JSON evidence payload for one psp_a row. It preserves the
// source strings, including the untrusted memo.
type pspRaw struct {
	RecordID   string `json:"record_id"`
	Type       string `json:"type"`
	OccurredAt string `json:"occurred_at"`
	SettledAt  string `json:"settled_at,omitempty"`
	Gross      string `json:"gross"`
	Fee        string `json:"fee"`
	Net        string `json:"net"`
	Currency   string `json:"currency"`
	Reference  string `json:"reference"`
	BatchID    string `json:"batch_id"`
	Memo       string `json:"memo"`
}

// Parse implements Parser. It rejects malformed envelopes, unknown record
// types, bad dates, currency mismatches, and inexact amounts with typed
// errors. Declared control totals are returned as-is; FIN-50 comparison is the
// pipeline's job.
func (p *PSPParser) Parse(r io.Reader) (ParseResult, error) {
	file, err := readSourceFile(r, pspColumns)
	if err != nil {
		return ParseResult{}, err
	}
	if err := checkMetadata(file.metadata, pspRequiredMetadata, pspOptionalMetadata); err != nil {
		return ParseResult{}, err
	}
	currency, err := file.declaredCurrency()
	if err != nil {
		return ParseResult{}, err
	}
	rowCount, err := file.intValue("rows")
	if err != nil {
		return ParseResult{}, err
	}
	total, err := file.amountValue("total", currency)
	if err != nil {
		return ParseResult{}, err
	}
	batch := file.metadata["batch"]

	periodStart, periodEnd, err := pspPeriod(file)
	if err != nil {
		return ParseResult{}, err
	}

	seen := make(map[string]struct{}, len(file.rows))
	records := make([]Record, 0, len(file.rows))
	for index, row := range file.rows {
		record, err := pspRecord(row, index+1, currency, batch)
		if err != nil {
			return ParseResult{}, err
		}
		if _, duplicate := seen[record.SourceRecordID]; duplicate {
			return ParseResult{}, fmt.Errorf("%w: data row %d: record_id %q",
				ErrDuplicateRecordID, index+1, record.SourceRecordID)
		}
		seen[record.SourceRecordID] = struct{}{}
		records = append(records, record)
	}

	return ParseResult{
		Records: records,
		Declared: Declared{
			TotalMinor: total.AmountMinor(),
			RowCount:   rowCount,
			Currency:   currency.Code(),
		},
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
	}, nil
}

// pspPeriod returns the optional declared period. Both dates must be present,
// valid, and ordered.
func pspPeriod(file *sourceFile) (time.Time, time.Time, error) {
	startRaw, hasStart := file.metadata["period_start"]
	endRaw, hasEnd := file.metadata["period_end"]
	if !hasStart && !hasEnd {
		return time.Time{}, time.Time{}, nil
	}
	if !hasStart || !hasEnd {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"%w: period_start and period_end must both be present", ErrBadMetadata)
	}
	start, err := file.dateValue("period_start")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := file.dateValue("period_end")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"%w: period_start %s is after period_end %s", ErrBadMetadata, startRaw, endRaw)
	}
	return start, end, nil
}

// pspRecord validates one psp_a data row and converts it to a canonical record.
// number is the 1-based data row number used in error context.
func pspRecord(row []string, number int, currency money.Currency, batch string) (Record, error) {
	fail := func(err error) (Record, error) {
		return Record{}, fmt.Errorf("data row %d: %w", number, err)
	}

	// record_id must be non-empty after trimming whitespace. A non-empty ID
	// with surrounding spaces is accepted and preserved verbatim.
	recordID := row[0]
	if strings.TrimSpace(recordID) == "" {
		return fail(fmt.Errorf("%w: record_id is empty or whitespace-only", ErrMalformedRow))
	}
	kind := Kind(row[1])
	switch kind {
	case KindCharge, KindRefund, KindFee, KindPayout, KindChargeback:
	default:
		return fail(fmt.Errorf("%w: %q is not charge, refund, fee, payout, or chargeback",
			ErrUnknownRecordType, row[1]))
	}

	occurredAt, err := rowTimestamp(row[2], "occurred_at")
	if err != nil {
		return fail(err)
	}
	var settledAt *time.Time
	if row[3] != "" {
		settled, err := rowTimestamp(row[3], "settled_at")
		if err != nil {
			return fail(err)
		}
		settledAt = &settled
	}

	gross, err := rowAmount(row[4], "gross", currency)
	if err != nil {
		return fail(err)
	}
	fee, err := rowAmount(row[5], "fee", currency)
	if err != nil {
		return fail(err)
	}
	net, err := rowAmount(row[6], "net", currency)
	if err != nil {
		return fail(err)
	}

	if row[7] != currency.Code() {
		return fail(fmt.Errorf("%w: row currency %q, file currency %q",
			ErrCurrencyMismatch, row[7], currency.Code()))
	}
	if row[9] != batch {
		return fail(fmt.Errorf("%w: batch_id %q, file batch %q",
			ErrMetadataMismatch, row[9], batch))
	}
	if fee.AmountMinor() < 0 {
		return fail(fmt.Errorf("%w: fee %q is negative", ErrInvalidSign, row[5]))
	}

	want, err := gross.Sub(fee)
	if err != nil {
		return fail(fmt.Errorf("%w: gross %q minus fee %q overflows", ErrAmountMismatch, row[4], row[5]))
	}
	if want.AmountMinor() != net.AmountMinor() {
		return fail(fmt.Errorf("%w: gross %q - fee %q = %s, but net is %q",
			ErrAmountMismatch, row[4], row[5], want.DecimalString(), row[6]))
	}

	if err := pspSign(kind, net, row[6]); err != nil {
		return fail(err)
	}

	raw, err := json.Marshal(pspRaw{
		RecordID:   row[0],
		Type:       row[1],
		OccurredAt: row[2],
		SettledAt:  row[3],
		Gross:      row[4],
		Fee:        row[5],
		Net:        row[6],
		Currency:   row[7],
		Reference:  row[8],
		BatchID:    row[9],
		Memo:       row[10],
	})
	if err != nil {
		return fail(fmt.Errorf("%w: encode raw payload: %v", ErrMalformedRow, err))
	}

	return Record{
		SourceRecordID: recordID,
		Kind:           kind,
		AmountMinor:    net.AmountMinor(),
		Currency:       currency.Code(),
		FeeMinor:       fee.AmountMinor(),
		OccurredAt:     occurredAt,
		SettledAt:      settledAt,
		Raw:            raw,
	}, nil
}

// pspSign enforces the documented sign convention: charges are credits
// (strictly positive) and every other psp_a type is a debit (strictly
// negative). Zero is never a valid event amount.
func pspSign(kind Kind, net money.Money, raw string) error {
	switch kind {
	case KindCharge:
		if net.AmountMinor() <= 0 {
			return fmt.Errorf("%w: charge net %q must be positive", ErrInvalidSign, raw)
		}
	default:
		if net.AmountMinor() >= 0 {
			return fmt.Errorf("%w: %s net %q must be negative", ErrInvalidSign, kind, raw)
		}
	}
	return nil
}
