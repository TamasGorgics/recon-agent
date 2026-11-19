package lifecycle

import (
	"errors"
	"fmt"

	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
)

// Sentinel errors returned by the lifecycle services. Callers compare with
// errors.Is; every error a service returns wraps one of these sentinels or a
// sentinel from the underlying authority.
var (
	// ErrInvalidRequest means a required identifier is missing or malformed,
	// or a dependency is nil.
	ErrInvalidRequest = errors.New("cases/lifecycle: invalid request")
	// ErrNoTransaction means the caller did not supply its transaction.
	ErrNoTransaction = fmt.Errorf("%w: caller transaction is required", ErrInvalidRequest)
	// ErrInvalidActor means the actor string is not canonical
	// human:<id>/system:<component>. A well-formed agent:* actor is
	// ErrActorNotAllowed instead.
	ErrInvalidActor = errors.New("cases/lifecycle: invalid actor")
	// ErrActorNotAllowed means the actor class may not perform lifecycle work.
	// Agent credentials and actors are deliberately rejected.
	ErrActorNotAllowed = errors.New("cases/lifecycle: actor is not allowed")
	// ErrInvalidReason means a lifecycle reason is missing or malformed
	// (over-long or containing a control character).
	ErrInvalidReason = errors.New("cases/lifecycle: invalid reason")
	// ErrInvalidCaseStatus means the stored case status is outside the six case
	// statuses. The schema CHECK prevents this; the defensive branch fails
	// closed instead of silently proceeding.
	ErrInvalidCaseStatus = errors.New("cases/lifecycle: invalid case status")
	// ErrMatchNotFound means no match row exists for the id supplied to
	// ReconcileLateMatch.
	ErrMatchNotFound = errors.New("cases/lifecycle: match not found")
	// ErrInvalidMatchStatus means the stored match status is outside the
	// active/unmatched set. The schema CHECK prevents this; the defensive branch
	// fails closed instead of treating an unknown row as active.
	ErrInvalidMatchStatus = errors.New("cases/lifecycle: invalid match status")
	// ErrMatchNotActive means the match is already unmatched (or otherwise not
	// active), so an admin unmatch is a conflict with no writes.
	ErrMatchNotActive = errors.New("cases/lifecycle: match is not active")
	// ErrAdjustmentApplied means a linked adjustment or write-off proposal was
	// already applied to the ledger. Admin unmatch refuses before any write
	// until the operator reverses that ledger transaction.
	ErrAdjustmentApplied = errors.New("cases/lifecycle: linked adjustment has been applied")
	// ErrInvalidBreakStatus means the stored break status is outside the five
	// break statuses. The schema CHECK prevents this; the defensive branch fails
	// closed instead of guessing whether a break is terminal.
	ErrInvalidBreakStatus = errors.New("cases/lifecycle: invalid break status")
	// ErrCaseNotFound means no case row exists for the id. It is the case
	// authority's sentinel so callers can match either spelling.
	ErrCaseNotFound = casestate.ErrCaseNotFound
	// ErrTerminalState means the case is resolved and may not be superseded.
	// It is the case authority's sentinel (which also satisfies
	// errors.Is(err, casestate.ErrIllegalTransition)).
	ErrTerminalState = casestate.ErrTerminalState
	// ErrProposalNotFound means no proposal row exists for the id supplied to
	// OnProposalClosed.
	ErrProposalNotFound = errors.New("cases/lifecycle: proposal not found")
	// ErrInvalidWaitingReason means ResumeWaiting received a reason outside the
	// closed human_reply/new_ingest set, or an actor class that does not match
	// that reason.
	ErrInvalidWaitingReason = errors.New("cases/lifecycle: invalid waiting-info reason")
)
