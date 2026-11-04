package ingest

import (
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Errors returned by Ingest. A control-total, continuity, currency, or scope
// problem is not an error: it quarantines the batch and Ingest returns a
// Result with StatusQuarantined. These sentinels cover caller mistakes and
// parser contract violations, which have no trustworthy batch row to persist.
var (
	// ErrInvalidRequest means Ingest was called without a source, data, or
	// parser, or with an ingester that was not constructed by New.
	ErrInvalidRequest = errors.New("ingest: invalid request")
	// ErrUnknownSource means the source has no per-source calendar config.
	ErrUnknownSource = errors.New("ingest: unknown source")
	// ErrParse wraps a Parser error. A parse failure cannot be quarantined
	// because the declared totals, currency, and counts are unknown.
	ErrParse = errors.New("ingest: parse")
	// ErrInvalidRecord means the parser returned a canonical record that
	// violates the ingest contract (empty id, unknown currency or kind, missing
	// timestamp, malformed raw JSON).
	ErrInvalidRecord = errors.New("ingest: invalid record")
)

// Kind is a canonical external record kind. The values match the
// external_records.kind CHECK constraint.
type Kind string

// Canonical external record kinds. Adjustments and reversals are ledger-only
// and never appear in a source file.
const (
	KindCharge     Kind = "charge"
	KindRefund     Kind = "refund"
	KindPayout     Kind = "payout"
	KindFee        Kind = "fee"
	KindChargeback Kind = "chargeback"
	KindFX         Kind = "fx"
)

// Valid reports whether the kind is one external_records accepts.
func (k Kind) Valid() bool {
	switch k {
	case KindCharge, KindRefund, KindPayout, KindFee, KindChargeback, KindFX:
		return true
	default:
		return false
	}
}

// Parser turns one source file into canonical records, its declared control
// totals, and any statement balances it carries. M1-08 supplies the psp_a and
// bank_x implementations; M1-07 only depends on this interface.
//
// Parse must not touch the database or the wall clock. All timestamps come
// from the file or the caller's injected clock; the ingest pipeline derives
// missing value dates from the per-source calendar.
type Parser interface {
	Parse(r io.Reader) (ParseResult, error)
}

// ParseResult is one parser's complete view of a file.
type ParseResult struct {
	// Records are canonical external records in file order.
	Records []Record
	// Declared is the trailer the file claims. FIN-50 compares it with the
	// parsed records before anything is inserted.
	Declared Declared
	// Statements are the statement balances the file carries (bank files).
	// PSP settlement files normally return none.
	Statements []Statement
	// PeriodStart and PeriodEnd are the covered dates. Both zero means derive
	// them from the records' value dates; one zero is a parser contract error.
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// Declared is a file's control trailer.
type Declared struct {
	// TotalMinor is the signed sum of the file's record amounts, in
	// Declared.Currency's minor units.
	TotalMinor int64
	// RowCount is the number of records the file claims.
	RowCount int
	// Currency is the batch currency every record must use.
	Currency string
}

// Record is one normalized external record. Amounts are signed integer minor
// units; no floating point crosses this boundary (FIN-01, FIN-05).
type Record struct {
	// SourceRecordID is the source's stable identifier. It is unique per
	// source and drives FIN-51 dedupe.
	SourceRecordID string
	// Kind is the canonical event kind.
	Kind Kind
	// AmountMinor is the signed amount in Currency's minor units.
	AmountMinor int64
	// Currency is the ISO 4217 code.
	Currency string
	// FeeMinor is the signed-to-non-negative fee portion, when the source
	// reports one. Zero when absent.
	FeeMinor int64
	// OccurredAt is the source event time. It is stored as UTC TIMESTAMPTZ
	// (FIN-40).
	OccurredAt time.Time
	// SettledAt is the optional settlement time.
	SettledAt *time.Time
	// ValueDate is the optional business date supplied by the file (bank
	// statements carry one). When zero, the pipeline derives it with
	// ValueDate(source, OccurredAt) after converting to the source timezone.
	ValueDate time.Time
	// Raw is the untrusted source payload (for example memo/description kept
	// for evidence). It must be valid JSON.
	Raw json.RawMessage
	// RawHash is the SHA-256 hex of the raw payload. The pipeline derives it
	// when empty.
	RawHash string
}

// Statement is one end-of-day balance a bank file reports. Continuity is per
// reconciliation scope: opening must equal the previous statement's closing
// (FIN-55), with the first statement accepted as the baseline.
type Statement struct {
	// AsOfDate is the statement's business date (DATE, no time component).
	AsOfDate time.Time
	// OpeningMinor is the opening balance in signed minor units.
	OpeningMinor int64
	// ClosingMinor is the closing balance in signed minor units.
	ClosingMinor int64
	// Currency is the scope currency.
	Currency string
}
