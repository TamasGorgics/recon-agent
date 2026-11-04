package ledger

import (
	"time"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

// Audit actions and the entity type written by this package (FIN-30).
const (
	ActionPost    = "ledger.post"
	ActionReverse = "ledger.reverse"
	EntityTxn     = "ledger_txn"
)

// Kind is the ledger transaction kind. The values match the ledger_txns.kind
// CHECK constraint.
type Kind string

// Transaction kinds accepted by Post.
const (
	KindCharge     Kind = "charge"
	KindRefund     Kind = "refund"
	KindPayout     Kind = "payout"
	KindFee        Kind = "fee"
	KindChargeback Kind = "chargeback"
	KindFX         Kind = "fx"
	KindAdjustment Kind = "adjustment"
	KindReversal   Kind = "reversal"
)

func (k Kind) valid() bool {
	switch k {
	case KindCharge, KindRefund, KindPayout, KindFee, KindChargeback, KindFX, KindAdjustment, KindReversal:
		return true
	default:
		return false
	}
}

// Direction is a double-entry side.
type Direction string

// Entry directions.
const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

func (d Direction) opposite() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

func (d Direction) valid() bool {
	return d == Debit || d == Credit
}

// Line is one side of a double-entry transaction. AmountMinor is always
// positive; Direction carries the sign, matching ledger_entries. Amounts are
// integer minor units and never floating point (FIN-01, FIN-05).
type Line struct {
	// AccountID is the accounts.id UUID.
	AccountID string
	// Direction is debit or credit.
	Direction Direction
	// AmountMinor is the positive amount in the currency's minor units.
	AmountMinor int64
	// Currency is the ISO 4217 code; it must equal the account's currency.
	Currency string
	// ExternalRef optionally links the line to a source record.
	ExternalRef string
	// Metadata is optional JSON metadata. Numbers must be integer minor units
	// or strings (FIN-05).
	Metadata map[string]any
}

// PostRequest is the input to Service.Post. IdempotencyKey identifies the
// operation; a replay with the same canonical payload returns the original
// transaction (FIN-12).
//
// ID, CreatedAt, and BookedAt are optional caller-supplied values for the
// deterministic generator (D-7). When absent the service uses a UUIDv4 and the
// injected clock's Now.
type PostRequest struct {
	IdempotencyKey string
	Kind           Kind
	ValueDate      time.Time
	Description    string
	Lines          []Line
	Actor          audit.Actor

	ID        string
	CreatedAt time.Time
	BookedAt  time.Time

	// ReversalOf is set by Service.Reverse. Callers must not set it directly;
	// Post rejects reversal_of on non-reversal kinds.
	ReversalOf string
}

// ReverseRequest is the input to Service.Reverse. The reversal reuses the
// original transaction's value date and entries, so a closed original period is
// handled by the normal period selection (FIN-56).
type ReverseRequest struct {
	TxnID          string
	Reason         string
	IdempotencyKey string
	Actor          audit.Actor

	// Optional caller-supplied values for the reversal transaction (D-7).
	ID        string
	CreatedAt time.Time
	BookedAt  time.Time
}

// Result describes a posted (or replayed) transaction. Replayed is true when an
// existing idempotency key returned a previously committed transaction and no
// new rows were written.
type Result struct {
	ID              string
	Seq             int64
	Kind            Kind
	ValueDate       time.Time
	CreatedAt       time.Time
	BookedAt        time.Time
	PeriodID        string
	AdjustsPeriodID *string
	ReversalOf      *string
	IdempotencyKey  string
	RequestHash     string
	Replayed        bool
}

// Service posts and reverses ledger transactions. It is stateless apart from
// the injected clock and account policy, so it is safe for concurrent use.
type Service struct {
	Clock  clock.Clock
	Config config.LedgerConfig
}

// NewService returns a ledger service. clk must be non-nil; domain code must
// never fall back to time.Now (FIN-43).
func NewService(clk clock.Clock, cfg config.LedgerConfig) *Service {
	return &Service{Clock: clk, Config: cfg}
}
