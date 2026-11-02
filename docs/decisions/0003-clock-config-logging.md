# 0003 — Clock, configuration, logging, and domain time lint

- **Status:** Accepted
- **Date:** 2026-11-02

## Context

M0-04 provides the deterministic time source that FIN-43 requires, strict
runtime configuration, trace-correlated JSON logging, and a lint rule that
keeps domain packages from reading the wall clock directly. These are shared
contracts that M1 and later milestones build on, so the shapes and boundaries
are fixed here.

## Decision

### Clock (`internal/clock`)

- `Clock` is exactly:

  ```go
  type Clock interface {
      Now() time.Time
      Since(time.Time) time.Duration
  }
  ```

- `Real` is a stateless implementation that delegates to `time.Now` and
  `time.Since`. Domain packages never use it directly; it is injected at a
  process boundary.
- `Fake` is protected by a `sync.Mutex` and provides `NewFake(now)`, `Now`,
  `Since`, `Advance`, and `Set`. `Advance` accepts a negative duration and
  moves the clock backward.
- There are deliberately no timers, `After`, or `Ticker` methods at M0. A later
  milestone that needs them adds them with a new decision record (HD-09).
- `clock_test.go` carries `// covers: FIN-43` and exercises `Now`, `Since`,
  `Advance`, `Set`, `Real`, and concurrent access under `go test -race`.

### Configuration (`internal/config`)

- Types are per-component with no global configuration: `Config{Database,
  Log, LLM}`, `DatabaseConfig{URL, TestURL}`, `LogConfig{Level}`, and
  `LLMConfig{AnthropicAPIKey}`. YAML keys are `database.url`,
  `database.test_url`, `log.level`, and `llm.anthropic_api_key`.
- Precedence is **defaults < YAML < environment**. The only default at M0 is
  `log.level: info`.
- YAML is optional. `Load` reads the path in `CONFIG_FILE`; when the variable is
  unset or empty there is no YAML. When `CONFIG_FILE` is set, the file must be
  readable and parseable, otherwise `Load` returns an error. An empty file is
  treated as an empty configuration.
- Unknown YAML keys are rejected with `yaml.v3`'s `Decoder.KnownFields(true)`;
  type mismatches are errors. There is no silent key or type coercion.
- Environment variables are fixed: `DATABASE_URL`, `TEST_DATABASE_URL`,
  `LOG_LEVEL`, `ANTHROPIC_API_KEY`, and `CONFIG_FILE`. An environment variable
  set to the empty string is treated as unset, so an `.env` file containing
  empty optional values does not erase YAML or defaults.
- Secrets are optional at M0. `Validate` requires only a non-blank
  `Database.URL`, which runtime database commands need. `TestURL` and
  `AnthropicAPIKey` are not required until their consumers exist.
- The YAML dependency is `gopkg.in/yaml.v3` **v3.0.1**, the newest v3 release at
  implementation time; it is a direct requirement in `go.mod`.
- `.env.example` parity is unit-tested by reading `../../.env.example` and
  asserting every environment variable read by the config package is listed.

### Telemetry (`internal/telemetry`)

- `NewLogger(w, level)` returns a `slog.Logger` with a JSON handler and the
  given minimum level.
- `WithTraceID(ctx, id)` and `TraceID(ctx)` store and read the trace identifier
  in a private context key. `FromContext(ctx, base)` returns `base` with a
  `trace_id` attribute only when the context carries a non-empty identifier; it
  returns the base logger unchanged otherwise.
- Trace-context helpers live in `internal/telemetry`; M5-01 extends this package
  for OpenTelemetry. `internal/obs` is not created.
- Logging policy: no secrets, raw tool arguments/results, or untrusted memos at
  info level. Those payloads belong in the access-restricted tables described
  by the product spec, not in logs.

### Domain time lint (`forbidigo`)

- `.golangci.yml` uses the v2 schema and enables `forbidigo` with
  `analyze-types: true`.
- Forbidden patterns are `^time\.Now$` and `^time\.Since$`, each with a message
  pointing at `clock.Clock`.
- The rule is scoped to the domain packages `internal/{ledger,match,cases,
  policy,approval,executor,audit,agent}`. `forbidigo` cannot filter by the
  caller's package, so the scope is expressed with a golangci-lint exclusion
  rule using `path-except`; findings outside those paths are filtered.
  `internal/clock/` and `cmd/` also have explicit exclusion rules for clarity.
  Test files inside the domain packages are included.
- `scripts/lint-negative.sh` proves the rule is live. It injects
  `internal/ledger/zz_forbidigo_negative.go` once with `time.Now()` and once
  with `time.Since`, runs the pinned golangci-lint v2.15.0 on
  `./internal/ledger`, requires a non-zero exit whose output names `forbidigo`,
  and removes the file after each case. `set -eu` plus an
  `EXIT`/`INT`/`TERM` cleanup trap guarantee removal. The script compares
  `git status --porcelain` before and after, so it fails if it changes the
  working tree while still allowing unrelated in-progress edits.
- `make lint` runs `go vet ./...`, the pinned golangci-lint, and the negative
  script. `make lint-negative` runs only the script.

### `cmd/recon` integration

- `migrate up`, `migrate status`, and `testdb clean` load configuration through
  `config.Load` and call `Validate` before opening a database connection; they
  use `cfg.Database.URL`. Missing configuration therefore still exits 1 with an
  error, and the commands gain optional YAML support without changing their
  arguments or success output.

## Consequences

- Domain packages must accept a `clock.Clock` (or a narrower time dependency)
  instead of calling `time.Now`/`time.Since`; the lint rule fails the build if
  they do not.
- A malformed or misspelled YAML key fails fast instead of being ignored.
- `CONFIG_FILE` and environment variables are the only configuration inputs;
  there is no global config singleton to reset in tests.
- Adding a new environment variable requires updating the config package and
  `.env.example`; the parity test enforces the latter.
- The negative lint check adds two extra golangci-lint invocations to
  `make lint`; this is deliberate evidence that the rule works, not a silent
  configuration check.
