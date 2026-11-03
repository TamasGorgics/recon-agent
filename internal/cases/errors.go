package cases

import "errors"

// Sentinel errors returned by the case service. Tests use errors.Is so callers
// can add context without losing the classification.
var (
	// ErrInvalidRequest means a required identifier or field is missing or
	// malformed.
	ErrInvalidRequest = errors.New("cases: invalid request")
	// ErrInvalidTTL means Lease or Renew was called with a non-positive TTL.
	ErrInvalidTTL = errors.New("cases: lease ttl must be positive")
	// ErrNoBreaks means CreateCase was called with an empty break set.
	ErrNoBreaks = errors.New("cases: a case requires at least one break")
	// ErrBreakNotFound means one of the requested break IDs does not exist.
	ErrBreakNotFound = errors.New("cases: break not found")
	// ErrBreakNotOpen means the break is resolved, superseded, or void and
	// cannot be added to a new case.
	ErrBreakNotOpen = errors.New("cases: break is not open")
	// ErrBreakInCase means the break already belongs to a case
	// (case_breaks UNIQUE (break_id)).
	ErrBreakInCase = errors.New("cases: break already belongs to a case")
	// ErrCaseNotFound means the case ID does not exist.
	ErrCaseNotFound = errors.New("cases: case not found")
	// ErrNoCase means the lease queue is empty.
	ErrNoCase = errors.New("cases: no case is available to lease")
	// ErrStale means the caller's expected version is older than the stored
	// version; nothing was changed.
	ErrStale = errors.New("cases: stale case version")
	// ErrNotLeased means the case holds no lease (for example a double
	// release).
	ErrNotLeased = errors.New("cases: case is not leased")
	// ErrNotLeaseOwner means the case is leased by a different worker.
	ErrNotLeaseOwner = errors.New("cases: caller does not own the lease")
	// ErrLeaseExpired means the lease expired and must be reclaimed/re-leased.
	ErrLeaseExpired = errors.New("cases: lease has expired")
	// ErrInvalidStatus means a status outside the spec section 3.1 set was
	// requested.
	ErrInvalidStatus = errors.New("cases: invalid case status")
)
