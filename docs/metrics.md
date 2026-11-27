# Prometheus metrics (M5-03)

This page is the operator and developer reference for the M5-03 Prometheus
exporter: the endpoints, the frozen metric catalog, the label/cardinality
rules, scrape examples, and the database-gauge behavior. `docs/spec.md` §12 is
canonical; this page does not replace it. The M5-03 boundaries and residuals
are recorded as Q-101 in `docs/open-questions.md`.

The metric names, types, help text, label names/values, and histogram buckets
are a **public contract**. M5-04 (Prometheus/Grafana provisioning and
dashboards) consumes them without a metric change. Renaming or re-labelling a
family requires a specification/version note first.

## Endpoints and exposure

| Process | Endpoint | Default bind | Authentication | When |
|---|---|---|---|---|
| `cmd/api` | `GET /metrics` on the existing API server | `:8080` (`api.addr`) | public, same as the old stub route | always |
| `cmd/worker` | `GET /metrics` | `127.0.0.1:9091` (`metrics.worker_addr`) | none | long-running mode only |
| `cmd/outbox` | `GET /metrics` | `127.0.0.1:9092` (`metrics.outbox_addr`) | none | long-running mode only |

- The API keeps the existing public `/metrics` route and serves Prometheus text
  with the frozen prefix `text/plain; version=0.0.4; charset=utf-8`. The pinned
  `client_golang`/expfmt encoder appends `; escaping=underscores`, so the live
  header is
  `Content-Type: text/plain; version=0.0.4; charset=utf-8; escaping=underscores`.
  The frozen prefix is the contract; clients should parse the media type rather
  than compare the whole header byte-for-byte. The API does not open a second
  listener.
- The worker and outbox listeners serve **only** `GET /metrics`; any other path
  returns 404. They start before the poll loop and shut down with the signal
  context (bounded graceful shutdown).
- `--once` modes process one unit and exit. They do **not** open a metrics
  listener, so they cannot race for the port.
- A bind failure aborts startup with an error naming the config key (for
  example `metrics.worker_addr`), never the DSN or another secret.
- A partial collector failure returns `200` with the reachable families and is
  logged. A **total** gather failure with zero families returns `500` with a
  fixed body, so collector/error detail never reaches the response.
- `recon_build_info{component="api|worker|outbox"}` identifies the process. One
  process registry is shared by that process's API/services and its endpoint;
  there is no global/default registry and no `promauto`.

Both worker/outbox listeners are unauthenticated. Defaults stay on loopback so
a developer laptop does not expose business counts. A non-loopback bind is
allowed only by explicit configuration and logs a warning at startup; network
policy is the control until M5-04 revisits exposure. Do not expose the API
`/metrics` beyond a trusted network: it is public by contract.

