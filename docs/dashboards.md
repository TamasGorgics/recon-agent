# Dashboards (M5-04 ops, M5-05 agent quality)

This page is the operator guide for the M5-04 Grafana/Prometheus stack and both
provisioned dashboards: `Recon Ops` (`uid: recon-ops`, spec §13.1) and
`Recon Agent Quality` (`uid: recon-agent-quality`, spec §13.2). It covers the
shared datasources, read-only role, view surface, bring-up/reset, verification,
and security defaults; the quality panel catalog and detailed view semantics
are in [`docs/agent-quality.md`](agent-quality.md). The accepted boundaries and
residuals are recorded as Q-102 (ops) and Q-103 (agent quality) in
`docs/open-questions.md`.

Both dashboards are **dev-only**. They are provisioned from files in `deploy/`,
read Postgres through a dedicated read-only login that can select the twelve
dashboard-facing owner-rights views (five `v_ops_*` from M5-04 and seven
`v_eval_*` added by M5-05), and the ops dashboard shows a Prometheus
self-scrape as one supplementary panel. They are not a production monitoring
deployment, and they do not add application services to compose. The ops panel
data comes from the synthetic `recon demo ops` seed; the quality panel data
comes from `recon demo quality`, which includes the M1 and ops layers. Both are
**demo evidence, not a production M2–M4 pipeline run, eval result, or
audit-chain proof** (Q-98 / Q-102 / Q-103). The full boundary is under
[Synthetic demo-data boundary](#synthetic-demo-data-boundary).

## Bring-up

Prerequisites: Docker Compose, Go 1.27, and the repository's `Makefile`
defaults (the compose Postgres is `postgres://recon:recon@localhost:5432/recon`).

### Fresh Postgres volume

```sh
docker compose up -d --wait
make migrate
make demo-quality        # M1 + ops + quality synthetic rows (destructive reset)
./scripts/verify-dashboard.sh
```

`make demo-quality` populates both dashboards. `make demo-ops` is the ops-only
alternative: it leaves the quality panels empty, so the verifier (which now
requires non-empty seeded panels) is expected to fail until
`recon demo quality` is run.

`docker compose up -d --wait` starts Postgres, Phoenix, Prometheus, and Grafana
and waits for the healthchecks. No API, worker, outbox, or other application
service is added (M5-04 decision AQ-3). Grafana is published on
`http://127.0.0.1:3000` and Prometheus on `http://127.0.0.1:9090` by default;
`GRAFANA_PORT` / `PROMETHEUS_PORT` may change the host port only.

On a first-time volume the init script creates the five group roles and the
five local-only dev logins, including `recon_dev_dashboards_ro`. The M5-04
migration creates the five `v_ops_*` views and grants `SELECT` on them to
`recon_dashboards_ro`; the M5-05 migration adds the seven `v_eval_*` views and
grants `SELECT` on those too. The provisioned datasource uses environment
placeholders, so no manual datasource setup is needed.

### Existing Postgres volume

An existing `recon-pgdata` volume does **not** rerun `docker-entrypoint-initdb.d`.
If the dashboard datasource is unhealthy because the dashboard login is missing,
run the init script by hand (it is idempotent) and migrate:

```sh
docker compose exec -T postgres psql -U recon -d recon \
  -f /docker-entrypoint-initdb.d/01-roles.sql
make migrate
docker compose up -d --wait prometheus grafana
```

`docker compose down -v` followed by `make db-up` is the destructive
alternative; it deletes the dev Postgres volume, so do not use it without
explicit approval. Do not claim zero manual steps for existing volumes.

### Reset data

`make demo-quality` runs `recon demo quality --seed 1 --days 14 --difficulty
realistic --reset`. It drops and recreates both the `public` and `eval_truth`
schemas, replays migrations, seeds the M1 demo, seeds the synthetic ops layer
when it is not already present, and then seeds the quality layer. The three
layers are all-or-nothing because the whole seed is one `READ COMMITTED`
transaction that takes `EXCLUSIVE` locks on every seed-target table (24 tables
today) before the presence checks and the marker snapshot: a failure rolls back
exactly to the state at transaction start, with no compensating reset. That
rollback does not undo the reset; a failed seed after a reset leaves the
migration-only database. Concurrent writers are serialized: a writer that
commits before the locks are acquired is visible to the presence checks (an
existing `eval_runs` row or demo-quality marker aborts the seed with no writes)
and to the baseline snapshot, so its rows are never adopted as demo data and
survive any later guarded reset (which refuses the mixed database); a writer
that starts after the locks blocks until the seed commits. An explicit `--reset`
is a separate guarded destructive step; it is destructive and refuses to run
when `RECON_ENV=production`; it is the documented data path for both dashboards.
A database seeded before the `demo_metadata` marker migration needs a one-time
manual/fresh-database wipe; never bypass the guard.

`make demo-ops` (`recon demo ops ... --reset`) is the M1 + ops-only path; it
leaves the quality panels empty. `recon demo seed` alone repopulates only the
M1 dataset; the ops and quality panels then have no rows. A second run of either
`recon demo ops` or `recon demo quality` without `--reset` fails closed before
writing any row: `demo ops: ops data already present; rerun with --reset` and
`demo quality: eval_runs already contains rows or demo-quality rows exist;
rerun with --reset on a scratch database`.

`recon demo quality --reset` (and every other demo `--reset`) runs an
all-tables guard before the schema reset. The guard inspects **every table in
both schemas the reset destroys** (`public` and `eval_truth`), not only the
eval/approval tables. It allows only reproducible migration reference rows
(`currencies`, `periods`, `system_switches`), rows recorded in the
`demo_metadata` marker table by the M1/ops/quality seed layers, and the
`goose_db_version`/marker infrastructure. Any untagged row in `cases`,
`case_breaks`, `breaks`, `matches`, `match_legs`, `match_runs`,
`ledger_txns`, `ledger_entries`, `external_records`, `settlement_batches`,
`statement_balances`, `reconciliation_scopes`, `audit_log`, `audit_chain_head`,
`outbox`, `approval_messages`, `applied_proposals`, policy/config tables, or
`eval_truth` aborts with a typed error **before any DDL**. The guard query runs
inside the same transaction that takes `ACCESS EXCLUSIVE` locks on all dropped
tables and then drops the schemas, so a concurrent insert either commits before
the lock and aborts the reset or blocks until the schema is gone; it cannot be
silently destroyed between check and drop.

**Integrity run history.** The reset also destroys `integrity_runs`: run
results are not preserved across a demo reset and cannot be recovered. The
guard has one named exception for `integrity_runs` because those rows are
operational evidence produced by running the job against the demo database, not
untagged user data; no other table is exempted. `outbox` is **not** exempted,
so a failed demo integrity run leaves an untagged `integrity.failed` outbox row
that still blocks a reset until the outbox handles/drains it (AQ-3b / Q-104).
The reset and the worker scheduler use different advisory locks: stop the
worker before a demo reset (or otherwise serialize the two); never run them
concurrently.

M1 demo rows have no per-row marker column, so migration
`20261125130000_m5_05_demo_metadata.sql` adds `demo_metadata`; the M1 pipeline
records the rows it creates after a successful run, and the ops/quality layers
record theirs in the same transaction as the writes. A database seeded before
this migration has unmarked rows and the guard will refuse to destroy it: use a
fresh/disposable database or a one-time manual wipe (the rows are synthetic
demo data by definition). The reset is a **full dev-only schema reset rather
than a tagged-only delete** because `audit_log` is append-only: prior
escalation history cannot be removed by row deletes, and repeated reseeds would
otherwise double the escalation-reasons panel. This boundary and the unresolved
spec wording are recorded in Q-103; see
[`docs/agent-quality.md`](agent-quality.md#seed-and-reset).

Determinism is guaranteed only for the same seed and reset database on the
**same UTC day** (spec §6.3). The demo seeds read the clock once per run and
anchor their date buckets to that value, so two runs that straddle 00:00 UTC
can produce different bucket/date values. That is a low-probability
test-flake risk (the determinism tests compare date-bearing columns), not a
data-path change; tests inject a fixed clock, and a fixed-clock CLI injection
remains the future fix.

## Datasources and provisioning

| Datasource | UID | Type | Endpoint | Credential |
|---|---|---|---|---|
| Recon Postgres | `recon-postgres` | `grafana-postgresql-datasource` | `postgres:5432`, database `recon` | `RECON_DASHBOARDS_RO_USER` / `RECON_DASHBOARDS_RO_PASSWORD` env placeholders; default is the local-only `recon_dev_dashboards_ro` |
| Recon Prometheus | `recon-prometheus` | `prometheus` | `http://prometheus:9090` | none (loopback-only dev service) |

`recon-postgres` is the default datasource, is non-editable, and sets a UTC
session. `recon-prometheus` is non-default and uses a 15s interval. The
provisioning files are:

- `deploy/grafana/provisioning/datasources/postgres.yaml`
- `deploy/grafana/provisioning/datasources/prometheus.yaml`
- `deploy/grafana/provisioning/dashboards/provider.yaml` (folder `Recon`,
  file provider over `/var/lib/grafana/dashboards`, UI edits and deletion
  disabled, 30s reload)
- `deploy/grafana/dashboards/ops.json` (the `Recon Ops` dashboard)
- `deploy/grafana/dashboards/agent-quality.json` (the `Recon Agent Quality`
  dashboard)
- `deploy/prometheus/prometheus.yml` (self-scrape only)

Images are pinned exactly: `grafana/grafana:13.3.2` and
`prom/prometheus:v3.16.0`. The Grafana provisioning and dashboard directories
are read-only bind mounts. There is no one-shot init service and no Docker
socket mount.

## Panel catalog

Both dashboards are UTC and default to `now-14d`. `Recon Ops` refreshes every
30s and has stable panel IDs 1–6; `Recon Agent Quality` refreshes every 5m and
has stable panel IDs 1–8. Neither dashboard uses template variables.

### Recon Ops (`recon-ops`)

| ID | Title | Type | Datasource | Query source | Notes |
|---|---|---|---|---|---|
| 1 | Match rate by source | timeseries | `recon-postgres` | `v_ops_match_rate` | Match share per UTC day and source. An absent or non-number `stats->'matched'`/`external_candidates` counts as zero; a zero denominator renders NULL, not 0. A numeric but non-integer value (for example `1.5`) passes the `jsonb_typeof(...)='number'` guard and then fails the `::bigint` cast, so the query errors (fails closed) instead of counting zero. Percent unit. |
| 2 | Unmatched value by currency | table | `recon-postgres` | `v_ops_unmatched_value` | Exposure proxy: sum of absolute open/in-case break deltas per currency. `exposure_major` is display-only; minor units are the source of truth. No cross-currency total. |
| 3 | Open breaks by type and aging bucket | table | `recon-postgres` | `v_ops_break_aging` | Break type is `breaks.side`; canonical bucket order `0-1`, `2-3`, `4-7`, `>7`, `unknown`. |
| 4 | Approval queue | table | `recon-postgres` | `v_ops_approval_queue` | Pending/oldest-pending are query-time snapshots. Latency uses the earliest approved decision; any rejected decision suppresses latency (fail-closed presentation). |
| 5 | Resolution mix over time | timeseries (stacked bars) | `recon-postgres` | `v_ops_resolution_mix` | One terminal classification per case, precedence escalated > human > auto. `event_at` is `resolved_at`, or `created_at` for escalated cases. |
| 6 | Prometheus self-scrape | stat | `recon-prometheus` | `up{job="prometheus"}` | Supplementary availability check only; green at `1`. **Not live application metrics.** |

Panel 4 contains `$__timeFilter(approved_at)` inside a `FILTER (...)` clause.
It was executed through the real provisioned datasource on Grafana 13.3.2 and
expanded correctly to a `BETWEEN` predicate; no query rewrite was needed. Only
the three approved macros (`$__timeFilter(bucket_start)`,
`$__timeFilter(approved_at)`, `$__timeFilter(event_at)`) appear in the ops
dashboard SQL.

### Recon Agent Quality (`recon-agent-quality`)

Every quality panel reads one `v_eval_*` view and filters with exactly one
`$__timeFilter(day)`. No raw table, `COALESCE` ratio, zero-fill, or template
variable is used.

| ID | Title | Type | Query source | Unit | Notes |
|---|---|---|---|---|---|
| 1 | Accuracy by model / prompt | timeseries | `v_eval_accuracy` | percentunit | Completed, non-repeat `agent`/`e2e` runs; injection excluded. |
| 2 | False-fix rate by model / prompt | timeseries | `v_eval_false_fix` | percentunit | Every split, including injection; separate denominator from accuracy. |
| 3 | Human override rate | timeseries | `v_eval_override_rate` | percentunit | Rejections over terminal decisions; no edit event inferred. |
| 4 | Apply-failure rate | timeseries | `v_eval_apply_failure_rate` | percentunit | Strict `apply_failed / (applied + apply_failed)` terminal denominator. |
| 5 | Average cost per case | timeseries | `v_eval_cost_latency` | currencyUSD | Per-case `cost_usd`, not a per-run total. |
| 6 | Latency p50 / p95 | timeseries | `v_eval_cost_latency` | ms | Two targets (p50/p95), nearest-rank `percentile_disc`. |
| 7 | Escalation reasons | table | `v_eval_escalations` | short | `sum(count)` by reason; empty/missing reason is `unknown`. |
| 8 | Escalation rate | timeseries | `v_eval_escalation_rate` | percentunit | Creation-day cohort rate; right-censoring caveat in the panel description. |

The full view/panel semantics, seed/reset boundary, verifier details, and
visual PENDING checklist are in [`docs/agent-quality.md`](agent-quality.md).

## Read-only role and views

`recon_dashboards_ro` is a `NOLOGIN` group role. It is not a member of
`recon_app`, `recon_executor`, `recon_agent_ro`, or `recon_audit_ro`; it has no
superuser, database-creation, role-creation, or replication attributes. Its
role-level settings are `default_transaction_read_only=on` and
`statement_timeout=30s`. The local login
(`recon_dev_dashboards_ro`) sets the same settings directly, because
PostgreSQL does not inherit a group role's `rolconfig` at connection time.

The database surface is the twelve owner-rights views below, with `SELECT`
granted on the views only:

| View | Grain |
|---|---|
| `v_ops_match_rate` | one row per UTC day and source |
| `v_ops_unmatched_value` | one row per currency |
| `v_ops_break_aging` | one row per open/in-case break |
| `v_ops_approval_queue` | one row per proposal |
| `v_ops_resolution_mix` | one row per classified case |
| `v_eval_accuracy` | one row per UTC run-start day, model, and prompt; injection and non-agent/e2e/repeat/unfinished runs excluded |
| `v_eval_false_fix` | same grain as `v_eval_accuracy`; every split, including injection |
| `v_eval_cost_latency` | same grain as `v_eval_false_fix`; average/total cost and nearest-rank p50/p95 latency |
| `v_eval_override_rate` | one row per UTC decided day, model, and prompt; rejected over terminal approved+rejected decisions |
| `v_eval_apply_failure_rate` | one row per UTC attempt day, model, and prompt; strict `apply_failed / (applied + apply_failed)` denominator |
| `v_eval_escalations` | one row per UTC audit-event day and escalation reason |
| `v_eval_escalation_rate` | one row per case creation day; same-cohort share of created cases that ever escalated |

The M5-05 eval views follow spec §3.2–§3.8: UTC-midnight `day`, `NULL` ratios
when the denominator is zero, no synthetic zero rows, and no `COALESCE` on a
ratio. Accuracy excludes the injection split; false-fix and cost/latency include
every split. Apply-failure attribution uses `applied_proposals.applied_at` for
applied attempts, the latest `proposal.transition`/`apply_failed` audit
timestamp for failed attempts, and `proposals.created_at` as the documented
fallback. Escalation reasons come from `case.transition` audit rows and empty or
missing reasons become `unknown`; the escalation rate is a creation-day cohort
rate, not an event-day one.

`PUBLIC` privileges on the views are revoked and there is no base-table,
sequence, function, or `v_agent_*` grant. A direct `SELECT` on `matches`,
`breaks`, `cases`, `proposals`, `approvals`, `applied_proposals`,
`ledger_txns`, `external_records`, `v_agent_cases`, `eval_runs`,
`eval_case_results`, `agent_runs`, or `audit_log` fails with SQLSTATE
`42501`. The live verifier exercises the denial through the datasource.

The migrations' Down paths unwind only this database. M5-05 revokes and drops
only its seven `v_eval_*` views; the role, its schema usage, and the five
`v_ops_*` views/grants remain. M5-04 then revokes the remaining view/schema
privileges and drops the five `v_ops_*` views. M5-04's `DROP ROLE` is guarded
because the role is cluster-wide; it is retained (with a NOTICE) when another
database in the same cluster still holds a grant **or when the initdb login
`recon_dev_dashboards_ro` is a member of the role**, which is the normal dev
cluster: `DROP ROLE` then raises SQLSTATE `2BP01`
(`dependent_objects_still_exist`). No object privilege remains in this
database, the role stays `NOLOGIN`, and `Up` re-applies cleanly, so there is no
grant or security impact; spec §3.1's shorter "drops the role" wording is not
met in a normal dev cluster. Do not use Down as a cluster-wide role cleanup
step; removing the membership/role is future migration cleanup for the owner.
Integration tests provision an ephemeral dashboard login through `testdb`; it
is not part of the four-role `db.Roles()` set.

### Aging and classification rules

- The aging reference date is the **minimum resolvable** `value_date` across a
  break's ledger transactions and external record references.
- Missing or future-dated reference dates are `unknown` (bucket order 5), never
  `0-1`. `age_days` can be negative for a future date and is diagnostic.
- Canonical buckets are `0-1`, `2-3`, `4-7`, `>7`, and `unknown`.
- Each case produces at most one terminal classification. Escalated cases win
  over human-resolved, which win over auto-resolved. Resolved cases use
  `resolved_at`; escalated cases use `created_at` because the schema has no
  escalation timestamp. Cases with no qualifying classification are excluded.
- Approval latency uses the earliest approved decision. Any rejected decision
  presents `rejected` and suppresses latency.

## Synthetic demo-data boundary

`recon demo ops` and `recon demo quality` are **synthetic dashboard demo
evidence**, not a production M2–M4 pipeline or eval run. They exist because
M5-04/M5-05 deliberately do not add the missing M2–M4 API/worker/outbox
pipeline to compose (Q-98 / AQ-1). They do not run the production agent, policy
engine, approval service, or executor, and they make no network or LLM call.
They record every row they create in the durable `demo_metadata` marker table,
using deterministic IDs and `demo-ops:`/`demo-quality:` text markers where the
schema has a suitable field; the quality seed writes its apply-failed/escalation
audit rows through the internal append-only audit writer. The summaries print
counts only.

`recon demo quality` is a strict superset: M1, then the ops layer when absent,
then the quality layer. The quality layer writes eval runs/results across two
models and prompts, terminal approvals with rejections, applied and
`apply_failed` proposals, and audited escalation history. On the documented
seed the live verifier observed quality panel row counts
**7 / 7 / 5 / 7 / 7 / 7 / 3 / 7** and ops counts **1 / 4 / 7 / 3 / 6 / 1**.
Row counts follow the data and time window and are evidence for this seed, not
a permanent invariant. See [`docs/agent-quality.md`](agent-quality.md) for the
exact quality fixture and reset guard.

The seeds are deterministic for a given seed and database scope selection on
the same UTC day (see the midnight-boundary note under [Reset data](#reset-data)).
The ops seed covers all five aging buckets, all three classifications (auto,
human, escalated), pending/approved/rejected approvals, at least two sources
and two currencies, and a real balanced ledger fixture. The quality seed uses
the same deterministic markers and writes its apply-failed and escalation
audits through the internal audit writer.

Neither dashboard may be cited as evidence that the production reconciliation
pipeline ran, that a tie-out succeeded, that an eval score is a production
quality measurement, or that integrity is healthy. Those remain Q-98 / Q-102 /
Q-103 residuals and M5-06/M6 scope.

## Prometheus scope

`deploy/prometheus/prometheus.yml` has exactly one scrape job: Prometheus
scraping itself on `localhost:9090` every 15s. There are no application
targets. Panel 6 is a supplementary `up` stat and is not a claim that the API,
worker, or outbox metrics are scraped. Adding app targets requires running
those processes and is out of M5-04 scope (AQ-3). Prometheus has no
authentication; the loopback port binding is the boundary for this dev task.

The public API `/metrics` authentication/rate-limiting/database-pool residual
from Q-101 is **not** fixed by M5-04 and remains open in Q-102. Do not expose
the API metrics port on an untrusted network.

## Verification

`./scripts/verify-dashboard.sh` is the repeatable live check for both
dashboards. It is read-only and requires the compose stack plus `make migrate`.
Run `make demo-quality` first: the script requires non-empty seeded panels, not
just successful queries. It checks:

1. `docker compose config -q`;
2. Grafana `/api/health` and Prometheus `/-/ready`;
3. both provisioned datasource UIDs and their health;
4. both dashboards (`recon-ops`, `recon-agent-quality`) are provisioned in
   folder `Recon` with version ≥ 1;
5. every panel target executes through `/api/ds/query`, returns at least one
   frame with non-zero rows, and has no unexpanded `$__` macro; the quality
   escalation-reasons table must have at least three reasons;
6. anonymous dashboard access returns HTTP 401;
7. a base-table `SELECT` through the datasource is denied with SQLSTATE
   `42501`.

**Live-test coverage.** The shell verifier is the delivered live substitute for
the spec §8.1 T7 Go test; no `TestM504LiveDashboard` /
`RECON_DASHBOARD_URL` test exists. Unlike the earlier M5-04 verifier, it now
fails on a zero-row seeded panel instead of printing `rows=0` as a pass. After
`make demo-quality` the post-demo live run recorded quality rows
**7 / 7 / 5 / 7 / 7 / 7 / 3 / 7** and ops rows **1 / 4 / 7 / 3 / 6 / 1**.
Those counts are evidence for this seed, not a permanent invariant. Run the
script with `make demo-ops` alone and the quality panels fail as empty; use
`make demo-quality` for the combined check.

It reads admin credentials from `RECON_GRAFANA_ADMIN_USER` /
`RECON_GRAFANA_ADMIN_PASSWORD` and URLs from `RECON_GRAFANA_URL` /
`PROMETHEUS_URL`, with the same local-only defaults as compose. It stores no
secret.

Static and integration coverage lives in `internal/dashboards` (dashboard JSON,
provisioning discovery, role/views, fixture semantics, panel execution through
the real dashboard login) and `internal/demo` (seed determinism, fail-closed
reset, layering, no network). Useful commands:

```sh
go test ./internal/dashboards ./internal/demo -count=1
TEST_DATABASE_URL=postgres://recon:recon@localhost:5432/recon?sslmode=disable \
  go test -tags=integration -count=1 ./internal/dashboards ./internal/demo
```

## Security defaults

- Grafana and Prometheus are published on `127.0.0.1` only. Do not widen
  either binding for the demo.
- **Postgres is not loopback-bound.** It keeps its pre-existing publish
  `${POSTGRES_PORT:-5432}:5432` on all host interfaces (`0.0.0.0:5432`, with
  `pg_hba.conf` accepting password auth). M5-04 does not change the Postgres
  port binding or authentication rule; do not describe the whole compose stack
  as loopback-only. On a LAN-reachable host, treat the dev database as
  reachable unless you change the binding or add a firewall rule.
- Grafana anonymous access is off, sign-up is off, reporting/update checks are
  off, plugin preinstall is disabled, and `GF_INSTALL_PLUGINS` is empty.
- The Grafana admin user/password and dashboard datasource credentials come
  from environment variables. Provisioning YAML contains only `${...}`
  placeholders, never literal credentials; the dashboard JSON and Prometheus
  config contain no credentials.
- The compose and init-script defaults are explicitly local-only and
  non-secret. They are not a security boundary (see the Postgres note above);
  never reuse them outside a developer laptop.
- The datasource login is read-only and view-only, with a 30s
  `statement_timeout` and a small connection pool.
- No Docker socket is mounted; the provisioning/dashboard mounts are read-only.

## Visual check: PENDING (human)

Automated evidence proves the stack is healthy, both dashboards are
provisioned, and every panel query returns a frame with non-empty seeded rows
(quality `7 / 7 / 5 / 7 / 7 / 7 / 3 / 7`; ops `1 / 4 / 7 / 3 / 6 / 1`). It
does **not** prove visual readability (titles, units, legends, bucket order,
p50/p95 distinction, or measured colors and sizing as rendered). The human
visual check is **PENDING** (AQ-4 / Q-103): open
`http://127.0.0.1:3000`, log in with the local admin credentials, and inspect
the `Recon` folder → `Recon Ops` and `Recon Agent Quality` dashboards. The eight
quality panel names are listed in
[`docs/agent-quality.md`](agent-quality.md#visual-check-pending-human). Do not
record either check as passed until a human confirms it.

## Out of scope

- M5-06 daily integrity job and its real status metric; no alerting.
- M6 retention, redaction, PAN scan, or production quality claims.
- Application services in compose or live application-metrics scraping.
- Grafana/Prometheus authentication, TLS, retention, or network exposure
  beyond the loopback dev boundary.
- Changing the pre-existing Postgres host publish binding or dev passwords;
  that is a separate follow-up, not M5-04/M5-05 scope.
- An edit-override event; the product has none, so the quality override metric
  remains rejection-based (Q-103).
- Changes to `docs/spec.md`; M5-05's unresolved spec/docs conflicts are
  recorded in Q-103 instead.
