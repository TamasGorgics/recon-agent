# Payment Reconciliation Agent: Implementation Roadmap

Companion to `docs/spec.md` (v0.5). The spec says *what* to build. This file says *in what order, in what pieces, and how to know each piece is done*. It is written to be fed to a coding agent (opencode) one task at a time.

---

## 1. How to use this file

**You:** take the first unchecked task in the index (section 4) whose `Needs` are all checked. Give its ID to the agent. Review the diff against the task's "Done when" list. Merge. Tick the box.

**Agent rules** (M0-01 creates `AGENTS.md` as a short pointer to this section and `docs/spec.md`; keep both in sync):

1. Work on exactly one task ID per session. Before writing code, read the task, the spec sections it cites, and section 2 of this file.
2. Write tests first for anything with an invariant: money, ledger, state machines, policy.
3. Never weaken or delete a test to make it pass. If a test or the spec looks wrong, stop, add the question to `docs/open-questions.md`, and ask.
4. Do not edit `docs/spec.md`. Spec problems go to `docs/open-questions.md`.
5. Do not start a task whose `Needs` are not merged.
6. Finish with the Definition of Done (section 2.3) and report the commands you ran and their output.

**Kickoff prompt** (paste once per session):

```
You are implementing the Payment Reconciliation Agent in this repo.
Read AGENTS.md, docs/spec.md, and docs/ROADMAP.md section 2 first.
Your task is <ID>. Read only that task's section and the spec sections it cites.
Implement it test-first, run the "Done when" commands, fix until green, then stop.
Do not start another task. If anything in the task or spec is ambiguous or
contradictory, add it to docs/open-questions.md and ask me instead of guessing.
```

**Running agents in parallel:** any tasks whose `Needs` are merged can run at the same time in separate git worktrees. Each milestone lists its lanes. Migrations are timestamp-named (goose default) so parallel branches do not collide on numbers. Expect small merge conflicts in `Makefile`, config files, and the index.

---

## 2. Conventions

### 2.1 Stack defaults

Change only through a note in `docs/decisions/`.

| Concern | Choice |
|---|---|
| Language | Go 1.27.3 (`log/slog`, `math/rand/v2`), single module; pin `go 1.27.3` and `toolchain go1.27.3` in `go.mod` |
| Database | Postgres 16, `pgx/v5`, `goose` SQL migrations: timestamp-named, forward-only (spec section 21) |
| HTTP, schemas | `chi`; `santhosh-tekuri/jsonschema` for tool and API payloads |
| Decimals | `shopspring/decimal` for FX rates and input parsing only. Money itself is `int64` minor units |
| Tests | stdlib `testing`, `pgregory.net/rapid` (properties), `go.uber.org/goleak`, golden files in `testdata/` with an `-update` flag |
| LLM | OpenCode Console Inference API (OpenAI-compatible) behind `llm.Client`; prompts in `prompts/<version>/` |
| Observability | OpenTelemetry SDK to OTLP to Phoenix; `prometheus/client_golang`; Grafana provisioned from files |
| PDF | `html/template` rendered to PDF with `chromedp` |
| Lint | `golangci-lint`, custom `floatlint` and `migrationlint` (M1-02), `forbidigo` banning `time.Now` in domain packages |

### 2.2 Code and test conventions

- Package layout follows spec section 17. Domain packages (`ledger`, `match`, `cases`, `policy`, `approval`, `executor`, `audit`, `agent`) never call `time.Now()` (use `clock.Clock`) and never use float types for money.
- Money is only `money.Money`. Amounts crossing JSON or tool boundaries are integer minor units (FIN-05).
- Multi-statement writes go through `db.WithTx`. No network call is ever made while a transaction is open (FIN-22).
- Build tags: `integration` (needs Postgres via `TEST_DATABASE_URL`), `faultinject` (failpoints), `live` (real LLM calls; never run in CI).
- Every test that verifies a spec requirement carries a comment `// covers: FIN-10, FIN-11` (checked by M6-07).
- Errors are typed sentinels wrapped with `%w`. Logs are `slog` JSON. Tool arguments and results are never logged at info level (they can contain memos).
- Commit messages start with the task ID: `M1-06: ledger service (post, reverse, idempotency)`. Tick the index box in the same commit.
- Synthetic data only. Secrets only via environment; keep `.env.example` current.

### 2.3 Definition of done (every task)

- [ ] Every "Done when" item is demonstrably true; commands and output are in the PR description.
- [ ] `make lint test` is green, and `make test-integration` is green if the task touches the database.
- [ ] New requirement-verifying tests carry `// covers:` annotations.
- [ ] Migrations apply cleanly to an empty database. Schema differences from spec section 3.1 are recorded in `docs/open-questions.md`.
- [ ] Docs the task names are written (package `doc.go`, `docs/*.md`).
- [ ] The index box is ticked.

### 2.4 Task sizes

Sizes estimate **your** time with an agent doing the typing: **S ≈ 1 h**, **M ≈ 2 h**, **L ≈ 4 h** (steering, review, fixing what the agent got wrong). They are planning numbers, not promises.

---

## 3. Schedule

| Milestone | Tasks | Hours | Cumulative | Ends in week at 10 h/wk | Ends in week at 20 h/wk |
|---|---|---|---|---|---|
| M0 Bootstrap | 4 | 4 | 4 | 1 | 1 |
| M1 Foundation | 16 | 35 | 39 | 4 | 2 |
| M2 Agent v0 | 11 | 22 | 61 | 7 | 4 |
| M3 Evals | 9 | 19 | 80 | 8 | 4 |
| M4 Approvals | 14 | 32 | 112 | 12 | 6 |
| M5 Visibility | 12 | 24 | 136 | 14 | 7 |
| M6 Hardening | 11 | 22 | 158 | 16 | 8 |

**This is longer than spec section 18's "~6 weeks".** The total is 158 h (151 h without the three stretch tasks): about 8 weeks at 20 h/week or 16 weeks at 10 h/week. The spec's 6 weeks implies roughly 27 h/week. M1 (35 h) and M4 (32 h) are the fintech-credibility core, so protect them and cut elsewhere.

**Cut order if time is short** (each keeps the portfolio story intact):

1. The three stretch tasks: M5-10, M5-11, M6-01 (7 h).
2. M5-05 agent quality dashboard: rely on eval reports instead (2 h).
3. M5-08 case file report: keep the trace CLI from M4-13 (2 h).
4. M6-10 ADRs: fold into the README (1 h).

---

## 3.1 Deviations from spec section 18, and decisions made while planning

- **M0 added.** Spec section 18 starts at the foundation; the repo, CI, and test database need bootstrapping first.
- **Audit package moved earlier.** The hash-chained audit log is built in M1-05, not M4, because every ledger write needs an audit entry from day one (FIN-30). `recon audit verify`, actor-typing enforcement, and traceability stay in M4-13.
- **Untrusted-data handling moved earlier.** Spec section 9 items 1, 4, 5 (delimiting, sanitizing, detection) are built in M2-07 with the agent loop, since prompts are assembled there. M6-02 is the adversarial *test suite*.
- **CI gate uses recorded LLM cassettes.** PR builds replay recorded model responses: deterministic, free, no API key. Live runs are scheduled or manual with a cost cap (M2-02, M3-09, M6-05).
- **Spec patched to v0.5 alongside this roadmap:** the tie-out acceptance example is corrected (a *dropped* match only creates breaks and leaves balances tied; the detectable bug is a tolerance match whose delta is never booked); proposals carry an untrusted `pattern` claim and policy acts on `pattern_verified`; an `llm_calls` table lets `recon replay --exact` reproduce a run.
- **Defaults for spec section 22 open questions:** ticketing is Linear; tracing backend is Phoenix; the fee schedule is derived from the generator config; v1 is batch-only; evals compare two OpenAI-compatible models (`deepseek-v4.1-flash`, `glm-5.3-flash`) and two prompt versions.
- **Tables not in spec section 3.1:** `match_runs` (M1-12), `system_switches` (M2-01), `policy_config_versions` (M4-01), `integrity_runs` (M5-06), and an `eval_truth` schema (M3-02). `external_records` gains `value_date DATE` (M1-07) because FIN-42 needs it.
- **Spec §17 layout delta:** the M0 plan adds `internal/{money,db,testdb,clock,config,outbox,periods,tieout,matchscore,fault,worker}` and a root `compose.yaml`; `deploy/` remains reserved for Grafana/Prometheus provisioning (M5-04).
- **Toolchain pins:** Go 1.27.3 (`go 1.27.3`, `toolchain go1.27.3`), golangci-lint v2.15.0, goose v3.29.0, pgx v5.12.0, Postgres 16.x pinned to one minor tag in compose and CI.
- **Exit gate:** a fresh clone passes `make verify` (= `db-up migrate build lint test test-integration`); CI runs the same make targets with a Postgres service container.
- **Canonical docs:** `docs/spec.md` and `docs/ROADMAP.md` stay under `docs/`; do not create root copies.

---

## 4. Index

Generated from the task headings. Pick the first unchecked row whose Needs are all checked. Keep this table in sync if you edit a task.

