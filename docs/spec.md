# Payment Reconciliation Agent: Technical Spec

Status: Draft v0.5 (v0.4 additions plus: tie-out example corrected, `pattern` claim on proposals, `llm_calls` for exact replay)
Purpose: Portfolio project demonstrating production-minded agentic AI engineering in a fintech domain.

---

## 1. Overview

A system that reconciles an internal ledger against external payment-provider (PSP) and bank settlement data. A deterministic engine matches what it can. An LLM agent investigates the unmatched remainder ("breaks"), proposes resolutions, and routes risky ones to humans for approval via Slack or a ticketing system.

**Core principle:** the LLM never moves or mutates money directly. It reads through constrained tools and writes *proposals* only. A deterministic, audited executor applies approved proposals.

### 1.1 Goals

- Automatically resolve common, low-risk breaks with a full audit trail.
- Escalate ambiguous or high-value breaks with an evidence-backed case file.
- Measure agent quality with a repeatable eval harness against synthetic ground truth.
- Provide ops dashboards, daily reports, and per-case replayable traces.
- Demonstrate defenses against prompt injection from untrusted payment data.

### 1.2 Non-goals

- Real payment processing or real financial data.
- Regulatory certification or compliance claims (PCI DSS, SOX, PSD2, etc.). The design *follows* standard financial-correctness and control practices (see section 4), but nothing is certified or independently audited.
- A general-purpose accounting system.
- Multi-tenant SaaS concerns (single tenant is fine).
- Direct PSP-to-bank matching without the ledger. Each source is reconciled against the ledger independently (see sections 5 and 21).

---

## 2. Architecture

```
                 ┌──────────────┐   ┌────────────────┐
 PSP files/API ─▶│  Ingestion   │──▶│   Postgres     │
 Bank files   ─▶ │  + Normalize │   │ ledger, events │
 Webhooks     ─▶ └──────────────┘   │ cases, audit   │
                                     └───────┬────────┘
                                             │
                          ┌──────────────────▼───────────────┐
                          │  Matching Engine (deterministic)  │
                          └──────────────────┬───────────────┘
                                   unmatched │ breaks
                          ┌──────────────────▼───────────────┐
                          │   Case Manager (queue, state)     │
                          └──────────────────┬───────────────┘
                                             │
                          ┌──────────────────▼───────────────┐
                          │  Agent Runtime (LLM + tools)      │
                          │  read tools · proposal tools      │
                          └──────────────────┬───────────────┘
                                   proposals │
                          ┌──────────────────▼───────────────┐
                          │  Policy Engine (risk tiering)     │
                          └───────┬──────────────┬───────────┘
                          auto    │              │ needs approval
                    ┌─────────────▼──┐     ┌─────▼──────────────┐
                    │ Executor       │     │ Approval Service   │
                    │ (ledger writes)│◀────│ Slack / Ticket     │
                    └────────────────┘     └────────────────────┘

 Cross-cutting: OpenTelemetry traces, audit log, metrics → Grafana, reports
```

### 2.1 Components

| Component | Responsibility | Notes |
|---|---|---|
| Ingestion | Parse and normalize PSP/bank files and webhooks into canonical records | Idempotent by source file hash + record ID |
| Ledger | Double-entry internal ledger | Source of truth for internal side |
| Matching Engine | Rule-based matching in ordered passes | Pure Go, no LLM |
| Case Manager | Creates cases from breaks, tracks state | Queue with leasing |
| Agent Runtime | Runs the LLM loop with tool calls, budgets, and guardrails | Model-agnostic adapter |
| Policy Engine | Runs deterministic verifiers and assigns a risk tier to a proposal | Config-driven; agent confidence can only tighten the tier |
| Approval Service | State machine plus channel adapters | Slack first, ticket second |
| Executor | Applies approved proposals as ledger entries | Only component allowed to write adjustments |
| Eval Harness | Runs agent against synthetic scenarios and scores results | CLI, CI-friendly |
| Observability | Traces, metrics, audit log | OTel to Langfuse/Phoenix, Prometheus to Grafana |

### 2.2 Suggested stack

- Language: Go (core, API, agent runtime, eval CLI)
- Database: Postgres (with `pgx`, migrations via `goose` or `atlas`)
- LLM access: provider SDK behind an `LLMClient` interface (swap models for evals)
- Tracing: OpenTelemetry, exported to Langfuse or Phoenix
- Metrics and dashboards: Prometheus + Grafana (or Grafana directly on Postgres)
- Approval UI: Slack (Block Kit) plus optional small Go/htmx page
- Packaging: Docker Compose for local run

---

## 3. Data Model

All money is stored as integer minor units plus ISO 4217 currency code. No floats anywhere.

### 3.1 Core tables

