-- +goose Up
-- M4-05: the deterministic verifier service records its output on the proposal
-- from recon_app. M4-01 left recon_app with SELECT only, M4-03 added
-- UPDATE(status, version), and M4-04 added UPDATE(risk_tier, policy_decision);
-- this migration adds only the two verifier columns. status/version remain
-- M4-03's grant, the policy columns remain M4-04's grant, and expires_at
-- remains M4-11's additive grant. payload, rationale, evidence, state_hash,
-- confidence, and every other column stay unwritable.
GRANT UPDATE (verification, pattern_verified) ON proposals TO recon_app;

-- +goose Down
-- Revoke only the M4-05 columns; never touch recon_app's SELECT, M4-03's
-- status/version grant, M4-04's policy-column grant, or the executor's
-- proposal grants.
REVOKE UPDATE (verification, pattern_verified) ON proposals FROM recon_app;
