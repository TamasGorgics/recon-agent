-- +goose Up
-- M4-03: the proposal state machine writes status/version from the approval
-- service. M4-01 deliberately left recon_app with SELECT only on proposals
-- (Q-71, Q-84); this grant is column-level so payload, rationale, evidence,
-- state_hash, and expires_at stay unwritable.
GRANT UPDATE (status, version) ON proposals TO recon_app;

-- +goose Down
-- Revoke only the M4-03 columns; never touch recon_app's SELECT, the
-- executor's proposal grants, or the agent conflict-inference grant.
REVOKE UPDATE (status, version) ON proposals FROM recon_app;
