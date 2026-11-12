// Package state is the single place proposal status transitions are decided
// and recorded (spec section 8.1; FIN-24, FIN-30, FIN-33, FIN-43).
//
// The typed status set and transition table are the canonical source for
// docs/state-machine.md. Transition runs inside the caller's transaction so a
// proposal status change, its audit entry, and the caller's other writes commit
// or roll back together. ExpireDue reads the injected clock and expires due
// proposed/pending_approval proposals one short transaction at a time.
//
// Actor policy here is type-only: agent actors never transition a proposal,
// human actors are limited to pending_approval -> approved/rejected, and system
// actors cover the remaining edges. Approver roles and four-eyes are M4-11.
//
//go:generate go run ../../../tools/state-machine-doc -out ../../../docs/state-machine.md
package state
