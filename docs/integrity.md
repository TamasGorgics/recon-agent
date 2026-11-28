# Daily integrity job (FIN-52)

The daily integrity job is an independent, read-only control over the data the
reconciliation pipeline produced. It runs five checks over one
`REPEATABLE READ` read-write transaction — a single database snapshot —
persists exactly one completed result row in that transaction, and returns a
stable machine-readable report. It does not call the matcher or executor
services,
does not import `internal/match`, and never repairs data. The checks are
evidence that no inconsistency was detected — not proof that the database
contains the expected data or that every scope has statement coverage.

The job always runs as the production application role `recon_app`. It needs
`SELECT` on the read tables and `INSERT` on `integrity_runs` and `outbox`; it
has no owner/superuser path. That is what makes a successful CLI or worker run
a proof of the production grants.

## Command

```text
recon integrity run [--date YYYY-MM-DD] [--json]
```

- **`--date`** is parsed strictly as `YYYY-MM-DD`. It sets `run_date` and the
  upper bound used to pick each tie-out statement (`as_of_date <= run_date`).
  It is **not a historical replay**: the other four checks always describe the
  current database state, and a backdated date does not rewind matcher,
  ledger, or audit state.
- The default `run_date` is the process wall-clock UTC date.
- The command opens only `DATABASE_APP_URL` (`recon_app`). It never falls back
  to the admin/owner `DATABASE_URL`; a missing app URL is a config error
  (exit 2).
- The row is written with `triggered_by='manual'`. The worker scheduler writes
  `'schedule'`.
- **Exit codes:** `0` completed run with `status=passed`; `1` completed run with
  `status=failed` (the JSON report is still printed and remains valid); `2`
  usage/invalid date/config/connection/advisory-lock/DB/cancellation before a
  completed row. Exit 2 never claims a report.
- **`--json`** prints the report envelope below. Without it, the human output
  prints run id/date/status, one line per check (status, checked, skipped,
  problem count), and up to five distinct problem codes; it does not dump every
  raw detail.

A failed manual run also enqueues the `integrity.failed` alert in the same
transaction as its row. If `cmd/outbox` is not running, the outbox row stays
durable until it is.

## Five checks

The order is frozen and every report always contains exactly these five
results:

| # | Check | Evaluates | Problem codes |
|---|---|---|---|
| 1 | `ledger_balance` | every `ledger_txns` row, per transaction and currency | `ledger_txn_unbalanced`, `ledger_txn_no_entries` |
| 2 | `value_conservation` | every `source + currency` key in the union of ingested records and active-match scopes | `value_conservation_mismatch` |
| 3 | `applied_proposal_trail` | `count(*) FROM applied_proposals`, using the stable `internal/invariants` contract unchanged | the 14 invariant codes, e.g. `applied_missing_ledger`, `executor_applied_audit_missing` |
| 4 | `audit_chain` | the full audit hash chain from sequence 1 | `audit_chain_problem` |
| 5 | `tieout` | every `reconciliation_scopes` row whose latest statement is at or before `run_date`; scopes without a statement are skipped | `tieout_unexplained`, `tieout_error` |

Any check that cannot complete its own work records a `check_error` problem
with a bounded cause (`query_failed`, `numeric_overflow`, `invariants_failed`,
`verify_failed`, `count_failed`, `scope_query_failed`, `statement_query_failed`,
`savepoint_failed`, …). The check's savepoint is rolled back, the remaining
checks still run, and the run still persists a failed row. A check error is a
failed check, not a missing run.

- **`ledger_balance`** independently sums `debit − credit` per
  `(txn_id, currency)`; a non-zero net is `ledger_txn_unbalanced` with
  `currency=<code> net_minor=<signed>`. A transaction with no entries is
  `ledger_txn_no_entries` with `entries=0`. The database's deferred
  `ledger_assert_balanced` trigger is left in place; this check is independent
  evidence for restored data, disabled triggers, or a future trigger bug.
- **`value_conservation`** verifies the FIN-52 equation per `source` and
  `currency`:

  ```text
  Σ active match_legs
  + Σ external records with no active match header
  = Σ all external records
  ```

  It uses signed minor units, only active match headers and legs, and
  attributes matched value through the match's scope. There is no `abs()`, no
  currency netting, no tolerance exemption, and no source allowlist. A
  mismatch reports `ingested_minor`, `accounted_minor`, and
  `delta_minor = ingested − accounted`. A numeric value outside `int64` is a
  `numeric_overflow` check error, never a silently truncated amount.
