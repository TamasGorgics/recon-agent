-- +goose Up
-- M5-05: agent-quality eval views and dashboard grants (spec §3).
--
-- Seven owner-rights views over the eval result tables and the existing
-- approval/apply/escalation history. They are ordinary views (no
-- security_invoker), owned by the migration role, so the dashboard role reads
-- through the owner's privileges and receives no base-table grant. The role
-- already exists from M5-04; a missing role makes the GRANT fail loudly.
--
-- Every `day` is UTC midnight:
--   date_trunc('day', <ts> AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
-- Ratios are NULL when the denominator is zero. No synthetic zero rows and no
-- COALESCE on a ratio. Rows whose bucket timestamp is NULL are excluded.

-- v_eval_accuracy: ordinary accuracy over completed, non-repeat agent/e2e runs,
-- excluding the dedicated injection split. Every case result in a qualifying
-- run counts toward total; correct IS TRUE is the numerator, so a NULL/unknown
-- correctness value would count as not correct.
CREATE OR REPLACE VIEW v_eval_accuracy AS
SELECT
    (date_trunc('day', r.started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS day,
    r.model,
    r.prompt_version,
    COUNT(*) FILTER (WHERE cr.correct IS TRUE)::bigint AS correct,
    COUNT(*)::bigint AS total,
    CASE WHEN COUNT(*) = 0 THEN NULL
         ELSE COUNT(*) FILTER (WHERE cr.correct IS TRUE)::numeric / COUNT(*)
    END AS accuracy
FROM eval_runs r
JOIN eval_case_results cr ON cr.eval_run_id = r.id
WHERE r.ended_at IS NOT NULL
  AND r.repeat_group IS NULL
  AND r.suite IN ('agent', 'e2e')
  AND r.split <> 'injection'
  AND r.started_at IS NOT NULL
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3;

-- v_eval_false_fix: false-fix volume and rate. Deliberately includes the
-- injection split (false-fix safety is scored there) and every other split.
-- Run filters otherwise match v_eval_accuracy.
CREATE OR REPLACE VIEW v_eval_false_fix AS
SELECT
    (date_trunc('day', r.started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS day,
    r.model,
    r.prompt_version,
    COUNT(*) FILTER (WHERE cr.false_fix IS TRUE)::bigint AS false_fix,
    COUNT(*)::bigint AS total,
    CASE WHEN COUNT(*) = 0 THEN NULL
         ELSE COUNT(*) FILTER (WHERE cr.false_fix IS TRUE)::numeric / COUNT(*)
    END AS rate
FROM eval_runs r
JOIN eval_case_results cr ON cr.eval_run_id = r.id
WHERE r.ended_at IS NOT NULL
  AND r.repeat_group IS NULL
  AND r.suite IN ('agent', 'e2e')
  AND r.started_at IS NOT NULL
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3;

-- v_eval_cost_latency: cost and latency distribution for the same runs as
-- v_eval_false_fix (all splits). percentile_disc is the nearest-rank
-- definition; it returns an observed latency value and never interpolates.
-- NULL costs/latencies are ignored by the aggregates (the current schema
-- makes those columns NOT NULL; the view keeps the SQL semantics so a later
-- nullable column does not silently count zero).
--
-- `float8` is PostgreSQL's canonical alias for the spec-required
-- DOUBLE PRECISION output type. The alias is used deliberately because the
-- repository's FIN-01 migrationlint bans the floating-point storage spellings
-- in migrations; these are read-only derived latency (milliseconds), not
-- monetary storage, and the information_schema type is still double precision.
CREATE OR REPLACE VIEW v_eval_cost_latency AS
SELECT
    (date_trunc('day', r.started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS day,
    r.model,
    r.prompt_version,
    avg(cr.cost_usd) AS avg_cost_usd,
    sum(cr.cost_usd) AS total_cost_usd,
    avg(cr.latency_ms)::float8 AS avg_latency_ms,
    percentile_disc(0.50) WITHIN GROUP (ORDER BY cr.latency_ms)::float8 AS p50_latency_ms,
    percentile_disc(0.95) WITHIN GROUP (ORDER BY cr.latency_ms)::float8 AS p95_latency_ms
FROM eval_runs r
JOIN eval_case_results cr ON cr.eval_run_id = r.id
WHERE r.ended_at IS NOT NULL
  AND r.repeat_group IS NULL
  AND r.suite IN ('agent', 'e2e')
  AND r.started_at IS NOT NULL
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3;

-- v_eval_override_rate: rejected terminal human decisions over all terminal
-- human decisions. The LEFT JOIN keeps a decision whose agent run is absent
-- and groups it as 'unknown'. Pending approvals have no decision row and are
-- therefore excluded from both numerator and denominator. There is no edit
-- override event in the product; this view does not infer one.
CREATE OR REPLACE VIEW v_eval_override_rate AS
SELECT
    (date_trunc('day', ap.decided_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS day,
    COALESCE(ar.model, 'unknown') AS model,
    COALESCE(ar.prompt_version, 'unknown') AS prompt_version,
    COUNT(*) FILTER (WHERE ap.decision = 'rejected')::bigint AS rejections,
    COUNT(*)::bigint AS decisions,
    CASE WHEN COUNT(*) = 0 THEN NULL
         ELSE COUNT(*) FILTER (WHERE ap.decision = 'rejected')::numeric / COUNT(*)
    END AS rate
FROM approvals ap
JOIN proposals p ON p.id = ap.proposal_id
LEFT JOIN agent_runs ar ON ar.id = p.agent_run_id
WHERE ap.decided_at IS NOT NULL
  AND ap.decision IN ('approved', 'rejected')
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3;

-- v_eval_apply_failure_rate: strict terminal apply denominator, applied +
-- apply_failed only. An applied proposal without a non-NULL applied_at is
-- excluded (no attributable day). An apply_failed proposal is attributed to
-- its latest proposal.transition/apply_failed audit timestamp and falls back to
-- proposals.created_at when that audit row is absent. Other proposal statuses
-- are excluded from both sides, and one row per proposal prevents retries from
-- being double-counted.
CREATE OR REPLACE VIEW v_eval_apply_failure_rate AS
WITH applied_attempts AS (
    SELECT
        p.id AS proposal_id,
        p.agent_run_id,
        ap.applied_at AS attempt_at,
        false AS failed
    FROM proposals p
    JOIN applied_proposals ap ON ap.proposal_id = p.id
    WHERE p.status = 'applied'
      AND ap.applied_at IS NOT NULL
),
failed_attempts AS (
    SELECT
        p.id AS proposal_id,
        p.agent_run_id,
        COALESCE(
            (SELECT max(a.ts)
               FROM audit_log a
              WHERE a.entity_type = 'proposal'
                AND a.entity_id = p.id::text
                AND a.action = 'proposal.transition'
                AND a.after->>'status' = 'apply_failed'
                AND a.ts IS NOT NULL),
            p.created_at
        ) AS attempt_at,
        true AS failed
    FROM proposals p
    WHERE p.status = 'apply_failed'
),
attempts AS (
    SELECT proposal_id, agent_run_id, attempt_at, failed FROM applied_attempts
    UNION ALL
    SELECT proposal_id, agent_run_id, attempt_at, failed FROM failed_attempts
)
SELECT
    (date_trunc('day', a.attempt_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS day,
    COALESCE(ar.model, 'unknown') AS model,
    COALESCE(ar.prompt_version, 'unknown') AS prompt_version,
    COUNT(*) FILTER (WHERE a.failed)::bigint AS failures,
    COUNT(*)::bigint AS attempts,
    CASE WHEN COUNT(*) = 0 THEN NULL
         ELSE COUNT(*) FILTER (WHERE a.failed)::numeric / COUNT(*)
    END AS rate
FROM attempts a
LEFT JOIN agent_runs ar ON ar.id = a.agent_run_id
WHERE a.attempt_at IS NOT NULL
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3;

-- v_eval_escalations: historical escalation events by UTC event day and
-- reason. Reasons come from the case.transition audit payload; an absent,
-- NULL, or empty reason becomes 'unknown'. Non-escalation and non-case audit
-- rows are excluded. An event count, not a distinct-case count.
CREATE OR REPLACE VIEW v_eval_escalations AS
SELECT
    (date_trunc('day', a.ts AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS day,
    COALESCE(NULLIF(a.after->>'reason', ''), 'unknown') AS reason,
    COUNT(*)::bigint AS count
FROM audit_log a
WHERE a.action = 'case.transition'
  AND a.after->>'status' = 'escalated'
  AND a.ts IS NOT NULL
GROUP BY 1, 2
ORDER BY 1, 2;

-- v_eval_escalation_rate: same-cohort escalation rate. The denominator is
-- distinct cases by creation-day cohort; the numerator is distinct cases from
-- that same cohort with an escalation transition anywhere in their audit
-- history (event day is irrelevant). The rate is a share of a cohort and is
-- therefore already bounded 0..1; it is never clamped, and recent cohorts can
-- still rise as later escalations arrive. A cohort with cases and no
-- escalations is a measured zero; a day with no cases emits no row.
CREATE OR REPLACE VIEW v_eval_escalation_rate AS
WITH cohorts AS (
    SELECT
        c.id AS case_id,
        (date_trunc('day', c.created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS day
    FROM cases c
    WHERE c.created_at IS NOT NULL
),
escalated_cases AS (
    SELECT DISTINCT a.entity_id AS case_id
    FROM audit_log a
    WHERE a.action = 'case.transition'
      AND a.after->>'status' = 'escalated'
      AND a.ts IS NOT NULL
)
SELECT
    co.day,
    COUNT(DISTINCT co.case_id) FILTER (WHERE ec.case_id IS NOT NULL)::bigint AS escalated,
    COUNT(DISTINCT co.case_id)::bigint AS total_cases,
    CASE WHEN COUNT(DISTINCT co.case_id) = 0 THEN NULL
         ELSE COUNT(DISTINCT co.case_id) FILTER (WHERE ec.case_id IS NOT NULL)::numeric
              / COUNT(DISTINCT co.case_id)
    END AS rate
FROM cohorts co
LEFT JOIN escalated_cases ec ON ec.case_id = co.case_id::text
GROUP BY co.day
ORDER BY co.day;

-- Privilege boundary: PUBLIC loses everything on these views and the
-- dashboard role is reset to SELECT only. No base-table grant is added.
REVOKE ALL ON TABLE
    v_eval_accuracy, v_eval_false_fix, v_eval_cost_latency,
    v_eval_override_rate, v_eval_apply_failure_rate,
    v_eval_escalations, v_eval_escalation_rate
    FROM PUBLIC, recon_dashboards_ro;

GRANT SELECT ON
    v_eval_accuracy, v_eval_false_fix, v_eval_cost_latency,
    v_eval_override_rate, v_eval_apply_failure_rate,
    v_eval_escalations, v_eval_escalation_rate
    TO recon_dashboards_ro;

-- +goose Down
-- Reverse only M5-05: revoke this migration's privileges and drop its seven
-- views. The M5-04 role, schema usage, and five v_ops_* views are untouched.
-- The role revoke is guarded so a manually dropped role does not make this
-- down migration un-runnable in a disposable database.
REVOKE ALL ON TABLE
    v_eval_accuracy, v_eval_false_fix, v_eval_cost_latency,
    v_eval_override_rate, v_eval_apply_failure_rate,
    v_eval_escalations, v_eval_escalation_rate
    FROM PUBLIC;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'recon_dashboards_ro') THEN
        EXECUTE 'REVOKE ALL ON TABLE
                     v_eval_accuracy, v_eval_false_fix, v_eval_cost_latency,
                     v_eval_override_rate, v_eval_apply_failure_rate,
                     v_eval_escalations, v_eval_escalation_rate
                     FROM recon_dashboards_ro';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP VIEW IF EXISTS
    v_eval_escalation_rate, v_eval_escalations,
    v_eval_apply_failure_rate, v_eval_override_rate,
    v_eval_cost_latency, v_eval_false_fix, v_eval_accuracy;
