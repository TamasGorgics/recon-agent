package verify

import "errors"

// Sentinel errors returned by the service. Callers compare with errors.Is.
// Source-data problems (a missing or ambiguous external record, ledger, fee
// schedule, rate, or candidate) are not errors: they return a failed Result so
// the caller can persist the machine reason in proposals.verification.
var (
	// ErrInvalidProposalID means the proposal id is not a canonical lowercase
	// UUID and was rejected before reaching Postgres.
	ErrInvalidProposalID = errors.New("verify: invalid proposal id")
	// ErrProposalNotFound means no proposal row exists for the id.
	ErrProposalNotFound = errors.New("verify: proposal not found")
	// ErrNotVerifiable means the proposal is not in the proposed state, or a
	// concurrent transition changed it before the verifier could persist.
	ErrNotVerifiable = errors.New("verify: proposal is not verifiable")
	// ErrInvalidInput means the service or proposal data cannot be used at all
	// (for example a nil database or malformed evidence JSON). Malformed
	// proposal payload lines are a failed Result, not this error.
	ErrInvalidInput = errors.New("verify: invalid input")
)
