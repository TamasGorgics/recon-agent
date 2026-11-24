-- M1-04 local-development roles (M1 spec D-6/D-12; Q-14).
--
-- The postgres image runs this file only when it initialises an empty
-- recon-pgdata volume. It creates the five NOLOGIN group roles and five
-- local-only development LOGIN users. Migrations re-create the groups
-- idempotently (in case this file did not run); run `make migrate` afterwards.
-- M5-04 adds the fifth read-only dashboard group and login; its v_ops_* view
-- grants are owned by migration 20261124120000_m5_04_dashboard_views.sql.
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
-- then run `make migrate`. Re-running the file is idempotent: it restores the
-- group role, the login, and its membership/password after a manual cleanup.
--
-- Local-only credentials:
--   recon_dev_app            / recon_dev_app_local_only
--   recon_dev_executor       / recon_dev_executor_local_only
--   recon_dev_agent_ro       / recon_dev_agent_ro_local_only
--   recon_dev_audit_ro       / recon_dev_audit_ro_local_only
--   recon_dev_dashboards_ro  / recon_dev_dashboards_ro_local_only

DO $$
DECLARE
    group_name TEXT;
BEGIN
    FOR group_name IN
        SELECT unnest(ARRAY['recon_app', 'recon_executor', 'recon_agent_ro',
                            'recon_audit_ro', 'recon_dashboards_ro'])
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
              ('recon_dev_audit_ro', 'recon_audit_ro'),
              ('recon_dev_dashboards_ro', 'recon_dashboards_ro')
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

-- Role-level settings are not inherited by a member LOGIN at connection time,
-- so bind the read-only/default-timeout guard to the dashboard login itself.
ALTER ROLE recon_dev_dashboards_ro SET default_transaction_read_only = on;
ALTER ROLE recon_dev_dashboards_ro SET statement_timeout = '30s';
