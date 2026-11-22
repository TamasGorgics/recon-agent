# OpenTelemetry tracing (M5-01)

This page is the operator and developer reference for the M5-01 tracing stack:
what is traced, how to enable it, how to run the local Phoenix viewer, and how
to prove a demo trace reached Phoenix. `docs/spec.md` §3–§9 is canonical; this
page does not replace it.

## What M5-01 traces

| Process | Span | Notes |
|---|---|---|
| `cmd/worker`, `cmd/recon agent run` | `case.run` root | One per agent run; started before the run row is written |
| same | `llm.call` | Child of `case.run`; model, token counts, request/response hashes, cost |
| same | `tool.call` | Child of `case.run`; registered tool name (or `unknown`), sequence, result hash, refs, error code |
| `cmd/api` (approval) | `policy.evaluate` | Child of an active agent span when called in-process; otherwise a root span |
| executor (in-process today) | `executor.apply` | Child in-process; otherwise a root span |

An unknown or unregistered model tool name never reaches the span:
`recon.tool.name` is the exact registry name when the lookup succeeds and
`unknown` otherwise, so a model cannot inject text through a span attribute.
The raw attempted name remains only in the persisted `tool_calls.tool_name`
record.

Attributes are the frozen names in `internal/telemetry/attrs.go`. Payload
capture is **off by default**. With `RECON_OTEL_CAPTURE_PAYLOADS=true`,
`tool.call` may attach a redacted/truncated `recon.payload`; M5-01 keeps
`llm.call` hash-only and does not attach a response snippet. Prompts, memos,
raw arguments, and secrets are never attached. The optional snippet is a
dev/demo aid: `SafePayload` removes secrets, not business content or untrusted
memo text, so do not enable capture against real customer data.

**Known UTF-8 limitation (recorded in Q-99).** The truncation path is UTF-8
safe (it backs off a split rune and strips earlier invalid bytes from the
retained prefix), but `SafePayload` / `RedactAndTruncate` sanitize UTF-8 only
when truncation occurs. An **under-limit** input that already contains invalid
bytes can still come back as an invalid UTF-8 string. Every current production
call site passes JSON or error strings, so no end-to-end leak is known; treat
payload bytes as untrusted and expect every return path to be sanitized in a
follow-up.

### Correlation boundary (important)

Trace IDs are persisted for manual correlation:

```
agent_runs.trace_id → proposals.trace_id → audit_log.trace_id
```

`agent_runs.trace_id` and `proposals.trace_id` are written from
`telemetry.SpanTraceID`, i.e. only the trace ID of a valid active OTel span
(32 lowercase hex). A non-traced context, or one carrying only the legacy
`WithTraceID` correlation value, writes NULL. `audit_log.trace_id` is filled
centrally in `audit.Append` before the row hash from the same active-span-only
helper when the caller did not set an explicit value; explicit caller values
still win. Historical audit rows and hashes are never rewritten.

The legacy `WithTraceID` value exists for logs, the `X-Trace-ID` response
header, and request correlation (spec A6). It comes from an inbound
`traceparent` when one is present and is otherwise generated per request; either
way it is **never** a persistence source: an unauthenticated client cannot
forge an `agent_runs`, `proposals`, or `audit_log` trace ID by sending a
`traceparent`. The HTTP correlation ID and an OTel trace started later during
the same request may differ.

**Remote span-context trust boundary (latent hardening, Q-99).**
`telemetry.SpanTraceID` accepts any valid OTel `SpanContext`, including one
marked remote, and M5-01 does not otherwise distinguish local from remote
contexts. There is no production path today that extracts an inbound
`traceparent` into a span context, so a request cannot currently inject a
persisted trace ID through this gap; the legacy `X-Trace-ID`/`WithTraceID`
value remains non-persistent. If inbound propagation is ever added, reject
remote span contexts at the persistence boundary or document the accepted
trust model explicitly.

`case.run` + `llm.call` + `tool.call` form one trace in the agent process.
When `policy.evaluate` or `executor.apply` runs later in another process or
request, it starts a **separate trace**; its persisted proposal trace ID is
recorded as `recon.origin.trace_id` when it differs. Its audit rows carry that
async span's trace ID, not the origin trace ID; the origin attribute is the
join back to the agent trace. M5-01 does not persist a W3C `traceparent`, so
Phoenix shows two traces joined by trace ID, not one nested tree. Never
describe the async policy/executor path as a single tree.

