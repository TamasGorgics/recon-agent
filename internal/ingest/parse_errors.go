package ingest

import "errors"

// Source names for the two M1 file formats. They are the values stored in
// reconciliation_scopes.source and the keys of the per-source ingest config.
const (
	// SourcePSP is the PSP settlement file source.
	SourcePSP = "psp_a"
	// SourceBank is the bank statement CSV source.
	SourceBank = "bank_x"
)

// Untrusted free-text columns (spec section 9). The parsers keep these values
// in Record.Raw; the per-source config lists them as untrusted_fields so M2-07
// prompt assembly never treats them as instructions.
const (
	// UntrustedPSPMemo is the psp_a free-text memo column.
	UntrustedPSPMemo = "memo"
	// UntrustedBankDescription is the bank_x free-text description column.
	UntrustedBankDescription = "description"
)

// Typed sentinel errors returned by the psp_a and bank_x parsers. Every error
// is wrapped with the file, row, and column context, so callers branch with
// errors.Is and never parse error strings.
var (
	// ErrInvalidEncoding means the file is not valid UTF-8.
	ErrInvalidEncoding = errors.New("ingest: file is not valid UTF-8")
	// ErrInvalidFormat means the file does not have the documented envelope
	// (for example it has no header line).
	ErrInvalidFormat = errors.New("ingest: invalid file format")
	// ErrBadMetadata means the metadata line is missing, malformed, or has
	// unknown, duplicate, or unparseable keys.
	ErrBadMetadata = errors.New("ingest: bad file metadata")
	// ErrMissingColumn means the header omits a documented column.
	ErrMissingColumn = errors.New("ingest: missing required column")
	// ErrUnknownColumn means the header has an extra, duplicated, or
	// misordered column.
	ErrUnknownColumn = errors.New("ingest: unknown or misordered column")
	// ErrMalformedRow means a data row has the wrong field count or is not
	// valid CSV.
	ErrMalformedRow = errors.New("ingest: malformed data row")
	// ErrUnknownRecordType means a psp_a type value is not one of the five
	// documented settlement kinds.
	ErrUnknownRecordType = errors.New("ingest: unknown record type")
	// ErrMalformedDate means a date or timestamp value does not match the
	// documented layout.
	ErrMalformedDate = errors.New("ingest: malformed date")
	// ErrCurrencyMismatch means a row's currency differs from the file's
	// declared currency.
	ErrCurrencyMismatch = errors.New("ingest: row currency does not match the file")
	// ErrInvalidAmount means an amount is malformed, over-precise, or
	// otherwise not an exact minor-unit value for its currency.
	ErrInvalidAmount = errors.New("ingest: invalid amount")
	// ErrInvalidSign means an amount is zero or has the wrong sign for its
	// record type, or a fee is negative.
	ErrInvalidSign = errors.New("ingest: invalid amount sign")
	// ErrAmountMismatch means psp_a net != gross - fee.
	ErrAmountMismatch = errors.New("ingest: net does not equal gross minus fee")
	// ErrMetadataMismatch means a row contradicts the file metadata (for
	// example a psp_a batch_id differs from the declared batch).
	ErrMetadataMismatch = errors.New("ingest: row does not match file metadata")
	// ErrDuplicateRecordID means one file contains the same record_id twice.
	ErrDuplicateRecordID = errors.New("ingest: duplicate record id")
)
