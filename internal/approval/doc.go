// Package approval records human decisions on proposed reconciliation actions.
//
// The proposal status machine lives in the state subpackage:
// internal/approval/state owns the status set, transition table, typed errors,
// transactional audit writes, and ExpireDue.
package approval
