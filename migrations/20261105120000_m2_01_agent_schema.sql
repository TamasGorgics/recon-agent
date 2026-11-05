-- +goose Up
-- M2-01: agent runs, tool/LLM trace, proposals, read views, grants, switches.
--
-- Contract: M2 piece 2 spec section 3 (roadmap M2-01; docs/spec.md sections
-- 3.1/3.2 and 16). This is the only migration in the M2 piece.
--
-- Grants and ownership (ADR 0005): "owns" in the roadmap means the privilege
-- matrix below, not ALTER TABLE ... OWNER. Every table and view stays owned by
-- the migration role; recon_app receives write grants for the run trace.
--
-- View boundary: all v_agent_* views are explicit-column, owner-rights views
-- (no security_invoker = true). The view owner can read the base tables, while
-- recon_agent_ro receives SELECT only on the views and no base-table grant.
--
-- M1-07 table exception: external_records_agent_search_idx is an M2-01 read
-- index added to the M1-07-owned external_records table; M1-07 still owns the
-- table. v_agent_history follows the M1-14 breaks/cases shape recorded in
-- docs/open-questions.md Q-08 (signature plus case_breaks, not the spec's
-- singular break columns).
--
-- The switch seed is the only idempotent statement; tables, indexes, and views
-- use normal CREATE. agent_runs intentionally has no audit trigger.

CREATE TABLE agent_runs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id        UUID NOT NULL REFERENCES cases (id) ON DELETE RESTRICT,
    model          TEXT NOT NULL CHECK (model <> ''),
    prompt_version TEXT NOT NULL CHECK (prompt_version <> ''),
    started_at     TIMESTAMPTZ NOT NULL,
    ended_at       TIMESTAMPTZ NULL,
    tokens_in      BIGINT NOT NULL DEFAULT 0 CHECK (tokens_in >= 0),
    tokens_out     BIGINT NOT NULL DEFAULT 0 CHECK (tokens_out >= 0),
    cost_usd       NUMERIC(20, 6) NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    trace_id       TEXT NULL,
    outcome        TEXT NULL CHECK (outcome IS NULL OR outcome IN (
                       'proposal', 'escalated', 'budget_exhausted',
                       'invalid_output', 'provider_error'
                   )),
    -- Q-C: outcome and ended_at are finalised together when the run ends.
    CHECK (ended_at IS NULL OR ended_at >= started_at),
    CHECK (ended_at IS NULL OR outcome IS NOT NULL)
);

CREATE INDEX agent_runs_case_started_idx ON agent_runs (case_id, started_at);

CREATE TABLE tool_calls (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id UUID NOT NULL REFERENCES agent_runs (id) ON DELETE RESTRICT,
    seq          INT NOT NULL CHECK (seq > 0),
    tool_name    TEXT NOT NULL CHECK (tool_name <> ''),
    args         JSONB NOT NULL,
    result       JSONB NOT NULL,
    result_hash  TEXT NOT NULL CHECK (result_hash <> ''),
    error        TEXT NULL CHECK (error IS NULL OR error <> ''),
    started_at   TIMESTAMPTZ NOT NULL,
    duration_ms  INT NOT NULL CHECK (duration_ms >= 0),
    CONSTRAINT tool_calls_run_seq_key UNIQUE (agent_run_id, seq)
);

CREATE INDEX tool_calls_agent_run_id_idx ON tool_calls (agent_run_id);

CREATE TABLE llm_calls (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id UUID NOT NULL REFERENCES agent_runs (id) ON DELETE RESTRICT,
    seq          INT NOT NULL CHECK (seq > 0),
    model        TEXT NOT NULL CHECK (model <> ''),
    request_hash TEXT NOT NULL CHECK (request_hash <> ''),
    response     JSONB NOT NULL,
    usage        JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_at   TIMESTAMPTZ NOT NULL,
    duration_ms  INT NOT NULL CHECK (duration_ms >= 0),
    CONSTRAINT llm_calls_run_seq_key UNIQUE (agent_run_id, seq)
);

CREATE INDEX llm_calls_agent_run_id_idx ON llm_calls (agent_run_id);