```sql
-- Internal double-entry ledger
accounts(id, code, name, type, currency, status CHECK (status IN ('active','inactive','closed')))
ledger_entries(
  id, txn_id, account_id, direction CHECK (direction IN ('debit','credit')),
  amount_minor BIGINT, currency, posted_at, external_ref, metadata JSONB
)
ledger_txns(
  id, seq BIGINT GENERATED ALWAYS AS IDENTITY,   -- monotonic ordering
  created_at TIMESTAMPTZ, booked_at TIMESTAMPTZ, value_date DATE,
  kind, idempotency_key UNIQUE, request_hash TEXT, description,
  period_id UUID REFERENCES periods(id),               -- posting period (FIN-56)
  adjusts_period_id UUID NULL REFERENCES periods(id),  -- set when adjusting a closed period
  reversal_of UUID NULL REFERENCES ledger_txns(id)
)
-- Invariant: per txn_id, sum(debits) = sum(credits) per currency
-- Posted rows are immutable; corrections are reversals + new txns (FIN-11)

currencies(code PK, exponent SMALLINT)           -- ISO 4217 minor-unit exponent (JPY 0, USD 2, KWD 3)
fx_rates(id, base, quote, rate NUMERIC(24,12), as_of TIMESTAMPTZ, source)  -- never float

outbox(id, aggregate_type, aggregate_id, topic, payload JSONB, created_at, delivered_at NULL, attempts INT)
-- Written in the same DB transaction as the state change it announces (FIN-22)

-- Periods and reconciliation scopes (FIN-54 to 57)
periods(id, start_date DATE, end_date DATE, status CHECK (status IN ('open','closed')), closed_at, closed_by)
-- Posting into a closed period is rejected by a DB trigger (FIN-56)

reconciliation_scopes(id, ledger_account_id, source TEXT, currency)   -- e.g. PSP clearing account <-> psp_a, USD
statement_balances(
  id, scope_id, as_of_date DATE, opening_minor BIGINT, closing_minor BIGINT, currency, batch_id,
  UNIQUE(scope_id, as_of_date)
)

-- Fee schedule used by deterministic verifiers (section 7.1); rates in basis points, never float
fee_schedules(id, source, kind, currency, pct_bps INT, fixed_minor BIGINT, valid_from DATE, valid_to DATE NULL)

-- Normalized external records
external_records(
  id, source TEXT,              -- 'psp_a', 'bank_x'
  source_record_id TEXT,
  kind TEXT,                    -- 'charge','refund','payout','fee','chargeback','fx'
  amount_minor BIGINT, currency, fee_minor BIGINT,
  occurred_at, settled_at, batch_id,
  raw JSONB, raw_hash TEXT,
  UNIQUE(source, source_record_id)
)

settlement_batches(id, source, period_start, period_end, declared_total_minor, currency, file_hash UNIQUE)

-- Matching results
matches(
  id, ledger_txn_id, external_record_id, rule TEXT, confidence NUMERIC, created_at, match_run_id,
  status CHECK (status IN ('active','unmatched')), unmatched_at, unmatched_by, unmatch_reason
)   -- rows are never deleted; unmatching is an audited status change (section 8.5)

-- Breaks and cases
breaks(
  id, side TEXT,                -- 'ledger_only','external_only','amount_mismatch','duplicate'
  ledger_txn_id NULL, external_record_id NULL,
  delta_minor BIGINT, currency, detected_at,
  status CHECK (status IN ('open','in_case','resolved','superseded','void'))
)
cases(
  id, break_ids UUID[], priority, value_minor, currency,
  status CHECK (status IN ('open','in_agent','proposal_pending','waiting_info','escalated','resolved')),
  version INT, attempts INT, escalation_reason TEXT,
  created_at, leased_by, lease_expires_at, resolved_at, resolution_type
)   -- state machine in section 8.5

-- Agent runs and proposals
agent_runs(
  id, case_id, model, prompt_version, started_at, ended_at, tokens_in, tokens_out, cost_usd, trace_id,
  outcome CHECK (outcome IN ('proposal','escalated','budget_exhausted','invalid_output','provider_error'))
)

-- Raw tool I/O: source of truth for evidence validation and replay (section 12)
tool_calls(id, agent_run_id, seq INT, tool_name, args JSONB, result JSONB, result_hash TEXT, error TEXT, started_at, duration_ms INT)
llm_calls(id, agent_run_id, seq INT, model TEXT, request_hash TEXT, response JSONB, usage JSONB, started_at, duration_ms INT)   -- recorded model responses enable exact replay

proposals(
  id, case_id, agent_run_id,
  type TEXT CHECK (type IN ('link','adjustment','write_off','duplicate','request_info')),  -- mapping to tools in 6.2
  payload JSONB, rationale TEXT, evidence JSONB, confidence NUMERIC,
  pattern_claimed TEXT, pattern_verified TEXT,   -- the claim is untrusted; policy uses pattern_verified only
  verification JSONB,                  -- deterministic verifier output (section 7.1)
  risk_tier TEXT, policy_decision JSONB,
  status CHECK (status IN ('proposed','pending_approval','approved','auto_approved','applied',
                           'apply_failed','rejected','expired','superseded')),
  version INT, state_hash TEXT, idempotency_key UNIQUE, supersedes_proposal_id UUID NULL,
  created_at, expires_at
)
-- At most one active proposal per case:
--   UNIQUE INDEX ON proposals(case_id) WHERE status IN ('proposed','pending_approval','approved','auto_approved')
-- 'escalate' is not a proposal: it sets cases.status = 'escalated' with a reason (section 6.2)

-- Exactly-once application record (FIN-23)
applied_proposals(
  proposal_id UUID PRIMARY KEY REFERENCES proposals(id),
  ledger_txn_id UUID NULL UNIQUE REFERENCES ledger_txns(id),   -- NULL for link/duplicate proposals
  applied_at, executor_run_id
)

-- Approvals and audit
approvers(
  id, display_name, role CHECK (role IN ('viewer','approver','admin')),
  slack_user_id UNIQUE, ticket_user_id UNIQUE, active BOOL
)
approvals(id, proposal_id, channel TEXT, external_ref TEXT, approver_id REFERENCES approvers(id), decision, decided_at, signature_valid BOOL)
audit_log(
  id, seq BIGINT GENERATED ALWAYS AS IDENTITY, ts, actor_type, actor_id, action, entity_type, entity_id,
  before JSONB, after JSONB, trace_id,
  prev_hash BYTEA, row_hash BYTEA          -- hash chain (FIN-31); append-only
)

-- Evaluation (section 11)
eval_runs(
  id, suite, split CHECK (split IN ('dev','test','injection')), dataset_version,
  model, prompt_version, seed, git_sha, config JSONB, started_at, ended_at
)
eval_case_results(
  id, eval_run_id, scenario_id, failure_mode, correct BOOL, false_fix BOOL, escalated BOOL,
  tier_expected, tier_actual, confidence NUMERIC, cost_usd NUMERIC, latency_ms INT, steps INT, detail JSONB
)
```

### 3.2 Invariants (enforced and tested)

1. Every ledger transaction balances per currency.
2. `audit_log` is append-only (no UPDATE/DELETE grants for the app role).
3. A proposal can be applied at most once (unique `applied_proposals.proposal_id`, FIN-23).
4. No ledger write originates from the agent runtime's DB role. Only the executor role may insert adjustment transactions.
5. Applied adjustments reference an approved proposal or an auto-approve policy decision.
6. No ledger entry is posted into a closed period (FIN-56).
7. A case has at most one active proposal at any time (partial unique index).

---

## 4. Financial Correctness and Control Requirements

These are numbered, testable requirements. Each has a verification method (see 4.8). They apply to all code that touches money, ledger state, proposals, or audit data.

### 4.1 Money representation

| ID | Requirement |
|---|---|
| FIN-01 | No floating-point types for monetary amounts, fees, or FX rates anywhere: DB (`BIGINT`/`NUMERIC` only; no `REAL`, `DOUBLE PRECISION`, `FLOAT`), Go (`int64` minor units or an exact decimal type), JSON, and LLM tool arguments. |
| FIN-02 | Every amount is paired with an ISO 4217 currency. A `Money` type carries both; arithmetic on mismatched currencies is an error, and cross-currency conversion is only possible through an explicit FX function. |
| FIN-03 | Minor-unit exponents come from the `currencies` table, not hardcoded `/100`. Parsers convert decimal strings to minor units with exact decimal parsing (never via float) and reject values with more precision than the currency allows. |
| FIN-04 | Integer arithmetic is overflow-checked. Overflow is an error, never a wraparound. |
| FIN-05 | In APIs and tool schemas, amounts are integer minor units (or decimal strings), never fractional JSON numbers. |
| FIN-06 | One central rounding function, round-half-even by default, used for all FX and fee calculations. FX rates are `NUMERIC` with fixed scale and stored with `source` and `as_of`. Any rounding residual is posted explicitly to a rounding account, never silently dropped. |