- **`applied_proposal_trail`** is a documented superset of FIN-52: it maps
  every `internal/invariants` finding one-for-one (same stable code, entity,
  id, bounded detail). It also requires each applied proposal's ledger
  transaction, executor/transition/resolution audit rows, resolved
  break/case, and `proposal.applied` outbox row. Invariant code strings are
  never renamed by the job.
- **`audit_chain`** calls the read-only `audit.Verify` from genesis. Problems
  contain only the finding kind and sequence (plus a bounded head sequence),
  never an audit payload. The job writes no audit row.
- **`tieout`** computes FIN-54 for every scope at the latest
  `statement_balances.as_of_date <= run_date`. A scope with no statement is
  *skipped*, not failed. A non-zero unexplained amount is
  `tieout_unexplained` with the scope, as-of date, and signed amounts; a scope
  whose computation fails is `tieout_error` and the other scopes still run.
  There is no `psp_a`/`bank_x` special case.

## Report shape

`--json` and the persisted `checks` use the same stable envelope:

```json
{
  "run_id": "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b",
  "run_date": "2026-11-26",
  "started_at": "2026-11-26T02:00:00Z",
  "finished_at": "2026-11-26T02:00:03.412Z",
  "status": "failed",
  "check_count": 5,
  "failure_count": 2,
  "checks": [
    {
      "name": "value_conservation",
      "status": "failed",
      "checked": 3,
      "skipped": 0,
      "problem_count": 1,
      "truncated": false,
      "problems": [
        {
          "code": "value_conservation_mismatch",
          "entity": "source_currency",
          "id": "psp_a:EUR",
          "detail": "ingested_minor=1000000 accounted_minor=999998 delta_minor=-2"
        }
      ]
    }
  ]
}
```

Rules:

- `run_id` is the `integrity_runs.id`; it is added by the CLI/worker envelope
  and is not repeated inside the persisted `checks`.
- `run_date` is a UTC business date rendered as `YYYY-MM-DD`, never an RFC3339
  timestamp. Timestamps are RFC3339 UTC with optional fractional seconds and
  `finished_at >= started_at`.
- `status` is only `passed` or `failed` at run and check level. There is **no
  `skipped` status**; a check that skips inputs still passes and carries the
  count in `skipped`.
- `check_count` is always 5 and equals the length of `checks`.
- `failure_count` is the number of failed checks, not the number of problem
  rows. Run status is `failed` iff `failure_count > 0`.
- `checked` is the number of inputs evaluated: ledger transactions, value
  keys, applied proposals, audit rows hashed, or evaluated tie-out scopes.
  `skipped` is meaningful only for `tieout` and is 0 elsewhere.
- `problem_count` is the true pre-cap count. `problems` is sorted by
  `(code, entity, id, detail)` and capped at **100** entries per check;
  `truncated` is true iff `problem_count > len(problems)`.
- Every field is always encoded, including zero values and empty arrays, so
  consumers can rely on the shape. Problem `detail` is bounded single-line
  operator text (at most 256 characters) and never contains raw external
  memos/payloads, audit payloads, PAN-like values, DSNs, SQL parameters, or
  stack traces.
- Additive fields may be added later. Consumers should use `name`, `status`,
  and `problem_count` for summaries and must not parse `detail` for machine
  logic.

## Storage, append-only history, and the latest run

Results live in `integrity_runs` (migration
`20261126120000_m5_06_integrity_runs.sql`): `id`, `run_date`, `started_at`,
`finished_at`, `status`, `check_count`, `failure_count`, `checks` (JSONB),
`triggered_by` (`schedule`/`manual`), with the status/count/length/date
constraints and `(run_date, started_at DESC)` index. There is deliberately no
`running` status, no lease column, and no unique `run_date`: the table is
append-only completed evidence.

`recon_app` has `SELECT` and `INSERT` only; `UPDATE` and `DELETE` are revoked,
and no other runtime role is granted access. An operator rerun inserts a second
row for the same date. A process crash, timeout, cancellation, lock loser, or
persistence failure leaves **no** row.

To read the latest completed attempt for a date, use the documented query; do
not assume one row per day:

