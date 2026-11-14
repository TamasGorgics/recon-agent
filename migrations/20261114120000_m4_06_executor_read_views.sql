-- +goose Up
-- M4-06: the executor re-validates a proposal with
-- internal/agent/proposal.ResolveState before applying it. ResolveState reads
-- its case, break, and cited-evidence projections through the M2 agent views.
-- Before this migration recon_executor had base-table SELECT but no SELECT on
-- any v_agent_* view, so every re-validation failed at runtime under the
-- executor pool.
--
-- Scope: exactly the nine owner-rights views ResolveState reads. The grant is
-- additive and view-only: no base-table privilege, no INSERT/UPDATE/DELETE
-- change, and no new table. The other four agent views (accounts, scopes,
-- statement_balances, history) stay agent-only because ResolveState never
-- reads them.
GRANT SELECT ON
    v_agent_cases, v_agent_case_breaks, v_agent_breaks,
    v_agent_ledger_txns, v_agent_ledger_entries,
    v_agent_external_records, v_agent_settlement_batches,
    v_agent_fx_rates, v_agent_fee_schedules
    TO recon_executor;

-- +goose Down
-- Revoke exactly the nine M4-06 reads. recon_agent_ro keeps its M2 view grants,
-- recon_app keeps its M4-05 verifier reads, and no base-table privilege is
-- touched.
REVOKE SELECT ON
    v_agent_cases, v_agent_case_breaks, v_agent_breaks,
    v_agent_ledger_txns, v_agent_ledger_entries,
    v_agent_external_records, v_agent_settlement_batches,
    v_agent_fx_rates, v_agent_fee_schedules
    FROM recon_executor;
