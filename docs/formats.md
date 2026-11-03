# Source file formats: `psp_a` and `bank_x`

Two deliberately small, fully specified CSV formats stand in for real PSP
settlement files and bank statements. They are the only formats `internal/ingest`
parses in M1. Everything here is synthetic data: no real account numbers, bank
files, or PSP exports are used.

The matching engine and the synthetic generator are written against this
document, so a change to a column name, a sign convention, or the metadata keys
is a breaking format change.

## 1. Common envelope

Both formats share the same envelope.

### 1.1 Encoding and line endings

- **Encoding:** UTF-8 only. Any invalid UTF-8 byte sequence is rejected with
  `ingest.ErrInvalidEncoding`; files are never decoded as Latin-1 or any other
  legacy encoding.
- **Line endings:** LF (`\n`) and CRLF (`\r\n`) are both accepted and may be
  mixed in one file. A lone CR inside an unquoted field is a CSV error.
- **BOM:** an optional single UTF-8 byte-order mark (`EF BB BF`) at the very
  start of the file is accepted and ignored. A BOM anywhere else is ordinary
  data and fails parsing. Files without a BOM are the canonical output.
- **Blank lines:** blank lines (including the final newline) are ignored
  everywhere and do not count towards the declared row count.

### 1.2 Metadata line

The first non-blank line of the file is metadata. It starts with a single `#`,
followed by space-separated `key=value` tokens:

```text
# rows=3 total=-140.20 currency=USD batch=B-2026-06-15-001
```

- Keys are lower-case ASCII (`[a-z][a-z0-9_]*`) and appear at most once.
- Values are non-empty tokens without spaces or quotes. Values are never CSV
  quoted and never contain a comma, space, or tab.
- Unknown keys, duplicate keys, missing required keys, an empty value, and a
  token without `=` are all `ingest.ErrBadMetadata`.
- A file whose first non-blank line does not start with `#` is
  `ingest.ErrBadMetadata`.
- Only the first line is metadata. A `#` comment after the header is not
  supported and is treated as a data row (which then fails the column count).

Required and optional keys per format are listed below. `rows`, `total`, and
`currency` are the **declared control totals** (FIN-50):

- `rows` — the number of data rows that follow the header. A non-negative
  decimal integer.
- `total` — the exact signed sum of the data rows' signed `net` (`psp_a`) or
  `amount` (`bank_x`) values, written as a decimal string in `currency`.
- `currency` — the ISO 4217 code shared by every row and, for `bank_x`, by the
  statement balances.

The parser returns these as `ingest.Declared` and does **not** compare them with
the parsed rows itself: FIN-50 comparison and quarantine are the ingestion
pipeline's job, so a corrupted trailer produces a quarantined batch (with a
reason) instead of a parse error. The parser does reject a metadata line that is
structurally invalid or whose numbers cannot be parsed.

### 1.3 Header line

The second non-blank line is the header. It is a CSV record whose field names
must match the documented names **exactly**: case-sensitive, same order, no
extras, no omissions.

- A missing documented column is `ingest.ErrMissingColumn`.
- An unexpected column, a reordered header, or a duplicated column is
  `ingest.ErrUnknownColumn`.
- A file without a header line is `ingest.ErrInvalidFormat`.

### 1.4 Data rows and quoting

Data rows use RFC 4180 CSV as implemented by Go's `encoding/csv`:

- Fields are separated by commas.
- A field may be double-quoted; inside a quoted field, `""` represents one
  double quote and commas, spaces, and line breaks are literal.
- Unquoted fields must not contain `"`.
- Every data row must have exactly as many fields as the header; otherwise the
  file fails with `ingest.ErrMalformedRow`.
- Empty fields are allowed where a column is documented as optional.

### 1.5 Decimal amounts

Every monetary value is a **decimal string**, never a float and never an
integer of unknown units. It is parsed by `money.ParseDecimal` with the file's
currency, so the string is exact and scaled by the currency's exponent:

