-- +goose Up
-- M3-01: eval runs and per-case results (spec section 3.1, section 11.5).
--
-- This is the only M3-01 migration. The eval truth database schema is M3-02's
-- and is deliberately absent here: M3-01 owns the public eval tables and the
-- versioned JSON ground-truth schema only.
--
-- Documented spec section 3.1 deltas (recorded in docs/open-questions.md):
--   * eval_runs.repeat_group/repeat_index are added now for M3-04 repeats.
--   * eval_case_results has no scenario foreign key because scenario truth lives
--     in the M3-02 eval_truth schema, not in the public schema.
--   * eval_runs.started_at has no default; the runner supplies the timestamp.
--
-- Privileges: REVOKE PUBLIC first, then the exact matrix. recon_app records eval
-- runs and results; recon_agent_ro, recon_audit_ro, and recon_executor get no
-- eval privileges. No dashboard grant is added here.

CREATE TABLE eval_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    suite           TEXT NOT NULL CHECK (suite <> ''),
    split           TEXT NOT NULL CHECK (split IN ('dev', 'test', 'injection')),
    dataset_version TEXT NOT NULL CHECK (dataset_version <> ''),
    model           TEXT NOT NULL CHECK (model <> ''),
    prompt_version  TEXT NOT NULL CHECK (prompt_version <> ''),
    seed            BIGINT NOT NULL,
    git_sha         TEXT NULL,
    config          JSONB NOT NULL DEFAULT '{}'::jsonb
                    CHECK (jsonb_typeof(config) = 'object'),
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ NULL,
    -- M3-04 resume/repeat contract: repeats are separate runs linked by a
    -- group id, and repeat_index is only meaningful inside a group.
    repeat_group    UUID NULL,
    repeat_index    INT NULL CHECK (repeat_index IS NULL OR repeat_index >= 0),
    CHECK (ended_at IS NULL OR ended_at >= started_at),
    CHECK (repeat_group IS NOT NULL OR repeat_index IS NULL)
);

CREATE INDEX eval_runs_split_started_idx
    ON eval_runs (split, started_at DESC);
CREATE INDEX eval_runs_dataset_version_idx
    ON eval_runs (dataset_version);
CREATE INDEX eval_runs_model_prompt_started_idx
    ON eval_runs (model, prompt_version, started_at DESC);
CREATE INDEX eval_runs_repeat_group_idx
    ON eval_runs (repeat_group)
    WHERE repeat_group IS NOT NULL;

CREATE TABLE eval_case_results (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    eval_run_id   UUID NOT NULL REFERENCES eval_runs (id) ON DELETE RESTRICT,
    scenario_id   TEXT NOT NULL CHECK (scenario_id <> ''),
    -- failure_mode is generator-owned; intentionally not a database enum.
    failure_mode  TEXT NOT NULL CHECK (failure_mode <> ''),
    correct       BOOLEAN NOT NULL,
    false_fix     BOOLEAN NOT NULL,
    escalated     BOOLEAN NOT NULL,
    tier_expected TEXT NULL
                  CHECK (tier_expected IS NULL OR tier_expected IN ('T0', 'T1', 'T2')),
    tier_actual   TEXT NULL
                  CHECK (tier_actual IS NULL OR tier_actual IN ('T0', 'T1', 'T2')),
    confidence    NUMERIC(5, 4) NULL
                  CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1)),
    cost_usd      NUMERIC(20, 6) NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    latency_ms    INT NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
    steps         INT NOT NULL DEFAULT 0 CHECK (steps >= 0),
    detail        JSONB NOT NULL DEFAULT '{}'::jsonb
                  CHECK (jsonb_typeof(detail) = 'object'),
    -- M3-04 resume/idempotency key: one result per scenario per run.
    CONSTRAINT eval_case_results_run_scenario_key
        UNIQUE (eval_run_id, scenario_id)
);

CREATE INDEX eval_case_results_run_mode_idx
    ON eval_case_results (eval_run_id, failure_mode);
CREATE INDEX eval_case_results_run_correct_idx
    ON eval_case_results (eval_run_id, correct);
CREATE INDEX eval_case_results_scenario_idx
    ON eval_case_results (scenario_id);

REVOKE ALL ON TABLE eval_runs, eval_case_results FROM PUBLIC;

GRANT SELECT, INSERT, UPDATE ON eval_runs TO recon_app;
GRANT SELECT, INSERT ON eval_case_results TO recon_app;

-- +goose Down
DROP INDEX IF EXISTS eval_case_results_scenario_idx;
DROP INDEX IF EXISTS eval_case_results_run_correct_idx;
DROP INDEX IF EXISTS eval_case_results_run_mode_idx;

DROP INDEX IF EXISTS eval_runs_repeat_group_idx;
DROP INDEX IF EXISTS eval_runs_model_prompt_started_idx;
DROP INDEX IF EXISTS eval_runs_dataset_version_idx;
DROP INDEX IF EXISTS eval_runs_split_started_idx;

REVOKE ALL ON TABLE eval_runs, eval_case_results
    FROM recon_app, recon_agent_ro, recon_audit_ro, recon_executor;

DROP TABLE IF EXISTS eval_case_results;
DROP TABLE IF EXISTS eval_runs;
