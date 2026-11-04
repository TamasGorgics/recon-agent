-- +goose Up
-- M1-14: breaks, cases, and case membership (M1 spec D-5, D-11).
--
-- The spec's breaks.ledger_txn_id/external_record_id singular columns cannot
-- express a composite break over N ledger transactions and N external records.
-- D-5 makes the canonical signature the identity and hashes sorted ID lists, so
-- this migration stores both lists:
--   * breaks.signature TEXT NOT NULL UNIQUE: SHA-256 hex of
--     match.BreakCandidate.Signature() (scope, side, sorted ledger IDs, sorted
--     external refs, currency, delta). Re-runs use ON CONFLICT DO NOTHING.
--   * breaks.ledger_txn_ids UUID[] and breaks.external_refs JSONB: the refs the
--     signature covers. Ledger IDs are not individually foreign-keyed because a
--     composite break names several claimed transactions and the signature is
--     the immutable identity; the first detection's match_run_id is retained.
--   * case_breaks instead of the spec's cases.break_ids UUID[] so membership is
--     referentially enforced; UNIQUE (break_id) means a break belongs to at most
--     one case (D-5).
-- These deltas are recorded in docs/open-questions.md Q-08.

CREATE TABLE breaks (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    signature      TEXT NOT NULL UNIQUE CHECK (signature ~ '^[0-9a-f]{64}$'),
    scope_id       UUID NOT NULL REFERENCES reconciliation_scopes (id) ON DELETE RESTRICT,
    match_run_id   UUID NOT NULL REFERENCES match_runs (id) ON DELETE RESTRICT,
    side           TEXT NOT NULL CHECK (side IN (
                       'ledger_only', 'external_only', 'amount_mismatch', 'duplicate'
                   )),
    ledger_txn_ids UUID[] NOT NULL DEFAULT '{}',
    external_refs  JSONB NOT NULL DEFAULT '[]'::jsonb
                   CHECK (jsonb_typeof(external_refs) = 'array'),
    delta_minor    BIGINT NOT NULL DEFAULT 0,
    currency       TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    status         TEXT NOT NULL DEFAULT 'open' CHECK (status IN (
                       'open', 'in_case', 'resolved', 'superseded', 'void'
                   )),
    detected_at    TIMESTAMPTZ NOT NULL,
    CHECK (cardinality(ledger_txn_ids) > 0 OR jsonb_array_length(external_refs) > 0)
);

CREATE INDEX breaks_scope_status_idx ON breaks (scope_id, status);
CREATE INDEX breaks_match_run_id_idx ON breaks (match_run_id);

CREATE TABLE cases (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    priority          INT NOT NULL DEFAULT 0,
    value_minor       BIGINT NOT NULL DEFAULT 0 CHECK (value_minor >= 0),
    currency          TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN (
                          'open', 'in_agent', 'proposal_pending', 'waiting_info',
                          'escalated', 'resolved'
                      )),
    version           INT NOT NULL DEFAULT 0 CHECK (version >= 0),
    attempts          INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    escalation_reason TEXT,
    created_at        TIMESTAMPTZ NOT NULL,
    leased_by         TEXT,
    lease_expires_at  TIMESTAMPTZ,
    resolved_at       TIMESTAMPTZ,
    resolution_type   TEXT,
    -- FIN-24 lease coherence: the owner and expiry are both present or both
    -- absent, and only an in_agent case may hold a lease.
    CHECK ((leased_by IS NULL) = (lease_expires_at IS NULL)),
    CHECK (leased_by IS NULL OR (status = 'in_agent' AND leased_by <> '')),
    CHECK (status <> 'resolved' OR resolved_at IS NOT NULL)
);

-- Lease queue order: highest priority first, then oldest, then id for a stable
-- tie-break. Reclaim scans live leases by expiry.
CREATE INDEX cases_open_queue_idx
    ON cases (priority DESC, created_at ASC, id ASC)
    WHERE status = 'open' AND leased_by IS NULL;
CREATE INDEX cases_lease_expiry_idx
    ON cases (lease_expires_at)
    WHERE leased_by IS NOT NULL;
CREATE INDEX cases_status_idx ON cases (status);

CREATE TABLE case_breaks (
    case_id    UUID NOT NULL REFERENCES cases (id) ON DELETE RESTRICT,
    break_id   UUID NOT NULL REFERENCES breaks (id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (case_id, break_id),
    CONSTRAINT case_breaks_break_key UNIQUE (break_id)
);

CREATE INDEX case_breaks_case_id_idx ON case_breaks (case_id);

-- D-6 grant matrix: the posting roles create breaks/cases and lease them
-- (UPDATE is required for the lease, version, attempts, and status columns).
-- No DELETE is granted: breaks and cases are historical evidence.
GRANT SELECT, INSERT, UPDATE ON breaks, cases, case_breaks
    TO recon_app, recon_executor;

-- +goose Down
DROP TABLE IF EXISTS case_breaks;
DROP TABLE IF EXISTS cases;
DROP TABLE IF EXISTS breaks;
