package state

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by the case state machine. Callers compare with
// errors.Is; every error Transition returns wraps one of these sentinels.
var (
	// ErrCaseNotFound means no case row exists for the id.
	ErrCaseNotFound = errors.New("cases/state: case not found")
	// ErrInvalidStatus means the stored status is outside the six case
	// statuses. An unknown requested target is ErrInvalidRequest (spec 4.1).
	ErrInvalidStatus = errors.New("cases/state: invalid case status")
	// ErrIllegalTransition means the (from, to) pair is not in the edge table.
	ErrIllegalTransition = errors.New("cases/state: illegal case transition")
	// ErrActorNotAllowed means the actor is an agent actor, has the wrong class
	// for the edge, or is a human claiming system-only superseded_by_match.
	ErrActorNotAllowed = errors.New("cases/state: actor is not allowed for this transition")
	// ErrReasonRequired means a transition that requires a reason received an
	// empty or whitespace-only one.
	ErrReasonRequired = errors.New("cases/state: transition reason is required")
	// ErrInvalidReason means a required free-text reason is malformed. This
	// includes escalation reasons: they are free text, so an empty value is
	// ErrReasonRequired and an over-long or control-character value is
	// ErrInvalidReason.
	ErrInvalidReason = errors.New("cases/state: invalid transition reason")
	// ErrResolutionRequired means the target is resolved and no resolution type
	// was supplied.
	ErrResolutionRequired = errors.New("cases/state: resolution type is required")
	// ErrTerminalState means the source is resolved and the target is anything
	// other than open. A self-transition is ErrAlreadyInState instead.
	ErrTerminalState = fmt.Errorf("%w: terminal source status", ErrIllegalTransition)
	// ErrAlreadyInState means the requested target equals the stored status. No
	// update, version bump, or audit row is produced.
	ErrAlreadyInState = errors.New("cases/state: case is already in that state")
	// ErrStale means the caller's expected version differs from the locked row,
	// or the guarded update lost the CAS. Nothing was changed.
	ErrStale = errors.New("cases/state: stale case version")
	// ErrInvalidRequest means the request shape is invalid: nil transaction,
	// empty or malformed case id, invalid target status, negative expected
	// version, malformed actor, or a resolution type on a non-resolved target.
	ErrInvalidRequest = errors.New("cases/state: invalid request")
)
