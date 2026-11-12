package state

import (
	"errors"
	"fmt"

	"github.com/TamasGorgics/recon-agent/internal/audit"
)

// Sentinel errors returned by the proposal state machine. Callers compare with
// errors.Is; every returned error wraps one of these sentinels.
var (
	// ErrInvalidRequest means a required identifier or field is missing,
	// malformed, or a dependency is nil.
	ErrInvalidRequest = errors.New("approval/state: invalid request")
	// ErrProposalNotFound means no proposal row exists for the id.
	ErrProposalNotFound = errors.New("approval/state: proposal not found")
	// ErrUnknownStatus means a status value is outside the nine proposal
	// statuses, either as a transition target or as the stored value.
	ErrUnknownStatus = errors.New("approval/state: unknown proposal status")
	// ErrStale means the caller's expected version differs from the stored
	// version; nothing was changed.
	ErrStale = errors.New("approval/state: stale proposal version")
	// ErrIllegalTransition means the (from, to) pair is not a legal edge.
	ErrIllegalTransition = errors.New("approval/state: illegal proposal transition")
	// ErrTerminalState means the source status has no outgoing edges. It also
	// satisfies errors.Is(err, ErrIllegalTransition).
	ErrTerminalState = fmt.Errorf("%w: terminal source status", ErrIllegalTransition)
	// ErrAlreadyInState means the requested target equals the stored status.
	// No update, audit row, or version bump is produced.
	ErrAlreadyInState = errors.New("approval/state: proposal is already in that state")
	// ErrActorNotAllowed means the actor type is not permitted for the edge.
	ErrActorNotAllowed = errors.New("approval/state: actor type is not allowed for this transition")
	// ErrReasonRequired means a transition that requires a reason received an
	// empty or whitespace-only one.
	ErrReasonRequired = errors.New("approval/state: transition reason is required")
	// ErrInvalidReason means the reason is too long, contains a control
	// character, or is not an allowed apply_failed enum value.
	ErrInvalidReason = errors.New("approval/state: invalid transition reason")
	// ErrInvalidActor means the supplied audit.Actor is malformed. It wraps
	// audit.ErrInvalidActor.
	ErrInvalidActor = fmt.Errorf("approval/state: invalid actor: %w", audit.ErrInvalidActor)
)
