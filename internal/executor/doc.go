// Package executor applies approved proposals exactly once.
//
// # Apply
//
// Apply(ctx, proposalID) locks the proposal and runs every write inside one
// SERIALIZABLE db.WithTxPolicy transaction with bounded serialization/deadlock
// retry (FIN-20, FIN-21): re-validate the proposal's canonical state hash and
// break state, post the adjustment/write-off transaction through
// internal/ledger, insert applied_proposals, resolve the attached breaks and
// the case, transition the proposal to applied through
// internal/approval/state, append an executor.applied audit entry, and enqueue
// one proposal.applied outbox row. Any error rolls the whole transaction back,
// so a crash never leaves a ledger entry without its applied record or audit
// (FIN-23, FIN-25).
//
// A re-validation failure is recorded as apply_failed in a second transaction
// with exactly one of state_changed, break_resolved, or expired. A recorded
// failure is a successful Apply: the error is nil and Result.Status is
// apply_failed. Invalid ids, unknown proposals, and non-appliable statuses
// return typed sentinels; infrastructure database errors are returned
// unchanged. Both leave the status approved for a later retry.
//
// # State-hash reconstruction
//
// The proposal state hash is computed while the case is open or in_agent
// (internal/agent/proposal), but the agent loop moves the case to
// proposal_pending before approval. Apply therefore also compares the stored
// hash against candidate states that restore the two proposal-time case
// statuses and the matching case: evidence record; all other drift is
// state_changed.
//
// # Adjustment and write-off
//
// The stored payload.lines are decoded strictly (known fields, positive
// integer minor units, debit/credit, ISO currency, one line per account, one
// currency). Codes map to live active accounts with matching currency, and
// debits must equal credits with overflow-checked sums. The value date is the
// earliest external value_date referenced by the case breaks, then the
// earliest value_date among the proposal's cited ledger_txn: evidence refs,
// then the clock's UTC date. The ledger transaction uses kind=adjustment and
// idempotency key "proposal:<proposal id>", so a repeated Apply can never post
// twice (FIN-12, FIN-23).
//
// # Link and duplicate
//
// A link payload names exactly one external reference and one ledger
// transaction or a set of ledger transactions. The proposed set must match the
// break's ledger set, or, for an external-only break, the unique M4-05 scope
// candidate. Apply writes one synthetic succeeded match run
// (config_hash=executor/proposal), one active proposal_link header, and one
// signed scope-account leg per ledger transaction; it never posts to the
// ledger. A duplicate payload names an external reference already present on
// the break and writes the same synthetic run plus one active duplicate header
// with no legs, claiming the external record so a later matcher pass cannot
// re-break it. Active external matches or active ledger legs are deterministic
// state_changed conflicts.
//
// # Run
//
// Run polls approved/auto-approved proposals in deterministic order, including
// rows whose expires_at has passed: Apply owns the expiry decision and records
// apply_failed(expired), so expired rows are drained rather than stranded. It
// applies each in its own transaction, logs and continues past a bad proposal,
// and waits PollInterval on an empty (or non-progressing) poll. It returns nil
// on context cancellation and performs no network I/O.
//
// # Audit
//
// The executor writes break.resolve and case.resolve audit entries for its
// minimal resolution (FIN-30) in addition to the proposal.transition,
// ledger.post, and executor.applied entries; one executor.applied entry is
// written per applied proposal.
//
// # Grants
//
// The service expects to run on the recon_executor pool. Migration
// 20261114120000_m4_06_executor_read_views.sql adds exactly the nine
// v_agent_* view reads that proposal.ResolveState needs; the executor keeps its
// existing base-table write grants and its column-level UPDATE(status, version)
// on proposals.
package executor