```sql
SELECT id, run_date, started_at, finished_at, status, check_count,
       failure_count, checks, triggered_by
  FROM integrity_runs
 WHERE run_date = $1
 ORDER BY started_at DESC, id DESC
 LIMIT 1;
```

A later manual pass may coexist with an earlier scheduled failure. A period
close (M5-09) must require a **passed** run appropriate to the period and must
also inspect tie-out `checked`/`skipped` coverage; it must not infer success
from "no failed run" or from one latest row alone. Retention is indefinite in
M5-06: there is no cleanup job. M5-07 owns its own read-only grant for this
table; do not broaden `recon_app`'s write grants.

## Scheduling in the worker

Long-running `cmd/worker` runs one scheduler goroutine alongside the case poll
loop; `worker --once` stays case-only and starts no scheduler.

- Fixed schedule: **02:00 UTC daily** (`DefaultRunAtUTC = 2h`), the UTC
  business date. There is no config key for time or disable in M5-06.
- Due logic uses the persisted `max(run_date) FROM integrity_runs WHERE
  triggered_by='schedule'`. If no scheduled row exists for today and now is at
  or after 02:00, it catches up once for the current UTC date. A row for today
  suppresses further scheduled runs; a **failed** scheduled run is the day's
  attempt (the manual CLI is the explicit rerun path and does not satisfy
  scheduled idempotency).
- Catch-up does **not** backfill missed days. An all-worker outage misses a day,
  and that missing row must be surfaced by M5-07/M5-09.
- There is **no leader election**. Every worker may run the scheduler; an
  in-memory attempted-date guard and the database row cover local and
  cross-restart duplicates, and the run's transaction-scoped advisory lock
  (key `7305060001`) makes exactly one run execute. A lock loser sleeps 30
  seconds and re-queries; it writes no row and changes no gauge.
- An operational error before a row exists is logged with a bounded message
  and retried with bounded backoff (30s, 1m, 5m, then 15m) until a row exists,
  the UTC date changes, or shutdown. A completed run that persisted a passed or
  failed row ends the day's attempts; it is not retried automatically until the
  next UTC date, and the manual CLI is the explicit rerun path.
- The scheduler is joined before the worker's database pool closes. `--once`,
  shutdown, and cancellation do not write an integrity row.

## Metric

`recon_integrity_job_status{job="integrity"}`:

| Value | Meaning |
|---:|---|
| `0` | Seeded/unknown; no completed run observed by this process. Never set at run start. |
| `1` | Last completed run passed. |
| `2` | Last completed run failed (any finding or `check_error`). |

The worker is the only producer. It sets the gauge **only after `COMMIT`** of a
completed run, through the `NewIntegrityObserver` adapter. It does not set `0`
at start or to erase a prior result, and it does not change the gauge on
`ErrAlreadyRunning`, shutdown/cancellation, a begin/query/insert/commit
failure, a config/connection error, or a panic — the previous value remains.
A worker restart reseeds the in-process registry to `0` until the next
completed run (normally the catch-up); `0` means unknown, not pass. The CLI has
no long-lived registry and produces no gauge. No metric name, type, unit, or
label changes in M5-06.

## Failure alert and deploy order

A failed run enqueues **one** durable `integrity.failed` outbox row in the same
transaction as its `integrity_runs` row. A passed run enqueues nothing. The
payload is a bounded summary only — run id/date, status, counts, failed check
names in frozen order, per-check problem counts, and trigger — with no checks
array, problem details, entity ids, amounts, or raw external content. The
idempotency key is `integrity.failed:<run_id>`, so a retry cannot duplicate the
enqueue.

`cmd/outbox` registers four topics — the three approval topics plus
`integrity.failed` — and the new `integrity.failed` handler posts one plain-text
message (run id, date, status, failed checks/counts, and a pointer to
`recon integrity run --date <date> --json`). Delivery is at-least-once;
Slack has no idempotency key, so duplicate messages are accepted. There is no
`notified_at` column.

**Deploy order:** ship `cmd/outbox` with the handler **before** the worker
scheduler starts producing. An unregistered topic dead-letters immediately and
is not retried, so an alert produced before the handler exists is lost as a
delivered alert (the row itself remains).

The frozen outbox metric topic label set does not include `integrity.failed`;
its lag/dead-letter series fold to `unknown`. The handler still works. This is
a Q-104 residual, not a reason to change the label contract here.

## Reading results: the vacuous-pass rule

