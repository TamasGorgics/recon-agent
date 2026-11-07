package proposal

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

type commonArgs struct {
	CaseID     string      `json:"case_id"`
	Rationale  string      `json:"rationale"`
	Evidence   []string    `json:"evidence"`
	Confidence json.Number `json:"confidence"`
	Pattern    *string     `json:"pattern"`
}

func (a commonArgs) pattern() string {
	if a.Pattern == nil {
		return ""
	}
	return *a.Pattern
}

func decodeArguments(data json.RawMessage, dst any) error {
	if err := tools.DecodeArgs(data, dst); err != nil {
		return invalidArgumentsError("arguments are invalid")
	}
	return nil
}

// ---------------------------------------------------------------------------
// propose_link
// ---------------------------------------------------------------------------

type linkTool struct{ deps Deps }

func (*linkTool) Name() string { return "propose_link" }

func (*linkTool) Description() string {
	return "Propose linking a ledger transaction, or an exact set of ledger transactions, to an external record. Does NOT post to the ledger."
}

func (*linkTool) InputSchema() json.RawMessage { return schemaLink }

func (t *linkTool) Call(ctx context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error) {
	var request struct {
		commonArgs
		LedgerTxnID  *string   `json:"ledger_txn_id"`
		LedgerTxnIDs *[]string `json:"ledger_txn_ids"`
		ExternalRef  string    `json:"external_ref"`
	}
	if err := decodeArguments(args, &request); err != nil {
		return tools.Result{}, err
	}
	if (request.LedgerTxnID == nil) == (request.LedgerTxnIDs == nil) {
		return tools.Result{}, invalidArgumentsError("exactly one of ledger_txn_id or ledger_txn_ids is required")
	}
	record := recordRequest{
		kind:        "link",
		caseID:      request.CaseID,
		rationale:   request.Rationale,
		evidence:    request.Evidence,
		confidence:  request.Confidence,
		pattern:     request.pattern(),
		externalRef: request.ExternalRef,
	}
	if request.LedgerTxnID != nil {
		record.ledgerTxnID = *request.LedgerTxnID
	} else {
		record.ledgerTxnIDs = *request.LedgerTxnIDs
		record.linkSet = true
	}
	return t.deps.record(ctx, run, record)
}

// ---------------------------------------------------------------------------
// propose_adjustment
// ---------------------------------------------------------------------------

type adjustmentTool struct{ deps Deps }

func (*adjustmentTool) Name() string { return "propose_adjustment" }

func (*adjustmentTool) Description() string {
	return "Propose a balanced adjusting entry. Does NOT post to the ledger."
}

func (*adjustmentTool) InputSchema() json.RawMessage { return schemaAdjustment }

func (t *adjustmentTool) Call(ctx context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error) {
	var request struct {
		commonArgs
		Lines []lineInput `json:"lines"`
	}
	if err := decodeArguments(args, &request); err != nil {
		return tools.Result{}, err
	}
	return t.deps.record(ctx, run, recordRequest{
		kind:       "adjustment",
		caseID:     request.CaseID,
		rationale:  request.Rationale,
		evidence:   request.Evidence,
		confidence: request.Confidence,
		pattern:    request.pattern(),
		lines:      request.Lines,
	})
}

// ---------------------------------------------------------------------------
// propose_write_off
// ---------------------------------------------------------------------------

type writeOffTool struct{ deps Deps }

func (*writeOffTool) Name() string { return "propose_write_off" }

func (*writeOffTool) Description() string {
	return "Propose writing off a small residual to a designated account. Does NOT post to the ledger."
}

func (*writeOffTool) InputSchema() json.RawMessage { return schemaWriteOff }

