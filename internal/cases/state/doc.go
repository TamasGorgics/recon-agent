// Package state is the single authority for case status transitions (spec
// 4; FIN-24, FIN-30, FIN-33, FIN-43).
//
// Transition runs inside the caller's transaction: it locks the case row
// FOR UPDATE, validates the request against the locked row, performs the
// version-guarded update, and appends exactly one case.transition audit entry.
// It never starts, commits, or rolls back a transaction, so a caller rollback
// removes the status change, lease/resolution changes, and the audit row
// together.
//
// Status transitions are a closed 6x6 table with per-edge actor classes,
// reason rules, and resolution requirements. Agent actors are never allowed to
// drive case status; legitimate loop/lease writes use system:agent-loop. The
// package deliberately does not import internal/cases: callers map Snapshot to
// their own model.
//
// The injected clock lives in TransitionRequest.Now so this package has no
// wall-clock dependency. A zero Now is a fallback for tests and migration
// callers and is documented on the field.
package state
