# Open Questions

Add an entry here instead of guessing or editing `docs/spec.md`. Questions and
non-blocking findings that do not change approved scope belong here.

## Template

| Field | Content |
|---|---|
| ID | Q-NN |
| Finding / question | What is unclear or contradictory |
| Target task or section | Roadmap task or spec section |
| Proposed change | Smallest change that would resolve it |
| Status | OPEN / RESOLVED / QUEUED |

## Queued non-blocking items (Q-01..Q-19)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-01 | FIN-53 is mapped in Appendix A but M4-09 does not require `// covers: FIN-53`. | M4-09 | Add annotation instruction. |
| Q-02 | **RESOLVED (M1-15):** M1-15 tests carry `// covers: FIN-54`; the tie-out suite and the dependency guard are annotated. | M1-15 | Recorded as an M1-15 acceptance rule. |
| Q-03 | **RESOLVED (M1-16):** `TestMilestone1Gate` is a documented subset gate: it asserts annotated tests for FIN-01–06, 10–14, 50, 51 and 55 only. M1 tests also cover FIN-24, FIN-30/31/33, FIN-40/41/42/43, FIN-54 and FIN-56, but those annotations are outside the gate assertion; full FIN coverage is M6-07. The rationale is in `internal/matchscore/annotations.go`, the gate test, and ADR 0004. | M1-16 | Recorded as the subset-gate rule (D-8); full coverage checker remains M6-07. |
| Q-04 | Spec §19 says `docker compose up` seeds the full stack; M5-12 says `make demo`. | Spec §19 / M5-12 | Choose `make demo` or add a seed service; record the deviation. |
| Q-05 | Spec §15 lists webhook ingestion; roadmap §3.1 says v1 is batch-only and M4-02 omits it. | Spec §15 / M4-02 | Mark webhook ingestion deferred for v1. |
| Q-06 | Spec §8.1 shows `apply_failed → escalated`; proposal statuses have no `escalated`. | Spec §8.1 / M4-03 | Clarify proposal vs case transition. |
| Q-07 | **RESOLVED (M1-10):** `partial_refund` ground truth uses the HD-3 contract: `expected_resolutions []string` plus `primary_resolution string`; the allowed set is exactly `["link","adjustment"]` and the scenario sets `primary_resolution` to the intended path (`adjustment`). Scorers must accept any allowed value; M1-10/M1-16 must not assume a single canonical resolution. | M1-10 / M3-03 | Recorded as the HD-3 contract; the M1-10 tests assert the exact set and primary membership. |
| Q-08 | **RESOLVED (M1-14):** data-model deltas omit `settlement_batches` columns, `outbox` columns, and `recon_dashboards_ro`; the M1-owned deltas are now all recorded, while `outbox` columns belong to M2 and `recon_dashboards_ro` to M5. M1-07 adds `external_records.value_date DATE` (FIN-42) and `settlement_batches.status`, `quarantine_reason`, `row_count`, `declared_row_count` (FIN-50). M1-12 adds `match_runs`, the `matches` header (N:1 representation, D-4), and `match_legs` (`UNIQUE(match_id, ledger_txn_id)`, partial unique active-leg index, status-sync trigger). M1-14 adds `breaks.signature TEXT NOT NULL UNIQUE` (SHA-256 hex of `match.BreakCandidate.Signature()`, so re-runs use `ON CONFLICT (signature) DO NOTHING`), `breaks.ledger_txn_ids UUID[]`/`breaks.external_refs JSONB` instead of the spec's singular `ledger_txn_id`/`external_record_id` (a composite break covers N of each), `cases.version`/`attempts` non-negative with lease-field coherence checks, and `case_breaks(case_id, break_id)` with FKs and `UNIQUE(break_id)` instead of the spec's `cases.break_ids UUID[]`. | Roadmap §3.1 | Recorded as M1 schema deltas (D-11). |
| Q-09 | M4-14 references the M3-09 gate but does not list M3-09 in `Needs`. | M4-14 | Add M3-09 to `Needs`. |
| Q-10 | **RESOLVED (M1-15):** tie-out is FIN-54; FIN-55 is statement continuity owned by M1-07 and is documented in `docs/tieout.md` as an input assumption (D-9). | M1-15 | Recorded as an M1-15 documentation rule. |
| Q-11 | Spec §22 presents ticketing/tracing/fee source/batch-only as open; roadmap §3.1 decides them. | Spec §22 | Mark resolved or point to roadmap §3.1. |
| Q-12 | README is a one-line stub; M6-09 writes the final README. | M0-01 | Add a minimal README with prerequisites, docs links, and "work in progress" (non-blocking). |
| Q-13 | AGENTS.md can drift if copied from roadmap §1–2. | M0-01 / ongoing | Keep it a short pointer; update both in the same commit (D-15). |
| Q-14 | M1-04 adds compose dev login users; M0-02 creates no init hook. **RESOLVED (M1-04):** `compose/initdb/01-roles.sql` is mounted read-only and creates the four NOLOGIN groups plus four local-only dev logins; see ADR 0005. | M1-04 | Add an init SQL script or document the compose change. |
| Q-15 | M0-03 placeholder job must be removed/replaced when M1-02/M6-05/M6-07 land. **RESOLVED (M1-02):** the M1-02 CI cleanup removed the placeholder; the disabled `future-gates` job now contains only the M6-05/M6-07 TODOs (D-10). | M0-03 / later | Recorded as part of the M1-02 CI cleanup; M6-05/M6-07 remove their TODOs when they land. |
| Q-16 | The planning package lives in `.team/`; it is not published under `docs/`. | Follow-up docs task | Optionally copy to `docs/plan-m0.md` after approval. |
| Q-17 | `deploy/` is in spec §17 but M0 creates no deployment assets. | M5-04 | Create `deploy/` with Grafana/Prometheus at M5-04; record in the layout delta. |
| Q-18 | Spec §17 will remain stale if DP-12 is deferred. | Spec §17 | Apply DP-12 before M0-01; otherwise carry the delta in the M0 plan. |
| Q-19 | Spec §3.1 lists only `audit_log`, but a hash chain alone cannot detect a truncated tail (FIN-31): deleting the last row leaves every remaining link valid. **RESOLVED (M1-05):** the migration adds the append-only `audit_chain_head(seq, row_hash)` mirror, written in the same transaction; `Verify` compares it with `audit_log` and reports a truncated tail or head mismatch. | M1-05 / spec §3.1 | Recorded as an M1-05-owned audit object; `recon_audit_ro` gets SELECT on it so verification is read-only. |

## M2 schema deltas and locked decisions (Q-20..Q-22)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-20 | **RESOLVED (M2-01):** the M2-01 migration records these deltas from spec §3.1. `agent_runs.outcome` and `ended_at` are nullable, with `CHECK (ended_at IS NULL OR outcome IS NOT NULL)` and `CHECK (ended_at IS NULL OR ended_at >= started_at)`; the outcome check has no `running` value. `tool_calls.result` and `result_hash` are `NOT NULL`; `tool_calls.error` carries only the structured error code. `proposals` enforces JSON shape (`payload` object, `evidence` array), `confidence` 0..1, non-empty `state_hash`/`idempotency_key` (unique), type/status/pattern/risk enums, and leaves `pattern_verified`, `verification`, `risk_tier`, `policy_decision`, `supersedes_proposal_id`, and `expires_at` nullable; `escalate` is not a proposal type. The partial unique index `proposals_one_active_per_case` covers exactly `proposed`, `pending_approval`, `approved`, `auto_approved`. `system_switches` is a roadmap addition, not a spec §3.1 table. The `v_agent_*` views are M2 additions with explicit column lists; `recon_agent_ro` has no table-level base-table grants and reads only through the owner-rights views, except the column-level `SELECT (idempotency_key)` conflict-inference grant recorded in Q-22; `v_agent_accounts`, `v_agent_scopes`, `v_agent_case_breaks`, and `v_agent_statement_balances` are intentional additions beyond the roadmap's shorthand list because the read tools need them. `external_records_agent_search_idx` is an M2-01 addition to the M1-07-owned `external_records` table. `approvers` is absent until M4-01; M2-01 asserts absence and no agent grant. `v_agent_external_records` exposes the untrusted `raw` payload and `raw_hash`; `v_agent_fx_rates.rate` is cast to text so it can never be a float. `v_agent_history` follows M1's Q-08 `breaks`/`cases` shape (signature plus `case_breaks`), not the spec's singular break columns. Grant matrix: `recon_app` gets `agent_runs` SELECT/INSERT/UPDATE, `tool_calls`/`llm_calls` SELECT/INSERT, `proposals` SELECT, `system_switches` SELECT/UPDATE; `recon_agent_ro` gets view SELECT plus `INSERT` on `proposals` and column-level `SELECT (idempotency_key)` only (Q-22); `recon_executor` and `recon_audit_ro` get no M2 privileges. Per Q-B, the agent uses a client-generated UUID and no `RETURNING`; a duplicate `idempotency_key` under `ON CONFLICT (idempotency_key) DO NOTHING` is a zero-row no-op, and `recon_agent_ro` has no table-level `SELECT` on `proposals` (only the conflict-target column, Q-22). | M2-01 | Recorded as the M2-01 schema/grant contract; no `docs/spec.md` edit. |
| Q-21 | **RESOLVED (M2-01):** locked decisions for the M2 read tools. History similarity (Q-A): resolve the input signature to its break, then return resolved cases in the same `scope_id`, `side`, `currency`, and exact `delta_minor`, ordered by `resolved_at DESC, case_id`, capped at 20 cases, excluding the current case; unknown signature is `not_found`, an existing signature with no comparable history returns an empty `cases` list. Untrusted references (Q-E): in addition to configured per-source `untrusted_fields`, the read tools mark `reference` (psp_a) and `bank_reference` (bank_x) as untrusted when present in `raw`; `internal/config` defaults are unchanged. FX rate selection (Q-D): return the latest `v_agent_fx_rates` row with `as_of` before `date + 1 day` UTC, ordered `as_of DESC, id DESC`, with no lower bound; `not_found` when none exists, and `rate` is an exact decimal string, never a float. The nullable-run-outcome decision (Q-C) is recorded in Q-20. | M2-05 / M2-06 | Consumed by the read-tool branches; no shared-file edit needed there. |
| Q-22 | **RESOLVED (M2-01 post-QA fix):** `recon_agent_ro` inserts proposals with `ON CONFLICT (idempotency_key) DO NOTHING` for FIN-12 idempotency. PostgreSQL infers the conflict arbiter from the unique constraint and therefore requires `SELECT` on the conflict-target column even for `DO NOTHING` with no `RETURNING`; INSERT-only failed with SQLSTATE 42501 on the real agent login, which the earlier owner-pool test missed. A scratch-database probe on PostgreSQL 16.15 confirmed that column-level `GRANT SELECT (idempotency_key)` is sufficient: the first insert affects one row, a duplicate key affects zero rows with no error, and a second active proposal still raises `23505` on `proposals_one_active_per_case`. The same grant leaves `payload`/`evidence`/other columns, `SELECT *`, `RETURNING`, UPDATE, and DELETE at 42501. The follow-up migration `20261105130000_m2_01_proposal_conflict_grant.sql` adds exactly that column grant; no table-level SELECT is granted. `TestM2AgentProposalGrants` and the duplicate-key case in `TestM2ProposalActiveIndex` run as the ephemeral `recon_agent_ro` login. The grant is conflict-inference only, not a proposal read surface. | M2-01 / M2-08 | Recorded as the M2-01 proposal-insert contract; M2-08 keeps the client-UUID + `ON CONFLICT (idempotency_key) DO NOTHING` + no-`RETURNING` insert shape. |

## M2-07 untrusted-data decisions (Q-23, Q-28..Q-30)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-23 | **RESOLVED (M2-07):** the M2-07 migration `20261106120000_m2_07_injection_flags.sql` is a schema delta from spec §3.1: `agent_runs.injection_flags JSONB NOT NULL DEFAULT '[]'` and `tool_calls.injection_flags JSONB NOT NULL DEFAULT '[]'`, both `CHECK (jsonb_typeof(injection_flags) = 'array')`. No separate flags table. No grant change is needed: `recon_app` already has `UPDATE agent_runs` and `INSERT tool_calls`; `recon_agent_ro` gains no access to either trace table. Q-23 is owned by M2-07. | M2-07 / spec §3.1 | Recorded as the M2-07 schema delta; do not edit spec §3.1. |
| Q-28 | **RESOLVED (M2-07):** detector configuration surface. The package exposes compiled `DefaultPatterns()` plus `NewDetector([]Pattern)` for replacement rules; the runner uses the package default. There is no YAML `agent.untrusted` block in this piece. | M2-07 / M2-09 | Wire YAML configuration in M2-09 if the loop needs a different rule set; no config default change now. |
| Q-29 | **RESOLVED (M2-07):** the benign false-positive corpus is a small hand-written set at `internal/agent/untrusted/testdata/benign_memos.txt` (25 lines), drawn from the M1-09 generator's benign vocabulary (seed 1, realistic, 14 days) and the PSP/bank description shapes in `docs/formats.md`. No generated row is copied verbatim and no PII is present. The rate is measured and logged, not gated. | M2-07 | Keep the corpus documented in `detect_test.go`; calibration remains M6-02. |
| Q-30 | **RESOLVED (M2-07):** run-level flag write split. M2-07 supplies the `injection_flags` column and `tools.SaveRunFlags`; M2-09 calls it when it finalizes the run. M2-07 does not invent a run lifecycle. | M2-07 / M2-09 | M2-09 calls `SaveRunFlags` at run end and forces a flagged run outcome to `escalated`. |

## M2-08 proposal-tool decisions (Q-24..Q-27, Q-31, Q-32)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-24 | **RESOLVED (M2-08):** `state_hash` and `cases.version`. The canonical state projection excludes `cases.version`, narrative text, and wall-clock fields; it covers case status/resolution type, the case's break state, and every cited evidence ref's canonical projection. `Canonical`/`Hash` are exported for M4-06 and fixed by the golden fixture `internal/agent/proposal/testdata/state_golden.json` + `.hash`; M4-06 rechecks break-open/state and proposal expiry rather than relying on the case version. | M2-08 / M4-06 | Recorded as the canonical-state contract; no `docs/spec.md` change. |
| Q-25 | **RESOLVED (M2-08):** account allowlist matching is exact `config.LedgerConfig.AllowsAdjustment` equality, with no prefix or class matching. Integration tests create accounts whose codes are explicitly allowlisted; M2-09 must configure the concrete generator-suffixed account codes in the demo configuration. The package defaults are unchanged. | M2-08 / M2-09 | Keep exact matching; configure concrete codes in M2-09. |
| Q-26 | **RESOLVED (M2-08, post-adversary fix):** evidence TOCTOU window accepted. Existence is authoritative only from this run's successful, non-truncated `tool_calls.result.refs` rows read through the app pool; there is no in-memory `RetrievedRefs` precheck, because a replay or restart starts with an empty collector. A tool call whose result is marked `truncated:true` cannot authorize any citation, including refs for the rows it did return. Record values come from the current `v_agent_*` views. The executor (M4-06) revalidates, and `state_hash` records the validated state at proposal time. | M2-08 / M4-06 | No extra locking in this piece; executor revalidation is the control. |
| Q-27 | **RESOLVED (M2-08):** write-off shape is balanced lines (per currency) with at least one line on a configured `WriteOffAccounts` code (default `["suspense"]`) that is also in the adjustment allowlist, not a single amount. The rest of the entry follows the adjustment balance, delta, active-account, currency, and allowlist rules. | M2-08 | Recorded as the write-off contract; no spec edit. |
| Q-31 | **RESOLVED (M2-08):** `mark_duplicate.duplicate_of` is optional. When present it must be a canonical `ext:` ref, exist, and differ from `external_ref`; it need not belong to the case scope. The cited `external_ref` itself must be in this run's evidence and in the case's converted break scope. | M2-08 | Recorded as the duplicate contract; no spec edit. |
| Q-32 | **RESOLVED (M2-08, post-adversary fix):** the spec's "a run produces at most one proposal" (section 6.1) is a durable database invariant. Migration `20261106130000_m2_08_one_proposal_per_run.sql` replaces the non-unique `proposals_agent_run_id_idx` with the unique index `proposals_one_proposal_per_run` on `proposals(agent_run_id)`. A 23505 on that index maps to `ErrProposalExists` / `proposal_already_exists`; a fresh `RunContext` with an empty in-memory slot is therefore rejected by the database, not only by application logic. Exact idempotency-key replay remains `already_recorded` through `ON CONFLICT (idempotency_key) DO NOTHING` and is checked before case-open/business validation so a replay wins after the case has moved on. The in-memory `RunState` slot is only a same-process short-circuit. | M2-08 / M4-06 | Recorded as the one-proposal-per-run DB contract; no `docs/spec.md` edit. |

## M2-02 LLM provider and configuration decisions (Q-33..Q-35)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-33 | **RESOLVED (M2-02):** spec section 17 sketches the LLM client interface under `internal/agent`, while roadmap M2-02 and this implementation place the provider-neutral types, adapter, pricing, scripted client, and cassettes in `internal/llm`. Section 17 is a layout sketch, and the package boundary keeps `internal/llm` from importing `internal/agent` so M2-09 can depend one way. | Spec section 17 / M2-02 | Recorded as a layout delta; do not edit `docs/spec.md`. |
| Q-34 | **RESOLVED (M2-02):** spec section 2.2 says "provider SDK behind an `LLMClient` interface". The implementation uses a narrow `net/http` OpenAI-compatible adapter behind `llm.Client` instead of a vendor SDK, because the available provider is OpenCode Console Inference and a small adapter gives exact control over headers, `reasoning_content`, and `opencode` usage/cost fields. "SDK" is descriptive; the interface requirement is met and `go.mod` gains no LLM dependency. | Spec section 2.2 / M2-02 | Recorded as wording clarification; no `docs/spec.md` edit. |
| Q-35 | **RESOLVED (M2-02):** cost semantics. `usage.cost` is 0 on Go; `opencode.list_cost` is recorded as `cost_usd` when present and `opencode.allowance` is recorded separately with no hard-coded Go-plan multiplier (observed 60% for DeepSeek and 100% for GLM). The pricing table computes a fallback cost from the token breakdown and records `CostMismatch` rather than failing. Product confirmation of whether `list_cost` already includes peak multipliers remains non-blocking. | M2-02 / M3-08 | Recorded as the M2 cost contract; revisit when the eval gate compares model costs. |

## M2-09 loop decisions (OQ-1..OQ-3, OQ-6)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| OQ-1 | **RESOLVED (M2-09):** the 3-minute lease TTL is shorter than the 5-minute run wall clock. M2 accepts reclaim-as-attempt instead of lease renewal: a run that outlives its lease is bounded by the wall clock and the worker's reclaim path owns the attempt. Revisit if slow runs appear. | M2-09 / M2-10 | Recorded as the M2 lease contract; no spec edit. |
| OQ-2 | **RESOLVED (M2-09):** `agent_runs.outcome` keeps the five-value enum. A disabled run is finalized `escalated` and the case is not transitioned, so no `aborted`/`running` value is added. | M2-09 | Recorded as the disabled-run semantic; future migration only if M5 needs it. |
| OQ-3 | **RESOLVED (M2-09):** budget exhaustion is the run outcome `budget_exhausted`, the loop does not transition the case, and the worker records one attempt; the attempt cap escalates `agent_failed`. | M2-09 / M2-10 | Recorded as the budget contract; no spec edit. |
| OQ-6 | **RESOLVED (M2-09):** replay injects `NewRunID(caseID)` so the request transcript and wrapper token are deterministic. Replaying onto a database that already holds the run id fails loudly on the primary key; the CLI error tells the operator to reset or clone the fixture. | M2-09 / M2-11 | Recorded as the replay determinism contract; no silent run-id rewrite. |

## M2-10 worker decisions (OQ-1, OQ-3, OQ-4)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| OQ-1 | **RESOLVED (M2-10):** the worker passes the 3-minute lease TTL to `cases.Lease` and does not renew mid-run. A run that outlives its lease is reclaimed inline by the next `Lease` (or `ReclaimExpired`), which increments attempts; the losing worker's late release/attempt returns `ErrNotLeased`/`ErrNotLeaseOwner`/`ErrStale` and is logged as a no-op, never retried with a fresh version. This is the M2 risk accepted in M2-09. | M2-10 | Recorded as the worker lease contract; no spec edit. |
| OQ-3 | **RESOLVED (M2-10):** `budget_exhausted` and `invalid_output` call `cases.RecordFailedAttempt` (one attempt, lease cleared); at `max_attempts` the same atomic call escalates `agent_failed`. `provider_error` and a disabled switch release without an attempt, and an expired-lease reclaim owns its own increment. A poison case therefore gets exactly `max_attempts` model runs. | M2-10 | Recorded as the attempt contract; no spec edit. |
| OQ-4 | **RESOLVED (M2-10):** the daily USD budget is per process: `SUM(agent_runs.cost_usd)` for the UTC day is queried at most once per poll interval and in-flight run costs are added locally between queries. At/above the cap the worker stops leasing; in-flight runs finish and cases stay `open`. With one worker the overshoot is bounded by one poll interval of completed runs, and with several workers by `workers × concurrency × agent.max_cost_usd`; a spend ledger is deferred until a second worker is deployed. | M2-10 | Recorded as the approximate daily-budget contract; no spec edit. |

## M2-11 CLI and gate decisions (OQ-5, OQ-6)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| OQ-5 | **RESOLVED (M2-11):** `make demo-m2` is the M2 gate test (`TestMilestone2Gate`), not a `recon demo m2` subcommand. It replays the committed cassettes against a fresh `testdb` clone with ephemeral role logins, so it runs in the CI integration job after `make demo-m1` with no `OPENCODE_API_KEY` and no network. | M2-11 | Recorded as the demo contract; the top-level `demo` stub stays reserved for M5. |
| OQ-6 | **RESOLVED (M2-11):** scripted replay deterministically derives the run id from the case id, so the request transcript and wrapper token are stable and cassettes hit. A second replay on a database that already holds that run id fails loudly on `agent_runs_pkey`; the CLI error (and the fixture README) tells the operator to use a fresh clone or reset the fixture. The gate and CLI tests always use fresh `testdb` clones; live `--llm opencode` runs use random run ids. | M2-09 / M2-11 | Recorded as the replay determinism contract; no silent run-id rewrite. |

OQ-1..OQ-4 keep their M2-09/M2-10 resolutions above; M2-11 adds no new
blocking open question.

## M2-08 prerequisites: composite link and cassette re-record (Q-36)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-36 | **RESOLVED (M2-08 prereq):** `propose_link` accepts an exact-set `ledger_txn_ids` form in addition to `ledger_txn_id`, with a separately keyed canonical payload (sorted, lowercased, de-duplicated) and every leg required to be in the same source+currency scope as the external record; the singular payload/key is byte-identical to the pre-change shape. The schema change invalidated the M2-11 gate cassette keys, so the 11 committed interactions were deliberately re-recorded. The live provider produced **12** interactions: `fee_variance` 3 → 4 (one extra `get_settlement_batch` verification step before proposing), while `reference_typo` stayed 5 and `duplicate_charge` stayed 3. The 12-cassette set passes `TestCommittedCassettes` and `make demo-m2` offline (adjustment/link/duplicate plus outage), and hygiene checks found no key, session, authorization material, or real PII. `wantM2CassetteFiles` was explicitly updated 11 → 12; no recorded response was hand-edited and no test was weakened. | M2-08 prereq / M2-11 gate | Recorded as the accepted 12-interaction deviation; update the pin and this row together on any future re-record. |

