-- +goose Up
-- M5-05: durable marker for demo-owned rows.
--
-- The demo quality reset is a dev-only full-schema reset, not a tagged-only
-- delete, because audit_log is append-only. To keep that destructive path safe
-- it must be able to distinguish rows produced by the demo seed from real
-- rows. The M1 pipeline writes random UUIDs and has no per-row marker column,
-- so every row the demo layers create is recorded here, inside the same
-- transaction as the ops/quality inserts (and in one marking transaction after
-- the M1 pipeline). A reset aborts if any row in any dropped table has no
-- matching (table_name, row_key) entry.
--
-- batch_id groups the rows written by one seed invocation so a failed seed can
-- be reasoned about; layer names the layer that owns the row. This table is
-- itself dropped and recreated by demo.Reset and is granted to no application
-- role.
CREATE TABLE demo_metadata (
    table_name TEXT NOT NULL CHECK (table_name <> ''),
    row_key    TEXT NOT NULL CHECK (row_key <> ''),
    layer      TEXT NOT NULL CHECK (layer IN ('m1', 'ops', 'quality')),
    batch_id   UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (table_name, row_key)
);

CREATE INDEX demo_metadata_batch_idx ON demo_metadata (batch_id);

REVOKE ALL ON demo_metadata FROM PUBLIC;

-- +goose Down
DROP TABLE IF EXISTS demo_metadata;
