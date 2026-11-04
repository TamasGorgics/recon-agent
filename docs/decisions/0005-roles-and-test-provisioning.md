# 0005 — Database roles, grants, and test provisioning

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Spec §16 separates four database roles, and FIN-11 / spec §3.2 invariant 4 require
that posted ledger rows are immutable and that no ledger write originates from the
agent runtime's role. M1-03 installed the ledger tables, triggers, and a
superuser-level immutability backstop but no roles or grants. M0's `internal/testdb`
had a placeholder `RoleDSNs` map and `DSNFor` always returned the maintenance DSN,
so integration tests could not exercise grants at all. The compose file had no
init hook, while M1-04 needs local development logins (Q-14).

## Decision

### Group roles and grants

- Migration `20261104100000_m1_04_roles.sql` creates four **NOLOGIN** group roles:
  `recon_app`, `recon_executor`, `recon_agent_ro`, `recon_audit_ro`. The creation
  is an idempotent `DO` block because compose init may have created the groups
  first (D-1). No LOGIN user and no password exists in any migration.
- Ledger grants (all other objects remain owned by their M1 task):
  - `recon_app`: `SELECT, INSERT` on `ledger_txns` and `ledger_entries`;
    `USAGE, SELECT` on the `ledger_txns.seq` identity sequence; no
    `UPDATE`/`DELETE` anywhere.
  - `recon_executor`: the same grants as `recon_app` plus permission to insert
    `ledger_txns.kind = 'adjustment'`, enforced by trigger. Executor receives the
    grants directly rather than membership in `recon_app`, so the executor
    membership check has one meaning.
  - `recon_agent_ro`: no privileges on any ledger table or ledger sequence. M2
    adds `SELECT` on read-only views.
  - `recon_audit_ro`: no ledger privileges. M1-05 adds `SELECT` on `audit_log`
    only.
- Both posting roles also receive `SELECT` on `accounts`, `currencies`, and
  `periods`. This is an addition to the grant list in D-6 and is required by the
  M1-03 validation and balance triggers, which execute with the caller's
  privileges, and by the M1-06 ledger service's account/period checks. These are
  read-only grants; the no-`UPDATE`/`DELETE` rule is unchanged.
- All four roles receive `USAGE` on schema `public` so the M1-05/M2 objects can be
  used without schema-level surprises.

### Adjustment-only trigger

`ledger_txns_adjustment_role` is a `BEFORE INSERT` row trigger on `ledger_txns`.
It rejects `kind = 'adjustment'` unless
`pg_has_role(current_user, 'recon_executor', 'MEMBER')` is true, with SQLSTATE
`42501` (`insufficient_privilege`). The trigger is deliberately not a grant:
`recon_app` must be able to insert ordinary ledger kinds. PostgreSQL treats a
superuser as a member of every role, so the maintenance role can still insert any
kind; that is consistent with the M1-03 design where triggers, not grants, protect
against a superuser (FIN-11) and is not a production path.

### Role-scoped pools

`internal/db.OpenRole(ctx, name, dsn)` opens a named pool and refuses to return it
unless the session is a member of `name` and is **not** a superuser. This prevents
a role test from accidentally connecting with the maintenance DSN and passing
vacuously. `db.Roles()` is the single source of the four role names.

### Ephemeral test logins

For every cloned test database, `internal/testdb` creates four ephemeral LOGIN
roles — one per group. Each login:

- has a name under `recon_test_login_<pid>_...` (unique per database and role),
- has a password generated with `crypto/rand` (never logged or committed),
- is `LOGIN INHERIT ... IN ROLE <group>`, and
- is returned by `DB.DSNFor(role)` as a rewrite of the maintenance DSN pointing at
  the clone. `DB.DSNFor("")` keeps the maintenance DSN; an unknown role panics
  rather than falling back to elevated privileges.

`DB.cleanup` drops the cloned database first (`WITH (FORCE)`), then drops every
ephemeral login; `CleanupOrphanLogins` / `recon testdb clean` removes leftovers
after a crashed run. The maintenance DSN therefore needs `CREATEDB` and
`CREATEROLE`; the compose and CI users are superusers. Tests never depend on the
compose development logins, so CI's plain Postgres service works without compose
init: the migration creates the groups and the tests create their own logins.

### Compose development logins (Q-14, D-12)

`compose/initdb/01-roles.sql` is mounted read-only at
`/docker-entrypoint-initdb.d/01-roles.sql`. It creates the four groups
idempotently and four local-only LOGIN users:

| User | Group | Local-only password |
|---|---|---|
| `recon_dev_app` | `recon_app` | `recon_dev_app_local_only` |
| `recon_dev_executor` | `recon_executor` | `recon_dev_executor_local_only` |
| `recon_dev_agent_ro` | `recon_agent_ro` | `recon_dev_agent_ro_local_only` |
| `recon_dev_audit_ro` | `recon_audit_ro` | `recon_dev_audit_ro_local_only` |

The passwords are explicitly non-secret and must never be used outside a
developer laptop. The init directory runs only when the `recon-pgdata` volume is
first initialised; existing volumes need `docker compose down -v` followed by
`make db-up`, or a manual run of the file. The script is re-runnable (existence
checks plus `ALTER ROLE ... PASSWORD`, which resets only the documented dev
password and restores group membership).

## Consequences

- The ledger write path must use `recon_app` for ordinary postings and
  `recon_executor` for adjustments (M1-06). Posting as the maintenance/superuser
  user is a test-only escape hatch.
- Posting roles can read reference data but cannot mutate any M1-03 table.
- `recon_agent_ro` is a genuinely write-less role; M2 adds its read-only views.
- Every integration test that calls `testdb.New` performs four extra `CREATE ROLE`
  statements and four `DROP ROLE` statements; the clone/role cycle remains
  parallel-safe because names include the process id and a per-process counter.
- A non-superuser, non-`CREATEROLE` maintenance DSN cannot run the integration
  suite. The requirement is documented on `testdb.Options.AdminDSN`.
- Q-14 is resolved by this ADR and the compose init script.
