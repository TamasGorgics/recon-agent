-- +goose Up
-- M2-08 post-adversary fix (Q-32): "a run produces at most one proposal"
-- (spec section 6.1) is a durable database invariant, not only the in-memory
-- RunState slot. A replay in a fresh process starts with an empty slot, so the
-- unique index is the authority that rejects a second row for the same run.
--
-- The M2-01 non-unique index on the same column is redundant once this unique
-- index exists; drop it rather than maintain two indexes. The case-level
-- partial index is recreated last on purpose: PostgreSQL checks a table's
-- indexes in OID (creation) order, so an insert that violates both the
-- one-proposal-per-run and one-active-per-case invariants reports the run
-- conflict deterministically as proposal_already_exists. With the reverse
-- order the reported code would depend on which index happened to be older.
DROP INDEX IF EXISTS proposals_agent_run_id_idx;
CREATE UNIQUE INDEX proposals_one_proposal_per_run ON proposals (agent_run_id);

DROP INDEX IF EXISTS proposals_one_active_per_case;
CREATE UNIQUE INDEX proposals_one_active_per_case
    ON proposals (case_id)
    WHERE status IN ('proposed', 'pending_approval', 'approved', 'auto_approved');

-- +goose Down
DROP INDEX IF EXISTS proposals_one_proposal_per_run;
DROP INDEX IF EXISTS proposals_one_active_per_case;
CREATE UNIQUE INDEX proposals_one_active_per_case
    ON proposals (case_id)
    WHERE status IN ('proposed', 'pending_approval', 'approved', 'auto_approved');
CREATE INDEX proposals_agent_run_id_idx ON proposals (agent_run_id);
