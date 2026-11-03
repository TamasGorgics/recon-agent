# Matching engine

The matcher pairs external source records with ledger transactions inside one
reconciliation scope (one ledger account + one source + one currency). It is
deterministic: no agent, LLM, wall clock, or floating point participates. This
document describes the pass order, the rules and configuration, the ambiguity
behaviour, and the complexity limits added by M1-13.

`internal/match` implements the engine; `docs/spec.md` section 5 and
`docs/ROADMAP.md` M1-12/M1-13 are the requirements.

## Model

- `matches` is a header (`external_record_id`, `rule`, `confidence`,
  `match_run_id`, `status`) and `match_legs` holds one leg per claimed ledger
  transaction. A 1:1 match has one leg; a composite match has N legs.
- A run loads only external records without an active header and ledger
  transactions without an active leg, within an inclusive `value_date` window.
  Re-running on unchanged data therefore inserts no new matches; a run row may
  be recorded and its counts are unchanged.
- Amounts are signed integer minor units. An external record's amount is the
  source's signed net; a ledger transaction's amount is the net on the scope's
  ledger account (debit positive, credit negative). `confidence` is integer
  basis points (10000 = 1.0) and is stored as `NUMERIC(5,4)`.
- Passes are pure functions over the run snapshot. `Evaluate` claims accepted
  candidates between passes, so a later pass never reuses a record or ledger
  transaction claimed by an earlier one. Inputs are sorted by id before any pass
  runs, and each pass iterates deterministically, so shuffling the input cannot
  change the result.

## Pass order

| # | Pass | Rule stored | Purpose |
|---|---|---|---|
| 1 | Exact | `exact` | Same reference, amount, and currency. |
| 2 | Reference + tolerance | `reference_tolerance` | Same reference and currency; absolute amount delta within the configured per-currency fee tolerance. |
| 3 | Composite | `composite_batch` or `composite` | One external payout against N ledger transactions whose signed sum equals the payout exactly. Declared batch membership first, then bounded subset-sum. |
| 4 | Fuzzy | `fuzzy` | Amount + value-date window + counterparty hint, scored in basis points. |
| 5 | Duplicates | never stored | Repeated external ids and same-amount/same-time charges become `duplicate` break candidates. Never auto-links. |

A record left unmatched after all passes is the remainder; M1-14 turns it into
breaks. Passes 3 and 5 additionally return explicit break candidates (see
below), which M1-14 persists with `ON CONFLICT (signature) DO NOTHING`.

## Pass 3: composite

One payout can settle N ledger transactions.

1. **Declared batch membership first.** An external record's batch is
   `external_records.batch_id` (the settlement batch its file declared). The
   matcher takes the other external records in that same batch, collects their
   references, and resolves each reference to exactly one unmatched ledger
   transaction in the scope. The candidate matches only when every reference
   resolves uniquely and the signed leg sum equals the payout amount exactly.
   The rule is `composite_batch`.
2. **Bounded subset-sum fallback.** When declared membership is absent,
   ambiguous, or does not sum exactly, the pass searches the unmatched ledger
   transactions for a subset whose signed sum equals the payout exactly. The
   pool is restricted to
   - the scope currency,
   - the same sign as the payout,
   - a leg magnitude no larger than the payout magnitude,
   - a value date within `composite_date_window_days` of the payout.
3. **Exactness.** There is no tolerance in pass 3. A subset is accepted only
   when the sum difference is zero minor units, and a composite always has at
   least two legs (a single equal-amount ledger transaction is left to the fuzzy
   pass, if it qualifies).
4. **Ambiguity.** Two or more distinct valid subsets of the same payout are
   ambiguous: the pass returns no match rather than choosing arbitrarily.
5. **Caps.** `composite_candidate_limit` bounds the pool size,
   `composite_max_legs` bounds the subset size, and `composite_max_nodes` bounds
   the depth-first search. Exceeding any cap produces a break candidate, never a
   partial or best-effort match.

### Complexity

