# LLM cassettes

Cassette files make agent tests deterministic and free: CI replays recorded
model responses and never calls a live provider. The first recording needs
`OPENCODE_API_KEY`; every later run replays offline.

## Layout

```text
eval/cassettes/<provider>/<model>/<prompt_version>/<sha256>.json
```

One JSON file per interaction. The key is the SHA-256 of a canonical request
that excludes authorization, session IDs, User-Agent, request IDs, timestamps,
and all transport headers. It includes the provider, base URL, model, prompt
version, system prompt, messages, tools, tool choice, temperature (including
`0` vs `null`), max tokens, and `stream: false`.

## Modes

- `record` calls the wrapped client and writes one file per successful call.
  It refuses to overwrite unless `Update` is set.
- `replay` is read-only and never calls the wrapped client. A missing, corrupt,
  oversized, or key-mismatched file is a hard error (`ErrCassetteMiss` wrapped
  in a `ProviderError` with kind `cassette_miss`). CI must fail on a miss.
- `passthrough` calls the wrapped client and never reads or writes files.

## Rules

- Synthetic prompts and responses only: no real case data, names, account
  numbers, memos, API keys, Authorization headers, or session IDs.
- Files are pretty-printed JSON with a trailing newline. The hard cap is
  **1 MiB per file**. Total committed cassette evidence has a **10 MiB
  (10,485,760-byte)** budget across `eval/cassettes/`,
  `eval/cassette-evidence/`, and every `eval/baselines/*/cassettes/` root.
  `TestCommittedEvalCassetteBudget` enforces the per-file cap, the total budget,
  schema 1, filename == key, path model/prompt consistency, `stream: false`,
  `temperature: 0`, and empty `redactions`.
- The `redactions` array names any redaction class applied while writing
  (`api_key`, `bearer`, `authorization`, `session_id`); it is empty for clean
  synthetic data.
- Do not commit a cassette generated from a live call that used real data.

## Replay fixtures vs record-only evidence

Two kinds of cassette files exist in this repository:

- **Replay fixtures** live under `eval/cassettes/` and are the only cassettes
  any test replays. They are the 12 M2 fixture cassettes described below.
- **Record-only evidence** lives under `eval/cassette-evidence/` and
  `eval/baselines/`. It records what an eval run actually requested and
  received; nothing loads it as a replay set, and no test replays it.

M3-08/M3-09 eval-run cassettes are record-only evidence. Q-53 random
IDs/timestamps prevent byte-identical replay on another clone. They live under
`eval/cassette-evidence/` and `eval/baselines/*/cassettes/`, are not loaded by
`TestCommittedCassettes` or any replay path, and are not CI inputs.
Deterministic replay and the CI eval gate remain M6-05 and require a pinned
world (Q-68).

The selected `test` baseline used prompt v1. Dev comparison evidence includes
both v1 and v2 runs; `v2` was recorded only as dev evidence for the stricter
evidence checklist and was never run on `test`. **v1 is frozen.** Its M2
cassettes are never re-recorded; v2 recordings are new files under the `v2`
prompt path and do not alter released v1 files.

## M2 fixture cassettes

The M2 smoke scenarios are recorded once against the eval-only fixture in
`eval/fixtures/m2` and committed under:

```text
eval/cassettes/opencode/deepseek-v4.1-flash/v1/
```

The M2-11 commit contains **exactly 12 cassette files** covering the three
scenarios (`fee_variance`, `reference_typo`, `duplicate_charge`). A scenario can
take several model steps, so the invariant is one hit per request, not a fixed
file count. `TestCommittedCassettes` pins the count at 12 and enforces the
no-secret rules; the M2 gate fails loudly with a re-record hint on any miss.

Re-record only when the request shape changes: prompts (`prompts/v1/*`), tool
schemas or descriptions, the fixture, the seed message, repair strings, or
request construction. Prompt changes become a new prompt version with new
cassettes; they never edit a released version. Re-record with the key supplied
through the process environment (never on the command line or in a file):

```bash
RECON_RECORD_CASSETTES=1 go test -tags=live -run TestRecordM2Cassettes ./internal/agent -count=1
```

The recorder self-skips unless `OPENCODE_API_KEY` and
`RECON_RECORD_CASSETTES=1` are set, so `make test-live` cannot overwrite
cassettes by accident. CI only replays and never sets a key. After recording,
run the gate and `TestCommittedCassettes` before committing:

```bash
make demo-m2
go test -race -run TestCommittedCassettes ./internal/agent
```
