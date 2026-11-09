# Matcher report — split dev

| Field | Value |
|---|---|
| Split | dev |
| Dataset version | fixture-version |
| Scenarios | 8 |
| Scoreable scenarios | 6 |
| Generated at | 2026-11-09T00:00:00Z |

Warnings:

- recall below 1.0: inspect per-pass misses and ADR 0004 residuals

## Headline

| Metric | Value | n |
|---|---|---|
| Precision | 0.5000 | 2/4 |
| Recall | 0.6667 | 2/3 |

## By pass

Recall uses the global required-link denominator, so per-pass recall sums to overall recall.

| Pass | Matches | Correct | Precision | Required hits | Recall |
|---|---|---|---|---|---|
| exact | 3 | 1 | 0.3333 | 1 | 0.3333 |
| fuzzy | 1 | 1 | 1.0000 | 1 | 0.3333 |

## Exclusions

| Reason | Scenarios |
|---|---|
| ambiguous | 1 |
| escalate | 1 |
| no_external_record | 1 |
| no_ledger_txn | 1 |
| not_expected_link | 1 |

Recall below 1.0: inspect per-pass misses and ADR 0004 residuals.
