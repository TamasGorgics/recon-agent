package limits

import (
	"context"
	"errors"
)

// Observer receives the post-check cap usage ratio and bounded failure codes.
// CapUsageRatio is called once after a successful Check whose result carries a
// positive daily cap; the ratio is already clamped to the 0–1 contract.
// CheckFailed is called with a bounded code when Check returns a database or
// internal error, and for a daily-cap denial (the candidate is never labelled).
//
// A nil Observer disables observation.
type Observer interface {
	CapUsageRatio(currency string, ratio float64) //floatlint:allow the Prometheus gauge boundary is float64 ratio
	CheckFailed(code string)
}

// Bounded limits failure codes. They match the frozen recon_errors_total code
// allow-list; the metrics adapter folds any other value to unknown.
const (
	ErrorCodeInternal         = "internal"
	ErrorCodeTimeout          = "timeout"
	ErrorCodeDailyCapExceeded = "daily_cap_exceeded"
)

// capUsageRatio returns min(1, cumulative/cap) and reports whether a ratio is
// meaningful. A zero or negative cap (no configured currency cap) emits
// nothing, which leaves the seeded unknown series untouched.
func capUsageRatio(cumulative, capMinor int64) (float64, bool) { //floatlint:allow the Prometheus gauge boundary is float64 ratio
	switch {
	case capMinor <= 0:
		return 0, false
	case cumulative <= 0:
		return 0, true
	case cumulative >= capMinor:
		return 1, true
	default:
		return float64(cumulative) / float64(capMinor), true //floatlint:allow the Prometheus gauge boundary is float64 ratio
	}
}

// checkErrorCode maps Check errors to the frozen code allow-list. Raw error
// text never reaches an observer.
func checkErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ErrorCodeTimeout
	default:
		return ErrorCodeInternal
	}
}

// observeCheck records the cap usage ratio and bounded failures for one Check.
func (s *Service) observeCheck(currency string, result Result, err error) {
	if s == nil || s.observer == nil {
		return
	}
	if err != nil {
		s.observer.CheckFailed(checkErrorCode(err))
		return
	}
	if result.Reason == ReasonDailyCapExceeded {
		s.observer.CheckFailed(ErrorCodeDailyCapExceeded)
	}
	if ratio, ok := capUsageRatio(result.CumulativeMinor, result.CapMinor); ok {
		s.observer.CapUsageRatio(currency, ratio)
	}
}
