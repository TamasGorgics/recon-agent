# Agent quality dashboard (M5-05)

This page is the operator/developer reference for the M5-05 Grafana dashboard
`Recon Agent Quality` (`uid: recon-agent-quality`) and the seven `v_eval_*`
views behind it. `docs/spec.md` §13.2 is canonical; this page describes the
delivered shape and its boundaries. The unresolved spec/docs conflicts and the
accepted residuals are recorded as Q-103 in `docs/open-questions.md`.

The dashboard is **dev-only**, provisioned from files in `deploy/`, and reads
Postgres through the same dedicated read-only login as the M5-04 ops dashboard
(`recon_dashboards_ro`). It is not a production quality-monitoring deployment
and does not add an application service, datasource, or compose service. Panel
data comes from the synthetic `recon demo quality` seed: it is **synthetic demo
evidence, not a production eval or M2–M4 pipeline run** (see
[Synthetic data boundary](#synthetic-data-boundary)).

## Bring-up

Prerequisites: Docker Compose, Go 1.27, and the repository's `Makefile`
defaults (the compose Postgres is
`postgres://recon:recon@localhost:5432/recon`).

```sh
docker compose up -d --wait
make migrate
make demo-quality        # M1 + ops + quality synthetic rows (destructive reset)
./scripts/verify-dashboard.sh
```

`make demo-quality` runs `recon demo quality --seed 1 --days 14 --difficulty
realistic --reset`. It is a strict superset of `make demo-ops`: M1 base layer,
then the M5-04 ops layer when absent, then the M5-05 quality layer. Use it when
you want both dashboards populated. `make demo-ops` alone leaves the quality
panels empty; `recon demo seed` alone populates only the M1 dataset.

The existing-volume login note from `docs/dashboards.md` applies unchanged: an
old `recon-pgdata` volume does not rerun `docker-entrypoint-initdb.d`, so rerun
`compose/initdb/01-roles.sql` by hand and `make migrate`, or recreate the
volume. The M5-05 migration assumes `recon_dashboards_ro` exists and fails
loudly if it does not.

If a long-running Grafana container does not see a newly added
`deploy/grafana/dashboards/agent-quality.json`, restart Grafana once
(`docker compose restart grafana`) to refresh the bind mount. Fresh compose
startup is the normal path; the restart is an environment action, not a JSON
defect.

## Dashboard and panels

| Field | Value |
|---|---|
| UID | `recon-agent-quality` |
| Title | `Recon Agent Quality` |
| Folder | `Recon` (from the existing file provider) |
| Datasource | `recon-postgres` |
| Timezone / range / refresh | `utc` / `now-14d`..`now` / `5m` |
| Panels | 8, stable IDs 1–8, no template variables |

Every panel query reads one `v_eval_*` view, selects `day AS time` (except the
reason table), and filters with exactly one `$__timeFilter(day)`. No raw table,
no `COALESCE` on a ratio, no zero-fill, and no other Grafana macro or template
variable is used. Empty results render as “No data”.

| ID | Title | Type | Source | Unit | Notes |
|---|---|---|---|---|---|
| 1 | Accuracy by model / prompt | timeseries | `v_eval_accuracy` | percentunit | Completed, non-repeat `agent`/`e2e` runs; injection excluded. A zero/absent denominator is NULL, never zero. |
| 2 | False-fix rate by model / prompt | timeseries | `v_eval_false_fix` | percentunit | Same run filters, all splits including injection. Separate denominator from panel 1. |
| 3 | Human override rate | timeseries | `v_eval_override_rate` | percentunit | Rejections over terminal approved+rejected decisions. No edit event is inferred; missing agent runs group as `unknown`. |
| 4 | Apply-failure rate | timeseries | `v_eval_apply_failure_rate` | percentunit | Strict `apply_failed / (applied + apply_failed)` terminal denominator; `applied_at`, latest apply-failed audit, then `created_at` attribution. |
| 5 | Average cost per case | timeseries | `v_eval_cost_latency` | currencyUSD | Per-case `cost_usd`, not per-run or provider-billed total. |
| 6 | Latency p50 / p95 | timeseries | `v_eval_cost_latency` | ms | Two targets (`A` p50, `B` p95) with distinct legends; nearest-rank `percentile_disc`, never interpolated. |
| 7 | Escalation reasons | table | `v_eval_escalations` | short | `sum(count)` grouped by reason over the range; empty/missing reason is `unknown`; events, not distinct cases. |
| 8 | Escalation rate | timeseries | `v_eval_escalation_rate` | percentunit | Creation-day cohort rate. Panel description states the right-censoring and non-event-day caveat. |

## Views and semantics

The M5-05 migration `20261125120000_m5_05_eval_views.sql` creates seven
owner-rights views. Together with M5-04's five `v_ops_*` views they are the
**twelve** dashboard-facing views. All `day` values are UTC midnight
(`date_trunc('day', <ts> AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`); ratios are
`NULL` when the denominator is zero and are never `COALESCE`d to zero; counts
are `bigint`.

| View | Grain | Source and filters |
|---|---|---|
| `v_eval_accuracy` | run-start UTC day, model, prompt | Completed (`ended_at IS NOT NULL`), non-repeat (`repeat_group IS NULL`), `suite IN ('agent','e2e')`, `split <> 'injection'` runs. `correct` counts `correct IS TRUE`; every joined case counts in `total`; a NULL/false correctness value is not correct. |
| `v_eval_false_fix` | run-start UTC day, model, prompt | Same run filters; **all splits**, including injection. `false_fix` counts `false_fix IS TRUE`; rate uses the same case total. |
| `v_eval_cost_latency` | run-start UTC day, model, prompt | Same run filters as false-fix. `avg_cost_usd`/`total_cost_usd` from per-case `cost_usd`; `avg_latency_ms`, `p50_latency_ms`, `p95_latency_ms` from per-case `latency_ms`; percentiles are nearest-rank `percentile_disc(0.50/0.95)` cast to double precision. |
| `v_eval_override_rate` | decided UTC day, model, prompt | `approvals -> proposals -> LEFT JOIN agent_runs`; only `approved`/`rejected` decisions with non-NULL `decided_at`; pending rows excluded from both sides; missing run groups as `unknown` without losing the decision. |
| `v_eval_apply_failure_rate` | attempt UTC day, model, prompt | Applied proposals with `applied_proposals.applied_at`; `apply_failed` proposals timed by the latest `proposal.transition` audit with `after.status = 'apply_failed'`, falling back to `proposals.created_at`. All other statuses excluded from both sides. |
| `v_eval_escalations` | audit-event UTC day, reason | `audit_log` rows with `action = 'case.transition'` and `after->>'status' = 'escalated'`; reason is `after->>'reason'`, with empty/missing folded to `unknown`; counts events. |
| `v_eval_escalation_rate` | case-creation UTC day | Denominator: distinct cases created that day. Numerator: distinct cases from that same creation-day cohort with any escalation transition in their audit history, regardless of event day. Days with cases but no escalations emit a measured zero; days with no cases emit no row. |

Accuracy and false-fix deliberately use **different** split filters: injection
cases exercise false-fix safety but are not ordinary accuracy. Repeat-group
runs, unfinished (`ended_at IS NULL`) runs, and non-`agent`/`e2e` suites
(notably `matcher`) are excluded from all three eval views; repeat runs remain
available to the eval report and are not pooled into these denominators.

Grants: `recon_dashboards_ro` has `SELECT` on the views only. `PUBLIC` is
revoked; there is no base-table, sequence, function, or `v_agent_*` grant.
Direct base-table reads through the datasource fail with SQLSTATE `42501`.
The M5-05 Down revokes and drops only its seven views; the role, schema usage,
and five `v_ops_*` views survive.

## Seed and reset

`recon demo quality` (and `make demo-quality`) writes a deterministic quality
layer on top of the M1 and M5-04 ops layers:

- 2 models (`demo-model-a/b`), 2 prompt versions (`demo-prompt-v1/v2`),
  suites `agent`/`e2e`, splits `dev`/`test`/`injection`, across 7 UTC days
  inside the dashboard's 14-day window.
- 13 eval runs / 64 case results on the documented seed, including
  repeat-group, unfinished, and `matcher` exclusion runs whose distinctive
  values never appear in the views, plus injection rows that appear in
  false-fix/cost/latency but not accuracy.
- 12 production cases / 10 agent runs / 10 proposals / 5 approvals
  (3 approved, 2 rejected) / 6 applied / 2 `apply_failed`, with one failed
  audit timestamp on a later UTC day than `created_at` to exercise attribution.
- 4 `case.transition` escalation events with reasons `policy` (×2),
  `low_confidence` (×1), and a missing reason that renders as `unknown`.
- Every quality row is recorded in the durable `demo_metadata` marker table in
  the same transaction as the write; where the schema has a text field it also
  carries a `demo-quality:` prefix or the `demo-quality-v1` dataset version
  (proposal idempotency keys, agent-run trace IDs, approver API keys, approval
  channel, eval scenario IDs). The apply-failed and escalation audits are
  written through the internal audit writer so the audit chain stays valid.

Without `--reset`, any `eval_runs` row or demo-quality marker aborts before
any write, and the command exits non-zero. The M1, ops, and quality layers are
all-or-nothing because the whole seed is one `READ COMMITTED` transaction that
takes `EXCLUSIVE` locks on every seed-target table (24 tables today) before the
presence checks and the marker snapshot: a failure at any layer rolls back
exactly to the state at transaction start, so a failed run never leaves a
partial superset. That rollback does not undo an explicit `--reset`:
`make demo-quality` resets first, so a failed seed after a reset leaves the
migration-only database. There is no compensating reset and no unguarded
fallback. Concurrent writers are serialized: a writer that commits before the
locks are acquired is visible to the presence checks (any existing `eval_runs`
row or demo-quality marker aborts with no writes) and to the baseline snapshot,
so its rows are never adopted; a writer that starts after the locks blocks until
the seed commits. A real row can therefore never be adopted into `demo_metadata`
or be deleted by a later `--reset`. An explicit `--reset` remains a separate
guarded destructive step. A database seeded before the `demo_metadata` marker
migration still needs a one-time manual/fresh-database wipe; the guard must not
be bypassed.

`--reset` (and every other demo `--reset`) runs the all-tables guard. It
inspects every table in both schemas the reset destroys (`public` and
`eval_truth`) and aborts with `*UntaggedDataError` (matching
`ErrQualityUntaggedData`) if any row is not provably demo-owned. Allowed rows
are the reproducible migration reference rows (`currencies`, `periods`,
`system_switches`), rows recorded in `demo_metadata` by the M1/ops/quality
seed layers, and the `goose_db_version`/marker infrastructure. That covers
`cases`, `case_breaks`, `breaks`, `matches`, `match_legs`, `match_runs`,
`ledger_txns`, `ledger_entries`, `external_records`, `settlement_batches`,
`statement_balances`, `reconciliation_scopes`, `audit_log`, `audit_chain_head`,
`outbox`, `approval_messages`, `applied_proposals`, policy/config tables, and
`eval_truth`; anything untagged aborts before any DDL.

The guard query runs in the same transaction that takes `ACCESS EXCLUSIVE`
locks on all dropped tables and performs the drop, so a concurrent insert
either commits before the lock and aborts the reset or blocks until the schema
is gone; nothing can be destroyed between check and drop. M1 rows have no
per-row marker column, so migration `20261125130000_m5_05_demo_metadata.sql`
adds the `demo_metadata` table; the M1 pipeline records the rows it creates,
and the ops/quality layers record theirs in the same transaction as the writes.
A database seeded before that migration has unmarked rows and the guard will
refuse to destroy it: use a fresh/disposable database or a one-time manual
wipe. The command refuses `RECON_ENV=production`.

This is a **full schema reset rather than a tagged-only delete** because
`audit_log` is append-only: a tagged delete cannot remove prior escalation
history, so repeated reseeds would double the reasons panel. The guard is the
safety control, and it makes the destructive path abort on untagged evidence
instead of silently wiping a shared database.

The seed uses no network, no LLM, and no API key. It prints
`SYNTHETIC DEMO DATA — NOT PRODUCTION EVIDENCE` at start and end and reports
counts only, never UUIDs.

## Verification

`./scripts/verify-dashboard.sh` is the repeatable live check for both
dashboards. It is read-only and requires the compose stack plus `make migrate`.
After `make demo-quality` it requires non-empty data, not just successful
queries. It checks:

1. `docker compose config -q`;
2. Grafana `/api/health` and Prometheus `/-/ready`;
3. both provisioned datasource UIDs and their health;
4. both dashboards (`recon-ops`, `recon-agent-quality`) are provisioned in
   folder `Recon` with version ≥ 1;
5. every panel target executes through `/api/ds/query` and returns at least
   one frame with non-zero rows; the quality dashboard's escalation-reasons
   table must have at least three reasons; an unexpanded `$__` macro fails;
6. anonymous dashboard access returns HTTP 401;
7. a base-table `SELECT` through the datasource is denied with SQLSTATE
   `42501`.

On the documented seed the live run observed quality panel rows
**7 / 7 / 5 / 7 / 7 / 7 / 3 / 7** and ops panel rows **1 / 4 / 7 / 3 / 6 / 1**.
Row counts follow the data and the time window; they are evidence for this
seed, not a permanent invariant.

Static and integration coverage lives in `internal/dashboards` (view fixtures
and grants, dashboard JSON/identity/query contract, provisioning discovery,
panel execution through the real dashboard login) and `internal/demo` (seed
determinism, fail-closed/reset, layering, no network, audit-chain validity).
Useful commands:

```sh
go test ./internal/dashboards ./internal/demo -count=1
TEST_DATABASE_URL=postgres://recon:recon@localhost:5432/recon?sslmode=disable \
  go test -tags=integration -count=1 ./internal/dashboards ./internal/demo
```

The verifier reads admin credentials from `RECON_GRAFANA_ADMIN_USER` /
`RECON_GRAFANA_ADMIN_PASSWORD` and URLs from `RECON_GRAFANA_URL` /
`PROMETHEUS_URL`, with the same local-only defaults as compose. It stores no
secret.

## Synthetic data boundary

`recon demo quality` is **synthetic dashboard demo evidence**, not a production
M2–M4 pipeline run and not production eval evidence. It runs no agent,
policy/approval service, executor, or live LLM; it writes deterministic demo
rows through controlled database paths. It must not be cited as proof that the
production reconciliation pipeline ran, that a tie-out succeeded, that the
audit chain is production-healthy, or that a model/prompt has a production
quality level.

The data is deterministic for a pinned clock, or for the same seed and reset
database on the same UTC day; a seed straddling 00:00 UTC can produce different
date buckets. The generated worlds and eval baseline artifacts are not
republished by this seed. Q-98/Q-102 boundaries for the ops dashboard apply to
the shared stack; Q-103 records the M5-05 boundaries.

## Visual check: PENDING (human)

Automated evidence proves the stack is healthy, both dashboards are
provisioned, every panel query executes, and the seeded panels are non-empty.
It does **not** prove visual readability (titles, units, legends, colors,
sizing, or the rendered p50/p95 distinction). The human visual check is
**PENDING**: open <http://127.0.0.1:3000/d/recon-agent-quality>, log in with the
local admin credentials, and inspect these eight panels in the `Recon` folder:

1. Accuracy by model / prompt
2. False-fix rate by model / prompt
3. Human override rate
4. Apply-failure rate
5. Average cost per case
6. Latency p50 / p95
7. Escalation reasons
8. Escalation rate

Do not record it as passed until a human confirms it.

## Out of scope

- M5-06 daily integrity job and its real status metric; no alerting.
- M6 retention, redaction, PAN scan, or production quality claims.
- New compose services, Grafana/Prometheus containers, datasources, or roles.
- Grafana/Prometheus authentication, TLS, retention, or non-loopback exposure
  beyond the existing dev boundary.
- Changing the pre-existing Postgres host publish binding or dev passwords.
- An edit-override event; the product has none, so the override metric remains
  rejection-based.
- Changes to `docs/spec.md`; the unresolved spec/docs conflicts are recorded in
  Q-103 instead.
