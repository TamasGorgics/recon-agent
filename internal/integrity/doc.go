// Package integrity implements the FIN-52 daily integrity control.
//
// Run executes exactly five independent checks against one REPEATABLE READ
// snapshot:
//
//  1. ledger_balance       — every ledger transaction balances per currency and
//     has at least one entry.
//  2. value_conservation   — matched plus unmatched value equals ingested value
//     per source and currency.
//  3. applied_proposal_trail — internal/invariants.Check, unchanged: every
//     applied proposal has its ledger transaction, audit trail, resolved
//     break/case, and outbox row.
//  4. audit_chain          — internal/audit.Verify from genesis, read-only.
//  5. tieout               — internal/tieout.Compute for every scope with a
//     statement at or before the run date.
//
// A completed run is append-only evidence: exactly one integrity_runs row is
// inserted inside the run transaction, and a failed run also enqueues exactly
// one integrity.failed outbox alert in the same transaction. The package never
// imports internal/match, never writes ledger/audit/match rows, and never
// repairs data. A pass means "no inconsistency was detected", not "the database
// contains data": an empty database and an all-skipped tie-out both pass
// vacuously.
package integrity
