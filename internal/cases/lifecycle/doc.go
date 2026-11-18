// Package lifecycle owns the M4-10 case-lifecycle services: proposal
// supersession, the approval.cancel outbox row, late-match reconciliation,
// retry/expiry, waiting-info re-entry, the missing-settlement SLA, and the
// audited admin unmatch.
//
// Every method that mutates lifecycle state takes the caller's pgx.Tx. The
// caller commits the proposal transition, the cancel outbox row, the lifecycle
// audit, and any case transition together; an error is always returned with the
// caller's transaction still uncommitted so the caller's rollback removes every
// write.
//
// Lock order follows the run specification to avoid deadlocks:
//
//	match -> cases (ascending id) -> proposals (ascending id) -> breaks -> outbox
//
// SupersedeActiveProposal locks the case FOR UPDATE, reads the one active
// proposal under proposals_one_active_per_case, and lets the M4-03 proposal
// authority lock and CAS that proposal. The case status is never changed here:
// a normal replacement leaves the case in proposal_pending, and unmatched or
// late-match callers perform their own case transition afterwards.
//
// Supersession is idempotent by state, not by a side table. Once the old
// proposal is superseded there is no active proposal to find, so a repeated
// call is a no-op with no second transition, cancel row, or audit. The
// approval.cancel idempotency key approval.cancel:<proposal id> is the second
// guard for any caller that races at a different lock scope.
//
// ReconcileLateMatch is invoked by match.Engine.Run inside the matcher's
// transaction after a new active match is inserted. It derives consumed breaks
// from match_legs.ledger_txn_id against breaks.ledger_txn_ids and from the
// match's external record against breaks.external_refs (JSONB containment),
// because the schema has no breaks.match_id. The matcher hook is optional; a
// nil hook leaves older callers unchanged.
//
// Unmatch uses the same matchLink/consumedBreakIDs linkage. It refuses before
// the first write when a linked adjustment/write_off proposal has an applied
// ledger transaction, then marks the match unmatched (the trigger syncs legs),
// supersedes active proposals with reason case_reopened, reopens linked
// breaks, reopens linked cases through the state authority, and writes exactly
// one match.unmatch audit. The caller supplies the human actor; there is no
// silent system actor.
package lifecycle
