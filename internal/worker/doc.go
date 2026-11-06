// Package worker runs the M2 case worker: it polls the case queue, leases one
// case at a time under a concurrency cap, runs the agent loop, and applies the
// attempt policy.
//
// # Poll and lease
//
// Each poll checks system_switches.agent_enabled (fail closed), reads the daily
// USD spend, waits for a concurrency slot, re-checks the switch and spend, and
// leases the next open case with FOR UPDATE SKIP LOCKED and a lease TTL. The
// second check closes the window while a poll is blocked on a full concurrency
// slot: a flip or budget exhaustion during that wait must not start another run.
// A case already at the attempt cap is escalated with reason agent_failed
// without a model call. The worker id is stable per process:
// worker:<hostname>:<pid>:<8 random hex>.
//
// # Attempts
//
// Attempt increments are owned by the worker except provider_error (no
// increment) and expired-lease reclaim (cases.ReclaimExpired increments and
// escalates agent_failed when the reclaim reaches the attempt cap).
// budget_exhausted and invalid_output call cases.RecordFailedAttempt, which
// atomically clears the lease and increments attempts; the same call escalates
// the case agent_failed at max_attempts. Every worker-owned release and attempt
// re-checks status='in_agent' and the same leased_by inside the transaction: a
// worker that lost its lease logs the sentinel and no-ops instead of
// overwriting another worker's case.
//
// # Budget and switches
//
// The daily budget is the SUM(agent_runs.cost_usd) for the UTC day from the
// worker clock. At or above the cap the worker stops leasing (cases stay open,
// in-flight runs finish); overshoot is bounded by concurrency times the per-run
// cost cap. agent_enabled is read before every lease and before every model
// call (through the agent loop). A mid-run flip finalizes the run escalated and
// releases the lease without an attempt.
//
// # Shutdown
//
// SIGINT/SIGTERM cancels the poll context: the worker stops leasing, waits up to
// shutdown_grace for in-flight runs, then cancels them. Every release or
// attempt uses a fresh 5-second context so a canceled run context cannot prevent
// the lease cleanup. The lease TTL (3 minutes default) is shorter than the
// per-run wall clock (5 minutes); a run that outlives its lease is reclaimed and
// counts as an attempt (documented M2 risk).
package worker
