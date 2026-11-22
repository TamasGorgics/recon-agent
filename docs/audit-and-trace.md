# Audit verification and traceability (FIN-30 to FIN-34)

This page is the operator and developer reference for M4-13: verifying the
append-only audit hash chain, the `actor_id` format rule, the trace surfaces
for a ledger transaction or a case, and the narrow audit-sequence retry.
`docs/spec.md` sections 3, 4.2, 5, and 9 and `docs/ROADMAP.md` M4-13 are the
requirements. Decisions and accepted boundaries are recorded in
`docs/open-questions.md` Q-96; `docs/spec.md` itself is unchanged.

For OpenTelemetry span/Phoenix documentation and the M5-01 trace-ID
persistence path (`agent_runs`, `proposals`, and audit entries), see
`docs/telemetry.md`. M4-13's readers surface the persisted IDs; M5-01 writes
them from the active span context. Level-3 Phoenix UI visibility remains
`PENDING (manual UI check)`.

Neither CLI command writes to the database: both open one `REPEATABLE READ`,
`READ ONLY` transaction and only roll it back. Both need only `database.app_url`
(`DATABASE_APP_URL` or the `database.app_url` config key), not the migration
`database.url`, `policy.file`, or unrelated runtime settings. The DSN must be
the application-role DSN: a non-superuser session that is a member of
`recon_app` (`db.OpenRole(RoleApp, ...)`). A superuser session or one outside
the role is rejected when the pool is opened, with exit `2`, before the
verification or trace queries run.

## `recon audit verify`

```text
recon audit verify [--from <seq>] [--json]
```

Recomputes each audit row's hash from its current column values and compares
the resulting chain with the stored rows and the head mirror in `audit_log` and
`audit_chain_head`. Verification is not reimplemented in the CLI; it calls
`audit.Verify` inside the read-only snapshot.

| Exit | Meaning |
|---|---|
| `0` | Chain intact (including an empty `--from head+1` check). |
| `1` | Verification completed and reported one or more findings. |
| `2` | Usage/config/connection/query error, including `--from` beyond the chain tail. |

Flags:

- `--from <seq>` defaults to `1`; numeric values below `1` clamp to `1`.
  `--from head+1` is a valid empty incremental check (`checked: 0`, exit 0);
  `--from head+2` is beyond the tail (exit 2). A non-integer is a usage error.
- `--help` prints the audit usage and exits `0`; unknown flags or extra
  arguments are usage errors (exit `2`, no stdout).
- `--json` prints exactly one object:

```json
{"from":1,"head_seq":520,"checked":520,"findings":[]}
```

`findings` is always an array, never `null`, and is sorted by `(seq, kind)`.
Each finding is `{"seq":N,"kind":"...","detail":"..."}`. `checked` is the head
span `max(0, head_seq - from + 1)`, not the number of surviving rows, so a
truncated chain still reports the span that should have been verified.

The human form prints one summary line and one line per finding:

```text
audit verify: OK from=1 head=520 checked=520 findings=0
audit verify: FINDINGS from=1 head=4 checked=4 findings=3
  seq=2 kind=hash_mismatch detail=...
```

Finding kinds: `hash_mismatch` (a row's stored `row_hash` is not the hash of
its current column values), `broken_link` (a row's `prev_hash` does not match
its predecessor's hash), `gap` (a seq is missing, including a deleted prefix or
a missing start), `truncated_tail` (the head mirror points past the last row),
and `head_mismatch` (the mirror is behind the rows, or its hash does not match
the row it mirrors). Query errors override findings: the command exits `2` and
writes no partial JSON.

A superuser tamper test must disable the `audit_log` triggers, which requires
superuser; the committed coverage does this only in disposable `testdb`
clones, restores every trigger, and asserts `tgenabled = 'O'`.

### Threat model and limits

The verifier detects tampering **relative to the stored head mirror**: mutated
or forged rows, deleted rows or prefixes (`gap`, `broken_link`), a deleted tail
while `audit_chain_head` still points past the last row (`truncated_tail`), and
a mirror that is behind the rows or disagrees with the row it mirrors
(`head_mismatch`). Every one of those cases exits `1` with a finding.

