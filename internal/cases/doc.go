// Package cases turns matcher breaks into cases and owns the case lease state
// machine (M1-14, FIN-24, FIN-30).
//
// Breaks are immutable after detection except for their status. Their identity
// is the canonical match.BreakCandidate.Signature() (D-5), so re-running the
// matcher over unchanged data inserts nothing: CreateBreaks uses ON CONFLICT
// (signature) DO NOTHING. A resolved break with the same signature is never
// recreated; M1 has no reopen path.
//
// Grouping: breaks whose external refs all name the same settlement batch, when
// the batch contributes at least two breaks to this creation pass, form one
// composite-batch case. Every other break gets its own case. case_breaks is the
// membership join table and UNIQUE (break_id) is the one-case-per-break rule.
//
// Leasing is database-exclusive, not in-process: Lease picks one open case with
// FOR UPDATE SKIP LOCKED, assigns leased_by/lease_expires_at, and increments
// version. ReclaimExpired (also run inline by Lease) returns expired leases to
// open and audits each reclaim, incrementing attempts; when the new count
// reaches the service attempt cap the case is escalated agent_failed instead of
// returning to the queue. A fresh lease does not increment attempts; only
// expiry does (spec section 6.5). Renew and Release are owner-guarded.
// Transition is guarded by the caller's expected version and returns ErrStale
// without changing anything when another writer won. RecordFailedAttempt is the
// worker's counted path: in one transaction it re-checks the lease owner,
// increments attempts, clears the lease, and escalates with reason agent_failed
// at the attempt cap (M2-10).
//
// Every state change writes an audit entry through internal/audit in the same
// transaction, with a typed actor and a timestamp from the injected clock.
package cases
