package proposal

import (
	"errors"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// Sentinel errors. Handlers return them wrapped in a *tools.CodedError with a
// stable code and a message that is safe to show the model.
var (
	// ErrActiveProposalExists means the case already has an active proposal;
	// the partial unique index proposals_one_active_per_case rejected the row.
	ErrActiveProposalExists = errors.New("proposal: case already has an active proposal")
	// ErrProposalExists means this run already recorded a different proposal.
	ErrProposalExists = errors.New("proposal: run already recorded a different proposal")
	// ErrEvidenceNotRetrieved means a cited ref was not returned by a
	// successful tool call in this run.
	ErrEvidenceNotRetrieved = errors.New("proposal: evidence was not retrieved in this run")
	// ErrBusinessRule means a deterministic money, account or scope rule
	// failed.
	ErrBusinessRule = errors.New("proposal: business rule violation")
	// ErrCaseMismatch means case_id does not equal the run's case or the case
	// row is missing.
	ErrCaseMismatch = errors.New("proposal: case does not match the run")
	// ErrCaseNotOpen means the case state cannot accept a proposal.
	ErrCaseNotOpen = errors.New("proposal: case is not open")
	// ErrInvalidEvidence means an evidence string is malformed or unknown.
	ErrInvalidEvidence = errors.New("proposal: invalid evidence reference")
)

// Short, stable business-rule reason codes. They are safe constants and are
// the only detail added to the model-visible message.
const (
	reasonUnbalanced            = "unbalanced"
	reasonCurrencyMismatch      = "currency_mismatch"
	reasonCurrencyInvalid       = "currency_invalid"
	reasonAccountNotFound       = "account_not_found"
	reasonAccountInactive       = "account_inactive"
	reasonAccountNotAllowed     = "account_not_allowed"
	reasonExceedsDelta          = "exceeds_delta"
	reasonNotInCaseScope        = "not_in_case_scope"
	reasonWriteOffRequired      = "write_off_account_required"
	reasonLedgerTxnNotFound     = "ledger_txn_not_found"
	reasonExternalRecordMissing = "external_record_not_found"
	reasonDuplicateOfInvalid    = "duplicate_of_invalid"
	reasonDuplicateOfMissing    = "duplicate_of_not_found"
	reasonDuplicateOfSame       = "duplicate_of_same_record"
	reasonNotInEvidence         = "not_in_evidence"
	reasonUnresolvedEvidence    = "unresolved_evidence"
	reasonEmptyQuestion         = "empty_question"
)

func coded(code, message string, cause error) error {
	return &tools.CodedError{Code: code, Message: message, Err: cause}
}

func caseMismatchError() error {
	return coded(tools.CodeCaseMismatch, "case_id does not match the run", ErrCaseMismatch)
}

func caseNotOpenError() error {
	return coded(tools.CodeCaseNotOpen, "case state cannot accept a proposal", ErrCaseNotOpen)
}

func evidenceNotRetrievedError() error {
	return coded(tools.CodeEvidenceNotRetrieved,
		"evidence was not returned by a successful call in this run", ErrEvidenceNotRetrieved)
}

func invalidEvidenceError() error {
	return coded(tools.CodeInvalidEvidence, "evidence contains an invalid reference", ErrInvalidEvidence)
}

func proposalExistsError() error {
	return coded(tools.CodeProposalAlreadyExists,
		"run already recorded a different proposal", ErrProposalExists)
}

func activeProposalError() error {
	return coded(tools.CodeActiveProposalExists,
		"case already has an active proposal", ErrActiveProposalExists)
}

func invalidArgumentsError(message string) error {
	return &tools.CodedError{Code: tools.CodeInvalidArguments, Message: message}
}

// ruleViolation returns a business_rule_violation CodedError carrying only a
// short reason code, never DB text or a field value.
func ruleViolation(reason string) error {
	return coded(tools.CodeBusinessRuleViolation, "business rule violation: "+reason, ErrBusinessRule)
}
