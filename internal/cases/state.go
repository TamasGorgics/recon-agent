package cases

import (
	"math"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// Status is one value of the spec section 3.1 case status set.
type Status string

// The case lifecycle statuses from spec section 3.1. M1-14 implements the
// open/in_agent/escalated/resolved transitions plus lease expiry; the remaining
// values are reserved for the M4 case lifecycle.
const (
	StatusOpen            Status = "open"
	StatusInAgent         Status = "in_agent"
	StatusProposalPending Status = "proposal_pending"
	StatusWaitingInfo     Status = "waiting_info"
	StatusEscalated       Status = "escalated"
	StatusResolved        Status = "resolved"
)

// Valid reports whether s is in the spec status set.
func (s Status) Valid() bool {
	switch s {
	case StatusOpen, StatusInAgent, StatusProposalPending, StatusWaitingInfo,
		StatusEscalated, StatusResolved:
		return true
	default:
		return false
	}
}

// BreakStatus is one value of the spec section 3.1 break status set.
type BreakStatus string

// The break statuses from spec section 3.1.
const (
	BreakStatusOpen       BreakStatus = "open"
	BreakStatusInCase     BreakStatus = "in_case"
	BreakStatusResolved   BreakStatus = "resolved"
	BreakStatusSuperseded BreakStatus = "superseded"
	BreakStatusVoid       BreakStatus = "void"
)

// Audit actions written by this package. Entity types are "case" and "break".
const (
	ActionBreakCreate    = "break.create"
	ActionBreakInCase    = "break.in_case"
	ActionCaseCreate     = "case.create"
	ActionCaseLease      = "case.lease"
	ActionCaseRenew      = "case.renew"
	ActionCaseRelease    = "case.release"
	ActionCaseReclaim    = "case.reclaim"
	ActionCaseTransition = "case.transition"
)

// Entity type names in audit_log.
const (
	EntityCase  = "case"
	EntityBreak = "break"
)

// Case is one reconciliation exception case with its lease and concurrency
// fields. BreakIDs is populated on reads and case creation.
type Case struct {
	ID               string
	Priority         int
	ValueMinor       int64
	Currency         string
	Status           Status
	Version          int
	Attempts         int
	EscalationReason *string
	CreatedAt        time.Time
	LeasedBy         *string
	LeaseExpiresAt   *time.Time
	ResolvedAt       *time.Time
	ResolutionType   *string
	BreakIDs         []string
}

// Leased reports whether the case currently holds a lease.
func (c Case) Leased() bool {
	return c.LeasedBy != nil && c.LeaseExpiresAt != nil
}

// Break is one stored break. Signature is the canonical
// match.BreakCandidate.Signature() and is the row identity for reruns.
type Break struct {
	ID           string
	Signature    string
	ScopeID      string
	MatchRunID   string
	Side         match.BreakSide
	LedgerTxnIDs []string
	ExternalRefs []match.RecordRef
	DeltaMinor   int64
	Currency     string
	Status       BreakStatus
	DetectedAt   time.Time
}

// PriorityForValue maps a case's absolute value in minor units to a 1..5 queue
// band. Higher value means higher priority; the bands are deterministic policy
// (100.00, 1,000.00, 10,000.00, 100,000.00 major units at two decimals).
func PriorityForValue(valueMinor int64) int {
	value := absMinor(valueMinor)
	switch {
	case value >= 10_000_000:
		return 5
	case value >= 1_000_000:
		return 4
	case value >= 100_000:
		return 3
	case value >= 10_000:
		return 2
	default:
		return 1
	}
}

// absMinor returns |v| without overflowing on math.MinInt64.
func absMinor(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	if v < 0 {
		return -v
	}
	return v
}