| ☐ | ID | Task | Size | Needs |
|---|---|---|---|---|
| ☑ | M0-01 | Repo skeleton and agent instructions | S | — |
| ☑ | M0-02 | Postgres, migrations, transaction helper, test database | S | M0-01 |
| ☑ | M0-03 | CI pipeline | S | M0-02 |
| ☑ | M0-04 | Clock, config, logging, domain lint | S | M0-01 |
| ☑ | M1-01 | Money, currency, rounding, FX | L | M0-01 |
| ☑ | M1-02 | Float lint and migration lint | S | M0-03, M1-01 |
| ☑ | M1-03 | Ledger schema and database-level invariants | M | M0-02 |
| ☑ | M1-04 | Database roles and grants | S | M1-03 |
| ☑ | M1-05 | Audit log package with hash chain | M | M1-04 |
| ☑ | M1-06 | Ledger service: post, reverse, idempotency | L | M1-01, M1-03, M1-05 |
| ☑ | M1-07 | Ingestion core: batches, control totals, quarantine | L | M1-01, M1-05 |
| ☑ | M1-08 | PSP and bank file parsers | M | M1-07 |
| ☑ | M1-09 | Synthetic generator core | M | M1-06, M1-08 |
| ☑ | M1-10 | Failure modes, part A | M | M1-09 |
| ☑ | M1-11 | Failure modes, part B | M | M1-10 |
| ☑ | M1-12 | Matcher: scopes, runs, passes 1 and 2 | M | M1-06, M1-07 |
| ☑ | M1-13 | Matcher: passes 3 to 5 | M | M1-12 |
| ☑ | M1-14 | Breaks, cases, leasing | M | M1-12 |
| ☑ | M1-15 | Balance tie-out | M | M1-07, M1-12 |
| ☑ | M1-16 | Matcher scoring, demo pipeline, and M1 gate | S | M1-11, M1-13, M1-14, M1-15 |
| ☑ | M2-01 | Agent schema, views, grants, switches | S | M1-14, M1-04 |
| ☑ | M2-02 | LLM client, OpenCode adapter, cassettes | M | M0-04 |
| ☑ | M2-03 | Provider resilience | S | M2-02 |
| ☑ | M2-04 | Tool framework | M | M2-01, M1-01 |
| ☑ | M2-05 | Read tools, part 1 | M | M2-04 |
| ☑ | M2-06 | Read tools, part 2 | S | M2-04 |
| ☑ | M2-07 | Untrusted data handling | M | M2-04, M1-11 |
| ☑ | M2-08 | Proposal tools and server-side validation | L | M2-04, M2-05 |
| ☑ | M2-09 | Agent loop and prompts | L | M2-02, M2-03, M2-05, M2-06, M2-07, M2-08 |
| ☑ | M2-10 | Case worker | M | M1-14, M2-09, M2-01 |
| ☑ | M2-11 | Agent CLI, smoke tests, and M2 gate | S | M2-09, M2-10, M1-11 |
| ☑ | M3-01 | Eval schema, datasets, splits | M | M1-11 |
| ☑ | M3-02 | Scenario sandbox | M | M3-01, M2-09 |
| ☑ | M3-03 | Scorers | L | M3-01, M2-08 |
| ☑ | M3-04 | Eval runner | M | M3-02, M3-03, M2-09 |
| ☐ | M3-05 | Reports | M | M3-04 |
| ☐ | M3-06 | Component evals and calibration | M | M1-16, M3-03, M3-04 |
| ☐ | M3-07 | Run comparison | S | M3-04 |
| ☐ | M3-08 | Prompt versions and second model | M | M2-02, M3-04 |
| ☐ | M3-09 | Baseline, thresholds, gate command | M | M3-05, M3-06, M3-07, M3-08 |
| ☐ | M4-01 | Approval, executor, and outbox schema | S | M2-01 |
| ☐ | M4-02 | API server skeleton | M | M1-07, M2-01 |
| ☐ | M4-03 | Proposal state machine | M | M2-01, M1-05 |
| ☐ | M4-04 | Policy engine | M | M2-01 |
| ☐ | M4-05 | Deterministic verifiers | M | M1-01, M1-09, M2-08 |
| ☐ | M4-06 | Executor | L | M4-01, M4-03, M1-06, M2-08 |
| ☐ | M4-07 | Outbox worker | M | M4-01 |
| ☐ | M4-08 | Concurrency and fault-injection tests | M | M4-06, M4-07 |
| ☐ | M4-09 | Auto-apply limits and circuit breaker | S | M4-04, M4-06 |
| ☐ | M4-10 | Case lifecycle | L | M4-03, M4-06, M4-07, M1-12, M1-14 |
| ☐ | M4-11 | Approval service and channel interface | M | M4-02, M4-03, M4-04, M4-05, M4-07 |
| ☐ | M4-12 | Slack adapter | L | M4-11, M4-02, M4-07 |
| ☐ | M4-13 | Audit verification, actor typing, traceability | M | M1-05, M4-06, M4-11 |
| ☐ | M4-14 | Simulated approver, end-to-end pipeline, and M4 gate | M | M3-04, M4-06, M4-10, M4-11, M4-13 |
| ☐ | M5-01 | OpenTelemetry tracing | M | M2-09, M4-06 |
| ☐ | M5-02 | Replay | M | M2-09 |
| ☐ | M5-03 | Prometheus metrics | S | M4-02 |
| ☐ | M5-04 | Ops dashboard | M | M5-03, M1-15 |
| ☐ | M5-05 | Agent quality dashboard | M | M5-03, M3-04 |
| ☐ | M5-06 | Daily integrity job | M | M4-08, M1-15, M4-13 |
| ☐ | M5-07 | Daily reconciliation report | M | M5-06, M4-07 |
| ☐ | M5-08 | Case file report | M | M5-01, M4-13, M4-07 |
| ☐ | M5-09 | Period close | L | M5-06, M4-06 |
| ☐ | M5-10 | Approval queue UI *(stretch)* | M | M4-11 |
| ☐ | M5-11 | Weekly eval report *(stretch)* | S | M3-05 |
| ☐ | M5-12 | Demo environment and M5 gate | M | M5-04, M5-05, M5-07, M5-08 |
| ☐ | M6-01 | Ticket adapter (Linear) *(stretch)* | L | M4-11, M4-12 |
| ☐ | M6-02 | Injection suite and red-team tests | M | M2-07, M3-04, M4-14 |
| ☐ | M6-03 | Threat model | S | M4-14 |
| ☐ | M6-04 | "What the agent must not do", enforced | M | M4-14, M1-04 |
| ☐ | M6-05 | CI eval gate | M | M3-09 |
| ☐ | M6-06 | Retention, redaction, PAN scan | M | M2-09, M5-01, M1-05 |
| ☐ | M6-07 | Requirement coverage checker | M | M4-14 |
| ☐ | M6-08 | Final evals and failure analysis | M | M6-02, M3-09, M4-14 |
| ☐ | M6-09 | README and demo GIF | M | M6-08, M6-03, M6-04, M5-12 |
| ☐ | M6-10 | Decision records | S | M4-14 |
| ☐ | M6-11 | Final acceptance sweep | M | M6-05, M6-06, M6-07, M6-09 |

---

## M0 · Bootstrap

Goal: an empty but green skeleton. **Exit gate:** a fresh clone passes `make verify` (= `db-up migrate build lint test test-integration`) locally; CI runs the same underlying make targets, with a Postgres service container for integration.

**Lanes:** M0-01 first. Then M0-02 ‖ M0-04. M0-03 after M0-02.

### M0-01 · Repo skeleton and agent instructions · S
**Spec:** section 17 · **Needs:** —

**Build**
- `go mod init github.com/TamasGorgics/recon-agent`; create the layout in the M0 plan (spec §17 baseline + layout delta) with a `doc.go` in every non-main package and a minimal `main.go` in each `cmd/` package; keep `docs/spec.md` and `docs/ROADMAP.md` canonical; do not create root copies.
- `AGENTS.md`: the agent rules (section 1) and conventions (section 2). opencode reads it automatically.
- `Makefile` targets: `help`, `build`, `lint`, `lint-negative`, `test`, `test-integration`, `test-fault` (stub), `db-up`, `db-down`, `migrate`, `test-db-clean`, `verify`, `fin-coverage` (stub), `must-not` (stub), `demo` (stub), `demo-m1` (stub), `demo-m2` (stub). Stubs print a "not implemented until <task>" message and exit 1; they are excluded from `verify` until implemented.
- `.gitignore`, `.env.example`, `docs/open-questions.md`, `docs/decisions/README.md`.

**Done when**
- `go build ./...` and `make lint test` pass on the empty skeleton.
- `AGENTS.md` exists, and opencode started in the repo picks up the rules.

### M0-02 · Postgres, migrations, transaction helper, test database · S
**Spec:** sections 2.2, 3 · **Needs:** M0-01

**Build**
- Root `compose.yaml` with a pinned `postgres:16.<minor>` image and a `pg_isready` healthcheck; `internal/db` with a pgx pool and `WithTx(ctx, Beginner, pgx.TxOptions, fn)` that retries SQLSTATE `40001`/`40P01` with bounded exponential backoff and full jitter (max 5 attempts); the callback must be re-runnable.
- `cmd/recon migrate up` using goose; `migrations/` directory.
- `internal/testdb`: reads `TEST_DATABASE_URL` (the maintenance role needs `CREATEDB`); ensures `recon_test_template` exists before `TestMain` records its baseline (create and migrate once if absent, reuse if present); clones it per test with `CREATE DATABASE ... TEMPLATE`; registers `t.Cleanup` to close the pool and drop the clone; `CleanupOrphans`/`make test-db-clean` removes orphan `recon_test_%` databases except the template; pool cap 4 and `-parallel=4`; role DSNs are reserved for M1-04.

**Done when**
- `make db-up migrate test-integration` runs a sample test that gets an isolated database, and no databases are left behind afterwards.
- A unit test proves `WithTx` retries on a simulated `40001` error and gives up after the bound.

### M0-03 · CI pipeline · S
**Spec:** section 4.8 (verification), section 18 (milestones), section 17 (layout) · **Needs:** M0-02

**Build**
- GitHub Actions: `golangci-lint`, `go vet`, unit tests with `-race`, integration tests against a Postgres service container, module and build cache.
- One disabled placeholder job (`if: ${{ false }}`) with TODO comments referencing later tasks: `floatlint` (M1-02), `fin-coverage` (M6-07), eval gate (M6-05). Do not use commented-out YAML; do not make it a required check.

**Done when**
- CI is green on `main`; a PR with a deliberately failing test goes red.

### M0-04 · Clock, config, logging, domain lint · S
**Spec:** FIN-43, section 12 · **Needs:** M0-01

**Build**
- `internal/clock`: `Clock` interface with `Now() time.Time` and `Since(time.Time) time.Duration`; `Real` and a mutex-protected `Fake` with `Advance` and `Set`. No timers/`After`/`Ticker` at M0.
- `internal/config`: environment plus YAML loading with strict unknown-key rejection; precedence defaults < YAML < env; secrets optional at M0.
- `internal/telemetry`: `slog` JSON logger with a hook that adds `trace_id` when present; trace-context helpers live here (M5-01 extends this package).
- `forbidigo` rule banning `time.Now` and `time.Since` in the domain packages listed in section 2.2; exclude `internal/clock/` and `cmd/`. `make lint` includes a negative check (`scripts/lint-negative.sh`).

