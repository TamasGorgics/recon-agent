-- +goose Up
-- M4-11: approval service identity and proposal TTL. M4-01 left approvers
-- read-only for recon_app and M4-03/M4-04/M4-05 left expires_at unwritable;
-- this migration adds exactly the two additive capabilities the approval
-- service needs:
--
--   1. approvers.api_key_id maps a config API key to an approver identity.
--   2. recon_app may set proposals.expires_at from the configured policy TTL.
--
-- No new tables and no other grants: approvals INSERT/SELECT and approvers
-- SELECT already exist from M4-01, status/version remain M4-03's grant, the
-- policy columns remain M4-04's grant, and the verifier columns remain
-- M4-05's grant.
ALTER TABLE approvers ADD COLUMN api_key_id TEXT UNIQUE
    CHECK (api_key_id IS NULL OR api_key_id <> '');

GRANT UPDATE (expires_at) ON proposals TO recon_app;

-- +goose Down
-- Reverse in the opposite order: revoke only the M4-11 column grant, then drop
-- only the M4-11 column. Earlier grants and every other column stay untouched.
REVOKE UPDATE (expires_at) ON proposals FROM recon_app;

ALTER TABLE approvers DROP COLUMN api_key_id;
