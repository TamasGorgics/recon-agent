// Package proposal implements the agent's proposal tools and their
// server-side validation (roadmap M2-08, spec section 6.2).
//
// The package registers five proposal tools (propose_link, propose_adjustment,
// propose_write_off, mark_duplicate, request_info) and the terminal escalate
// signal. Every proposal tool validates in this order: JSON schema (registry),
// flagged-run guard, run/case binding, evidence membership, business rules,
// the one-proposal-per-run slot, canonical state hash and idempotency key, then
// an insert through the recon_agent_ro pool. A failed validation never
// consumes the run's slot; only a recorded or replayed insert does.
//
// Evidence is authoritative in this run's successful tool_calls.result.refs
// rows, read through the app pool; RunContext.RetrievedRefs is only a fast
// in-memory precheck. The proposal INSERT is the only write this package
// performs. It uses a client-generated UUID, ON CONFLICT (idempotency_key) DO
// NOTHING, and never RETURNING, because the agent role cannot read proposal
// payloads.
//
// # Canonical state contract (M4-06)
//
// State is the exported canonical projection used to detect stale proposals.
// It covers:
//
//   - CaseState: id, status, resolution_type;
//   - BreakState: id, signature, side, status, delta_minor, currency, sorted
//     ledger_txn_ids, and sorted canonical ext: refs converted from the stored
//     {source, source_record_id} objects;
//   - RecordState: one projection per evidence ref, sorted by ref. The fields
//     cover the six projection kinds: case, ledger_txn, ext, batch, fx and
//     fee_schedule. Keys inside Fields are sorted by encoding/json.
//
// Canonical excludes cases.version, narrative text (descriptions, memos,
// rationales), wall-clock timestamps (created_at, posted_at, detected_at,
// occurred_at, settled_at) and the untrusted raw payload. Dates are YYYY-MM-DD
// strings, timestamps are RFC3339 UTC strings, and money stays an integer in
// minor units. Canonical returns deterministic JSON; Hash returns its
// lowercase hex SHA-256. M4-06 must call these functions rather than
// reimplementing the projection.
package proposal
