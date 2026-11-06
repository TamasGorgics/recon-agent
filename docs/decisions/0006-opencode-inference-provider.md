# 0006 — OpenCode Console Inference provider behind `llm.Client`

- **Status:** Accepted
- **Date:** 2026-11-06
- **Supersedes:** ADR 0003's `LLMConfig{AnthropicAPIKey}` / `ANTHROPIC_API_KEY`
  configuration and the roadmap's Anthropic Go SDK default

## Context

The roadmap assumed the Anthropic Go SDK for M2. There is no Anthropic
credential, and the available provider is OpenCode Console Inference on the Go
plan. The product specification requires a provider-neutral `LLMClient`
interface (section 2.2), not a particular vendor SDK. The V2 inference API is
documented as experimental, so the adapter must be narrow, pinned to documented
paths, and easy to contract-test.

Research is recorded in `.team/research/opencode-llm-api.md` and the M2 piece 1
run artifacts. Live probes established:

- Base URL `https://opencode.ai/inference/go/openai/v1`; `POST /chat/completions`
  with standard OpenAI Chat Completions tool calling.
- `Authorization: Bearer $OPENCODE_API_KEY`; `x-opencode-session` is mandatory
  for Go workspaces and must be stable per agent run.
- `deepseek-v4.1-flash` supports tools, tool choice, temperature, reasoning, and
  cache reads; `glm-5.3-flash` is a second OpenAI-compatible model.
- `usage.cost` is 0 on Go; `opencode.list_cost` and `opencode.allowance` carry
  the meaningful numbers. `opencode.tokens.input` excludes cache reads while
  `prompt_tokens` includes them.
- Error bodies arrive in at least three shapes: `{"error":{"type","message"}}`,
  `{"type":"error","error":{...}}`, and top-level typed OpenAPI objects.

## Decision

### Provider and adapter

- The sole M2 provider is OpenCode Console Inference, ID `opencode-go`.
- `internal/llm` owns a small `net/http` OpenAI-compatible adapter. No
  Anthropic or OpenAI SDK is added; `go.mod`/`go.sum` gain no LLM dependency.
- Default base URL `https://opencode.ai/inference/go/openai/v1` (configurable for
  pay-as-you-go); HTTPS is required by configuration. The adapter accepts HTTP
  only for `httptest`.
- Headers are `Authorization: Bearer $OPENCODE_API_KEY`, `Content-Type:
  application/json`, `User-Agent: recon-agent/<version>`,
  `x-opencode-client: recon-agent/<version>`, and a required
  `x-opencode-session: ses_<stable-per-run>`. A session is reused across
  retries and fallbacks and is never derived from the key or case data.
- `Client.Complete` is non-streaming. Requests send `stream: false`,
  `max_tokens`, and `temperature` as `*float64` so an explicit `0` serializes.
  `reasoning_content` is preserved inbound and omitted outbound.
- Default model `deepseek-v4.1-flash`; second eval model `glm-5.3-flash`.
  Configuration stores bare API model IDs; an optional `opencode-go/` selector
  prefix is stripped for the wire and registry.

### Cost and pricing

- Prices are embedded in `internal/llm/pricing.yaml` with a source URL and
  fetched date. Per-million-token prices, limits, capabilities, cache pricing,
  DeepSeek peak windows, and `allowed_fallback` are reviewed data.
- All cost arithmetic uses `shopspring/decimal`; no monetary value is a float.
- `opencode.list_cost` is recorded as `cost_usd` when reported and preserved
  separately from `opencode.allowance`; `usage.cost` is informational. Cost
  mismatch is recorded but does not fail the call. Unknown models fail loudly
  before HTTP.

### Errors, cassettes, and tests

- Error bodies are parsed for all observed families and classified by status,
  type, and message. Retryable availability errors carry `Retry-After`
  (header wins, body fallback). Secrets are redacted from messages, bodies,
  logs, and cassette diagnostics.
- Cassettes key on a SHA-256 of canonical JSON containing provider, base URL,
  model, prompt version, system/messages/tools/tool choice/temperature/max
  tokens/`stream:false`. Authorization, session, User-Agent, request IDs,
  timestamps, and transport headers are excluded.
- Replay is offline and read-only; a miss, corrupt file, or key mismatch is a
  hard `cassette_miss` error and never falls through to a live call.
- The live test is behind the `live` build tag, skips without
  `OPENCODE_API_KEY` or in CI, and uses `temperature: 0`. The text subtest uses
  `max_tokens: 32`; the tool subtest uses `max_tokens: 256` because reasoning is
  enabled and reasoning tokens count against the same budget, so a budget of 32
  can be exhausted before the tool call is emitted (stop reason `length`).
  `make test-live` is manual; `make verify` and CI never call the provider.
- The package lives at `internal/llm`, not under `internal/agent` as sketched in
  spec section 17. This is recorded in `docs/open-questions.md`; the provider
  layer does not import agent packages.

### Configuration

- `LLMConfig` has `Provider`, `BaseURL`, `APIKey` (YAML `-`), `Model`,
  `FallbackModels`, `Timeout`, and `MaxAttempts`.
- Environment: `OPENCODE_API_KEY`, optional `OPENCODE_BASE_URL` and
  `OPENCODE_MODEL`. Precedence remains defaults < YAML < environment; empty
  environment values are unset.
- `Config.Validate` checks structural LLM fields but does not require a key, so
  `migrate`, `testdb clean`, and `synth` continue without one. Live
  construction calls `LLMConfig.ValidateLive`, which names `OPENCODE_API_KEY`
  without printing a value.
- `FallbackModels` defaults to empty. A model may be a fallback only when the
  pricing registry marks `allowed_fallback: true` and it has passed the M3 eval
  gate; until then the demo configuration enables none.

## Consequences

- `LLMConfig` and `.env.example` replace `ANTHROPIC_API_KEY` with
  `OPENCODE_API_KEY` and add the base URL/model overrides. ADR 0003 remains the
  authority for clock, logging, and general configuration precedence; only its
  LLM configuration portion is superseded.
- `go.mod` gains no LLM SDK; the adapter is testable with `httptest` and
  record/replay cassettes.
- Live tests require a Console API key and are skipped in CI; first-time
  cassette recording needs that key.
- Provider-specific error and cost handling is required. `ProviderError` is the
  only provider failure type crossing the package boundary.
- Future Anthropic or OpenAI adapters can implement the same `llm.Client`
  without changing callers.
- Go plan terms and API stability remain product/operational risks: use Go for
  development/eval, and switch to pay-as-you-go for production unless the terms
  allow reconciliation workloads.
