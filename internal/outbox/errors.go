package outbox

import "errors"

// Sentinel errors returned by Enqueue and NewWorker. Callers compare with
// errors.Is; every returned error wraps one of these sentinels.
var (
	// ErrNoTransaction means Enqueue was called without the caller's
	// transaction. Enqueue never opens its own transaction.
	ErrNoTransaction = errors.New("outbox: caller transaction is required")

	// ErrInvalidMessage means a topic, idempotency key, aggregate field, or
	// payload is empty or the payload is not valid JSON.
	ErrInvalidMessage = errors.New("outbox: invalid message")

	// ErrInvalidOptions means NewWorker received a nil dependency or a
	// negative loop setting.
	ErrInvalidOptions = errors.New("outbox: invalid options")
)
