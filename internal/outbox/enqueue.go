package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Enqueue inserts one side-effect event in the caller's transaction and reports
// whether a row was inserted. A duplicate idempotency_key is a zero-row no-op.
//
// The event is recorded with aggregate_type='outbox' and aggregate_id equal to
// idemKey. created_at and next_attempt_at use the database clock
// (CURRENT_TIMESTAMP); the Go wall clock is never read. A nil tx returns
// ErrNoTransaction, and an empty topic/idempotency key or a payload that is
// not valid JSON returns ErrInvalidMessage.
//
// Enqueue never starts, commits, or rolls back a transaction: the caller's
// commit makes the event and its state change durable together, and a caller
// rollback removes both. The insert uses ON CONFLICT (idempotency_key) DO
// NOTHING without RETURNING, so it also works through recon_executor's
// column-level conflict-target grant.
func Enqueue(ctx context.Context, tx pgx.Tx, topic string, payload json.RawMessage, idemKey string) (bool, error) {
	if err := validateEnqueue(tx, "outbox", idemKey, topic, payload, idemKey); err != nil {
		return false, err
	}
	return insert(ctx, tx, "outbox", idemKey, topic, payload, idemKey)
}

// EnqueueAggregate is Enqueue with a caller-owned aggregate for traceability,
// for example aggregateType "proposal" and aggregateID <proposal id>. It has
// the same idempotency, clock, transaction, and validation semantics as
// Enqueue.
func EnqueueAggregate(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID, topic string, payload json.RawMessage, idemKey string) (bool, error) {
	if err := validateEnqueue(tx, aggregateType, aggregateID, topic, payload, idemKey); err != nil {
		return false, err
	}
	return insert(ctx, tx, aggregateType, aggregateID, topic, payload, idemKey)
}

// insert writes one outbox row. It deliberately relies on the table's UUID
// default and the statement tag instead of RETURNING.
func insert(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID, topic string, payload json.RawMessage, idemKey string) (bool, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES ($1, $2, $3, $4::jsonb, $5, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		aggregateType, aggregateID, topic, string(payload), idemKey)
	if err != nil {
		return false, fmt.Errorf("outbox: enqueue %q: %w", topic, err)
	}
	return tag.RowsAffected() == 1, nil
}

// validateEnqueue rejects messages that cannot be delivered. Whitespace-only
// fields are empty; surrounding whitespace is preserved.
func validateEnqueue(tx pgx.Tx, aggregateType, aggregateID, topic string, payload json.RawMessage, idemKey string) error {
	if isNilTransaction(tx) {
		return ErrNoTransaction
	}
	if strings.TrimSpace(topic) == "" {
		return fmt.Errorf("%w: topic is required", ErrInvalidMessage)
	}
	if strings.TrimSpace(idemKey) == "" {
		return fmt.Errorf("%w: idempotency key is required", ErrInvalidMessage)
	}
	if strings.TrimSpace(aggregateType) == "" {
		return fmt.Errorf("%w: aggregate type is required", ErrInvalidMessage)
	}
	if strings.TrimSpace(aggregateID) == "" {
		return fmt.Errorf("%w: aggregate id is required", ErrInvalidMessage)
	}
	if len(payload) == 0 || !json.Valid(payload) {
		return fmt.Errorf("%w: payload must be valid JSON", ErrInvalidMessage)
	}
	return nil
}

// isNilTransaction also catches a typed nil pointer inside the interface, which
// would otherwise reach Exec and panic instead of failing closed.
func isNilTransaction(tx pgx.Tx) bool {
	return isNilValue(tx)
}

// isNilValue reports whether value is nil or a typed nil inside an interface,
// such as a nil pointer, func, map, slice, channel, or interface. NewWorker uses
// it to reject typed-nil dependencies and handlers before they can panic during
// Process.
func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
