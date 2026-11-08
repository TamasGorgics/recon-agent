package sandbox

import "time"

// selectScenarioTime returns the latest event instant, or fallback when there
// are no events. All results are normalized to UTC.
func selectScenarioTime(ledger, external []time.Time, fallback time.Time) time.Time {
	var max time.Time
	for _, candidate := range ledger {
		if candidate.After(max) {
			max = candidate
		}
	}
	for _, candidate := range external {
		if candidate.After(max) {
			max = candidate
		}
	}
	if max.IsZero() {
		max = fallback
	}
	return max.UTC()
}