The API's `X-Trace-ID` header remains the existing request correlation ID. M5-01
adds no inbound server span (`otelhttp`), so it can differ from an OTel trace ID
started inside an API request. This is a documented limitation (spec A6).

## Configuration

Telemetry reads standard OTel environment variables. With no OTLP endpoint,
`OTEL_SDK_DISABLED=true`, or `OTEL_TRACES_EXPORTER=none`, the process runs a
no-op provider: no exporter goroutines, no network, and shutdown is a no-op.
An explicitly configured but invalid protocol/sampler/header setting is a
startup error.

| Variable | Effect |
|---|---|
| `OTEL_SERVICE_NAME` | `service.name`; default `recon-agent` |
| `OTEL_SDK_DISABLED` | `true` forces the no-op provider |
| `OTEL_TRACES_EXPORTER` | `none` forces the no-op provider |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP base endpoint, e.g. `http://localhost:4318`; absent (with no traces endpoint) ⇒ no-op |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Trace-specific endpoint; wins over the base endpoint |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` (default) or `grpc` |
| `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | Trace-specific protocol; wins over the base protocol |
| `OTEL_EXPORTER_OTLP_HEADERS` | OTLP headers; never logged |
| `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Trace-specific headers; merged over the base headers |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | Exporter timeout, read by the OTel exporter (not by the wrapper) |
| `OTEL_TRACES_SAMPLER` / `_ARG` | `parentbased_always_on` (default), `always_on`, `always_off`, `parentbased_traceidratio`, `traceidratio`; ratio must be in `[0,1]` |
| `OTEL_RESOURCE_ATTRIBUTES` | Extra resource attributes, e.g. `deployment.environment=dev` |
| `RECON_OTEL_CAPTURE_PAYLOADS` | `true` opts into the bounded `tool.call` snippet; default false |

For gRPC, set `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` and point the endpoint at
`localhost:4317` (or use `http://localhost:4317`); plaintext is allowed only
for localhost/dev. No collector is required: the app exports directly to
Phoenix.

No endpoint is the normal unit-test/dev state: tests never dial Phoenix. The
provider is injected explicitly; no binary calls `otel.SetTracerProvider`.

### Wired processes

| Binary | Wiring |
|---|---|
| `cmd/api` | Initializes from env, passes the provider to `approval.Options.Telemetry`, drains HTTP, then flushes |
| `cmd/worker` | Initializes from env, passes `Telemetry` through `agent.SetupDeps`, then flushes after the poll loop / `--once` |
| `cmd/recon agent run` | Initializes for the command and flushes before exit, so the scripted demo exports |
| `cmd/outbox` | **Not wired**: it creates no M5-01 spans. Wire it only if it later calls policy/executor code |

Shutdown is explicit: work stops first (HTTP drain or worker loop exit), then
telemetry flushes with a fresh `context.WithTimeout(context.Background(), 5s)`.
A flush failure is logged; it does not turn a successful command into a
failure. An invalid telemetry configuration still fails startup.

## Local Phoenix viewer

`compose.yaml` contains one dev-only Phoenix service pinned to
`arizephoenix/phoenix:22.0.1` (never `latest`). Phoenix uses its default SQLite
database under the `recon-phoenix-data` volume; Postgres is not used for
Phoenix.

| Host bind | Container port | Purpose |
|---|---|---|
| `127.0.0.1:6006` | `6006` | UI, REST API (`/v1`), GraphQL (`/graphql`) |
| `127.0.0.1:4317` | `4317` | OTLP/gRPC |
| `127.0.0.1:4318` | `6006` | OTLP/HTTP; Phoenix serves `/v1/traces` on 6006 |

The non-obvious `4318:6006` mapping preserves the spec's
`OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` contract without changing
Phoenix's UI port. The image exposes no 4318 listener. All published ports are
bound to `127.0.0.1`, so the unauthenticated service is not reachable from the
LAN. `PHOENIX_ENABLE_AUTH` is explicitly `false`: Phoenix 21+ otherwise enables
auth for the image's non-loopback host and rejects OTLP exports. **This is a
local demo service; do not widen the loopback binding on an untrusted
network.**

```bash
docker compose up -d postgres phoenix    # or: make db-up && docker compose up -d phoenix
docker compose ps phoenix
docker compose logs -f phoenix
docker compose down                      # keeps the trace volume
docker compose down -v                   # deletes local traces too
```

Open <http://localhost:6006> for the UI. UI visibility is a separate,
human-gated evidence level: **M5-01 records level 2 only.** Level 3 is
`PENDING (manual UI check)`, an API query is not UI evidence, and the roadmap
M5-01 box stays unticked until a human opens the UI and confirms the tree. Do
not describe a passing receipt test as proof that the UI renders the trace.

