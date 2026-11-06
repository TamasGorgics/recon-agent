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
- Files are pretty-printed JSON with a trailing newline. The hard cap is 1 MiB;
  M3-09 owns the committed-size budget.
- The `redactions` array names any redaction class applied while writing
  (`api_key`, `bearer`, `authorization`, `session_id`); it is empty for clean
  synthetic data.
- Do not commit a cassette generated from a live call that used real data.