The chain is **unanchored**, so `recon audit verify` does **not** detect a
privileged attacker who can rewrite `audit_log` and `audit_chain_head`
together. The only head witness is a mutable table in the same database, and
`audit.Append` writes the mirror in the same transaction as the row. A
superuser, or any role that can disable triggers and write storage, can:

- delete every row and the mirror, then append a fresh, internally consistent
  chain with recomputed `prev_hash`/`row_hash` values and a matching mirror; or
- delete a tail together with the mirror (or move the mirror back), leaving a
  shorter but internally consistent chain.

Both states verify with exit `0` and `findings: []`. The trigger-disabled
tamper tests above prove the row-level cases; they do not turn the chain into an
anchor against the superuser who runs them. An external anchor - a published
head hash/sequence, WORM/immutable storage, or signed off-box checkpoints - is
future hardening and is **out of M4-13 scope** (`docs/open-questions.md`
Q-96(10)). Operators must not read exit `0` as proof that a privileged attacker
did not rewrite the chain.

## `actor_id` format

An audit actor is the canonical `type:id`, where `type` is `system`, `agent`,
or `human`. The `id` half is 1 to 128 characters, contains no colon, and
contains no whitespace or control characters. `internal/audit` constructors
(`SystemActor`, `AgentActor`, `HumanActor`, `ParseActor`) enforce the ASCII
subset: empty, over 128 runes, `:`, ASCII space, U+0000 to U+001F, and U+007F
are rejected with `ErrInvalidActor`.

The database is the authoritative backstop through the named constraint
`audit_log_actor_id_format`:

```text
CHECK (actor_id ~ '^[^[:space:][:cntrl:]:]{1,128}$')
```

The migration `20261119120000_m4_13_actor_id_format.sql` first counts invalid
or NULL existing `actor_id` values and aborts with the count only (never the
IDs). It never rewrites, trims, or normalizes rows, so existing hashes and the
chain stay valid. An empty `actor_id` can be reported by the older
`audit_log_actor_id_check` (`actor_id <> ''`) rather than
`audit_log_actor_id_format`, because PostgreSQL may evaluate the older check
first; the insert is rejected either way. Go parity is deliberately ASCII-only:
PostgreSQL's POSIX classes also reject some Unicode spaces/controls, and that
locale-dependent difference is accepted with the database as the stronger rule.

## Trace surfaces

### A6: persisted OTel trace IDs vs HTTP correlation

Only the trace ID of a valid active OTel span is persisted. `agent_runs.trace_id`,
`proposals.trace_id`, and the central `audit_log.trace_id` fallback all read
`telemetry.SpanTraceID`, which returns empty when no valid span is active and
never falls back to the legacy correlation value. Rows written by non-traced
paths, and historical rows, stay NULL.

The API's `X-Trace-ID` response header (and the legacy `WithTraceID` context
value) is **HTTP/log correlation only**. It comes from an inbound `traceparent`
when present (otherwise a per-request random value) and may differ from an OTel
trace ID started inside the same request, because M5-01 adds no inbound server
span (`otelhttp`). Sending a
`traceparent` or `X-Trace-ID` therefore cannot forge a persisted trace ID: the
request still returns `X-Trace-ID`, while `audit_log.trace_id` stays NULL unless
an internal span is active. An explicit caller-supplied audit `Entry.TraceID`
still wins over the context fallback. `docs/telemetry.md` documents the full
bridge and the async correlation (`recon.origin.trace_id`) rule.

### `recon trace --txn`

```text
recon trace --txn <ledger-txn-uuid> [--json]
```

Walks one ledger transaction back through `applied_proposals`, the proposal,
case, breaks, agent run, verification, policy decision and
`policy_config_versions` YAML, approvals and approver display/role, executor
audit, and the ledger entries. The UUID must be canonical lowercase. Exit `0`
means the root exists (the chain may be partial and is described by `missing`);
exit `1` means the root ledger transaction was not found; exit `2` is a
usage/config/connection/query error.

