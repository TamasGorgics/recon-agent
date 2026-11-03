package ledger

import "errors"

// Typed sentinel errors. Callers branch with errors.Is; the service wraps them
// with the offending key, account, currency, or transaction id.
var (
	// ErrNoTransaction means Post or Reverse was called without the caller's
	// pgx.Tx. Ledger rows and their audit entries must commit together.
	ErrNoTransaction = errors.New("ledger: caller's transaction is required")

	// ErrNoClock means the service was constructed without an injected clock.
	// Domain code never reads the wall clock (FIN-43).
	ErrNoClock = errors.New("ledger: an injected clock is required")

	// ErrInvalidRequest means the request is malformed: missing idempotency
	// key, unknown kind, zero value date, invalid UUID, or a missing actor.
	ErrInvalidRequest = errors.New("ledger: invalid request")

	// ErrEmptyTransaction means the request has no lines.
	ErrEmptyTransaction = errors.New("ledger: transaction has no lines")

	// ErrInvalidLine means a line has an unknown direction or currency, a
	// non-positive amount, or an invalid account id.
	ErrInvalidLine = errors.New("ledger: invalid line")

	// ErrUnbalanced means debits do not equal credits in one or more
	// currencies (FIN-10). It is checked in code; the DB trigger is the
	// backstop.
	ErrUnbalanced = errors.New("ledger: transaction is unbalanced")

	// ErrIdempotencyConflict means the key was already used with a different
	// canonical payload (FIN-12).
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with a different request")

	// ErrAlreadyReversed means the transaction already has a reversal. Reverse
	// creates at most one reversal per transaction; reversing a reversal is
	// allowed because that is a different transaction.
	ErrAlreadyReversed = errors.New("ledger: transaction already reversed")

	// ErrAccountNotFound means an entry references an account that does not
	// exist.
	ErrAccountNotFound = errors.New("ledger: account not found")

	// ErrAccountNotPostable means the account is inactive or closed (FIN-13).
	ErrAccountNotPostable = errors.New("ledger: account cannot be posted to")

	// ErrEntryCurrencyMismatch means an entry's currency differs from its
	// account's currency (FIN-13).
	ErrEntryCurrencyMismatch = errors.New("ledger: entry currency does not match its account")

	// ErrAdjustmentNotAllowed means an adjustment targets an account outside
	// the configured allowlist (FIN-13).
	ErrAdjustmentNotAllowed = errors.New("ledger: adjustment account is not allowlisted")

	// ErrCustomerFundsT2 means the transaction touches a customer-funds
	// account and therefore requires tier T2 approval (FIN-13). The policy
	// engine maps this error to the T2 flow.
	ErrCustomerFundsT2 = errors.New("ledger: customer-funds posting requires tier T2")

	// ErrTxnNotFound means Reverse was given a transaction id that does not
	// exist.
	ErrTxnNotFound = errors.New("ledger: transaction not found")
)