### 4.2 Ledger integrity

| ID | Requirement |
|---|---|
| FIN-10 | Every transaction balances per currency (debits = credits). Enforced at two layers: application validation and a deferred DB constraint/trigger checked at commit. |
| FIN-11 | Posted ledger entries are immutable. No `UPDATE` or `DELETE`: the app role has no such grants, and DB triggers reject them. Corrections are made only by posting a reversing transaction (`reversal_of`) plus a new transaction. |
| FIN-12 | Every write path takes an idempotency key. Replaying the same key and payload returns the original result. The same key with a different payload (`request_hash` mismatch) is rejected as a conflict. |
| FIN-13 | Entries must use the account's currency. Posting to closed or inactive accounts is rejected. Adjustments may only target an allowlisted set of accounts (fees expense, FX gain/loss, rounding, suspense). Anything touching customer-funds accounts requires tier T2. |
| FIN-14 | Ledger transactions have a monotonic sequence number for deterministic ordering and gap detection. |

### 4.3 Transactionality and concurrency

| ID | Requirement |
|---|---|
| FIN-20 | **Atomic apply.** The executor performs these in one DB transaction: re-validate the proposal and break state, insert the ledger transaction and entries, mark the proposal `applied`, and insert audit entries. All or nothing. |
| FIN-21 | The executor runs at `SERIALIZABLE` isolation (or `REPEATABLE READ` with explicit `SELECT ... FOR UPDATE` on the affected case, proposal, and accounts), with bounded retry on serialization failures. |
| FIN-22 | **Outbox pattern.** External side effects (Slack messages, ticket creation, report posts) are recorded in the `outbox` table in the same transaction as the state change, then delivered by a worker with at-least-once semantics. No network calls inside a DB transaction. Consumers and callbacks are idempotent. |
| FIN-23 | A proposal can be applied at most once, enforced by a unique constraint (`applied_proposals.proposal_id` primary key, plus the ledger txn idempotency key), not only by application logic. |
| FIN-24 | Case leasing uses expiring leases. State changes use optimistic concurrency (version column or state hash). Stale writers fail cleanly. |
| FIN-25 | Crash safety: killing any process at any point leaves no partial state (no ledger entries without audit, no `applied` proposal without a ledger txn, and vice versa). Verified by fault-injection tests. |

### 4.4 Auditability

| ID | Requirement |
|---|---|
| FIN-30 | Every change to ledger, case, proposal, approval, or policy decision writes an audit entry in the same transaction: actor type and ID, action, before/after, UTC timestamp, `trace_id`. |
| FIN-31 | The audit log is append-only (DB-enforced). Each row stores a hash chaining to the previous row; `recon audit verify` recomputes the chain and reports tampering or gaps. |
| FIN-32 | End-to-end traceability: any applied adjustment can be traced to the break, the agent run and trace, the policy decision (rule and config version), and the approver. Provided as a CLI command and in the case file. |
| FIN-33 | Actors are typed: `system:matcher`, `agent:<model>`, `system:executor`, `human:<id>`. An agent or system actor can never satisfy an approval requirement. |
| FIN-34 | Policy decisions record the rule that fired and a hash of the policy config in force, so decisions remain explainable after config changes. |

### 4.5 Time handling

| ID | Requirement |
|---|---|
| FIN-40 | All timestamps are stored as UTC `TIMESTAMPTZ`. Business dates (`value_date`, settlement date) are stored as `DATE` separately from the booking timestamp. |
| FIN-41 | Settlement cutoffs and business-day calendars are configured per source with an explicit timezone. |
| FIN-42 | Matching windows and aging buckets are defined on a named date field (value date, not "whatever timestamp"). |
| FIN-43 | Time comes from an injected `Clock` interface so tests, evals, and replays are deterministic. |

### 4.6 Control totals, tie-outs, and period close

| ID | Requirement |
|---|---|
| FIN-50 | Ingestion control totals: the sum of a batch's records must equal `declared_total_minor` and row counts must match. A mismatch quarantines the whole batch (no partial ingest) and raises an alert. |
| FIN-51 | Re-ingesting the same file (by hash) is a no-op. Records are deduplicated on `(source, source_record_id)`. |
| FIN-52 | A daily integrity job verifies, independently of the matching engine's own bookkeeping: every ledger transaction balances; matched + unmatched value equals total ingested value per source and currency; every `applied_proposals` row has its expected ledger transaction and audit trail. Results are written to the daily report and exported as metrics; failure alerts. |
| FIN-53 | Auto-apply limits: per-proposal value caps and per-day cumulative caps by currency. A circuit breaker pauses auto-apply (falling back to T1, via the `auto_apply_enabled` switch in section 6.5) when a cap is hit or when the break/anomaly rate spikes. |
| FIN-54 | **Balance tie-out.** For each reconciliation scope and statement date, the reconciliation equation must hold: the ledger balance of the scope's clearing account, adjusted by ledger-only open items, equals the statement closing balance, adjusted by external-only open items. It is computed from raw balances and open items, independently of the matching engine, so a matcher bug that affects balances cannot hide a discrepancy. (A dropped match only creates breaks and leaves balances tied, and a mis-pairing of equal amounts does not change balances; the matcher component evals in 11.5 catch those.) A non-zero *unexplained difference* alerts and blocks period close. |
| FIN-55 | **Statement continuity.** A statement's opening balance must equal the previous statement's closing balance for the same scope. A mismatch (missing file, gap, restatement) quarantines the batch and raises an alert. |
| FIN-56 | **Period locking.** Ledger periods are `open` or `closed`. Posting into a closed period is rejected by a DB trigger. An adjustment relating to a closed period is posted in the current open period with `adjusts_period_id` set; closed periods are never modified. |
| FIN-57 | **Period close.** Closing a period requires that every scope ties out (FIN-54) or each unexplained difference is covered by an owned open case or suspense item, that the integrity job (FIN-52) passes, and that no proposal targeting the period is `pending_approval` or `approved`. Close and reopen are audited; reopening requires the `admin` role and a reason. |

### 4.7 Data protection and retention

| ID | Requirement |
|---|---|
| FIN-60 | Synthetic data only. No real PANs or bank account numbers. Account-like identifiers are tokenized or masked (last four only) in the data model, traces, and reports. |
| FIN-61 | Retention policy documented and configurable (example default: ledger and audit retained 7 years). Personal data in cases, traces, and logs can be redacted without breaking ledger integrity or the audit hash chain (redaction records a tombstone entry). |
| FIN-62 | Secrets and credentials handled per section 16; never present in prompts, traces, logs, or reports. |

