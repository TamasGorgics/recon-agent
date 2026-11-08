-- +goose Up
-- M3-02: isolated eval ground truth (docs/ROADMAP.md M3-02, spec section 11.1).
--
-- eval_truth is migration-owned and present in every clone built from the eval
-- template. It holds the generated scenario truth plus the scenario -> case
-- mapping. The agent role must never be able to read it: PUBLIC is revoked and
-- recon_app, recon_agent_ro, recon_audit_ro, and recon_executor receive no
-- schema or table privileges. The sandbox loader and the trusted eval runner
-- use the maintenance/admin pool only.
--
-- This schema is a documented spec section 3.1 delta recorded in
-- docs/open-questions.md (Q-50). Scenario truth stays the versioned
-- eval/dataset.ScenarioTruth JSON document; the typed columns are the stable
-- query surface.

CREATE SCHEMA IF NOT EXISTS eval_truth;

REVOKE ALL ON SCHEMA eval_truth FROM PUBLIC;

CREATE TABLE eval_truth.sandbox_meta (
    id                    INT PRIMARY KEY CHECK (id = 1),
    eval_dataset_version  TEXT NOT NULL CHECK (eval_dataset_version <> ''),
    split                 TEXT NOT NULL CHECK (split IN ('dev', 'test', 'injection')),
    seed                  BIGINT NOT NULL,
    world_clock           TIMESTAMPTZ NOT NULL,
    loaded_at             TIMESTAMPTZ NOT NULL
);

CREATE TABLE eval_truth.scenarios (
    scenario_id           TEXT PRIMARY KEY CHECK (scenario_id <> ''),
    split                 TEXT NOT NULL CHECK (split IN ('dev', 'test', 'injection')),
    mode                  TEXT NOT NULL CHECK (mode <> ''),
    difficulty            TEXT NOT NULL CHECK (difficulty <> ''),
    break_signature       TEXT NOT NULL DEFAULT '',
    truth_schema_version  TEXT NOT NULL CHECK (truth_schema_version <> ''),
    truth                 JSONB NOT NULL CHECK (jsonb_typeof(truth) = 'object'),
    loaded_at             TIMESTAMPTZ NOT NULL
);

CREATE TABLE eval_truth.scenario_cases (
    scenario_id           TEXT PRIMARY KEY
                          REFERENCES eval_truth.scenarios (scenario_id) ON DELETE CASCADE,
    case_id               UUID NOT NULL,
    break_id              UUID NULL,
    synthetic             BOOLEAN NOT NULL DEFAULT FALSE,
    scenario_time         TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL
);

CREATE INDEX eval_truth_scenario_cases_case_idx
    ON eval_truth.scenario_cases (case_id);

REVOKE ALL ON ALL TABLES IN SCHEMA eval_truth FROM PUBLIC;

-- Deliberately no grants to recon_app, recon_agent_ro, recon_audit_ro, or
-- recon_executor, and no ALTER DEFAULT PRIVILEGES entry.

-- +goose Down
DROP TABLE IF EXISTS eval_truth.scenario_cases;
DROP TABLE IF EXISTS eval_truth.scenarios;
DROP TABLE IF EXISTS eval_truth.sandbox_meta;
DROP SCHEMA IF EXISTS eval_truth;
