-- +goose Up
-- M5-04: read-only dashboard role and owner-rights v_ops_* views (spec §3).
--
-- The five views are the dashboard's entire database surface. They are
-- owner-rights views (the default, no security_invoker) owned by the migration
-- role, so the view owner can read the base tables while recon_dashboards_ro
-- receives SELECT on the views only. No base-table, sequence, function, or
-- schema-creation privilege is granted and the role is not a member of any
-- existing application role.
--
-- Role creation/settings and every view statement are idempotent: the role is
-- created in a DO block, views use CREATE OR REPLACE, and the grants are
-- re-applied. A goose re-run is a version-table no-op; a manual re-execution of
-- the statements is also safe.
--
-- TODO(M5-04 W1): fill in the final role/grant decisions before commit.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'recon_dashboards_ro') THEN
        ALTER ROLE recon_dashboards_ro NOLOGIN;
    ELSE
        CREATE ROLE recon_dashboards_ro NOLOGIN;
    END IF;
END;
$$;
-- +goose StatementEnd

-- Bound the blast radius of a dashboard query. Role-level settings apply to the
-- role itself (and SET ROLE sessions); the local dev login also sets them
-- directly because membership alone does not inherit rolconfig at login.
ALTER ROLE recon_dashboards_ro SET default_transaction_read_only = on;
ALTER ROLE recon_dashboards_ro SET statement_timeout = '30s';

-- Schema usage only: no CREATE.
GRANT USAGE ON SCHEMA public TO recon_dashboards_ro;

