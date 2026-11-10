-- +goose Up
-- M4-01: approval, executor, and outbox schema (spec section 3.1; FIN-22,
-- FIN-23; roadmap M4-01).
--
-- Scope: approvers, approvals, applied_proposals, outbox, and
-- policy_config_versions, plus the minimum grants those tables need.
--
-- Ownership (ADR 0005): "owns" in the roadmap means the privilege matrix at the
-- bottom of this migration, not ALTER TABLE ... OWNER. Every new table stays
-- owned by the migration role; roles are M1-owned.
--
-- Outbox is created, not extended: the roadmap says "add next_attempt_at, dead,
-- idempotency_key", but no earlier migration created the table. This migration
-- creates the spec section 3.1 base columns plus those three extensions, the
-- partial due-claim index, and records the correction to Q-08 in Q-71.
--
-- Audit entries for apply/approval decisions are application-level writes in
-- M4-03/M4-06/M4-11, not database triggers here. No ledger grants change in
-- this migration; the existing executor adjustment path is unchanged.
--
-- Applied once per database; goose's version table is the re-run idempotency
-- mechanism (ADR 0002). Up deliberately has no CREATE ... IF NOT EXISTS.

CREATE TABLE approvers (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name   TEXT NOT NULL CHECK (display_name <> ''),
    role           TEXT NOT NULL CHECK (role IN ('viewer', 'approver', 'admin')),
    slack_user_id  TEXT UNIQUE CHECK (slack_user_id IS NULL OR slack_user_id <> ''),
    ticket_user_id TEXT UNIQUE CHECK (ticket_user_id IS NULL OR ticket_user_id <> ''),
    active         BOOLEAN NOT NULL DEFAULT true
);

CREATE TABLE approvals (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    proposal_id     UUID NOT NULL REFERENCES proposals (id) ON DELETE RESTRICT,
    channel         TEXT NOT NULL CHECK (channel <> ''),
    external_ref    TEXT CHECK (external_ref IS NULL OR external_ref <> ''),
    approver_id     UUID NOT NULL REFERENCES approvers (id) ON DELETE RESTRICT,
    decision        TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
    decided_at      TIMESTAMPTZ NOT NULL,
    signature_valid BOOLEAN NOT NULL DEFAULT false,
    CONSTRAINT approvals_proposal_approver_key UNIQUE (proposal_id, approver_id)
);

CREATE INDEX approvals_proposal_id_idx ON approvals (proposal_id);

CREATE TABLE applied_proposals (
    proposal_id     UUID PRIMARY KEY REFERENCES proposals (id) ON DELETE RESTRICT,
    ledger_txn_id   UUID NULL,
    applied_at      TIMESTAMPTZ NOT NULL,
    executor_run_id TEXT NOT NULL CHECK (executor_run_id <> ''),
    CONSTRAINT applied_proposals_ledger_txn_key UNIQUE (ledger_txn_id),
    CONSTRAINT applied_proposals_ledger_txn_fkey
        FOREIGN KEY (ledger_txn_id) REFERENCES ledger_txns (id) ON DELETE RESTRICT
);

CREATE TABLE outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  TEXT NOT NULL CHECK (aggregate_type <> ''),
    aggregate_id    TEXT NOT NULL CHECK (aggregate_id <> ''),
    topic           TEXT NOT NULL CHECK (topic <> ''),
    payload         JSONB NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    created_at      TIMESTAMPTZ NOT NULL,
    delivered_at    TIMESTAMPTZ NULL,
    attempts        INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    dead            BOOLEAN NOT NULL DEFAULT false,
    CONSTRAINT outbox_idempotency_key_key UNIQUE (idempotency_key),
    CHECK (NOT dead OR delivered_at IS NULL)
);

CREATE INDEX outbox_undelivered_idx
    ON outbox (next_attempt_at, id)
    WHERE delivered_at IS NULL AND dead = false;

CREATE TABLE policy_config_versions (
    hash      TEXT PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    yaml      TEXT NOT NULL CHECK (yaml <> ''),
    loaded_at TIMESTAMPTZ NOT NULL
);

-- Privilege boundary: revoke PUBLIC first on every new object, then apply the
-- exact matrix. recon_app owns the approval service, outbox worker, and policy
-- loader; recon_executor owns the atomic apply write path. recon_agent_ro and
-- recon_audit_ro gain nothing (spec section 16).
REVOKE ALL ON TABLE approvers, approvals, applied_proposals, outbox,
    policy_config_versions FROM PUBLIC;

-- recon_app: approval service, outbox worker, policy loader, trace reads.
-- Approvals are append-only: SELECT, INSERT only. Outbox supports enqueue,
-- claim, and delivery-column updates; no DELETE. Policy versions are
-- content-addressed and immutable. applied_proposals and approvers are reads.
-- No UPDATE on proposals: M4-03 owns the proposal state machine.
GRANT SELECT ON approvers TO recon_app;
GRANT SELECT, INSERT ON approvals TO recon_app;
GRANT SELECT, INSERT, UPDATE ON outbox TO recon_app;
GRANT SELECT, INSERT ON policy_config_versions TO recon_app;
GRANT SELECT ON applied_proposals TO recon_app;

-- recon_executor: atomic apply needs the proposal lock/state update, the
-- applied record, and an outbox row in one transaction. Column-level
-- UPDATE(status, version) is intentional: SELECT ... FOR UPDATE needs UPDATE on
-- at least one column, while payload/rationale/evidence stay unwritable.
-- Outbox INSERT is paired with column-level SELECT(idempotency_key) because
-- INSERT ... ON CONFLICT (idempotency_key) DO NOTHING requires SELECT on the
-- conflict-target column (Q-22). No RETURNING; generate the UUID in Go.
GRANT SELECT ON proposals TO recon_executor;
GRANT UPDATE (status, version) ON proposals TO recon_executor;
GRANT SELECT, INSERT ON applied_proposals TO recon_executor;
GRANT INSERT ON outbox TO recon_executor;
GRANT SELECT (idempotency_key) ON outbox TO recon_executor;

-- +goose Down
-- Revoke the new-table grants and the two new proposals grants before dropping.
-- REVOKE ALL ON TABLE does not remove column-level grants, so revoke the outbox
-- conflict-inference column explicitly.
REVOKE ALL ON TABLE approvers, approvals, applied_proposals, outbox,
    policy_config_versions FROM recon_app, recon_executor;
REVOKE SELECT ON proposals FROM recon_executor;
REVOKE UPDATE (status, version) ON proposals FROM recon_executor;
REVOKE SELECT (idempotency_key) ON outbox FROM recon_executor;

-- Child tables before approvers; proposals and ledger_txns are untouched.
DROP TABLE IF EXISTS applied_proposals;
DROP TABLE IF EXISTS approvals;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS policy_config_versions;
DROP TABLE IF EXISTS approvers;
