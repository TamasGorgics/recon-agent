# Replay

`recon replay` reconstructs one recorded agent run from its stored
`llm_calls`/`tool_calls` and reports whether the resulting proposal is
identical. M5-02 delivers exact replay and rerun; the approved boundaries are
recorded in `docs/open-questions.md` Q-100. `docs/spec.md` §§12 and 19 are the
requirements and are unchanged.

Replay is a **read-only diagnostic**, not a supported restore or re-apply path.
It never executes a recorded tool, never writes a row, and never calls live
data. Exact replay never touches the network; rerun is offline by default and
only reaches a provider with explicit `--live`.

## Usage

```text
recon replay --run <uuid> --exact [--json]
recon replay --run <uuid> --rerun [--model <id>] [--prompt <version>]
                               [--cassette-dir <dir>] [--live] [--json]
```

Exactly one of `--exact` or `--rerun` is required. `--run` must be a canonical
UUID.

| Flag | Applies to | Meaning |
|---|---|---|
| `--run <uuid>` | both | `agent_runs.id` to replay; required. |
| `--exact` | both (one) | Recompute request hashes and the proposal digest with no model call. |
| `--rerun` | both (one) | Drive a model from the recorded transcript and injected tool results. |
| `--model <id>` | rerun | Override the recorded model. Rejected with `--exact`. |
| `--prompt <version>` | rerun | Override the recorded prompt version. Rejected with `--exact`. |
| `--cassette-dir <dir>` | rerun | Cassette directory; default `eval/cassettes`. |
| `--live` | rerun | Call the provider instead of cassettes; requires the process-env key. Rejected with `--exact`. |
| `--json` | both | Print exactly one JSON report on stdout. |

Only the application-role DSN (`DATABASE_APP_URL` or `database.app_url`) and
the agent/LLM settings are needed. The migration DSN and `policy.file` are not.

## The no-DB boundary

The command has exactly one database phase. It opens the application role,
loads the run in one `REPEATABLE READ READ ONLY` transaction, commits that
transaction, and closes the pool. `replay.ExactReplay(recording, config)` and
`replay.Rerun(...)` receive only the in-memory recording (plus an injected model
client for rerun), so neither has a pool, transaction, DB-carrying context, or
executor to reach. A test closes every pool before exact replay and asserts a
later query fails while replay still succeeds.

Replay initializes no OpenTelemetry provider and exports no spans; a counting
OTLP endpoint test asserts zero hits. Telemetry stays off on this path.

## What exact mode checks

Exact replay supports only `outcome='proposal'` with a stored proposal. Any
other outcome, or a proposal outcome with no stored proposal row, is exit `2`
`unsupported_run`; replay never fabricates an identity or placeholder proposal.

Starting from the recorded seed `get_case` result, replay rebuilds the
transcript in memory, recomputes every canonical LLM request hash from the
current prompt/code/config, and compares it with `llm_calls.request_hash`.
Recorded tool results are injected, not executed. The proposal is rebuilt
through the production normalization path and compared by digest.

Every model-requested tool call — the seed `get_case`, investigation tools, and
proposal tools — also has its arguments compared with the stored
`tool_calls.args`. Both sides are canonicalized with the shared JSON encoder
(sorted keys, `UseNumber`, `null` distinct from `""`), and empty arguments are
normalized exactly as the recorder stored them (empty/nil becomes `{}`). A
mismatch is a `tool_result_mismatch` hard stop with `field: "args"` and makes
`identical` false, so replay cannot silently rebuild a proposal from a
different argument set than the one recorded. A recorded response cannot
preserve the difference between an empty argument payload and a literal JSON
`null` (both serialize as `null`); for that malformed-call case only, replay
accepts either stored spelling (`{}` or `null`). Malformed calls never record a
proposal.

A proposal tool call is terminal only when the recorded call succeeded (no
stored tool error). An invalid-arguments proposal attempt is injected as its
recorded error result, the production repair/retry flow is mirrored, and a
later valid proposal ends the replay. Exact replay does not treat the proposal
tool *name* alone as the end of the run, so a legitimate
malformed → invalid-arguments → repair → valid-proposal sequence is identical
rather than a false `step_count_mismatch`.

