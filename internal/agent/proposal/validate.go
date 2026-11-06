package proposal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
)

// recordRequest is the decoded, tool-specific input to the validation
// pipeline.
type recordRequest struct {
	kind        string
	caseID      string
	rationale   string
	evidence    []string
	confidence  json.Number
	pattern     string
	lines       []lineInput
	ledgerTxnID string
	externalRef string
	duplicateOf string
	question    string
}

// record runs the ordered validation pipeline for one proposal tool. It never
// marks the run's one-proposal slot until an insert succeeds (or is an exact
// replay).
func (d Deps) record(ctx context.Context, run *tools.RunContext, req recordRequest) (tools.Result, error) {
	if run == nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "missing run context"}
	}
	// Step 2: flagged runs may only escalate.
	if err := run.State().ProposalGuard(); err != nil {
		return tools.Result{}, err
	}

	// Step 3: run/case binding.
	caseID, err := tools.ParseUUID(req.caseID)
	if err != nil {
		return tools.Result{}, invalidArgumentsError("case_id must be a UUID")
	}
	caseID = strings.ToLower(caseID)
	if !strings.EqualFold(caseID, run.CaseID) {
		return tools.Result{}, caseMismatchError()
	}
	if run.Agent == nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "proposal tools have no agent queryer"}
	}
	info, err := loadCase(ctx, run.Agent, caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tools.Result{}, caseMismatchError()
	}
	if err != nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "case lookup failed"}
	}
	if info.Status != "open" && info.Status != "in_agent" {
		return tools.Result{}, caseNotOpenError()
	}

	rationale := sanitizeRequired(req.rationale)
	if rationale == "" {
		return tools.Result{}, invalidArgumentsError("rationale is empty after sanitization")
	}
	if req.pattern != "" && req.kind != "link" && req.kind != "adjustment" && req.kind != "write_off" {
		return tools.Result{}, invalidArgumentsError("pattern is not allowed for this tool")
	}
	if req.kind == "link" {
		ledgerTxnID, err := tools.ParseUUID(req.ledgerTxnID)
		if err != nil {
			return tools.Result{}, invalidArgumentsError("ledger_txn_id must be a UUID")
		}
		req.ledgerTxnID = strings.ToLower(ledgerTxnID)
	}
	_, confidenceText, err := parseConfidence(req.confidence)
	if err != nil {
		return tools.Result{}, invalidArgumentsError("confidence must be between 0 and 1")
	}

	// Step 4: evidence.
	refs, err := validateEvidence(ctx, run, req.evidence, d.Evidence)
	if err != nil {
		return tools.Result{}, err
	}

	breaks, err := resolveBreaks(ctx, run.Agent, caseID)
	if err != nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "case break lookup failed"}
	}

	// Step 5: business rules.
	switch req.kind {
	case "link":
		if err := d.checkLink(ctx, run.Agent, req, refs, breaks); err != nil {
			return tools.Result{}, err
		}
	case "adjustment", "write_off":
		if err := d.checkLines(ctx, run.Agent, req, breaks); err != nil {
			return tools.Result{}, err
		}
	case "duplicate":
		if err := d.checkDuplicate(ctx, run.Agent, req, breaks); err != nil {
			return tools.Result{}, err
		}
	case "request_info":
		question := sanitizeRequired(req.question)
		if question == "" {
			return tools.Result{}, ruleViolation(reasonEmptyQuestion)
		}
		req.question = question
	default:
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "unknown proposal type"}
	}

	payload, err := buildPayload(req)
	if err != nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "could not build proposal payload"}
	}
	key, err := idempotencyKey(caseID, run.RunID, req.kind, payload, rationale,
		refs, confidenceText, req.pattern)
	if err != nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "could not compute idempotency key"}
	}

	// Step 6: at most one proposal per run. An exact replay of the same key
	// proceeds to the idempotent insert.
	if existing := run.State().ProposalKey(); existing != "" && existing != key {
		return tools.Result{}, proposalExistsError()
	}

	// Step 7: canonical state hash.
	state, err := ResolveState(ctx, run.Agent, caseID, refs)
	if err != nil {
		return tools.Result{}, ruleViolation(reasonUnresolvedEvidence)
	}
	stateHash, err := Hash(state)
	if err != nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "could not hash proposal state"}
	}

	// Step 8: insert through the agent pool.
	id, err := d.NewID()
	if err != nil {
		return tools.Result{}, tools.Failure("proposal could not be recorded")
	}
	proposal := Proposal{
		ID:             id,
		CaseID:         caseID,
		RunID:          run.RunID,
		Type:           req.kind,
		Payload:        payload,
		Rationale:      rationale,
		Evidence:       refs,
		Confidence:     confidenceText,
		Pattern:        req.pattern,
		StateHash:      stateHash,
		IdempotencyKey: key,
		CreatedAt:      d.Clock.Now().UTC(),
	}
	inserted, err := proposalStore{execer: d.AgentExec}.insert(ctx, proposal)
	if err != nil {
		if errors.Is(err, ErrActiveProposalExists) {
			return tools.Result{}, activeProposalError()
		}
		return tools.Result{}, tools.Failure("proposal could not be recorded")
	}
	run.State().MarkProposalUsed(key)
	status := "recorded"
	if !inserted {
		status = "already_recorded"
	}
	return tools.Result{Data: map[string]any{
		"status":          status,
		"idempotency_key": key,
	}}, nil
}