A **passed** report means "no inconsistency was detected", not "the database
has data" or "every scope has a statement".

- An empty database passes all five checks vacuously (`checked=0`).
- An all-skipped tie-out check is `status=passed` with `checked=0` and
  `skipped>0`. It is not evidence of tie-out coverage.
- A **missing** daily row is not a pass. M5-07/M5-09 must detect absence
  explicitly and must inspect `checked`/`skipped` and statement coverage, not
  just run `status`.
- The persisted row is authoritative. If the process dies after `COMMIT` but
  before the gauge update, the row exists and the gauge is stale until the next
  completed run or a worker restart. A post-commit log/observer failure does
  not invalidate the row.

## Demo and clean-fixture caveat

The deterministic demo dataset is **red for integrity by default** and is not
clean evidence for FIN-52:

- the demo's tolerance/rounding matches leave small signed minor-unit
  differences per source and currency that `value_conservation` reports as a
  mismatch (no tolerance exemption; the live release run observed `psp_a`
  EUR `-2`, SEK `-3`, and USD `+2`), and
- `bank_x` opening balances leave unexplained amounts in four currencies
  (EUR, JPY, SEK, USD), so tie-out fails.

The job does not repair the demo data, exempt a tolerance, or special-case
`psp_a`/`bank_x`. Acceptance uses a purpose-built clean fixture in the
`internal/integrity` integration tests, not `demo seed`. If the demo must show
a green run, that is a separate fixture/data change (AQ-4 / Q-104), not a job
change.

## Demo reset interaction

`recon demo --reset` (and the `demo ops`/`demo quality` reset paths) drops both
`public` and `eval_truth` schemas and replays migrations. Consequences:

- **The reset drops `integrity_runs` history.** Run results are not preserved
  across a demo reset and cannot be recovered from the database afterwards.
- The M5-05 reset guard has one named exception for `integrity_runs`: rows in
  that table are operational evidence produced by running the job against the
  demo database, not untagged user data. No other table is exempted and there
  is no wildcard.
- **Serialize demo reset and the worker.** The reset holds its own locks and
  the scheduler uses a different (transaction-scoped advisory) lock. Do not run
  the worker scheduler concurrently with a demo reset; stop the worker first.
- A **failed** demo run also writes an `integrity.failed` outbox row. `outbox`
  is not exempted by the guard, so that untagged alert row can still block a
  reset until the outbox drains/handles it. M5-06 deliberately did not weaken
  the outbox guard (AQ-3b); this is recorded in Q-104.

## Trust boundary and residual risks

- **Not tamper-evident.** `recon_app` holds `INSERT` on `integrity_runs`, so any
  process with the app role can insert a fake `passed` row directly through
  SQL. There is no audit row, signature, or MAC for a run, and the owner/
  superuser can mutate append-only rows. Grants are the M5-06 boundary; the
  results are operational evidence, not forensic proof. Future hardening
  (append-only trigger/separate role/run signature) is out of scope.
- **No `running` row.** A hard timeout (5-minute package constant), context
  cancellation, crash, lock loss, or persistence failure leaves no row and no
  alert. Consumers must treat a missing daily row as missing evidence.
- **Commit-before-metric window.** The gauge is set only after commit, by
  design; a crash in between leaves a stale gauge and an authoritative row.
- **Cost/retention.** The audit chain is verified in full every run; this cost
  grows with the chain. Rows are retained indefinitely. Incremental audit
  verification, retention, and a reporting-role grant are Q-104 items for
  later tasks.
- **Date semantics.** `--date` scopes only the tie-out as-of; it is not a
  historical replay. M5-07/M5-09 must not present it as one.
- **Aggregate value, not per-match correctness.** `value_conservation` is
  computed per `source + currency`, not per match. An equal-and-opposite
  cross-scope pair can therefore cancel and pass (Q-104 N1), and
  `match_legs.currency` is not compared with the scope currency — the leg
  amount is attributed to the scope key (Q-104 N2). A pass is evidence about
  aggregate value, not match-level correctness.

Q-104 in [`docs/open-questions.md`](open-questions.md) records every residual
above, including the reset/outbox interaction, tie-out as-of and skip rules,
value sign/tolerance semantics, outbox deploy order and topic-label folding,
scheduler behavior, append-only/latest selection, metric mapping, demo red,
trust boundary, timeout/cancel absence, and the migration/spec delta.
