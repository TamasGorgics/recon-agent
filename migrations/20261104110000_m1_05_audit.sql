-- +goose Up
-- M1-05: hash-chained, append-only audit log (spec section 3.1; FIN-30, FIN-31,
-- FIN-33).
--
-- audit_log rows are immutable: the triggers reject UPDATE/DELETE/TRUNCATE even
-- for the maintenance/superuser role, so the append-only rule is enforced by
-- the database and not only by the role grants (FIN-31).
--
-- The hash chain covers seq, ts, the typed actor columns, action, entity
-- columns, before/after, trace_id, and prev_hash. The application computes
-- SHA-256 over canonical JSON (sorted keys, no whitespace) inside the caller's
-- transaction; see internal/audit. The first row links to 32 zero bytes.
--
-- audit_chain_head mirrors the last (seq, row_hash) pair in the same
-- transaction. A hash chain alone cannot distinguish a truncated tail from a
-- complete chain, so Verify uses this append-only mirror to detect tail
-- deletion. It is part of M1-05's audit objects (recorded as a schema delta in
-- docs/open-questions.md).

CREATE TABLE audit_log (
    seq         BIGINT GENERATED ALWAYS AS IDENTITY,
    id          UUID NOT NULL DEFAULT gen_random_uuid(),
    ts          TIMESTAMPTZ NOT NULL,
    actor_type  TEXT NOT NULL CHECK (actor_type IN ('system', 'agent', 'human')),
    actor_id    TEXT NOT NULL CHECK (actor_id <> ''),
    action      TEXT NOT NULL CHECK (action <> ''),
    entity_type TEXT NOT NULL CHECK (entity_type <> ''),
    entity_id   TEXT NOT NULL CHECK (entity_id <> ''),
    before      JSONB,
    after       JSONB,
    trace_id    TEXT,
    prev_hash   BYTEA NOT NULL CHECK (octet_length(prev_hash) = 32),
    row_hash    BYTEA NOT NULL CHECK (octet_length(row_hash) = 32),
    PRIMARY KEY (id),
    UNIQUE (seq)
);

CREATE TABLE audit_chain_head (
    seq      BIGINT PRIMARY KEY,
    row_hash BYTEA NOT NULL CHECK (octet_length(row_hash) = 32)
);

-- FIN-31: the audit trail is append-only at the database level.
-- +goose StatementBegin
CREATE FUNCTION audit_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is not allowed on %: the audit log is append-only (FIN-31)',
        TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_log_reject_update
    BEFORE UPDATE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_reject_mutation();

CREATE TRIGGER audit_log_reject_delete
    BEFORE DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_reject_mutation();

CREATE TRIGGER audit_log_reject_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_reject_mutation();

CREATE TRIGGER audit_chain_head_reject_update
    BEFORE UPDATE ON audit_chain_head
    FOR EACH ROW EXECUTE FUNCTION audit_reject_mutation();

CREATE TRIGGER audit_chain_head_reject_delete
    BEFORE DELETE ON audit_chain_head
    FOR EACH ROW EXECUTE FUNCTION audit_reject_mutation();

CREATE TRIGGER audit_chain_head_reject_truncate
    BEFORE TRUNCATE ON audit_chain_head
    FOR EACH STATEMENT EXECUTE FUNCTION audit_reject_mutation();

-- D-6 grant matrix. recon_app and recon_executor append and read; recon_audit_ro
-- reads (including the chain head needed by Verify); recon_agent_ro has no
-- access at all. No role receives UPDATE or DELETE.
GRANT SELECT, INSERT ON audit_log TO recon_app, recon_executor;
GRANT SELECT ON audit_log TO recon_audit_ro;
GRANT SELECT, INSERT ON audit_chain_head TO recon_app, recon_executor;
GRANT SELECT ON audit_chain_head TO recon_audit_ro;

-- The application inserts explicit contiguous seq values with OVERRIDING
-- SYSTEM VALUE (so a caller rollback cannot leave a chain gap), but the
-- identity sequence still exists and the posting roles are granted access to
-- it as required by the M1-05 contract.
-- +goose StatementBegin
DO $$
DECLARE
    identity_sequence TEXT;
BEGIN
    identity_sequence := pg_get_serial_sequence('audit_log', 'seq');
    IF identity_sequence IS NULL THEN
        RAISE EXCEPTION 'audit_log.seq has no identity sequence';
    END IF;
    EXECUTE format(
        'GRANT USAGE, SELECT ON SEQUENCE %s TO recon_app, recon_executor',
        identity_sequence);
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS audit_chain_head;
DROP TABLE IF EXISTS audit_log;
DROP FUNCTION IF EXISTS audit_reject_mutation();
