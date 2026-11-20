package slack

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Store sentinels. A missing message is not an error for Get, but callers that
// require a row can use GetRequired and test ErrMessageNotFound. Malformed
// identifiers and values fail closed before any SQL reaches Postgres.
var (
	ErrInvalidMessage  = errors.New("approval/slack: invalid approval message")
	ErrMessageNotFound = errors.New("approval/slack: approval message not found")
)

// MessageState is the closed approval_messages.state set. A posted row is the
// only state that may transition to updated; cancelled is terminal.
type MessageState string

const (
	// StatePosted means chat.postMessage succeeded and the external ref is stored.
	StatePosted MessageState = "posted"
	// StateUpdated means the message was edited after a decision.
	StateUpdated MessageState = "updated"
	// StateCancelled means the request was withdrawn or superseded.
	StateCancelled MessageState = "cancelled"
)

// Valid reports whether s is one of the three stored states.
func (s MessageState) Valid() bool {
	switch s {
	case StatePosted, StateUpdated, StateCancelled:
		return true
	default:
		return false
	}
}

// Message is one approval_messages row. ExternalRef is the opaque
// "<channel_id>:<ts>" Slack reference; this store never parses it.
type Message struct {
	ProposalID  string
	Channel     string
	ExternalRef string
	State       MessageState
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Queryer is the narrow database surface the store needs. Both *pgxpool.Pool
// and pgx.Tx satisfy it. Callers must not keep a network call open while a
// transaction is held (FIN-22); the store itself only executes one statement.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store reads and writes approval message transport state. It is stateless and
// safe to share; created_at/updated_at are written with the database clock
// (now()) so API and outbox processes cannot disagree on time.
type Store struct{}

var canonicalUUIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Get loads one row. A missing row is (Message{}, false, nil), per the frozen
// store contract; a database or validation failure is an error.
func (s *Store) Get(ctx context.Context, q Queryer, proposalID, channel string) (Message, bool, error) {
	if err := validateGet(q, proposalID, channel); err != nil {
		return Message{}, false, err
	}

	var (
		message Message
		state   string
	)
	err := q.QueryRow(ctx,
		`SELECT proposal_id::text, channel, external_ref, state, created_at, updated_at
		   FROM approval_messages
		  WHERE proposal_id = $1::uuid AND channel = $2`,
		proposalID, channel).
		Scan(&message.ProposalID, &message.Channel, &message.ExternalRef,
			&state, &message.CreatedAt, &message.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf(
			"approval/slack: get message for proposal %s channel %q: %w",
			proposalID, channel, err)
	}
	message.State = MessageState(state)
	if !message.State.Valid() {
		return Message{}, false, fmt.Errorf(
			"approval/slack: stored message for proposal %s channel %q has invalid state %q",
			proposalID, channel, state)
	}
	return message, true, nil
}

// GetRequired is Get with a typed ErrMessageNotFound instead of a found flag.
// The interaction handler uses it when a click must match a stored message.
func (s *Store) GetRequired(ctx context.Context, q Queryer, proposalID, channel string) (Message, error) {
	message, found, err := s.Get(ctx, q, proposalID, channel)
	if err != nil {
		return Message{}, err
	}
	if !found {
		return Message{}, fmt.Errorf("%w: proposal %s channel %q",
			ErrMessageNotFound, proposalID, channel)
	}
	return message, nil
}

// InsertPosted records a message after a successful chat.postMessage. A
// duplicate (proposal_id, channel) is a no-op: inserted is false and the
// existing row is unchanged. created_at/updated_at use the database clock.
func (s *Store) InsertPosted(ctx context.Context, q Queryer, proposalID, channel, externalRef string) (bool, error) {
	if err := validateInsert(q, proposalID, channel, externalRef); err != nil {
		return false, err
	}

	tag, err := q.Exec(ctx,
		`INSERT INTO approval_messages
		     (proposal_id, channel, external_ref, state, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, 'posted', now(), now())
		 ON CONFLICT (proposal_id, channel) DO NOTHING`,
		proposalID, channel, externalRef)
	if err != nil {
		return false, fmt.Errorf(
			"approval/slack: insert posted message for proposal %s channel %q: %w",
			proposalID, channel, err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkPosted is the frozen store-contract name (spec section 4.2) for
// InsertPosted. Callers may use either name.
func (s *Store) MarkPosted(ctx context.Context, q Queryer, proposalID, channel, externalRef string) (bool, error) {
	return s.InsertPosted(ctx, q, proposalID, channel, externalRef)
}

// MarkUpdated moves a posted row to updated. An already-updated or cancelled
// row is an idempotent no-op (changed=false) because cancellation is terminal.
func (s *Store) MarkUpdated(ctx context.Context, q Queryer, proposalID, channel string) (bool, error) {
	if err := validateTransition(q, proposalID, channel); err != nil {
		return false, err
	}

	tag, err := q.Exec(ctx,
		`UPDATE approval_messages
		    SET state = 'updated', updated_at = now()
		  WHERE proposal_id = $1::uuid AND channel = $2 AND state = 'posted'`,
		proposalID, channel)
	if err != nil {
		return false, fmt.Errorf(
			"approval/slack: mark message updated for proposal %s channel %q: %w",
			proposalID, channel, err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkCancelled moves a posted or updated row to cancelled. A missing or
// already-cancelled row is an idempotent no-op (changed=false).
func (s *Store) MarkCancelled(ctx context.Context, q Queryer, proposalID, channel string) (bool, error) {
	if err := validateTransition(q, proposalID, channel); err != nil {
		return false, err
	}

	tag, err := q.Exec(ctx,
		`UPDATE approval_messages
		    SET state = 'cancelled', updated_at = now()
		  WHERE proposal_id = $1::uuid AND channel = $2
		    AND state IN ('posted', 'updated')`,
		proposalID, channel)
	if err != nil {
		return false, fmt.Errorf(
			"approval/slack: mark message cancelled for proposal %s channel %q: %w",
			proposalID, channel, err)
	}
	return tag.RowsAffected() == 1, nil
}

func validateGet(q Queryer, proposalID, channel string) error {
	if err := validateQueryer(q); err != nil {
		return err
	}
	return validateKey(proposalID, channel)
}

func validateInsert(q Queryer, proposalID, channel, externalRef string) error {
	if err := validateGet(q, proposalID, channel); err != nil {
		return err
	}
	if strings.TrimSpace(externalRef) == "" {
		return fmt.Errorf("%w: external ref is required", ErrInvalidMessage)
	}
	return nil
}

func validateTransition(q Queryer, proposalID, channel string) error {
	return validateGet(q, proposalID, channel)
}

func validateKey(proposalID, channel string) error {
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return fmt.Errorf("%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidMessage, proposalID)
	}
	if channel == "" {
		return fmt.Errorf("%w: channel is required", ErrInvalidMessage)
	}
	return nil
}

func validateQueryer(q Queryer) error {
	if q == nil {
		return fmt.Errorf("%w: nil queryer", ErrInvalidMessage)
	}
	value := reflect.ValueOf(q)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: nil queryer", ErrInvalidMessage)
		}
	}
	return nil
}