| Currency | Input | Minor units |
|---|---|---|
| USD (exponent 2) | `10.00` | 1000 |
| USD (exponent 2) | `10.001` | rejected: `money.ErrExcessPrecision` |
| JPY (exponent 0) | `100` | 100 |
| JPY (exponent 0) | `100.5` | rejected: `money.ErrExcessPrecision` |
| KWD (exponent 3) | `1.234` | 1234 |

Accepted syntax: optional leading `-`, one or more digits, and an optional
`.` with one or more digits. No `+` sign, no exponent notation, no thousands
separators, no currency symbol, no leading or trailing whitespace. An amount
token longer than 32 bytes is rejected: no value representable in `int64`
minor units needs more, and the bound keeps hostile inputs cheap to reject.
Malformed values are `ingest.ErrInvalidAmount` (which also wraps the
underlying `money` error, so `errors.Is(err, money.ErrExcessPrecision)` works).

### 1.6 Dates and timestamps

- **Dates** are `YYYY-MM-DD` in the proleptic Gregorian calendar, with no time
  or zone. They are interpreted as UTC midnights.
- **Timestamps** are RFC 3339 with a mandatory offset, for example
  `2026-06-15T10:00:00Z` or `2026-06-15T12:00:00+02:00`. They are normalized to
  UTC in the parsed record.
- A malformed date or timestamp is `ingest.ErrMalformedDate`.

### 1.7 Canonical record mapping

`internal/ingest` turns each data row into an `ingest.Record`:

- `source_record_id` is the row's `record_id`; it must be non-empty and unique
  within the file (`ingest.ErrDuplicateRecordID` on a repeat).
- `occurred_at` is the row's event timestamp (for `bank_x`, the booking date at
  UTC midnight).
- `value_date` is set only by `bank_x` (its value date); `psp_a` leaves it zero
  and the pipeline derives it from the per-source calendar (FIN-41/FIN-42).
- `raw` is a JSON object holding the row's original string values, including
  the untrusted free-text column. It is evidence only and is never used for
  matching or policy decisions.

## 2. `psp_a` — PSP settlement file

One file is one PSP settlement batch in one currency. It contains charges,
refunds, fees, payouts, and chargebacks.

### 2.1 Metadata keys

| Key | Required | Meaning |
|---|---|---|
| `rows` | yes | Number of data rows. |
| `total` | yes | Signed sum of the `net` column. |
| `currency` | yes | Batch currency; every row must use it. |
| `batch` | yes | PSP batch ID; every row's `batch_id` must equal it. |
| `period_start` | no | First covered date (`YYYY-MM-DD`). |
| `period_end` | no | Last covered date (`YYYY-MM-DD`). |

`period_start` and `period_end` must either both be present or both absent.
When present they become `ParseResult.PeriodStart`/`PeriodEnd`; otherwise the
pipeline derives the period from the records.

### 2.2 Header and columns

```text
record_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo
```

| Column | Required value |
|---|---|
| `record_id` | Stable PSP record ID. Non-empty, unique in the file. |
| `type` | One of `charge`, `refund`, `fee`, `payout`, `chargeback`. Unknown values are `ingest.ErrUnknownRecordType`. |
| `occurred_at` | RFC 3339 event timestamp. |
| `settled_at` | RFC 3339 settlement timestamp, or empty when the row is not settled yet. |
| `gross` | Signed gross amount before fees, decimal string. |
| `fee` | Non-negative fee amount, decimal string. |
| `net` | Signed net amount, decimal string. Must equal `gross - fee` exactly (`ingest.ErrAmountMismatch`). |
| `currency` | ISO 4217 code. Must equal the metadata currency (`ingest.ErrCurrencyMismatch`). |
| `reference` | PSP reference used by matching. May be empty. |
| `batch_id` | Must equal the metadata `batch` (`ingest.ErrMetadataMismatch`). |
| `memo` | Untrusted free text. May be empty and may contain commas, quotes, or newlines when quoted. |

### 2.3 Record types and sign conventions