The digest covers only
`{type, payload, rationale, evidence (sorted unique), confidence (4dp string),
pattern_claimed}`. JSON `null` and `""` are distinct. Timestamps, stored IDs,
`state_hash`, and `idempotency_key` are excluded. Exact replay recomputes the
idempotency key as a diagnostic: a key-only difference is
`proposal_field_diff(field=idempotency_key)` and does **not** change
`identical`, but it is still a divergence and the command exits `1`.
`state_hash` is reported but is diagnostic only (a malformed value adds
`state_hash_invalid`); replay never compares it for identity.

Tool results carry two hashes. `result_hash` hashes the original Go envelope
bytes; `result_canonical_hash` hashes canonical JSON of the JSONB-normalized
row and is the value replay can reproduce. A non-NULL mismatch is
`tool_result_mismatch` and a hard stop. A legacy row with NULL
`result_canonical_hash` reports the non-breaking
`tool_result_hash_unavailable` diagnostic; argument/content checks still apply.

Divergences are sorted by `(step, code rank, field)` with a null field first.
Hard stops end the sequence, so downstream artifacts of the first failure are
not reported. The codes are `request_hash_mismatch`, `tool_call_unrecorded`,
`tool_result_mismatch`, `step_count_mismatch`, `proposal_digest_mismatch`, and
`proposal_field_diff`.

## What rerun mode does

Rerun rebuilds the seed/investigation transcript and drives a model. A
model-requested tool call is matched by `(tool_name, canonical arguments)`
against **unconsumed** recorded calls in ascending `seq`; the matching recorded
result is injected. An unmatched request is the `tool_call_unrecorded` hard
stop and is never executed. The first proposal tool call is terminal: the
proposal is rebuilt through production normalization, compared by digest only,
and the rerun stops. Rerun has no executor and writes no row.

The default client replays `eval/cassettes` and never touches the network. A
cassette miss is exit `2` `cassette_miss`, never a divergence. `--cassette-dir`
selects a different cassette directory; it is a delivered M5-02 extension, not
a spec change.

### Live gating

`--live` is the only provider path. It requires `OPENCODE_API_KEY` in the
**process environment**; a missing key is exit `2` `live_key_missing`. The key
is never read from `.env`, a config file, or any other file: `LLMConfig.APIKey`
is `yaml:"-"`, so configuration cannot supply it. Tests and CI never use
`--live`. Rotate the key after any exposure.

## Exit and JSON contract

| Exit | Meaning |
|---|---|
| `0` | Replay completed with no divergences and, in exact mode, equal proposal digests (`identical: true`). |
| `1` | The divergence list is non-empty; the full report is still printed to stdout. This includes a key-only `proposal_field_diff(idempotency_key)`, which keeps `identical: true` but is still a divergence. |
| `2` | Usage/config/load/DB/`unsupported_run`/`cassette_miss`/`live_key_missing` error; stderr only, even with `--json` (no partial JSON on stdout). |

Exit status follows the divergence list (spec §5), not the `identical` boolean
alone. A key-only idempotency-key difference is deliberately non-identity-breaking
for the report, but it is a real divergence for the exit status. Automated
callers must read both `identical` and `divergences`.

With `--json`, exactly one object is written. `run_id`, `mode`, `identical`,
`proposal_hash`, `stored_hash`, and `divergences` are always present;
`diagnostics`, `state_hash`, `outcome`, `steps_total`, `model`, `prompt`, and
`clock` are optional diagnostic fields. `divergences` is always an array.
Each divergence is `{"code":"...","step":N,"field":null|"...",
"detail":null|"..."}`.

```json
{"run_id":"77777777-7777-4777-8777-777777777777","mode":"exact","identical":true,"proposal_hash":"d10ec05260ac00f04070b6ebed19f4eace8c2c4c5d8fea36a573ca69270a7c17","stored_hash":"d10ec05260ac00f04070b6ebed19f4eace8c2c4c5d8fea36a573ca69270a7c17","divergences":[],"outcome":"proposal","steps_total":2,"model":"...","prompt":"v1"}
```

