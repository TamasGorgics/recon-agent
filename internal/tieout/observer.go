package tieout

import (
	"context"
	"errors"
)

// Observer receives one successful tie-out computation or a bounded failure
// code. TieoutComputed is called only after Compute returns a result; a failed
// computation emits no gauge value. The result carries the scope source and
// currency the gauge needs. Raw error text is never forwarded.
//
// A nil Observer disables observation. Today no long-lived process computes a
// tie-out on a schedule, so an API or worker scrape only sees the seeded
// unknown series (Q-101).
type Observer interface {
	TieoutComputed(result Result)
	TieoutFailed(code string)
}

// Bounded tie-out failure codes. They match the frozen recon_errors_total code
// allow-list; the metrics adapter folds any other value to unknown.
const (
	ErrorCodeInvalidRequest = "invalid_request"
	ErrorCodeNotFound       = "not_found"
	ErrorCodeConflict       = "conflict"
	ErrorCodeTimeout        = "timeout"
	ErrorCodeInternal       = "internal"
)

// tieoutErrorCode maps Compute errors to the frozen code allow-list.
func tieoutErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ErrorCodeTimeout
	case errors.Is(err, ErrInvalidRequest):
		return ErrorCodeInvalidRequest
	case errors.Is(err, ErrScopeNotFound), errors.Is(err, ErrNoStatement):
		return ErrorCodeNotFound
	case errors.Is(err, ErrCurrencyMismatch):
		return ErrorCodeConflict
	default:
		return ErrorCodeInternal
	}
}

// firstObserver returns the first non-nil observer. The variadic form lets
// existing callers omit the observer while a scrape-time producer can pass
// one without a second entry point.
func firstObserver(observers []Observer) Observer {
	for _, observer := range observers {
		if observer != nil {
			return observer
		}
	}
	return nil
}