## Demo trace path

`scripts/phoenix-demo.sh` is the canonical scripted demo:

```bash
scripts/phoenix-demo.sh
```

It checks `docker compose config -q`, starts Postgres and Phoenix, waits for the
Phoenix API, and runs the opt-in receipt test
(`TestPhoenixDemoReceipt` in `cmd/recon`). That test executes the real
`recon agent run` command path against the deterministic M2 scripted fixture on
an ephemeral test database, with OTLP export enabled, then:

1. reads `agent_runs.trace_id` and `proposals.trace_id` and asserts they match;
2. asserts the OTLP resource that Phoenix accepted reports
   `service.name=recon-agent` (Phoenix 22 does not expose resource attributes
   through its REST/GraphQL API, so the test captures the exact OTLP request it
   forwards to Phoenix and links it by trace ID);
3. queries Phoenix's REST API for that trace and asserts one `case.run` root,
   the expected `llm.call`/`tool.call` children, and that every child's
   `parent_id` is the root span ID;
4. prints `PHOENIX_LEVEL2 ...` and `PHOENIX_LEVEL3=PENDING (manual UI check)`.

The final verified tree (`73d42c7`) recorded these level-2 receipts:

| Run | `trace_id` | Shape |
|---|---|---|
| QA cold run | `54679d053e5564dec2b97f401ba7ac5e` | 15 spans: one `case.run` root, 5 `llm.call`, 9 `tool.call`; `service.name=recon-agent` |
| Tester run | `04261564286eebd63c58c28112e54b52` | same shape; 15 spans, 5 `llm.call`, 9 `tool.call` |

Trace IDs are random per run, so read the ID printed by the current
`PHOENIX_LEVEL2` line rather than reusing one from this page. The earlier W4
demo recorded `8be7a7fb82e767c73d3fe8f85a347a52`; that is another level-2
receipt, not UI evidence.

The live check is opt-in so the normal unit/integration suites never require
Phoenix:

```bash
RECON_PHOENIX_E2E=1 \
PHOENIX_URL=http://localhost:6006 \
PHOENIX_OTLP_ENDPOINT=http://localhost:4318 \
TEST_DATABASE_URL=postgres://recon:recon@localhost:5432/recon?sslmode=disable \
go test -race -tags=integration -run '^TestPhoenixDemoReceipt$' -v -count=1 ./cmd/recon
```

Manual CLI equivalent, when a database has the M2 scripted fixture loaded (the
same command the test runs in-process):

```bash
OTEL_SERVICE_NAME=recon-agent \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=dev \
go run ./cmd/recon agent run \
  --case 10000000-0000-4000-8000-000000000002 \
  --llm scripted --prompt v1
```

`cmd/recon agent run` prints no trace ID. Read the persisted value with the app
role:

```bash
psql "$DATABASE_APP_URL" -Atc \
  "select trace_id from agent_runs where case_id = '10000000-0000-4000-8000-000000000002' order by started_at desc limit 1"
```

### Level-2 receipt query

Phoenix 22 REST (exact path used by the opt-in test):

```bash
curl -fsS 'http://localhost:6006/v1/projects/default/traces?limit=100&include_spans=true' \
  | jq --arg id "$TRACE_ID" '
      .data[] | select(.trace_id == $id)
      | {trace_id, num_spans: (.spans | length), names: [.spans[].name]}'
```

GraphQL alternative for the exact trace (verified against 22.0.1):

```bash
curl -fsS http://localhost:6006/graphql \
  -H 'content-type: application/json' \
  -d '{"query":"query($traceId: String!) { getTraceByOtelId(traceId: $traceId) { traceId numSpans rootSpan { name spanId } spans(first: 100) { edges { node { name spanId parentId } } } } }","variables":{"traceId":"'"$TRACE_ID"'"}}' \
  | jq '.data.getTraceByOtelId | {traceId, numSpans, root: .rootSpan.name}'
```

### Evidence levels

| Level | Meaning |
|---|---|
| 1 | In-memory exporter unit tests: span names, parent/child IDs, attributes, status |
| 2 | OTLP export to Phoenix plus the REST/GraphQL receipt query above |
| 3 | Human opens the UI and confirms the tree; `PENDING (manual UI check)` until then |

Do not promote a level-2 API answer to level 3. If Phoenix cannot start, record
the exact error and mark receipt evidence BLOCKED; the level-1 tests still
stand.