The declared-membership path is `O(records in batch + legs)` per payout. The
subset-sum path is exponential in the worst case, so it is bounded: at most
`composite_candidate_limit` items, `composite_max_legs` chosen legs, and
`composite_max_nodes` visited nodes, with a suffix-sum prune. Overall pass 3 is
`O(E + L + P · nodes)` where P is the number of unmatched payouts and nodes is
the configured cap.

## Pass 4: fuzzy

Each unmatched external record is scored against unmatched ledger transactions
in the same currency, within `fuzzy_date_window_days` value-date distance, and
whose absolute amount delta is within the configured fee tolerance for that
currency. The score is integer basis points:

| Component | Max | Rule |
|---|---|---|
| Amount | 5000 | 5000 for an exact amount; otherwise `5000 × (tolerance − delta) / tolerance`. |
| Value date | 3000 | 3000 for the same business date; otherwise `3000 × (window − days) / window`. |
| Counterparty hint | 2000 | 2000 when both references are non-empty and equal. |

The best-scoring ledger transaction matches when its score is at least
`fuzzy_threshold_bps` (default 8000: exact amount on the same date) and no other
candidate has the same top score. A score below the threshold means no match; an
exact tie between the top two candidates also means no match. The score is
stored in `matches.confidence`.

### Complexity

Ledger transactions are bucketed by UTC value date, so a run is
`O(E · (2·window+1) · L_day + L)`; only in-window day buckets are scanned.

## Pass 5: duplicates

After passes 1 to 4, the remaining records are grouped by

- `(source, source_record_id)` — a repeated external id, and
- `(currency, amount_minor, occurred_at)` for charges — the classic duplicate
  charge.

Each group of two or more becomes one `duplicate` break candidate containing all
its `(source, source_record_id)` references. Duplicate detection never writes a
match, even when a plausible ledger transaction exists. Group keys are visited
in sorted order, so the break set is deterministic; signatures dedupe any group
reported by both signals.

### Complexity

`O(E)` grouping plus `O(B log B)` for the deterministic ordering of B breaks.

## Break candidates and signatures

Passes that deliberately refuse to match a record implement `BreakPass` and
return `match.BreakCandidate` values:

- every payout still unmatched after pass 3 → `external_only` (covers ambiguous
  subsets and exceeded caps),
- every duplicate group after pass 5 → `duplicate`.

`BreakCandidate.Signature()` (D-5) is the canonical SHA-256 over scope id, side,
sorted ledger txn ids, sorted `(source, source_record_id)` pairs, currency, and
`delta_minor`. It excludes the run id, detection time, and status, so a rerun
produces the same signature and M1-14's `ON CONFLICT (signature) DO NOTHING` is
idempotent. Break candidates are de-duplicated by signature and sorted before
the engine returns them in `RunResult.Breaks`.

## Configuration

`match:` in the YAML config (defaults in `internal/config/match.go`):

| Key | Default | Meaning |
|---|---|---|
| `fee_tolerance_minor` | USD/EUR/GBP 100, SEK 1000 | Per-currency pass-2 and fuzzy amount tolerance. |
| `composite_max_legs` | 8 | Maximum legs in one composite match. |
| `composite_max_nodes` | 100000 | Subset-sum nodes per payout. |
| `composite_candidate_limit` | 256 | Maximum subset-sum pool. |
| `composite_date_window_days` | 7 | Leg value-date distance around the payout. |
| `fuzzy_threshold_bps` | 8000 | Minimum fuzzy score to match. |
| `fuzzy_date_window_days` | 3 | Fuzzy value-date distance. |

Zero values select the defaults; negative values and a threshold above 10000 are
rejected. The whole matcher configuration is canonicalized and hashed into
`match_runs.config_hash`, so every run records the policy that produced it. The
engine always writes exactly one `system:matcher` audit summary with the run's
counts, including break counts by side.

## Benchmark

`go test -run '^$' -bench BenchmarkMatch100k -benchmem -benchtime=1x ./internal/match`
runs 100k external records and 94k ledger transactions through all five passes
in one scope (80k exact, 5k tolerance, 2k declared composites with 4k member
records, 5k fuzzy, 4k duplicate records). The M1-13 acceptance bound is under
60 s; the development-laptop result is recorded in the task report in
`.team/runs/`.
