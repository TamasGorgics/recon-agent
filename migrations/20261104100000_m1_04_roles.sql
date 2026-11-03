-- +goose Up
-- M1-04: database roles and grants.
--
-- Four NOLOGIN group roles from spec section 16 / M1 spec D-6. Login users are
-- never created by a migration: integration tests provision ephemeral logins and
-- compose/initdb/01-roles.sql creates local-only development logins.
--
-- The migration must apply both to a plain Postgres service (CI) and to a
-- compose volume where the init script already created the groups, so role
-- creation is an idempotent DO block (D-1). Guard the table grants with the
-- same assumption: the M1-03 migration owns the tables.

-- +goose StatementBegin
DO $$
DECLARE
    group_name TEXT;
BEGIN
    FOR group_name IN
        SELECT unnest(ARRAY['recon_app', 'recon_executor', 'recon_agent_ro', 'recon_audit_ro'])
    LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = group_name) THEN
            -- Compose init may have created the group first. Keep the invariant
            -- that all four are NOLOGIN.
            EXECUTE format('ALTER ROLE %I NOLOGIN', group_name);
        ELSE
            EXECUTE format('CREATE ROLE %I NOLOGIN', group_name);
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO recon_app, recon_executor, recon_agent_ro, recon_audit_ro;

-- FIN-11: the app and executor roles may append ledger rows but never UPDATE or
-- DELETE them. M1-03's triggers remain the superuser-level backstop.
GRANT SELECT, INSERT ON ledger_txns, ledger_entries TO recon_app, recon_executor;

-- The M1-03 validation and balance triggers execute with the caller's
-- privileges, so the posting roles also need SELECT on the reference data they
-- look up. These are reads only: no UPDATE/DELETE is granted anywhere.
GRANT SELECT ON accounts, currencies, periods TO recon_app, recon_executor;

-- +goose StatementBegin
DO $$
DECLARE
    identity_sequence TEXT;
BEGIN
    identity_sequence := pg_get_serial_sequence('ledger_txns', 'seq');
    IF identity_sequence IS NULL THEN
        RAISE EXCEPTION 'ledger_txns.seq has no identity sequence';
    END IF;
    EXECUTE format(
        'GRANT USAGE, SELECT ON SEQUENCE %s TO recon_app, recon_executor',
        identity_sequence);
END;
$$;
-- +goose StatementEnd

-- M1-05 adds audit_log and the recon_audit_ro SELECT grant. This migration owns
-- only the role itself, asserted by internal/db/roles_test.go.
--
-- recon_agent_ro deliberately receives no ledger-table privileges (section 3.2
-- invariant 4 and spec section 16).

-- Spec section 3.2 invariant 4 / D-6: only a session that is a member of
-- recon_executor may append an adjustment transaction. The check is a trigger,
-- not a grant, because the same INSERT privilege is needed for ordinary kinds.
-- +goose StatementBegin
CREATE FUNCTION ledger_enforce_adjustment_role() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.kind = 'adjustment'
       AND NOT pg_has_role(current_user, 'recon_executor', 'MEMBER') THEN
        RAISE EXCEPTION 'only members of recon_executor may insert adjustment transactions (current user %)',
            current_user
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_txns_adjustment_role
    BEFORE INSERT ON ledger_txns
    FOR EACH ROW EXECUTE FUNCTION ledger_enforce_adjustment_role();

-- +goose Down
DROP TRIGGER IF EXISTS ledger_txns_adjustment_role ON ledger_txns;
DROP FUNCTION IF EXISTS ledger_enforce_adjustment_role();

REVOKE ALL ON ledger_txns, ledger_entries FROM recon_app, recon_executor;
REVOKE ALL ON accounts, currencies, periods FROM recon_app, recon_executor;

-- +goose StatementBegin
DO $$
DECLARE
    identity_sequence TEXT;
BEGIN
    identity_sequence := pg_get_serial_sequence('ledger_txns', 'seq');
    IF identity_sequence IS NOT NULL THEN
        EXECUTE format(
            'REVOKE ALL ON SEQUENCE %s FROM recon_app, recon_executor',
            identity_sequence);
    END IF;
END;
$$;
-- +goose StatementEnd

REVOKE USAGE ON SCHEMA public FROM recon_app, recon_executor, recon_agent_ro, recon_audit_ro;

DROP ROLE IF EXISTS recon_audit_ro;
DROP ROLE IF EXISTS recon_agent_ro;
DROP ROLE IF EXISTS recon_executor;
DROP ROLE IF EXISTS recon_app;
