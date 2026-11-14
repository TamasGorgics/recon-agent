package executor

import "errors"

// Typed errors returned by Apply and Run. Every error returned by the service
// wraps one of these sentinels so callers branch with errors.Is rather than on
// message text.
var (
	// ErrInvalidProposalID means the supplied proposal identifier is empty,
	// has surrounding whitespace, or is not a UUID.
	ErrInvalidProposalID = errors.New("executor: invalid proposal id")

	// ErrProposalNotFound means no proposal row exists for the identifier.
	ErrProposalNotFound = errors.New("executor: proposal not found")

	// ErrNotApplicable means the proposal status or type cannot be applied by
	// the executor (for example pending_approval, rejected, already
	// apply_failed, request_info, or a type this wave does not implement).
	// Nothing is changed.
	ErrNotApplicable = errors.New("executor: proposal is not applicable")

	// ErrInvalidOptions means NewService received a nil dependency or an
	// invalid default (empty executor run id, negative batch size).
	ErrInvalidOptions = errors.New("executor: invalid options")

	// ErrNoTransaction means a write helper was called without the caller's
	// transaction. Apply always supplies one; the sentinel exists so internal
	// helpers fail closed if that contract is broken.
	ErrNoTransaction = errors.New("executor: caller transaction is required")

	// ErrApplyUnavailable means the executor could not complete the apply
	// decision for an infrastructure reason: the failure path could not be
	// entered, the queue query failed, or a unique-race re-read was
	// unavailable. The proposal is left for the next poll.
	ErrApplyUnavailable = errors.New("executor: apply decision unavailable")

	// ErrInconsistentState means the stored rows contradict each other, for
	// example a proposal marked applied with no applied_proposals row. It is a
	// data-integrity error, not a re-validation failure.
	ErrInconsistentState = errors.New("executor: inconsistent apply state")
)