## M3-01 eval schema, dataset, and ground-truth decisions (Q-37..Q-41)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-37 | **RESOLVED (M3-01):** the eval schema is a documented delta from the spec §3.1 eval sketch. `eval_runs` adds `repeat_group UUID NULL` and `repeat_index INT NULL` with `CHECK (repeat_group IS NOT NULL OR repeat_index IS NULL)` for M3-04 repeats; `started_at` has no database default because the runner supplies it. `eval_case_results` adds `UNIQUE (eval_run_id, scenario_id)` as the M3-04 resume/idempotency key and has **no** scenario foreign key because scenario truth lives in the M3-02 `eval_truth` schema, not in `public`. `failure_mode` is generator-owned text, not a database enum. `recon_app` receives `SELECT, INSERT, UPDATE` on `eval_runs` and `SELECT, INSERT` on `eval_case_results`; `recon_agent_ro`, `recon_audit_ro`, and `recon_executor` receive nothing, and `PUBLIC` is revoked first. Migration: `20261107120000_m3_01_eval_schema.sql`. | M3-01 / spec §3.1 | Recorded as the M3-01 schema/grant delta; no `docs/spec.md` edit. |
| Q-38 | **RESOLVED (M3-01):** `eval_truth` database-schema ownership stays with M3-02. M3-01 creates only the public `eval_runs`/`eval_case_results` tables and the versioned JSON ground-truth schema; it creates no `eval_truth` schema, grants no agent access to eval tables, and does not load a world into Postgres. M3-02 owns the isolated schema, loader, and the proof that `recon_agent_ro` cannot read it. | M3-01 / M3-02 | Recorded as the ownership boundary; no spec edit. |
| Q-39 | **RESOLVED (M3-01):** eval ground truth is a wrapper around raw `synth.Scenario`, versioned as schema v1 (`eval/dataset/schema/ground_truth.schema.json`). It adds explicit `expected_link` (sorted ledger set + external-record set), `expected_adjustment` (balanced line multiset with concrete suffixed accounts), `expected_duplicate` (`dup_` record plus original), and per-scenario `injection` identity. `propose_link` truth is normalized to `link`. For every `injection_memo` scenario the allowed set is exactly `["escalate"]` with primary `escalate` regardless of the generator's raw `["link"]`; the underlying `expected_link` is preserved for evidence relevance, and `expected_tier` is carried but not scored in M3-03. Missing required expected payloads are builder errors. Only `eval/datasets/<split>/manifest.json` and the schema are committed; worlds stay in temporary directories. | M3-01 / M3-03 | Recorded as the M3-01 ground-truth contract; no spec edit. |
| Q-40 | **RESOLVED (M3-01):** `synth`'s generator `dataset_version` still excludes injection-template content and source calendars, even though both can change generated bytes (`late_settlement` uses the calendar; `injection_memo` uses template text). M3-01 does not change `synth`; instead `eval_dataset_version` in `eval/datasets/<split>/manifest.json` is the authoritative eval version over every file hash plus sorted template hashes, source-calendar canonical string, config canonicals, counts, and `truth_schema_version`. `generator_dataset_version` is retained as provenance. `ground_truth.json` carries only `generator_dataset_version`, so the eval version is not circular. Backporting template/calendar hashing into `synth` remains a possible later change; until then M3-02/M3-04/M3-09 must use the eval manifest version. | M3-01 / synth | Recorded as the eval-version gap; no `synth` change. |
| Q-41 | **RESOLVED (M3-01):** `fx_rounding` truth uses `fees_expense_<ccy>` as the designated credit account for the conversion residual because the generator has no rounding/FX-gain-loss account; the debit side is `clearing_psp_a_<ccy>`. All eval adjustment lines use concrete generator-suffixed accounts, never the unsuffixed `internal/config` defaults: `clearing_psp_a_<ccy>` for fee variance and partial refunds, `fees_expense_<ccy>` for fee variance/fx rounding, and `refunds_expense_<ccy>` for partial refunds. `DefaultConfig` validates that all three codes exist in the eval adjustment allowlist for every dataset currency. | M3-01 / M3-03 | Recorded as the eval account convention; no generator change. |

## M3-03 scorer decisions (Q-42..Q-44)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-42 | **RESOLVED (M3-03):** the spec §6.1 `Actual` sketch is extended with optional M3-04-facing carriers: `Confidence` (`proposals.confidence`), `CostDetail` (`{allowance, tokens_in, tokens_out, cost_source, cost_mismatch}`), `LatencyDetail` (`{llm_call_ms, tool_call_ms}`) and `LoopSteps`. They are stored in `eval_case_results.detail` and never replace the headline metric sources: cost still comes from `agent_runs.cost_usd`, latency from the `agent_runs` wall clock, and steps from `COUNT(tool_calls)`. `ValidateTruth(dataset.GroundTruth)` is the schema-version gate M3-04 calls once per split; `ScoreCase` additionally validates each scenario and treats invalid truth as a hard error. No `docs/spec.md` edit. | M3-03 / M3-04 | Recorded as the scorer input contract and metric-source boundary; no spec edit. |
| Q-43 | **RESOLVED (M3-03):** spec §6.2.5 defines behavior for an empty expected `duplicate_of` (the actual must omit it), but the M3-01 schema v1 requires a non-empty `expected_duplicate.duplicate_of`. `eval/score` keeps the spec behavior by relaxing only that field during score-time truth validation (a temporary sentinel is used for schema validation); every other v1 rule still applies. The dataset builder always populates `duplicate_of`, so generated truth is unaffected. A future schema v2 may make the field optional; until then this is the documented scoring delta. | M3-03 / M3-01 schema | Recorded as a schema/scorer delta; no builder or `docs/spec.md` change. |
| Q-44 | **RESOLVED (M3-03):** truth v1 has no expected write-off payload, so `write_off` equivalence is allowed-set membership only: a write-off is correct exactly when `expected_resolutions` explicitly allows `write_off`, and it is never equivalent to an adjustment. No current generator mode allows `write_off`; the distinction is covered by fixtures. If a later mode gains write-off truth, schema v2 should add expected write-off lines. | M3-03 / M3-01 schema | Recorded as an equivalence limitation; no generator or schema change now. |

## M3 piece A post-review follow-ups (Q-45..Q-49)

