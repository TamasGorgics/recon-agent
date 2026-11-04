package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// bankColumns is the exact, ordered bank_x header.
var bankColumns = []string{
	"record_id",
	"booking_date",
	"value_date",
	"amount",
	"currency",
	"description",
	"bank_reference",
}

var bankRequiredMetadata = []string{"rows", "total", "currency", "opening", "closing", "as_of"}

// BankParser parses `bank_x` statement CSV files into canonical records plus
// the statement balances the file declares. It is stateless; one parser value
// may be reused across files and goroutines.
type BankParser struct{}

// NewBankParser returns a parser for the bank_x format described in
// docs/formats.md.
func NewBankParser() *BankParser { return &BankParser{} }

// Source returns the configured source name this parser implements.
func (p *BankParser) Source() string { return SourceBank }

// UntrustedFields returns the free-text columns kept in Record.Raw. The values
// must never be treated as instructions (spec section 9).
func (p *BankParser) UntrustedFields() []string { return []string{UntrustedBankDescription} }

// bankRaw is the JSON evidence payload for one bank_x row. It preserves the
// source strings, including the untrusted description.
type bankRaw struct {
	RecordID      string `json:"record_id"`
	BookingDate   string `json:"booking_date"`
	ValueDate     string `json:"value_date"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	Description   string `json:"description"`
	BankReference string `json:"bank_reference"`
}

// Parse implements Parser. The canonical kind is derived from the amount sign
// only (credit -> payout, debit -> fee); the free-text description never
// influences it. The declared opening/closing balances become one Statement so
// the pipeline can check FIN-55 continuity. The parser does not verify
// closing = opening + total; that is continuity/tie-out arithmetic.
func (p *BankParser) Parse(r io.Reader) (ParseResult, error) {
	file, err := readSourceFile(r, bankColumns)
	if err != nil {
		return ParseResult{}, err
	}
	if err := checkMetadata(file.metadata, bankRequiredMetadata, nil); err != nil {
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
	opening, err := file.amountValue("opening", currency)
	if err != nil {
		return ParseResult{}, err
	}
	closing, err := file.amountValue("closing", currency)
	if err != nil {
		return ParseResult{}, err
	}
	asOf, err := file.dateValue("as_of")
	if err != nil {
		return ParseResult{}, err
	}

	seen := make(map[string]struct{}, len(file.rows))
	records := make([]Record, 0, len(file.rows))
	for index, row := range file.rows {
		record, err := bankRecord(row, index+1, currency)
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
		Statements: []Statement{{
			AsOfDate:     asOf,
			OpeningMinor: opening.AmountMinor(),
			ClosingMinor: closing.AmountMinor(),
			Currency:     currency.Code(),
		}},
	}, nil
}

// bankRecord validates one bank_x data row and converts it to a canonical
// record. number is the 1-based data row number used in error context.
func bankRecord(row []string, number int, currency money.Currency) (Record, error) {
	fail := func(err error) (Record, error) {
		return Record{}, fmt.Errorf("data row %d: %w", number, err)
	}

	// record_id must be non-empty after trimming whitespace. A non-empty ID
	// with surrounding spaces is accepted and preserved verbatim.
	recordID := row[0]
	if strings.TrimSpace(recordID) == "" {
		return fail(fmt.Errorf("%w: record_id is empty or whitespace-only", ErrMalformedRow))
	}
	bookingDate, err := rowDate(row[1], "booking_date")
	if err != nil {
		return fail(err)
	}
	valueDate, err := rowDate(row[2], "value_date")
	if err != nil {
		return fail(err)
	}
	amount, err := rowAmount(row[3], "amount", currency)
	if err != nil {
		return fail(err)
	}
	if row[4] != currency.Code() {
		return fail(fmt.Errorf("%w: row currency %q, file currency %q",
			ErrCurrencyMismatch, row[4], currency.Code()))
	}
	if amount.AmountMinor() == 0 {
		return fail(fmt.Errorf("%w: amount %q is zero", ErrInvalidSign, row[3]))
	}
	kind := KindPayout
	if amount.AmountMinor() < 0 {
		kind = KindFee
	}

	raw, err := json.Marshal(bankRaw{
		RecordID:      row[0],
		BookingDate:   row[1],
		ValueDate:     row[2],
		Amount:        row[3],
		Currency:      row[4],
		Description:   row[5],
		BankReference: row[6],
	})
	if err != nil {
		return fail(fmt.Errorf("%w: encode raw payload: %v", ErrMalformedRow, err))
	}

	return Record{
		SourceRecordID: recordID,
		Kind:           kind,
		AmountMinor:    amount.AmountMinor(),
		Currency:       currency.Code(),
		OccurredAt:     bookingDate,
		ValueDate:      valueDate,
		Raw:            raw,
	}, nil
}
