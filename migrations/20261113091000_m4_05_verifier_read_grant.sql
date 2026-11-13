-- +goose Up
-- M4-05: the deterministic verifier runs the production read path as recon_app
-- and loads its sources through the M2 agent read views. Before this migration
-- only recon_agent_ro could SELECT them, so the service failed at runtime under
-- the app pool. This is the verifier's read surface, not a write expansion:
-- no base-table privilege and no INSERT/UPDATE/DELETE grant changes.
GRANT SELECT ON
    v_agent_case_breaks, v_agent_breaks, v_agent_external_records,
    v_agent_ledger_txns, v_agent_ledger_entries, v_agent_fee_schedules,
    v_agent_fx_rates
    TO recon_app;

-- +goose Down
-- Revoke exactly the seven verifier reads. recon_agent_ro keeps its M2 view
-- grants and no other recon_app privilege is touched.
REVOKE SELECT ON
    v_agent_case_breaks, v_agent_breaks, v_agent_external_records,
    v_agent_ledger_txns, v_agent_ledger_entries, v_agent_fee_schedules,
    v_agent_fx_rates
    FROM recon_app;