The human form prints a verdict line (`IDENTICAL` or `DIVERGED`), the proposal
and stored hashes, mode/outcome/steps/model/prompt when present, then one line
per divergence and diagnostic.

```text
replay IDENTICAL run=77777777-7777-4777-8777-777777777777 mode=exact
  proposal_hash d10ec052...
  stored_hash   d10ec052...
```

Exit `1` is a completed replay that differs, not an operational failure.
Automated callers must not treat it as "replay could not run".

## Evidence

The done-when gate is the seeded integration test
`TestTwentyRunExactReplayGate` in `internal/replay/gate_test.go`. It creates a
fresh test database, loads the M2 fixture, seeds 20 distinct cases over three
scenarios split 7/7/6, records each through the production agent loop and the
real recorder, then loads each run read-only and replays it exactly. It
requires **20/20 identical**; the recorded evidence is 20/20 over
`fee_variance=7`, `reference_typo=7`, `duplicate_charge=6`. The same package
proves a different-prompt rerun reports digest divergences.

The committed fixture `internal/replay/testdata/recording_v1.json` (schema 1,
run `77777777-7777-4777-8777-777777777777`, case
`88888888-8888-4888-8888-888888888888`, prompt v1, outcome `proposal`) is the
small engine/CLI fixture. Its proposal hash is
`d10ec05260ac00f04070b6ebed19f4eace8c2c4c5d8fea36a573ca69270a7c17`. The
fixture loader is test-only; no production package imports it. Mutating the
fixture in memory produces the expected exit `1` reports, and a legacy-NULL-hash
variant stays identical with `tool_result_hash_unavailable` diagnostics.

Reproduce the gate (the database must be up and migrated):

```sh
TEST_DATABASE_URL='postgres://recon:recon@localhost:5432/recon?sslmode=disable' \
  go test -race -tags=integration -run TestTwentyRunExactReplayGate -v ./internal/replay
```

The gate is part of `make test-integration` and therefore `make verify`. It
needs a test database and is intentionally not in the fastest unit lane.

## Limits

- **`llm_calls` does not store the full request, provider, or base URL.**
  Exact replay must rebuild each request with the current code, prompt, model
  choice (from each recorded `response.requested_model`), temperature/token
  limits, provider, and base URL. A change to any of those, or to
  request-building code, can make an old run unreplayable and surfaces as
  `request_hash_mismatch`. That is a faithful mismatch, not noise: check
  code/config drift before treating it as a decision change. Replay is coupled
  to the code and configuration that recorded the run.
- **Legacy rows cannot be hash-verified.** `result_canonical_hash` was added
  without backfill; NULL rows get the non-breaking
  `tool_result_hash_unavailable` diagnostic. Re-record the run to gain the
  stronger check.
- **Integrity hashes are not authentication (non-blocking trust boundary).**
  `result_canonical_hash` is stored beside the data it protects. A writer with
  update access to both `tool_calls.result` and `tool_calls.result_canonical_hash`
  can rewrite them consistently, and replay will accept the forged result; the
  same applies to rewriting both `tool_calls.args` and the tool-call arguments
  inside `llm_calls.response`. Replay detects accidental or partial corruption
  and an inconsistent edit, not a fully consistent rewrite by someone with
  write access. There is no signature, MAC, or external anchor in M5-02, and
  adding authentication is out of scope; protect the tables with database
  privileges and audit rather than treating replay as tamper-proof.
- **Exact mode is proposal-only.** Killed, errored, and escalated runs are not
  replayable in exact mode; rerun still requires a stored proposal.
- **Rerun can only explore cassette coverage.** A genuinely new prompt needs
  cassettes that cover it or explicit `--live`.
- **`state_hash` is not identity.** Post-recording state changes are invisible
  to replay by design; only the proposal digest is compared.
- **The 20-run gate needs a test database.** The committed fixture covers the
  engine/CLI path offline, not the full recording gate.

M5-02 adds no metrics, dashboards, reports, retention job, scheduler, or other
M5-03+ scope, and does not edit `docs/spec.md`.
