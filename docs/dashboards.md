# Ops dashboard (M5-04)

This page is the operator guide for the M5-04 Grafana/Prometheus stack and the
provisioned `Recon Ops` dashboard (`uid: recon-ops`). `docs/spec.md` §13.1 is
canonical; this page describes the delivered shape and its boundaries. The
accepted boundaries and residuals are recorded as Q-102 in
`docs/open-questions.md`.

The dashboard is **dev-only**. It is provisioned from files in `deploy/`, reads
Postgres through a dedicated read-only login that can select five `v_ops_*`
views, and shows a Prometheus self-scrape as one supplementary panel. It is
not a production monitoring deployment, and it does not add application
services to compose.

## Bring-up

Prerequisites: Docker Compose, Go 1.27, and the repository's `Makefile`
defaults (the compose Postgres is `postgres://recon:recon@localhost:5432/recon`).

### Fresh Postgres volume

```sh
docker compose up -d --wait
make migrate
make demo-ops            # M1 demo + synthetic ops rows (destructive reset)
./scripts/verify-dashboard.sh
```

`docker compose up -d --wait` starts Postgres, Phoenix, Prometheus, and Grafana
and waits for the healthchecks. No API, worker, outbox, or other application
service is added (M5-04 decision AQ-3). Grafana is published on
`http://127.0.0.1:3000` and Prometheus on `http://127.0.0.1:9090` by default;
`GRAFANA_PORT` / `PROMETHEUS_PORT` may change the host port only.

On a first-time volume the init script creates the five group roles and the
local-only login `recon_dev_dashboards_ro`. The M5-04 migration creates the
five `v_ops_*` views and grants `SELECT` on the views to
`recon_dashboards_ro`. The provisioned datasource uses environment
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

`make demo-ops` runs `recon demo ops --seed 1 --days 14 --difficulty realistic
--reset`. It drops and recreates the public schema, replays migrations, seeds
the M1 demo, and then seeds the synthetic ops rows. It is destructive and
refuses to run when `RECON_ENV=production`.

`recon demo seed` alone repopulates only the M1 dataset; panels 3–5 will then
have no ops rows. `recon demo ops` includes the M1 seed, so it is the single
documented data path for every panel. A second `recon demo ops` without
`--reset` fails closed with
`demo ops: ops data already present; rerun with --reset` before adding any row.

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
- `deploy/grafana/dashboards/ops.json` (the dashboard)
- `deploy/prometheus/prometheus.yml` (self-scrape only)

Images are pinned exactly: `grafana/grafana:13.3.2` and
`prom/prometheus:v3.16.0`. The Grafana provisioning and dashboard directories
are read-only bind mounts. There is no one-shot init service and no Docker
socket mount.

## Panel catalog

The dashboard is UTC, defaults to `now-14d`, and refreshes every 30s. Panels
have stable IDs 1–6 and no template variables.

| ID | Title | Type | Datasource | Query source | Notes |
|---|---|---|---|---|---|
| 1 | Match rate by source | timeseries | `recon-postgres` | `v_ops_match_rate` | Match share per UTC day and source; malformed/missing run stats count as zero, a zero denominator renders NULL, not 0. Percent unit. |
| 2 | Unmatched value by currency | table | `recon-postgres` | `v_ops_unmatched_value` | Exposure proxy: sum of absolute open/in-case break deltas per currency. `exposure_major` is display-only; minor units are the source of truth. No cross-currency total. |
| 3 | Open breaks by type and aging bucket | table | `recon-postgres` | `v_ops_break_aging` | Break type is `breaks.side`; canonical bucket order `0-1`, `2-3`, `4-7`, `>7`, `unknown`. |
| 4 | Approval queue | table | `recon-postgres` | `v_ops_approval_queue` | Pending/oldest-pending are query-time snapshots. Latency uses the earliest approved decision; any rejected decision suppresses latency (fail-closed presentation). |
| 5 | Resolution mix over time | timeseries (stacked bars) | `recon-postgres` | `v_ops_resolution_mix` | One terminal classification per case, precedence escalated > human > auto. `event_at` is `resolved_at`, or `created_at` for escalated cases. |
| 6 | Prometheus self-scrape | stat | `recon-prometheus` | `up{job="prometheus"}` | Supplementary availability check only; green at `1`. **Not live application metrics.** |

Panel 4 contains `$__timeFilter(approved_at)` inside a `FILTER (...)` clause.
It was executed through the real provisioned datasource on Grafana 13.3.2 and
expanded correctly to a `BETWEEN` predicate; no query rewrite was needed. Only
the three approved macros (`$__timeFilter(bucket_start)`,
`$__timeFilter(approved_at)`, `$__timeFilter(event_at)`) appear in the
dashboard SQL.

## Read-only role and views

`recon_dashboards_ro` is a `NOLOGIN` group role. It is not a member of
`recon_app`, `recon_executor`, `recon_agent_ro`, or `recon_audit_ro`; it has no
superuser, database-creation, role-creation, or replication attributes. Its
role-level settings are `default_transaction_read_only=on` and
`statement_timeout=30s`. The local login
(`recon_dev_dashboards_ro`) sets the same settings directly, because
PostgreSQL does not inherit a group role's `rolconfig` at connection time.