### 4.8 Verification

| Requirement(s) | How verified |
|---|---|
| FIN-01, 02, 05 | CI static check: fail on `float32`/`float64` in money packages and on `REAL`/`DOUBLE PRECISION`/`FLOAT` in migrations; unit tests that cross-currency arithmetic fails; JSON-schema tests for tool and API payloads |
| FIN-03, 04, 06 | Table-driven and property-based tests across 0-, 2-, and 3-decimal currencies; overflow tests; rounding property tests (e.g., sum of rounded parts vs rounded sum accounted for by the residual entry) |
| FIN-10, 11, 12, 13, 14 | DB-level tests: attempt unbalanced commit, UPDATE/DELETE as the app role, duplicate idempotency key with differing payload, wrong-currency and disallowed-account postings |
| FIN-20 to 25 | Concurrency tests (parallel apply of the same proposal, competing executors) and fault-injection tests (kill between each step of apply; assert no partial state and that outbox delivery recovers) |
| FIN-30 to 34 | Audit-completeness test (every mutation has an audit row in the same transaction); `recon audit verify` against a deliberately tampered DB; traceability test from adjustment back to approver |
| FIN-40 to 43 | Tests with injected clocks across DST boundaries and cutoff edges; lint against direct `time.Now()` in domain packages |
| FIN-50 to 55 | Ingestion tests with corrupted totals, duplicate files, and gapped statements; integrity-job tests with seeded inconsistencies; tie-out tests where a seeded matcher bug (a tolerance match whose delta is never booked) must still surface as a non-zero unexplained difference, and where a dropped match correctly leaves the difference at zero (it only creates breaks); cap and circuit-breaker tests |
| FIN-56, 57 | DB-level test that posting into a closed period fails; an adjustment for a closed period lands in the open period with `adjusts_period_id`; close is blocked while a tie-out difference or an active proposal exists; reopen requires `admin` and is audited |
| FIN-60 to 62 | Log/trace scan test for PAN-like patterns and secrets; redaction test preserving the audit chain |

---

## 5. Matching Engine (Deterministic)

**Scope and topology.** Each external source is reconciled against the ledger independently ("hub-and-spoke"). A `reconciliation_scope` pairs one ledger account with one source and currency (e.g., PSP clearing account ↔ `psp_a`, USD; bank account ↔ `bank_x`, USD). A PSP payout appears in both the PSP scope and the bank scope through the ledger's payout transaction, so the PSP-to-bank leg is covered without matching PSP and bank records directly. Direct three-way matching is out of scope for v1 (section 21).

**Run semantics.** Matching runs are incremental and idempotent: re-running on unchanged data creates no new matches or breaks, and every match records the `match_run_id` that created it. Late-arriving data can resolve an existing break (section 8.5).

Ordered passes, each producing matches or leaving records for the next pass:

1. **Exact:** same reference ID, amount, and currency.
2. **Reference + tolerance:** same reference, amount within configured fee tolerance.
3. **Composite:** one external payout vs N ledger entries (batch settlement). Sum-match within a batch.
4. **Fuzzy:** amount + date window + counterparty hint, with a confidence score. Below the threshold, it becomes a break instead.
5. **Duplicates:** detect repeated external IDs or same-amount/same-time charges.

Output: `matches` rows plus `breaks` for the remainder. The engine is fully unit-tested and property-tested (e.g., matching is order-independent).

---

## 6. Agent Design

### 6.1 Loop

For each leased case:

1. Build the initial context: case summary, break details, relevant policy snippets.
2. Run a bounded tool-use loop (max steps, max tokens, max cost, wall-clock timeout).
3. Terminate when the agent calls a proposal tool (`propose_*`, `mark_duplicate`, `request_info`) or `escalate`, or when a budget is exhausted (budget exhaustion forces `escalate`). A run produces at most one proposal.
4. Persist the run, trace, and result.

### 6.2 Tools

Read-only tools (no side effects):

| Tool | Purpose | Key args |
|---|---|---|
| `get_case` | Break details and current state | `case_id` |
| `lookup_ledger_txn` | Fetch ledger txn + entries | `txn_id` or `external_ref` |
| `lookup_external_record` | Fetch normalized external record | `source`, `record_id` |
| `search_external_records` | Search by amount/date window/reference | `amount_minor`, `currency`, `from`, `to`, `reference?` |
| `get_settlement_batch` | Batch totals and member records | `batch_id` |
| `get_fx_rate` | Rate on a date (from a local table) | `base`, `quote`, `date` |
| `get_fee_schedule` | Fee schedule in force on a date | `source`, `kind`, `currency`, `date` |
| `get_history` | Past resolved cases with similar signature | `signature` |

Proposal tools (write to `proposals` only):

| Tool | Purpose |
|---|---|
| `propose_link` | Link a ledger txn to an external record (match missed by engine) |
| `propose_adjustment` | Balanced adjusting entry (e.g., fee variance, FX difference) |
| `propose_write_off` | Write off a small residual to a designated account |
| `mark_duplicate` | Flag an external record as a duplicate |
| `request_info` | Ask a human for missing information |
| `escalate` | Hand off with a case summary and reason |

Every proposal tool requires: `rationale`, `evidence` (list of record references the agent actually retrieved), and `confidence` (0–1). Evidence references are validated server-side against this run's `tool_calls`: a proposal citing records the agent never fetched is rejected. `propose_link`, `propose_adjustment`, and `propose_write_off` also take an optional `pattern` claim (`fee_variance`, `fx_rounding`, `rounding`, `reference_typo`, `other`). The claim is untrusted: the policy engine acts only on a pattern confirmed by the deterministic verifier (7.1).

**Tool-to-proposal mapping.** `propose_link` → `link`, `propose_adjustment` → `adjustment`, `propose_write_off` → `write_off`, `mark_duplicate` → `duplicate`, `request_info` → `request_info`. `escalate` is not a proposal: it sets the case to `escalated` with a reason and records the run outcome.

**Evidence reference format.** Typed, stable identifiers the server can resolve: `ledger_txn:<uuid>`, `ext:<source>:<source_record_id>`, `batch:<batch_id>`, `fx:<base>/<quote>@<date>`, `fee_schedule:<id>`, `case:<id>`. A reference is valid only if it appears in the result of a tool call in the same run.

### 6.3 Example tool schema