**Done when**
- Unit tests cover `Fake` clock behavior (including concurrent access under `-race`) and config validation errors; the clock test carries `// covers: FIN-43`.
- `make lint` fails on a deliberate `time.Now()` added to `internal/ledger`, and passes once removed.

---

## M1 · Foundation

Goal: a correct money type, an immutable double-entry ledger enforced by the database, ingestion with control totals, a deterministic matcher, a synthetic data generator with ground truth, and a balance tie-out. **Exit gate:** M1-16.

**Lanes:** A (money): M1-01 → M1-02. B (ledger): M1-03 → M1-04 → M1-05 → M1-06. C (ingest and synth): M1-07 → M1-08 → M1-09 → M1-10 → M1-11 (M1-07 needs M1-01 and M1-05; M1-09 also needs M1-06). D (matching): M1-12 → then M1-13 ‖ M1-14 ‖ M1-15 (M1-12 needs M1-06 and M1-07). Merge at M1-16.

### M1-01 · Money, currency, rounding, FX · L
**Spec:** FIN-01 to FIN-06; section 3.1 (`currencies`, `fx_rates`) · **Needs:** M0-01

**Build** (`internal/money`)
- `Currency` (code, exponent) backed by an embedded ISO 4217 table covering at least USD, EUR, SEK, GBP, JPY (0 decimals), KWD (3 decimals).
- `Money` with unexported fields: `FromMinor`, `ParseDecimal(string, Currency)` (exact decimal parsing; reject more precision than the currency allows), `Add`, `Sub`, `Neg`, `Cmp`, `IsZero`. Arithmetic is overflow-checked and returns an error on overflow or on mismatched currencies.
- `Rate` (decimal, fixed scale 12) and `Convert(m, rate, to, mode)`. One central `Round` function, half-even by default. `Allocate(m, weights)` splits an amount with the residual going to a named part.
- JSON encoding as `{"amount_minor": int, "currency": "USD"}`; decimal strings are accepted only at parse boundaries.

**Done when**
- Table-driven tests across JPY, USD, and KWD for parse, format, and round trips.
- `rapid` properties: addition is commutative and associative; `Allocate` always sums to the total; parse then format is the identity; conversion error stays within one minor unit.
- Overflow tests at `math.MaxInt64` and `MinInt64`; `go test -race ./internal/money` passes with at least 90% coverage.
- Tests carry `// covers: FIN-01, FIN-02, FIN-03, FIN-04, FIN-06`.

### M1-02 · Float lint and migration lint · S
**Spec:** FIN-01; section 4.8 · **Needs:** M0-03, M1-01

**Build**
- `tools/floatlint` (`go/analysis`): flags `float32`, `float64`, float literals, `strconv.ParseFloat`, and `math.Round`/`math.Floor` in the packages listed in `tools/floatlint/config.yaml`. A narrowly scoped `//floatlint:allow <reason>` comment is permitted for non-monetary values (confidence, latency).
- `tools/migrationlint`: scans `migrations/*.sql` for `REAL`, `DOUBLE PRECISION`, `FLOAT` (case-insensitive).
- Both wired into `make lint` and CI.

**Done when**
- Analyzer tests with good and bad fixtures; migration lint tests likewise.
- CI goes red when a `float64` is added to `internal/money`, and when a migration adds a `REAL` column.

### M1-03 · Ledger schema and database-level invariants · M
**Spec:** section 3.1 (`accounts`, `ledger_txns`, `ledger_entries`, `currencies`, `periods`); FIN-10, 11, 13, 14, 56; section 3.2 invariants 1 and 6 · **Needs:** M0-02

**Build** (one migration)
- Tables with `CHECK` constraints: direction, positive amounts, account status, period status. `ledger_txns.seq` identity; `idempotency_key UNIQUE`; `reversal_of`, `period_id`, `adjusts_period_id`. Seed `currencies` and one open period.
- A deferred constraint trigger that checks, at commit, that each transaction balances per currency (FIN-10).
- Triggers that reject `UPDATE` and `DELETE` on `ledger_txns` and `ledger_entries` (FIN-11).
- A trigger rejecting entries whose currency differs from the account's, whose account is not `active`, or whose period is closed (FIN-13, FIN-56).

**Done when** (integration tests, run as superuser; M1-04 repeats them as the app role)
- An unbalanced transaction fails at commit; a transaction balanced in one currency but not another fails.
- `UPDATE` and `DELETE` raise; wrong-currency and inactive-account postings fail; posting into a closed period fails.
- Sequence numbers are strictly increasing.
- Tests carry `// covers: FIN-10, FIN-11, FIN-13, FIN-14, FIN-56`.

### M1-04 · Database roles and grants · S
**Spec:** section 16; FIN-11; section 3.2 invariant 4 · **Needs:** M1-03

**Build**
- Migration creating group roles `recon_app`, `recon_executor`, `recon_agent_ro`, `recon_audit_ro`, plus dev login users in the compose file.
- Grants: the app role has no `UPDATE`/`DELETE` on ledger tables; the agent role has none on ledger tables at all.
- A trigger allowing inserts of `ledger_txns.kind = 'adjustment'` only when the session user is a member of `recon_executor`.
- `internal/db` exposes separate pools per role.

**Done when**
- A role matrix test (`internal/db/roles_test.go`) connects as each role and asserts what is allowed and denied: app cannot update or delete ledger rows; agent cannot insert into `ledger_txns`; a non-executor cannot insert an adjustment.
- The M1-03 immutability tests also pass when run as `recon_app`.

### M1-05 · Audit log package with hash chain · M
**Spec:** section 3.1 (`audit_log`); FIN-30, 31, 33 · **Needs:** M1-04

**Build**
- Migration: `audit_log` with `seq`, `prev_hash`, `row_hash`; append-only triggers.
- `internal/audit`: `Append(ctx, tx, Entry)` computes the chain inside the caller's transaction, serialized with an advisory lock (document the throughput tradeoff). Entries are hashed over canonical JSON (sorted keys). `Actor` is a typed value (`system:matcher`, `agent:<model>`, `system:executor`, `human:<id>`) validated by its constructor. `Verify(ctx, fromSeq)` returns breaks and gaps.
- Rule: `before` and `after` payloads hold IDs and amounts, not personal data (needed by M6-06).

**Done when**
- 100 concurrent appends yield a valid chain.
- Editing any column as superuser is caught by `Verify`; a deleted row (gap) is caught; an invalid actor string is rejected.
- The app role cannot update or delete audit rows.

### M1-06 · Ledger service: post, reverse, idempotency · L
**Spec:** FIN-10 to 14, FIN-30; section 3.2 · **Needs:** M1-01, M1-03, M1-05

**Build** (`internal/ledger`)
- `Post(ctx, tx, PostRequest)`: idempotency key plus `request_hash` of canonical lines (replay returns the original; same key with a different hash is a conflict error); balance validation in code; account checks (currency, status); adjustment allowlist from config (fees expense, FX gain/loss, rounding, suspense); an error type for customer-funds accounts that the policy engine maps to tier T2.
- Period selection: post in the period containing `value_date` if open; otherwise post in the current open period with `adjusts_period_id` set (FIN-56).
- `Reverse(ctx, tx, txnID, reason, idemKey)` creates a reversing transaction via `reversal_of`; a second reversal is rejected.
- `Balance(account, asOf)`. Every `Post` writes its audit entry in the same transaction.

**Done when**
- Integration tests: idempotent replay returns the same transaction; conflicting replay errors; 50 goroutines posting the same key produce exactly one transaction; double reversal rejected; closed-period adjustment lands in the open period with `adjusts_period_id`.
- A helper `testutil.AssertAudited` confirms every `Post` has its audit row.
- Tests carry `// covers: FIN-10, FIN-12, FIN-13, FIN-30, FIN-56`.

### M1-07 · Ingestion core: batches, control totals, quarantine · L
**Spec:** section 3.1 (`external_records`, `settlement_batches`, `statement_balances`, `reconciliation_scopes`); FIN-40, 41, 42, 50, 51, 55 · **Needs:** M1-01, M1-05

**Build**
- Migration: `external_records` (add `value_date DATE`), `settlement_batches` (add `status`, `quarantine_reason`, `row_count`, `declared_row_count`), `reconciliation_scopes`, `statement_balances`.
- Per-source config (YAML): timezone, cutoff time, business-day calendar (FIN-41). `ValueDate(source, ts)` derives the business date (FIN-42).
- `internal/ingest`: a `Parser` interface returning canonical records, declared totals, and statement balances. Pipeline: hash the file (already seen means no-op, FIN-51) → parse → control-total and row-count check (FIN-50) → continuity check against the previous closing balance (FIN-55) → insert everything in one transaction, or mark the whole batch `quarantined` with a reason and an audit entry. Records dedupe on `(source, source_record_id)`.

**Done when**
- Tests: same file twice is a no-op; a corrupted total quarantines the batch with zero records inserted; a statement gap quarantines; a record re-sent in a different file is skipped, not duplicated.
- Cutoff and DST-boundary tests for `ValueDate`.
- Tests carry `// covers: FIN-40, FIN-41, FIN-42, FIN-50, FIN-51, FIN-55`.

### M1-08 · PSP and bank file parsers · M
**Spec:** sections 2.1, 10 · **Needs:** M1-07

**Build**
- `docs/formats.md` defining two simple documented formats: a `psp_a` settlement file (charge, refund, fee, payout, chargeback rows; decimal-string amounts; reference; batch ID; memo) and a `bank_x` statement CSV (booking date, value date, amount, currency, description, bank reference, opening and closing balances).
- Parsers implementing `Parser` using `money.ParseDecimal`. Free-text fields (memo, description) are kept in `raw` and listed in config as untrusted for section 9.

**Done when**
- Golden-file tests for both formats; excess precision rejected; JPY zero-decimal amounts parse correctly.
- `FuzzParsePSP` and `FuzzParseBank` run 60 seconds without a panic.

### M1-09 · Synthetic generator core · M
**Spec:** section 10 · **Needs:** M1-06, M1-08