| `type` | Meaning | `net` sign |
|---|---|---|
| `charge` | Card charge settled by the PSP. | strictly positive |
| `refund` | Refund of an earlier charge. | strictly negative |
| `fee` | PSP fee charged against the merchant. | strictly negative |
| `payout` | Transfer from the PSP to the merchant bank account. | strictly negative |
| `chargeback` | Unexpected debit from a chargeback. | strictly negative |

`net` is never zero. A zero `net`, or a `net` with the wrong sign for its type,
is `ingest.ErrInvalidSign`. `gross` is signed with the same sign as `net` (fees
are subtracted, so `gross - fee` keeps the sign); `fee` is always `>= 0`.

Canonical mapping: `kind` is the `type` value, `amount_minor` is `net`,
`fee_minor` is `fee` in minor units, `settled_at` is the optional settlement
time, and no statement balance is emitted.

### 2.4 Example

```text
# rows=5 total=-140.20 currency=USD batch=B-2026-06-15-001 period_start=2026-06-15 period_end=2026-06-15
record_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo
psp_0001,charge,2026-06-15T10:00:00Z,2026-06-15T10:00:00Z,100.00,2.90,97.10,USD,REF-0001,B-2026-06-15-001,"widget, blue"
psp_0002,refund,2026-06-15T11:00:00Z,,-30.00,0.00,-30.00,USD,REF-0002,B-2026-06-15-001,"customer said ""too big"""
psp_0003,fee,2026-06-15T12:00:00Z,,0.00,1.25,-1.25,USD,,B-2026-06-15-001,monthly fee
psp_0004,payout,2026-06-15T13:00:00Z,2026-06-16T09:00:00Z,-200.00,0.00,-200.00,USD,REF-PAYOUT-1,B-2026-06-15-001,
psp_0005,chargeback,2026-06-15T14:00:00Z,,-6.05,0.00,-6.05,USD,REF-0002,B-2026-06-15-001,chargeback on REF-0002
```

## 3. `bank_x` — bank statement CSV

One file is one bank statement for one account, one currency, and one statement
date. Rows are the statement's transaction lines.

### 3.1 Metadata keys

| Key | Required | Meaning |
|---|---|---|
| `rows` | yes | Number of data rows. |
| `total` | yes | Signed sum of the `amount` column. |
| `currency` | yes | Statement currency; every row must use it. |
| `opening` | yes | Opening balance, decimal string. |
| `closing` | yes | Closing balance, decimal string. |
| `as_of` | yes | Statement date (`YYYY-MM-DD`), emitted as the statement's `as_of_date`. |

The parser emits one `ingest.Statement` with `as_of_date = as_of` and the
declared opening/closing balances, so the ingestion pipeline can check FIN-55
continuity against the previous closing balance. The parser does not verify
`closing = opening + total`; that arithmetic belongs to continuity and tie-out.

### 3.2 Header and columns

```text
record_id,booking_date,value_date,amount,currency,description,bank_reference
```

| Column | Required value |
|---|---|
| `record_id` | Stable statement-line ID. Non-empty, unique in the file. |
| `booking_date` | Date the bank posted the line (`YYYY-MM-DD`). |
| `value_date` | Date the funds are valued (`YYYY-MM-DD`). Usually equal to or after the booking date. |
| `amount` | Signed decimal amount in `currency`. Credits are positive, debits are negative, zero is rejected (`ingest.ErrInvalidSign`). |
| `currency` | ISO 4217 code. Must equal the metadata currency (`ingest.ErrCurrencyMismatch`). |
| `description` | Untrusted free text. May be empty and may contain commas, quotes, or newlines when quoted. |
| `bank_reference` | Bank's reference for the line. May be empty. |

### 3.3 Record types and sign conventions

A bank statement does not carry a PSP record type. The canonical kind is derived
from the sign only, never from the free-text description:

| `amount` | Canonical `kind` |
|---|---|
| positive | `payout` (a credit received into the bank account) |
| negative | `fee` (a debit charged to the bank account) |
| zero | rejected: `ingest.ErrInvalidSign` |

