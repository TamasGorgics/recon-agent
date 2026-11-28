-- +goose Up
-- M5-06: append-only evidence for the daily integrity job (FIN-52).
--
-- One row is inserted only when a run completes. There is deliberately no
-- `running` status and no UPDATE/DELETE grant: a crash, timeout, cancellation,
-- or advisory-lock loser leaves no row, and a later attempt is a new row.
--
-- There is no UNIQUE(run_date): manual reruns after a failure, scheduler
-- restarts, and catch-up attempts may produce more than one completed row for
-- the same UTC business date. Consumers select the latest attempt with
-- ORDER BY started_at DESC, id DESC (M5-07/M5-09).
--
-- `checks` is the stable JSON array of five check-result objects. The DB guard
-- only enforces an array whose length equals check_count; the application
-- always writes exactly five (the > 0 guard leaves room for a future additive
-- check without a table migration).
CREATE TABLE integrity_runs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_date      DATE NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('passed', 'failed')),
    check_count   INT NOT NULL CHECK (check_count > 0),
    failure_count INT NOT NULL CHECK (failure_count >= 0 AND failure_count <= check_count),
    checks        JSONB NOT NULL CHECK (jsonb_typeof(checks) = 'array'
                                        AND jsonb_array_length(checks) = check_count),
    triggered_by  TEXT NOT NULL CHECK (triggered_by IN ('schedule', 'manual')),
    CHECK (finished_at >= started_at),
    CHECK ((status = 'passed' AND failure_count = 0)
        OR (status = 'failed' AND failure_count > 0))
);

CREATE INDEX integrity_runs_run_date_started_idx
    ON integrity_runs (run_date, started_at DESC);

-- Append-only by privilege. recon_app may read completed evidence and insert
-- one completed row; it may not mutate or delete history. PUBLIC and every
-- other runtime role get nothing. M5-07 owns any reporting-role grant.
REVOKE ALL ON TABLE integrity_runs FROM PUBLIC;
GRANT SELECT, INSERT ON integrity_runs TO recon_app;
-- Explicitly deny mutation even if a default privilege granted more.
REVOKE UPDATE, DELETE ON TABLE integrity_runs FROM recon_app;

-- +goose Down
DROP TABLE IF EXISTS integrity_runs;
