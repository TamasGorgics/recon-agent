package limits

import (
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/policy"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Check reasons. These are closed machine codes: M4-11 persists the T1 reason
// on its approval audit/decision extension and must not pass untrusted text.
const (
	ReasonWithinLimits       = "within_limits"
	ReasonNotT0              = "not_t0"
	ReasonInvalidTier        = "invalid_tier"
	ReasonAutoApplyDisabled  = "auto_apply_disabled"
	ReasonValueUnknown       = "value_unknown"
	ReasonCurrencyCapMissing = "currency_cap_missing"
	ReasonDailyCapExceeded   = "daily_cap_exceeded"
	ReasonBreakRateSpike     = "break_rate_spike"
	ReasonLimitsUnavailable  = "limits_unavailable"
)

// Switch and audit vocabulary used by the system breaker. The values mirror
// internal/api (api.SwitchAutoApply, api.ActionSwitchSet, api.EntitySwitchSet)
// without importing the HTTP package; the API reset integration test keeps the
// two definitions aligned.
const (
	// SwitchAutoApply is the seeded switch the breaker disables.
	SwitchAutoApply = "auto_apply_enabled"
	// CircuitBreakerActor is the typed actor stored on the switch and audit row.
	CircuitBreakerActor = "system:circuit-breaker"
	// ActionSwitchSet is the audit action written for a switch change.
	ActionSwitchSet = "switch.set"
	// EntitySystemSwitch is the audit entity type for a switch change.
	EntitySystemSwitch = "system_switch"
)

// Candidate is one proposal presented to Check. T0 is gated; T1 and T2 pass
// through unchanged; any other tier fails closed as invalid_tier. ValueMinor is
// the policy Facts debit total in minor units. For a T0 candidate a non-empty
// ProposalID must be a canonical lower-case UUID and is excluded from the
// cumulative total, so an already-persisted candidate is counted once via
// ValueMinor; an empty ProposalID excludes nothing.
type Candidate struct {
	ProposalID string
	CaseID     string
	Currency   string
	ValueMinor int64
	Tier       policy.Tier
}

// Result is the deterministic outcome of Check. CumulativeMinor and CapMinor
// are populated for daily-cap decisions; BreakerTripped reports whether this
// call changed the switch, and SwitchEnabled is the switch state after Check.
type Result struct {
	Allowed         bool
	Tier            policy.Tier
	Reason          string
	CumulativeMinor int64
	CapMinor        int64
	BreakerTripped  bool
	SwitchEnabled   bool
}

// Options are the limits-service construction inputs. Pool must be the
// recon_app pool (the role holds SELECT/UPDATE on system_switches and INSERT on
// audit_log).
type Options struct {
	Pool   *pgxpool.Pool
	Clock  clock.Clock
	Config config.AutoApplyLimitsConfig
	// Observer optionally receives the post-check cap usage ratio and bounded
	// failure codes. Nil disables observation.
	Observer Observer
}

// Service evaluates T0 candidates against FIN-53 limits. It holds no mutable
// state after construction and is safe for concurrent use.
type Service struct {
	pool     *pgxpool.Pool
	clock    clock.Clock
	config   config.AutoApplyLimitsConfig
	observer Observer
}