CREATE TABLE proposals (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id               UUID NOT NULL REFERENCES cases (id) ON DELETE RESTRICT,
    agent_run_id          UUID NOT NULL REFERENCES agent_runs (id) ON DELETE RESTRICT,
    type                  TEXT NOT NULL CHECK (type IN (
                              'link', 'adjustment', 'write_off', 'duplicate',
                              'request_info'
                          )),
    payload               JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    rationale             TEXT NOT NULL CHECK (rationale <> ''),
    evidence              JSONB NOT NULL DEFAULT '[]'::jsonb
                          CHECK (jsonb_typeof(evidence) = 'array'),
    confidence            NUMERIC(5, 4) NOT NULL
                          CHECK (confidence >= 0 AND confidence <= 1),
    -- pattern_claimed is untrusted; only pattern_verified may drive policy.
    pattern_claimed       TEXT NULL CHECK (pattern_claimed IS NULL OR pattern_claimed IN (
                              'fee_variance', 'fx_rounding', 'rounding',
                              'reference_typo', 'other'
                          )),
    pattern_verified      TEXT NULL CHECK (pattern_verified IS NULL OR pattern_verified IN (
                              'fee_variance', 'fx_rounding', 'rounding',
                              'reference_typo', 'other'
                          )),
    verification          JSONB NULL,
    risk_tier             TEXT NULL CHECK (risk_tier IS NULL OR risk_tier IN (
                              'T0', 'T1', 'T2'
                          )),
    policy_decision       JSONB NULL,
    status                TEXT NOT NULL DEFAULT 'proposed' CHECK (status IN (
                              'proposed', 'pending_approval', 'approved',
                              'auto_approved', 'applied', 'apply_failed',
                              'rejected', 'expired', 'superseded'
                          )),
    version               INT NOT NULL DEFAULT 0 CHECK (version >= 0),
    state_hash            TEXT NOT NULL CHECK (state_hash <> ''),
    idempotency_key       TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    supersedes_proposal_id UUID NULL REFERENCES proposals (id) ON DELETE RESTRICT,
    created_at            TIMESTAMPTZ NOT NULL,
    expires_at            TIMESTAMPTZ NULL
);

-- Spec section 3.2 invariant 7: at most one active proposal per case. The
-- predicate is exactly the set of non-terminal statuses; applied, apply_failed,
-- rejected, expired, and superseded are not active.
CREATE UNIQUE INDEX proposals_one_active_per_case
    ON proposals (case_id)
    WHERE status IN ('proposed', 'pending_approval', 'approved', 'auto_approved');
CREATE INDEX proposals_case_id_idx ON proposals (case_id);
CREATE INDEX proposals_agent_run_id_idx ON proposals (agent_run_id);