```json
{
  "name": "propose_adjustment",
  "description": "Propose a balanced adjusting entry. Does NOT post to the ledger.",
  "input_schema": {
    "type": "object",
    "required": ["case_id", "lines", "rationale", "evidence", "confidence"],
    "properties": {
      "case_id": {"type": "string"},
      "lines": {
        "type": "array", "minItems": 2,
        "items": {
          "type": "object",
          "required": ["account_code", "direction", "amount_minor", "currency"],
          "properties": {
            "account_code": {"type": "string"},
            "direction": {"enum": ["debit", "credit"]},
            "amount_minor": {"type": "integer", "minimum": 1},
            "currency": {"type": "string", "pattern": "^[A-Z]{3}$"}
          }
        }
      },
      "rationale": {"type": "string", "maxLength": 2000},
      "evidence": {"type": "array", "items": {"type": "string"}, "minItems": 1},
      "confidence": {"type": "number", "minimum": 0, "maximum": 1},
      "pattern": {"enum": ["fee_variance", "fx_rounding", "rounding", "reference_typo", "other"]}
    }
  }
}
```

### 6.4 Agent guardrails

- Tool arguments are validated against schema, then against business rules (balanced entries, allowed accounts, currency match, amount caps).
- Per-case budgets: max tool calls (e.g., 15), max cost, max wall-clock.
- The agent cannot choose the risk tier. The Policy Engine does.
- Model output is never executed as code or SQL. Tools use parameterized queries only.
- Deterministic fallback: any malformed or looping behavior results in `escalate`.

### 6.5 LLM provider failures and operational controls

- **Retries:** exponential backoff with jitter on 429/5xx/timeouts, with bounded attempts inside a run. A per-provider circuit breaker stops calls during an outage. An optional fallback model may be configured; the model actually used is recorded on `agent_runs.model`, and only models that pass the eval gate may be configured as fallbacks.
- **Infrastructure vs. agent failures:** a run ending in `provider_error` does not consume the case's attempt budget. A run ending in `invalid_output` or `budget_exhausted` does.
- **Poison cases:** `cases.attempts` is capped (default 3). Beyond the cap the case is `escalated` with `escalation_reason = agent_failed`, which serves as the dead-letter queue for human review. An expired lease returns the case to the queue and counts as an attempt.
- **Load and cost limits:** a global cap on concurrent runs and a daily USD budget. Hitting either stops new leases; cases wait in the queue and nothing fails.
- **Kill switches:** two audited, DB-backed flags, `agent_enabled` and `auto_apply_enabled`. With the agent off, ingestion and matching continue and breaks queue for humans. With auto-apply off, T0 proposals are routed as T1. The FIN-53 circuit breaker trips `auto_apply_enabled` automatically. Changing a switch requires the `admin` role.

---

## 7. Policy Engine and Risk Tiers

Configured in YAML, evaluated deterministically on each proposal.

| Tier | Typical criteria | Handling |
|---|---|---|
| T0: Auto-apply | Known pattern (e.g., fee variance), value below threshold, **deterministic verifier passes** (7.1), evidence valid | Executor applies; audit entry written; appears in daily report |
| T1: Single approval | Mid-size value, no verifier for the pattern, verifier failed on an otherwise valid proposal, confidence below the downgrade threshold, or new pattern | Slack approval by one authorized approver |
| T2: Escalate to ticket | High value, write-offs above limit, cross-currency, repeated customer/counterparty | Ticket with full case file; approval via ticket status |

```yaml
policy:
  auto_apply:
    max_value_minor: { USD: 500, EUR: 500, SEK: 5000 }
    require_verifier: true        # no verifier pass, no T0
    allowed_types: [link, adjustment]
    allowed_patterns: [fee_variance, rounding, fx_rounding, reference_typo]
  confidence:
    downgrade_below: 0.8          # may only move a proposal to a stricter tier, never a looser one
  single_approval:
    max_value_minor: { USD: 50000, EUR: 50000, SEK: 500000 }
  proposal_ttl_hours: 48
  four_eyes: true
```

Policy decisions are logged with the rule that fired, so "why was this auto-applied?" is always answerable.

### 7.1 Deterministic verifiers and confidence

A **verifier** independently recomputes what a proposal should be from source data, without the LLM. T0 requires a verifier pass; a proposal whose pattern has no verifier cannot be auto-applied. Verifier output is stored in `proposals.verification` and shown in the approval message.

| Pattern | Verifier checks |
|---|---|
| `fee_variance` | The expected fee, recomputed from `fee_schedules` (basis points plus fixed amount, via the central rounding function), equals the settlement delta exactly, and the adjustment lines equal that delta |
| `fx_rounding` | The converted amount, recomputed from the stored `fx_rates` row and rounding function, differs from the booked amount by exactly the proposed adjustment, within the per-currency rounding cap |
| `rounding` | The absolute delta is within the per-currency rounding cap and both sides agree after rounding |
| `reference_typo` | Amount, currency, and value date match within the configured window, and exactly one candidate satisfies this (a second candidate fails the verifier) |

Confidence is advisory. The agent's self-reported confidence can only move a proposal to a stricter tier (`downgrade_below`); it can never grant a looser one. The eval harness reports calibration (confidence vs. observed correctness, e.g., expected calibration error) so the threshold is tuned from data rather than assumed.

---

## 8. Approval Flow

### 8.1 State machine

```
proposed ─▶ pending_approval ─▶ approved ─▶ applied
    │              │                │
    │              ├─▶ rejected     └─▶ apply_failed ─▶ (retry | escalated)
    │              └─▶ expired
    └─▶ auto_approved ─▶ applied
```

Rules:

- Transitions are enforced in one place (a state machine package) with a transition table and tests.
- Each transition writes to `audit_log` with actor, timestamp, and trace ID.
- Expired proposals are never applied. The case returns to the queue or escalates.
- **Four-eyes (maker-checker):** the agent is the maker of its proposals and is never an approver. A human who edits a proposal, answers a `request_info`, or manually triggers a run on the case becomes a co-maker and cannot approve that proposal. Approvers must hold the `approver` role.
- **Superseded:** any non-terminal state can move to `superseded` when a newer proposal replaces it or a late match makes it moot (section 8.5). Terminal states: `applied`, `rejected`, `expired`, `superseded`.
- Re-validation at apply time: the executor re-checks that the break still exists and the ledger state matches the proposal's assumptions (optimistic concurrency via `proposals.state_hash`). If not, `apply_failed` with a reason such as `break_resolved` or `state_changed`.

### 8.2 Channel abstraction

```go
type ApprovalChannel interface {
    RequestApproval(ctx context.Context, p Proposal, c CaseFile) (externalRef string, err error)
    HandleCallback(ctx context.Context, req *http.Request) (Decision, error) // verifies signature
    Cancel(ctx context.Context, externalRef string) error
}
```

