package state

// Status is a proposal status from spec section 3.1/8.1. The set is closed:
// Valid reports false for anything outside the nine values below.
type Status string

// The nine proposal statuses in spec order. There is deliberately no
// "escalated" status: an apply failure stays apply_failed and the case
// escalates through the executor/case-lifecycle tasks (Q-06).
const (
	StatusProposed        Status = "proposed"
	StatusPendingApproval Status = "pending_approval"
	StatusApproved        Status = "approved"
	StatusAutoApproved    Status = "auto_approved"
	StatusApplied         Status = "applied"
	StatusApplyFailed     Status = "apply_failed"
	StatusRejected        Status = "rejected"
	StatusExpired         Status = "expired"
	StatusSuperseded      Status = "superseded"
)

var allStatuses = []Status{
	StatusProposed,
	StatusPendingApproval,
	StatusApproved,
	StatusAutoApproved,
	StatusApplied,
	StatusApplyFailed,
	StatusRejected,
	StatusExpired,
	StatusSuperseded,
}

// AllStatuses returns the nine proposal statuses in spec order. The returned
// slice is a copy; callers cannot mutate the package set.
func AllStatuses() []Status {
	out := make([]Status, len(allStatuses))
	copy(out, allStatuses)
	return out
}

// Valid reports whether s is one of the nine proposal statuses.
func (s Status) Valid() bool {
	for _, candidate := range allStatuses {
		if s == candidate {
			return true
		}
	}
	return false
}

// Terminal reports whether s has no outgoing transition. Exactly applied,
// rejected, expired, and superseded are terminal; apply_failed deliberately is
// not (it has retry and supersede edges).
func Terminal(s Status) bool {
	switch s {
	case StatusApplied, StatusRejected, StatusExpired, StatusSuperseded:
		return true
	default:
		return false
	}
}

func statusRank(s Status) int {
	for i, candidate := range allStatuses {
		if s == candidate {
			return i
		}
	}
	return len(allStatuses)
}