M1 synthetic data puts charges, refunds, chargebacks, and FX detail in the PSP
files; the bank statement carries the cash movements. A chargeback or FX event
that reaches the bank is still a debit line and is recorded as `fee` in M1.

Canonical mapping: `amount_minor` is `amount`, `fee_minor` is 0,
`occurred_at` is the booking date at 00:00 UTC, and `value_date` is the value
date at 00:00 UTC (so the pipeline uses it directly instead of deriving it).

### 3.4 Example

```text
# rows=3 total=85.00 currency=USD opening=1000.00 closing=1085.00 as_of=2026-06-15
record_id,booking_date,value_date,amount,currency,description,bank_reference
bnk_0001,2026-06-15,2026-06-15,120.00,USD,"payout from psp, batch B-2026-06-15-001",BNK-0001
bnk_0002,2026-06-15,2026-06-16,-25.00,USD,"wire fee, international",BNK-0002
bnk_0003,2026-06-15,2026-06-15,-10.00,USD,monthly account fee,BNK-0003
```

## 4. Error cases

Every parser error is a typed sentinel (wrapped with row/column context, so
`errors.Is` works). Parsers never panic on malformed input.

| Condition | Sentinel |
|---|---|
| File is not valid UTF-8 | `ingest.ErrInvalidEncoding` |
| Missing/duplicate/unknown metadata key, bad `rows`, unparseable decimal in metadata, unknown metadata currency | `ingest.ErrBadMetadata` |
| No header line | `ingest.ErrInvalidFormat` |
| Missing documented column | `ingest.ErrMissingColumn` |
| Extra, duplicated, or reordered column | `ingest.ErrUnknownColumn` |
| Data row with the wrong field count, or unparseable CSV | `ingest.ErrMalformedRow` |
| Unknown `psp_a` record type | `ingest.ErrUnknownRecordType` |
| Malformed date, timestamp, or `as_of` | `ingest.ErrMalformedDate` |
| Row currency differs from the metadata currency | `ingest.ErrCurrencyMismatch` |
| Malformed or excess-precision amount (wraps the `money` error) | `ingest.ErrInvalidAmount` |
| Zero amount or wrong sign for the record type | `ingest.ErrInvalidSign` |
| `psp_a` `net != gross - fee` | `ingest.ErrAmountMismatch` |
| `psp_a` `batch_id` differs from metadata `batch` | `ingest.ErrMetadataMismatch` |
| Repeated `record_id` in one file | `ingest.ErrDuplicateRecordID` |
| Zero-byte file (no metadata) | `ingest.ErrBadMetadata` |

A corrupted `rows`/`total` claims are **not** parse errors: the parser reports
what the file declares, and the ingestion pipeline quarantines the batch
(FIN-50).

## 5. Untrusted fields

Spec section 9 treats all free text from source files as untrusted data. The
only free-text columns in these formats are:

- `psp_a`: `memo`
- `bank_x`: `description`

They are preserved verbatim in `Record.Raw` and must be listed per source in
the ingestion configuration so later prompt assembly (M2-07) and the adversarial
suite (M6-02) can label them as data:

```yaml
ingest:
  sources:
    psp_a:
      timezone: America/New_York
      cutoff: "17:00"
      untrusted_fields: [memo]
    bank_x:
      timezone: Europe/Stockholm
      cutoff: "15:30"
      untrusted_fields: [description]
```

`reference` and `bank_reference` are identifiers, not free text, and must not
be used as instructions either. All other columns are format-controlled values
(record type, dates, currencies, decimals).

## 6. Test fixtures

Golden files live in `internal/ingest/testdata/psp_a` and
`internal/ingest/testdata/bank_x`. Each `<name>.csv` input has a
`<name>.golden` expected parse result (indented JSON). Regenerate them only when
the format intentionally changes:

```text
go test -run TestGolden -update ./internal/ingest
```

The fixtures cover every `psp_a` record type, comma-and-quote quoting, CRLF,
BOM, zero-decimal JPY amounts, and an empty (zero-row) file.
