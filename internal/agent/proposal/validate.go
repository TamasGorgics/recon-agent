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

	// Step 4: argument normalization. These values feed the idempotency key, so
	// they must be settled before the replay lookup.
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
	if req.kind == "request_info" {
		// Sanitize now because the question is part of the key; the empty
		// check stays in the business-rule stage to preserve pipeline order.
		req.question = sanitizeRequired(req.question)
	}

	// Step 5: evidence format. The database authority check runs after the
	// replay lookup.
	refs, err := canonicalEvidence(req.evidence)
	if err != nil {
		return tools.Result{}, err
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

	// Step 6: exact idempotency-key replay wins over case-open and business
	// validation. The case may already have moved to proposal_pending while
	// the run is replayed in a new process; the stored row is the result.
	replayed, err := proposalStore{execer: d.AgentExec}.existsByKey(ctx, run.Agent, key)
	if err != nil {
		return tools.Result{}, tools.Failure("proposal could not be recorded")
	}
	if replayed {
		run.State().MarkProposalUsed(key)
		return tools.Result{Data: map[string]any{
			"status":          "already_recorded",
			"idempotency_key": key,
		}}, nil
	}

	// Step 7: run/case state.
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

	// Step 8: evidence must come from a successful, non-truncated tool call in
	// this run.
	if err := checkEvidenceRetrieved(ctx, run, refs, d.Evidence); err != nil {
		return tools.Result{}, err
	}

	breaks, err := resolveBreaks(ctx, run.Agent, caseID)
	if err != nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "case break lookup failed"}
	}

	// Step 9: business rules.
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
		if req.question == "" {
			return tools.Result{}, ruleViolation(reasonEmptyQuestion)
		}
	default:
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "unknown proposal type"}
	}

	// Step 10: at most one proposal per run. The durable authority is the
	// proposals_one_proposal_per_run unique index; this in-memory slot only
	// short-circuits the same process before another database round trip.
	if existing := run.State().ProposalKey(); existing != "" && existing != key {
		return tools.Result{}, proposalExistsError()
	}

	// Step 11: canonical state hash.
	state, err := ResolveState(ctx, run.Agent, caseID, refs)
	if err != nil {
		return tools.Result{}, ruleViolation(reasonUnresolvedEvidence)
	}
	stateHash, err := Hash(state)
	if err != nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "could not hash proposal state"}
	}

	// Step 12: insert through the agent pool.
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
		if errors.Is(err, ErrProposalExists) {
			return tools.Result{}, proposalExistsError()
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