The JSON has exactly `schema_version: 1`, `ledger_txn`, `chains`, and
`missing`; chains and missing are always arrays. Chain order is `applied_at`
then proposal ID; approvals are ordered by `decided_at`; ledger entries by
`posted_at` then ID.

An application whose `ledger_txn_id` is NULL (link/duplicate proposals) is
still a chain, with `ledger_txn_id: null`, `ledger_txn: null`, and the
`ledger_txn` marker. A found root with no application returns `chains: []`
and `missing: ["applied_proposal"]`.

### `GET /v1/cases/{id}` (case-file v2)

The case file is now `schema_version: 2` and adds the top-level `trace`
object `{case, chains, missing}`; `case`, `breaks`, `proposals`, and
`agent_runs` keep their previous shape. It requires a viewer-or-higher bearer
key (`Authorization: Bearer <key>`), and the trace is read inside the same
read-only repeatable-read transaction as the rest of the document. Chains are
one per `applied_proposals` row, so an unlinked application appears exactly as
it does in `recon trace`.

This is a breaking version bump for consumers: v1 documents are no longer
returned. The trace exposes `policy.config_yaml` (threshold and rule
configuration from `policy_config_versions`) to viewers; it is not secrets, and
any future secret material must be redacted before it reaches this column
(Q-96(2)).
`trace.case` intentionally omits `break_ids`, which remains on the top-level
`case`.

Missing-link markers, in emission order, are: `applied_proposal`,
`proposal`, `case`, `breaks`, `agent_run`, `agent_run.trace_id`,
`verification`, `policy`, `policy_config`, `approvals`, `executor.audit`,
`ledger_txn`, `ledger_entries`. Markers are deduplicated and ordered by this
list, never lexicographically. A T0 decision needs no approvals; a T1/T2
decision without approvals gets the `approvals` marker.

One edge is not produced by normal writes: a JSONB `null` in
`proposals.policy_decision` (as opposed to SQL `NULL`) decodes to a zero policy
object, so the `policy` marker is suppressed, `policy_config` is emitted
instead, and the empty decision tier also suppresses the `approvals` marker for
what should have been a T1/T2 decision. The policy store always persists a
validated decision object, so reaching this state requires a direct SQL writer;
SQL `NULL` is handled correctly (`policy` marker). This is an accepted
limitation recorded in Q-96(11), not a supported decision shape.

## Narrow audit-sequence retry

Every audit append re-reads the chain head under an advisory lock. A
SERIALIZABLE transaction whose snapshot was fixed earlier can still read a
stale head and collide with the committed next `seq`. `db.WithTxPolicy` now
retries SQLSTATE `23505` only when the violated constraint is
`audit_log_seq_key`. All other unique violations are returned after one
attempt, the existing five-attempt bound and rollback-before-retry behavior
are unchanged, and exhaustion still returns the original `*pgconn.PgError`
wrapped by `db.ErrRetryExhausted`. The retry is bounded race healing, not a
schema change.

## Boundaries

- M4-13 reads and surfaces `agent_runs.trace_id`; it creates no OTel spans or
  trace IDs. M5-01 now writes `agent_runs.trace_id`, `proposals.trace_id`, and
  the central `audit_log.trace_id` fallback from the active span context only
  (A6 boundary above; see also `docs/telemetry.md`). Rows written by
  non-traced paths and historical rows stay NULL; readers render the ID as
  missing rather than inventing one. Phoenix UI visibility is still
  `PENDING (manual UI check)`; a level-2 API receipt must not be presented as
  UI evidence.
- The simulated approver, `recon-eval run --e2e`, and `TestMilestone4Gate`
  are M4-14, not part of this deliverable.
- The e2e test seeds `agent_runs` and `proposals` by SQL because those insert
  paths are unaudited; every later proposal mutation runs through its owning
  service and the expected audit registry is asserted explicitly.
- No new tables and no `docs/spec.md` edit. The only M4-13 migration is
  `20261119120000_m4_13_actor_id_format.sql`.
