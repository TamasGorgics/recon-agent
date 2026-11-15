// Package outbox implements the transactional outbox pattern (FIN-22).
//
// # Enqueue
//
// Enqueue writes one side-effect event in the caller's transaction, so the
// event commits or rolls back with the state change it announces. The insert
// is idempotent on idempotency_key and never starts a transaction of its own.
// The four-argument Enqueue records aggregate_type='outbox' and
// aggregate_id=idempotency_key; EnqueueAggregate carries a caller-owned
// aggregate (for example proposal/<id>) for traceability. Both use
// CURRENT_TIMESTAMP for created_at and next_attempt_at so the database clock
// owns the initial due time.
//
// # Claim and delivery
//
// A worker claims due rows in a short transaction with FOR UPDATE SKIP LOCKED,
// moves next_attempt_at forward by Lease, and commits before any handler runs.
// The lease makes a claimed row invisible to other workers until it expires;
// a crash after the claim redelivers after the lease. Delivery therefore is
// at-least-once, never exactly-once, and handlers must be idempotent on the
// message idempotency key.
//
// Handlers are registered per topic and execute outside any transaction with a
// HandlerTimeout (default 30s). A panic is recovered and treated as a failed
// attempt. Success sets delivered_at. Failure increments attempts, applies
// deterministic exponential backoff min(BaseDelay*2^(attempts-1), MaxDelay) to
// next_attempt_at, and dead-letters once attempts reaches MaxAttempts. An
// unregistered topic dead-letters immediately so a missing handler cannot
// become a retry storm.
//
// # Run
//
// Run calls Process until ctx is cancelled. A non-empty batch is followed
// immediately by another poll; an empty batch waits PollInterval through the
// injected Sleep. Run returns nil on context cancellation. Multiple workers
// may run concurrently: SKIP LOCKED and leases prevent a simultaneous double
// claim, while lease expiry deliberately allows redelivery.
//
// # Time and metrics
//
// The worker reads due times, leases, delivery timestamps, and backoff from
// the injected clock.Clock, never the wall clock. Metrics receives Delivered,
// Failed, and DeadLettered callbacks after the corresponding database mark
// commits; the default implementation is a no-op and is not a paging
// integration.
//
// # Grants
//
// The worker expects the recon_app role: table-level SELECT, INSERT, and UPDATE
// on outbox. Enqueue also works through recon_executor's INSERT plus
// column-level SELECT(idempotency_key) for the ON CONFLICT target, so the
// insert never uses RETURNING.
//
// # Test helpers
//
// FakeChannel is an in-memory Handler that deduplicates side effects by
// idempotency key. The sibling package internal/outbox/httpguard fails outbound
// HTTP while a tracked transaction is open; it is test-only and is not wired
// into production transports.
package outbox