-- v_ops_match_rate: external-record match share of each succeeded run, per UTC
-- day and source. A malformed/absent stats value counts as zero; a zero
-- denominator yields NULL rather than a fake 0.
CREATE OR REPLACE VIEW v_ops_match_rate AS
SELECT
    (date_trunc('day', mr.started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS bucket_start,
    s.source,
    COALESCE(SUM(CASE WHEN jsonb_typeof(mr.stats->'matched') = 'number'
                      THEN (mr.stats->>'matched')::bigint ELSE 0 END), 0)::bigint AS matched,
    COALESCE(SUM(CASE WHEN jsonb_typeof(mr.stats->'external_candidates') = 'number'
                      THEN (mr.stats->>'external_candidates')::bigint ELSE 0 END), 0)::bigint AS considered,
    CASE WHEN COALESCE(SUM(CASE WHEN jsonb_typeof(mr.stats->'external_candidates') = 'number'
                                THEN (mr.stats->>'external_candidates')::bigint ELSE 0 END), 0) = 0
         THEN NULL
         ELSE ROUND(
             SUM(CASE WHEN jsonb_typeof(mr.stats->'matched') = 'number'
                      THEN (mr.stats->>'matched')::bigint ELSE 0 END)::numeric
             / SUM(CASE WHEN jsonb_typeof(mr.stats->'external_candidates') = 'number'
                        THEN (mr.stats->>'external_candidates')::bigint ELSE 0 END), 6)
    END AS match_rate
FROM match_runs mr
JOIN reconciliation_scopes s ON s.id = mr.scope_id
WHERE mr.status = 'succeeded'
GROUP BY 1, 2;

-- v_ops_unmatched_value: exposure proxy of open/in-case breaks, per currency.
-- This is not an accounting total and not a cross-currency total;
-- signed_delta_minor is informational and exposure_major is display-only.
CREATE OR REPLACE VIEW v_ops_unmatched_value AS
SELECT
    b.currency,
    count(*)::bigint AS break_count,
    SUM(abs(b.delta_minor))::bigint AS exposure_minor,
    SUM(b.delta_minor)::bigint AS signed_delta_minor,
    ROUND(SUM(abs(b.delta_minor))::numeric / power(10::numeric, c.exponent), 3) AS exposure_major,
    min(b.detected_at) AS oldest_detected_at
FROM breaks b
JOIN currencies c ON c.code = b.currency
WHERE b.status IN ('open', 'in_case')
GROUP BY b.currency, c.exponent;

-- v_ops_break_aging: one row per open/in-case break; the reference date is the
-- minimum resolvable referenced value_date (ledger txns and external records).
-- No resolvable date or a future-dated minimum is 'unknown' (bucket order 5),
-- never '0-1'. age_days may be negative for a future date and is diagnostic.
CREATE OR REPLACE VIEW v_ops_break_aging AS
WITH params AS (
    SELECT (now() AT TIME ZONE 'UTC')::date AS today
),
open_breaks AS (
    SELECT b.id, b.side, b.currency, b.delta_minor, b.detected_at,
           b.ledger_txn_ids, b.external_refs
    FROM breaks b
    WHERE b.status IN ('open', 'in_case')
),
ref_dates AS (
    SELECT ob.id AS break_id, lt.value_date
    FROM open_breaks ob
    CROSS JOIN LATERAL unnest(ob.ledger_txn_ids) AS txn(id)
    JOIN ledger_txns lt ON lt.id = txn.id
    UNION ALL
    SELECT ob.id, er.value_date
    FROM open_breaks ob
    CROSS JOIN LATERAL jsonb_array_elements(ob.external_refs) AS ref
    JOIN external_records er
      ON er.source = ref->>'source'
     AND er.source_record_id = ref->>'source_record_id'
    WHERE jsonb_typeof(ref) = 'object'
),
aged AS (
    SELECT ob.*, (SELECT min(rd.value_date) FROM ref_dates rd WHERE rd.break_id = ob.id) AS min_value_date
    FROM open_breaks ob
)
SELECT
    a.id::text AS break_id,
    a.side AS break_type,
    a.currency,
    a.delta_minor,
    a.detected_at,
    a.min_value_date,
    CASE WHEN a.min_value_date IS NULL THEN NULL
         ELSE (p.today - a.min_value_date) END AS age_days,
    CASE
        WHEN a.min_value_date IS NULL OR a.min_value_date > p.today THEN 'unknown'
        WHEN (p.today - a.min_value_date) <= 1 THEN '0-1'
        WHEN (p.today - a.min_value_date) <= 3 THEN '2-3'
        WHEN (p.today - a.min_value_date) <= 7 THEN '4-7'
        ELSE '>7'
    END AS bucket,
    CASE
        WHEN a.min_value_date IS NULL OR a.min_value_date > p.today THEN 5
        WHEN (p.today - a.min_value_date) <= 1 THEN 1
        WHEN (p.today - a.min_value_date) <= 3 THEN 2
        WHEN (p.today - a.min_value_date) <= 7 THEN 3
        ELSE 4
    END AS bucket_order
FROM aged a
CROSS JOIN params p;

-- v_ops_approval_queue: one row per proposal with aggregate decisions. The
-- earliest approved decision drives approval latency; any rejected decision
-- makes the presentation 'rejected' and suppresses latency (fail closed).
-- pending_seconds is a snapshot for proposals still pending_approval.
CREATE OR REPLACE VIEW v_ops_approval_queue AS
SELECT
    p.id::text AS proposal_id,
    COALESCE(p.risk_tier, 'unset') AS risk_tier,
    p.status,
    p.created_at,
    a.approved_at,
    a.rejected_at,
    CASE WHEN a.rejected_at IS NOT NULL THEN 'rejected'
         WHEN a.approved_at IS NOT NULL THEN 'approved'
         ELSE NULL END AS decision,
    CASE WHEN a.rejected_at IS NULL AND a.approved_at IS NOT NULL
         THEN EXTRACT(EPOCH FROM (a.approved_at - p.created_at))::bigint END AS approval_seconds,
    CASE WHEN p.status = 'pending_approval'
         THEN GREATEST(EXTRACT(EPOCH FROM (now() - p.created_at))::bigint, 0) END AS pending_seconds
FROM proposals p
LEFT JOIN LATERAL (
    SELECT min(ap.decided_at) FILTER (WHERE ap.decision = 'approved') AS approved_at,
           min(ap.decided_at) FILTER (WHERE ap.decision = 'rejected') AS rejected_at
    FROM approvals ap
    WHERE ap.proposal_id = p.id
) a ON TRUE;

-- v_ops_resolution_mix: exactly one terminal classification per case, with
-- precedence escalated > human > auto. Escalated cases use created_at as
-- event_at because the schema has no escalation timestamp; resolved cases use
-- resolved_at. Cases with no qualifying classification are excluded.
CREATE OR REPLACE VIEW v_ops_resolution_mix AS
WITH classified AS (
    SELECT
        c.id,
        c.currency,
        c.value_minor,
        c.created_at,
        c.resolved_at,
        CASE
            WHEN c.status = 'escalated' THEN 'escalated'
            WHEN c.status = 'resolved' AND EXISTS (
                SELECT 1 FROM proposals p
                JOIN approvals a ON a.proposal_id = p.id
                WHERE p.case_id = c.id AND a.decision = 'approved'
            ) THEN 'human'
            WHEN c.status = 'resolved' AND EXISTS (
                SELECT 1 FROM applied_proposals ap
                JOIN proposals p ON p.id = ap.proposal_id
                WHERE p.case_id = c.id
                  AND p.status IN ('auto_approved', 'applied')
                  AND NOT EXISTS (
                      SELECT 1 FROM approvals a
                      WHERE a.proposal_id = p.id AND a.decision = 'approved'
                  )
            ) THEN 'auto'
            ELSE NULL
        END AS classification,
        COALESCE(c.resolved_at, c.created_at) AS event_at
    FROM cases c
)
SELECT id::text AS case_id, classification, event_at, currency, value_minor, created_at, resolved_at
FROM classified
WHERE classification IS NOT NULL;

-- Privilege boundary: PUBLIC loses everything on these views, the dashboard
-- role is reset to SELECT only, and no base-table grant is added anywhere.
REVOKE ALL ON TABLE
    v_ops_match_rate, v_ops_unmatched_value, v_ops_break_aging,
    v_ops_approval_queue, v_ops_resolution_mix
    FROM PUBLIC, recon_dashboards_ro;

GRANT SELECT ON
    v_ops_match_rate, v_ops_unmatched_value, v_ops_break_aging,
    v_ops_approval_queue, v_ops_resolution_mix
    TO recon_dashboards_ro;

-- +goose Down
-- Reverse only M5-04: revoke the dashboard privileges, drop the five views, and
-- drop the role. DROP ROLE cannot succeed while another database in the cluster
-- still holds a grant to the role (a test-cluster template, or a second dev
-- database). The guarded drop keeps a multi-database down migration safe: the
-- role is dropped when this database was the only holder, otherwise it is left
-- with no privilege in this database for the operator to clean up explicitly.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'recon_dashboards_ro') THEN
        IF to_regclass('public.v_ops_match_rate') IS NOT NULL THEN
            EXECUTE 'REVOKE ALL ON TABLE
                         v_ops_match_rate, v_ops_unmatched_value, v_ops_break_aging,
                         v_ops_approval_queue, v_ops_resolution_mix
                         FROM recon_dashboards_ro';
        END IF;
        EXECUTE 'REVOKE USAGE ON SCHEMA public FROM recon_dashboards_ro';
    END IF;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON TABLE
    v_ops_match_rate, v_ops_unmatched_value, v_ops_break_aging,
    v_ops_approval_queue, v_ops_resolution_mix
    FROM PUBLIC;

DROP VIEW IF EXISTS
    v_ops_resolution_mix, v_ops_approval_queue, v_ops_break_aging,
    v_ops_unmatched_value, v_ops_match_rate;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'recon_dashboards_ro') THEN
        BEGIN
            EXECUTE 'DROP ROLE recon_dashboards_ro';
        EXCEPTION WHEN dependent_objects_still_exist THEN
            RAISE NOTICE 'recon_dashboards_ro retained: other databases in this cluster still hold grants; revoke them before dropping the role';
        END;
    END IF;
END;
$$;
-- +goose StatementEnd
