// Package audit records an immutable trail of agent and human decisions.
//
// Every mutation of ledger, case, proposal, approval, or policy state writes an
// Entry through Append inside the caller's transaction (FIN-30), so the audit
// row and the change it describes commit or roll back together. audit_log is
// append-only at both layers: recon_app/recon_executor hold only SELECT and
// INSERT grants, and database triggers reject UPDATE, DELETE, and TRUNCATE even
// for the table owner (FIN-31).
//
// # Actors
//
// Actors are typed (FIN-33). The accepted canonical forms are
//
//	system:matcher    deterministic matching work
//	system:executor   deterministic execution work
//	agent:<model>     the agent runtime, identified by model
//	human:<id>        a human approver or operator
//
// Entry stores the actor as actor_type + actor_id (the M1 spec's D-15); Actor
// parses and re-renders the canonical "type:id" form and rejects unknown types,
// empty ids, and surrounding whitespace. An agent or system actor is never a
// human approval; approval policy is enforced elsewhere.
//
// # Hash chain
//
// Each row carries prev_hash and row_hash, both 32-byte SHA-256 values. The
// genesis row links to 32 zero bytes. row_hash is SHA-256 over canonical JSON
// with lexicographically sorted object keys and no insignificant whitespace:
//
//	seq, ts, actor_type, actor_id, action, entity_type, entity_id,
//	before, after, trace_id, prev_hash (hex)
//
// Timestamps are UTC and truncated to Postgres microsecond precision before
// hashing, so Verify recomputes the same digest from the stored row. Payload
// numbers must be integer minor units or JSON strings (FIN-05), never floats:
// before/after carry IDs and amounts only and must not contain personal data
// (M6-06 redaction relies on this).
//
// Append is the only writer. Under a global pg_advisory_xact_lock it reads the
// last (seq, row_hash) from audit_chain_head, assigns the next contiguous seq,
// hashes the row, and inserts audit_log plus the mirror head row in the same
// transaction. Inserting an explicit contiguous seq (OVERRIDING SYSTEM VALUE)
// rather than relying on the identity sequence means a caller rollback, or a
// db.WithTx retry, leaves no phantom gap in the chain.
//
// audit_chain_head mirrors the last pair because a hash chain alone cannot
// distinguish a truncated tail from a complete chain. The mirror is itself
// append-only, and Verify compares it to audit_log, so deleting the last
// audit row is detectable. This extra table is a schema delta recorded in
// docs/open-questions.md (Q-19).
//
// # Advisory-lock throughput tradeoff
//
// Appends are globally serialized. Throughput is bounded by one insert pair plus
// the head read per transaction, regardless of table size, but a transaction
// that calls Append holds the lock until it commits or rolls back. Callers
// should append as late as practical and keep money-writing transactions short;
// a slow transaction blocks every other audit writer. This is an intentional
// M1 choice: the volume is low, and a lock-free/per-chain design would need
// retries or sequence gaps, both of which weaken FIN-31's tamper evidence. If a
// future milestone needs concurrent chains, shard the advisory lock by scope and
// keep this package's Verify semantics unchanged. The lock also introduces a
// deadlock window if a caller takes conflicting row locks before Append; the
// db.WithTx retry policy handles serialization failures and deadlocks, and
// Append itself is idempotent because the retried transaction re-reads the head.
//
// # Verification
//
// Verify recomputes hashes from fromSeq to the head and reports hash mismatches,
// broken prev_hash links, seq gaps (start, middle, or before fromSeq), a
// truncated tail, and a head/audit mirror mismatch. Findings are returned in a
// Report; a non-nil error means verification could not run.
package audit
