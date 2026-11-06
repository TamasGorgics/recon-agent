package agent

import "errors"

// Sentinel errors returned by the loop.
var (
	// ErrAgentDisabled means system_switches.agent_enabled is off. The run row
	// is finalized escalated and the case is not transitioned.
	ErrAgentDisabled = errors.New("agent: agent_enabled is off")
	// ErrNotLeased means the case is not in_agent with a live owner lease.
	ErrNotLeased = errors.New("agent: case is not leased to an agent run")
	// ErrBudgetExceeded is the internal budget classification. A run that hits
	// a budget returns OutcomeBudgetExhausted with a nil error.
	ErrBudgetExceeded = errors.New("agent: budget exhausted")
	// ErrProposalNotFound means a proposal outcome could not find the row the
	// proposal tool wrote for this run.
	ErrProposalNotFound = errors.New("agent: proposal for run not found")
	// ErrRunNotFound means a finalize update matched no agent_runs row.
	ErrRunNotFound = errors.New("agent: run not found")
	// ErrInvalidConfig means the runner dependencies are incomplete.
	ErrInvalidConfig = errors.New("agent: invalid configuration")
)