The database surface is exactly five owner-rights views, with `SELECT` granted
on the views only:

| View | Grain |
|---|---|
| `v_ops_match_rate` | one row per UTC day and source |
| `v_ops_unmatched_value` | one row per currency |
| `v_ops_break_aging` | one row per open/in-case break |
| `v_ops_approval_queue` | one row per proposal |
| `v_ops_resolution_mix` | one row per classified case |

`PUBLIC` privileges on the views are revoked and there is no base-table,
sequence, function, or `v_agent_*` grant. A direct `SELECT` on `matches`,
`breaks`, `cases`, `proposals`, `approvals`, `applied_proposals`,
`ledger_txns`, `external_records`, or `v_agent_cases` fails with SQLSTATE
`42501`. The live verifier exercises the denial through the datasource.

The migration's Down unwinds only this database: it revokes the view/schema
privileges and drops the views. `DROP ROLE` is guarded because the role is
cluster-wide; when another database in the same cluster still holds a grant,
the role is retained (with a NOTICE) and this database's privileges are gone.
Do not use Down as a cluster-wide role cleanup step. Integration tests
provision an ephemeral dashboard login through `testdb`; it is not part of the
four-role `db.Roles()` set.

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

`recon demo ops` is **synthetic dashboard demo evidence**, not a production
M2–M4 pipeline run. It exists because M5-04 deliberately does not add the
missing M2–M4 API/worker/outbox pipeline to compose (Q-98 / AQ-1). It runs no
agent, policy, approval service, executor, or audit chain, makes no network or
LLM call, and writes rows marked with deterministic demo identifiers and
`demo-ops` prefixes. The summary prints counts only.

The seed is deterministic for a given seed and database scope selection. It
covers all five aging buckets, all three classifications (auto, human,
escalated), pending/approved/rejected approvals, at least two sources and two
currencies, and a real balanced ledger fixture. On the documented seed the live
verifier observed panel row counts **1 / 4 / 7 / 3 / 6 / 1** for panels 1–6;
row counts follow the data and time window and are evidence for this seed, not
a permanent invariant.

The dashboard must not be cited as evidence that the production reconciliation
pipeline ran, that a tie-out succeeded, or that integrity is healthy. Those
remain Q-98 / Q-102 residuals and M5-06 scope.

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

`./scripts/verify-dashboard.sh` is the repeatable live check. It is read-only
and requires the compose stack plus `make migrate` (and `make demo-ops` for
non-empty panel data). It checks:

1. `docker compose config -q`;
2. Grafana `/api/health` and Prometheus `/-/ready`;
3. both provisioned datasource UIDs and their health;
4. the dashboard `recon-ops` is provisioned in folder `Recon`;
5. every panel executes through `/api/ds/query` with no unexpanded `$__` macro;
6. a base-table `SELECT` through the datasource is denied with SQLSTATE
   `42501`.

It reads admin credentials from `RECON_GRAFANA_ADMIN_USER` /
`RECON_GRAFANA_ADMIN_PASSWORD` and URLs from `RECON_GRAFANA_URL` /
`PROMETHEUS_URL`, with the same local-only defaults as compose. It stores no
secret.

Static and integration coverage lives in `internal/dashboards` (dashboard JSON,
provisioning, role/views, demo coverage/determinism) and `internal/demo` (seed
and fail-closed rerun). Useful commands:

```sh
go test ./internal/dashboards -count=1
go test -tags=integration -run 'TestM504' -count=1 ./internal/dashboards ./internal/demo
```

## Security defaults

- Grafana is published on `127.0.0.1` only. Prometheus is published on
  `127.0.0.1` only. Do not widen either binding for the demo.
- Grafana anonymous access is off, sign-up is off, reporting/update checks are
  off, plugin preinstall is disabled, and `GF_INSTALL_PLUGINS` is empty.
- The Grafana admin user/password and dashboard datasource credentials come
  from environment variables. Provisioning YAML contains only `${...}`
  placeholders, never literal credentials; the dashboard JSON and Prometheus
  config contain no credentials.
- The compose and init-script defaults are explicitly local-only and
  non-secret. Never reuse them outside a developer laptop.
- The datasource login is read-only and view-only, with a 30s
  `statement_timeout` and a small connection pool.
- No Docker socket is mounted; the provisioning/dashboard mounts are read-only.

## Visual check: PENDING (human)

Automated evidence proves the stack is healthy, the dashboard is provisioned,
and every panel query returns a frame. It does **not** prove visual
readability (titles, units, legends, bucket order, or non-empty charts as
rendered). The human visual check is **PENDING** (AQ-4): open
`http://127.0.0.1:3000`, log in with the local admin credentials, and inspect
the `Recon` folder → `Recon Ops` dashboard. Do not record it as passed until a
human confirms it.

## Out of scope

- M5-05 agent quality dashboard (`v_eval_*`).
- M5-06 daily integrity job and its real status metric.
- Application services in compose or live application-metrics scraping.
- Grafana/Prometheus authentication, TLS, retention, or network exposure
  beyond the loopback dev boundary.
- Changes to `docs/spec.md`.
