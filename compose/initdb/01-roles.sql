-- M1-04 local-development roles (M1 spec D-6/D-12; Q-14).
--
-- The postgres image runs this file only when it initialises an empty
-- recon-pgdata volume. It creates the four NOLOGIN group roles and four
-- local-only development LOGIN users. Migration 20261104100000_m1_04_roles.sql
-- re-creates the groups idempotently (in case this file did not run) and owns
-- all table grants; run `make migrate` afterwards.
--
-- Passwords are deliberately non-secret and LOCAL-ONLY. Never use these users
-- outside a developer laptop and never copy these credentials into another
-- environment. CI does not use this file; its Postgres service is plain and
-- integration tests provision their own ephemeral logins.
--
-- Existing volumes do not re-run init scripts. Use `docker compose down -v`
-- and `make db-up`, or run this file by hand:
--   docker compose exec -T postgres psql -U recon -d recon \
--     -f /docker-entrypoint-initdb.d/01-roles.sql
--
-- Local-only credentials:
--   recon_dev_app       / recon_dev_app_local_only
--   recon_dev_executor  / recon_dev_executor_local_only
--   recon_dev_agent_ro  / recon_dev_agent_ro_local_only
--   recon_dev_audit_ro  / recon_dev_audit_ro_local_only

DO $$
DECLARE
    group_name TEXT;
BEGIN
    FOR group_name IN
        SELECT unnest(ARRAY['recon_app', 'recon_executor', 'recon_agent_ro', 'recon_audit_ro'])
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = group_name) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN', group_name);
        ELSE
            EXECUTE format('ALTER ROLE %I NOLOGIN', group_name);
        END IF;
    END LOOP;
END;
$$;

DO $$
DECLARE
    dev RECORD;
BEGIN
    FOR dev IN
        SELECT username, group_name
          FROM (VALUES
              ('recon_dev_app', 'recon_app'),
              ('recon_dev_executor', 'recon_executor'),
              ('recon_dev_agent_ro', 'recon_agent_ro'),
              ('recon_dev_audit_ro', 'recon_audit_ro')
          ) AS users(username, group_name)
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = dev.username) THEN
            EXECUTE format('CREATE ROLE %I LOGIN PASSWORD %L IN ROLE %I',
                dev.username, dev.username || '_local_only', dev.group_name);
        ELSE
            EXECUTE format('ALTER ROLE %I LOGIN PASSWORD %L',
                dev.username, dev.username || '_local_only');
            -- GRANT is idempotent; it restores membership after a partial rerun.
            EXECUTE format('GRANT %I TO %I', dev.group_name, dev.username);
        END IF;
    END LOOP;
END;
$$;
