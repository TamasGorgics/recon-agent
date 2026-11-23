package approval

import (
	"context"
	"errors"
	"time"
)

// Observer receives the post-commit outcome of a human decision and bounded
// failure codes from the decision paths. ApprovalDecided is called once per
// committed, non-replayed decision with the proposal's risk tier and the
// latency from proposals.created_at to the stored decision time.
// ApprovalFailed is called with a bounded code when a decision returns an
// error; raw error text is never forwarded.
//
// Request itself emits no latency: no human decision exists yet. A replayed
// decision, an expired proposal, and every failed transaction emit no latency
// either.
//
// A nil Observer disables observation.
type Observer interface {
	ApprovalDecided(tier string, latencySeconds float64) //floatlint:allow the Prometheus histogram boundary is float64 seconds
	ApprovalFailed(code string)
}

// Bounded approval failure codes. They match the frozen recon_errors_total
// code allow-list; the metrics adapter folds any other value to unknown.
const (
	ErrorCodeInvalidRequest = "invalid_request"
	ErrorCodeNotFound       = "not_found"
	ErrorCodeConflict       = "conflict"
	ErrorCodeForbidden      = "forbidden"
	ErrorCodeTimeout        = "timeout"
	ErrorCodeInternal       = "internal"
)

// decisionObservation carries the post-commit facts needed for one latency
// sample. It stays nil for a replay or a proposal that expired before a
// decision, so those paths emit nothing.
type decisionObservation struct {
	tier      string
	createdAt time.Time
	decidedAt time.Time
}

// observeDecision records the latency for one committed, non-replayed
// decision, or a bounded failure code. It is safe to call with a nil observer.
func (s *Service) observeDecision(observation *decisionObservation, err error) {
	if s == nil || s.observer == nil {
		return
	}
	if err != nil {
		s.observer.ApprovalFailed(decisionErrorCode(err))
		return
	}
	if observation == nil {
		return
	}
	latency := observation.decidedAt.Sub(observation.createdAt).Seconds()
	s.observer.ApprovalDecided(observation.tier, latency)
}

// decisionErrorCode maps decision errors to the frozen code allow-list. The
// mapping mirrors the public API's status mapping so a dashboard code and an
// HTTP client see the same class of failure.
func decisionErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ErrorCodeTimeout
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrInvalidDecision):
		return ErrorCodeInvalidRequest
	case errors.Is(err, ErrProposalNotFound), errors.Is(err, ErrApproverNotFound):
		return ErrorCodeNotFound
	case errors.Is(err, ErrApproverRole), errors.Is(err, ErrApproverInactive),
		errors.Is(err, ErrAgentActor):
		return ErrorCodeForbidden
	case errors.Is(err, ErrNotPending), errors.Is(err, ErrExpired),
		errors.Is(err, ErrDecisionConflict), errors.Is(err, ErrCoMaker):
		return ErrorCodeConflict
	default:
		return ErrorCodeInternal
	}
}
