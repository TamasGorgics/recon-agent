package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Worker claims due outbox rows and delivers them to topic handlers. It is
// safe for concurrent use: every claim and mark is its own short transaction,
// and PostgreSQL SKIP LOCKED plus the lease decide ownership.
type Worker struct {
	pool           db.Beginner
	handlers       map[string]Handler
	clock          clock.Clock
	batchSize      int
	lease          time.Duration
	maxAttempts    int
	baseDelay      time.Duration
	maxDelay       time.Duration
	pollInterval   time.Duration
	handlerTimeout time.Duration
	sleep          func(ctx context.Context, d time.Duration) error
	metrics        Metrics
	logger         *slog.Logger
}

// NewWorker validates opts, applies loop defaults, copies the handler map, and
// returns a worker. Pool and Clock are required; a negative setting returns
// ErrInvalidOptions. NewWorker does not touch the database or call a handler.
func NewWorker(opts WorkerOptions) (*Worker, error) {
	if isNilValue(opts.Pool) {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidOptions)
	}
	if isNilValue(opts.Clock) {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidOptions)
	}

	batchSize := opts.BatchSize
	if batchSize == 0 {
		batchSize = DefaultBatchSize
	}
	if batchSize < 0 {
		return nil, fmt.Errorf("%w: batch size must not be negative", ErrInvalidOptions)
	}

	lease := opts.Lease
	if lease == 0 {
		lease = DefaultLease
	}
	if lease < 0 {
		return nil, fmt.Errorf("%w: lease must not be negative", ErrInvalidOptions)
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = DefaultMaxAttempts
	}
	if maxAttempts < 0 {
		return nil, fmt.Errorf("%w: max attempts must not be negative", ErrInvalidOptions)
	}

	baseDelay := opts.BaseDelay
	if baseDelay == 0 {
		baseDelay = DefaultBaseDelay
	}
	if baseDelay < 0 {
		return nil, fmt.Errorf("%w: base delay must not be negative", ErrInvalidOptions)
	}

	maxDelay := opts.MaxDelay
	if maxDelay == 0 {
		maxDelay = DefaultMaxDelay
	}
	if maxDelay < 0 {
		return nil, fmt.Errorf("%w: max delay must not be negative", ErrInvalidOptions)
	}

	pollInterval := opts.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}
	if pollInterval < 0 {
		return nil, fmt.Errorf("%w: poll interval must not be negative", ErrInvalidOptions)
	}

	handlerTimeout := opts.HandlerTimeout
	if handlerTimeout == 0 {
		handlerTimeout = DefaultHandlerTimeout
	}
	if handlerTimeout < 0 {
		return nil, fmt.Errorf("%w: handler timeout must not be negative", ErrInvalidOptions)
	}

	handlers := make(map[string]Handler, len(opts.Handlers))
	for topic, handler := range opts.Handlers {
		if isNilValue(handler) {
			return nil, fmt.Errorf("%w: nil handler for topic %q", ErrInvalidOptions, topic)
		}
		handlers[topic] = handler
	}

	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	metrics := opts.Metrics
	if metrics == nil {
		metrics = nopMetrics{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Worker{
		pool:           opts.Pool,
		handlers:       handlers,
		clock:          opts.Clock,
		batchSize:      batchSize,
		lease:          lease,
		maxAttempts:    maxAttempts,
		baseDelay:      baseDelay,
		maxDelay:       maxDelay,
		pollInterval:   pollInterval,
		handlerTimeout: handlerTimeout,
		sleep:          sleep,
		metrics:        metrics,
		logger:         logger,
	}, nil
}

// Process claims and handles at most BatchSize due messages and returns the
// number claimed. A handler failure is recorded on the row and does not make
// the batch fail; a claim or mark database failure is returned. Messages left
// in a batch after context cancellation keep their lease and are retried after
// it expires.
func (w *Worker) Process(ctx context.Context) (int, error) {
	now := w.clock.Now()
	messages, err := w.claim(ctx, now)
	if err != nil {
		return 0, err
	}

	var firstErr error
	for _, msg := range messages {
		if ctx.Err() != nil {
			break
		}
		if err := w.deliver(ctx, msg); err != nil {
			w.logger.Error("outbox: mark delivery outcome failed",
				"message_id", msg.ID,
				"topic", msg.Topic,
				"idempotency_key", msg.IdempotencyKey,
				"error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return len(messages), firstErr
}

// Run polls Process until ctx is cancelled and returns nil. A non-empty batch
// is followed immediately by another poll; an empty batch waits PollInterval
// through the injected Sleep.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		claimed, err := w.Process(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.logger.Error("outbox: process batch failed", "error", err)
		}
		if claimed > 0 {
			continue
		}

		if err := w.sleep(ctx, w.pollInterval); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("outbox: poll wait: %w", err)
		}
	}
}

// deliver runs one handler outside any transaction and marks the row. It
// returns only database errors; handler failures become attempt/dead-letter
// updates.
func (w *Worker) deliver(ctx context.Context, msg Message) error {
	handler, ok := w.handlers[msg.Topic]
	if !ok {
		return w.deadLetterUnknownTopic(ctx, msg)
	}

	handlerErr := w.callHandler(ctx, handler, msg)
	now := w.clock.Now()
	if handlerErr == nil {
		return w.markDelivered(ctx, msg, now)
	}
	return w.markFailed(ctx, msg, now, handlerErr)
}

// callHandler invokes one handler with HandlerTimeout and converts a panic
// into an error so one bad handler cannot kill the worker.
func (w *Worker) callHandler(ctx context.Context, handler Handler, msg Message) (err error) {
	handlerCtx, cancel := context.WithTimeout(ctx, w.handlerTimeout)
	defer cancel()

	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("outbox: handler panic: %v", recovered)
		}
	}()
	return handler.Handle(handlerCtx, msg)
}