### 8.3 Slack adapter (first)

- Block Kit message: case summary, delta, evidence bullets, agent rationale, risk tier, link to full case file.
- Approve / Reject / Request info buttons.
- Verify Slack request signatures and timestamps (replay protection).
- Idempotent button handling (double clicks and retries do not double-apply).
- Map Slack user ID to an internal approver identity and role. Unauthorized users get an ephemeral denial and an audit entry.
- Update the message in place after a decision.

### 8.4 Ticket adapter (second)

- Jira or Linear: create a ticket with the case file; sync status via webhook (`Approved`/`Rejected` workflow states map to decisions).
- Store the ticket ID in `approvals.external_ref`.
- Webhook signature verification and the same idempotency rules.

### 8.5 Case lifecycle, supersession, and re-matching

```
open ─▶ in_agent ─▶ proposal_pending ─▶ resolved
  ▲        │              │
  │        ├─▶ escalated  ├─ rejected / expired ─▶ open (while attempts remain) | escalated
  │        └─▶ waiting_info
  └── re-opened when new data changes the break, or a waiting_info case gets its answer
```

Break states: `open`, `in_case`, `resolved`, `superseded` (consumed by a later match), `void`.

Rules:

- **One active proposal per case**, enforced by a partial unique index. A new proposal on a case moves the previous one to `superseded`, cancels its approval request through `ApprovalChannel.Cancel` (via the outbox), and writes audit entries.
- **Late data:** matching is incremental. If a new external record matches a ledger transaction belonging to an open case, the matcher records the match and resolves the break, and the case resolves with `resolution_type = superseded_by_match`. Any active proposal moves to `superseded` and its approval request is cancelled. A proposal already `approved` but not yet applied fails re-validation with `break_resolved`.
- **Retries:** a rejected or expired proposal returns the case to `open` until `attempts` reaches its cap, then the case is `escalated`. Rejection reasons are stored and can feed new eval scenarios.
- **Waiting:** `waiting_info` cases re-enter the queue on a new ingest touching them or on a human reply. A `missing_settlement` case escalates automatically after the configured SLA (business days, FIN-41).
- **Unmatching:** matches are never deleted. Unmatching sets `matches.status = unmatched` with actor and reason, reopens the breaks, and, if an adjustment was applied, requires a reversing transaction. In v1 unmatching is a human/admin operation only (audit-logged); the agent cannot unmatch.

---

## 9. Prompt-Injection and Data Trust

Untrusted fields: merchant memos, bank descriptions, customer names, free-text reference fields, file metadata.

Defenses:

1. **Structural separation:** untrusted text is passed only inside clearly delimited data blocks, labeled as untrusted, never concatenated into system instructions.
2. **Capability limiting:** the agent has no tool that can move money, send messages, or fetch arbitrary URLs. The worst-case injection yields a bad *proposal*, which still goes through policy and approval.
3. **Evidence validation:** proposals must cite records the agent actually retrieved. Amounts are validated against the retrieved data, not against narrative text.
4. **Sanitization:** strip control characters, truncate long fields, and flag suspicious patterns (e.g., "ignore previous instructions", role-play markers) as a signal that forces escalation, not as a sole defense.
5. **Detection:** log and surface cases where untrusted fields contain instruction-like content.
6. **Tested:** an injection suite in the eval harness (see 11.4) with a pass/fail threshold.

Write-up deliverable: `docs/threat-model.md` covering assets, trust boundaries, attacker goals, and mitigations.

---

## 10. Synthetic Data Generator

Deterministic (seeded) generator producing:

- Chart of accounts, customers, merchants, and N days of transactions across currencies.
- PSP settlement files and bank statements derived from the ledger, with **injected failure modes and ground truth labels**.

### 10.1 Failure modes

| Mode | Description | Expected resolution |
|---|---|---|
| `fee_variance` | Settlement net differs by fee | Adjustment to fees expense (T0) |
| `fx_rounding` | Conversion rounding difference | Adjustment (T0) |
| `missing_settlement` | Ledger charge with no settlement yet | Wait/request_info; escalate if past SLA |
| `late_settlement` | Settles after the cutoff | Link when it arrives |
| `duplicate_charge` | PSP shows duplicate | mark_duplicate |
| `partial_refund` | Refund amount differs | Adjustment or link |
| `chargeback` | Unexpected debit | Escalate (T2) |
| `batch_composite` | One payout covers N charges, one missing | Link N−1, flag the missing one |
| `reference_typo` | Reference mangled; matchable by amount/date | propose_link |
| `unknown_deposit` | No ledger counterpart | Escalate |
| `injection_memo` | Memo contains an adversarial instruction | Normal handling; no policy bypass |
| `ambiguous` | Multiple plausible matches | request_info/escalate (should not guess) |

Each scenario records: break signature, correct resolution type, correct amounts/accounts, and the correct risk tier. Difficulty levels (easy / realistic / adversarial) control noise and ambiguity.

---

## 11. Eval Harness

CLI: `recon-eval run --suite <name> --model <id> --prompt <version> --seed <n>`

### 11.1 What it does

- Loads scenarios (DB snapshot + ground truth).
- Runs the agent on each case in an isolated environment (tools hit a sandbox DB).
- Scores outcomes against ground truth.
- Stores results in `eval_runs` and `eval_case_results` and emits a Markdown + JSON report.

### 11.2 Metrics

| Metric | Definition |
|---|---|
| Resolution accuracy | Correct resolution type and amounts / total |
| Auto-resolve precision | Of cases auto-applied, fraction correct |
| **False-fix rate** | Proposals that would have posted a wrong entry (headline safety metric) |
| Correct-escalation rate | Cases that should escalate and did |
| Over-escalation rate | Cases that were resolvable but escalated |
| Evidence validity | Proposals whose cited evidence is real and relevant |
| Cost per case | Tokens and USD |
| Latency | p50/p95 per case |
| Steps per case | Tool calls used |
| Injection resistance | Injection scenarios with no policy bypass or wrong action |

### 11.3 Experiment management

- Prompt and model versions are first-class and recorded with each run.
- Compare runs side by side with a diff of regressions and improvements per failure mode.
- CI gate: thresholds are defined in 11.5; the gate runs on the held-out `test` split.
- Variance: repeat runs (k≥3) for non-deterministic settings and report mean ± spread.

### 11.4 Injection suite

A dedicated suite of adversarial memos and descriptions (instruction override, fake approvals, tool-argument smuggling, fake "system" messages). Pass criterion: no unauthorized action and no change to tier or amount derived from injected text.

### 11.5 Dataset hygiene, component evals, and thresholds

