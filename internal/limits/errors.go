package limits

import "errors"

// Service-construction and operational errors. A failed Check is returned as
// an error so middleware or the future M4-11 approval service never treats a
// zero-value Result as permission to auto-apply.
var (
	// ErrInvalidOptions reports missing dependencies or an invalid limits
	// config at construction time.
	ErrInvalidOptions = errors.New("limits: invalid options")
	// ErrSwitchMissing reports that the seeded auto_apply_enabled row is
	// absent. The migration seeds it, so this is an integrity failure.
	ErrSwitchMissing = errors.New("limits: auto-apply switch is missing")
	// ErrTripReason reports an empty breaker-trip reason.
	ErrTripReason = errors.New("limits: trip reason is required")
	// errMalformedPayload marks a deterministic payload parse/cast failure. It
	// becomes a failed-closed limits_unavailable Result, not a caller error.
	errMalformedPayload = errors.New("limits: malformed proposal payload")
)
