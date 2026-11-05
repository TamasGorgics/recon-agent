-- +goose Up
-- M2-01 follow-up (Q-22): proposal inserts are idempotent via
-- ON CONFLICT (idempotency_key) DO NOTHING (FIN-12 / Q-B). PostgreSQL infers
-- the conflict arbiter from the named unique index (or unique constraint) and
-- requires SELECT on the conflict-target column even when the statement only
-- does DO NOTHING and returns no row. INSERT-only therefore failed with
-- SQLSTATE 42501 for the real recon_agent_ro login.
--
-- A scratch-database probe on PostgreSQL 16.15 confirmed that column-level
-- SELECT is sufficient: the first insert affects one row, a duplicate key
-- affects zero rows without an error, and a second active proposal still
-- raises 23505 on proposals_one_active_per_case. The same grant leaves
-- payload/evidence/other columns, SELECT *, RETURNING, UPDATE, and DELETE at
-- 42501.
--
-- Keep this column-level. A table-level SELECT would expose proposal payloads
-- and evidence; this grant exists only for conflict inference.
GRANT SELECT (idempotency_key) ON proposals TO recon_agent_ro;

-- +goose Down
REVOKE SELECT (idempotency_key) ON proposals FROM recon_agent_ro;