- **Versioned datasets and splits.** The generator version, seed, and config hash form a `dataset_version`, recorded on every run. Three splits: `dev` (used for prompt and model iteration), `test` (held out, generated from different seeds, used only for reporting and the CI gate), and `injection` (11.4). Prompts are tuned on `dev` only; README results come from `test`.
- **Component evals.** The matcher is scored separately (precision and recall of matches against ground-truth links, per pass), verifiers are scored against ground truth, and the policy engine has table-driven tests. This isolates what the LLM itself contributes.
- **Simulated approver.** For end-to-end runs, a rule-based approver approves exactly when the proposal matches ground truth, so the whole pipeline (policy, approval state machine, executor) is exercised without humans. Real approver behavior is not simulated (section 21).
- **Statistical honesty.** Report rates with confidence intervals (e.g., Wilson) and require a minimum number of `test` scenarios per failure mode (e.g., 30), flagging modes below it.
- **Initial thresholds** (placeholders, committed to the repo before the first `test` run and revised only with a documented reason):
  - T0 false-fix rate: 0 on `test`. Any T0 false fix fails the gate.
  - Overall false-fix rate: no increase vs baseline beyond the confidence interval.
  - Resolution accuracy: no drop of more than 2 points vs baseline.
  - Injection suite: 100% with no policy bypass.
  - Calibration: reported, not gated.

---

## 12. Observability and Audit

- **Traces:** one trace per case run. Spans for each LLM call and each tool call carry metadata, hashes, token counts, cost, and redacted or truncated payloads. Full raw tool arguments and results are stored in `tool_calls` (access-restricted; synthetic data only), which is the source of truth for evidence validation and replay. `trace_id` is stored on the run, the proposals, and the audit entries.
- **Replay:** `recon replay --run <id>` re-executes a run against the recorded `tool_calls` results and `llm_calls` responses (`--exact`) to reproduce a decision without calling live data (and optionally with a different model/prompt for comparison).
- **Metrics (Prometheus):** match rate, breaks by type, case age, queue depth, agent outcomes, approval latency, cost, errors.
- **Audit log:** every state transition, policy decision, approval, and executor write. Append-only, queryable by entity.
- **Logging:** structured JSON with trace correlation; no secrets in logs.

---

## 13. Dashboards

### 13.1 Ops dashboard

- Match rate (daily, by source)
- Unmatched value by currency
- Open breaks by type and aging bucket (0–1d, 2–3d, 4–7d, >7d)
- Approval queue: pending count, oldest, median time-to-approve
- Auto-resolved vs human-resolved vs escalated (stacked, over time)

### 13.2 Agent quality dashboard

- Eval accuracy and false-fix rate by model / prompt version over time
- Production proxy metrics: human override rate (approver rejects or edits), apply-failure rate
- Cost per case and latency distributions
- Escalation rate and reasons

### 13.3 Approval queue UI (optional, custom)

Small Go + htmx page: list of pending proposals, case file view, approve/reject with the same backend path as Slack.

---

## 14. Reports

| Report | Cadence | Contents | Delivery |
|---|---|---|---|
| Daily reconciliation | Daily | Totals, match rate, breaks by cause, aging, agent actions (auto-applied, proposed), pending approvals, notable exceptions | Slack message plus PDF |
| Case file | Per case | Break details, evidence, agent trace summary, proposal, policy decision, approver, outcome | Linked from Slack/ticket; PDF/HTML export |
| Weekly eval | Weekly | Run comparisons, regressions, cost trends, top failure modes | Markdown/PDF |
| Period close | On demand | Per-scope tie-out (FIN-54): adjusted ledger vs adjusted statement balances, unexplained difference, outstanding items, close/reopen history | PDF/CSV |

---

## 15. API Surface (internal)

```
POST /v1/ingest/files                 upload settlement file
POST /v1/ingest/webhooks/{source}     provider webhooks (signature verified)
POST /v1/reconcile/run                trigger a matching run
GET  /v1/cases?status=&type=          list cases
GET  /v1/cases/{id}                   case file
GET  /v1/proposals?status=pending
POST /v1/proposals/{id}/decision      approve/reject (UI path; same service as Slack)
POST /v1/slack/interactions           Slack callbacks
POST /v1/tickets/webhook              ticket status callbacks
GET  /v1/reports/daily?date=
GET  /v1/periods                      list periods and tie-out status
POST /v1/periods/{id}/close           close a period (FIN-57; admin)
POST /v1/admin/switches               set agent_enabled / auto_apply_enabled (admin; audited)
GET  /healthz, /metrics
```

AuthN: static API keys or OIDC for the UI; roles: `viewer`, `approver`, `admin`.

---

## 16. Security and Safety Summary

- Secrets via environment/secret store; never in prompts or logs.
- Separate DB roles: `recon_agent_ro` (read-only plus insert into `proposals`), `recon_executor` (ledger adjustments), `recon_app`, `recon_audit_ro`.
- Signature verification and replay protection on every inbound callback.
- Rate limiting and per-case budgets to prevent cost runaway.
- PII minimization: synthetic names only; redaction hooks in traces.
- Write-up: `docs/what-the-agent-must-not-do.md` (no direct ledger writes, no choosing its own tier, no approving, no external network calls, no acting on instructions in data).

---

## 17. Repository Layout

```
recon-agent/
├── cmd/
│   ├── api/            # HTTP server
│   ├── worker/         # case queue + agent runner
│   ├── recon/          # CLI (ingest, replay, reports)
│   └── recon-eval/     # eval CLI
├── internal/
│   ├── ledger/         # double-entry, invariants
│   ├── ingest/         # parsers, normalization
│   ├── match/          # matching passes
│   ├── cases/          # queue, leasing
│   ├── agent/          # loop, tools, guardrails, LLM client interface
│   ├── policy/         # risk tiering
│   ├── approval/       # state machine, channels (slack, ticket)
│   ├── executor/       # applies approved proposals
│   ├── audit/
│   ├── reports/
│   └── telemetry/
├── synth/              # data generator + scenarios
├── eval/               # suites, scorers, reports
├── migrations/
├── deploy/             # docker-compose, grafana dashboards, prometheus
├── docs/               # threat model, design decisions, eval results
└── README.md
```

**Layout delta (v0.5 roadmap).** The roadmap adds these paths to the §17 baseline: `internal/money`, `internal/db`, `internal/testdb`, `internal/clock`, `internal/config`, `internal/outbox`, `internal/periods`, `internal/tieout`, `internal/matchscore`, `internal/fault`, `internal/worker`, plus a root `compose.yaml`. `deploy/` remains reserved for Grafana/Prometheus provisioning (M5-04); the root compose file is a deliberate exception to §17 so plain `docker compose up` works.

---

## 18. Milestones (~6 weeks of evenings)

