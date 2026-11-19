package approval

import "errors"

// Sentinel errors returned by the approval service. Callers compare with
// errors.Is; every returned error wraps one of these sentinels where a typed
// outcome exists.
var (
	// ErrInvalidRequest means a required identifier, dependency, or stored
	// proposal input is missing or malformed. It is also returned for a
	// non-canonical proposal UUID before any database call.
	ErrInvalidRequest = errors.New("approval: invalid request")
	// ErrProposalNotFound means no proposal exists for the id.
	ErrProposalNotFound = errors.New("approval: proposal not found")
	// ErrAlreadyRequested means Request was called on a proposal that is not
	// proposed, or the approval.request outbox key already exists. Request
	// writes nothing when it returns this error.
	ErrAlreadyRequested = errors.New("approval: proposal already requested")
	// ErrNotPending means Decide found a proposal that is not pending_approval.
	ErrNotPending = errors.New("approval: proposal is not pending approval")
	// ErrApproverNotFound means no approver matches the supplied UUID or API
	// key id.
	ErrApproverNotFound = errors.New("approval: approver not found")
	// ErrApproverRole means the approver exists but does not hold the approver
	// or admin role.
	ErrApproverRole = errors.New("approval: approver role is not permitted")
	// ErrApproverInactive means the approver exists but is inactive.
	ErrApproverInactive = errors.New("approval: approver is inactive")
	// ErrAgentActor means an agent or system actor was presented as a human
	// approver (FIN-33).
	ErrAgentActor = errors.New("approval: agent or system actors cannot decide")
	// ErrCoMaker means the approver is a maker of the proposal/case/run and
	// four-eyes forbids the decision.
	ErrCoMaker = errors.New("approval: approver is a co-maker")
	// ErrExpired means the pending proposal passed its expiry before the
	// decision; Decide expires it instead.
	ErrExpired = errors.New("approval: proposal has expired")
	// ErrDecisionConflict means a prior decision by another approver or with a
	// different outcome conflicts with this one.
	ErrDecisionConflict = errors.New("approval: decision conflicts with an existing decision")
	// ErrInfoRequestedUnsupported means DecisionOptions.InfoRequested was used
	// without an InfoRequestedHook. The decision fails closed before any write
	// so a request for information can never silently retry or escalate.
	ErrInfoRequestedUnsupported = errors.New("approval: info-requested hook is not configured")
	// ErrInvalidDecision means the decision value is not approved or rejected.
	ErrInvalidDecision = errors.New("approval: invalid decision")
)

// maxDecisionReasonRunes bounds the optional decision reason. It matches the
// proposal and case authority reason bound.
const maxDecisionReasonRunes = 2000

// maxSlackUserIDRunes bounds the external Slack identity accepted by the
// channel lookup. Slack IDs are short ASCII tokens; the limit matches the
// audit actor id bound.
const maxSlackUserIDRunes = 128

// Decision is a human approval outcome. The set is closed.
type Decision string

const (
	// DecisionApproved approves a pending proposal.
	DecisionApproved Decision = "approved"
	// DecisionRejected rejects a pending proposal.
	DecisionRejected Decision = "rejected"
)

// Valid reports whether d is one of the two decision values.
func (d Decision) Valid() bool {
	return d == DecisionApproved || d == DecisionRejected
}