Non-blocking hardening and process follow-ups from the M3 piece A review. None
changes the delivered M3-01/M3-03 behavior; the accepted 12-cassette deviation
remains Q-36.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-45 | **OPEN (M3 piece A review):** `eval/dataset.LoadManifest` accepts a manifest whose `eval_dataset_version` was recomputed after tampering: it does not require non-empty `injection_template_hashes` or `source_calendars_canonical`, does not require the count maps or cross-check them, and ignores trailing JSON. The committed manifests are byte-identical to fresh builds, so this is a validator-hardening/future gate-gaming risk, not a current data defect. | M3-04 / M3-02 | Require the provenance fields and counts, enforce count consistency, reject trailing JSON, and optionally check `files` against the rebuilt file set. |
| Q-46 | **OPEN (M3 piece A review):** `eval/dataset.ValidateGroundTruth` accepts a truth scenario that allows `link` with no `expected_link` (and allows `duplicate` outside `duplicate_charge` with no `expected_duplicate`), while `eval/score.ScoreCase` rejects the same truth. Generated truth supplies both; hand-built/loaded truth can validate at load time and fail at score time. | M3-02 / M3-04 | Align `validateGroundTruthSemantics` with `validateScoreTruth`: require `expected_link` whenever `link` is allowed and `expected_duplicate` whenever `duplicate` is allowed. |
| Q-47 | **OPEN (M3 piece A review):** `eval/dataset` expected-adjustment balance validation sums `AmountMinor` in `int64` without overflow checks; crafted lines near `math.MaxInt64` can wrap and appear balanced. Generated data uses small amounts. | M3-02 | Use overflow-checked addition (the proposal path's `addMinor` pattern). |
| Q-48 | **OPEN (M3 piece A review):** new `// covers: FIN-12` annotations on the M2-08 link-key tests are outside the spec §7 annotation set for this piece (FIN-05, FIN-10, FIN-13). The annotations are plausible for idempotency-key behavior but should be reconciled. | M2-08 follow-up / docs | Reconcile the annotation IDs with the spec-listed set in a later docs/process pass; no test weakening. |
| Q-49 | **OPEN (M3 piece A review):** `eval/score.Aggregate` computes p50/p95 over every supplied `CaseResult.Latency`; it cannot distinguish an absent wall clock from a real 0 ms. M3-04 must supply only completed runs (`agent_runs.ended_at - agent_runs.started_at`) and document the denominator as supplied observations. | M3-04 | Filter to completed runs in the runner and keep the §9 source boundary. |

## M3-02 scenario sandbox decisions (Q-50..Q-53)

Non-blocking records from the M3-02 scenario sandbox. None changes
`docs/spec.md`; they refine the spec §3.1 sketch and document eval-only
behavior.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-50 | **RESOLVED (M3-02):** the `eval_truth` schema is a documented spec §3.1 delta (already anticipated at ROADMAP line 110). Migration `20261108120000_m3_02_eval_truth.sql` creates `eval_truth.sandbox_meta` (one row: `eval_dataset_version`, `split`, `seed`, `world_clock`, `loaded_at`), `eval_truth.scenarios` (`scenario_id` PK, `split`, `mode`, `difficulty`, `break_signature`, `truth_schema_version`, `truth` JSONB, `loaded_at`), and `eval_truth.scenario_cases` (`scenario_id` PK/FK cascade, `case_id`, nullable `break_id`, `synthetic`, `scenario_time`, `created_at`) plus a `case_id` index. The versioned `eval/dataset` JSON document remains the source of truth; the typed columns are the stable query surface. `PUBLIC` is revoked and no grant is made to `recon_app`, `recon_agent_ro`, `recon_audit_ro`, or `recon_executor`; only the admin/loader and trusted runner read it. | M3-02 / spec §3.1 | Recorded as the M3-02 schema/ownership delta; no `docs/spec.md` edit. |
| Q-51 | **RESOLVED (M3-02):** an `injection_memo` scenario intentionally has no production break, so the sandbox creates one eval-only case from an external-only break derived from the truth's external records (delta 0), marks `eval_truth.scenario_cases.synthetic = true`, and leaves `break_id` NULL. The agent can read the case and the expected outcome stays `escalate`. This path lives only in `eval/sandbox` and must never be copied into production code. Clean scenarios and other empty-signature non-injection scenarios remain `not_scoreable` and are excluded from scoring; zero/multiple truth candidates or duplicate signatures fail closed rather than guessing. | M3-02 / M3-04 | Recorded as the synthetic-case and not-scoreable policy; no production change. |
| Q-52 | **RESOLVED (M3-02):** `eval/dataset` truth signatures are computed over deterministic logical scope ids from the `synth/modes` UUIDv5 namespace, but `synth.Load` lets Postgres assign random `reconciliation_scopes` ids and the production matcher's candidate shapes differ from the generator's canonical shape. `Clone` therefore pins each clone's scope ids back to the dataset's deterministic ids (repointing `statement_balances` in one transaction) and reconstructs the unique truth candidate from the scenario's own ledger/external references with amount-derived side/delta combinations. The reconstructed candidate must hash to `truth.BreakSignature` exactly; zero or multiple reconstructions fail closed. The production matcher still runs per scope so matched rows and `match_runs` exist. Per-scenario `scenario_time` is the maximum referenced ledger/external event instant, falling back to the case's `created_at` or the sandbox base clock for synthetic cases, normalized to UTC. | M3-02 / eval/dataset / synth | Recorded as the scope-id pinning and truth-signature reconstruction contract; no `synth` change. |
| Q-53 | **OPEN (M3-02 follow-up):** case/break/run/proposal UUIDs are database-generated and scenario timestamps vary by clone, so cassettes recorded against one clone cannot replay byte-identically against another. M3-04 acceptance uses an injected per-case `llm.Scripted` seam and cassette replay stays on the fixed M2 fixture. A future pinned-world/cassette mode would need deterministic case/break/run ids (for example UUIDv5 derived from `scenario_id`) plus pinned audit/case timestamps and a pinned world clock. | M3-04 / future pinned-world mode | Add `DeterministicIDs`-style pinning and timestamp/clock pinning only when a pinned-world cassette mode is scoped; record it then. |

## M3-04 eval runner decisions (Q-54)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-54 | **RESOLVED (M3-04):** `eval/sandbox` creates a synthetic case for every `injection_memo` scenario in **every** split (Q-51), so the expected scoreable count for `dev`/`test` includes those cases: `dev` is 132 signature rows + 12 synthetic injection cases = 144, and `test` is 330 + 30 = 360 (the `injection` split is 30 synthetic cases). Resume/completeness therefore mirrors `ScoreableScenarios` — non-empty `break_signature` **or** `mode == injection_memo`, any split — rather than counting only non-empty signatures. `make test-integration` adds `-p 1` because `eval/sandbox.CleanupOrphans` is deliberately cluster-global over `recon_eval_%`; with package binaries in parallel, the sandbox cleanup test can drop the live clone of a concurrently running `eval/runner` binary (SQLSTATE 57P01). `-parallel=4` still parallelizes tests within a package, and the cleanup semantics and test assertions are unchanged. | M3-04 / M3-02 | Recorded as the scoreable-count contract and the integration-harness serialization; no `docs/spec.md` edit. |

## M3-04 post-adversary resume/cap hardening (Q-55)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-55 | **OPEN (M3-04 post-adversary, operational):** `internal/demo.Reset` refuses to drop/recreate schemas only when `RECON_ENV=production` (case-insensitive). `.env.example` now sets `RECON_ENV=development` for local runs, but a deployment that copies the example or leaves the variable unset is not protected from `recon demo --reset`. The guard is env-based by design; there is no additional host/database check. | M3-04 / deploy config | Ensure production and shared environments set `RECON_ENV=production`; keep the example at `development` for local use. No behavior change in this fix. |

## M3 piece C transcript prerequisite (Q-56)

Accepted post-merge M3-04 delta. No roadmap tick, migration, or grant change.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-56 | **RESOLVED (M3-04 post-merge prereq):** every newly scored case stores a sanitized transcript snapshot v1 in the existing `eval_case_results.detail` JSONB under `"transcript"`. The snapshot carries the terminal `agent_runs.outcome`, a `{id,type}` proposal summary (null when absent), the latest already-sanitized assistant excerpt redacted and capped at 200 runes, and ordered tool steps limited to `seq`, tool name, canonical sorted-unique refs, error code, and the envelope truncation flag. It never carries tool arguments, raw results, memo text, DSNs, API keys, or session IDs. `internal/llm.Redact` covers the shared secret classes; `eval/runner` adds `postgres://`/`postgresql://` DSN and password-key redaction. The snapshot is bounded to 20 steps, 20 refs per step, 64-rune error codes, and 16 KiB of marshaled JSON; over-bound snapshots drop refs first, then shorten the assistant, and set snapshot-level `truncated=true`. `internal/agentrun.Step` gains additive `refs`/`truncated` fields read only from `tool_calls.result->'refs'` and `result->>'truncated'`. Missing snapshots mean the run predates v1 and reports degrade to “transcript unavailable”; unknown versions are unsupported, never guessed. The snapshot is not full PII/PAN scrubbing (M6-06 owns retention/redaction). | M3-04 / M3-05 | Recorded as the versioned bounded snapshot contract; no schema change, no task tick, and `docs/spec.md` is untouched. |

## M3-05 report decisions (Q-57, Q-58, Q-64, Q-66, Q-67)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-57 | **RESOLVED (M3-05):** the failure appendix renders a locator (run/scenario/agent run), the sanitized transcript snapshot v1 (proposal summary, ≤200-rune redacted assistant excerpt, ordered safe tool steps) and the failure row. Raw tool arguments, results, memos, DSNs, API keys and session IDs are never read or rendered. Missing snapshots render “transcript unavailable (run predates snapshot v1)”; unsupported versions render a warning plus an unsupported marker and are never parsed with guessed semantics. | M3-05 | Recorded as the minimum sufficient transcript surface; no raw transcript embedding. |
| Q-58 | **RESOLVED (M3-05):** requesting any member of a repeat group reports the whole group by default. Build requires a complete contiguous `0..k-1` sibling set sharing suite, split, dataset_version, model, prompt_version and seed; a missing or inconsistent sibling fails closed. A different `git_sha` is allowed with a warning. Per-run metrics are computed first and summarised as mean/min/max/sample stddev; cases are never pooled into one binomial sample and no Wilson interval is attached to the mean. | M3-05 / M3-07 | Recorded as the repeat-group report contract; M3-07 compares named runs only. |
| Q-64 | **RESOLVED (M3-05):** `generated_at` comes from the injected `clock.Clock`; goldens pin it. It is the only volatile report field. The report never reads the wall clock directly. | M3-05 | Recorded as the determinism contract; no wall-clock use in `eval/report`. |
| Q-66 | **RESOLVED (M3-05):** an existing run with zero case rows is a valid report: exit 0, all rates undefined (`value`/`wilson_*` null), cost/latency/steps undefined, and explicit zero-case/partial warnings. Only load/validation/database/render/write failures are exit 1. | M3-05 | Recorded as the zero-case behavior; no silent 0/1 rates. |
| Q-67 | **RESOLVED (M3-05):** `--json <path>` and `--out-dir <dir>` write only to explicit paths; parents must already exist, files are written with `0644`, and nothing is written into the repository implicitly. `--json` and `--out-dir` are mutually exclusive (exit 2); a missing parent is exit 1. | M3-05 / M3-07 | Recorded as the explicit-output contract; no implicit directory creation or repo writes. |

## M3-06 component eval and calibration decisions (Q-61..Q-63, Q-65)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-61 | **RESOLVED (M3-06):** calibration is per run by default. A repeat group pools its proposal confidence observations into one reliability table and lists each run's ECE in `per_run_ece`; the group does not claim a pooled binomial interval. The target is resolution `correct`, never false-fix. Rows with null confidence (escalation, budget exhaustion, provider error) are excluded and `N` states the proposals-only denominator. Ten equal-width bins with 1.0 in the last bin; empty bins render null metrics; `N=0` is `applicable=false` with a null ECE. | M3-06 | Recorded as the calibration scope/target contract; calibration is descriptive and never gates. |
| Q-62 | **RESOLVED (M3-06):** `recon-eval matcher` supports `dev`, `test`, and `injection`; definition-of-done acceptance is `dev` and `test`. The injection split may render recall `n/a` because all truth rows expect escalate and matchscore excludes them. Recall below 1.0 is a finding, not a command failure; known ADR 0004 residuals are recorded, never special-cased. | M3-06 | Recorded as the matcher split and exit contract. |
| Q-63 | **RESOLVED (M3-06):** matcher reads use `sandbox.App` (`recon_app`) for `matches`, `match_legs`, and `external_records`; no Admin data read and no grant/migration is added. A read failure is returned rather than silently widening privileges. The matcher is standalone: it creates a disposable clone and writes no `eval_runs`/`eval_case_results` rows. | M3-06 | Recorded as the matcher read/persistence boundary; no eval schema change. |
| Q-65 | **RESOLVED (M3-06):** every agent report carries a generic verifier scorecard placeholder `{name:"verifier", applicable:false, deferred_to:"M4-05", reason:"deterministic verifiers land in M4-05", cases:0, metrics:[]}`. Markdown renders “not applicable until M4-05”. M3-06 does not create or import `internal/policy/verify`; M4-05 fills the same report-owned shape. | M3-06 / M4-05 | Recorded as the scorecard hook contract; no policy package in this piece. |

## M3-07 run comparison decisions (Q-59, Q-60)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-59 | **RESOLVED (M3-07):** comparison is run-to-run: when either named run has a `repeat_group`, only that run's own case results are compared and a warning names the group and repeat index. Repeat-group comparison is not redefined in M3-07; the M3-05 report already renders per-run means, min/max, and sample spread. | M3-07 / M3-09 | Recorded as the named-run compare boundary; no pooling of repeat cases. |
| Q-60 | **RESOLVED (M3-07):** gating exit 1 uses disjoint 95% Wilson intervals plus adverse direction for false-fix (increase), accuracy (decrease), evidence-validity (decrease), and correct-escalation (decrease), overall **and** per mode. Injection resistance and over-escalation are classified but non-gating in M3-07; cost, latency and steps are descriptive only. `--report-only` suppresses the gating exit 1 and the incomplete-data exit 2 (the footer marks the suppression) but never usage, metadata, load or write failures. | M3-07 / M3-09 | Recorded as the fixed M3-07 exit contract; M3-09 may add thresholds. |

## M3-09 gate decisions (Q-68, Q-69)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-68 | **RESOLVED (M3-09, human decision):** the gate's T0 false-fix threshold is `applicable: false` and `deferred_to: M4-14`; deterministic verifiers and policy tiers do not exist in M3, so T0 is not measurable and is rendered `not_applicable`, never `pass`. Injection resistance is gated only on the dedicated `injection` split; on `dev`/`test` it is `not_applicable` with the reason “dedicated injection split required (M6-02/M6-05)”. Deterministic cassette replay and the CI eval gate are deferred to M6-05 because Q-53 database-generated IDs/timestamps prevent byte-identical replay; the M3-09 baseline is record-only evidence and M3-09 adds no CI/Makefile gate target. | M3-09 / M4-14 / M6-05 | Recorded as the n/a and deferral contract; no T0 or injection-on-test pass is ever implied. |
| Q-69 | **OPEN (M3-09 statistical consistency):** the spec's example degraded false-fix case (baseline 0/100, candidate 5/100) is not a disjoint 95% Wilson regression under the committed `report.Wilson` implementation: the baseline upper bound is ≈0.0370 and the candidate lower bound is ≈0.0216, so the intervals overlap and `report.Compare` classifies it `no_change`. The gate correctly reuses that classification, so a 5/100 candidate would pass `overall_false_fix`; the package tests use 0/100 vs 10/100 (candidate lower bound ≈0.0553) to exercise a genuine regression. Decision needed: correct the example counts or accept the statistical behavior. | M3-09 / spec §6.2(G5), §6.3(G10) | Keep the gate aligned with the committed Wilson implementation; do not invent a second interval rule or special-case 5/100. |

## M3-09 baseline cassette sample (Q-70)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-70 | **RESOLVED (M3-09):** the frozen `test` run recorded 1,840 cassettes / 88,733,159 bytes. To stay inside the committed-cassette budget, `eval/baselines/m3-09-test/cassettes/` commits a deterministic **12-file / 623,701-byte** sample: one cassette per failure mode, chosen as the lexicographically smallest SHA-256 filename among that mode's recorded calls, with the mode resolved from the first user message's case id through the durable `eval_case_results` transcript `case:<uuid>` ref. All 12 test-split modes are represented, every file is ≤ 1 MiB, and total committed cassette roots are 2,608,523 bytes of the 10,485,760-byte budget. The full recording was local-only and is not committed. The sample is evidence, not a replay input; Q-53/Q-68 still defer deterministic replay and the CI gate to M6-05. | M3-09 / M6-05 | Recorded as the baseline sample policy in `metadata.json` and the baseline README; no replay, CI, or threshold behavior changes. |

## M4-01 approval, executor, and outbox schema (Q-71, Q-72)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-72 | **OPEN (M4-01 post-review):** the schema and grant contract are delivered; these follow-ups remain. (1) `recon_app` holds table-level `UPDATE` on `outbox`, so it can rewrite `payload`, `topic`, `aggregate_type`, `aggregate_id`, `idempotency_key`, and `created_at`, not only the four delivery columns; the migration comment describes delivery-column updates, Q-71 records the table-level grant as-is, and the negative test covers only delivery columns — either narrow the grant to `UPDATE (delivered_at, attempts, next_attempt_at, dead)` and add negative tests, or state that the whole row is app-mutable. (2) `policy_config_versions.hash` is only format-checked (64 lowercase hex); `sha256(yaml)` content verification is M4-04. (3) `outbox.idempotency_key` is globally unique and the executor inserts with `ON CONFLICT (idempotency_key) DO NOTHING`, so a key reused across topics silently drops the second event; M4-07 must namespace keys or treat a zero-row conflict explicitly. (4) No application role can write `approvers` (`recon_app` has SELECT only); M4-11 must add a grant/migration if runtime approver onboarding is needed. (5) The migration Down section is goose-only and is not exercised by `make verify` or any test; a test-only down/up probe (or an explicit release-engineering note) should prevent drift. Four-eyes, approver-role, and signature enforcement also remain deferred to M4-03/M4-11; the schema accepts a `viewer` approver and `signature_valid = false` on an approval. | M4-01 follow-up / M4-04 / M4-07 / M4-11 | Keep the delivered schema; resolve each follow-up in its target task and add the missing negative tests. |
| Q-71 | **RESOLVED (M4-01):** the migration creates `approvers`, `approvals`, `applied_proposals`, `outbox`, and `policy_config_versions`. `outbox` is **created** (not extended) with the spec §3.1 base columns plus `next_attempt_at`, `dead`, and `idempotency_key`; Q-08's "outbox columns belong to M2" is stale and corrected here, and `policy_config_versions` is the roadmap addition, not a spec §3.1 table. `approvals` targets `proposals(id)`, decisions are exactly `approved`/`rejected`, `(proposal_id, approver_id)` is unique, and every FK is `ON DELETE RESTRICT`. `applied_proposals` uses `proposal_id` as its PK, a nullable + unique `ledger_txn_id` (NULLs distinct, so many link/duplicate rows), and `executor_run_id TEXT` with no FK or UUID type. Outbox `aggregate_id` is `TEXT`, `idempotency_key` is globally unique and non-empty, the due-claim index is partial on `(next_attempt_at, id) WHERE delivered_at IS NULL AND dead = false`, and there is deliberately no `delivered_at >= created_at` cross-check, no payload-shape check, and no `last_error`/`dead_at` column. Policy versions are content-addressed by 64-lowercase-hex SHA-256, non-empty YAML, clock-supplied `loaded_at`, and no size cap. Grants (ADR 0005: "owns" means grants, not table ownership): `recon_app` gets `approvers` SELECT, `approvals` SELECT/INSERT, `outbox` SELECT/INSERT/UPDATE, `policy_config_versions` SELECT/INSERT, and `applied_proposals` SELECT; `recon_executor` gets `proposals` SELECT plus column-level `UPDATE(status, version)`, `applied_proposals` SELECT/INSERT, and `outbox` INSERT plus column-level `SELECT(idempotency_key)` for `ON CONFLICT` inference (Q-22); no app UPDATE on `proposals` (M4-03 owns it), and no new `recon_agent_ro`/`recon_audit_ro` grants. `TestM2ExecutorAuditNoGrants` is deliberately updated for the executor's proposal SELECT (table-level UPDATE remains false; the column grant is proven in `TestM401ExecutorGrants`); no other M2 grant changes. M4-01 adds no `apply_failed_reason` column; M4-06 records the reason in the audit entry. | M4-01 / M4-06 / M4-07 | Recorded as the M4-01 schema and grant contract; `docs/spec.md` is untouched. |

## M4-02 API server skeleton (Q-73..Q-83)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-73 | **RESOLVED (M4-02):** `internal/api` and the top-level `api/` directory are approved layout additions. Spec §17 lists `cmd/api` but not `internal/api` or `api/`; the roadmap requires `api/openapi.yaml`. The implementation keeps the router, middleware, handlers, read model, and switch writer in `internal/api`, and the hand-written contract in `api/openapi.yaml`. `cmd/api` stays a thin composition root. | M4-02 / spec §17 | Recorded as the M4-02 layout delta; no `docs/spec.md` edit. |
| Q-74 | **RESOLVED (M4-02):** `GET /v1/cases?type=` is deferred. A case can contain multiple breaks with different sides and the `cases` table has no type column, so the API rejects any `type` query value with 400 `unsupported_filter` instead of guessing. Listing supports `status`, `limit`, and `cursor` only. | M4-02 / spec §8.8 | Revisit when cases gain a durable type or a join-backed filter; do not silently ignore `type`. |
| Q-75 | **RESOLVED (M4-02):** `API_KEYS` is one env-only JSON array of `{id, role, key}` objects; roles are `viewer`, `approver`, and `admin`. Plaintext keys are 32–512 characters, must match exactly (no surrounding whitespace), and are hashed with SHA-256; only `{ID, Role, Digest}` is retained. Duplicate IDs and duplicate key material are rejected, unknown JSON fields are rejected, and a YAML `api.keys` entry is a strict-decode error. Malformed key material aborts startup, and parse errors name only the array index or field. Multiple entries per role support rolling rotation; there is no expiry or rotation endpoint. | M4-02 / spec §5 | Recorded as the API-key contract; a future identity task may add rotation or OIDC. |
| Q-76 | **RESOLVED (M4-02):** `POST /v1/reconcile/run` is deliberately non-atomic. The matcher commits its run first, then `cases.CreateBreaks` persists breaks/cases in a separate transaction. A failure after the matcher run returns a generic 500 and logs the `run_id`, never a partial-success response; retry safety relies on incremental runs and the unique `breaks.signature` dedupe (`ON CONFLICT DO NOTHING`). | M4-02 / spec §8.5, §13 | Documented behavior; a future task may add a durable reconciliation job if atomic orchestration is required. |
| Q-77 | **RESOLVED (M4-02):** `/metrics` is a public, unauthenticated static stub exposing only `recon_build_info{component="api"} 1` in Prometheus text format; it contains no business data. M5-03 must decide auth and listener placement when real metrics land. | M4-02 / M5-03 | Recorded as the metrics stub boundary; M5-03 owns the real exporter. |
| Q-78 | **RESOLVED (M4-02):** HTTP ingest audits as `system:ingest`, not `human:<key-id>`, preserving the existing ingest service contract. The calling key identity is available in the HTTP access log (`key_id`, `role`), not in the ingest audit row. Human attribution would require an ingest service change. | M4-02 / M4-13 | Recorded as the ingest actor contract; M4-13 owns actor typing and traceability. |
| Q-79 | **RESOLVED (M4-02):** the webhook replay cache is per-process, in-memory, with a 10-minute TTL (2× the 5-minute timestamp tolerance) and capacity 4096; entries are pruned lazily and a full cache fails closed with 409 replay. Multi-replica deployments can replay across replicas; v1 is single-instance and a shared cache is later hardening. | M4-02 / M4-12 | Recorded as the v1 replay contract; M4-12 may supply a shared store. |
| Q-80 | **RESOLVED (M4-02):** `api/openapi.yaml` is hand-written OpenAPI 3.1.0 with no code generation. The contract test parses it and compares the route set with the runtime route registry in both directions, and asserts public `security: []` plus `bearerAuth` and `x-required-role` on protected operations. | M4-02 / spec §10 | Recorded as the contract-maintenance rule; route or contract changes must update both sides and the test. |
| Q-81 | **RESOLVED (M4-02):** a switch update with the already-current value returns `changed:false` and writes no audit row; it reports the current `updated_by`/`updated_at`. A real change updates `system_switches` and appends exactly one `switch.set` audit row in the same `recon_app` transaction, with actor `human:<key-id>`, the supplied reason, and the request trace ID. | M4-02 / FIN-30 | Recorded as the switch no-op and audit contract; keep the no-audit-on-no-op assertion. |
| Q-82 | **RESOLVED (M4-02):** the case file is a v1 subset with `schema_version: 1` and only `case`, `breaks`, `proposals`, and `agent_runs`. Proposal rationale is sanitized (control characters removed) and truncated to 4000 runes; evidence is returned as reference strings only. Raw evidence objects, raw external payloads, policy/approval/executor/ledger sections are not exposed; M4-13 adds those sections and clients must tolerate additive fields. | M4-02 / M4-13 | Recorded as the v1 case-file contract; M4-13 owns the full document. |
| Q-83 | **QUEUED (M4-02 post-review):** four non-blocking residuals from the adversary review. (1) Redaction is formatting-only: `APIKey`/`APIConfig`/`Config` and webhook `Options`/`Verifier` redact through `String`/`Format`/`GoString`, but `encoding/json.Marshal` and `slog.Any` with a JSON handler still emit database passwords, API-key digest bytes, and the exported `Options.Secret`. No production path in M4-02 JSON-marshals or `slog.Any`-logs these values; the boundary is defense in depth. (2) Exact replay at the TTL boundary: `MemoryReplayCache.CheckAndStore` treats an entry as expired when `expires == now` (`expires.After(now)` is false), while a timestamp exactly at the 5-minute tolerance still passes the strict `> tolerance` check, so an identical request sent at exactly `base + 2*tolerance` is accepted once more; this needs an exact-nanosecond or coarse-clock tick. (3) YAML type errors echo a short value prefix: yaml.v3 reports e.g. ``cannot unmarshal !!str `FINAL-A...` into time.Duration``, leaking the first seven characters when a secret is misplaced in a numeric or duration field; the normal DSN and `API_KEYS` paths do not take this path. (4) A single `make demo-m1` reset run failed transiently with `relation "settlement_batches" does not exist` immediately after a prior run; it was not reproducible, and no M4-02 source touches the demo reset path. | Later hardening / M4-11 | Add `MarshalJSON` and `slog.LogValuer` redaction (or unexport secret fields); reject replay at `expires <= now` or extend the marker TTL by an epsilon; add a sanitizing config-error wrapper if untrusted config authors become a threat model; and recheck the demo reset in the next demo/reset task. |

## M4-03 proposal state machine (Q-84, Q-85)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-84 | **RESOLVED (M4-03):** the proposal state machine is delivered as `internal/approval/state` with exactly the nine spec statuses, a 17-edge table, actor-type-only enforcement (agent never; human only on `pending_approval -> approved/rejected`; system elsewhere), and typed errors. Contract frozen for consumers: `Service.Transition(ctx, tx, TransitionRequest{ProposalID, To, ExpectedVersion, Actor, Reason, TraceID})` uses the caller's `pgx.Tx`, checks `ExpectedVersion` before any write (`ErrStale`), and appends exactly one `proposal.transition` audit entry with `before {status,version}`, `after {status,version[,reason]}`, the caller's typed actor, a clock timestamp, and the optional trace ID; only `status` and `version` are written (`state_hash` stays M4-06's re-validation hash). `reason` is audit-only and bounded to 2000 runes with no control characters; `apply_failed` requires exactly `state_changed`, `break_resolved`, or `expired`. `ExpireDue` uses the injected clock once per call and expires only due `proposed`/`pending_approval` rows (`expires_at <= now`, inclusive) in one short `db.WithTx` per row with `FOR UPDATE SKIP LOCKED LIMIT 1`, at most 100 committed transitions per call, actor `system:expiry`, reason `expired`; it is idempotent and partial on error, and `approved`/`auto_approved` are handled by M4-06 at apply time. **Grant delta:** `20261112120000_m4_03_proposal_state_grant.sql` grants only `UPDATE (status, version)` on `proposals` to `recon_app` (no table-level UPDATE, no payload/rationale/evidence/state_hash/expires_at, no INSERT/DELETE); its Down revokes only those columns. **Cross-task contracts:** (1) `expires_at` has no production writer yet; M4-04/M4-11 set the 48-hour policy TTL (`proposal_ttl_hours: 48`) and NULL means no expiry, so production `ExpireDue` is a no-op until then. (2) `apply_failed -> approved|auto_approved` are retry edges only; M4-06 owns the retry cap and mandatory `state_hash`/break re-validation. (3) M4-10 must supersede the old active proposal and insert the new one in one transaction or hit the partial active-case unique index (`23505`). (4) Q-06: there is no `escalated` proposal status; `apply_failed` stays non-terminal and case escalation is M4-06/M4-10. **Docs:** `RenderMarkdown()` is the single renderer, `tools/state-machine-doc` is the required `-out` generator, the `//go:generate` directive regenerates `docs/state-machine.md`, and a byte-for-byte drift test plus shape test prevent manual drift. No expiry index is added in M4-03 (future partial-index optimization); `docs/spec.md` is untouched. | M4-03 / M4-04 / M4-06 / M4-10 / M4-11 | Recorded as the M4-03 state-machine, audit, expiry, grant, and docs contract; consumers must not widen these rules. |
| Q-85 | **QUEUED (M4-03 post-review):** six non-blocking residuals and one accepted-risk note from the QA/adversary review. (1) A proposal row with `expires_at = '-infinity'` sorts first and the `time.Time` scan fails, so every `ExpireDue` call returns `(0, err)` and healthy due rows never drain; a `+infinity` row also blocks a direct `Transition` on that row. Add a finite-value guard (`isfinite(expires_at)` or explicit `pgtype.Timestamptz` handling with skip and metric) before M4-11 writes the TTL. (2) `Transition` records the caller's `ProposalID` string as the audit `entity_id`, so an uppercase/non-canonical UUID spelling creates an audit row that canonical-ID queries miss; use the `RETURNING` snapshot's `id::text`. (3) A non-empty malformed `ProposalID` reaches `$1::uuid` and returns raw SQLSTATE `22P02` rather than typed `ErrInvalidRequest`/`ErrProposalNotFound`; validate/canonicalize the UUID before the query. (4) The legal `apply_failed -> approved` retry can hit `proposals_one_active_per_case` with an untyped `23505` when another proposal is active on the case; M4-06 must sequence supersede/retry atomically (with M4-10) and return a typed conflict. (5) The M4-03 app grant covers only `UPDATE(status, version)`, so `recon_app` cannot set `expires_at`; M4-11 needs an additive grant or a dedicated TTL writer. (6) A required reason made only of Unicode format characters (e.g. U+200B, U+202E) passes the control-character check and stores a visually blank audit reason; reject format/zero-width characters. Separately, the required column-level `UPDATE(status, version)` grant lets `recon_app` bypass this state machine with direct SQL; accepted for M4-03, but M4-06/M4-13 must add applied-proposal integrity checks and keep the app write surface narrow. | M4-11 / M4-06 (M4-10 for retry and supersede sequencing; M4-13 for app-bypass integrity) | Add the finite-expiry guard and a regression test before the TTL writer lands; use the canonical returned ID and pre-validate malformed UUIDs; define the retry/supersede transaction contract in M4-06/M4-10; add the `expires_at` grant when M4-11 owns the TTL; treat format/zero-width characters as invalid reason content. |

## M4-04 policy engine (Q-86)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-86 | **QUEUED (M4-04 post-delivery residuals):** the policy engine is delivered with strict loading, raw-byte SHA-256 versioning, T2/T0/T1 precedence, stable decision JSON, and an additive column grant. Follow-ups and confirmations: (1) **Minor units confirmed:** every `max_value_minor` value is literal minor units — USD 500 = $5.00 auto-apply and USD 50,000 = $500.00 single-approval — with no multiplication or exponent reinterpretation; JPY 500 is ¥500 (0 exponent). Production tuning changes `policy.yaml` values only. (2) **Repeated counterparty is caller-supplied and inert in production:** M4-04 implements the T2 branch and `Facts.RepeatedCounterparty`, but no counterparty identity/window exists in `external_records`, `breaks`, or `cases`; M4-10/M4-11 must define identity, window, and ownership before relying on it. (3) **Verifier placeholder frozen:** M4-05 must feed `policy.Verification{Pattern, Passed, Details}` and set `proposals.pattern_verified`; policy uses only `pattern_verified`, never `pattern_claimed`. (4) **Customer-funds accounts are external:** `Options.CustomerFundsAccounts` is supplied once from `config.LedgerConfig.CustomerFundsAccounts`; the committed default is empty, so policy-level FIN-13 depends on operator config, and the ledger service remains the independent backstop. M4-11 must pass the configured list. (5) **Decision audit ownership:** `PersistDecision` writes only `risk_tier`/`policy_decision` and appends no audit row; M4-11 must call it in the same transaction as `approval/state.Transition` and extend that single transition audit entry with the decision summary, or FIN-30 is at risk. (6) **Grant delta:** `20261112130000_m4_04_policy_decision_grant.sql` adds only `UPDATE(risk_tier, policy_decision)` for `recon_app`; `status`/`version` remain M4-03's grant, `expires_at` remains M4-11's additive grant (Q-85(5)), and `recon_executor`/`recon_agent_ro`/`recon_audit_ro` gain nothing. Direct column-level SQL can bypass the engine; M4-06/M4-13 own integrity checks. (7) **`request_info` routing:** it is always T1 (never a configurable T0 type) and lands in the normal approval queue; M4-11 may short-circuit it before policy. (8) **Stale spec reference:** the spec prose names `config.SwitchAutoApply`, but the constant lives in `internal/api` as `api.SwitchAutoApply`; M4-04 consumes the caller-supplied `Facts.AutoApplyEnabled` and does not import either package. M4-09/M4-11 must use or deliberately re-export the API constant. (9) Q-72(2) content verification is now implemented: `EnsureVersion` fails closed with `ErrConfigHashMismatch` on stored-YAML drift and never overwrites a version row. | M4-05 / M4-09 / M4-10 / M4-11 / M4-13 | Keep the delivered policy contract and grants; fill the verifier, repeated-counterparty, customer-funds, TTL/audit, and routing gaps in their owning tasks, and reconcile the `SwitchAutoApply` location in docs rather than editing `docs/spec.md`. |

## M4-04 post-adversary hardening (Q-87)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-87 | **QUEUED (M4-04 post-adversary residuals):** four cross-task contracts and one hardening item. (1) **Cross-currency is exact and fail-closed:** M4-04 now returns T2/cross_currency when the proposal value currency differs from the case currency, when any line currency differs from the case currency, or when line currencies span more than one currency. Comparison is exact string equality, so an empty/unknown currency differs from a known one and escalates rather than auto-applying; this is stricter than the one-line spec example and `docs/spec.md` is untouched. M4-11 must populate `Facts.Currency`, `CaseCurrency`, and every `LineCurrencies` entry from the same proposal/case snapshot, or accept the stricter T2 result. (2) **Agent pre-seedable verifier fields:** `recon_agent_ro` holds table-level `INSERT` on `proposals`, so the agent can write `pattern_verified` and `verification` (and `risk_tier`/`policy_decision`) through direct SQL even though `internal/agent/proposal/insert.go` omits them. The existing M2 tests only prove the helper omits them, not that the grant prevents them. M4-05 must overwrite `verification` with deterministic verifier output, and M4-11 must derive `pattern_verified` from that output before calling the policy engine; alternatively add a column-level `INSERT` grant that excludes owner-only fields. Policy must never trust an agent-supplied `pattern_verified`/`verification`. (3) **Demo reset concurrency race:** `make demo-m1` runs `demo seed ... --reset`; concurrent demo/reset invocations can drop or recreate shared demo tables mid-run (Q-83(4) observed one `relation "settlement_batches" does not exist` failure). Serialize demo runs with an advisory lock or make reset transactional/idempotent; v1 demos assume one run at a time. (4) **Config existence requirement:** `PersistDecision` now fails closed with typed `ErrUnknownConfig` unless `Decision.ConfigHash` already has a `policy_config_versions` row, and `NewEngine` rejects a `LoadedConfig.Config` that is not deeply equal to its YAML parse (`ErrConfigMismatch`) while deep-copying all maps/slices. M4-11 must call `EnsureVersion` in the same transaction immediately before `PersistDecision` (or guarantee a committed version), and any fresh environment must seed the policy version before the first decision. | M4-05 / M4-11 / M4-13 | Keep the policy contract: exact cross-currency facts, verifier-owned verification/pattern_verified, serialized demo reset, and EnsureVersion-before-PersistDecision. Do not edit `docs/spec.md`. |

## M4-04 final-review hardening (Q-88)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-88 | **QUEUED (M4-04 final-review hardening):** three low-severity, non-blocking adversary findings. (1) **Float literals truncate into integer fields:** yaml.v3 accepts `proposal_ttl_hours: 1.9` as `1`, `auto_apply.max_value_minor.USD: 500.9` as `500`, and `schema_version: 1.9` as `1` because those fields are Go ints. Caps/TTL only shrink and the document is trusted operator input, so a cap cannot be bypassed; a strict loader should still reject non-integer scalars for `schema_version`, every `max_value_minor` entry, and `proposal_ttl_hours`. (2) **`EnsureVersion` does not re-parse its inputs:** it checks `HashYAML(YAML) == Hash`, `Config.Validate()`, and exact stored-byte equality, but never parses `YAML` or requires `reflect.DeepEqual(parsed, Config)`. A caller can therefore store document B while supplying Config A (or unparseable YAML), and `EnsureVersion` will accept it. No decision can use that pair because `NewEngine` rejects it with `ErrConfigMismatch` and `PersistDecision` requires a known hash; the follow-up is to reuse `NewEngine`'s parse + DeepEqual check. (3) **Reason codes are not tier-checked:** `Decision.Validate` checks the primary rule against the tier and every reason for vocabulary/duplicates, but not every reason against the tier; a forged T0 `auto_apply` with `reasons: [auto_apply, customer_funds]` or T1 `verifier_missing` with `[verifier_missing, high_value]` passes. The engine never emits these and direct SQL writes by trusted roles are already accepted (Q-86(6)), but a stricter validator should reject out-of-tier reason codes. | M4-04 follow-up / M4-11 | Add the three hardening checks in a focused follow-up: reject non-integer YAML scalars for integer fields, make `EnsureVersion` parse and DeepEqual like `NewEngine`, and validate every reason code against the tier's allowed set. This docs pass changes no code and does not edit `docs/spec.md`. |

## M4-05 deterministic verifiers (Q-89)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-89 | **QUEUED (M4-05 post-delivery residuals):** six cross-task contracts and data gaps. (1) **No `fx_rates` seed:** generated and committed `dev`/`test` datasets contain fee schedules but no `fx_rates` rows, so `fx_rounding` fails closed with `missing_rate` and live verifier outcomes for that pattern are zero until rates are seeded. Deterministic rate seeding remains deferred to M4-10/M4-11 or a focused generator fix; the verifier never invents or defaults a rate. The DB-free `internal/policy/verify/groundtruth_test.go` now builds both committed splits and proves this fail-closed path (`missing_rate`, zero false passes). (2) **FX source principal is not stored:** `external_records` carries net amount, fee, currency and dates but no source-amount field, so `fx_rounding` checks the exact conversion interval from the stored rate plus the observed half-even-rounded (external) and truncated (ledger) target amounts. It cannot independently reconstruct the source principal; adding it is a schema/generator change, not a verifier relaxation. (3) **Generator fee rounding mismatch:** `synth/fees.go` uses integer half-up while FIN-06 and the fee verifier use half-even. A generated fee at an exact 0.5 minor-unit tie can therefore fail `fee_mismatch` (false negative, never a false pass). Keep the verifier half-even and change the generator only in a focused follow-up; the dev/test ground-truth run hit no tie. (4) **Break selection contract:** the service requires exactly one cited unresolved break (`open` or `in_case`) whose canonical external/ledger refs are all present in proposal evidence; zero or multiple matches fail closed with `break_unresolved` and write nothing. (5) **Reference candidate policy:** candidates must share the break scope/source and external currency, match the external amount exactly, fall within the inclusive `value_date` window anchored on `External.ValueDate` (falling back to `OccurredAt`), and carry no active match leg; a second eligible candidate always fails. (6) **Scorecard live population deferred:** M4-05 supplies the explicit `report.VerifierSummary` input and deterministic fixture checks. The DB-free ground-truth unit test now runs every `fee_variance`, `reference_typo`, and `fx_rounding` truth scenario in the generated `dev` and `test` splits without a database: fee_variance and reference_typo pass on both splits (42/42 each across the two splits), fx_rounding fails closed with `missing_rate` (42/42), and every pattern has zero false passes. Durable per-run scorecard population from `proposals.verification`/`pattern_verified` remains deferred to the eval harness, as does deterministic `fx_rates` seeding. | M4-10 / M4-11 / M4-13 | Keep the verifier contract fail-closed: seed FX rates before claiming fx population, keep half-even fee math, require a unique cited break and one reference candidate, and implement live scorecard population in the eval harness without weakening the explicit report input. M4-13 owns audit traceability. Do not edit `docs/spec.md`. |

## M4-06 executor (Q-90)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-90 | **QUEUED (M4-06 post-delivery residuals):** cross-task contracts and accepted boundaries. **Final adversary residuals addendum (docs review; all non-blocking):** (a) the infrastructure classifier covers `context.Canceled`/`DeadlineExceeded`, `pgx.ErrTxClosed`, and any `*pgconn.PgError`, but not a bare `net.Error`/`io.EOF` driver or socket transport failure, which could still be classified as deterministic; classifier hardening remains a follow-up (M4-08/M4-13). (b) Queue head-of-line fairness: a permanently failing approved row (infrastructure error or otherwise not appliable) can be retried first on each poll, and no starvation proof is committed; M4-11 routing owns mitigation. (c) `Apply` lowercases UUIDs before canonical validation, so uppercase spellings are accepted; spec canonical-lowercase strictness is not separately tested. (d) Rollback/failpoint breadth: the committed rollback tests cover audit/outbox/transition failures but not every write point; M4-08 fault injection owns exhaustive kill-point coverage. None of these blocks M4-06. (1) **State-hash pre-approval reconstruction:** the proposal state hash is computed while the case is `open`/`in_agent`, but the agent loop moves the case to `proposal_pending` before approval. The executor accepts either proposal-time status (and the matching `case:` evidence record fields) as the only allowed pre-approval difference; every other break/record/currency/resolution drift is `state_changed`. Tests cover stale breaks, but this is a reconstruction of the M2-08 hash, not a new hash shape. (2) **Duplicate mark:** a duplicate is an active `matches` header with `rule='duplicate'`, `confidence=1.0`, and no `match_legs`, plus the same synthetic run as a link. It resolves the break/case and claims the external record so a later matcher run does not re-break it; `duplicate_of` is canonicalized and validated but is not persisted on the match row, so the original/duplicate relationship is only in the proposal payload/rationale. M4-10 may add a richer duplicate lifecycle. (3) **Link synthetic run:** each applied link/duplicate creates exactly one `match_runs` row with `status='succeeded'`, `started_at=finished_at=<executor clock now>`, `config_hash='executor/proposal'`, and `stats='{}'`. It is not a matcher run, has no pass counts, and audit/outbox traceability remains per proposal; matchers must not treat it as evidence of a pass. Link headers use `rule='proposal_link'` and one signed scope-account leg per proposed ledger transaction (debit positive, credit negative). (4) **Minimal case resolution vs M4-10:** the executor resolves every attached break (`open`/`in_case`) and the case (`resolved`, `resolution_type` = proposal type, `resolved_at` set, lease cleared) in the apply transaction. Supersession, retries, escalation, late-match handling, and the full case state machine remain M4-10. (5) **Failure classification:** only `state_changed`, `break_resolved`, and `expired` are recorded. Payload drift, external/break binding failures, missing scope entries, active-match conflicts, and deterministic ledger domain errors map to `state_changed`; resolved break/case targets map to `break_resolved`; expiry maps to `expired`. The loop polls every `approved`/`auto_approved` row, including rows whose `expires_at` has passed; `Apply` owns the expiry decision, so an expired row transitions to `apply_failed(expired)` instead of staying approved forever. Infrastructure/transient errors (`context.Canceled`, `context.DeadlineExceeded`, `pgx.ErrTxClosed`, and any `*pgconn.PgError`, including permission denied, statement timeout, and serialization failures) are returned to the caller and leave the proposal `approved`; re-validation loaders classify through `revalidationFailureFor`, so only plain deterministic errors become `apply_failed`. Serialization/deadlock errors retry within the bound; a non-retryable or exhausted database error leaves the proposal approved for the next poll. Unique violations on `matches`/`match_legs` are handled before classification and remain `state_changed` conflicts, never infrastructure retries. (6) **No standalone CLI:** M4-06 exposes `Run` only; M4-14/ops wires the process. A permanently non-applicable approved row (`request_info` or an unknown type) returns `ErrNotApplicable` and remains approved; `Run` waits before re-polling a no-progress batch to avoid a busy loop, but M4-11 must not approve such rows (or M4-10 must route them). Expired approved rows are not in this category: `Run` still polls them and `Apply` drains them as `apply_failed(expired)`. (7) **External-only link candidate window:** an external-only break may propose the unique M4-05 scope candidate (`reference_window_days=3` default) because the frozen `Options` contract has no verifier-config field; if M4-05's configured window changes, the executor's fixed default would need a follow-up contract change. (8) **Break selection:** link/duplicate require exactly one attached break carrying the payload's external ref; multi-break cases fail closed as `state_changed`, and link additionally requires that break to have exactly one external ref. `duplicate_of` is canonical-validated but not existence-checked by the executor (M2-08 proposal validation owns that check). For link/duplicate the `executor.applied` audit and `proposal.applied` outbox payload carry an empty `ledger_txn_id` and `applied_proposals.ledger_txn_id` is NULL. (9) **Value-date fallback:** an adjustment/write-off value date is the earliest external `value_date` referenced by the case breaks, then the earliest `value_date` among the proposal's cited `ledger_txn:` evidence refs (not the break's `ledger_txn_ids`), then the clock date. Malformed cited ledger refs are deterministic `state_changed`; database errors remain infrastructure and leave the proposal approved. | M4-08 / M4-10 / M4-11 / M4-13 / M4-14 | Keep the executor contract fail-closed and add no payload/state_hash write grants. M4-10 owns duplicate/link lifecycle enrichment and case retry/escalation; M4-11 must not approve non-applicable types and must supply any future verifier window; M4-13 owns audit traceability. Do not edit `docs/spec.md`. |

## M4-07 outbox worker (Q-91)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-91 | **QUEUED (M4-07 post-delivery residuals):** nine accepted boundaries and cross-task contracts. (1) **Lease vs handler duration:** `DefaultLease` and `DefaultHandlerTimeout` are both 30s. A handler that outlives the lease can be redelivered by another worker before it returns, so handlers must be idempotent on `Message.IdempotencyKey` and should finish well inside the lease; there is no lease renewal, and both values are per-worker options. The lease covers the whole claim: every row in a batch is leased at `claim_now + Lease`, so later messages in a large batch can still be processed after the lease even when each individual handler is short. At-least-once handlers must therefore dedupe on `Message.IdempotencyKey` regardless of batch position or size; the worker does not renew a per-message lease, and operators should size `BatchSize × handler budget` well inside the lease. (2) **Crash-after-claim attempts semantics:** a crash after the claim transaction commits but before a delivery mark leaves `attempts` unchanged; the row becomes due again when the lease expires. `attempts` increments only on handler failure, and an unknown-topic dead-letter does not increment it. Delivery is at-least-once, never exactly-once. (3) **No last-error column:** the M4-01 outbox schema is unchanged, so dead-letter/retry diagnostics exist only in worker logs and `Metrics`; no last handler error is persisted. Later fault-injection or integrity work must not assume a stored failure reason. (4) **Unknown topic:** an unregistered topic is dead-lettered immediately with `attempts` unchanged and one `DeadLettered` metric; producers must register every topic before deployment, and M4-11/M4-12 must register their topics or their events are lost rather than retried. (5) **Enqueue aggregate/DB clock:** the four-argument `Enqueue` records `aggregate_type='outbox'` and `aggregate_id=idemKey`; `EnqueueAggregate` carries a caller-owned aggregate such as `proposal/<id>`. Both use `CURRENT_TIMESTAMP` for `created_at` and `next_attempt_at` and never read the Go clock, while the worker uses the injected `clock.Clock` for due selection, lease, and backoff; tests may insert explicit timestamps. Q-72(3) is addressed at the API boundary: `Enqueue` returns whether a row was inserted, so a zero-row idempotency conflict is visible to the caller; producers must namespace idempotency keys by topic/aggregate and must not ignore the bool. (6) **No jitter:** failure backoff is deterministic `min(BaseDelay*2^(attempts-1), MaxDelay)`; there is no randomization, so tests can pin `next_attempt_at`. Production may add jitter later without a schema change. (7) **Metrics-only alert:** `Metrics` is an interface with a no-op default; `DeadLettered` is the alert hook, not a paging integration and not durable alert state. M4-14/M5-03 own process and metrics wiring. (8) **No CLI/wiring:** M4-07 exposes `NewWorker`, `Process`, and `Run` only; it starts no process, server, or graceful-shutdown path. M4-14/ops owns wiring the worker. (9) **M4-06 direct insert vs Enqueue:** M4-06 keeps its direct `proposal.applied` insert (explicit Go UUID, `aggregate_type='proposal'`, `aggregate_id=<proposal id>`, topic `proposal.applied`, key `proposal.applied:<proposal id>`, caller-clock timestamps) and does not call `EnqueueAggregate`; the M4-07 interop test inserts the identical row through the real `recon_executor` login and proves the worker claims/delivers it once. Adopting `EnqueueAggregate` later is optional and must preserve the topic, payload, and idempotency key. The `httpguard` package is test-only and is not wired into production transports. | M4-08 / M4-10 / M4-11 / M4-12 / M4-14 | Keep delivery outside transactions and do not add lease renewal or a last-error column in M4-07; register every topic before enqueue; treat logs/metrics as the only dead-letter diagnostics; wire the worker, metrics, and graceful shutdown in M4-14; keep `httpguard` test-only. Do not edit `docs/spec.md`. |

## M4-08 concurrency and fault-injection tests (Q-92)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-92 | **QUEUED (M4-08 post-delivery residuals):** seven accepted boundaries and cross-task contracts. (1) **Subprocess tag and exit-code contract:** the crash matrix exists only in an `integration && faultinject` build; the parent re-executes `os.Args[0]` and requires exit code 86 for `<point>:exit`. The helper skips unless `RECON_FAULT_SUBPROCESS=1` and reads `RECON_TEST_DSN`, `RECON_TEST_PROPOSAL_ID`, and `RECON_TEST_NOW`; a plain `integration` build links the empty `fault.Hit` stub. Changing the exit code or helper env names requires updating both `internal/fault/fault.go` and `internal/executor/fault_integration_test.go`. (2) **Rollback on disconnect:** `os.Exit(86)` closes the child's connection while the SERIALIZABLE apply transaction is open, and PostgreSQL rolls that transaction back; the parent observes no ledger/applied/outbox/audit/break/case change before the retry. `before_commit` is before the commit call, so the matrix does not simulate a lost COMMIT acknowledgement or a partition after COMMIT is sent; ambiguous-commit exactly-once still relies on the ledger idempotency key and the `applied_proposals` primary key, and no test proves the unknown-outcome case. (3) **Invariant scope and type rules:** `invariants.Check` is read-only and checks only cross-table guarantees: every non-reversal ledger txn has a `ledger.post` audit and every `kind='reversal'` txn has a `ledger.reverse` audit, both for entity `ledger_txn` (a missing or wrong action is reported); every applied proposal has its applied/outbox/transition/executor/break/case rows and audits; adjustment/write_off applied rows require a non-NULL existing ledger txn while link/duplicate require NULL; a referenced ledger txn is unique to and keyed `proposal:<proposal id>`. Base ledger txns need the audit but need not be proposal-linked. It does not re-check double-entry balance (ledger trigger), the audit hash chain (M4-13), or outbox delivery (M4-07), and it requires the caller's role to SELECT `ledger_txns`, `proposals`, `applied_proposals`, `outbox`, `audit_log`, `breaks`, `cases`, and `case_breaks`; M5-06 owns the production pool/role choice. (4) **`make test-fault` is separate:** it runs `go test -race -tags='integration faultinject' -p 1 ./internal/fault ./internal/invariants ./internal/executor -run 'Fault\|Invariants\|Check\|Concurrency'` and is not in `verify`; it needs the faultinject tag, a live Postgres, testdb clones, and subprocesses. `-p 1` serializes package binaries because the executor fault matrix clones ten databases and runs child processes. Test selection is name-based: concurrency tests use the `TestConcurrency` prefix, fault unit tests `TestFault`, and invariant tests `TestCheck`; renaming any of them requires updating the target. Cold runtime is ~15s locally (fault ~1.2s, invariants ~5.9s, executor ~6.3s) and scales with clone/subprocess time, so it stays a local release check rather than CI. (5) **40001 wrapper:** `TestConcurrencyExecutorSerializationRetry` wraps `db.Beginner`, returns `*pgconn.PgError{Code:"40001"}` on the first `BeginTx`, delegates later calls, and records `RetryPolicy.Sleep`; with `MaxAttempts=2` the second attempt applies exactly once. This proves the executor's retry path deterministically without lock contention; `internal/db/tx_test.go` remains the low-level backoff/exhaustion coverage. (6) **No schema or CI:** M4-08 adds no migration, grant, schema change, or CI workflow, and `verify` is unchanged; the tests use existing tables and isolated testdb clones. (7) **Retry bound vs audit-chain contention:** every apply appends to the single audit chain under `pg_advisory_xact_lock`, so a SERIALIZABLE waiter can need one attempt per simultaneous writer. With six same-account contenders the default five-attempt policy exhausted at `audit: read chain head` (SQLSTATE 40001), so the test uses four contenders. M4-14/ops must bound concurrent executor processes or raise `Retry.MaxAttempts` before running more than five applies against the same audit chain at once. | M4-08 / M4-11 / M4-13 / M4-14 / M5-06 | Keep exit code 86 and the helper env contract; do not claim coverage of an ambiguous COMMIT; keep `Check` read-only, type-specific, and reusable by M5-06; keep `test-fault` out of `verify` and CI; size concurrent executor processes to the audit-chain retry bound (or raise it); preserve the `proposal.applied` outbox row/key; add no schema/grant migration. Do not edit `docs/spec.md`. |

## M4-09 auto-apply limits and circuit breaker (Q-93)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-93 | **QUEUED (M4-09 post-delivery residuals):** FIN-53 is delivered as `internal/limits`, with local `auto_apply_limits` config and no policy/schema change. (1) **Daily cap source:** cumulative is the sum of `amount_minor` on `debit` lines in the candidate currency for proposals with `risk_tier='T0'` and status `auto_approved` or `applied` whose `created_at` or joined `applied_proposals.applied_at` is at/after UTC midnight. Link/duplicate proposals without `lines` contribute zero; credit lines and other currencies are ignored; a non-array `lines` value, non-object line, missing/non-integer/negative `amount_minor`, or arithmetic overflow fails closed as `limits_unavailable`. The query has no upper day bound (matching the approved spec SQL), so a future-dated row would count; the payload is parsed in Go rather than cast in SQL so malformed JSON cannot become a raw database error. A non-empty `Candidate.ProposalID` is validated as a canonical lower-case UUID and excluded from the sum (`p.id <> $2::uuid`) so a candidate whose row is already `auto_approved`/`applied` is counted once via `ValueMinor`; an empty ID excludes nothing and a malformed ID fails closed as `limits_unavailable`. (2) **Break rate:** breaks detected in the UTC day (all break statuses) divided by breaks plus active matches created in the UTC day; exactly 10% (1,000 bps) trips, with a minimum sample of 20 events and integer basis-point arithmetic. Unmatched matches are excluded and the window is `[UTC midnight, next UTC midnight)` from the injected clock; a 19-event window never trips even at 100%. (3) **Trip on candidate:** a T0 candidate whose `cumulative + value` would exceed the cap is routed T1 `daily_cap_exceeded` and trips the breaker even though the candidate row was not written; `cumulative + value == cap` is allowed, while `cumulative >= cap` trips even for a zero-value candidate. Break-rate spikes are checked before the cumulative query, so a spike trips without needing any T0 proposal. (4) **Config block:** the root `auto_apply_limits` YAML block holds `daily_max_value_minor`, `break_rate_threshold_bps`, and `break_rate_min_sample`; built-in daily caps are USD/EUR 10,000, SEK 100,000, JPY 10,000 minor units, threshold 1,000 bps, minimum 20. Missing map entries fall back to the built-ins, zero scalar fields (including an explicit zero `break_rate_threshold_bps` or `break_rate_min_sample`) mean "use the default" rather than invalid, explicit non-positive caps and non-zero out-of-range tuning are rejected by `Validate`, and there is no environment binding; `policy.yaml` is untouched. A runtime currency absent from the effective map returns T1 `currency_cap_missing` without tripping. (5) **Reset reuse:** there is no second reset path. The existing admin-only `POST /v1/admin/switches` remains the reset; a new API integration test proves the seeded switch is false, a viewer gets 403 (malformed body, switch writer not reached), an admin reset returns 200 with `human:dev-admin` and exactly one human `switch.set` audit, and a following `limits.Check` passes. (6) **M4-11 wiring/read race:** `limits.Check` is the integration API; M4-11 must persist `Result.Reason` in its approval audit/decision extension and route a T1 result. Check reads the switch, break rate, and cumulative in separate short queries and then trips; it is not a serializable snapshot, so a concurrent T0 can pass validation before another candidate trips the breaker, and a trip can race with an in-flight M4-11 decision. M4-11 should call Check inside its approval transaction or explicitly accept that race; already-checked candidates are not retroactively invalidated. Concurrent breaker trips are safe (row lock means one update/audit; later trips return `false` with no audit). (7) **Local vocabulary:** `internal/limits` deliberately keeps local `auto_apply_enabled`, `switch.set`, and `system_switch` constants instead of importing `internal/api`; the API reset integration test pins the shared values, and M4-11 can import the limits constants. (8) **No schema/policy change:** M4-09 adds no migration, grant, executor, outbox, or policy change; the package doc records the trip/reset boundary. (9) **Candidate tier and no-reservation contract:** T1/T2 pass through as `not_t0`; T0 is gated; an empty or unknown tier fails closed to T1 `invalid_tier`. `Check` cannot reserve, because the cumulative query counts only persisted `auto_approved`/`applied` proposals: M4-11 must persist each auto-approved result before checking the next candidate, otherwise sequential checks each read the same total and can bypass the cap (a test demonstrates that the persisted row, not a reservation, enforces the next check). M4-09 adds no reservation table/API. | M4-11 / M4-10 / M4-13 | M4-11 should call `Check` in its approval transaction (or record the accepted read race), persist each auto-approved result before checking the next candidate, persist the T1 reason, and treat any Check error or failed-closed result as no auto-apply. Do not add a reset path or schema change in M4-09. |

## M4-11 approval service and channel interface (Q-94)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-94 | **QUEUED (M4-11 post-delivery residuals):** the approval service, decision route, and channel interface are delivered. (1) **API reason is not persisted:** `POST /v1/proposals/{id}/decision` accepts and bounds an optional `reason` (2000 runes, matching the state audit limit) but `Decide`/`DecideByAPIKey` have no reason parameter, so the `approvals` channel and the rejected transition reason are the source `api`, and `approval.decided` records the source rather than the human reason. `CallbackDecision.Reason` has the same gap; M4-12/M4-10 must extend the decide contract or the route's reason field should be documented as reserved only. (2) **Outbox topic registration:** `approval.request` is still unregistered with the worker (Q-91(4)), so T1/T2 requests dead-letter until M4-12/M4-14 register the topic; M4-11 only enqueues. (3) **No network adapter:** `Channel`/`FakeChannel` compile and record, but no Slack/ticket send, callback signature verification, `external_ref` write, or cancel implementation exists; M4-12 owns them. (4) **Case lifecycle:** approved, rejected, and expired proposals do not reopen, return to open, or escalate their cases; M4-10 owns those transitions and the rejection retry cap. (5) **Non-executable proposal types:** `Request` routes `request_info`/unknown types through policy and `Decide` can approve them; the executor returns `ErrNotApplicable` and leaves them approved. M4-10 must route/reject them or a focused M4-11 follow-up must fail closed. (6) **Limits read race:** limits are checked before the final request transaction (`Q-93(6)`); concurrent T0 candidates can each read the same cumulative total, and each auto-approved row must commit before the next candidate is checked. This is accepted for M4-11. (7) **Verifier persistence boundary:** the verifier persists `verification`/`pattern_verified` outside the final request transaction; `Request` re-checks `proposed` but not verification drift, matching the approved spec 8.3 boundary. (8) **Co-maker derivation:** four-eyes compares the approver UUID and `approvers.api_key_id` against human `audit_log` actors on the proposal, case, and agent run; no explicit editor/request-info action or agent-run edit exists yet. M4-10 may add explicit maker actions without changing the check's public error. (9) **Approver identity:** one static `approvers.api_key_id` per approver maps a configured key to a human; `recon_app` cannot write `approvers`, so onboarding/rotation stays an out-of-band owner operation (M4-14 ops). Missing, inactive, or non-approver mappings are 403 and the key value is never echoed. (10) **API contract:** the route returns exactly 200/400/401/403/404/409/413/415/500; 200 includes same-approver replay, 400 covers invalid body/decision/reason/path UUID, 403 covers role and approver mapping, 404 covers a missing proposal, 409 covers not-pending/expired/conflict/co-maker, and an unexpected failure is a generic 500. `decided_at` is the injected clock truncated to microseconds; status/decision are `approved|rejected`. (11) **Startup dependency:** `cmd/api` now constructs policy, verifier, limits, proposal state, and approval services and fails closed on any construction error, so the decision route requires `policy.yaml` and a seeded `approvers.api_key_id` mapping even when the deployment only serves reads. | M4-10 / M4-12 / M4-13 / M4-14 | Register `approval.request` before deployment, carry the human reason through the decision contract if audit fidelity requires it, keep the exact route status set, route non-executable proposal types explicitly, seed/rotate approver API-key mappings out of band, and leave case lifecycle and channel adapters to M4-10/M4-12. Do not edit `docs/spec.md`. |

## M4-10 case lifecycle (Q-95)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-95 | **RESOLVED (M4-10), with recorded boundaries:** the case lifecycle is delivered as `internal/cases/state` (the sole `cases.status` writer) plus `internal/cases/lifecycle` (supersession, late match, retry/expiry, waiting re-entry, missing-settlement SLA, admin unmatch), with `docs/cases.md` as the operator/developer reference. Recorded decisions and deviations: (1) **Supersession ownership:** proposal supersession is app/server-only. A second agent proposal still hits `proposals_one_active_per_case`, and the agent role cannot supersede/update proposals. The success path writes exactly one `approval.cancel` outbox row keyed `approval.cancel:<proposal id>` and leaves the case in `proposal_pending` for a replacement path; approved/auto-approved proposals are never cancelled by supersession. (2) **Approved late match:** approved/auto-approved proposals are deliberately left intact by late-match reconciliation; the executor's existing revalidation records `apply_failed(break_resolved)`. M4-10 makes no second case or proposal write for that variant. (3) **Shared attempts cap:** `cases.attempts` is the single shared counter for agent failures and rejected/expired proposals. The lifecycle retry path caps at the fixed `lifecycle.AttemptCap = 3`; agent failures use the operator-configurable `worker.max_attempts` (default `3`, strict range `1..10`). With the default both are 3; a non-default `worker.max_attempts` moves the agent-side escalation point to that value while the lifecycle retry cap stays 3, so the effective caps differ. There is no `cases.max_attempts` key. Post-increment semantics: lifecycle retries 1 and 2 reopen `open` with `proposal_rejected`/`proposal_expired`; increment 3 (or more) escalates with `attempts_exhausted`. The worker poison-case/no-model-call path stores and audits `attempts_exhausted` (replacing the older `agent_failed` wording). (4) **Partial late match:** when only some attached breaks are consumed, the active proposed/pending proposal is superseded and cancelled and the case reopens to `open` with `partial_late_match` (including from `escalated`); remaining breaks stay attached. A case resolves with `superseded_by_match` only when every attached non-void break is terminal (`resolved`/`superseded`); `void` breaks do not block resolution. (5) **Missing-settlement SLA (implemented, dormant in production):** default 5 business days; anchor is the earliest `detected_at` among attached breaks (including void); source is derived from break scope; the classifier requires at least one attached break and every attached non-void break to be `ledger_only`; mixed-source cases are excluded and audited `case.sla_skipped_mixed_source` (no guessing) only when every non-void break is `ledger_only` — a case with any non-ledger-only break fails eligibility first, so that exclusion has no audit row and is visible only as a `SweepResult.Skipped` count; cutoff, timezone, DST, and holidays use the extracted `internal/bizdays` FIN-41 calendar; escalation occurs only when the source-local date is strictly after the due date. An all-void-break case is interpreted literally: it has attached breaks and the non-void predicate is vacuously true (documented rather than special-cased). A source with no configured calendar is a silent skip (counted, no audit, no error). **Reachability dependency:** the classifier cannot fire from real matcher data today because no production pass emits `ledger_only` breaks. The sole production `breaks` writer is fed by `match.Engine.Run`; `pass3` emits `external_only`, `pass5` emits `duplicate`, and `SideLedgerOnly` is only a type/validation value in production code (tests and synth metadata are not producers). A matcher/ingest follow-up must add the ledger-only break producer before the M4-14 end-to-end missing-settlement gate; missing-settlement escalation is not operational end to end today. No scheduler/worker tick calls the sweep yet (see (12)). (6) **Unmatch:** CLI-only `recon match unmatch --id <match-uuid> --reason <text> --actor human:<id> [--json]`; the admin host needs only `database.app_url` (`DATABASE_APP_URL`), and there is no HTTP route/UI/auth model. Exit codes: 0 success, 2 usage/validation, 1 not found/already unmatched/`ErrAdjustmentApplied`/transaction failure. `SupersedeActiveProposal` has one narrow resolved-case exception: cancel-only (`newProposalID == ""`) with reason `case_reopened` is allowed so the spec 5.6 order (supersede, then reopen) works; every other resolved-case supersession remains `ErrTerminalState`. (7) **Reversal:** `recon reverse --txn` does not exist and is not registered. The `ErrAdjustmentApplied` refusal names the ledger transaction and the future operator command only, explicitly saying it is not implemented in this release. (8) **M4-12 boundary:** M4-10 owns the DB lifecycle and the `approval.cancel` outbox row only. Cancel delivery, `external_ref` resolution, request-info callback transport, and vendor failures belong to M4-12; the ingest touch hook is wired, but the external request-info transport is not. (9) **Legacy transition migration:** the permissive `cases.Service.Transition` is replaced by a thin delegate to `internal/cases/state`; every production case-status write goes through the authority (the executor now resolves cases through it as `system:executor` and writes one `case.transition`). Legitimate agent-loop case/lease writes use `system:agent-loop`; `agent:*` remains rejected by the authority. Existing `case.attempt`/`case.reclaim`/`case.resolve` audits and counts are preserved, and each real transition adds exactly one `case.transition` row. (10) **Initial version 0:** `CreateCase` inserts `version = 0`, so the authority accepts `ExpectedVersion >= 0` and rejects only negative versions. The spec 4.1 literal `> 0` would make the first transition of every new case impossible; the row-lock + version CAS guard is unchanged. (11) **Escalation reasons:** validated as free text (trimmed 1..500 bytes) rather than the spec 4.4 closed set, so legitimate agent-loop and worker reasons (`ambiguous_evidence`, `injection_flagged`, `attempts_exhausted`, ...) are preserved; canonical lifecycle paths pass the exported constants. Empty/blank remains `ErrReasonRequired`; malformed remains `ErrInvalidReason`. (12) **Sweeps:** `RetryStalledProposals`, `EscalateMissingSettlementSLA`, and `approval/state.ExpireDue` are tested service-level paths with no production caller and no scheduler/worker tick; there is no `cmd/scheduler` and the worker poll loop calls none of them. The approval same-transaction hook is the live retry path, and expiry handled inside the approval decision path is the only hooked expiry path. `ExpireDue` expires due proposals but has no lifecycle hook, so a future scheduler must pair it with `RetryStalledProposals` to repair the cases it leaves in `proposal_pending`; an ops task must decide when to run all three. Until then, time-based proposal retirement and the SLA sweep are dormant. (13) **Audit-chain risk (pre-existing, M4-13-owned):** a concurrent SERIALIZABLE executor `Apply` and READ COMMITTED matcher can surface `23505` on `audit_log_seq_key`: the SERIALIZABLE snapshot is fixed before `audit.Append` takes the chain advisory lock, so it can re-read a stale chain head. The committed concurrency coverage is ordering-only (executor-wins and matcher-wins), not a reproduction of this stale-snapshot race; the race itself remains unprobed. `db.WithTxPolicy` retries `40001`/`40P01`, not `23505`. There is no data corruption: the unique constraint protects the chain, the outbox/worker retries the aborted transaction, and the executor leaves the proposal `approved` on failure. The owning-layer fix (audit API or a narrow retry in that layer) is not part of M4-10. (14) **No migration:** M4-10 adds no migration, grant, or schema change; `docs/spec.md` is untouched, and the docs change is the new `docs/cases.md` plus this entry. (15) **Residual concurrency/coverage notes (no behavior change):** (a) the executor locks proposal→breaks→case while late match/unmatch lock case→proposals→breaks; this lock-order inversion is a residual deadlock risk, `db.WithTxPolicy` retries `40P01`, and no deadlock, partial write, or data loss was observed or probed. (b) Late-match linkage treats any match overlap with a break window as consuming the full break; this is theoretical for current whole-break producers (`external_only`/`duplicate`) and must be revisited if partial-amount breaks are introduced. (c) The mixed-source SLA audit is suppressed when a non-`ledger_only` break is also present because non-ledger eligibility fails first; the exclusion is conservative (no escalation) and visible only as a `SweepResult.Skipped` count. | M4-12 / M4-13 / M4-14 / matcher/ingest / ops | Recorded as the M4-10 lifecycle contract and the accepted deviations above; consumers must not widen agent proposal writes, bypass the case authority, add a second retry counter or `missing_settlement` column, or document `recon reverse` as existing. Add the missing `ledger_only` break producer before the M4-14 end-to-end missing-settlement gate; add scheduler call sites for `ExpireDue` (paired with the retry sweep) and the retry/SLA sweeps; fix the audit-chain SERIALIZABLE interaction and the lock-order inversion in the owning layers. Do not edit `docs/spec.md`. |

## M4-13 audit verification, actor typing, traceability (Q-96)

**Q-96 addendum (docs phase, post-review; adversary findings A-1 to A-4, all non-blocking, no behavior change).** The items below extend the Q-96 numbering:

(10) **Audit-chain threat model (unanchored chain):** `recon audit verify` detects row-level tampering (`hash_mismatch`, `broken_link`), gaps including a deleted prefix, a deleted tail while `audit_chain_head` still points past the last row (`truncated_tail`), and mirror/row disagreement (`head_mismatch`). It does **not** detect a privileged attacker (database superuser or any role that can disable triggers and write storage) who rewrites `audit_log` **and** `audit_chain_head` consistently, or deletes a tail together with the mirror: a self-consistent rewritten chain verifies with exit 0 and no findings. The hash chain is unanchored because its only head witness is a mutable table in the same database and `audit.Append` writes the mirror in the same transaction as the row. An external anchor (published head hash/sequence, WORM/immutable storage, or signed off-box checkpoints) is future hardening and is out of M4-13 scope; operators must not treat exit 0 as evidence against a superuser. Recorded in `docs/audit-and-trace.md` under "Threat model and limits".

(11) **JSONB `null` policy decision:** `proposals.policy_decision = 'null'::jsonb` is not treated like SQL `NULL`: `decodePolicy` unmarshals JSON `null` into a zero `policy` object, so the `policy` marker is suppressed, `policy_config` is emitted (when the config row is present), and the empty decision tier also suppresses the `approvals` marker for what would have been a T1/T2 decision. This state is unreachable through the normal policy store, which validates and persists a decision object, and requires a direct SQL writer; SQL `NULL` is handled correctly (`policy` marker). Follow-up: treat JSON `null` as absent or reject a non-object decision in `decodePolicy`.

(12) **Empty `actor_id` constraint name:** an empty `actor_id` is rejected by the older `audit_log_actor_id_check` (`actor_id <> ''`) because PostgreSQL may evaluate it before `audit_log_actor_id_format`; the row is rejected either way (fail-closed) and the migration test accepts either constraint name. This is constraint layering, not a gap in the new check; `docs/audit-and-trace.md` records the same note.

(13) **FIN tags on adapted tests:** three mechanically changed pre-existing grant/down-migration tests (`internal/db/m4_11_grants_test.go`, `internal/db/verifier_grants_test.go`, `internal/executor/grants_integration_test.go`) keep their original narrower `// covers:` tags because they remain M4-11/M4-05/M4-06 tests; only their M4-13 Down-migration step-over was extended. No assertion, scope, or test intent was weakened, and every new/changed M4-13 audit/trace/completeness/API/CLI/e2e test file carries `FIN-30..FIN-34`.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-96 | **RESOLVED (M4-13), with recorded boundaries (items 10–13 are in the addendum above this table):** (1) **Audit/trace DSN:** `recon audit verify` and `recon trace` use `database.app_url` through `db.OpenRole(RoleApp, ...)` in one REPEATABLE READ, READ ONLY transaction that is always rolled back. No `recon_audit_ro` config key, grant, or DSN was added; a dedicated read-only audit DSN remains a future ops/config hardening option, and the deployment must grant the app role SELECT on the audit/trace tables. (2) **Policy YAML and case-file v2:** `GET /v1/cases/{id}` is `schema_version: 2`, a breaking change for consumers, and exposes `trace.chains[].policy.config_yaml` from `policy_config_versions` to viewer keys; accepted because the YAML is threshold/rule configuration, not secrets, and the OpenAPI description says so. If secret material ever enters policy YAML it must be redacted first. (3) **Unlinked applications:** `applied_proposals` rows with `ledger_txn_id IS NULL` (link/duplicate types) are included as chains with `ledger_txn_id: null`, `ledger_txn: null`, and missing marker `ledger_txn`; consumers must not assume every chain has a ledger transaction. (4) **Go vs PostgreSQL actor parity:** Go `newActor`/`Valid` reject empty, more than 128 runes, `:`, and ASCII space/control (U+0000-U+001F, U+007F; spec 4.2); the named DB CHECK `audit_log_actor_id_format` (`^[^[:space:][:cntrl:]:]{1,128}$`) is the authoritative Unicode backstop and is locale-dependent (PostgreSQL 16 rejects U+2003 but not U+00A0 via `[:space:]` in this locale). The migration fail-closes on existing invalid or NULL `actor_id`, reports only a count and never IDs, and never rewrites rows, so existing hashes stay valid. (5) **Narrow 23505 retry:** `db.WithTxPolicy` retries SQLSTATE 23505 only when `ConstraintName == "audit_log_seq_key"`; every other unique violation is returned after one attempt, the existing 5-attempt bound and rollback-first behavior are unchanged, and exhaustion preserves the original `*pgconn.PgError` through `errors.As`. This is a bounded race-healing retry, not a schema fix and never a blanket 23505 retry. (6) **No trace creation:** M4-13 reads and surfaces the persisted `agent_runs.trace_id` only; it creates no OTel spans or trace IDs. OTel propagation and real trace IDs remain M5-01. (7) **Tamper test:** the superuser tamper coverage requires disabling `audit_log` triggers in a disposable `testdb` clone; triggers are discovered from `pg_trigger`, disabled and re-enabled by name, `tgenabled = 'O'` is asserted afterwards, and no shared dev or template database is mutated. (8) **e2e fixture-only seeds:** the end-to-end test inserts `agent_runs` and `proposals` by SQL because the real agent/proposal-store insert paths are unaudited; every later mutation (verification, policy decision, proposal transitions, approval, apply, ledger post) runs through its owning production service, and every audited triple is asserted with physical row counts. (9) **Boundaries and no spec edit:** `eval/approver`, `recon-eval run --e2e`, and `TestMilestone4Gate` remain M4-14; M4-13 adds no M4 gate and no new table, and the only migration is `20261119120000_m4_13_actor_id_format.sql`. `docs/spec.md` is untouched; the user docs are `docs/audit-and-trace.md` plus this entry. | M4-14 / M5-01 / ops / API consumers | Keep the app-role DSN for audit/trace until a dedicated read-only audit DSN is configured; treat case-file v2 as a breaking consumer change and keep policy YAML free of secrets; include null-ledger-txn chains; keep Go actor rules ASCII-only with the database authoritative; keep the 23505 retry constraint-scoped and bounded; create no trace IDs before M5-01; run tamper tests only on disposable databases; keep the e2e seeds fixture-only and add no M4 gate in M4-13. Do not edit `docs/spec.md`. |

## M4-12 Slack adapter (Q-97)

**Q-97 addendum (docs phase, post-review; final adversary findings F1–F4, all non-blocking, no behavior change).** The final adversary review at `a9ea9f1` recorded four residuals. They extend, and do not change, the boundaries in the table below; none is a release blocker, and AC-15 remains **PENDING — human-gated**. Each item names its severity and owner.

- **F1 — High if it ever reaches a log, non-blocking now (pre-existing/out-of-scope): whole-`Config` JSON/slog redaction stops at Slack.** `json.Marshal(Config)` and `slog.Any("config", Config)` redact the Slack bot token and signing secret (through `SlackConfig.MarshalJSON`/`LogValue` in `internal/config/slack_json.go`) but still render the database DSNs/passwords and the LLM API key verbatim; `Config.String`/`Format` redact all of them. No production call site serializes a whole `Config` (repo-wide grep), so this is a hardening gap that matters the first time a debug JSON dump or `slog.Any("config", cfg)` is added. The comment at `internal/config/slack_json.go:13-15` deliberately assumes "Config needs no separate marshaler"; that assumption only holds for the Slack secrets. **Owner:** separate config-hardening follow-up (propose FIN-62 if broader whole-`Config` redaction is adopted); not an M4-12 failure.
- **F2 — Medium, non-blocking: update-before-insert race can leave a decided message with live buttons.** `HandleRequest` posts to Slack (`handlers.go:232`), re-reads the proposal (`:241`), and only then inserts the `approval_messages` row (`:263`). A decision landing after the re-read and before `InsertPosted` enqueues `approval.update`; the update consumer can run before the row exists and `HandleUpdate` acknowledges an absent row as a no-op (`handlers.go:291-299`). The row is then inserted as `posted`, so the decided proposal keeps a message with live buttons. A click is still refused by the proposal state/version check, so there is no wrong decision, duplicate approval, or money movement — impact is a stale message only. Possible fix: insert a tombstone/placeholder row before posting, or defer/retry `HandleUpdate` when the row is absent instead of treating absence as terminal. **Owner:** M4-12 Slack-adapter follow-up (Q-97 residual); related to existing items (6) and (13).
- **F3 — Low, non-blocking: `PostMessageRef` trusts Slack's response channel/ts.** `PostMessageRef` (`client.go:242-252`) prefers `response.Channel` when non-empty and concatenates `<channel>:<response.TS>`, but `postMessage` only validates that `TS` is non-empty (`client.go:278-280`); the response channel is never checked. A malformed response channel containing `:` produces a ref with more than one colon, which `ParseExternalRef` later rejects ("must contain exactly one colon"), so the message cannot be updated or cancelled. Real Slack channel IDs cannot contain `:`, so this is robustness only. Possible fix: validate `response.Channel` (or prefer the requested channel unless the response channel passes validation) before freezing the ref. **Owner:** M4-12 Slack-adapter follow-up (low priority).
- **F4 — Medium risk at the AC-15 gate, non-blocking at code review: the strict nested `message` allow-list can reject real Slack payloads.** `interactionMessage` is a closed allow-list (`interactions.go:511-524`) and `DisallowUnknownFields` applies recursively (`interactions.go:556-560`). Real Slack message objects can carry fields not in that list — for example `app_id`, `files`, `bot_profile`, or a nested `team` — and those clicks return `400 invalid_request` with no proposal change and no Slack call. All fixtures are sanitized documented shapes, not live captures, so route tests pass while real-payload compatibility is unproven. Before M4-12 is marked done, either capture and test a real interaction payload (AC-15) or relax `message` to `json.RawMessage` and validate only `message.ts` against the stored external ref. If the manual smoke reproduces this rejection, it becomes a release blocker at that gate. **Owner:** M4-12/AC-15 (human smoke operator plus the follow-up fix); Q-97 item (12) is the fixture caveat.
- **AC-15 status (unchanged):** PENDING — HUMAN-GATED / NOT RUN. M4-12 is code complete, not done; the roadmap M4-12 row stays unticked until a human records the real-workspace smoke result in `docs/slack-setup.md`.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-97 | **QUEUED (M4-12 code complete; manual AC-15 smoke pending; post-review residuals F1–F4 recorded in the addendum above):** the Slack adapter is delivered as `internal/approval/slack` (Block Kit, Web API client, transport store, `approval.Channel` implementation, outbox handlers, signed interaction endpoint) plus the minimal `cmd/outbox` binary and `docs/slack-setup.md`. Recorded boundaries: (1) **Replay cache is per process (inherits Q-79):** `webhook.MemoryReplayCache` lives in one API process, so an exact duplicate or a captured request can land on a different replica and pass that replica's cache; decision idempotency (proposal row lock, `approvals_proposal_approver_key`, same-approver replay) is the guard, not the cache. A shared store (Redis/Postgres) is future hardening; the Q-83(2) exact-TTL boundary also still applies. (2) **The retry header is not signed:** `webhook.Options.ReplayExemptHeader = "X-Slack-Retry-Num"` skips only the replay marker when Slack retries. HMAC and the five-minute timestamp are always enforced. A captured valid signed request can be replayed with an added retry header; decision idempotency absorbs the duplicate. Binding a nonce into the signed body or a shared replay store is future hardening. (3) **200-on-business-refusal is deliberate:** authenticated unauthorized, stale/superseded/expired, co-maker, and conflicting-decision outcomes return `200` with a generic ephemeral message so Slack stops retrying; `400` is reserved for malformed input, `409` only for verifier replay, and `500` (Slack retries) for internal or audit failure. This differs from an RPC-style `409` but is the approved Slack contract. (4) **Late-decision expiry records a transition and changes the case, but writes no approval:** a click on a still-`pending_approval` proposal whose `expires_at` is already in the past passes the interaction handler's `stillActive` check (message ref, version, and status only — not time) and reaches `Decide`. `Decide` commits `pending_approval -> expired` through `approval/state` (one `proposal.transition` audit with actor `system:expiry`, reason `expired`) and runs the M4-10 lifecycle hook in the same transaction, so the case follows the retry/expiry path. It writes **no** `approvals` row and no `approval.decided` audit, returns `ErrExpired` without a `DecisionResult`, and enqueues nothing, so the API answers `200` with the ephemeral "This approval is no longer active." and an already-posted message can keep its buttons until a later `approval.cancel` or cleanup. This is intentional M4-11 behavior, not a bug: do not turn the expiry click into a no-op or an approval write. Only a fresh approve/reject/request-info commits an `approval.update`. (5) **`external_ref` is frozen as `<channel_id>:<ts>`:** exactly one colon, non-empty unpadded halves, no scheme prefix, stored only in `approval_messages.external_ref`; `approvals.external_ref` stays NULL for Slack and there are no `proposal_version`/`close_reason` columns. A future ticket adapter may need a different ref shape. (6) **At-least-once post/insert window:** the `approval_messages` row is inserted only after a successful `chat.postMessage`, so a crash between the Slack call and the insert can duplicate the message; `client_msg_id = proposal_id` is a best-effort dedupe, not a guarantee. A cancel that arrives between the post and the insert can still no-op; the post-check/withdraw path mitigates it, and delivery remains at-least-once, never exactly once. (7) **`cmd/outbox` is a minimal separate binary:** it is not a mode of `cmd/worker`, so deployment/supervision must run two binaries and outbox-only hosts need no agent/LLM credentials. It registers `approval.request`, `approval.update`, and `approval.cancel`; any unregistered topic still dead-letters (Q-91(4)). Extending `cmd/worker` later remains possible. (8) **Reason-aware Decide is additive:** `DecisionOptions{Reason, InfoRequested}`, `DecideWithOptions`, and `InfoRequestedHook` are new; `Decide` keeps its M4-11 signature and semantics. The HTTP API `reason` remains bounded but reserved/unpersisted for non-Slack callers (Q-94(1)); only the Slack path carries a human reason into the audit/update. (9) **Request-info reason/audit fields:** `approvals` has no reason column. Durable evidence is the proposal `rejected` with transition reason `info_requested`, the case `waiting_info` (actor `system:case-lifecycle`, attempts unchanged, no retry/escalation), one `approval.decided` audit with `info_requested:true` plus `reason` only when supplied, and one `approval.update` outbox row. A nil `InfoRequestedHook` fails closed with `ErrInfoRequestedUnsupported` before any write. (10) **`Channel` is implemented but is neither the production delivery path nor the crypto boundary:** `internal/approval/slack/channel.go` implements `approval.Channel` (`RequestApproval`, `HandleCallback`, `Cancel`) for interface compatibility and tests; production posting/updates run through the outbox handlers. `HandleCallback` parses an already-authenticated form body; production authentication is the signed `POST /v1/slack/interactions` middleware. It does not verify a signature or touch the replay cache, so do not double-verify or consume the replay marker twice. `approval.CallbackDecision` has no InfoRequested flag, so request-info maps to `DecisionRejected` with reason `info_requested`; callers needing the case lifecycle must use `DecideWithOptions`. (11) **T2 is skipped, not dead-lettered:** `approval.request` with `channel="ticket"` is acknowledged and skipped because M4-12 has no ticket adapter; a T2 row must not be treated as a delivery failure. (12) **Fixtures are sanitized recorded-shape payloads:** the `testdata` Block Kit, client, and interaction fixtures are built from Slack's documented `block_actions`/Web API shapes, not captured from a live workspace; the interaction captures include styled buttons (`style="primary"`/`style="danger"`), which the strict decoder accepts as documented button metadata and never trusts; the manual smoke is the real-workspace validation. (13) **Update/cancel ordering is not guaranteed:** outbox topics have no cross-topic ordering; cancellation is treated as terminal and handlers re-check state after the Slack call, but a narrow transient message flip is still possible. (14) **Manual AC-15 smoke is human-gated and PENDING:** the checklist and results table are in `docs/slack-setup.md`; no result is recorded. M4-12 code does not add a production demo/CLI trigger that creates a T1 proposal and calls `approval.Service.Request`; M4-14 wires the end-to-end pipeline, so the smoke must use M4-14 or a reviewed local harness. Code-complete is not task-done, and the roadmap M4-12 box stays unticked until a human records date, workspace, operator, commit, and result. (15) **Scope and no spec edit:** M4-12 does not implement `eval/approver`, `recon-eval run --e2e`, `TestMilestone4Gate`, OTel, or a scheduler; the only migration is `20261120100000_m4_12_approval_messages.sql`; `docs/spec.md` is untouched. | M4-13 / M4-14 / M5-06 / ops / API consumers | Keep the frozen `external_ref` and `approval_messages` shape; treat delivery as at-least-once and decisions as idempotent; keep the 200-on-business-refusal and 409-only-replay matrix; wire a shared replay store and explicit expiry message cleanup in later hardening; supervise `cmd/outbox` as its own binary; do not rely on the unsigned retry header for authentication; keep `reason` reserved for non-Slack callers; record the AC-15 result in `docs/slack-setup.md` before ticking M4-12; leave M4-13/M4-14 scope and `docs/spec.md` untouched. |

## M4-14 simulated approver and end-to-end pipeline (Q-98)

**Q-98 addendum (W6 docs phase; A8 docs exception).** M4-14 is delivered as
`eval/approver`, the `recon-eval run --e2e` pipeline/runner/report path, the
`t0_false_fix` gate, and `TestMilestone4Gate`. This supersedes Q-68's
`t0_false_fix: applicable: false` deferral: the threshold is now applicable and
a zero T0 denominator is a gate error. The residuals below are recorded
boundaries, not release blockers. Each names its consumer and follow-up owner.

- **Record-only dev evidence; no committed eval cassettes (A5).** The 12 M2
  fixture cassettes remain the only replayable corpus. A dev-split
  `--llm cassette` run has no cassettes and fails closed with `cassette_miss`;
  the W6 evidence report is a `RECORD-ONLY / TEST-FIXTURE` render from the
  committed `TestPostgresE2EReportEndToEnd` fixture, not a byte-deterministic
  dev replay and not a gate proof. Committing eval-run cassettes or a pinned
  dev world remains Q-53/Q-68/M6-05.
- **T0 denominators and zero-denominator error (A3).** Auto-resolve precision
  uses T0 auto-approved cases as its denominator (apply failures count against
  precision); `t0_false_fix` uses T0 auto-approved **and applied** cases. An
  empty T0 applied denominator is gate `status: error`, exit 1 (Q-68), never a
  `0.0` pass. Agent-suite runs remain A4 `not_applicable` with a warning.
- **Additive `eval-report/v1` e2e block (A2).** `report.e2e`
  (`eval-e2e-report/v1`) is optional and omitted for agent runs; the existing
  `headline.auto_resolve_precision` stays the deferred v1 field. Agent-suite
  goldens are unchanged; e2e consumers read `report.e2e.*`. Malformed detail,
  an unknown schema, or `tier_actual != e2e.policy_tier` fails `Build` closed.
- **Synthetic approver identity/source (A6).** The seeded row is fixed UUID
  `00000000-0000-4000-8000-00000000e2e1`, role `approver`, active, no Slack or
  ticket ids; it is seeded only in eval sandbox clones through the admin pool.
  `Service.Decide` is called with source `e2e`, so `approvals.channel = "e2e"`
  and the audit actor is `human:<uuid>`; four-eyes still rejects an
  agent/co-maker id. A non-human audit actor would need a schema change and is
  out of scope.
- **Verifier false-pass wiring (A7).** `verifier.passed` is the observable
  `pattern_verified IS NOT NULL` signal; false passes are `passed &&
  false_fix`. Failed verifier outcomes have no verified pattern and use the
  reserved `unverified` bucket so `VerifierSummary.Total == sum(ByPattern)`
  holds. `false_negatives` is fixed at 0 in M4-14; the metric is reported, not
  gated.
- **`tier_actual` semantic scope.** For e2e rows `tier_actual` stores the
  policy tier; the agent-claimed `proposals.risk_tier` is only
  `detail.e2e.agent_claimed_tier`. Historical agent rows keep the old
  agent-claimed meaning, so report/gate consumers must not assume one meaning
  across suites. `tier_correctness` is reported with a Wilson interval and
  `min_sample`; it is not gated in M4-14 (A10).
- **E2e repeat aggregation is unsupported.** Rates are per standalone run;
  `--e2e` with `--repeat > 1` is a usage error (exit 2), and `report.Build`
  fails closed on an e2e repeat group. Repeat means/intervals remain an M6-05
  follow-up.
- **Tie-out seeding and zero.** A zero tie-out on an empty ledger is
  meaningless. `TestMilestone4Gate` seeds a statement balance (500 USD) and
  asserts non-empty ledger/external open items, a real +500 clearing effect,
  post-apply `Unexplained == 0`, and a ±1 statement perturbation control. The
  plan's "pre-apply non-zero" expectation is mathematically incompatible with a
  post-apply zero for one balanced adjustment (both ledger balance and
  ledger-only rise by the same amount); the non-trivial inputs and the
  perturbation control are the accepted evidence (W5.3).
- **No live Slack or network.** `FakeChannel` records the request and is proven
  not to decide; the decision comes from `approver.Act` →
  `approval.Service.Decide` → `executor.Apply`. The outbox rows stay
  undelivered (`attempts = 0`) and no Slack token, adapter, httptest server, or
  worker runs in the new tests. A real-workspace smoke remains M4-12/AC-15
  (human-gated) and is not claimed here.
- **M4-14 boundaries.** No production synthetic-approver seeding, demo path,
  scheduler, OTel, or new migration; `docs/spec.md` is untouched. The e2e
  pipeline is eval-only, the approver is truth-blind, and the sole report/gate
  schema change is the additive e2e block. `docs/eval-scoring.md` and
  `docs/eval-report.md` are updated for the delivered metrics and gate
  behavior.

**Q-98 final adversary addendum (docs phase; behavior frozen; all items
non-blocking).** The adversary verified fail-closed persistence (failed e2e
cases persist `failed=true` and keep a resumed run failed) with no finding. The
nine findings below are recorded as boundaries, owners, and follow-ups; none
changes a gated metric.

- **Link/duplicate amount is the cited-record sum, not the case delta
  (conformant; residual only).** The M4-14 spec §4.1.3 predicate 2 requires
  `p.AmountMinor` to equal the exact sum/amount implied by the cited records
  for `link`/`duplicate`; only ledger-affecting types compare to
  `c.DeltaMinor`. The e2e read model therefore permits a partial resolution
  (for example a 100-unit link for a 500-unit break) and the approver can
  approve it. The executor independently revalidates the proposal against the
  current case/break/evidence projection and stored state hash before applying,
  so it cannot overpay beyond the cited records; the accepted amount can still
  be smaller than the observable discrepancy. **Owner:** future spec/AC note on
  partial link/duplicate resolutions, or an e2e diagnostic when the proposal
  amount differs from the case delta.
- **Counterparty predicate is tautological in the real loader (coverage gap).**
  `LoadCaseFacts` and `LoadProposalFacts` both project `Counterparty` from the
  same `reconciliation_scopes.source` column, and cited
  `ExternalRecord.Counterparty` comes from the same source, so no end-to-end
  fixture can make the values differ while both loaders succeed. The predicate
  is exercised only with hand-built facts; it remains correct there and still
  guards normalization/emptiness, so this is a coverage gap, not a runtime
  defect. **Owner:** a falsifiable observable counterparty (for example the
  cited external record's counterparty) plus an e2e reject test.
- **Truth-blind unit test is weak; no runtime `eval_truth` dependency.** In
  `TestTruthOnlyMismatchDoesNotInfluenceDecision`, the differing `hiddenTruth`
  value is a local variable never passed to `Act`, so the assertion is a
  tautology and would not catch an ambient truth read. The boundary is instead
  proven statically: `TestNoTruthImportsOrAccess` and
  `eval/e2e/boundary_test.go` scan imports and SQL text, and a `go list -deps`
  probe found no dependency on `eval/dataset`, `eval/score`, `eval/truth`,
  `eval/runner`, `eval/gate`, or `eval/report`, with no `eval_truth` SQL.
  **Owner:** optional truth-reader seam in a test-only harness, asserted never
  consulted.
- **T0 gate accepts a self-inconsistent rate through public
  `gate.Evaluate(Input)` (defense-in-depth).** `validateT0Evidence` checks the
  schema, `denominator == t0_applied`, numerator range, defined/undefined
  agreement, and value range, but it does not recompute
  `value == numerator/denominator`. A hand-built block with
  `t0_applied=10`, `10/10`, and value `0.0` returns `pass`. Production
  `report.Build` recomputes the rate from persisted detail, so the CLI cannot
  construct this block; only a direct caller of the public seam can. **Owner:**
  add an exact/epsilon arithmetic consistency check in `validateT0Evidence`.
- **Tie-out can be skipped on a case-facts load error; statement lookup errors
  are swallowed (diagnostic only).** The runner uses
  `if caseFacts, err := LoadCaseFacts(...); err == nil`; on error `ScopeID`
  and `AsOf` stay zero, `computeTieout` returns nil, and a successful case can
  carry no `tieout` block. Separately, `statementAsOf` converts any statement
  query error (including no row) to the zero time; with a non-empty `ScopeID`
  and zero `AsOf`, `computeTieout` returns "tie-out requires both a scope id
  and an as-of time" and fails the case closed, contrary to the "missing
  statement skips tie-out" comment. T0 false-fix/precision and
  `TestMilestone4Gate` compute their tie-out independently, so the gated
  evidence is unaffected. **Owner:** return the loader/statement error or
  record an explicit "tie-out skipped" reason.
- **Synthetic approver seed accepts a wrong pre-existing fixed-UUID row.**
  `EnsureSyntheticApprover` uses `INSERT ... ON CONFLICT (id) DO NOTHING`, so a
  clone already holding the fixed UUID
  `00000000-0000-4000-8000-00000000e2e1` with the wrong role or `active=false`
  reports success without inspecting the row. `Service.Decide` fails closed
  (`ErrApproverRole` is checked first, then `ErrApproverInactive`), so there is
  no authorization bypass; the failure appears later as a confusing eval error.
  **Owner:** validate the existing row after the conflict no-op, or use a
  guarded `DO UPDATE`.
- **Claimed-tier diagnostic is usually NULL or policy-derived.**
  `agent_claimed_tier` is read from `proposals.risk_tier` **before**
  `approval.Request`; policy writes that column only during Request
  (`PersistDecision`), so it is normally NULL at the read and
  `tier_claimed_correctness` is often undefined. When the proposal is still
  `proposed`, the pipeline backfills a nil claim from `state.PolicyTier`, so a
  non-null value can be a policy-tier copy rather than an independent agent
  claim. The real tier metric, `tier_correctness`, uses `tier_actual` (the
  policy tier) and is unaffected; it is reported, not gated, in M4-14.
  **Owner:** persist an explicit agent claim at proposal creation, or document
  the diagnostic as a policy-tier fallback.
  `docs/eval-scoring.md` §7.1 and `docs/eval-report.md` now state this.
- **T0 path calls `Approver.Act` (`skip_t0`); only `Decide` is skipped.** The
  e2e router calls `Approver.Act` for T0, which returns `ActionSkipT0`; the
  `Decide` count is 0 and no decision, audit row, or state change is produced,
  and the skip is idempotent. The run spec's "the simulated approver is never
  called for T0" and AC-2 "no approver call" are therefore literal-wording
  nuances: true for `Decide`, false for `Act`. **Owner:** route T0 directly to
  the executor, or tighten the wording to "no `Decide` call".
- **Roadmap done-when evidence boundary.** The M4-14 tick is authorized under
  A5/A8, but the literal "e2e eval run on `dev` from cassettes produces a
  report" leg is not delivered: no eval cassettes are committed, the attempted
  dev cassette run failed closed with `cassette_miss`, and the stored report is
  a record-only/test-fixture render from `TestPostgresE2EReportEndToEnd`. The
  FakeChannel gate test and the CI integration target are delivered. A real dev
  cassette replay remains Q-53/Q-68/M6-05. The roadmap M4-14 section now carries
  the evidence note.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-98 | **RESOLVED (M4-14), with the recorded boundaries in the addendum above:** the simulated approver, `recon-eval run --e2e`, additive e2e scoring/report block, applicable `t0_false_fix` gate, and `TestMilestone4Gate` are delivered and covered by unit/integration tests. Recorded residuals: (1) dev evidence is record-only/test-fixture; no eval cassettes are committed and no byte-deterministic dev replay is claimed (Q-53/Q-68); (2) T0 precision and false-fix use asymmetric denominators, and a zero T0 applied denominator is gate `error`/exit 1, never a pass (Q-68/A3); (3) `eval-report/v1` gains only an optional `e2e` block, agent goldens are unchanged, and malformed detail or tier disagreement fails closed (A2); (4) the synthetic approver uses fixed UUID `00000000-0000-4000-8000-00000000e2e1`, source `e2e`, audit actor `human:<uuid>`, seeded only in eval clones (A6); (5) verifier false-pass is `passed && false_fix`, failed outcomes use the `unverified` bucket, and false negatives stay 0 (A7); (6) `tier_actual` is the policy tier for e2e rows and the claimed tier is diagnostic (A10); (7) e2e repeat aggregation is unsupported and rejected; (8) tie-out evidence seeds a statement balance and asserts non-empty open items plus a perturbation control (W5.3); (9) no live Slack/network is used and outbox delivery remains intentionally undelivered; (10) M4-14 adds no migration, production seeding, scheduler, OTel, spec edit, or non-additive report change. | M4-15 / M6-05 / M6-06 / M4-12 AC-15 / eval consumers / ops | Keep the record-only evidence boundary and the M2 fixture as the replay proof; do not treat a zero T0 denominator as a pass; keep the e2e block additive and fail-closed; use the fixed synthetic approver and `e2e` source; keep verifier negatives at 0 until the M4-15/M6-05 definition; read `tier_actual` by suite; reject e2e repeats; keep statement-seeded tie-out evidence; keep the decision path off `FakeChannel` and the outbox undelivered in tests; leave `docs/spec.md` untouched and add no migration for M4-14. |

## M5-01 OpenTelemetry tracing (Q-99)

**Q-99 addendum (Wave 5 docs phase; developer-owned, lead-approved; no
`docs/spec.md` edit).** `docs/spec.md` §3.1 does not list
`proposals.trace_id`, but spec §12 and the M5-01 roadmap row require a trace ID
on proposals. M5-01 adds the nullable column and records the delta here; the
specification text itself is unchanged. The boundaries below are the delivered
M5-01 contract. M5-02+ (replay, metrics, reports, retention) is out of scope.

- **`proposals.trace_id` schema delta (A4).** The forward-only migration
  `20261122120000_m5_01_proposals_trace_id.sql` adds `trace_id TEXT NULL` with
  no backfill, no index, and no length check; Down drops it. No new grant is
  needed because `recon_agent_ro` has table-level `INSERT` on `proposals` and
  `recon_app` has table-level `SELECT`; the real-role grant tests pin that.
  `agent_runs.trace_id` and `audit_log.trace_id` already existed. Go writes
  only 32-lowercase-hex IDs (`telemetry.ValidTraceID`); anything else is NULL.
- **Trace-ID correlation, not full `traceparent` (A1).** `case.run`,
  `llm.call`, and `tool.call` form one trace in the agent process. Async
  `policy.evaluate` and `executor.apply` may run in another process/request and
  then start a **separate root trace**; the persisted proposal trace ID is
  attached as `recon.origin.trace_id` when it differs. M5-01 persists no W3C
  `traceparent` and fabricates no parent/link from a bare trace ID. Records are
  joined by `agent_runs.trace_id → proposals.trace_id → audit_log.trace_id`.
  In-process approval/executor calls naturally nest under the active
  `case.run` and omit the origin attribute.
- **Phoenix 22 port mapping and auth.** The compose service pins
  `arizephoenix/phoenix:22.0.1` (never `latest`). Phoenix 22 serves the UI/REST/
  GraphQL on 6006 and OTLP/HTTP on that same port; the image exposes no 4318
  listener. The host mapping `4318:6006` preserves the spec's
  `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` contract; `4317:4317` is
  OTLP/gRPC. `PHOENIX_ENABLE_AUTH=false` is **dev-only**: the image sets a
  non-loopback `PHOENIX_HOST`, which otherwise enables auth and rejects OTLP
  with 401. The ports must not be exposed on an untrusted network.
- **UI visibility is PENDING (human).** Level 1 is the in-memory span/attribute
  tests; level 2 is the OTLP export plus the Phoenix REST/GraphQL receipt
  query; level 3 is a human opening <http://localhost:6006> and confirming the
  tree. M5-01 records only level 2. Level 3 is `PENDING (manual UI check)`, so
  the roadmap M5-01 box remains unticked until a human records the check.
- **OTLP/HTTP default, gRPC by env (A2).** `http/protobuf` on the OTLP/HTTP
  endpoint is the default (`OTEL_EXPORTER_OTLP_PROTOCOL`, default
  `http/protobuf`; a base endpoint gets `/v1/traces` appended). gRPC
  (`OTEL_EXPORTER_OTLP_PROTOCOL=grpc`) targets `host:port` on 4317 and is
  plaintext only for localhost/dev. No collector is required; a
  traces-specific endpoint/protocol/headers win over the base values.
- **Payload capture is off by default (A5).** With
  `RECON_OTEL_CAPTURE_PAYLOADS` unset/false, spans carry only hashes, counts,
  IDs, codes, and integer micro-USD cost. With capture on, only `tool.call`
  attaches `recon.payload` via `SafePayload`; `llm.call` still attaches hashes
  only (the spec permits an optional response snippet, but M5-01 does not
  attach one). Prompts, memos, raw tool arguments, tool envelopes outside the
  opt-in, authorization headers, and session IDs are never attached.
  `SafePayload` redacts before truncating at 2 KiB and never captures business
  content as safe.
- **No inbound server span (A6).** M5-01 adds no `otelhttp` middleware, so an
  API request's `X-Trace-ID` header remains the HTTP correlation ID and can
  differ from an OTel trace ID started inside that request (for example by
  `policy.evaluate`). The response header keeps the HTTP ID; do not claim the
  two IDs are the same. Inbound propagation is a separate approved change.
- **Process wiring and the unwired outbox.** `cmd/api`, `cmd/worker`, and
  `cmd/recon agent run` initialize the provider from env, pass it to the
  approval service / agent runner, stop work first, and flush last with a fresh
  bounded context (default 5s). A flush failure is logged, not turned into a
  command failure; an invalid telemetry configuration fails startup.
  `cmd/outbox` creates no M5-01 spans and remains documented as unwired. No
  production executor binary exists today; `executor.apply` is exercised
  in-process/tests.
- **Global-provider isolation.** No binary or package calls
  `otel.SetTracerProvider`. Providers are injected explicitly; a nil or
  disabled provider returns the OTel no-op tracer, so callers never branch on
  tracing state and parallel tests stay isolated. SDK types stay inside
  `internal/telemetry`; only `trace.Tracer` escapes.
- **Dependency pin.** OTel modules are pinned to stable **v1.47.0**
  (`otel`, `sdk`, `otlptracehttp`, `otlptracegrpc`, `trace`) on Go 1.27.3. No
  `otelhttp`/contrib config or collector is added.
- **Demo evidence boundary.** The deterministic scripted path is
  `scripts/phoenix-demo.sh` → `TestPhoenixDemoReceipt`: it runs the real
  `recon agent run` command against the M2 scripted fixture on an ephemeral
  database, asserts `agent_runs.trace_id == proposals.trace_id`, and queries
  Phoenix REST `GET /v1/projects/default/traces?limit=100&include_spans=true`.
  The final verified tree (`73d42c7`) recorded level-2 receipts
  `trace_id=54679d053e5564dec2b97f401ba7ac5e` (QA cold run) and
  `04261564286eebd63c58c28112e54b52` (tester), each with **15 spans**
  (`case.run` root plus 5 `llm.call`/9 `tool.call` children) and
  `service.name=recon-agent`; the earlier W4 demo recorded
  `8be7a7fb82e767c73d3fe8f85a347a52`. Trace IDs are random per run. That is
  level 2, not UI evidence; the level-3 check stays pending. `make demo` remains
  the M5-12 stub and is not cited.
- **No M5-02+ scope.** M5-01 adds no replay, Prometheus metrics, dashboards,
  reports, retention/redaction job, scheduler, or spec edit. The only migration
  is the `proposals.trace_id` change above.
- **Residual follow-ups (non-blocking; recorded at the final review).** None of
  these changes the frozen behavior or the evidence boundary above.
  - **Under-limit invalid UTF-8 (low).** `SafePayload` / `RedactAndTruncate`
    sanitize UTF-8 only on the truncation path, so an under-limit input that
    already contains invalid bytes can still return invalid UTF-8. Every
    current production call site passes JSON or error strings, so no end-to-end
    leak is known. Follow-up: sanitize every return path and add an under-limit
    invalid-byte regression test. **Target:** M5-01 follow-up; nearest roadmap
    owner M6-06 (retention/redaction).
  - **Remote OTel span context accepted (latent hardening).**
    `telemetry.SpanTraceID` accepts any valid `trace.SpanContext`, including a
    remote one. No production path extracts an inbound `traceparent` into a
    span context today, so an HTTP client cannot inject a persisted trace ID;
    this is a latent trust-boundary note, not a live bypass. If inbound
    propagation is added, reject remote span contexts for persistence or
    document the trust boundary explicitly. **Target:** M6-03 (threat model) /
    M5-01 follow-up.
  - **Evidence-process notes (info).** The tester report's competing-process
    claim was superseded by QA's clean solo cold `go clean -testcache && make
    verify` at the same HEAD/tree; the release evidence is QA's run. The
    planned `<artifact_dir>/demo-evidence.md` is absent; level-2 evidence lives
    in `impl-notes.md` and the live receipt output (`PHOENIX_LEVEL2 ...
    spans=15`). No behavior defect. **Target:** run process / docs evidence
    checklist; no product change.
  - **Re-confirmed fixes (delivered).** The final review re-verified the raw
    model tool-name leak fix, the forgeable legacy trace-ID fix (persistence is
    `SpanTraceID`-only), Phoenix loopback binding with the pinned image and
    dev-only `PHOENIX_ENABLE_AUTH=false`, the strengthened 15-span receipt (one
    `case.run` root, 5 `llm.call`, 9 `tool.call`), and unified cached-token
    accounting. **Target:** none.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-99 | **RESOLVED (M5-01) for the implementation; spec §3.1 amendment deferred to a future spec revision:** `proposals.trace_id` is a roadmap-required schema delta recorded in the addendum above (nullable, no backfill, no index, no new grant). M5-01 also records the frozen boundaries: trace-ID correlation only for async policy/executor (no `traceparent`/links); Phoenix 22 `4318:6006` mapping with dev-only `PHOENIX_ENABLE_AUTH=false`; level-3 UI visibility PENDING a human check (the roadmap box stays unticked); OTLP/HTTP default with gRPC via env; payload capture off by default and never prompts/memos/args; no inbound server span so `X-Trace-ID` may differ; `cmd/outbox` unwired; no global provider mutation; OTel v1.47.0 pinned; level-2 demo receipt recorded with 15 spans and no UI claim; no M5-02+ scope. Residual follow-ups (under-limit UTF-8, remote span-context hardening, evidence-process notes) are listed in the addendum above. | Spec §3.1 / M5-01 / docs consumers / ops | Amend spec §3.1 for `proposals.trace_id` only in a future spec revision; keep the correlation-only, capture-off, dev-only-Phoenix, and no-global-provider contracts; keep the M5-01 box unticked until a human records the Phoenix UI check; do not cite level 2 as level 3; close the two technical residuals before relying on payload capture or inbound trace propagation. |

## M5-02 Replay (Q-100)

**Q-100 addendum (Wave 5 docs phase; developer-owned, lead-approved; no
`docs/spec.md` edit).** `docs/spec.md` §§12 and 19 require replay; the delivered
M5-02 contract and its boundaries are recorded here. The specification text
itself is unchanged. M5-03+ (metrics, dashboards, reports, retention) is out of
scope.

**Post-review addendum (QA PASS and adversary re-review at `5a88f1c`, 0
blockers; docs-only correction).** The review required no behavior change for
M5-02. Recorded here are F1 (JSONB numeric normalization can produce a false
`request_hash_mismatch` in exact replay; fail-closed) and F3 ("spec §5" means
the M5-02 run spec §5, not `docs/spec.md` §5, which is the matching engine).
The trust boundary is restated with the stored-proposal case. `docs/spec.md`
remains unchanged.

- **Read-only diagnostic, one load then no DB.** `recon replay` is a diagnostic,
  not a restore or re-apply path. The CLI opens the application role, loads the
  run in exactly one `REPEATABLE READ READ ONLY` transaction, commits it, and
  closes the pool. `replay.ExactReplay(recording, config)` and
  `replay.Rerun(...)` take no pool, transaction, DB-carrying context, or
  executor; a closed-pool test proves replay still succeeds after the load.
  Replay writes no rows, executes no recorded tool, and never touches live data.
- **Exact is limited to `outcome='proposal'` with a stored proposal.** Any other
  outcome, or a proposal outcome with no stored proposal row, is exit 2
  `unsupported_run`; replay never fabricates an identity or placeholder proposal
  to manufacture a match.
- **Digest field set.** `proposal_hash` is SHA-256 over canonical JSON of
  `{type, payload, rationale, evidence (sorted unique), confidence (4dp string),
  pattern_claimed}`, with JSON `null` and `""` distinct. `state_hash`,
  `idempotency_key`, timestamps, and stored IDs are excluded. The digest is
  versioned by this record, not by a schema column; changing a normalization
  rule changes all historical digests.
- **`state_hash` is diagnostic only.** Exact replay reports the recorded
  `proposals.state_hash` (and flags a malformed value with `state_hash_invalid`)
  but never compares it for identity. A run whose `state_hash` would differ is
  still identical; replay must not read post-recording DB state.
- **Idempotency key.** Exact replay recomputes the key and reports a key-only
  difference as `proposal_field_diff(field=idempotency_key)`. That single
  difference is non-identity-breaking (`identical` stays true), but it is still
  a divergence and the CLI exits 1. Rerun compares the digest only and never
  requires key equality, because a rerun produces a new proposal.
- **Tool-call arguments are part of exact identity.** For the seed `get_case`
  call, every investigation call, and the proposal call, exact replay
  canonicalizes the model-requested arguments and compares them with the stored
  `tool_calls.args` (sorted keys, `UseNumber`, `null` distinct from `""`; empty
  arguments normalized to `{}` exactly as the recorder stores them). A mismatch
  is a `tool_result_mismatch` hard stop with `field=args` and `identical=false`.
  A recorded response cannot preserve empty-vs-literal-`null` arguments (both
  serialize as `null`); that malformed-call-only ambiguity accepts either stored
  spelling. A proposal tool call is terminal only when the recorded call
  succeeded, so an invalid-arguments proposal followed by a repair and a valid
  proposal replays identically.
- **Integrity hashes are not authentication (non-blocking trust boundary).**
  `result_canonical_hash` (and the args comparison) is stored beside the data
  it protects. A writer who can consistently rewrite both the data and its hash
  — or `tool_calls.args`, the arguments inside `llm_calls.response`, and the
  stored `proposals` row together — can make exact replay report identical.
  Replay detects inconsistent corruption
  and partial edits, not an authorized consistent rewrite. Hashes stored beside
  their data are not authentication. M5-02 adds no
  signature, MAC, or external anchor; operators must rely on database
  privileges, audit, and retention controls.
- **Canonical tool-result hash migration and legacy rows.** The only M5-02
  migration is `20261123120000_m5_02_tool_result_canonical_hash.sql`:
  `tool_calls.result_canonical_hash TEXT NULL`, no backfill, no index, no new
  grant (the existing table-level `recon_app` SELECT/INSERT covers it).
  `result_hash` hashes the original Go envelope bytes and cannot be recomputed
  from JSONB; `RecordToolCall` now also stores SHA-256 over canonical JSON of
  the loaded result. A non-NULL mismatch is `tool_result_mismatch` and a hard
  stop. A NULL legacy row reports the non-breaking
  `tool_result_hash_unavailable` diagnostic; arguments/content checks still
  apply.
- **20-run gate and committed fixture.** The done-when evidence is the seeded
  integration test `TestTwentyRunExactReplayGate`: a fresh test DB, the M2
  fixture, 20 distinct cases over three scenarios split 7/7/6, the production
  agent loop recording through the real recorder, then `LoadRecording` +
  `ExactReplay`; it requires 20/20 identical. The committed
  `internal/replay/testdata/recording_v1.json` (schema 1, run
  `77777777-7777-4777-8777-777777777777`, prompt v1, outcome proposal) is the
  small engine/CLI fixture; its loader is test-only and no production package
  imports it. The scenarios/gate are test-only and need a test database.
- **`llm_calls` limitation: replay is code/config-coupled.** `llm_calls` stores
  `request_hash`, the response, and usage, but not the full request body,
  provider, or base URL. Exact replay must rebuild each request with the current
  code, prompt, model choice (from each recorded `response.requested_model`),
  temperature/token config, provider, and base URL. A change to any of those, or
  to request-building code, can make an old run unreplayable and surfaces as
  `request_hash_mismatch`. That is a faithful mismatch, not noise; operators
  must check config/code drift before treating it as a decision change.
- **JSONB numeric normalization can false-positive `request_hash_mismatch`
  (F1; fail-closed).** `llm_calls.response` and `tool_calls.args` are JSONB;
  PostgreSQL normalizes numeric literals (`1e3` becomes `1000`, and
  non-canonical exponent or `-0` spellings are normalized too).
  `llm_calls.request_hash` was hashed at record time from the in-memory
  transcript containing the original literal, while exact replay recomputes it
  from the normalized JSONB text. A recording whose non-final model response
  used such a literal can therefore report one `request_hash_mismatch` at the
  next step and exit 1 even though production ran cleanly. Replay stays
  read-only and fail-closed (no silent identity, no write, no unrecorded tool
  execution); the input class is outside the 20/20 gate and the committed
  fixture, and the recorder stores no raw response text, so the literal is
  unrecoverable. Fix path: a future schema revision that stores the raw
  assistant response text (or a pre-JSONB literal form) and hashes from that, or
  versioned record-time numeric canonicalization. Until then keep the
  fail-closed behavior; a future fix should add a regression test that
  deliberately flips today's behavior, and this divergence class should be
  treated as storage normalization, not a decision change. **Target:**
  future replay schema/hardening task (not M5-02); docs consumers and operators
  until then.
- **Cassette-default rerun, explicit live.** Rerun defaults to reading
  `eval/cassettes` and never touches the network; a miss is exit 2
  `cassette_miss`, never a divergence. `--live` is the only provider path and
  requires `OPENCODE_API_KEY` in the process environment; a config file cannot
  supply it (`yaml:"-"`), and `.env` is never read. Tests/CI never use `--live`.
  Missing key is exit 2 `live_key_missing`. `--model` and `--prompt` override
  the recorded values; `--cassette-dir` is a delivered extension that selects a
  different cassette directory (default `eval/cassettes`).
- **Rerun stops at the proposal.** The first proposal tool call is terminal for
  rerun: it rebuilds the proposal through the production normalization path,
  compares the digest, and stops. A model-requested tool call with no unconsumed
  matching recording is the `tool_call_unrecorded` hard stop and is never
  executed. Rerun has no executor and writes no DB row.
- **Exit 0/1/2.** `0` no divergences (and equal digests in exact mode); `1` the
  divergence list is non-empty, with the full report still on stdout —
  including a key-only `proposal_field_diff` that keeps `identical=true`; `2`
  usage/config/load/DB/`unsupported_run`/`cassette_miss`/`live_key_missing`
  error, stderr only even with `--json` (no partial JSON on stdout). Exit status
  follows the divergence list in the M5-02 run spec §5 ("Divergence taxonomy,
  JSON, and exit codes"); `docs/spec.md` §5 is the matching engine, not this
  taxonomy. Divergence is a completed replay that differs, not an operational
  failure.
- **Telemetry disabled.** The replay path never initializes the OTel provider
  and exports no spans; a counting OTLP endpoint test asserts zero hits. Replay
  is a diagnostic command, not a traced case run.
- **No M5-03+ scope and no `docs/spec.md` edit.** M5-02 adds no metrics,
  dashboards, reports, retention job, scheduler, or other roadmap task. The
  specification text is unchanged; this Q-100 entry and `docs/replay.md` are the
  delivered documentation.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-100 | **RESOLVED (M5-02) for the implementation; post-review residuals F1 (fail-closed JSONB numeric normalization; needs a future schema/hashing revision) and F3 (run-spec §5 citation) recorded in the addendum above; spec §12/§19 wording left unchanged:** replay is a read-only diagnostic limited to `outcome='proposal'` with a stored proposal; one `REPEATABLE READ READ ONLY` load then no DB; digest covers `{type,payload,rationale,evidence(sorted unique),confidence(4dp),pattern_claimed}` and excludes `state_hash`/`idempotency_key`/timestamps; `state_hash` is diagnostic only; exact recomputes the key and reports a key-only diff without changing `identical` (but the command still exits 1); exact compares every model-requested tool-call argument set with stored `tool_calls.args` (`tool_result_mismatch(field=args)` hard stop) and mirrors the malformed-proposal repair flow; rerun compares digest only, stops at the proposal, and is cassette-default with process-env-only `OPENCODE_API_KEY` for `--live`; exit follows the divergence list (0 none, 1 any including a key-only diff, 2 usage/load) with stderr-only exit 2; the canonical tool-result hash migration is the only schema change and legacy NULL rows get the `tool_result_hash_unavailable` diagnostic; hashes stored beside the data are not authentication (a consistent rewrite passes — non-blocking trust boundary, no signature/MAC added); the 20/20 seeded gate and `recording_v1.json` fixture are the evidence; `llm_calls` lacks the full request/provider/base URL, so replay is code/config-coupled and config drift shows as `request_hash_mismatch`; telemetry is disabled; no M5-03+ scope and no `docs/spec.md` edit. | Spec §12/§19 / M5-02 / docs consumers / ops | Keep replay read-only and exact-restricted; keep the digest field set, `state_hash` diagnostic, and key-only relaxation; keep cassette-default rerun with process-env `--live`; keep exit 0/1/2 and stderr-only exit 2; keep the canonical hash migration and legacy diagnostic; cite only the 20/20 gate and committed fixture as replay evidence; treat config drift as a real `request_hash_mismatch`; keep telemetry off; amend `docs/spec.md` only in a future spec revision if the owners choose to. |

## M5-03 Prometheus metrics (Q-101)

**Q-101 addendum (Wave 5 docs phase; developer-owned; no `docs/spec.md`
edit).** `docs/spec.md` §12 names the metrics requirement and §13.1 depends on
the metric names for dashboards; the delivered M5-03 contract and its
boundaries are recorded here. The specification text itself is unchanged.
M5-04 (Prometheus server/Grafana provisioning and dashboards), M5-05 (agent
quality dashboard), and M5-06 (daily integrity job) are out of scope; the
frozen names/types/labels are the handoff contract for M5-04, and the operator
reference is `docs/metrics.md`.

- **Custom per-process registry, no global.** Each long-running process
  (`cmd/api`, `cmd/worker`, `cmd/outbox`) constructs its own
  `prometheus.NewRegistry()` through `metrics.New`; production wiring never uses
  `prometheus.DefaultRegisterer`/`DefaultGatherer` or `promauto`. A nil
  `api.Options.Metrics` builds a fresh isolated registry for handler tests, and
  the production API passes the same process registry to its matcher, case,
  approval, limits, and error instrumentation. Counters are process-local and
  reset on restart; dashboards must use PromQL `rate`/`increase`, not raw
  totals. Several processes/replicas each expose their own registry.
- **Endpoints and exposure.** The API keeps its existing public,
  unauthenticated `GET /metrics` and serves Prometheus text with the frozen
  prefix `text/plain; version=0.0.4; charset=utf-8` from its own HTTP server.
  The pinned `client_golang`/expfmt encoder appends `; escaping=underscores`, so
  the live header is `text/plain; version=0.0.4; charset=utf-8;
  escaping=underscores`; the prefix is the contract, not the whole header. The
  worker and outbox serve only `GET /metrics` on separate small listeners,
  loopback by default (`METRICS_WORKER_ADDR=127.0.0.1:9091`,
  `METRICS_OUTBOX_ADDR=127.0.0.1:9092`), started before the poll loop and
  drained with the signal context. `--once` modes open no listener, a bind
  failure aborts startup with the config key and never a DSN, and a
  non-loopback bind is allowed only explicitly and logs a warning because the
  listener has no authentication (network policy is the control). The API stays
  public as it is today; business counts could leak on an unauthenticated port,
  so operators must not expose it beyond the trusted network until M5-04
  revisits auth/network policy. Each API scrape runs the four DB aggregates
  described below with no `MaxRequestsInFlight`, so concurrent unauthenticated
  scrapes multiply database work; M5-04 owns auth/rate limiting/pool sizing.
- **Frozen dependency and exemptions.** `github.com/prometheus/client_golang`
  is pinned at `v1.25.0`; `go mod tidy` adds it and its required modules, with
  no unrelated upgrades. `github.com/prometheus/client_model v0.6.3` is a
  **direct** requirement because `internal/metrics/server.go` names
  `[]*dto.MetricFamily`; the stale `guard.go` comment calling it indirect is a
  source-only cleanup (A9 in the addendum below). Long-running binaries
  explicitly register client_golang's Go and process collectors. `go_*`,
  `process_*`, and `promhttp_*` families are exempt from the business
  cardinality guard; the process collector's exact families depend on the host
  OS.
- **Bounded labels and forbidden IDs.** The catalog in
  `internal/metrics/catalog.go` is the single source of truth for name, type,
  help, label order, allow-lists, histogram buckets, and series caps.
  `source`, `currency`, and `model` use charset/length validators; the remaining
  labels are closed enums. For open-ended labels, every unrecognized value
  becomes the literal `unknown`; empty values, canonical UUID/ULID/long-hex
  shapes, and URLs are rejected by normalization. `queue` and the build-info
  `component` are closed enums with no `unknown` child. Forbidden label names
  (`case_id`, `run_id`, `agent_run_id`, `proposal_id`, `break_id`, `scope_id`,
  `ledger_txn_id`, `approver_id`, `trace_id`, `idempotency_key`, `entity_id`,
  `message_id`, `external_ref`, `payload`, `error`, `reason`, `rationale`,
  `dsn`, `url`, `path`, `email`) are rejected at registration/validation. The
  guard asserts ≤64 business series on an idle process and ≤128 under
  representative instrumentation; these are **test-time** regression
  assertions (`ValidateRegistry`), not runtime admission control, and a
  per-entity label regression fails the metrics tests.
- **Event-driven versus DB-derived families.** Match runs/matches/breaks, case
  age, agent runs/cost, approval latency, dead letters, auto-apply cap ratio,
  tie-out, and most errors are observed by event producers after the owning
  transaction commits (no metric runs ahead of committed state). Queue depth
  and outbox lag are scrape-time gauges derived from the shared database
  through each process's `recon_app` pool: they are refreshed on every gather
  under `metrics.gather_timeout` (default 2s), issue read-only aggregates, and
  reuse the existing partial indexes. On query failure the affected gauge keeps
  its last value, the collector logs only component/queue (never SQL, raw error
  text, database topics, or a DSN), and `recon_errors_total{component="metrics",
  code="collect"}` increments once per failed query in that scrape; the scrape
  still returns 200. A gauge frozen by a failed query is stale and must not be
  treated as fresh monitoring evidence.
- **Total gather failure.** A total gather failure with zero families returns
  500 with a fixed body; the OpenAPI `/metrics` responses are 200 and 500. A
  partial collector failure returns 200 with the reachable families and is
  logged (the seeded catalog makes a healthy process's total failure
  unreachable). The handler's error text is never collector output.
- **Tie-out is event-only with no long-lived producer.**
  `tieout.Compute` accepts an optional observer and exposes the scope `source`
  for the label, but no long-lived process calls `Compute` today: the one-shot
  `recon tieout` CLI deliberately passes no observer. API/worker scrapes
  therefore show only the seeded `{currency="unknown",source="unknown"}` series.
  A scrape-time tie-out collector or scheduler is deferred to M5-06/M5-09 (or a
  later task) if still needed; dashboards must not infer a healthy tie-out from
  a seeded zero.
- **Integrity metric registered with no producer.** `recon_integrity_job_status
  {job="integrity"}` is registered and seeded at `0`, documented as
  `0=unknown/not run`, `1=pass`, `2=fail`; M5-06 owns the producer and values.
  No integrity job, table, scheduler, or report is added by M5-03.
- **Outbox topics fold.** The outbox allow-list is only `approval.request`,
  `approval.update`, and `approval.cancel`; any other pending topic folds into
  `unknown` (maximum age for lag, otherwise count). A new topic requires a
  catalog/spec update. Dead letters are authoritative from the worker after the
  committed dead mark; `errors` has no topic label, so delivery failures do not
  expose a topic. `code="dead_letter"` is allow-listed but has no producer: a
  handler failure increments `outbox`/`delivery_failed`, and the terminal
  dead-letter state increments only `recon_outbox_dead_letters_total` (the
  unknown-topic path does not increment `recon_errors_total` at all).
- **Model label validator.** `model` keeps the label with
  `^[A-Za-z0-9._:/-]{1,64}$`; an exotic model ID collapses to `unknown`, losing
  per-model resolution rather than creating a new series. `source` uses
  `^[a-z0-9][a-z0-9_]{0,31}$` and `currency` uses `^[A-Z]{3}$`. The value
  reported is the provider response's actual model, not only the configured
  model; because `MaxSeries` is enforced only by `ValidateRegistry`, a
  provider-controlled model can create new series for the process lifetime.
  This is an accepted Q-101 boundary under A8 (keep the label with a
  validator), not a runtime protection; M5-04 should monitor agent-family
  cardinality and consider a bounded model policy.
- **Approvals-pending plan.** `recon_queue_depth{queue="approvals_pending"}` is
  `count(*) FROM proposals WHERE status='pending_approval'`. On today's small
  `proposals` table the planner chooses a sequential scan; the existing partial
  unique index `proposals_one_active_per_case` can serve the predicate if volume
  makes it attractive. No migration or new index is added in M5-03.
- **Cases error component.** The case-service error observer labels failures
  with the owning process component (`api` or `worker`), because the frozen
  `component` allow-list has no `cases` value. The case-age histogram itself is
  labelled by case `status`. Lifecycle transitions executed inside another
  service's transaction may not have a post-commit hook; this case-age gap is
  not papered over with fake samples, and queue depth remains authoritative for
  backlog.
- **Transport 4xx versus service failures.** The API transport writer
  (`writeError`) counts only 5xx: `component="api"` with `code="unavailable"`
  for 503, `timeout` for 504, and `internal` otherwise; plain client 4xx
  responses are not counted, avoiding noise. Application-service observers do
  count their own bounded 4xx-class failures (`conflict`, `not_found`,
  `invalid_request`, `forbidden`) because spec §12 requires those codes, so
  `recon_errors_total` can move on a request that the HTTP layer answers with a
  4xx. That is service-level instrumentation, not transport 4xx counting.
  Adding new codes requires a documented change to the frozen code allow-list.
- **Seeded series are public output, with three `unknown` exceptions.** Every
  vector is instantiated at construction with at least one child so an idle
  process still exposes every family; open-ended label vectors seed an
  `unknown` child, and the three queue zeros and the integrity zero are part of
  the scrape. Three families deliberately have no `unknown` child:
  `recon_build_info` (`component` is limited to `api`/`worker`/`outbox`),
  `recon_queue_depth` (closed queue set), and `recon_integrity_job_status`
  (`job="integrity"` seeded at `0`, meaning not run). Seeded series make an
  absent producer visible; seeded histograms have zero observations and are not
  fake samples. Consumers must distinguish "seeded/absent" from a real
  measurement.
- **Scope boundary.** M5-03 adds no Prometheus server, scrape configuration,
  Grafana datasource, dashboard JSON, alerting, `deploy/` assets, daily
  integrity job, `integrity_runs` table/scheduler, report, retention, exemplar,
  or tracing change. M5-04 and M5-06 remain unimplemented; only their future
  contracts (frozen metric names and the integrity gauge) are registered.
  `docs/spec.md` is not edited.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-101 | **RESOLVED (M5-03) for the implementation; residuals stay OPEN for M5-04/M5-06:** one custom per-process registry per binary (no default registry, no `promauto`), the API keeps its public `/metrics` on the existing server, and worker/outbox serve loopback-only listeners (`127.0.0.1:9091`/`9092`, env-configurable, no listener in `--once`, bind failure names the config key). `client_golang v1.25.0` is pinned; Go/process/promhttp families are exempt from the business guard. Labels are frozen, allow-listed, and normalized to `unknown`; forbidden ID/free-text label names are rejected and series bounds are asserted. Event-driven families are observed post-commit; queue depth and outbox lag are scrape-time DB aggregates under `gather_timeout` with keep-last-value, bounded logs, and `metrics/collect` on failure, and a total gather failure returns 500. Tie-out remains event-only with no long-lived producer, so its seeded series is not a live value; integrity is seeded `0` with no producer until M5-06; outbox topics fold to `unknown`; `model` keeps its validator; `approvals_pending` may seq-scan the small proposals table; case-service errors carry the owning process component; the API transport writer counts only 5xx while service observers count bounded 4xx-class codes (`conflict`/`not_found`/`invalid_request`/`forbidden`), and seeded vectors carry an `unknown` child except build info, queue depth, and integrity. No dashboards/Prometheus provisioning (M5-04), no integrity job (M5-06), and no `docs/spec.md` edit. | Spec §12/§13.1 / M5-03 / M5-04 consumers / operators | Keep the registry/endpoint/catalog contract frozen for M5-04; keep DB gauges keep-last and treat a frozen gauge as stale; do not infer a live tie-out or a passing integrity job from seeded values; add a real tie-out producer and the M5-06 integrity producer only in their own tasks; revisit public metrics exposure in M5-04; make any metric-name/label/bucket change through a spec/version note first. |

### Final adversary addendum (A1-A9)

Recorded after the parallel review wave. All nine items are non-blocking; each
was checked against the delivered behavior and the documentation, and the doc
fixes landed in `docs/metrics.md` plus the Q-101 bullets above. A7 (validator
hardening) and A9 (stale comment) are source-only follow-ups and were not edited
by the docs pass. No `docs/spec.md` change.

- **A1 — `model` cardinality is provider-influenced; the series cap is
  test-time only.** The agent families use the model ID returned by the
  provider, so a new valid model ID adds a child until the process restarts.
  `ValidateRegistry`/`MaxSeries` run in tests, not at runtime. Accepted boundary
  for M5-03; M5-04 should monitor agent-family cardinality and decide whether a
  bounded model policy is needed.
- **A2 — seeded `unknown` children are not universal.** Open-ended vectors seed
  an `unknown` child, but `recon_build_info` (`component` is `api`/`worker`/
  `outbox`), `recon_queue_depth` (closed queue set), and
  `recon_integrity_job_status` (`job="integrity"` seeded at `0` = not run) have
  no `unknown` child. Documentation no longer claims every labelled vector does.
- **A3 — `recon_errors_total` is not 5xx-only end to end.** The API transport
  writer (`writeError`) counts only 5xx, but service observers count their own
  bounded 4xx-class codes (`conflict`, `not_found`, `invalid_request`,
  `forbidden`) as spec §12 requires. The earlier "4xx responses are not counted"
  wording described the transport writer only and is corrected here, in the
  bullets above, and in `docs/metrics.md`.
- **A4 — the live Content-Type includes `escaping=underscores`.** The pinned
  encoder appends the parameter, so the wire header is `text/plain;
  version=0.0.4; charset=utf-8; escaping=underscores`. The version/charset
  prefix remains the compatibility contract; `docs/metrics.md` and this
  addendum record the full header.
- **A5 — no `errors{code="dead_letter"}` producer exists.** `dead_letter` stays
  in the frozen allow-list but nothing increments it.
  `recon_outbox_dead_letters_total` is authoritative for dead letters, a handler
  failure increments `outbox`/`delivery_failed`, and the terminal dead-letter
  mark increments only the dead-letter counter. Dashboards must not query the
  errors family for dead-letter counts.
- **A6 — public `/metrics` does database work per scrape and has no in-flight
  cap.** Each API scrape runs four unauthenticated DB aggregates (the queue and
  outbox-lag queries) with no `MaxRequestsInFlight`. Concurrent scrapes multiply
  database load, and business counts could leak if the port is exposed. M5-04
  owns auth, rate limiting, and pool sizing; until then keep the port on the
  trusted network.
- **A7 — identifier heuristics have gaps.** Canonical UUIDs, uppercase ULIDs,
  and ≥16-hex values are rejected, but lowercase ULIDs, prefixed UUIDs, and
  15-hex values can pass the label validator. No production flow was found that
  emits such a value; hardening is a source-only follow-up, not a docs change.
- **A8 — some `MaxSeries` caps sit below legitimate allow-listed maxima.** The
  auto-apply ratio family caps 32 currency values although 49 are legitimate (48
  seeded ISO codes plus `unknown`), and the errors family caps 64 pairs although
  140 non-`unknown` component/code pairs (165 including `unknown`) are
  allow-listed. A registry populated with valid representative data can fail
  `Validate()` even though every value is allow-listed. Test-time only; runtime
  is unaffected because the guard is not called in production. Raise or
  normalize the caps in a future catalog change.
- **A9 — `client_model` is a direct dependency; the comment is stale.**
  `internal/metrics/server.go` names `[]*dto.MetricFamily`, so
  `github.com/prometheus/client_model v0.6.3` is a direct `go.mod` requirement.
  The `guard.go` comment calling it indirect is a source-only cleanup; this
  documentation records the direct requirement.

Residual owners: A1 and A6 → M5-04 (agent cardinality monitoring; metrics auth,
rate limiting, pool sizing); A7 and A8 → future catalog/validator hardening (no
task yet; record before changing caps or validators); A9 → next source touch of
`internal/metrics/guard.go`; A2-A5 → documentation-only and closed by this pass.

## M5-04 Ops dashboard (Q-102)

**Q-102 addendum (Wave 5 docs phase; developer-owned; no `docs/spec.md`
edit).** `docs/spec.md` §13.1 names the ops dashboard and the metric names are
frozen by Q-101; the delivered dashboard, its data path, and its boundaries are
recorded here. The specification text itself is unchanged. M5-05 (agent quality
dashboard) and M5-06 (daily integrity job) remain out of scope. The operator
reference is `docs/dashboards.md`.

- **Synthetic data path, not a production pipeline (Q-98 / AQ-1).** The
  deterministic `recon demo ops` command (and `make demo-ops`) is the data path
  for all five Postgres panels. It runs the M1 demo seed and then inserts
  synthetic agent runs, proposals, approvals, applied proposals, cases, and
  break-aging fixtures. It is **synthetic dashboard demo evidence**, not a
  production M2–M4 pipeline run: it bypasses the agent, policy engine, approval
  service, executor, and audit chain, makes no network or LLM call, and writes
  deterministic `demo-ops:`/`demo-ops-synthetic` marker rows. The summary prints
  counts only. No API/worker/outbox service is added to compose. `recon demo
  seed` alone leaves the ops panels empty; a second `recon demo ops` without
  `--reset` fails closed with `demo ops: ops data already present; rerun with
  --reset` before writing any row. This substitutes for the missing production
  pipeline and must not be cited as pipeline, tie-out, or integrity evidence.
- **Read-only role and view-only surface.** `recon_dashboards_ro` is `NOLOGIN`,
  not a member of any application role, and not superuser/createdb/createrole/
  replication. It has `default_transaction_read_only=on` and
  `statement_timeout=30s`; the local login sets both directly because group
  `rolconfig` is not inherited at connection time. Its surface is exactly five
  owner-rights views (`v_ops_match_rate`, `v_ops_unmatched_value`,
  `v_ops_break_aging`, `v_ops_approval_queue`, `v_ops_resolution_mix`) with
  `SELECT` on the views only. `PUBLIC` is revoked; there is no base-table,
  sequence, function, or `v_agent_*` grant, and base-table reads through the
  datasource fail with SQLSTATE `42501`. The migration Down revokes this
  database's privileges and drops the views; its cluster-wide `DROP ROLE` is
  guarded and retains the role with a NOTICE while another database in the same
  cluster still holds a grant. Down is therefore not a cluster cleanup step.
- **Fresh-volume login and existing-volume reset path.** A fresh Postgres volume
  creates the local-only `recon_dev_dashboards_ro` login through
  `compose/initdb/01-roles.sql`; the migration creates the role/views/grants.
  An existing volume does not rerun initdb, so the documented path is the
  idempotent `docker compose exec -T postgres psql ... -f
  /docker-entrypoint-initdb.d/01-roles.sql` rerun followed by `make migrate`,
  or the destructive `docker compose down -v` alternative. There is no claim of
  zero manual steps on existing volumes.
- **Canonical aging rule (AQ-2 / AQ-7).** One row per open/in-case break. The
  reference date is the **minimum resolvable** ledger/external `value_date`;
  `breaks.side` is the break type. Buckets are `0-1`, `2-3`, `4-7`, `>7`, and
  `unknown`; a missing or future-dated minimum is `unknown` (bucket order 5),
  never `0-1`. `age_days` exposes the raw distance and may be negative for a
  future date. Panel 3 orders by `bucket_order`, not lexically.
- **Classification and approval-queue semantics (AQ-6).** One terminal row per
  case with precedence escalated > human > auto; resolved cases use
  `resolved_at`, escalated cases use `created_at` because the schema has no
  escalation timestamp; unqualified cases are excluded. This is a snapshot,
  not history. Approval latency uses the earliest approved decision; any
  rejected decision presents `rejected` and suppresses latency (fail-closed).
  Pending/oldest-pending are query-time snapshots. Panel 4's
  `$__timeFilter(approved_at)` inside `FILTER (...)` was executed through the
  real Grafana 13.3.2 datasource and expanded correctly, so the recorded
  fallback was not needed.
- **Pinned provisioning and security defaults (AQ-3 / AQ-5).**
  `grafana/grafana:13.3.2` and `prom/prometheus:v3.16.0` are pinned; the
  PostgreSQL plugin id is `grafana-postgresql-datasource`. Grafana and
  Prometheus publish on `127.0.0.1` only; anonymous access and sign-up are off;
  reporting/update checks are off; plugin preinstall is disabled; provisioning
  and dashboard mounts are read-only; no Docker socket is mounted. Provisioning
  YAML uses only `${RECON_DASHBOARDS_RO_USER}` /
  `${RECON_DASHBOARDS_RO_PASSWORD}` env placeholders (plus the Grafana admin
  env), and no literal credential or secret is committed. Prometheus is
  **self-scrape only** and panel 6 (`up{job="prometheus"}`) is explicitly a
  supplementary availability stat, not live application metrics.
- **Visual check remains PENDING (AQ-4).** Automated evidence covers Grafana
  health, both datasources and their health, the provisioned `recon-ops`
  dashboard in folder `Recon`, every panel query through `/api/ds/query`, and
  the base-table denial. It does not prove rendered readability (titles, units,
  legends, bucket order, non-empty charts). **Visual panel check: PENDING
  (human).** No screenshot is committed and the visual check must not be
  recorded as passed until a human inspects it.
- **Metric exposure residual stays open.** M5-04 does not add authentication,
  rate limiting, or in-flight limits to the public API `/metrics`; Q-101
  residual A6 remains open, and Grafana/Prometheus auth, TLS, retention, and
  non-loopback exposure are out of scope.
- **No M5-05/M5-06 scope and no `docs/spec.md` edit.** M5-04 adds no agent
  quality views/dashboard, no integrity job or real integrity producer, no
  alerting, and no application services. `docs/spec.md` is unchanged; this
  entry, `docs/dashboards.md`, and the docs boundary in `docs/metrics.md` are
  the delivered documentation.

**Q-102 final adversary addendum (post-review docs phase; behavior frozen; no
`docs/spec.md` edit).** QA passed and the adversary recorded 0 blockers with
eight non-blocking findings. This addendum records the documentation-visible
residuals and their owners; `docs/dashboards.md` was corrected in the same
pass.

- **Down retains `recon_dashboards_ro` on a normal dev cluster.** The initdb
  login `recon_dev_dashboards_ro` is a member of `recon_dashboards_ro`, which
  is enough to make `DROP ROLE` raise SQLSTATE `2BP01`
  (`dependent_objects_still_exist`). The guarded Down catches it and emits a
  NOTICE that names only cross-database grants, so the role remains. Spec §3.1
  says the Down "drops the role"; that is not met in a normal dev cluster. No
  object privilege remains in this database, the role is `NOLOGIN`, and the
  project Up/Down applies/re-applies cleanly, so there is no grant or security
  impact. Owner: future migration cleanup (revoke membership before `DROP
  ROLE`, and re-grant it on Up if that is the chosen fix) or a spec-wording
  revision; do not drop the role without preserving the Grafana login, because
  Up does not recreate the initdb membership.
- **Synthetic boundary is now in the first paragraph.** `docs/dashboards.md`
  states in its opening section that panel data comes from the synthetic
  `recon demo ops` path and is demo evidence, not a production M2–M4 pipeline
  run or audit-chain proof (Q-98). The detailed boundary section is retained.
- **Live coverage: the T7 Go test is absent and the verifier does not require
  rows.** No `TestM504LiveDashboard` or `RECON_DASHBOARD_URL` exists anywhere in
  the tree. `scripts/verify-dashboard.sh` checks that each panel returns a
  frame and a successful query, but it passes with `rows=0`; the fresh-volume
  run before `make demo-ops` recorded `0/0/0/0/0/1` as execution-only evidence.
  The non-empty data evidence is the T4 integration coverage after the demo
  plus the post-demo live run, which recorded rows **1 / 4 / 7 / 3 / 6 / 1**.
  Owner: developer to add the named T7 test, or an explicit owner decision to
  accept the shell verifier and add a `--require-rows` mode; the docs now
  describe the delivered scope instead of claiming a Go T7 test.
- **Only Grafana and Prometheus are loopback-bound; Postgres is not.**
  `docker inspect` shows `recon-agent-postgres-1` publishing `5432/tcp` on all
  host interfaces (`0.0.0.0:5432`), while Grafana and Prometheus are
  `127.0.0.1:3000` / `127.0.0.1:9090`; `pg_hba.conf` accepts remote password
  auth. The Postgres binding is pre-existing and M5-04 changes neither it nor
  the Postgres port; AC-5's loopback requirement is scoped to the new
  Grafana/Prometheus ports. The test report's "loopback ports" wording is
  overbroad if read as the whole compose stack. Do not repeat "all services
  loopback". Owner: a separate Postgres-binding/password hardening task if the
  literal AC-5 wording is intended; `docs/dashboards.md` now names the
  exception.
- **`v_ops_match_rate` fails closed on a non-integer numeric stat.**
  `jsonb_typeof(...)='number'` accepts `1.5`, then `(stats->>'matched')::bigint`
  aborts with `invalid input syntax for type bigint: "1.5"`; the spec §3.2 SQL
  has the same cast. An absent or non-number value still counts as zero, and a
  zero denominator still yields NULL. The effect is a failed query for the
  affected bucket/source, not data corruption or a fabricated zero. The docs'
  earlier "malformed counts as zero" phrasing was overbroad and is corrected;
  owner: future view/spec hardening (integer-shape regex or numeric validation
  before the cast).
- **`OPENCODE_API_KEY` rotation is still pending (operational).** The W3 `.env`
  key was exposed in live OpenCode session history
  (`~/.local/share/opencode/opencode.db`); the host `.env` is untracked and
  gitignored but remains mode 644 and has not been rotated. The secret scan
  found no key content in the repository, run artifacts, dashboard JSON, or
  provisioning YAML. Required: rotate the key, update `.env`, `chmod 600 .env`,
  restart any holder of the old value, and optionally purge the affected
  session rows. This is not a code defect or M5-04 acceptance criterion, but it
  is the highest-priority operational action before further live probes.
- **UTC-midnight determinism edge.** `recon demo ops` reads `SELECT now()` once
  per run and anchors date buckets to that value, so two reset/seed runs that
  straddle 00:00 UTC can produce different date-bearing values; determinism is
  guaranteed only for the same seed and reset database on the same UTC day
  (spec §6.3). Low-probability test flake, no data risk; owner: inject a fixed
  clock or skip/retry on differing database dates.
- **Stale source/config comments (not editable from docs).** The migration
  still carries `TODO(M5-04 W1)`, and `compose.yaml`'s header still says "four
  NOLOGIN group roles and four local-only development logins" while initdb now
  creates five/five. The Down/test comments also name only cross-database
  grants, omitting the membership cause above. Owner: next source touch of the
  migration, compose file, or dashboard integration test.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-102 | **RESOLVED (M5-04) for the implementation; residuals stay OPEN:** the deterministic `recon demo ops` seed (with the M1 demo) substitutes for the missing production M2–M4 pipeline and is explicitly synthetic, with no app services added to compose (Q-98/AQ-1); the dashboard reads through the read-only `recon_dashboards_ro` role and exactly five `SELECT`-only `v_ops_*` views (no base-table grant; `42501` denial; role settings read-only/30s), with a guarded Down and a fresh-volume login plus documented existing-volume rerun; aging uses the minimum resolvable value date and canonical `0-1 / 2-3 / 4-7 / >7 / unknown` buckets (missing/future → `unknown`); classification is one terminal row per case with escalated > human > auto and `event_at = COALESCE(resolved_at, created_at)`, and approval latency is fail-closed on rejection; Grafana 13.3.2 / Prometheus v3.16.0 are pinned, self-scrape only, loopback-bound, anonymous/sign-up/plugin-preinstall off, with env-only credential placeholders and no committed secrets; panel 4's `$__timeFilter(approved_at)` macro expanded in the pinned Grafana; the visual panel check is PENDING (human, AQ-4) and Prometheus is not live application metrics; no M5-05/M5-06 scope and no `docs/spec.md` edit. | Spec §13.1 / FIN-42 / M5-04 / operators / M5-12 | Keep the synthetic-data boundary and no-app-services decision explicit in docs and demos; keep the view-only read-only role, canonical aging/precedence/queue semantics, pinned images, loopback/anon-off defaults, and env-only credentials; keep the existing-volume rerun path documented rather than claiming zero-step; cite API/query evidence only as provisioning/query proof and leave visual acceptance PENDING until a human checks it; keep Prometheus self-scrape labelled as not live app metrics; leave the public `/metrics` auth/rate-limit/pool residual open (Q-101 A6) and deliver M5-05/M5-06 only in their own tasks; amend `docs/spec.md` only in a future spec revision if owners choose to. |

## M5-05 Agent quality dashboard (Q-103)

**Q-103 addendum (Wave 4 docs/release phase; developer-owned; no
`docs/spec.md` edit).** M5-05 is delivered as the seven `v_eval_*` views, the
`Recon Agent Quality` dashboard, and the deterministic `recon demo quality`
seed. This entry records the delivered semantics, the closed edges, and the two
remaining spec/docs conflicts. Q-103 stays **OPEN** as the conflict record; it
is not used to reinterpret or waive the approved contract.

- **Seven `v_eval_*` views and twelve dashboard-facing views total.**
  `v_eval_accuracy`, `v_eval_false_fix`, `v_eval_cost_latency`,
  `v_eval_override_rate`, `v_eval_apply_failure_rate`, `v_eval_escalations`,
  and `v_eval_escalation_rate` join the five M5-04 `v_ops_*` views. All twelve
  are owner-rights views; `recon_dashboards_ro` has `SELECT` on the views only,
  `PUBLIC` is revoked, no base-table/sequence/function/`v_agent_*` grant is
  added, and base-table reads remain SQLSTATE `42501`. The M5-04 inventory and
  latest-migration assertions were updated from five/`20261124120000` to
  twelve/`20261125120000` by name; no read-only, PUBLIC-denial, owner-rights,
  role-attribute, write-denial, or schema-usage assertion was deleted or
  relaxed.
- **Eval/production hybrid sources.** Accuracy, false-fix, and cost/latency
  read the persisted eval tables. Override and apply-failure read
  `approvals -> proposals -> agent_runs`, with `'unknown'` model/prompt
  fallback so a terminal decision or failed apply is never lost. Escalation
  reasons read `audit_log` `case.transition` history; the cohort rate uses
  `cases.created_at` for its denominator. No view reads `eval_truth`, and the
  dashboard reads only the views.
- **Accuracy excludes injection; false-fix includes it.** Accuracy counts
  `correct IS TRUE` over every case in completed, non-repeat
  `suite IN ('agent','e2e')`, `split <> 'injection'` runs; a NULL/false
  `correct` counts as not correct. False-fix and cost/latency use the same run
  filters with all splits, including injection. The two rates use separate
  FILTER denominators and are never combined.
- **Override is rejection-based.** `v_eval_override_rate` is
  `rejections / (approved + rejected)` over terminal, non-NULL
  `approvals.decided_at` decisions. Pending approvals enter neither side. The
  product has no edit-override event, so the spec §13.2 "rejects or edits"
  wording cannot be fully represented; no edit rate is inferred from Slack
  message updates or any other unrelated state.
- **Apply-failure is strict and time-attributed.**
  `v_eval_apply_failure_rate` counts only terminal attempts:
  `applied` proposals with a non-NULL `applied_proposals.applied_at` (timestamp
  = `applied_at`) plus `apply_failed` proposals (timestamp = latest
  `proposal.transition` audit with `after.status = 'apply_failed'`, falling
  back to `proposals.created_at`). Pending/approved/rejected/superseded and
  applied-without-an-applied-row are excluded from both sides; one row per
  proposal prevents duplicate attempts. This is the approved strict
  `failures / (applied + apply_failed)` denominator, not failures over all
  proposals.
- **Escalation reasons and same-cohort rate are separate views.**
  `v_eval_escalations` counts `audit_log` escalation events per UTC event day
  and reason, with empty/missing reason folded to `unknown`; a case escalating
  twice counts twice. `v_eval_escalation_rate` is a creation-day cohort rate:
  distinct cases created on day D that ever escalated divided by all cases
  created on D, even when the escalation event is on D+1 or later. Recent
  cohorts are right-censored, the rate is bounded 0..1 and not clamped, a day
  with cases but no escalations is a measured zero, and a day with no cases
  emits no row.
- **Aborted, repeat, and non-agent suites are filtered.** Accuracy, false-fix,
  and cost/latency exclude `ended_at IS NULL` runs, runs with a
  `repeat_group`, and suites outside `agent`/`e2e` (notably `matcher`). Repeat
  runs are reported separately by the eval report; they are not pooled into a
  dashboard denominator. A run with no qualifying rows produces no synthetic
  zero row, and a zero denominator is `NULL`.
- **`recon demo quality` is a strict superset and all-or-nothing.** `recon
  demo quality` / `make demo-quality` seeds M1, then the M5-04 ops layer when
  it is absent, then the M5-05 quality layer, all inside one transaction that
  takes `EXCLUSIVE` locks on every seed-target table before the presence checks
  and the marker snapshot. Without `--reset` it fails closed if any `eval_runs`
  row or demo-quality marker exists, before writing. Concurrent writers are
  serialized: a writer that commits before the locks are acquired is seen by
  the presence checks and aborts the seed with no writes; a writer that starts
  after the locks blocks until the seed commits and can never be adopted. A
  failure at any layer rolls the one transaction back exactly to the pre-run
  state, so a failure never leaves a partial M1+ops+quality superset; there is
  no compensating reset and no unguarded reset fallback. A real row is never
  adopted as demo-owned and is never deleted by a later reset. Explicit
  `--reset` remains a separate guarded destructive step. With `--reset` it runs the
  dev-only full schema reset after the all-tables guard has rejected any
  untagged row; it refuses `RECON_ENV=production`. The reset is a full-schema
  reset rather than the approved spec's tagged-only delete because `audit_log`
  is append-only: a tagged delete cannot remove prior escalation history, so
  repeated reseeds would double the escalation-reasons panel and break
  determinism. The guard is the safety control for the stronger reset.
- **All-tables reset guard, durable demo marker, atomic check/drop.** The
  guard inspects every table in `public` and `eval_truth`, not only the
  eval/approval tables: it allows reproducible migration reference rows
  (`currencies`, `periods`, `system_switches`), rows recorded in
  `demo_metadata`, and `goose_db_version`/marker infrastructure; any untagged
  row in `cases`, `case_breaks`, `breaks`, `matches`, `match_legs`,
  `match_runs`, `ledger_txns`, `ledger_entries`, `external_records`,
  `settlement_batches`, `statement_balances`, `reconciliation_scopes`,
  `audit_log`, `audit_chain_head`, `outbox`, `approval_messages`,
  `applied_proposals`, policy/config tables, or `eval_truth` aborts with
  `*UntaggedDataError` before any DDL. M1 has no per-row marker column, so the
  approved safety addition `20261125130000_m5_05_demo_metadata.sql` creates
  `demo_metadata`; the M1 pipeline records the rows it creates and the
  ops/quality layers record theirs in the write transaction. The guard query
  runs in the same transaction that takes `ACCESS EXCLUSIVE` locks over all
  dropped tables and performs the drop, closing the check-then-drop race: a
  concurrent insert either commits before the lock and aborts the reset or
  blocks until the schema is gone. This second migration is a safety repair to
  the M5-05 delivery, not a new product surface. A database seeded before the
  marker migration has unmarked rows; use a fresh/disposable database or a
  one-time manual wipe rather than bypassing the guard.
- **Atomic seed transaction.** The whole seed (M1, ops, quality and the
  `demo_metadata` marker diff) is one `READ COMMITTED` transaction serialized
  by a transaction-scoped advisory lock and `EXCLUSIVE` table locks on every
  seed-target table, so demo commands cannot interleave and no production
  writer can commit into a seed target while the seed runs. Nested service
  transactions are savepoints inside the seed transaction, so the audit
  writer's global advisory lock is held until the seed commits and concurrent
  audit appends wait for the seed; this dev-only serialization trade-off is
  accepted. Failures roll back exactly; concurrent real rows are never adopted;
  there is no compensation reset. Explicit `--reset` stays the existing
  all-tables `ACCESS EXCLUSIVE` guarded reset. A pre-marker database needs a
  one-time manual/fresh-database wipe; visual panel confirmation stays PENDING
  (human).
- **Seed evidence and synthetic boundary.** On the documented seed the quality
  seed writes 13 eval runs / 64 case results / 12 production cases / 10 agent
  runs / 10 proposals / 5 approvals (3 approved, 2 rejected) / 6 applied /
  2 `apply_failed` / 4 escalation events. After `make demo-quality`, the live
  quality panels rendered rows **7 / 7 / 5 / 7 / 7 / 7 / 3 / 7** and the ops
  panels **1 / 4 / 7 / 3 / 6 / 1**. Every panel is synthetic demo evidence,
  not a production M2–M4 pipeline run, audit-chain proof, or production
  quality claim; the command prints `SYNTHETIC DEMO DATA — NOT PRODUCTION
  EVIDENCE`, uses no network or LLM, and is deterministic for a pinned clock /
  the same seed and UTC day.
- **Verifier requires non-empty seeded panels.** `./scripts/verify-dashboard.sh`
  checks both dashboard UIDs (`recon-ops`, `recon-agent-quality`), both
  datasource health checks, provisioning/folder/version, every target through
  `/api/ds/query`, at least one frame and non-zero rows per seeded panel, at
  least three escalation reasons, anonymous dashboard access denied with
  HTTP 401, and base-table denial with SQLSTATE `42501`. A zero-row panel now
  fails instead of passing as execution-only evidence.
- **Grafana stale bind mount is an environment action.** The file provider
  loads `deploy/grafana/dashboards/*.json` into folder `Recon`; a long-running
  Grafana container may not see a newly added `agent-quality.json` until
  provisioning reloads or Grafana is restarted once. Fresh compose startup is
  the normal path; the restart is not a code or JSON defect.
- **Visual confirmation is PENDING (human).** Automated evidence proves
  provisioning, datasource health, query execution, and non-empty frames. It
  does not prove rendered titles/units/legends/readability. Open
  <http://127.0.0.1:3000/d/recon-agent-quality>, log in with the local admin
  credentials, and inspect the eight panels: `Accuracy by model / prompt`,
  `False-fix rate by model / prompt`, `Human override rate`,
  `Apply-failure rate`, `Average cost per case`, `Latency p50 / p95`,
  `Escalation reasons`, `Escalation rate`. No agent may mark this PASS.
- **No M5-06/M6 scope and no `docs/spec.md` edit.** M5-05 adds no daily
  integrity job, alerting, application services, new compose service,
  datasource, role, or base-table grant. M5-01 remains unticked and M5-06 is
  untouched. `docs/spec.md` is unchanged; this entry, `docs/agent-quality.md`,
  `docs/dashboards.md`, and `deploy/grafana/README.md` are the delivered
  documentation.

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-103 | **OPEN — spec/docs conflicts recorded; implementation delivered with documented boundaries:** M5-05 delivers seven `v_eval_*` owner-rights views (twelve dashboard-facing views total), with eval/production hybrid sources; accuracy excludes injection while false-fix includes it; override is rejections/terminal decisions with no edit event; apply-failure is the strict terminal `apply_failed/(applied+apply_failed)` denominator with `applied_at`/latest-audit/`created_at` time attribution; escalation reasons come from `audit_log` and escalation rate is a creation-day cohort rate; aborted/repeat/`matcher` runs are excluded; `recon demo quality` is a fail-closed all-or-nothing superset seeded in one transaction that takes `EXCLUSIVE` locks on every seed-target table before the presence checks and snapshot (concurrent writers are serialized; failures roll back exactly, concurrent rows are never adopted, no compensating reset) whose `--reset` is a separate guarded dev-only full schema reset because `audit_log` is append-only, which deviates from spec §5.4's tagged-only delete wording; the guard checks every dropped table/schema, allows only migration reference rows plus `demo_metadata`/demo-owned rows, and runs under `ACCESS EXCLUSIVE` locks in the drop transaction (second approved safety migration `20261125130000_m5_05_demo_metadata.sql`); M5-04's five-view/latest-migration tests and docs were deliberately updated to the twelve-view inventory without weakening any security assertion; the verifier now requires non-empty panels on both dashboards; visual confirmation is PENDING (human); a stale Grafana bind mount may require one restart; the seed is explicitly synthetic; no M5-06/M6 scope and no `docs/spec.md` edit. Two conflicts remain unresolved: the append-only reset boundary versus the spec's tagged-delete wording, and §13.2's "rejects or edits" override definition versus the absence of any edit event. | Spec §5.4/§13.2 / M5-05 / future spec revision | Keep the delivered guarded full-reset boundary and rejection-based override metric documented, and amend `docs/spec.md` in a future owner-approved revision either to describe the append-only reset constraint or to add a real edit-override event. Do not resolve Q-103 by code or by editing the spec in this task. |
