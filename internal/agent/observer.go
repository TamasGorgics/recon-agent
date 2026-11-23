package agent

// RunObserver receives one post-commit observation for a finalized agent run
// and bounded failure codes for rejected or failed finalizes. AgentRun is
// called only after the single-row agent_runs UPDATE succeeds; a zero-row
// update or any validation failure emits no run observation. Raw error text is
// never forwarded. A nil observer disables observation.
type RunObserver interface {
	AgentRun(outcome, model string, costUSD float64) //floatlint:allow the Prometheus counter boundary is float64 USD
	AgentFailed(code string)
}

// Bounded agent failure codes. They match the frozen recon_errors_total code
// allow-list; the metrics adapter folds any other value to unknown.
const (
	ErrorCodeInvalidRequest = "invalid_request"
	ErrorCodeInvalidOutput  = "invalid_output"
	ErrorCodeNotFound       = "not_found"
	ErrorCodeInternal       = "internal"
)

// StoreOption configures a DBStore at construction.
type StoreOption func(*DBStore)

// WithObserver installs the optional run observer. A nil observer leaves the
// store uninstrumented and behavior-identical.
func WithObserver(observer RunObserver) StoreOption {
	return func(s *DBStore) { s.observer = observer }
}
