# 0002 — Postgres, migrations, transaction helper, and test database

- **Status:** Accepted
- **Date:** 2026-11-02

## Context

M0-02 adds the local Postgres runtime, the transaction helper every
multi-statement write must use, the forward-only goose migration path, and an
isolated template-clone database for integration tests. The pins and the
retry/test-database contracts must be fixed now so M1 schema work does not
re-litigate them.

## Decision

### Postgres runtime

- **Image:** `postgres:16.15`, the newest `postgres:16.<minor>` tag available on
  Docker Hub at implementation time (2026-11-02). Pulled digest:
  `postgres@sha256:5db6dac2ef5dcff512f4f00a009c3c332b9f2f28f24277175bf3e4a3d02ef83a`.
  CI uses the identical tag; no floating `16` and no `latest`.
- **Compose location:** root `compose.yaml`, so plain `docker compose up` works
  (a deliberate exception to spec §17, recorded in ADR 0001).
- **Service:** user/password/database all `recon`; port
  `${POSTGRES_PORT:-5432}:5432`; named volume `recon-pgdata`; healthcheck
  `pg_isready -U recon -d recon` with interval 2s, timeout 3s, retries 30.
  There is no init SQL at M0; M1-04 adds application roles and grants.
- The compose default `recon` user is a superuser in the official image, so it
  satisfies the test database's `CREATEDB` requirement.

### Dependencies

- `github.com/jackc/pgx/v5` **v5.12.0** — pool and transaction API.
- `github.com/pressly/goose/v3` **v3.29.0** — SQL migrations.
- Both are direct requirements in `go.mod`; `go.sum` is committed.

### `WithTx` retry policy (`internal/db`)

- `WithTx(ctx, Beginner, pgx.TxOptions, fn)` uses the default policy;
  `WithTxPolicy` accepts an explicit `RetryPolicy{MaxAttempts, BaseDelay,
  MaxDelay, Sleep}`. `*pgxpool.Pool` satisfies `Beginner`. Callers pass explicit
  `pgx.TxOptions`; there is no silent isolation default.
- **Retryable errors:** SQLSTATE `40001` (serialization failure) and `40P01`
  (deadlock detected) only.
- **Bound:** at most 5 total attempts (initial + 4 retries). The zero-value
  policy and the default policy both mean 5 attempts.
- **Backoff:** exponential with full jitter, delay in
  `[0, min(MaxDelay, BaseDelay*2^attempt))`, defaults `BaseDelay=10ms` and
  `MaxDelay=500ms`. `Sleep` is injectable so unit tests record delays.
- **Callback:** may run up to five times and must be re-runnable; documented on
  `WithTx`. No network call may run while a transaction is open (FIN-22).
- **Exhaustion:** after the bound, `WithTx` returns an error that wraps both
  `ErrRetryExhausted` and the last transaction error.
- **No retry:** unique violations (`23505`), context cancellation/deadline, and
  every other error are returned immediately.
- The helper explicitly rolls back on callback or commit failure using a
  non-canceled context, because pgx does not auto-rollback when the caller's
  context expires.

### Migrations (`migrations/`, `cmd/recon`)

- `migrations.FS` embeds `*.sql`; `00001_init.sql` is a no-op `SELECT 1`
  placeholder (M1-03 owns the real schema).
- `cmd/recon migrate up` sets `goose.SetBaseFS(migrations.FS)`,
  `goose.SetDialect("postgres")`, and calls `goose.UpContext`. Running it twice
  is a no-op the second time.
- `cmd/recon migrate status` prints goose's applied-migration table plus the
  numeric current version and pending count.
- `migrate down` is deliberately not exposed: migrations are forward-only.

### Test database lifecycle (`internal/testdb`)

- `TEST_DATABASE_URL` is required; `New`/`NewWithOptions` and `TestMain` fail
  fast with a clear message when it is unset. The maintenance role needs
  `CREATEDB`.
- **Template:** `recon_test_template`. `EnsureTemplate` (called by `TestMain`
  **before** the database baseline is recorded) creates the template and
  applies migrations once when it is absent, and reuses it when present. It
  takes a Postgres advisory lock so concurrent test processes cannot race the
  setup. A failed migration drops the half-built template so the next run
  retries. Per-test cloning never creates the template lazily.
- **Clone:** `CREATE DATABASE recon_test_<pid>_<counter>_<rand> TEMPLATE
  recon_test_template`. The migration pool is closed before cloning because
  Postgres rejects a template with active connections.
- **Cleanup:** each `DB` registers `t.Cleanup`, which closes the pool and runs
  `DROP DATABASE ... WITH (FORCE)`; failures use `t.Errorf`, not `t.Fatal`.
- **Orphans:** `CleanupOrphans` drops every `recon_test_%` database except the
  template; it is idempotent and backs `make test-db-clean`.
- **ADV-01 second-run rule:** `TestMain` records the `recon_test_%` list after
  `EnsureTemplate` and, after `m.Run`, asserts no new databases remain. The
  template is explicitly excluded from that assertion. This makes a second
  consecutive `make test-integration` pass while reusing the template.
- **Limits:** per-test pool `MaxConns=4`; integration tests run with
  `-parallel=4` to stay below Postgres `max_connections`.
- **Roles:** `Options.RoleDSNs` and `DB.DSNFor(role)` reserve the M1-04 role
  design. M0 configures no roles, so every role resolves to the default DSN.

## Consequences

- Integration tests need a running Postgres and `TEST_DATABASE_URL`; `make`
  exports the compose defaults so the default path needs no `.env`.
- A crashed test run can leave an orphan; `make test-db-clean` removes it, and
  the template is never dropped by that command.
- M1-03 adds real migrations; the template is recreated only when it does not
  exist, so a stale local template must be dropped manually after a schema
  change (or `docker compose down -v` for a full reset).
- Later tasks must keep these pins unless a new ADR supersedes them.
