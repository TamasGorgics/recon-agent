-- +goose Up
-- M2-07: record untrusted-data injection flags on the run and each tool call.
--
-- Contract: M2 piece 3 spec sections 3.7 and 3.9 (roadmap M2-07, spec section
-- 9 items 1/4/5). Flags are access-restricted metadata: recon_app already has
-- UPDATE on agent_runs and INSERT on tool_calls, so no grant change is needed.
-- recon_agent_ro gains no access to the trace.
--
-- Q-23 in docs/open-questions.md records this schema delta from spec section
-- 3.1. No separate flags table: the arrays are append-only-per-call metadata.

ALTER TABLE agent_runs
    ADD COLUMN injection_flags JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(injection_flags) = 'array');

ALTER TABLE tool_calls
    ADD COLUMN injection_flags JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(injection_flags) = 'array');

-- +goose Down
ALTER TABLE tool_calls DROP COLUMN IF EXISTS injection_flags;
ALTER TABLE agent_runs DROP COLUMN IF EXISTS injection_flags;