func (d Deps) checkLink(ctx context.Context, q tools.Queryer, req recordRequest, refs []string, breaks []BreakState) error {
	ledgerTxnID, err := tools.ParseUUID(req.ledgerTxnID)
	if err != nil {
		return invalidArgumentsError("ledger_txn_id must be a UUID")
	}
	ledgerTxnID = strings.ToLower(ledgerTxnID)
	source, recordID, err := splitExternalRef(req.externalRef)
	if err != nil {
		return invalidEvidenceError()
	}
	ledgerRef := "ledger_txn:" + ledgerTxnID
	if !containsString(refs, ledgerRef) || !containsString(refs, req.externalRef) {
		return ruleViolation(reasonNotInEvidence)
	}
	exists, err := ledgerTxnExists(ctx, q, ledgerTxnID)
	if err != nil {
		return &tools.CodedError{Code: tools.CodeInternalError, Message: "ledger transaction lookup failed"}
	}
	if !exists {
		return ruleViolation(reasonLedgerTxnNotFound)
	}
	recordExists, err := externalRecordExists(ctx, q, source, recordID)
	if err != nil {
		return &tools.CodedError{Code: tools.CodeInternalError, Message: "external record lookup failed"}
	}
	if !recordExists {
		return ruleViolation(reasonExternalRecordMissing)
	}
	if !ledgerTxnInCaseScope(ledgerTxnID, req.externalRef, breaks) {
		return ruleViolation(reasonNotInCaseScope)
	}
	return nil
}

func (d Deps) checkLines(ctx context.Context, q tools.Queryer, req recordRequest, breaks []BreakState) error {
	codes := make([]string, 0, len(req.lines))
	for _, line := range req.lines {
		codes = append(codes, line.AccountCode)
	}
	accounts, err := loadAccounts(ctx, q, codes)
	if err != nil {
		return &tools.CodedError{Code: tools.CodeInternalError, Message: "account lookup failed"}
	}
	return checkAdjustment(req.kind, req.lines, breaks, accounts, d.Ledger, d.WriteOffAccounts)
}

func (d Deps) checkDuplicate(ctx context.Context, q tools.Queryer, req recordRequest, breaks []BreakState) error {
	source, recordID, err := splitExternalRef(req.externalRef)
	if err != nil {
		return invalidEvidenceError()
	}
	if !containsString(req.evidence, req.externalRef) {
		return ruleViolation(reasonNotInEvidence)
	}
	exists, err := externalRecordExists(ctx, q, source, recordID)
	if err != nil {
		return &tools.CodedError{Code: tools.CodeInternalError, Message: "external record lookup failed"}
	}
	if !exists {
		return ruleViolation(reasonExternalRecordMissing)
	}
	if !externalRefInCaseScope(req.externalRef, breaks) {
		return ruleViolation(reasonNotInCaseScope)
	}
	if req.duplicateOf != "" {
		if req.duplicateOf == req.externalRef {
			return ruleViolation(reasonDuplicateOfSame)
		}
		dupSource, dupRecord, err := splitExternalRef(req.duplicateOf)
		if err != nil {
			return ruleViolation(reasonDuplicateOfInvalid)
		}
		dupExists, err := externalRecordExists(ctx, q, dupSource, dupRecord)
		if err != nil {
			return &tools.CodedError{Code: tools.CodeInternalError, Message: "duplicate_of lookup failed"}
		}
		if !dupExists {
			return ruleViolation(reasonDuplicateOfMissing)
		}
	}
	return nil
}

func parseConfidence(value json.Number) (decimal.Decimal, string, error) {
	parsed, err := decimal.NewFromString(value.String())
	if err != nil {
		return decimal.Zero, "", fmt.Errorf("proposal: confidence is not a number")
	}
	one := decimal.NewFromInt(1)
	if parsed.IsNegative() || parsed.GreaterThan(one) {
		return decimal.Zero, "", fmt.Errorf("proposal: confidence outside [0,1]")
	}
	rounded := parsed.Round(4)
	if rounded.GreaterThan(one) {
		rounded = one
	}
	return rounded, rounded.StringFixed(4), nil
}

// sanitizeRequired strips control/bidi characters and returns the trimmed
// result. The raw value is never echoed in an error.
func sanitizeRequired(text string) string {
	return strings.TrimSpace(untrusted.Sanitize(text))
}
