-- +goose Up
-- M5-01: proposals carry the trace ID of the case run that produced them.
-- Nullable with no backfill: legacy proposals stay NULL, and non-traced paths
-- write NULL. Values written by Go are validated 32 lowercase hex.
ALTER TABLE proposals ADD COLUMN trace_id TEXT NULL;

-- +goose Down
ALTER TABLE proposals DROP COLUMN IF EXISTS trace_id;
