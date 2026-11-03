// Package ingest loads and normalizes source statements and internal records.
//
// The M1 source formats are documented in docs/formats.md:
//
//   - psp_a: a PSP settlement CSV (charge, refund, fee, payout, chargeback)
//     parsed by NewPSPParser.
//   - bank_x: a bank statement CSV parsed by NewBankParser, which also reports
//     the statement's opening/closing balances.
//
// Both parsers implement Parser and are database-free: Parse only reads the
// file bytes, never the wall clock or a connection. Amounts are exact decimal
// strings converted with money.ParseDecimal; free-text columns (psp_a memo,
// bank_x description) are preserved in Record.Raw and listed as untrusted in
// the per-source config. Control totals in the file are returned as Declared
// and compared by the pipeline (FIN-50); a mismatch quarantines the batch.
package ingest