The public API scrape is also DB-backed: each gather runs the four read-only
aggregates described under [Database-derived gauges](#database-derived-gauges),
and the handler does not set `MaxRequestsInFlight`. Concurrent unauthenticated
scrapes can therefore multiply database work (up to four aggregates each) until
the per-scrape `gather_timeout` bounds them. Keep the route on a trusted network;
M5-04 owns authentication/rate limiting and database-pool sizing for this path.

## Configuration

```yaml
metrics:
  worker_addr: "127.0.0.1:9091"   # METRICS_WORKER_ADDR
  outbox_addr: "127.0.0.1:9092"   # METRICS_OUTBOX_ADDR
  read_header_timeout: 5s
  shutdown_timeout: 5s
  gather_timeout: 2s
```

| Setting | Env override | Default | Meaning |
|---|---|---|---|
| `metrics.worker_addr` | `METRICS_WORKER_ADDR` | `127.0.0.1:9091` | Worker listener bind address |
| `metrics.outbox_addr` | `METRICS_OUTBOX_ADDR` | `127.0.0.1:9092` | Outbox listener bind address |
| `metrics.read_header_timeout` | — | `5s` | HTTP header read bound on both listeners |
| `metrics.shutdown_timeout` | — | `5s` | Graceful drain bound on both listeners |
| `metrics.gather_timeout` | — | `2s` | Bound for each scrape-time database collector invocation |

Addresses must be non-empty `host:port` with a numeric port; port 0 is accepted
(useful for ephemeral test binds). Durations must be positive; an invalid value
fails config validation before a listener opens. `gather_timeout` is consumed
by the database gauges in all three long-running binaries, not only the worker
and outbox. The API address remains `api.addr`; there is no separate API
metrics address.

## Metric catalog

All business families use the `recon_` prefix. The exact help text is frozen in
`internal/metrics/catalog.go` and asserted by the registry test; this table is
the operator-level summary.

| Metric | Type | Labels | Unit | Producer |
|---|---|---|---|---|
| `recon_build_info` | gauge | `component` (`api`,`worker`,`outbox`) | info; always `1` | seeded once per process |
| `recon_match_runs_total` | counter | `source` | runs | matcher, after a committed run |
| `recon_matches_total` | counter | `source`,`rule` | matches | matcher, per accepted rule from `RunStats.ByPass` |
| `recon_breaks_total` | counter | `source`,`side` | breaks | matcher, per detected break side |
| `recon_case_age_seconds` | histogram | `status` | seconds | case create/transition/resolve after commit |
| `recon_queue_depth` | gauge | `queue` (`cases_open`,`approvals_pending`,`outbox_pending`) | items | scrape-time database collector |
| `recon_agent_runs_total` | counter | `outcome`,`model` | runs | agent `FinishRun`, after the one-row update commits |
| `recon_agent_cost_usd_total` | counter | `model` | USD | agent `FinishRun`, after commit |
| `recon_approval_latency_seconds` | histogram | `tier` (`T0`,`T1`,`T2`) | seconds | successful, non-replayed human decision |
| `recon_errors_total` | counter | `component`,`code` | errors | bounded error paths (see below) |
| `recon_outbox_lag_seconds` | gauge | `topic` | seconds | scrape-time database collector |
| `recon_outbox_dead_letters_total` | counter | `topic` | messages | outbox dead-letter seam after commit |
| `recon_auto_apply_cap_usage_ratio` | gauge | `currency` | ratio 0–1 | limits `Check` result |
| `recon_tieout_unexplained_minor` | gauge | `currency`,`source` | minor units | tie-out observer (event-only; see Q-101) |
| `recon_integrity_job_status` | gauge | `job` (`integrity`) | status | worker only, after a completed run commits: `0` seeded/unknown, `1` passed, `2` failed (M5-06) |

### Histogram buckets

`recon_case_age_seconds`:

```
30, 300, 900, 3600, 21600, 43200, 86400, 172800, 259200, 604800, 2592000
```

`recon_approval_latency_seconds`:

```
30, 60, 300, 900, 1800, 3600, 14400, 86400, 259200
```

Both histograms clamp a negative age/latency (clock skew) to `0`; a zero value
is valid and observed.

### Seeded series

Every family is instantiated at construction, so an idle process still exposes
all of them:

- `recon_build_info{component="<process>"} 1` — one child for the running
  process; `component` is a closed enum and has no `unknown` child;
- every open-ended labelled vector has an `unknown` child (for example
  `recon_match_runs_total{source="unknown"} 0`);
- `recon_queue_depth` has exactly the three queue children at `0` (closed enum;
  no `unknown` child);
- `recon_integrity_job_status{job="integrity"} 0` (seeded with the closed
  `integrity` value, not `unknown`).

Seeded `unknown` series are public output and deliberately make an absent
producer visible. Seeded histograms have zero observations; they are not fake
samples. Do not read a seeded zero as a healthy measurement (especially
tie-out, and integrity before the first completed run of a process).

### Integrity status producer (M5-06)

`recon_integrity_job_status{job="integrity"}` is produced only by the
long-running worker, through `metrics.NewIntegrityObserver` after
`internal/integrity.Run` has committed a completed run:

| Value | Meaning |
|---:|---|
| `0` | Seeded/unknown; no completed run has been observed by this process. Never set at run start. |
| `1` | The last completed run passed. |
| `2` | The last completed run failed (any finding or check error). |

The adapter sets `1` on pass and `2` on fail and introduces no other value,
family, or label. The gauge is never updated on a lock conflict, shutdown or
cancellation, begin/query/insert/commit failure, config/connection error, or
panic: those are not completed results, so the previous value remains. It is
never reset to `0` to erase a prior result. A worker restart reseeds the
in-process registry to `0` until the next completed run (normally the daily
catch-up); `0` means unknown, not pass. The one-shot CLI does not create a
registry or produce this gauge. See [`docs/integrity.md`](integrity.md) for the
job and its 0/1/2 mapping.

## Labels, cardinality, and forbidden values

A label value is always one of the allow-listed values below. For an
open-ended label, any other value is normalized to the literal `unknown`; an
empty string, UUID, free text, or URL never creates a new series. Closed enums
(`queue` and the build-info `component`) never accept an arbitrary value or an
`unknown` child.

| Label | Allowed values / validator |
|---|---|
| `source` | `^[a-z0-9][a-z0-9_]{0,31}$`, else `unknown` |
| `currency` | `^[A-Z]{3}$`, else `unknown` |
| `model` | `^[A-Za-z0-9._:/-]{1,64}$`, else `unknown` |
| `rule` | `exact`, `reference_tolerance`, `composite_batch`, `composite`, `fuzzy`, `duplicate`, `unknown` |
| `side` | `ledger_only`, `external_only`, `amount_mismatch`, `duplicate`, `unknown` |
| `status` | `open`, `in_agent`, `proposal_pending`, `waiting_info`, `escalated`, `resolved`, `unknown` |
| `outcome` | `proposal`, `escalated`, `budget_exhausted`, `invalid_output`, `provider_error`, `unknown` |
| `tier` | `T0`, `T1`, `T2`, `unknown` |
| `queue` | `cases_open`, `approvals_pending`, `outbox_pending` (closed; no `unknown` child) |
| `topic` | `approval.request`, `approval.update`, `approval.cancel`, `unknown` |
| `job` | `integrity`, `unknown` |
| `component` | `api`, `worker`, `agent`, `matcher`, `approval`, `limits`, `outbox`, `tieout`, `metrics`, `integrity`, `unknown` (`recon_build_info` is narrower: `api`, `worker`, `outbox`) |
| `code` | `internal`, `invalid_request`, `not_found`, `conflict`, `unauthorized`, `forbidden`, `timeout`, `unavailable`, `provider_error`, `invalid_output`, `delivery_failed`, `dead_letter`, `collect`, `daily_cap_exceeded`, `unknown` |

Forbidden label **names** are rejected at registration and by the registry
validator:

```
case_id, run_id, agent_run_id, proposal_id, break_id, scope_id,
ledger_txn_id, approver_id, trace_id, idempotency_key, entity_id,
message_id, external_ref, payload, error, reason, rationale, dsn, url,
path, email
```

The guard (`ValidateRegistry`) asserts every `recon_*` family's type, help,
label set, buckets, values, and per-family `MaxSeries`. The per-family caps and
the total business-series budgets (**≤ 64 on an idle process** and **≤ 128 after
representative instrumentation**) are **test-time regression assertions**, not
runtime admission control: production binaries construct the catalog and serve
it and do not call `Validate()` while running. A provider-influenced `model`
value can therefore add a new child until the process restarts. `go_*`,
`process_*`, and `promhttp_*` families are client_golang/promhttp-owned and
exempt from the business allow-list; the process collector's exact families
depend on the host OS.

`recon_errors_total` is emitted only with allow-listed component/code pairs;
raw error strings never become labels. The API transport writer (`writeError`)
counts only 5xx: `api`/`unavailable` for 503, `api`/`timeout` for 504, and
`api`/`internal` otherwise; client 4xx responses are deliberately not counted.
Application-service observers also count their own bounded failures, for
example approval failures map to `conflict`, `not_found`, `invalid_request`,
`forbidden`, `timeout`, or `internal` even when the HTTP layer ultimately
answers 4xx. Those service-level codes are required by the spec and are not
transport 4xx counting. Case-service failures use the owning process component
(`api` or `worker`), because there is no `cases` component value.

`code="dead_letter"` remains in the frozen allow-list but has no producer.
Dead letters are authoritative from `recon_outbox_dead_letters_total`; a handler
failure increments `outbox`/`delivery_failed`, and an unknown-topic dead letter
increments only the dead-letter counter.

### Known limits of the guard

- **`MaxSeries` is not a runtime bound.** It is enforced by `ValidateRegistry`
  (tests) only, so `model` remains provider-influenced at runtime; the validator
  blocks out-of-charset and ID-like values but does not cap the number of
  distinct valid models. M5-04 should monitor agent-family cardinality before
  making it a dashboard/alert dependency.
- **Some caps are below legitimate allow-listed maxima.** The 48 seeded ISO
  currencies plus `unknown` give 49 valid `currency` values against the
  `recon_auto_apply_cap_usage_ratio` cap of 32. The errors family has 10
  non-`unknown` components × 14 non-`unknown` codes = 140 allow-listed pairs
  (165 including `unknown`) against its cap of 64. A registry populated with
  every valid value can therefore fail `Validate()` even though each value is
  allow-listed; runtime is unaffected because the guard is test-only. Raise or
  normalize these caps in a future catalog change.
- **Identifier heuristics are not exhaustive.** Canonical UUIDs, uppercase
  ULIDs, and ≥16-hex values are rejected, but lowercase ULIDs, prefixed UUIDs
  in `source`, and 15-hex values can satisfy the charset validators. No
  production producer was found for those shapes; the normalizers remain
  defense-in-depth, and tightening them is a future hardening item.

## Scraping

```bash
# API: public, on the existing server
curl -fsS http://localhost:8080/metrics | grep '^recon_'

# Worker and outbox: loopback listeners, long-running mode only
curl -fsS http://127.0.0.1:9091/metrics | grep '^recon_'
curl -fsS http://127.0.0.1:9092/metrics | grep '^recon_'

# Check every frozen family is present (idle process)
curl -fsS http://localhost:8080/metrics |
  grep -E '^recon_(build_info|match_runs_total|matches_total|breaks_total|case_age_seconds|queue_depth|agent_runs_total|agent_cost_usd_total|approval_latency_seconds|errors_total|outbox_lag_seconds|outbox_dead_letters_total|auto_apply_cap_usage_ratio|tieout_unexplained_minor|integrity_job_status)' |
  sed 's/{.*//' | sort -u
```

A representative excerpt (values move with real traffic):

```
recon_build_info{component="api"} 1
recon_queue_depth{queue="cases_open"} 0
recon_queue_depth{queue="approvals_pending"} 0
recon_queue_depth{queue="outbox_pending"} 0
recon_outbox_lag_seconds{topic="approval.request"} 0
recon_outbox_lag_seconds{topic="unknown"} 0
recon_integrity_job_status{job="integrity"} 0
```

Illustrative PromQL for M5-04 dashboards (the Prometheus server/dashboards are
M5-04 scope, not delivered here):

```promql
rate(recon_match_runs_total[5m])
recon_queue_depth{queue="outbox_pending"}
histogram_quantile(0.95, sum by (le) (rate(recon_approval_latency_seconds_bucket[5m])))
rate(recon_errors_total[5m])
```

Counters are process-local and reset on restart. Use `rate`/`increase` rather
than raw totals. Multiple processes/replicas each expose their own counters;
the DB-derived gauges reflect the shared database on each scrape.

## Database-derived gauges

`recon_queue_depth` and `recon_outbox_lag_seconds` are refreshed by a
scrape-time collector on every gather, using the process's `recon_app` pool.
The queries are read-only aggregates:

| Series | Query semantics |
|---|---|
| `recon_queue_depth{queue="cases_open"}` | `count(*)` of `cases` with `status='open' AND leased_by IS NULL` (matches `cases_open_queue_idx`) |
| `recon_queue_depth{queue="approvals_pending"}` | `count(*)` of `proposals` with `status='pending_approval'` |
| `recon_queue_depth{queue="outbox_pending"}` | `count(*)` of `outbox` with `delivered_at IS NULL AND dead = false` (matches `outbox_undelivered_idx`) |
| `recon_outbox_lag_seconds{topic}` | database-clock age of the oldest pending (`delivered_at IS NULL AND dead = false`) row per topic; a known topic with no pending rows is `0`; non-allow-listed topics fold into `unknown` by maximum age |

The three queue gauges are set on every successful scrape, including `0`.
Delivered and dead outbox rows are excluded. `approvals_pending` may use a
sequential scan on the currently small `proposals` table; the existing partial
unique index `proposals_one_active_per_case` can serve the predicate if volume
justifies it (no new migration/index in M5-03).

Each collector invocation shares one `metrics.gather_timeout` deadline across
its queries, so a hung database cannot make a scrape unbounded.

**Failure policy: keep the last value.** If a query or payload decode fails:

- the affected gauge keeps its last successful value (it is stale, not fresh
  monitoring evidence);
- the collector logs only `component=metrics` and a bounded queue name, never
  SQL, raw error text, database topic text, an ID, or a DSN;
- `recon_errors_total{component="metrics",code="collect"}` increments once per
  failed query in that scrape;
- the scrape still returns `200` with the remaining reachable families.

When a DB queryer is configured, the same scrape-time collector emits
`recon_errors_total` after counting failures, so a `metrics/collect` increment
is visible in the scrape that observed the failure. Without a queryer (unit
tests, nil metrics), the static seeded queue/lag gauges are served and no query
is issued.

## M5-04 boundary

M5-03 delivers the exporter, not the observability stack. It adds:

- the `internal/metrics` registry/catalog/guard and the frozen families;
- the API route and the worker/outbox loopback listeners;
- event producers and the scrape-time DB gauges;
- this documentation and Q-101.

M5-03 itself adds no Prometheus server, scrape configuration, Grafana
datasources, dashboard JSON, alerting, `deploy/` assets, or a
`recon_dashboards_ro` role. Those are delivered by M5-04 on pinned Grafana
13.3.2 / Prometheus v3.16.0 assets; the operator guide is
`docs/dashboards.md`, and Q-102 records the self-scrape-only and synthetic-data
boundaries. M5-05 (agent quality) remains out of scope. The daily integrity job
is M5-06: the worker now produces `recon_integrity_job_status` with the
post-commit `0`/`1`/`2` contract above, while a restart still reseeds `0`
until the next completed run. The tie-out gauge remains event-only; the worker
passes `metrics.NewTieoutObserver` for integrity runs, and the CLI passes no
observer (Q-101). M5-03 also leaves the API `/metrics` public with four
`/metrics` public with four unauthenticated DB aggregates per scrape and no
`MaxRequestsInFlight`; M5-04 did **not** add authentication, rate limiting, or
DB-pool sizing for that path, so the residual stays open (Q-101 A6 / Q-102) for
a future task. M5-04 kept the frozen names/types/labels/buckets and treats
seeded or keep-last values as not healthy.

See also: `docs/telemetry.md` for tracing (a separate signal) and Q-101 in
`docs/open-questions.md` for the accepted residuals.
