-- +goose Up
-- M1-12: matcher runs, match headers, and match legs (M1 spec D-4, D-11).
--
-- The spec's singular matches.ledger_txn_id cannot express one payout against N
-- ledger entries, so M1 uses a header/leg model while keeping the table name
-- matches. The Q-08 delta recorded in docs/open-questions.md by this migration:
--   * match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
--   * matches header (one active header per external record)
--   * match_legs (one active leg per ledger transaction; UNIQUE(match_id,
--     ledger_txn_id))
--
-- Rows are never deleted: unmatching is a status change and the status-sync
-- trigger keeps match_legs.status aligned with matches.status. Partial unique
-- indexes enforce that an external record or ledger transaction can be active
-- in at most one match at a time, while still allowing historical unmatched
-- rows.

CREATE TABLE match_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope_id     UUID NOT NULL REFERENCES reconciliation_scopes (id) ON DELETE RESTRICT,
    started_at   TIMESTAMPTZ NOT NULL,
    finished_at  TIMESTAMPTZ,
    status       TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    stats        JSONB NOT NULL DEFAULT '{}'::jsonb,
    config_hash  TEXT NOT NULL CHECK (config_hash <> ''),
    CHECK (finished_at IS NULL OR finished_at >= started_at),
    CHECK (status = 'running' OR finished_at IS NOT NULL)
);

CREATE INDEX match_runs_scope_started_idx ON match_runs (scope_id, started_at);

CREATE TABLE matches (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope_id           UUID NOT NULL REFERENCES reconciliation_scopes (id) ON DELETE RESTRICT,
    external_record_id UUID NOT NULL REFERENCES external_records (id) ON DELETE RESTRICT,
    rule               TEXT NOT NULL CHECK (rule <> ''),
    confidence         NUMERIC(5, 4) NOT NULL DEFAULT 1.0
                       CHECK (confidence >= 0 AND confidence <= 1),
    match_run_id       UUID NOT NULL REFERENCES match_runs (id) ON DELETE RESTRICT,
    status             TEXT NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active', 'unmatched')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    unmatched_at       TIMESTAMPTZ,
    unmatched_by       TEXT,
    unmatch_reason     TEXT,
    CHECK (
        (status = 'active'
         AND unmatched_at IS NULL AND unmatched_by IS NULL AND unmatch_reason IS NULL)
        OR (status = 'unmatched' AND unmatched_at IS NOT NULL)
    )
);

-- D-4: at most one active match per external record. Historical unmatched
-- headers stay readable and do not block a later run.
CREATE UNIQUE INDEX matches_external_active_uidx
    ON matches (external_record_id) WHERE status = 'active';

CREATE INDEX matches_scope_status_idx ON matches (scope_id, status);
CREATE INDEX matches_match_run_id_idx ON matches (match_run_id);

CREATE TABLE match_legs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    match_id      UUID NOT NULL REFERENCES matches (id) ON DELETE RESTRICT,
    ledger_txn_id UUID NOT NULL REFERENCES ledger_txns (id) ON DELETE RESTRICT,
    amount_minor  BIGINT NOT NULL,
    currency      TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'unmatched')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT match_legs_match_ledger_key UNIQUE (match_id, ledger_txn_id)
);

-- D-4: at most one active leg per ledger transaction. A 1:1 match has one leg;
-- a composite match has N legs but each ledger transaction is claimed once.
CREATE UNIQUE INDEX match_legs_ledger_active_uidx
    ON match_legs (ledger_txn_id) WHERE status = 'active';

CREATE INDEX match_legs_match_id_idx ON match_legs (match_id);

-- D-4: keep leg status synchronized with the header. Unmatching is a status
-- update on matches, never a delete, so history and the unique indexes agree.
-- +goose StatementBegin
CREATE FUNCTION match_sync_leg_status() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        UPDATE match_legs
           SET status = NEW.status
         WHERE match_id = NEW.id;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER matches_sync_leg_status
    AFTER UPDATE OF status ON matches
    FOR EACH ROW EXECUTE FUNCTION match_sync_leg_status();

-- D-6 grant matrix: the posting roles append and read match rows. UPDATE is
-- limited to the status/run-finish paths (unmatching is a status change). No
-- DELETE is granted anywhere; match rows are historical evidence.
GRANT SELECT, INSERT, UPDATE ON match_runs, matches, match_legs
    TO recon_app, recon_executor;

-- +goose Down
DROP TRIGGER IF EXISTS matches_sync_leg_status ON matches;
DROP FUNCTION IF EXISTS match_sync_leg_status();
DROP TABLE IF EXISTS match_legs;
DROP TABLE IF EXISTS matches;
DROP TABLE IF EXISTS match_runs;