func (t *writeOffTool) Call(ctx context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error) {
	var request struct {
		commonArgs
		Lines []lineInput `json:"lines"`
	}
	if err := decodeArguments(args, &request); err != nil {
		return tools.Result{}, err
	}
	return t.deps.record(ctx, run, recordRequest{
		kind:       "write_off",
		caseID:     request.CaseID,
		rationale:  request.Rationale,
		evidence:   request.Evidence,
		confidence: request.Confidence,
		pattern:    request.pattern(),
		lines:      request.Lines,
	})
}

// ---------------------------------------------------------------------------
// mark_duplicate
// ---------------------------------------------------------------------------

type duplicateTool struct{ deps Deps }

func (*duplicateTool) Name() string { return "mark_duplicate" }

func (*duplicateTool) Description() string {
	return "Flag an external record as a duplicate. Does not modify or delete the record."
}

func (*duplicateTool) InputSchema() json.RawMessage { return schemaDuplicate }

func (t *duplicateTool) Call(ctx context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error) {
	var request struct {
		commonArgs
		ExternalRef string  `json:"external_ref"`
		DuplicateOf *string `json:"duplicate_of"`
	}
	if err := decodeArguments(args, &request); err != nil {
		return tools.Result{}, err
	}
	duplicateOf := ""
	if request.DuplicateOf != nil {
		duplicateOf = *request.DuplicateOf
	}
	return t.deps.record(ctx, run, recordRequest{
		kind:        "duplicate",
		caseID:      request.CaseID,
		rationale:   request.Rationale,
		evidence:    request.Evidence,
		confidence:  request.Confidence,
		externalRef: request.ExternalRef,
		duplicateOf: duplicateOf,
	})
}

// ---------------------------------------------------------------------------
// request_info
// ---------------------------------------------------------------------------

type requestInfoTool struct{ deps Deps }

func (*requestInfoTool) Name() string { return "request_info" }

func (*requestInfoTool) Description() string {
	return "Ask a human for missing information needed to resolve the case."
}

func (*requestInfoTool) InputSchema() json.RawMessage { return schemaRequestInfo }

func (t *requestInfoTool) Call(ctx context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error) {
	var request struct {
		commonArgs
		Question string `json:"question"`
	}
	if err := decodeArguments(args, &request); err != nil {
		return tools.Result{}, err
	}
	return t.deps.record(ctx, run, recordRequest{
		kind:       "request_info",
		caseID:     request.CaseID,
		rationale:  request.Rationale,
		evidence:   request.Evidence,
		confidence: request.Confidence,
		question:   request.Question,
	})
}

// ---------------------------------------------------------------------------
// escalate
// ---------------------------------------------------------------------------

type escalateTool struct{}

func (*escalateTool) Name() string { return "escalate" }

func (*escalateTool) Description() string {
	return "Hand off to a human; no proposal is recorded."
}

func (*escalateTool) InputSchema() json.RawMessage { return schemaEscalate }

func (*escalateTool) Call(_ context.Context, run *tools.RunContext, args json.RawMessage) (tools.Result, error) {
	var request struct {
		CaseID  string `json:"case_id"`
		Reason  string `json:"reason"`
		Summary string `json:"summary"`
	}
	if err := decodeArguments(args, &request); err != nil {
		return tools.Result{}, err
	}
	if run == nil {
		return tools.Result{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "missing run context"}
	}
	caseID, err := tools.ParseUUID(request.CaseID)
	if err != nil {
		return tools.Result{}, invalidArgumentsError("case_id must be a UUID")
	}
	if !strings.EqualFold(caseID, run.CaseID) {
		return tools.Result{}, caseMismatchError()
	}
	reason := sanitizeRequired(request.Reason)
	summary := sanitizeRequired(request.Summary)
	if reason == "" {
		return tools.Result{}, invalidArgumentsError("reason is empty after sanitization")
	}
	if summary == "" {
		return tools.Result{}, invalidArgumentsError("summary is empty after sanitization")
	}
	run.State().SetEscalation(reason, summary)
	return tools.Result{Data: map[string]any{"status": "escalated"}}, nil
}
