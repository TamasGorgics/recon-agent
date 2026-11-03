# Open Questions

Add an entry here instead of guessing or editing `docs/spec.md`. Questions and
non-blocking findings that do not change approved scope belong here.

## Template

| Field | Content |
|---|---|
| ID | Q-NN |
| Finding / question | What is unclear or contradictory |
| Target task or section | Roadmap task or spec section |
| Proposed change | Smallest change that would resolve it |
| Status | OPEN / RESOLVED / QUEUED |

## Queued non-blocking items (Q-01..Q-19)

| ID | Finding | Target | Proposed change |
|---|---|---|---|
| Q-01 | FIN-53 is mapped in Appendix A but M4-09 does not require `// covers: FIN-53`. | M4-09 | Add annotation instruction. |
| Q-02 | FIN-54 is mapped but M1-15 does not require `// covers: FIN-54`. | M1-15 | Add annotation instruction. |
| Q-03 | M1-16 gate asserts only FIN-01–06, 10–14, 50, 51, 55; FIN-40/41/42 and FIN-43 are excluded without explanation. | M1-16 | State it is a subset gate; full coverage is M6-07. |
| Q-04 | Spec §19 says `docker compose up` seeds the full stack; M5-12 says `make demo`. | Spec §19 / M5-12 | Choose `make demo` or add a seed service; record the deviation. |
| Q-05 | Spec §15 lists webhook ingestion; roadmap §3.1 says v1 is batch-only and M4-02 omits it. | Spec §15 / M4-02 | Mark webhook ingestion deferred for v1. |
| Q-06 | Spec §8.1 shows `apply_failed → escalated`; proposal statuses have no `escalated`. | Spec §8.1 / M4-03 | Clarify proposal vs case transition. |
| Q-07 | `partial_refund` expected resolution is "Adjustment or link". | M1-10 / M3-03 | Pick one canonical truth or allow both in the scorer. |
| Q-08 | **RESOLVED (M1-14):** data-model deltas omit `settlement_batches` columns, `outbox` columns, and `recon_dashboards_ro`; the M1-owned deltas are now all recorded, while `outbox` columns belong to M2 and `recon_dashboards_ro` to M5. M1-07 adds `external_records.value_date DATE` (FIN-42) and `settlement_batches.status`, `quarantine_reason`, `row_count`, `declared_row_count` (FIN-50). M1-12 adds `match_runs`, the `matches` header (N:1 representation, D-4), and `match_legs` (`UNIQUE(match_id, ledger_txn_id)`, partial unique active-leg index, status-sync trigger). M1-14 adds `breaks.signature TEXT NOT NULL UNIQUE` (SHA-256 hex of `match.BreakCandidate.Signature()`, so re-runs use `ON CONFLICT (signature) DO NOTHING`), `breaks.ledger_txn_ids UUID[]`/`breaks.external_refs JSONB` instead of the spec's singular `ledger_txn_id`/`external_record_id` (a composite break covers N of each), `cases.version`/`attempts` non-negative with lease-field coherence checks, and `case_breaks(case_id, break_id)` with FKs and `UNIQUE(break_id)` instead of the spec's `cases.break_ids UUID[]`. | Roadmap §3.1 | Recorded as M1 schema deltas (D-11). |
| Q-09 | M4-14 references the M3-09 gate but does not list M3-09 in `Needs`. | M4-14 | Add M3-09 to `Needs`. |
| Q-10 | M1-15 cites FIN-55, but tie-out is FIN-54; FIN-55 is statement continuity in M1-07. | M1-15 | Cite FIN-54; mention FIN-55 as an input assumption. |
| Q-11 | Spec §22 presents ticketing/tracing/fee source/batch-only as open; roadmap §3.1 decides them. | Spec §22 | Mark resolved or point to roadmap §3.1. |
| Q-12 | README is a one-line stub; M6-09 writes the final README. | M0-01 | Add a minimal README with prerequisites, docs links, and "work in progress" (non-blocking). |
| Q-13 | AGENTS.md can drift if copied from roadmap §1–2. | M0-01 / ongoing | Keep it a short pointer; update both in the same commit (D-15). |
| Q-14 | M1-04 adds compose dev login users; M0-02 creates no init hook. **RESOLVED (M1-04):** `compose/initdb/01-roles.sql` is mounted read-only and creates the four NOLOGIN groups plus four local-only dev logins; see ADR 0005. | M1-04 | Add an init SQL script or document the compose change. |
| Q-15 | M0-03 placeholder job must be removed/replaced when M1-02/M6-05/M6-07 land. | M0-03 / later | Track cleanup in the owning tasks. |
| Q-16 | The planning package lives in `.team/`; it is not published under `docs/`. | Follow-up docs task | Optionally copy to `docs/plan-m0.md` after approval. |
| Q-17 | `deploy/` is in spec §17 but M0 creates no deployment assets. | M5-04 | Create `deploy/` with Grafana/Prometheus at M5-04; record in the layout delta. |
| Q-18 | Spec §17 will remain stale if DP-12 is deferred. | Spec §17 | Apply DP-12 before M0-01; otherwise carry the delta in the M0 plan. |
| Q-19 | Spec §3.1 lists only `audit_log`, but a hash chain alone cannot detect a truncated tail (FIN-31): deleting the last row leaves every remaining link valid. **RESOLVED (M1-05):** the migration adds the append-only `audit_chain_head(seq, row_hash)` mirror, written in the same transaction; `Verify` compares it with `audit_log` and reports a truncated tail or head mismatch. | M1-05 / spec §3.1 | Recorded as an M1-05-owned audit object; `recon_audit_ro` gets SELECT on it so verification is read-only. |
