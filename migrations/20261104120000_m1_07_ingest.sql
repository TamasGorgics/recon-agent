-- +goose Up
-- M1-07: ingestion schema (spec section 3.1; FIN-40, FIN-41, FIN-42, FIN-50,
-- FIN-51, FIN-55).
--
-- This migration is the single owner of external_records, settlement_batches,
-- reconciliation_scopes, and statement_balances (M1 spec D-1). Q-08 deltas
-- recorded in docs/open-questions.md:
--   * external_records.value_date DATE (FIN-42)
--   * settlement_batches.status, quarantine_reason, row_count,
--     declared_row_count (FIN-50)
--
-- Count semantics: declared_row_count is the file trailer count and, after the
-- FIN-50 check passes, equals the number of parsed records. row_count is the
-- number of rows actually inserted into external_records; a duplicate
-- (source, source_record_id) re-sent in a later file is skipped (FIN-51), so
-- row_count can be smaller. A quarantined batch always has row_count = 0.
--
-- All timestamps are UTC TIMESTAMPTZ and business dates are DATE (FIN-40).

CREATE TABLE reconciliation_scopes (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ledger_account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    source            TEXT NOT NULL CHECK (source <> ''),
    currency          TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    UNIQUE (source, currency)
);

CREATE INDEX reconciliation_scopes_ledger_account_id_idx
    ON reconciliation_scopes (ledger_account_id);

CREATE TABLE settlement_batches (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source               TEXT NOT NULL CHECK (source <> ''),
    period_start         DATE,
    period_end           DATE,
    declared_total_minor BIGINT NOT NULL,
    currency             TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    file_hash            TEXT NOT NULL UNIQUE CHECK (file_hash <> ''),
    status               TEXT NOT NULL DEFAULT 'ingested'
                         CHECK (status IN ('ingested', 'quarantined')),
    quarantine_reason    TEXT,
    row_count            INT NOT NULL DEFAULT 0 CHECK (row_count >= 0),
    declared_row_count   INT NOT NULL DEFAULT 0 CHECK (declared_row_count >= 0),
    CHECK (period_end IS NULL OR period_start IS NULL OR period_end >= period_start),
    CHECK (row_count <= declared_row_count),
    -- A quarantined batch always carries the non-empty reason it was rejected;
    -- an ingested batch never carries one.
    CHECK (
        (status = 'ingested' AND quarantine_reason IS NULL)
        OR (status = 'quarantined' AND quarantine_reason IS NOT NULL AND quarantine_reason <> '')
    )
);

CREATE INDEX settlement_batches_source_idx ON settlement_batches (source);

CREATE TABLE statement_balances (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope_id      UUID NOT NULL REFERENCES reconciliation_scopes (id) ON DELETE RESTRICT,
    as_of_date    DATE NOT NULL,
    opening_minor BIGINT NOT NULL,
    closing_minor BIGINT NOT NULL,
    currency      TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    batch_id      UUID NOT NULL REFERENCES settlement_batches (id) ON DELETE RESTRICT,
    UNIQUE (scope_id, as_of_date)
);

CREATE INDEX statement_balances_batch_id_idx ON statement_balances (batch_id);

CREATE TABLE external_records (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source           TEXT NOT NULL CHECK (source <> ''),
    source_record_id TEXT NOT NULL CHECK (source_record_id <> ''),
    kind             TEXT NOT NULL CHECK (kind IN (
                         'charge', 'refund', 'payout', 'fee', 'chargeback', 'fx'
                     )),
    amount_minor     BIGINT NOT NULL,
    currency         TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    fee_minor        BIGINT NOT NULL DEFAULT 0 CHECK (fee_minor >= 0),
    occurred_at      TIMESTAMPTZ NOT NULL,
    settled_at       TIMESTAMPTZ,
    value_date       DATE NOT NULL,
    batch_id         UUID NOT NULL REFERENCES settlement_batches (id) ON DELETE RESTRICT,
    raw              JSONB NOT NULL DEFAULT '{}'::jsonb,
    raw_hash         TEXT NOT NULL CHECK (raw_hash <> ''),
    UNIQUE (source, source_record_id)
);

CREATE INDEX external_records_source_value_date_idx
    ON external_records (source, value_date);

CREATE INDEX external_records_batch_id_idx ON external_records (batch_id);

-- D-6 grant matrix: ingestion tables are append/read only for the posting
-- roles. Quarantine is a final status chosen at insert time, never an UPDATE.
-- recon_agent_ro reads through M2 views, not these base tables.
GRANT SELECT, INSERT ON reconciliation_scopes, settlement_batches,
    statement_balances, external_records TO recon_app, recon_executor;

-- +goose Down
DROP TABLE IF EXISTS external_records;
DROP TABLE IF EXISTS statement_balances;
DROP TABLE IF EXISTS settlement_batches;
DROP TABLE IF EXISTS reconciliation_scopes;
