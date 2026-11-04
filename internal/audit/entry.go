package audit

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors returned by Append and Verify. They are sentinels so callers can
// distinguish a programming error (no transaction) from a data problem.
var (
	// ErrInvalidEntry means the Entry is incomplete (invalid actor, empty
	// action/entity fields, or a missing timestamp).
	ErrInvalidEntry = errors.New("audit: invalid entry")
	// ErrNoTransaction means Append was called without the caller's pgx.Tx.
	// Audit rows must join the caller's transaction so a rollback removes them.
	ErrNoTransaction = errors.New("audit: Append requires the caller's transaction")
	// ErrFromSeqBeyondTail means Verify was asked to start after the known head
	// of the chain (more than one past it).
	ErrFromSeqBeyondTail = errors.New("audit: fromSeq is beyond the chain tail")
)

// Entry is one audit record. Before and After carry IDs and amounts only (and
// never personal data); see the package documentation. Numbers in the payloads
// must be integer minor units or JSON strings, matching FIN-05. Timestamp is
// required and must come from the caller's injected clock (FIN-43); Append
// never reads the wall clock.
type Entry struct {
	// Actor is the typed actor that performed the action (FIN-33).
	Actor Actor
	// Action is the domain verb, for example "ledger.post" or "match.run".
	Action string
	// EntityType names the mutated object's type.
	EntityType string
	// EntityID identifies the mutated object (a UUID string or logical id).
	EntityID string
	// Before is the JSON payload before the change. Nil becomes JSON null.
	Before any
	// After is the JSON payload after the change. Nil becomes JSON null.
	After any
	// TraceID is nullable in M1 because there is no tracing yet.
	TraceID *string
	// Timestamp is the UTC event time, truncated to microsecond precision.
	Timestamp time.Time
}

// Record is the row Append wrote.
type Record struct {
	// ID is the audit_log row id assigned by the database.
	ID string
	// Seq is the contiguous chain position, starting at 1.
	Seq int64
	// Timestamp is the stored UTC timestamp (microsecond precision).
	Timestamp time.Time
	// PrevHash is the previous row's row_hash, or 32 zero bytes for genesis.
	PrevHash []byte
	// RowHash is the SHA-256 hash of this row's canonical fields.
	RowHash []byte
}

func (e Entry) validate() error {
	if !e.Actor.Valid() {
		return fmt.Errorf("%w: actor is not valid", ErrInvalidEntry)
	}
	if strings.TrimSpace(e.Action) == "" {
		return fmt.Errorf("%w: action is empty", ErrInvalidEntry)
	}
	if strings.TrimSpace(e.EntityType) == "" {
		return fmt.Errorf("%w: entity type is empty", ErrInvalidEntry)
	}
	if strings.TrimSpace(e.EntityID) == "" {
		return fmt.Errorf("%w: entity id is empty", ErrInvalidEntry)
	}
	if e.Timestamp.IsZero() {
		return fmt.Errorf("%w: timestamp is required", ErrInvalidEntry)
	}
	return nil
}