**Build** (`synth/`)
- Seeded generator on `math/rand/v2` PCG, no global state, injected clock. Produces: chart of accounts, merchants, customers (fake names), N days of charges, refunds, and payouts in USD, EUR, SEK, and JPY, with fees from a generator-defined fee schedule (also written to `fee_schedules`, which M2-01 and M4-05 read).
- Builds ledger transactions via `ledger.Post`, then derives *clean* PSP files and bank statements from them. Emits ground truth linking each ledger transaction to its external record(s).
- Manifest records generator version, seed, config hash (this becomes `dataset_version`).
- CLI: `recon synth generate --seed --days --out` and `recon synth load`.

**Done when**
- Same seed produces byte-identical output (test).
- Clean data ingests without quarantine, and every external record has exactly one ground-truth link.

### M1-10 · Failure modes, part A · M
**Spec:** section 10.1 (first six rows) · **Needs:** M1-09

**Build**
- `synth/modes`: a `Mode` interface that mutates the world and emits a ground-truth scenario (break signature, expected resolution type, amounts and accounts, expected tier, difficulty).
- Implement `fee_variance`, `fx_rounding`, `missing_settlement`, `late_settlement`, `duplicate_charge`, `partial_refund`. Difficulty levels (easy, realistic, adversarial) control noise: unrelated extra breaks, near-duplicate amounts.
- Configurable mode mix and counts.

