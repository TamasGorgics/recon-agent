package worker

// Observer receives bounded worker-level failure codes. It exists for the
// terminal or unexpected failure paths the worker logs but cannot return (the
// poll loop retries); raw error text is never forwarded. A nil Observer
// disables observation.
type Observer interface {
	WorkerFailed(code string)
}

// Bounded worker failure codes. They match the frozen recon_errors_total code
// allow-list; the metrics adapter folds any other value to unknown.
const (
	ErrorCodeInternal = "internal"
)

// observeFailure forwards one bounded code. The worker never labels by case or
// run identifier.
func (w *Worker) observeFailure(code string) {
	if w == nil || w.observer == nil || code == "" {
		return
	}
	w.observer.WorkerFailed(code)
}

// observeFirstFailure forwards to the first non-nil observer in a variadic
// list. It keeps Finalize's existing call sites source-compatible.
func observeFirstFailure(observers []Observer, code string) {
	for _, observer := range observers {
		if observer != nil {
			observer.WorkerFailed(code)
			return
		}
	}
}