CREATE TABLE system_switches (
    name       TEXT PRIMARY KEY CHECK (name <> ''),
    enabled    BOOLEAN NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL CHECK (updated_by <> ''),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- M1-07 owns external_records; this is the M2-01 read index for the M2-05
-- search tool's currency/amount/value_date filter.
CREATE INDEX external_records_agent_search_idx
    ON external_records (currency, amount_minor, value_date);

-- Narrow, explicit-column agent read surface. No SELECT *; UUIDs are text,
-- money stays BIGINT, and the FX rate is cast to text so it can never become a
-- JSON float. Views use the default owner-rights behaviour.
CREATE VIEW v_agent_accounts AS
    SELECT id::text AS id, code, name, type, currency, status
      FROM accounts;

CREATE VIEW v_agent_scopes AS
    SELECT id::text AS id, ledger_account_id::text AS ledger_account_id,
           source, currency
      FROM reconciliation_scopes;

CREATE VIEW v_agent_cases AS
    SELECT id::text AS id, priority, value_minor, currency, status, version,
           attempts, escalation_reason, created_at, resolved_at, resolution_type
      FROM cases;

CREATE VIEW v_agent_case_breaks AS
    SELECT case_id::text AS case_id, break_id::text AS break_id, created_at
      FROM case_breaks;

CREATE VIEW v_agent_breaks AS
    SELECT id::text AS id, signature, scope_id::text AS scope_id, side,
           ledger_txn_ids::text[] AS ledger_txn_ids, external_refs,
           delta_minor, currency, status, detected_at
      FROM breaks;

CREATE VIEW v_agent_ledger_txns AS
    SELECT id::text AS id, kind, value_date, created_at, booked_at, description,
           period_id::text AS period_id,
           adjusts_period_id::text AS adjusts_period_id,
           reversal_of::text AS reversal_of
      FROM ledger_txns;

CREATE VIEW v_agent_ledger_entries AS
    SELECT e.id::text AS id, e.txn_id::text AS txn_id,
           e.account_id::text AS account_id, a.code AS account_code,
           a.name AS account_name, e.direction, e.amount_minor, e.currency,
           e.posted_at, e.external_ref
      FROM ledger_entries e
      JOIN accounts a ON a.id = e.account_id;

CREATE VIEW v_agent_external_records AS
    SELECT id::text AS id, source, source_record_id, kind, amount_minor,
           currency, fee_minor, occurred_at, settled_at, value_date,
           batch_id::text AS batch_id, raw, raw_hash
      FROM external_records;

CREATE VIEW v_agent_settlement_batches AS
    SELECT id::text AS id, source, period_start, period_end,
           declared_total_minor, currency, status, quarantine_reason,
           row_count, declared_row_count
      FROM settlement_batches;

CREATE VIEW v_agent_statement_balances AS
    SELECT id::text AS id, scope_id::text AS scope_id, as_of_date,
           opening_minor, closing_minor, currency, batch_id::text AS batch_id
      FROM statement_balances;

CREATE VIEW v_agent_fx_rates AS
    SELECT id::text AS id, base, quote, rate::text AS rate, as_of, source
      FROM fx_rates;

CREATE VIEW v_agent_fee_schedules AS
    SELECT id::text AS id, source, kind, currency, pct_bps, fixed_minor,
           valid_from, valid_to
      FROM fee_schedules;

-- Basis for M2-06's structural history query. M1's case_breaks join replaces
-- the spec's singular cases.break_ids; only resolved cases appear.
CREATE VIEW v_agent_history AS
    SELECT c.id::text AS case_id, c.value_minor AS case_value_minor,
           c.currency AS case_currency, c.resolution_type, c.resolved_at,
           b.id::text AS break_id, b.signature, b.scope_id::text AS scope_id,
           b.side, b.delta_minor, b.currency AS break_currency, b.detected_at
      FROM cases c
      JOIN case_breaks cb ON cb.case_id = c.id
      JOIN breaks b ON b.id = cb.break_id
     WHERE c.status = 'resolved';

INSERT INTO system_switches (name, enabled, reason, updated_by, updated_at)
VALUES
    ('agent_enabled', true, 'seed', 'system:migration', now()),
    ('auto_apply_enabled', false, 'seed', 'system:migration', now())
ON CONFLICT (name) DO NOTHING;

-- Privilege boundary: revoke PUBLIC first on every new object, then apply the
-- exact matrix. recon_agent_ro gets view reads plus INSERT on proposals only;
-- recon_executor and recon_audit_ro get no M2 grants. No M1 base-table grant
-- changes here.
REVOKE ALL ON TABLE agent_runs, tool_calls, llm_calls, proposals, system_switches
    FROM PUBLIC;
REVOKE ALL ON TABLE
    v_agent_accounts, v_agent_scopes, v_agent_cases, v_agent_case_breaks,
    v_agent_breaks, v_agent_ledger_txns, v_agent_ledger_entries,
    v_agent_external_records, v_agent_settlement_batches,
    v_agent_statement_balances, v_agent_fx_rates, v_agent_fee_schedules,
    v_agent_history
    FROM PUBLIC;

GRANT SELECT, INSERT, UPDATE ON agent_runs TO recon_app;
GRANT SELECT, INSERT ON tool_calls, llm_calls TO recon_app;
GRANT SELECT ON proposals TO recon_app;
GRANT SELECT, UPDATE ON system_switches TO recon_app;

GRANT INSERT ON proposals TO recon_agent_ro;
GRANT SELECT ON
    v_agent_accounts, v_agent_scopes, v_agent_cases, v_agent_case_breaks,
    v_agent_breaks, v_agent_ledger_txns, v_agent_ledger_entries,
    v_agent_external_records, v_agent_settlement_batches,
    v_agent_statement_balances, v_agent_fx_rates, v_agent_fee_schedules,
    v_agent_history
    TO recon_agent_ro;

-- +goose Down
DROP VIEW IF EXISTS
    v_agent_history, v_agent_fee_schedules, v_agent_fx_rates,
    v_agent_statement_balances, v_agent_settlement_batches,
    v_agent_external_records, v_agent_ledger_entries, v_agent_ledger_txns,
    v_agent_breaks, v_agent_case_breaks, v_agent_cases, v_agent_scopes,
    v_agent_accounts;

REVOKE ALL ON TABLE agent_runs, tool_calls, llm_calls, proposals, system_switches
    FROM recon_app, recon_agent_ro;

DROP INDEX IF EXISTS external_records_agent_search_idx;
DROP INDEX IF EXISTS proposals_one_active_per_case;
DROP INDEX IF EXISTS proposals_case_id_idx;
DROP INDEX IF EXISTS proposals_agent_run_id_idx;
DROP INDEX IF EXISTS llm_calls_agent_run_id_idx;
DROP INDEX IF EXISTS tool_calls_agent_run_id_idx;
DROP INDEX IF EXISTS agent_runs_case_started_idx;

DROP TABLE IF EXISTS tool_calls;
DROP TABLE IF EXISTS llm_calls;
DROP TABLE IF EXISTS proposals;
DROP TABLE IF EXISTS agent_runs;
DROP TABLE IF EXISTS system_switches;
