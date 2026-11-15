package outbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Worker loop defaults.
const (
	// DefaultBatchSize bounds one claim.
	DefaultBatchSize = 20
	// DefaultLease is how long a claimed row stays invisible to other workers.
	DefaultLease = 30 * time.Second
	// DefaultMaxAttempts is the number of handler failures before dead-letter.
	DefaultMaxAttempts = 5
	// DefaultBaseDelay is the first failure backoff.
	DefaultBaseDelay = time.Second
	// DefaultMaxDelay caps the exponential failure backoff.
	DefaultMaxDelay = 5 * time.Minute
	// DefaultPollInterval is the idle wait between empty polls.
	DefaultPollInterval = 250 * time.Millisecond
	// DefaultHandlerTimeout bounds one handler invocation.
	DefaultHandlerTimeout = 30 * time.Second
)

// Message is one claimed outbox row decoded for a handler. Payload is a copy;
// the handler never receives a database row, transaction, or connection.
type Message struct {
	ID             string
	Topic          string
	Payload        json.RawMessage
	IdempotencyKey string
	// Attempts is the number of prior failed handler invocations.
	Attempts int
}

// Handler delivers one outbox message. Handlers must be idempotent on
// Message.IdempotencyKey because delivery is at-least-once.
type Handler interface {
	Handle(ctx context.Context, msg Message) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, msg Message) error

var _ Handler = HandlerFunc(nil)

// Handle calls f.
func (f HandlerFunc) Handle(ctx context.Context, msg Message) error {
	return f(ctx, msg)
}

// Metrics receives delivery outcomes after the corresponding database mark
// commits. Implementations must be safe for concurrent use. The default is a
// no-op.
type Metrics interface {
	Delivered(topic string)
	Failed(topic string)
	DeadLettered(topic string, attempts int)
}

// WorkerOptions is the worker dependency set. NewWorker copies and defaults
// it, so callers may reuse or mutate the value after construction.
type WorkerOptions struct {
	// Pool starts the short claim and mark transactions. Production uses a
	// recon_app pool.
	Pool db.Beginner
	// Handlers maps topic to handler. An unregistered topic is dead-lettered.
	Handlers map[string]Handler
	// Clock supplies due selection, leases, delivered_at, and backoff.
	Clock clock.Clock
	// BatchSize is the claim limit. Zero means DefaultBatchSize.
	BatchSize int
	// Lease is the claim lease. Zero means DefaultLease.
	Lease time.Duration
	// MaxAttempts is the failure count that dead-letters a message. Zero means
	// DefaultMaxAttempts.
	MaxAttempts int
	// BaseDelay is the first failure backoff. Zero means DefaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps the failure backoff. Zero means DefaultMaxDelay.
	MaxDelay time.Duration
	// PollInterval is the empty-batch wait. Zero means DefaultPollInterval.
	PollInterval time.Duration
	// HandlerTimeout bounds one handler invocation. Zero means
	// DefaultHandlerTimeout.
	HandlerTimeout time.Duration
	// Sleep is the interruptible empty-batch wait. Nil means a context-aware
	// timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Metrics receives delivery outcomes. Nil means a no-op recorder.
	Metrics Metrics
	// Logger receives delivery diagnostics. Nil means slog.Default().
	Logger *slog.Logger
}

// nopMetrics is the default Metrics implementation.
type nopMetrics struct{}

var _ Metrics = nopMetrics{}

func (nopMetrics) Delivered(string)         {}
func (nopMetrics) Failed(string)            {}
func (nopMetrics) DeadLettered(string, int) {}
