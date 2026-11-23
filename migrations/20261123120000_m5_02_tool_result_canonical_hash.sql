-- +goose Up
-- M5-02: canonical tool-result hash for replay (spec section 11).
--
-- result_hash is SHA-256 over the exact pre-insert envelope bytes, but
-- tool_calls.result is JSONB, which reorders object keys, normalises numbers
-- and inserts whitespace. Replay cannot recompute result_hash from a loaded
-- row. result_canonical_hash is SHA-256 over canonical JSON of the recorded
-- (JSONB-normalised) result, so a loaded row can be verified.
--
-- Nullable with no backfill: legacy rows stay NULL and replay reports a
-- tool_result_hash_unavailable diagnostic for them. Additive only: the
-- existing table-level recon_app SELECT/INSERT grant covers the new column.
ALTER TABLE tool_calls ADD COLUMN result_canonical_hash TEXT NULL;

-- +goose Down
ALTER TABLE tool_calls DROP COLUMN IF EXISTS result_canonical_hash;
