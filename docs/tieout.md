# Balance tie-out (FIN-54)

The tie-out is an independent check that, for one reconciliation scope and
statement date, the ledger and the external statement agree once open items on
each side are taken into account. It is computed from raw balances and open
items, not from matcher output. `internal/tieout` has its own SQL and reads
`matches` / `match_legs` only for set membership; it never imports
`internal/match`. A matcher bug that affects balances therefore cannot hide a
discrepancy.

## Equation

All amounts are integer minor units in the scope's currency.

```text
Unexplained = (LedgerBalance − ΣLedgerOnly)
            − (StatementClosing − ΣExternalOnly)
```

- **LedgerBalance** — the scope's clearing-account balance through the statement
  date, inclusive: `ledger_txns.value_date <= asOf`.
- **ΣLedgerOnly** — signed total of ledger transactions through `asOf` that no
  active match leg claims.
- **StatementClosing** — `statement_balances.closing_minor` for the scope with
  `as_of_date = asOf` exactly. A missing row is `ErrNoStatement`, never zero.
- **ΣExternalOnly** — signed total of external records for the scope's source
  and currency through `asOf` that no active match header claims.

A clean day yields exactly zero. A non-zero value is the unexplained difference
that alerts and blocks period close (FIN-54).

## Sign conventions (D-14)

Tie-out uses the account's normal balance:

| Account type | Normal balance |
|---|---|
| asset, expense | Σ debits − Σ credits |
| liability, equity, revenue | Σ credits − Σ debits |

The scope's clearing account is an asset, so **debits are positive and credits
are negative**. External record `amount_minor` uses the same convention: a
charge/payout receipt that debits the clearing account is positive, a refund,
fee, chargeback, or payout that credits it is negative. The statement closing
balance is the bank statement's positive account balance, which corresponds to
the clearing asset's debit-positive balance.

Ledger-only and external-only amounts are reported with that same sign. They
are not "absolute values to add": the equation subtracts each signed total from
its own side.

## Worked example

One USD scope on 2026-06-10:

| Item | Minor units | Note |
|---|---:|---|
| Matched exact pair 1 | +10,000 | ledger and statement |
| Matched exact pair 2 | +5,000 | ledger and statement |
| Ledger-only booking | +1,000 | ledger has it; statement has not seen it |
| External-only record | +2,000 | statement has it; ledger has not booked it |
| **LedgerBalance** | **16,000** | 10,000 + 5,000 + 1,000 |
| **ΣLedgerOnly** (1 txn) | **1,000** | the unmatched booking |
| **Adjusted ledger** | **15,000** | 16,000 − 1,000 |
| **StatementClosing** | **17,000** | includes the external-only record |
| **ΣExternalOnly** (1 record) | **2,000** | the unmatched record |
| **Adjusted statement** | **15,000** | 17,000 − 2,000 |
| **Unexplained** | **0** | 15,000 − 15,000 |

### Seeded matcher bug

The roadmap bug is a tolerance match whose delta is never booked. The external
record is 19,950 and the ledger booked 20,000; pass 2 accepts the 50-unit delta
as a fee tolerance, but no ledger entry books it. Both rows are matched, so
`ΣLedgerOnly = ΣExternalOnly = 0`:

```text
Unexplained = (20,000 − 0) − (19,950 − 0) = +50
```

The tie-out therefore surfaces the 50 minor units even though the matcher
reports a successful match.

A **dropped** match is different: neither row is claimed, so both become open
items and the equation still ties:

```text
(10,000 − 10,000) − (10,000 − 10,000) = 0
```

That is expected. A dropped match creates breaks and is caught by the matcher
component evals (§11.5); FIN-54 catches balances that do not tie.

## As-of and membership semantics

- `value_date <= asOf` is inclusive on both the ledger and external side. A
  transaction dated `asOf` is included; one dated `asOf + 1` is excluded (D-2).
- The statement row must match `as_of_date = asOf` exactly.
- Membership uses the **current active** match set at `Compute` time. M1
  evaluates the latest statement date after matching; recomputing a historical
  date after later matches arrive is an M5 concern and is not tested in M1
  (D-14).
- Only the scope's currency is summed. A statement or ledger currency that
  disagrees with the scope is an error, never silently included.

## FIN-55 input assumption

Statement continuity (FIN-55) is an **input assumption**, not re-verified here.
Ingestion (M1-07) checks that each statement's opening balance equals the
previous statement's closing balance for the same scope; a gap or restatement
quarantines the batch. The tie-out assumes the `statement_balances` row it reads
is the continuous statement for that date. If continuity fails, no trusted
statement row exists for the date and `Compute` returns `ErrNoStatement`.

## CLI

```text
recon tieout --scope <scope-uuid> --date YYYY-MM-DD [--json]
```

The human output prints the ledger and statement balances, both open-item
totals with counts, the adjusted balances, and the unexplained difference. The
`--json` output is a stable snake_case object:

```json
{"scope_id":"…","as_of":"2026-06-10","currency":"USD",
 "ledger_balance":{"amount_minor":16000,"currency":"USD"},
 "statement_closing":{"amount_minor":17000,"currency":"USD"},
 "ledger_only":{"amount":{"amount_minor":1000,"currency":"USD"},"count":1},
 "external_only":{"amount":{"amount_minor":2000,"currency":"USD"},"count":1},
 "unexplained":{"amount_minor":0,"currency":"USD"}}
```

Exit codes:

| Code | Meaning |
|---:|---|
| 0 | tied out (`Unexplained == 0`) |
| 2 | valid computation with a non-zero unexplained difference |
| 1 | error (bad arguments, missing statement, missing scope, currency mismatch, database error) |