// markDelivered records a successful delivery. A zero-row update means another
// worker already marked the message (benign duplicate) and does not emit the
// Delivered metric.
func (w *Worker) markDelivered(ctx context.Context, msg Message, now time.Time) error {
	affected, err := w.mark(ctx,
		`UPDATE outbox
		    SET delivered_at = $2
		  WHERE id = $1::uuid
		    AND delivered_at IS NULL
		    AND dead = false`,
		msg.ID, now)
	if err != nil {
		return err
	}
	if affected == 1 {
		w.metrics.Delivered(msg.Topic)
		w.logger.Debug("outbox: delivered",
			"message_id", msg.ID, "topic", msg.Topic, "attempts", msg.Attempts)
	}
	return nil
}

// markFailed records a failed attempt with deterministic exponential backoff
// and dead-letters when the incremented attempt count reaches MaxAttempts. The
// UPDATE returns the stored attempts and dead values, so the metric, dead
// decision, and backoff follow the authoritative row rather than the stale
// claimed Message.Attempts (a lease-expiry redelivery can advance the row
// between claim and mark).
func (w *Worker) markFailed(ctx context.Context, msg Message, now time.Time, handlerErr error) error {
	attempts, dead, found, next, err := w.markFailure(ctx, msg.ID, now, msg.Attempts)
	if err != nil {
		return err
	}
	if !found {
		w.logger.Warn("outbox: handler failed after delivery mark; ignoring",
			"message_id", msg.ID, "topic", msg.Topic, "error", handlerErr)
		return nil
	}

	w.metrics.Failed(msg.Topic)
	if dead {
		w.metrics.DeadLettered(msg.Topic, attempts)
		w.logger.Error("outbox: dead-lettered after handler failure",
			"message_id", msg.ID,
			"topic", msg.Topic,
			"attempts", attempts,
			"next_attempt_at", next,
			"error", handlerErr)
		return nil
	}
	w.logger.Warn("outbox: handler failed; scheduled retry",
		"message_id", msg.ID,
		"topic", msg.Topic,
		"attempts", attempts,
		"next_attempt_at", next,
		"error", handlerErr)
	return nil
}

// markFailure runs one short mark transaction that increments attempts for a
// live row and returns the authoritative post-update attempts and dead values,
// plus the next_attempt_at actually stored. The row is first updated with the
// backoff for the claimed attempt count; when a lease-expiry redelivery advanced
// the stored attempts, the same transaction corrects next_attempt_at from the
// returned count so the retry delay matches the row that committed. found is
// false when the row was already delivered or dead, which is benign.
func (w *Worker) markFailure(ctx context.Context, id string, now time.Time, claimedAttempts int) (attempts int, dead, found bool, next time.Time, err error) {
	expectedAttempts := claimedAttempts + 1
	err = db.WithTx(ctx, w.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		attempts, dead, found = 0, false, false
		next = now.Add(w.backoff(expectedAttempts))
		err := tx.QueryRow(ctx,
			`UPDATE outbox
			    SET attempts = attempts + 1,
			        next_attempt_at = $2,
			        dead = (attempts + 1 >= $3)
			  WHERE id = $1::uuid
			    AND delivered_at IS NULL
			    AND dead = false
			  RETURNING attempts, dead`,
			id, next, w.maxAttempts).Scan(&attempts, &dead)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if dead || attempts == expectedAttempts {
			return nil
		}
		next = now.Add(w.backoff(attempts))
		if _, err := tx.Exec(ctx,
			`UPDATE outbox
			    SET next_attempt_at = $2
			  WHERE id = $1::uuid
			    AND delivered_at IS NULL
			    AND dead = false`,
			id, next); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, false, false, time.Time{}, fmt.Errorf("outbox: mark message: %w", err)
	}
	return attempts, dead, found, next, nil
}

// deadLetterUnknownTopic removes an event with no registered handler from the
// queue immediately. It does not increment attempts because no handler ran.
func (w *Worker) deadLetterUnknownTopic(ctx context.Context, msg Message) error {
	affected, err := w.mark(ctx,
		`UPDATE outbox
		    SET dead = true
		  WHERE id = $1::uuid
		    AND delivered_at IS NULL
		    AND dead = false`,
		msg.ID)
	if err != nil {
		return err
	}
	if affected == 1 {
		w.metrics.DeadLettered(msg.Topic, msg.Attempts)
		w.logger.Error("outbox: dead-lettered unknown topic",
			"message_id", msg.ID,
			"topic", msg.Topic,
			"idempotency_key", msg.IdempotencyKey,
			"attempts", msg.Attempts)
	}
	return nil
}

// mark runs one short mark transaction and returns the affected row count.
func (w *Worker) mark(ctx context.Context, query string, args ...any) (int64, error) {
	var affected int64
	err := db.WithTx(ctx, w.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		affected = 0
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("outbox: mark message: %w", err)
	}
	return affected, nil
}

// backoff returns min(BaseDelay*2^(attempts-1), MaxDelay) for attempts >= 1.
// It saturates at MaxDelay instead of overflowing the duration.
func (w *Worker) backoff(attempts int) time.Duration {
	delay := w.baseDelay
	for i := 1; i < attempts; i++ {
		if delay >= w.maxDelay/2 {
			return w.maxDelay
		}
		delay *= 2
	}
	if delay > w.maxDelay {
		return w.maxDelay
	}
	return delay
}

// sleepContext waits for d or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