**Done when**
- For each mode, a test generates N scenarios, validates the ground-truth schema, and asserts the anomaly is really present in the files (for example the `fee_variance` delta equals the fee schedule's delta).
- The manifest lists counts per mode and difficulty.

### M1-11 · Failure modes, part B · M
**Spec:** section 10.1 (last six rows) · **Needs:** M1-10

**Build**
- Implement `chargeback`, `batch_composite` (one payout, N charges, one missing), `reference_typo`, `unknown_deposit`, `injection_memo`, `ambiguous`.
- Adversarial memo templates live in `synth/injections/*.txt`, separate from the logic, so M2-07 and M6-02 can reuse them.
- Ground truth for `ambiguous` is "do not guess": expected resolution is `request_info` or `escalate`.

**Done when**
- As M1-10, plus: `injection_memo` scenarios have a ground truth independent of memo content; each `ambiguous` scenario provably has at least two candidates satisfying the fuzzy criteria.

### M1-12 · Matcher: scopes, runs, passes 1 and 2 · M
**Spec:** section 5; section 3.1 (`matches`); FIN-42 · **Needs:** M1-06, M1-07

**Build** (`internal/match`)
- Migration: `matches` (with status fields) and `match_runs`.
- Engine skeleton: loads unmatched ledger transactions and external records for a scope within a value-date window; ordered `Pass` interface.
- Pass 1 (exact: reference, amount, currency) and pass 2 (reference plus fee tolerance from config).
- Runs are incremental and idempotent; each match records its `match_run_id`; each run writes a summary audit entry.

**Done when**
- Re-running on unchanged data creates no new rows (test).
- Order-independence property: shuffling inputs yields the same matches.
- Pass 2 never re-matches pass 1 results.

### M1-13 · Matcher: passes 3 to 5 · M
**Spec:** section 5 (passes 3 to 5) · **Needs:** M1-12

**Build**
- Composite: one payout against N ledger entries, using declared batch membership first, then a bounded subset-sum with a size cap. Ambiguity means no match.
- Fuzzy: amount plus date window plus counterparty hint, scored; below the threshold (config) it stays a break.
- Duplicates: repeated external IDs and same-amount same-time charges become duplicate breaks, never automatic links.
- Document algorithms and complexity limits in `docs/matching.md`.

**Done when**
- Property tests: composite never claims a match whose sum differs from the total; fuzzy never matches when two candidates tie; all passes are order-independent.
- Benchmark: 100k external records in one scope match in under 60 s on a laptop.

### M1-14 · Breaks, cases, leasing · M
**Spec:** section 3.1 (`breaks`, `cases`); FIN-24; section 6.5 (lease expiry) · **Needs:** M1-12

**Build** (`internal/cases`)
- Migration for `breaks` and `cases` (status checks, `version`, `attempts`, lease fields).
- Create breaks from the unmatched remainder after a run, idempotently (signature dedupe). Group into cases: composite-batch breaks together, one case per other break. Priority from value.
- `Lease(ctx, workerID, ttl)` with `FOR UPDATE SKIP LOCKED`, `Renew`, `Release`. An expired lease returns the case to the queue and counts as an attempt. State changes use optimistic concurrency on `version` and return `ErrStale` for stale writers. Case state changes write audit entries.

**Done when**
- 20 concurrent workers never lease the same case; an expired lease is reclaimed and increments `attempts`; a stale update returns `ErrStale`; re-running creates no duplicate breaks.
- Tests carry `// covers: FIN-24, FIN-30`.

### M1-15 · Balance tie-out · M
**Spec:** FIN-54, FIN-55 · **Needs:** M1-07, M1-12

**Build** (`internal/tieout`)
- `Compute(ctx, scopeID, asOf)` returns ledger balance, statement closing balance, ledger-only and external-only open items, and the unexplained difference:
  `Unexplained = (LedgerBalance − ΣLedgerOnly) − (StatementClosing − ΣExternalOnly)`, signed from the clearing account's perspective.
- Independence from matcher code: it uses its own SQL, reads the `matches` table only for set membership, and never calls `internal/match`.
- `docs/tieout.md` with a worked example and sign conventions; `recon tieout --scope --date` CLI.

**Done when**
- A clean synthetic day yields zero.
- A seeded matcher bug (a tolerance match whose delta is never booked) yields a non-zero unexplained difference.
- A *dropped* match still yields zero. The test asserts this and explains in a comment that it only creates breaks and that the matcher evals catch it.

### M1-16 · Matcher scoring, demo pipeline, and M1 gate · S
**Spec:** section 11.5 (component evals); section 18 M1 · **Needs:** M1-11, M1-13, M1-14, M1-15

**Build**
- `internal/matchscore`: precision and recall of matches against ground truth, per pass.
- `recon demo seed`: generate → ingest → match → cases → tie-out → summary table.
- `TestMilestone1Gate` (integration).

**Done when**
- On `realistic` difficulty with seed 1: match precision ≥ 0.99 and recall of matchable pairs ≥ 0.95 (placeholders; record the actual numbers in `docs/decisions/`).
- The gate test asserts annotated tests exist for FIN-01 to 06, 10 to 14, 50, 51, 55.
- `make demo-m1` prints the summary; CI runs the gate.

---

## M2 · Agent v0

Goal: an agent that investigates a case through read-only tools and writes validated proposals, with budgets, resilience, and kill switches. No approval or execution yet. **Exit gate:** M2-11.

Cassettes: the first recording of model responses needs `OPENCODE_API_KEY`. After that, every test and the gate replay from `eval/cassettes/`.

**Lanes:** A: M2-02 → M2-03. B: M2-01 → M2-04 → (M2-05 ‖ M2-06 ‖ M2-07) → M2-08. Merge at M2-09, then M2-10 → M2-11.

### M2-01 · Agent schema, views, grants, switches · S
**Spec:** section 3.1 (`agent_runs`, `tool_calls`, `llm_calls`, `proposals`); section 3.2 invariant 7; section 6.5 · **Needs:** M1-14, M1-04

**Build** (one migration)
- `agent_runs` (outcome check), `tool_calls`, `llm_calls`, `proposals` (type and status checks, `pattern_claimed`, `pattern_verified`, `state_hash`, `idempotency_key UNIQUE`, partial unique index allowing one active proposal per case).
- `system_switches(name PK, enabled, reason, updated_by, updated_at)` seeded with `agent_enabled = true`, `auto_apply_enabled = false`.
- Read views `v_agent_*` for everything the agent may see (ledger, external records, batches, FX, fee schedules, cases, history). Grants: `recon_agent_ro` gets `SELECT` on those views only, plus `INSERT` on `proposals`; `recon_app` owns `agent_runs`, `tool_calls`, `llm_calls`.

**Done when**
- Extended role matrix: the agent role cannot read `audit_log`, `approvers`, or base tables; can insert proposals; cannot update them.
- A second active proposal for the same case fails on the partial unique index.

### M2-02 · LLM client, OpenCode adapter, cassettes · M
**Spec:** sections 2.2, 6.5, 11.3 · **Needs:** M0-04

**Build** (`internal/llm`)
- Provider-neutral types (`Message`, `ToolDef`, `ToolCall`, `Usage`, `StopReason`) and `Client.Complete`.
- OpenCode Console Inference adapter over OpenAI-compatible Chat Completions using a small `net/http` client, not an SDK: tool use, system prompt, `temperature` as `*float64` (so 0 serializes), `max_tokens`, `stream: false`, stable `x-opencode-session`, and `reasoning_content` preserved inbound.
- Embedded `pricing.yaml` with decimal per-million prices, model limits/capabilities, DeepSeek peak windows, and `allowed_fallback`; cost is computed with `shopspring/decimal`, never floats.
- `ScriptedClient` (queue of canned responses) for tests.
- `CassetteClient` wrapping any client: modes `record`, `replay`, `passthrough`, keyed by hash of (provider, base URL, model, prompt version, canonical request), stored in `eval/cassettes/`. A replay miss is an error in CI, never a silent live call.
- Central redaction for API keys, bearer values, authorization headers, and OpenCode session IDs in errors, logs, and cassette diagnostics.

**Done when**
- Round-trip tests for scripted and cassette clients; adapter contract test against an `httptest` fake server.
- A `live`-tagged test hits the real API (skipped by default; never in CI).
- A test proves API keys never appear in logs or errors.

### M2-03 · Provider resilience · S
**Spec:** section 6.5 · **Needs:** M2-02

**Build**
- `llm.Resilient`: error classification (retryable: 429, 5xx, timeouts, overloaded; fatal: auth, schema), exponential backoff with jitter on an injected sleeper, bounded attempts, honors `Retry-After`.
- Per-provider circuit breaker (closed, open, half-open) with metric hooks. Optional fallback chain limited to models marked `allowed_fallback` that have passed the eval gate; the response records the model actually used.
- Typed `ProviderError` so the worker can record `provider_error`.

**Done when**
- Fake-client tests: transient errors recover; the breaker opens after N failures and rejects fast; a half-open probe closes it; fallback only to allowed models. All deterministic with a fake clock.

### M2-04 · Tool framework · M
**Spec:** sections 6.2 to 6.4; FIN-05; section 3.1 (`tool_calls`) · **Needs:** M2-01, M1-01

**Build** (`internal/agent/tools`)
- `Tool` interface (name, JSON schema, `Call`) and a registry.
- Argument validation: reject unknown fields; amounts must be integers (FIN-05).
- Execution wrapper: records every call (args, result, hash, error, duration) via the app pool while tool bodies run on the `recon_agent_ro` pool; result size cap with a truncation flag; structured errors to the model, never stack traces.
- Stable evidence-reference constructors: `ledger_txn:<uuid>`, `ext:<source>:<id>`, `batch:<id>`, `fx:<base>/<quote>@<date>`, `fee_schedule:<id>`, `case:<id>`. Tools attach `refs` to results; `RunContext.RetrievedRefs()` collects them.

**Done when**
- Invalid arguments yield a structured error and are still recorded; `12.5` for an `amount_minor` field is rejected; truncation is flagged; retrieved refs are tracked per run.
- Tests carry `// covers: FIN-05`.

### M2-05 · Read tools, part 1 · M
**Spec:** section 6.2 · **Needs:** M2-04

**Build**
- `get_case`, `lookup_ledger_txn`, `lookup_external_record`, `search_external_records`. Parameterized queries only; search results capped at 50 rows and a 31-day window. Results list which fields are untrusted (consumed by M2-07).

**Done when**
- Integration tests on a synthetic world; an SQL-injection string in `reference` returns zero rows and no error; the limit and window caps hold; the agent role cannot reach audit tables.

### M2-06 · Read tools, part 2 · S
**Spec:** section 6.2 · **Needs:** M2-04

**Build**
- `get_settlement_batch`, `get_fx_rate`, `get_fee_schedule`, `get_history` (similar resolved cases by break signature; returns resolution and amounts).

**Done when**
- Integration tests for each tool, including "no rate for that date" and "no similar cases".

### M2-07 · Untrusted data handling · M
**Spec:** section 9 items 1, 4, 5 · **Needs:** M2-04, M1-11

**Build** (`internal/agent/untrusted`)
- `Wrap(field, value)`: delimited data block with a random per-run boundary token and an "untrusted" label; any occurrence of the boundary inside the value is escaped.
- Sanitizer: strips control characters, bidi overrides, zero-width characters; truncates to a configured size.
- Detector `Suspicious(text) []Flag` with a configurable pattern list (instruction overrides, role markers, tool-call lookalikes, URLs). A flag is recorded on the tool call and on the run. **A flagged run may only end with `escalate`;** proposal tools are disabled for it.

**Done when**
- Every template in `synth/injections` is wrapped and flagged; a memo containing the boundary string cannot break out; the sanitizer survives a 60 s fuzz run.
- The false-positive rate on a benign synthetic memo corpus is measured and reported (not gated).

### M2-08 · Proposal tools and server-side validation · L
**Spec:** sections 6.2, 6.3; FIN-13; section 3.2 · **Needs:** M2-04, M2-05

**Build**
- `propose_link`, `propose_adjustment`, `propose_write_off`, `mark_duplicate`, `request_info`, `escalate`, with the tool-to-proposal mapping from spec 6.2 and the optional `pattern` claim.
- Validation pipeline: schema → evidence refs exist in this run's `tool_calls` → business rules (balanced per currency; currency matches account; accounts in the allowlist; amounts consistent with the retrieved records, for example an adjustment never exceeds the break delta) → at most one proposal per run.
- Insert through the agent pool with `idempotency_key = hash(case, run, payload)` and a `state_hash` over the referenced records and break state (used by the executor in M4-06). `risk_tier` stays null.
- `escalate` is a terminal signal returned to the loop; the loop persists the case change, because the agent role cannot write `cases`.

**Done when**
- Table-driven rejections, each recorded and each a structured error: unbalanced entry, evidence never fetched, second proposal in a run, wrong currency, non-allowlisted account, adjustment above the delta, confidence outside 0 to 1.
- A partial-index conflict surfaces as `ErrActiveProposalExists`.

**M3 piece A prerequisite:** M3 piece A extends `propose_link` with an
exact-set `ledger_txn_ids` form and re-records the M2-11 gate cassettes once
(12 interactions; see Q-36).

### M2-09 · Agent loop and prompts · L
**Spec:** sections 6.1, 6.4, 9 · **Needs:** M2-02, M2-03, M2-05, M2-06, M2-07, M2-08

**Build** (`internal/agent`)
- `Run(ctx, caseID)`: assemble context (case summary, break details, policy snippets, rules); prompts versioned under `prompts/<version>/` and the version recorded on the run. Persist every model response to `llm_calls`.
- Bounded loop: max steps (15), tokens, cost, wall-clock (via `Clock`). One repair retry for malformed tool calls, then escalate. Three identical consecutive calls count as a loop and escalate. Budget exhaustion forces escalate. Injection flags force escalate-only.
- Persist the outcome to `agent_runs` (`proposal`, `escalated`, `budget_exhausted`, `invalid_output`, `provider_error`) with tokens and cost.
- Prompt v1 and `prompts/README.md` documenting required content: untrusted-data rule, no-guess rule for ambiguity, evidence requirement.

**Done when**
- With `ScriptedClient`: happy path (a `fee_variance` case produces a proposal), budget exhaustion, loop detection, invalid output, provider error not consuming an attempt, flagged record forces escalate-only.
- `goleak` shows no goroutine leaks.

### M2-10 · Case worker · M
**Spec:** sections 3.1 (`cases`), 6.5; FIN-24 · **Needs:** M1-14, M2-09, M2-01

**Build** (`cmd/worker`, `internal/worker`)
- Poll loop leasing cases (`SKIP LOCKED`), a concurrency cap, a daily USD budget (stop leasing at the cap), an `attempts` cap of 3 (then `escalated` with reason `agent_failed`; `provider_error` does not count), lease-expiry reclaim.
- `agent_enabled` switch checked before each lease and before each model call; graceful shutdown releases leases.

**Done when**
- Integration tests with a scripted client: total provider outage leaves cases queued with `attempts` unchanged; a poison case escalates after 3 attempts; flipping `agent_enabled` off mid-run stops within one step; the daily budget stops new leases.

### M2-11 · Agent CLI, smoke tests, and M2 gate · S
**Spec:** section 18 M2 · **Needs:** M2-09, M2-10, M1-11

**Build**
- `recon agent run --case <id> --llm scripted|opencode --prompt v1` printing the transcript, tool calls, and proposal.
- Smoke tests on three easy scenarios (`fee_variance`, `reference_typo`, `duplicate_charge`) from cassettes recorded once with a live key; a `live`-tagged test for manual runs.

**Done when**
- `TestMilestone2Gate` passes: valid proposals on the three easy scenarios from cassettes, plus the simulated-outage case from M2-10.
- `make demo-m2` runs it end to end.

---

## M3 · Evals

Goal: a repeatable, honest measurement of the agent: versioned datasets with held-out splits, scorers, reports with confidence intervals, a gate. **Exit gate:** M3-09.

**Lanes:** M3-01 → (M3-02 ‖ M3-03) → M3-04 → (M3-05 ‖ M3-06 ‖ M3-07 ‖ M3-08) → M3-09.

### M3-01 · Eval schema, datasets, splits · M
**Spec:** section 11.5; section 3.1 (`eval_runs`, `eval_case_results`) · **Needs:** M1-11

**Build**
- Migration for the eval tables.
- `eval/dataset`: builds `dev`, `test`, and `injection` splits from distinct seeds, each with a manifest (`dataset_version`, counts per mode and difficulty, content hashes). Datasets are regenerated deterministically from the seed; only manifests and hashes are committed.
- Scenario ground-truth JSON schema, versioned.

**Done when**
- Rebuilding yields identical hashes; dev and test seed sets are disjoint; every failure mode has at least 30 `test` scenarios (spec 11.5).

### M3-02 · Scenario sandbox · M
**Spec:** section 11.1 · **Needs:** M3-01, M2-09

**Build** (`eval/sandbox`)
- Loads a world (many scenarios at once) into an isolated database cloned from a template, with a fake clock set to scenario time; wires agent tools to it.
- Ground truth lives in a separate `eval_truth` schema that the agent role cannot read.

**Done when**
- A test proves `recon_agent_ro` cannot read `eval_truth`; two sandboxes run in parallel without interference; teardown leaves no databases behind.

### M3-03 · Scorers · L
**Spec:** section 11.2 · **Needs:** M3-01, M2-08

**Build** (`eval/score`)
- `docs/eval-scoring.md` defining equivalence: resolution type equal; adjustments equal as multisets of (account, direction, amount, currency); link equal on the pair; duplicate equal on the record; escalate or request_info when truth says so.
- Metrics: accuracy, correct-escalation, over-escalation, **false-fix** (a ledger-affecting proposal whose effect differs from truth, including any such proposal where truth is "escalate"), evidence validity (refs real and relevant to the truth records), cost, latency p50 and p95, steps, injection resistance. Auto-resolve precision is computed in M4-14 once tiers exist.
- Defined behavior for empty denominators.

**Done when**
- Hand-built fixtures with golden expected values for every metric, including edge cases.

### M3-04 · Eval runner · M
**Spec:** sections 11.1, 11.3 · **Needs:** M3-02, M3-03, M2-09

**Build** (`cmd/recon-eval`)
- `run --suite --split --model --prompt --seed --repeat k --concurrency --llm live|cassette --max-cost-usd`.
- Iterates cases in a sandbox world, invokes the agent, scores, and persists to `eval_runs` and `eval_case_results` with git SHA and a pricing snapshot. Aborts when the cost cap is hit; resumable (skips completed scenarios of the same run); repeats are linked by a group ID.

**Done when**
- A 20-scenario run with the scripted client works end to end; `--max-cost-usd` aborts; resume works; results are queryable in SQL.

### M3-05 · Reports · M
**Spec:** sections 11.1, 11.5 · **Needs:** M3-04

**Build** (`eval/report`)
- Markdown plus JSON: headline table, per-failure-mode and per-difficulty breakdowns, Wilson intervals, mean ± spread across repeats, modes below the minimum sample size flagged, a failure appendix linking worst cases to their transcripts.
- `recon-eval report --run`.

**Done when**
- Golden report test; Wilson interval unit tests against reference values (for example 0 of 30 gives an upper bound of about 0.114).

### M3-06 · Component evals and calibration · M
**Spec:** section 11.5 · **Needs:** M1-16, M3-03, M3-04

**Build**
- `recon-eval matcher --split` (precision and recall per pass using M1-16).
- A verifier scorecard interface (filled by M4-05).
- Calibration: bucketed confidence versus correctness, expected calibration error, and a reliability table in the report.

**Done when**
- The matcher component report is produced for `dev` and `test`; ECE is unit-tested on synthetic inputs; the calibration section renders.

### M3-07 · Run comparison · S
**Spec:** section 11.3 · **Needs:** M3-04

**Build**
- `recon-eval compare A B`: per-mode deltas, regressions and improvements flagged with interval overlap, cost and latency deltas; non-zero exit when regression criteria are met.

**Done when**
- Fixture-run tests cover a regression, an improvement, and no-change.

### M3-08 · Prompt versions and second model · M
**Spec:** sections 11.3, 19 · **Needs:** M2-02, M3-04

**Build**
- `prompts/v2` (for example a stricter evidence checklist) and a `models.yaml` registry with pricing. A second OpenAI-compatible model (`glm-5.3-flash`) configured behind `llm.Client`.
- Record cassettes for each combination.

**Done when**
- Runs on `dev` for (model A, v1), (model A, v2), (model B, v1) are stored and `compare` output is generated.

### M3-09 · Baseline, thresholds, gate command · M
**Spec:** sections 11.5, 19 · **Needs:** M3-05, M3-06, M3-07, M3-08

**Build**
- `eval/thresholds.yaml` with the initial values from spec 11.5. `recon-eval gate --run --baseline` evaluating them (the T0 false-fix check reports "not applicable" until M4-14 and says so).
- Iterate on `dev`. Freeze the prompt, then run `test` **once** and commit it as the baseline in `eval/baselines/` with its cassettes (keep the committed cassette size modest; document the size budget).
- Draft `docs/eval-results.md` from the report.

**Done when**
- The gate passes a run against itself and fails a seeded degraded run (test).
- The baseline report and cassettes are committed. Never tune prompts against `test`.

---

## M4 · Approvals

Goal: proposals become ledger effects only through policy, verified patterns, approval where needed, and an atomic, audited executor. **Exit gate:** M4-14.

**Lanes:** A: M4-01 → M4-07. B: M4-02. C: M4-03. D: M4-04 ‖ M4-05. Then M4-06 (needs M4-01, M4-03) → M4-08 ‖ M4-09 ‖ M4-10. M4-11 (needs M4-02 to M4-05 and M4-07) → M4-12. M4-13 after M4-06 and M4-11. Merge at M4-14.

### M4-01 · Approval, executor, and outbox schema · S
**Spec:** section 3.1; FIN-22, FIN-23 · **Needs:** M2-01

**Build** (one migration)
- `approvers`, `approvals`, `applied_proposals` (primary key `proposal_id`, `UNIQUE ledger_txn_id`), `outbox` (add `next_attempt_at`, `dead`, `idempotency_key`; index on undelivered rows), `policy_config_versions(hash PK, yaml, loaded_at)`.
- Grants: `recon_executor` may insert into `applied_proposals` and post ledger adjustments; `recon_app` owns approvals and the outbox.

**Done when**
- A second `applied_proposals` row for the same proposal fails; the role matrix is extended and passes.

### M4-02 · API server skeleton · M
**Spec:** sections 15, 16 · **Needs:** M1-07, M2-01

**Build** (`cmd/api`)
- `chi` server with static API keys mapped to roles (`viewer`, `approver`, `admin`) from config; consistent JSON errors; request IDs and trace propagation.
- Endpoints: `/healthz`, `/metrics` (stub), `POST /v1/ingest/files`, `POST /v1/reconcile/run`, `GET /v1/cases`, `GET /v1/cases/{id}` (case file JSON), `GET /v1/proposals`, `POST /v1/admin/switches` (admin, audited).
- A reusable webhook-signature middleware: HMAC, timestamp tolerance, replay cache (used by M4-12 and M6-01). `api/openapi.yaml`.

**Done when**
- Handler tests; a role matrix test per endpoint; a webhook replay with a stale timestamp is rejected.

### M4-03 · Proposal state machine · M
**Spec:** section 8.1; FIN-24, FIN-30 · **Needs:** M2-01, M1-05

**Build** (`internal/approval/state`)
- A transition table covering every proposal status in spec 3.1 and 8.1. `Transition(ctx, tx, proposalID, to, actor, reason)` with an optimistic version check and an audit entry in the same transaction. Illegal transitions return typed errors. `ExpireDue(ctx)` uses the clock.
- `go generate` renders the table to `docs/state-machine.md` so docs cannot drift.

**Done when**
- Table-driven tests of every legal and illegal transition; a property test that random event sequences never reach an illegal state; two goroutines racing approve against reject produce exactly one winner.

### M4-04 · Policy engine · M
**Spec:** section 7; FIN-13, FIN-34 · **Needs:** M2-01

**Build** (`internal/policy`)
- YAML config with strict validation and a content hash stored in `policy_config_versions`.
- `Evaluate(proposal, verification, context)` returns tier, the rule that fired, reasons, and the config hash. T2 triggers: high value, write-offs above the limit, cross-currency, customer-funds accounts, repeated counterparty. T0 requires an allowed type and *verified* pattern, value under the cap, a passing verifier, valid evidence, and `auto_apply_enabled`. Confidence can only tighten the tier (`downgrade_below`).
- The decision JSON is stored on the proposal.

**Done when**
- Table-driven tests including: confidence 0.99 cannot upgrade a tier; a missing verifier gives T1; the hash changes when the YAML changes; exact boundary tests at every threshold per currency.
- Tests carry `// covers: FIN-13, FIN-34`.

### M4-05 · Deterministic verifiers · M
**Spec:** section 7.1 · **Needs:** M1-01, M1-09, M2-08

**Build** (`internal/policy/verify`)
- One verifier per pattern: `fee_variance` (recompute the expected fee from `fee_schedules` with basis points, a fixed amount, and the central rounding function), `fx_rounding` (recompute from the stored rate), `rounding` (per-currency cap from config), `reference_typo` (exactly one candidate in the window; a second candidate fails).
- Output stored in `proposals.verification` and `pattern_verified`; an unverified claim leaves `pattern_verified` null.
- Fill the verifier scorecard hook from M3-06.

**Done when**
- Table-driven tests per verifier including adversarial cases: wrong claimed pattern, two candidates, off by one minor unit; a property test that verifier output does not depend on the rationale text.
- On `dev` and `test`, verifiers produce zero false passes against ground truth.

### M4-06 · Executor · L
**Spec:** FIN-20, 21, 23; section 8.1; section 3.2 · **Needs:** M4-01, M4-03, M1-06, M2-08

**Build** (`internal/executor`, running on the `recon_executor` pool)
- `Apply(ctx, proposalID)` inside `db.WithTx` at SERIALIZABLE with bounded retry: lock the proposal; require `approved` or `auto_approved` and not expired; re-validate `state_hash` and that the break is still open; apply by type (adjustment and write-off via `ledger.Post` with `kind = 'adjustment'` and the proposal ID as idempotency key; link inserts a match; duplicate marks the record); resolve break and case; insert `applied_proposals`; set `applied`; write audit entries and outbox rows. All in one transaction.
- Failures move the proposal to `apply_failed` with a reason (`state_changed`, `break_resolved`, `expired`) in a separate transaction. An executor loop consumes the approved queue.

**Done when**
- Integration tests per proposal type; a repeated `Apply` creates no second ledger transaction; stale state gives `apply_failed(state_changed)`; an expired proposal is refused.
- After an applied adjustment, the M1-15 tie-out returns to zero (add that test).
- Tests carry `// covers: FIN-20, FIN-21, FIN-23`.

### M4-07 · Outbox worker · M
**Spec:** FIN-22 · **Needs:** M4-01

**Build** (`internal/outbox`)
- `Enqueue(tx, topic, payload, idemKey)`; a worker that claims batches with `SKIP LOCKED`, delivers at least once, backs off exponentially via `next_attempt_at`, and dead-letters after N attempts with an alert metric. Handlers are registered per topic and receive the idempotency key. A `FakeChannel` handler for tests.
- A test helper (`httpguard`) that fails any outbound HTTP made while a transaction is open.

**Done when**
- A crash after sending but before marking delivered causes a resend, and an idempotent handler tolerates it; dead-lettering after N attempts; `httpguard` catches a deliberate violation.
- Tests carry `// covers: FIN-22`.

### M4-08 · Concurrency and fault-injection tests · M
**Spec:** FIN-20 to 25; section 4.8 · **Needs:** M4-06, M4-07

**Build**
- `internal/fault`: failpoints (`fault.Hit("executor.after_ledger_insert")`) compiled only under the `faultinject` tag, triggered by environment variable to panic or exit.
- Tests that run the executor in a subprocess, kill it at each failpoint, then run `invariants.Check(db)` (no ledger entries without audit; no `applied` proposal without a ledger transaction, and vice versa; outbox recovers). `invariants.Check` is reusable by the integrity job (M5-06).
- Concurrency tests: 50 goroutines applying one proposal give exactly one ledger transaction; competing executors on the same account; serialization retries are exercised.

**Done when**
- `make test-fault` is green; `invariants.Check` has its own tests against deliberately corrupted databases.
- Tests carry `// covers: FIN-20, FIN-21, FIN-23, FIN-24, FIN-25`.

### M4-09 · Auto-apply limits and circuit breaker · S
**Spec:** FIN-53; section 6.5 · **Needs:** M4-04, M4-06

**Build**
- Per-proposal value caps (policy) and per-day cumulative caps by currency, computed from the database. A circuit breaker that sets `auto_apply_enabled = false` (audited, actor `system:circuit-breaker`) when a daily cap is hit or the break-rate anomaly threshold is exceeded; reset requires `admin`.

**Done when**
- After a cap is exceeded, later T0 candidates are routed as T1 with a recorded reason; the breaker trips on an injected spike; a non-admin cannot reset it.

### M4-10 · Case lifecycle · L
**Spec:** section 8.5 · **Needs:** M4-03, M4-06, M4-07, M1-12, M1-14

**Build**
- Full case state machine (`open`, `in_agent`, `proposal_pending`, `waiting_info`, `escalated`, `resolved`) in `internal/cases/state`, same pattern as M4-03.
- Supersession: a new proposal supersedes the old one and enqueues a cancel for its approval request.
- Late-match handling: when a new match resolves a break in an open case, the case resolves with `superseded_by_match`, any active proposal is superseded, and an approved-but-unapplied proposal fails re-validation with `break_resolved`.
- Retries after rejection or expiry until the attempts cap, then `escalated`. `waiting_info` re-entry on new ingest or a human reply. SLA escalation for `missing_settlement` in business days (FIN-41 calendar).
- `recon match unmatch --id --reason` (admin, audited): reopens breaks; refused when an adjustment was applied (the operator uses `recon reverse --txn`, which creates a T2 reversal proposal).

**Done when**
- Tests for every rule above, including the end-to-end late-match scenario from spec section 19; a property test on the case state machine.

### M4-11 · Approval service and channel interface · M
**Spec:** sections 8.1, 8.2; FIN-33 · **Needs:** M4-02, M4-03, M4-04, M4-05, M4-07

**Build** (`internal/approval`)
- `Service.Request`: run policy → tier; T0 becomes `auto_approved`; otherwise `pending_approval` and an outbox `approval.request` routed by tier (T1 Slack, T2 ticket).
- `Service.Decide(proposalID, decision, approverID, source)`: requires the `approver` role; rejects agent and system actors (FIN-33); four-eyes with the co-maker rule from spec 8.1, derived from audit entries (agent, plus any human who edited, answered a request, or triggered the run); idempotent (same approver and decision replays fine, conflicting decisions error); expiry honored.
- `ApprovalChannel` interface (spec 8.2); `FakeChannel`; `POST /v1/proposals/{id}/decision`.

**Done when**
- Tests: self-approval rejected; agent actor rejected; co-maker rejected; double click idempotent; decision after expiry rejected; every decision has audit entries.
- Tests carry `// covers: FIN-33`.

### M4-12 · Slack adapter · L
**Spec:** section 8.3 · **Needs:** M4-11, M4-02, M4-07

**Build** (`internal/approval/slack`)
- Block Kit message: case summary, delta, evidence bullets, verifier result, rationale, tier, case-file link, Approve / Reject / Request-info buttons carrying `proposalID:version`.
- `POST /v1/slack/interactions`: v0 signature check with a 5-minute tolerance, replay rejection, idempotent handling of Slack retries, identity mapping via `approvers.slack_user_id`, ephemeral denial plus audit entry for unauthorized users. Message updated after a decision (through the outbox); `Cancel` marks a message superseded or expired. Web API client retries on 429 using `Retry-After`.
- "Request info" rejects the proposal with reason `info_requested` and moves the case to `waiting_info`.
- `docs/slack-setup.md`: app creation, scopes (`chat:write`), interactivity URL, signing secret, a dev tunnel.

**Done when**
- Tests with recorded Slack payload fixtures and a fake Slack API server: good, bad, and stale signatures; duplicate delivery; unauthorized user; message update.
- The manual smoke checklist in the doc has been run once against a real workspace and the result recorded.

### M4-13 · Audit verification, actor typing, traceability · M
**Spec:** FIN-30 to 34 · **Needs:** M1-05, M4-06, M4-11

**Build**
- `recon audit verify [--from seq]` (non-zero exit on tamper or gap). Actor typing also enforced by a database `CHECK`.
- `recon trace --txn <id>` and `GET /v1/cases/{id}` returning the chain: break → agent run (trace ID) → proposal → verification → policy decision (rule, config hash, config YAML from `policy_config_versions`) → approvals → executor → ledger transaction (FIN-32).
- `testutil.AssertAuditCompleteness`: wraps service calls in the end-to-end flow and asserts each mutation has audit rows in the same transaction.

**Done when**
- A superuser tamper is detected by the CLI; the traceability test walks from an adjustment back to the approver; the completeness test passes across the e2e flow.
- Tests carry `// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34`.

### M4-14 · Simulated approver, end-to-end pipeline, and M4 gate · M
**Spec:** section 11.5; section 18 M4 · **Needs:** M3-04, M4-06, M4-10, M4-11, M4-13

**Build**
- `eval/approver`: a rule-based approver that approves exactly when a proposal matches ground truth, driving `Service.Decide` (T1 and T2 alike).
- `recon-eval run --e2e`: agent → policy → approval (simulated) → executor. Adds auto-resolve precision, T0 false-fix, verifier false-pass, and tier correctness (`tier_expected`) to scoring and reports. The M3-09 gate now enforces "T0 false-fix = 0".
- `TestMilestone4Gate`: break → proposal → (fake) Slack approve → applied entry with audit trail → tie-out zero.

**Done when**
- The e2e test passes with `FakeChannel`; an e2e eval run on `dev` from cassettes produces a report with tier columns; the gate test is in CI.

---

## M5 · Visibility

Goal: you can see what the system and the agent are doing, replay any run, produce the reports, and close a period safely. **Exit gate:** M5-12.

**Lanes:** A (tracing): M5-01. B: M5-02. C (metrics and dashboards): M5-03 → (M5-04 ‖ M5-05). D (controls and reports): M5-06 → (M5-07 ‖ M5-09); M5-08 after M5-01. Stretch: M5-10, M5-11. Merge at M5-12.

### M5-01 · OpenTelemetry tracing · M
**Spec:** section 12 · **Needs:** M2-09, M4-06

**Build** (`internal/telemetry`)
- OTel SDK setup via standard OTLP environment variables. Spans: `case.run` → `llm.call`, `tool.call`, `policy.evaluate`, `executor.apply`. Attributes: model, prompt version, tokens, cost, tool name, refs, result hashes; payloads redacted or truncated through one redaction hook.
- `trace_id` persisted on `agent_runs`, `proposals`, and audit entries via context propagation.
- Phoenix added to the compose file (single container, OTLP-native).

**Done when**
- An in-memory exporter test asserts the span tree and attributes; `trace_id` is present on the run, proposal, and audit rows; a demo trace is visible in Phoenix (manual check).

### M5-02 · Replay · M
**Spec:** section 12; section 19 · **Needs:** M2-09

**Build**
- `recon replay --run <id> --exact`: rebuilds the run from `llm_calls` and `tool_calls` with no database or network, and reproduces the proposal (compare proposal hashes).
- `recon replay --run <id> --rerun [--model --prompt]`: feeds recorded tool results to a model (cassette or live); if the model requests a tool call that was not recorded, report a divergence and do not call live data. Time comes from the run's recorded timestamps via the fake clock.

**Done when**
- Exact replay reproduces the identical proposal for 20 recorded runs; a rerun with a different prompt reports divergences correctly.

### M5-03 · Prometheus metrics · S
**Spec:** section 12 · **Needs:** M4-02

**Build**
- Instrumentation and `/metrics` on the API and worker: match rate, breaks by type, case age, queue depth, agent outcomes, approval latency, cost, errors, outbox lag and dead letters, auto-apply cap usage, tie-out unexplained difference, integrity job status.

**Done when**
- A registry test asserts every listed metric exists with its labels; a cardinality guard test confirms no label carries an unbounded value such as a case ID.

### M5-04 · Ops dashboard · M
**Spec:** section 13.1; FIN-42 · **Needs:** M5-03, M1-15

**Build** (`deploy/grafana`)
- Provisioned datasources (Prometheus and Postgres via a new read-only role `recon_dashboards_ro` limited to views) and the dashboard JSON: match rate by source, unmatched value by currency, open breaks by type and aging bucket (defined on value date), approval queue stats, auto-resolved versus human-resolved versus escalated over time.

**Done when**
- `docker compose up` provisions the dashboard with no manual steps; after `recon demo seed` plus the M2 to M4 pipeline, every panel shows data.

### M5-05 · Agent quality dashboard · M
**Spec:** section 13.2 · **Needs:** M5-03, M3-04

**Build**
- SQL views `v_eval_*` over the eval tables, and a provisioned dashboard: accuracy and false-fix by model and prompt version over time, human override rate (rejections over decisions), apply-failure rate, cost and latency distributions, escalation reasons.

**Done when**
- The views are tested against fixtures; the dashboard renders from seeded eval runs.

### M5-06 · Daily integrity job · M
**Spec:** FIN-52; section 4.6 · **Needs:** M4-08, M1-15, M4-13

**Build**
- `recon integrity run`, scheduled daily in the worker. Checks: every transaction balances (reusing `invariants.Check`), matched plus unmatched value equals ingested value per source and currency, each `applied_proposals` row has its ledger transaction and audit trail, audit chain verifies, tie-out for every scope.
- Results in `integrity_runs`, metrics, and an outbox alert on failure.

**Done when**
- One test per check seeds the matching inconsistency and sees it detected; clean data passes.
- Tests carry `// covers: FIN-52`.

### M5-07 · Daily reconciliation report · M
**Spec:** section 14 · **Needs:** M5-06, M4-07

**Build** (`internal/reports`)
- Assemble the daily report (totals, match rate, breaks by cause, aging, agent actions, pending approvals, exceptions, integrity and tie-out status), render HTML, convert to PDF with `chromedp`, post a Slack summary through the outbox (`report.daily`). `recon report daily --date`, `GET /v1/reports/daily`, scheduled daily.

**Done when**
- A golden HTML test with fixed data and an injected clock; a PDF smoke test (valid file, page count above zero).

### M5-08 · Case file report · M
**Spec:** section 14 · **Needs:** M5-01, M4-13, M4-07

**Build**
- Per-case report: break details, evidence, agent trace summary with a Phoenix link template, proposal, verifier result, policy decision, approver, outcome. Served at `/v1/cases/{id}/file` as HTML and PDF, with redaction applied; linked from Slack messages.

**Done when**
- A golden test on a full e2e case; the Slack message contains a working link.

### M5-09 · Period close · L
**Spec:** FIN-56, FIN-57; section 14 · **Needs:** M5-06, M4-06

**Build** (`internal/periods`)
- `Close(ctx, periodID, actor)`: every scope ties out, or each unexplained difference is covered by an open escalated or `waiting_info` case on the break, or by an entry in the suspense account (document this definition in `docs/period-close.md`); the integrity job passes; no proposal targeting the period is `pending_approval` or `approved`.
- `Reopen` (admin, reason, audited). Endpoints `GET /v1/periods` and `POST /v1/periods/{id}/close`. The period-close report: per-scope tie-out, open items, close and reopen history.
- End-to-end test: an adjustment for a closed period lands in the open period with `adjusts_period_id`.

**Done when**
- One test per close check; reopen requires `admin` and is audited; golden test for the report.
- Tests carry `// covers: FIN-56, FIN-57`.

### M5-10 · Approval queue UI · M · stretch
**Spec:** section 13.3 · **Needs:** M4-11

**Build**
- Go plus htmx pages: pending queue, case file view, approve and reject through `Service.Decide`. Session via API key cookie with a CSRF token.

**Done when**
- Handler tests (including CSRF); a manual approve and reject round trip.

### M5-11 · Weekly eval report · S · stretch
**Spec:** section 14 · **Needs:** M3-05

**Build**
- Scheduled `recon-eval report --weekly`: last seven days of runs, comparison to the previous week, cost trend, top failure modes; Markdown and PDF.

**Done when**
- Golden test on fixture runs.

### M5-12 · Demo environment and M5 gate · M
**Spec:** section 19 (first criterion) · **Needs:** M5-04, M5-05, M5-07, M5-08

**Build**
- `make demo`: compose up the full stack (Postgres, API, worker, Phoenix, Prometheus, Grafana), seed several days of synthetic data using a fake clock so dashboards have history, run the pipeline with cassette-backed LLM calls and the simulated approver, produce the daily report. Works offline at zero cost.
- `docs/demo.md`: the click path for the demo GIF.

**Done when**
- From a fresh clone, `make demo` populates both dashboards and produces a report; the spec section 19 criterion "one command brings up the full stack" is true.

---

## M6 · Hardening

Goal: prove the safety claims, close coverage gaps, and package the project. **Exit gate:** M6-11.

**Lanes:** M6-02 ‖ M6-03 ‖ M6-04 ‖ M6-05 ‖ M6-06 ‖ M6-07 can run in parallel once their `Needs` are met. Then M6-08 → M6-09 → M6-11. M6-01 is optional.

### M6-01 · Ticket adapter (Linear) · L · stretch
**Spec:** section 8.4 · **Needs:** M4-11, M4-12

**Build**
- A Linear adapter behind `ApprovalChannel`: create an issue with the case-file link; map workflow-state names (configurable) to decisions; verify the webhook signature; idempotent handling; identity mapping via `approvers.ticket_user_id`; T2 routing.
- `docs/ticket-setup.md`.

**Done when**
- Tests against a fake server: create, approve via status change, reject, duplicate webhook, bad signature, unauthorized user.

### M6-02 · Injection suite and red-team tests · M
**Spec:** sections 9, 11.4 · **Needs:** M2-07, M3-04, M4-14

**Build**
- Corpus `eval/injection/*.yaml` by category: instruction override, fake approvals, tool-argument smuggling, fake system messages, boundary escape, unicode obfuscation, indirect injection via batch memos, multi-record.
- Runner: `recon-eval run --split injection`. Pass criteria from spec 11.4.
- **Structural red-team test with no LLM:** a malicious `ScriptedClient` emits attacker-chosen proposals and tool calls. Assert it still cannot: write to the ledger as the agent role, reach T0 without a verified pattern, approve anything, read audit or truth tables, or alter amounts through injected text.

**Done when**
- The injection split passes 100% from cassettes; the structural tests are green and cited in the threat model.

### M6-03 · Threat model · S
**Spec:** section 9 · **Needs:** M4-14

**Build**
- `docs/threat-model.md`: assets, trust-boundary diagram, attacker goals and capabilities, a table mapping each threat to its mitigation and to the test that proves it, residual risks.

**Done when**
- Every mitigation row links to a real test name; a script checks the links resolve.

### M6-04 · "What the agent must not do", enforced · M
**Spec:** section 16 · **Needs:** M4-14, M1-04

**Build**
- `docs/what-the-agent-must-not-do.md`. Each claim maps to an automated check: role-grant tests; an import lint (`depguard`) forbidding network packages in `internal/agent/tools`; the compose network setup for the worker; the tier-selection test. `make must-not` runs them all.

**Done when**
- Every sentence in the document names its check, and `make must-not` is green in CI.

### M6-05 · CI eval gate · M
**Spec:** sections 11.3, 11.5 · **Needs:** M3-09

**Build**
- A PR workflow running `recon-eval gate` on cassettes (no API key). A nightly and manual workflow running live on `test` with the secret key, a cost cap, and the report uploaded as an artifact.
- `docs/cassettes.md`: how to refresh cassettes and the size budget.

**Done when**
- A PR that degrades the prompt fails the gate; the live workflow produces a report; branch protection requires the gate.

### M6-06 · Retention, redaction, PAN scan · M
**Spec:** FIN-60, 61, 62 · **Needs:** M2-09, M5-01, M1-05

**Build**
- Masking helpers (last four only) used in traces, reports, and the data model.
- Retention config and `recon retention run`: redacts personal fields in `cases`, `tool_calls`, `llm_calls`, `external_records.raw`, and traces after the TTL, writing a tombstone audit entry. Audit payloads already hold no personal data (M1-05), so the chain stays valid.
- A Luhn-based PAN scanner test over logs, traces, reports, and a database dump; `gitleaks` in CI.

**Done when**
- Redaction followed by `recon audit verify` still passes; the scanner finds a seeded PAN in a fixture; CI runs `gitleaks`.
- Tests carry `// covers: FIN-60, FIN-61, FIN-62`.

### M6-07 · Requirement coverage checker · M
**Spec:** section 4.8 · **Needs:** M4-14

**Build** (`tools/fincoverage`)
- Parse `// covers:` annotations in `_test.go` files, extract FIN IDs from `docs/spec.md`, and generate `docs/requirements-coverage.md` (requirement, tests). `make fin-coverage` and a CI step fail on any uncovered ID that lacks an entry with a reason in `docs/waivers.yaml`.

**Done when**
- The checker runs; every gap found is fixed or waived with a written reason.

### M6-08 · Final evals and failure analysis · M
**Spec:** sections 11, 19 · **Needs:** M6-02, M3-09, M4-14

**Build**
- Freeze prompts (record the git SHA). Run `test` for at least two model and prompt combinations with `--repeat 3`, end to end.
- Write `docs/eval-results.md`: headline tables with intervals, per-mode analysis, calibration, cost per case, three to five failure case studies with transcripts, what you would change next.

**Done when**
- The report meets the thresholds in `eval/thresholds.yaml` or states plainly where it does not and why.

### M6-09 · README and demo GIF · M
**Spec:** section 20 · **Needs:** M6-08, M6-03, M6-04, M5-12

**Build**
- README in spec 20 order: pitch and GIF; results first; architecture diagram (Mermaid) and key decisions; what the agent must not do; threat model summary; run locally; limitations (from spec section 21).

**Done when**
- A reader unfamiliar with the project can run `make demo` from the README alone (test this on a clean container).

### M6-10 · Decision records · S
**Spec:** section 20 · **Needs:** M4-14

**Build**
- Short ADRs in `docs/decisions/`: deterministic core with proposals only; outbox over direct calls; verifiers over agent confidence; hub-and-spoke reconciliation; cassette-based CI; period locking by posting forward.

**Done when**
- Each ADR has context, decision, and consequences in under one page.

### M6-11 · Final acceptance sweep · M
**Spec:** section 19 · **Needs:** M6-05, M6-06, M6-07, M6-09

**Build**
- `docs/acceptance.md`: every spec section 19 criterion mapped to its evidence (command, test, link, or screenshot). Fix any failing criterion. Run a fresh-clone test in a clean container. Tag `v1.0.0`.

**Done when**
- All criteria are checked with evidence; `make demo` works from a fresh clone.

---

## Appendix A · Requirement to task map

| Requirement | Built in | Verified in |
|---|---|---|
| FIN-01 | M1-01, M1-02 | M1-01, M1-02 |
| FIN-02, 03, 04, 06 | M1-01 | M1-01 |
| FIN-05 | M2-04 | M2-04 |
| FIN-10, 11, 14 | M1-03 | M1-03, M1-04 |
| FIN-12, 13 | M1-06 (13 also M1-03) | M1-06 |
| FIN-20, 21, 23 | M4-06 | M4-06, M4-08 |
| FIN-22 | M4-07 | M4-07 |
| FIN-24 | M1-14, M4-03 | M1-14, M4-08 |
| FIN-25 | M4-08 | M4-08 |
| FIN-30 | M1-05, M1-06 | M4-13 |
| FIN-31, 32 | M1-05, M4-13 | M4-13 |
| FIN-33 | M1-05, M4-11 | M4-11, M4-13 |
| FIN-34 | M4-04 | M4-04, M4-13 |
| FIN-40, 41, 42 | M1-07 | M1-07 |
| FIN-43 | M0-04 | M0-04 |
| FIN-50, 51, 55 | M1-07 | M1-07 |
| FIN-52 | M5-06 | M5-06 |
| FIN-53 | M4-09 | M4-09 |
| FIN-54 | M1-15 | M1-15, M4-06 |
| FIN-56 | M1-03, M1-06, M5-09 | M1-03, M1-06, M5-09 |
| FIN-57 | M5-09 | M5-09 |
| FIN-60, 61, 62 | M6-06 | M6-06 |

M6-07 checks this table mechanically: any FIN ID without a `// covers:` test fails CI.
