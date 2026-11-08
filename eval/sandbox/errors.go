package sandbox

import "errors"

var (
	// ErrScenarioNotFound reports a scenario id that is not in
	// eval_truth.scenarios.
	ErrScenarioNotFound = errors.New("sandbox: scenario not found")
	// ErrNotScoreable reports a known scenario with no eval case. Clean
	// scenarios and non-injection scenarios with an empty break signature are
	// intentionally not scoreable.
	ErrNotScoreable = errors.New("sandbox: scenario is not scoreable")
	// ErrAmbiguousMapping reports a truth signature that resolves to more than
	// one candidate, duplicate truth signatures, or an existing break where a
	// new one was required.
	ErrAmbiguousMapping = errors.New("sandbox: ambiguous scenario mapping")
	// ErrNoTruthCandidate reports a truth signature that cannot be reproduced
	// from the scenario's own references.
	ErrNoTruthCandidate = errors.New("sandbox: truth signature has no candidate")
	// ErrInvalidLoad reports a world load that cannot be attempted.
	ErrInvalidLoad = errors.New("sandbox: invalid load request")
	// ErrInvalidOptions reports an invalid Manager configuration.
	ErrInvalidOptions = errors.New("sandbox: invalid options")
)