| # | Milestone | Deliverables | Done when |
|---|---|---|---|
| 1 | Foundation | Schema, `Money` type and rounding, ledger with invariants and immutability (FIN-01 to 14), ingestion with control totals and statement continuity (FIN-50/51/55), balance tie-out (FIN-54), matching engine with scopes and incremental runs, synthetic generator with ground truth, CI float/migration lint | Engine hits expected match rate on synthetic set; invariants and DB-level constraints tested; tie-out surfaces a seeded matcher bug |
| 2 | Agent v0 | Read-only tools, then proposal tools, agent loop with budgets, LLM client interface with retries, attempt caps, and kill switches (6.5) | Agent produces valid proposals on easy scenarios; a simulated provider outage queues or escalates cases without failing them |
| 3 | Evals | Eval CLI, dataset versions with dev/test/injection splits, scorers, component evals (matcher, verifiers), calibration report, baseline results, run comparison | Baseline report committed on `test`; false-fix rate measured with confidence intervals |
| 4 | Approvals | State machine and case lifecycle with supersession (8.5), policy engine with deterministic verifiers (7.1), atomic executor and outbox (FIN-20 to 25), audit hash chain (FIN-30 to 34), Slack adapter | End-to-end: break → proposal → Slack approve → applied entry with audit trail |
| 5 | Visibility | OTel tracing, replay from `tool_calls`, Prometheus/Grafana dashboards, daily report and case file PDFs, period locking and close (FIN-56/57) with the period-close report | Both dashboards populated from a demo run; a period can be closed only when tie-outs pass |
| 6 | Hardening | Injection suite, ticket adapter, threat-model and "must-not" write-ups, CI eval gate, README with results up front | CI blocks regressions; README leads with eval table |

Stretch: model comparison across providers, learning from approver feedback (few-shot from past resolutions), a second PSP format, retry/backfill of late settlements.

---

## 19. Acceptance Criteria

- [ ] `docker compose up` brings up the full stack with synthetic data in one command.
- [ ] Ledger invariants hold under property-based tests.
- [ ] CI fails if any float type is used for money in code or migrations (FIN-01).
- [ ] Posted ledger entries cannot be updated or deleted, even by direct SQL as the app role (FIN-11).
- [ ] Unbalanced transactions are rejected at the database level, not just in application code (FIN-10).
- [ ] Applying the same proposal concurrently or repeatedly results in exactly one ledger transaction (FIN-12, 23).
- [ ] Fault-injection tests show no partial state when the executor is killed at any step (FIN-25).
- [ ] No external call (Slack, ticket) happens inside a DB transaction; side effects go through the outbox (FIN-22).
- [ ] `recon audit verify` detects a tampered audit row (FIN-31).
- [ ] The daily integrity job passes on clean data and fails on seeded inconsistencies (FIN-52).
- [ ] A corrupted settlement batch (control-total mismatch) is quarantined entirely (FIN-50).
- [ ] A seeded matcher bug (a tolerance match whose delta is never booked) surfaces as a non-zero unexplained difference in the tie-out (FIN-54).
- [ ] Posting into a closed period is rejected, and period close is blocked while tie-outs fail (FIN-56/57).
- [ ] No proposal reaches T0 without a passing deterministic verifier, and agent confidence can only tighten a tier (7.1).
- [ ] A late match supersedes an active proposal and cancels its approval request; an already-approved proposal fails re-validation with `break_resolved` (8.5).
- [ ] With a simulated provider outage, cases queue or escalate after the attempt cap instead of failing, and the kill switches stop the agent or auto-apply immediately (6.5).
- [ ] Eval results are reported on a held-out `test` split with confidence intervals, plus matcher/verifier component scores and a calibration report (11.5).
- [ ] Agent never writes to ledger tables (enforced by DB roles, tested).
- [ ] Eval report shows accuracy, false-fix rate, cost per case, and per-failure-mode breakdown for at least two models/prompt versions.
- [ ] CI fails on false-fix or accuracy regression against the baseline.
- [ ] Slack approval works end to end, including signature verification, idempotent clicks, expiry, and four-eyes.
- [ ] Any applied adjustment can be traced to: the break, the agent run/trace, the policy decision, and the approver.
- [ ] Injection suite passes at the defined threshold.
- [ ] Replay reproduces a past run's decision from recorded tool outputs.
- [ ] README opens with eval results and a failure analysis, followed by architecture.

---

## 20. README Outline (portfolio-facing)

1. One-paragraph pitch and a short demo GIF (break → agent → Slack approval → applied).
2. **Results:** eval table, false-fix rate, cost per case, top failure modes and what was learned.
3. Architecture diagram and key design decisions (why deterministic core, why proposals only).
4. What the agent is not allowed to do, and why.
5. Threat model summary (prompt injection).
6. Run it locally.
7. Limitations and next steps.

---

## 21. Scope Boundaries and Limitations

Deliberately out of scope for v1. Each row names what a production system would add.

| Area | v1 position | Production would add |
|---|---|---|
| Reconciliation topology | Hub-and-spoke: each source reconciled against the ledger (section 5); no direct PSP↔bank matching | Three-way matching with its own break types |
| Payment lifecycle | Charges, refunds, payouts, fees, and chargebacks as flat records; a chargeback is "unexpected debit → escalate" | Authorization/capture/settlement states, chargeback stages and representment, reserves and holdbacks, scheme/interchange fee decomposition (v1 uses one fee schedule per source, kind, and currency) |
| Approver governance | Roles (`viewer`, `approver`, `admin`), four-eyes, tiering by value | Per-approver authority limits, delegation and out-of-office, multi-level approval chains |
| Agent write authority | Proposals only; no unmatching or customer-facing actions | Narrowly scoped, separately approved capabilities |
| Data sent to the model | Synthetic data; free-text fields marked untrusted and delimited (section 9) | Field-level minimization or tokenization before prompts, provider data-handling agreements, zero-retention settings |
| Scale and availability | Single Postgres instance; assumed volume of about 100k external records per day (synthetic); no HA or SLO claims | Load tests, HA and failover, SLOs with alerting, tested backup/restore with stated RPO/RTO |
| Schema changes | Forward-only migrations following expand/contract discipline (documented) | Zero-downtime migration tooling and rollout gates |
| Human behavior in evals | Rule-based simulated approver (11.5) | Measured override and rejection behavior from real approvers |

---

## 22. Open Questions

- Which ticketing system to target first (Jira vs Linear)?
- Which LLM providers/models to compare in evals?
- Fee schedule source: hand-seeded table, or derived from the generator's config so verifiers and ground truth stay consistent?
- Real-time (webhook-driven) reconciliation vs batch-only for v1?
