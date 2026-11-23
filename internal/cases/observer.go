package cases

import (
	"context"
	"errors"
	"time"
)

// Observer receives post-commit case age observations and bounded failure
// codes. CaseAge is called once per case whose create or status change
// committed successfully, using the injected clock and the row's created_at.
// CaseFailed is called with a bounded code when a public service method returns
// an error; raw error text is never forwarded.
//
// Lifecycle transitions that run inside another service's transaction call the
// state authority directly and therefore have no post-commit hook (Q-101).
// A nil Observer disables observation.
type Observer interface {
	CaseAge(status string, ageSeconds float64) //floatlint:allow the Prometheus histogram boundary is float64 seconds
	CaseFailed(code string)
}

// Bounded case failure codes. They match the frozen recon_errors_total code
// allow-list; the metrics adapter folds any other value to unknown.
const (
	ErrorCodeInvalidRequest = "invalid_request"
	ErrorCodeNotFound       = "not_found"
	ErrorCodeConflict       = "conflict"
	ErrorCodeTimeout        = "timeout"
	ErrorCodeInternal       = "internal"
)

// WithObserver installs the optional case observer. A nil observer leaves the
// service uninstrumented and behavior-identical.
func WithObserver(observer Observer) ServiceOption {
	return func(s *Service) error {
		s.observer = observer
		return nil
	}
}

// observeCase reports one committed case status and its age.
func (s *Service) observeCase(c Case) {
	if s.observer == nil {
		return
	}
	s.observer.CaseAge(string(c.Status), caseAgeSeconds(c, s.clock.Now()))
}

// observeCases reports every case a call created or transitioned.
func (s *Service) observeCases(cases []Case) {
	for _, c := range cases {
		s.observeCase(c)
	}
}

// observeFailure reports one bounded failure code. An empty queue is a normal
// condition, not an error, and emits nothing.
func (s *Service) observeFailure(err error) {
	if s.observer == nil || err == nil {
		return
	}
	code := caseErrorCode(err)
	if code == "" {
		return
	}
	s.observer.CaseFailed(code)
}

// caseAgeSeconds is the observed age for one status change. A resolved case
// uses resolved_at so the sample reflects the resolution time rather than a
// later observation; every other status uses the injected clock. Clock skew
// clamps to zero.
func caseAgeSeconds(c Case, now time.Time) float64 { //floatlint:allow the Prometheus histogram boundary is float64 seconds
	end := now
	if c.Status == StatusResolved && c.ResolvedAt != nil {
		end = *c.ResolvedAt
	}
	age := end.Sub(c.CreatedAt).Seconds()
	if age < 0 {
		return 0
	}
	return age
}

// caseErrorCode maps case-service errors to the frozen code allow-list. An
// empty string means "do not observe" (the lease queue is empty).
func caseErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrNoCase):
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ErrorCodeTimeout
	case errors.Is(err, ErrCaseNotFound), errors.Is(err, ErrBreakNotFound):
		return ErrorCodeNotFound
	case errors.Is(err, ErrStale), errors.Is(err, ErrNotLeaseOwner),
		errors.Is(err, ErrNotLeased), errors.Is(err, ErrLeaseExpired):
		return ErrorCodeConflict
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrInvalidStatus),
		errors.Is(err, ErrInvalidTTL), errors.Is(err, ErrNoBreaks),
		errors.Is(err, ErrBreakInCase), errors.Is(err, ErrBreakNotOpen):
		return ErrorCodeInvalidRequest
	default:
		return ErrorCodeInternal
	}
}
