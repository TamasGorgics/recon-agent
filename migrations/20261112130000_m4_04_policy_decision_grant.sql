-- +goose Up
-- M4-04: the policy engine writes the decision onto the proposal from
-- recon_app. M4-01 left recon_app with SELECT only and M4-03 added
-- UPDATE(status, version); this migration adds only the two decision columns.
-- payload, rationale, evidence, state_hash, expires_at, verification,
-- pattern_verified, confidence, and every other column stay unwritable.
-- expires_at remains M4-11's additive grant (Q-85(5), Q-86); M4-04 only
-- exposes ProposalTTL().
GRANT UPDATE (risk_tier, policy_decision) ON proposals TO recon_app;

-- +goose Down
-- Revoke only the M4-04 columns; never touch recon_app's SELECT, M4-03's
-- status/version grant, or the executor's proposal grants.
REVOKE UPDATE (risk_tier, policy_decision) ON proposals FROM recon_app;
