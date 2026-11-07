package proposal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func proposalBaseArgs() map[string]any {
	return map[string]any{
		"case_id":    unitCaseID,
		"rationale":  "fee variance on the settlement batch",
		"evidence":   []string{"case:" + unitCaseID},
		"confidence": 0.9,
	}
}

func linkArgs() map[string]any {
	args := proposalBaseArgs()
	args["ledger_txn_id"] = unitTxnID
	args["external_ref"] = "ext:psp_a:rec-1"
	return args
}

func pluralLinkArgs(ids ...string) map[string]any {
	args := proposalBaseArgs()
	args["ledger_txn_ids"] = ids
	args["external_ref"] = "ext:psp_a:rec-1"
	return args
}

func adjustmentArgs() map[string]any {
	args := proposalBaseArgs()
	args["lines"] = []map[string]any{
		{"account_code": "fees_expense_usd", "direction": "debit", "amount_minor": 100, "currency": "USD"},
		{"account_code": "suspense_usd", "direction": "credit", "amount_minor": 100, "currency": "USD"},
	}
	return args
}

func duplicateArgs() map[string]any {
	args := proposalBaseArgs()
	args["external_ref"] = "ext:psp_a:rec-1"
	return args
}

func requestInfoArgs() map[string]any {
	args := proposalBaseArgs()
	args["question"] = "Which invoice does this payout settle?"
	return args
}

func escalateArgs() map[string]any {
	return map[string]any{
		"case_id": unitCaseID,
		"reason":  "ambiguous evidence",
		"summary": "Two external records could match this payout.",
	}
}

// TestProposalSchemaRejections proves the registry rejects malformed proposal
// arguments before any handler or database access and still records the call.
//
// covers: FIN-05
func TestProposalSchemaRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "unknown field", tool: "propose_link", args: withField(linkArgs(), "extra", true)},
		{name: "link both forms", tool: "propose_link", args: withField(linkArgs(), "ledger_txn_ids", []string{unitTxnID})},
		{name: "link neither form", tool: "propose_link", args: deleteField(linkArgs(), "ledger_txn_id")},
		{name: "link empty plural", tool: "propose_link", args: pluralLinkArgs()},
		{name: "link duplicate plural", tool: "propose_link", args: pluralLinkArgs(unitTxnID, unitTxnID)},
		{name: "link case-variant duplicate plural", tool: "propose_link", args: pluralLinkArgs(unitTxnID, strings.ToUpper(unitTxnID))},
		{name: "link non-UUID plural", tool: "propose_link", args: pluralLinkArgs("not-a-uuid")},
		{name: "fractional amount", tool: "propose_adjustment", args: withField(adjustmentArgs(), "lines", []map[string]any{
			{"account_code": "fees_expense_usd", "direction": "debit", "amount_minor": 12.5, "currency": "USD"},
			{"account_code": "suspense_usd", "direction": "credit", "amount_minor": 12.5, "currency": "USD"},
		})},
		{name: "exponent amount", tool: "propose_adjustment", args: withField(adjustmentArgs(), "lines", []map[string]any{
			{"account_code": "fees_expense_usd", "direction": "debit", "amount_minor": json.Number("1e3"), "currency": "USD"},
			{"account_code": "suspense_usd", "direction": "credit", "amount_minor": json.Number("1e3"), "currency": "USD"},
		})},
		{name: "zero amount", tool: "propose_adjustment", args: withField(adjustmentArgs(), "lines", []map[string]any{
			{"account_code": "fees_expense_usd", "direction": "debit", "amount_minor": 0, "currency": "USD"},
			{"account_code": "suspense_usd", "direction": "credit", "amount_minor": 0, "currency": "USD"},
		})},
		{name: "single line", tool: "propose_adjustment", args: withField(adjustmentArgs(), "lines", []map[string]any{
			{"account_code": "fees_expense_usd", "direction": "debit", "amount_minor": 100, "currency": "USD"},
		})},
		{name: "bad direction", tool: "propose_adjustment", args: withField(adjustmentArgs(), "lines", []map[string]any{
			{"account_code": "fees_expense_usd", "direction": "both", "amount_minor": 100, "currency": "USD"},
			{"account_code": "suspense_usd", "direction": "credit", "amount_minor": 100, "currency": "USD"},
		})},
		{name: "lowercase currency", tool: "propose_adjustment", args: withField(adjustmentArgs(), "lines", []map[string]any{
			{"account_code": "fees_expense_usd", "direction": "debit", "amount_minor": 100, "currency": "usd"},
			{"account_code": "suspense_usd", "direction": "credit", "amount_minor": 100, "currency": "usd"},
		})},
		{name: "confidence above one", tool: "propose_link", args: withField(linkArgs(), "confidence", 1.01)},
		{name: "confidence below zero", tool: "propose_link", args: withField(linkArgs(), "confidence", -0.01)},
		{name: "confidence string", tool: "propose_link", args: withField(linkArgs(), "confidence", "0.5")},
		{name: "empty evidence", tool: "propose_link", args: withField(linkArgs(), "evidence", []string{})},
		{name: "missing evidence", tool: "propose_link", args: deleteField(linkArgs(), "evidence")},
		{name: "empty rationale", tool: "propose_link", args: withField(linkArgs(), "rationale", "")},
		{name: "pattern on duplicate", tool: "mark_duplicate", args: withField(duplicateArgs(), "pattern", "fee_variance")},
		{name: "pattern on request_info", tool: "request_info", args: withField(requestInfoArgs(), "pattern", "rounding")},
		{name: "pattern on escalate", tool: "escalate", args: withField(escalateArgs(), "pattern", "rounding")},
		{name: "unknown pattern", tool: "propose_link", args: withField(linkArgs(), "pattern", "vibes")},
		{name: "bad case id", tool: "propose_link", args: withField(linkArgs(), "case_id", "not-a-uuid")},
		{name: "missing case id", tool: "propose_link", args: deleteField(linkArgs(), "case_id")},
		{name: "empty external ref", tool: "propose_link", args: withField(linkArgs(), "external_ref", "")},
		{name: "escalate missing summary", tool: "escalate", args: deleteField(escalateArgs(), "summary")},
		{name: "escalate empty reason", tool: "escalate", args: withField(escalateArgs(), "reason", "")},
		{name: "escalate takes no evidence", tool: "escalate", args: withField(escalateArgs(), "evidence", []string{"case:" + unitCaseID})},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			execer := &fakeExecer{tag: pgconn.NewCommandTag("INSERT 0 1")}
			runner, recorder, run := newUnitRunner(t, newUnitDeps(execer, &fakeEvidenceReader{}))
			raw, err := runner.Call(context.Background(), run, 1, test.tool, mustJSON(t, test.args))
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			envelope := decodeEnvelope(t, raw)
			if code := envelopeErrorCode(t, envelope); code != tools.CodeInvalidArguments {
				t.Fatalf("error code = %q, want invalid_arguments: %s", code, raw)
			}
			calls := recorder.all()
			if len(calls) != 1 || calls[0].ErrorCode != tools.CodeInvalidArguments {
				t.Fatalf("recorded calls = %+v", calls)
			}
			if execer.count() != 0 {
				t.Fatalf("schema rejection reached the database: %d exec calls", execer.count())
			}
		})
	}
}

// TestProposalSchemasAcceptValidShapes proves each closed schema accepts its
// documented happy-path argument shape. A flagged run short-circuits before the
// handler needs a database, so a successful schema is observable as
// injection_flagged rather than invalid_arguments. Escalate succeeds.
func TestProposalSchemasAcceptValidShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tool string
		args map[string]any
	}{
		{tool: "propose_link", args: linkArgs()},
		{tool: "propose_link", args: pluralLinkArgs(unitTxnID)},
		{tool: "propose_link", args: pluralLinkArgs(unitTxnID, otherCaseID, unitBatchID)},
		{tool: "propose_adjustment", args: adjustmentArgs()},
		{tool: "propose_write_off", args: adjustmentArgs()},
		{tool: "mark_duplicate", args: duplicateArgs()},
		{tool: "request_info", args: requestInfoArgs()},
	}
	for _, test := range tests {
		test := test
		t.Run(test.tool, func(t *testing.T) {
			t.Parallel()
			execer := &fakeExecer{tag: pgconn.NewCommandTag("INSERT 0 1")}
			runner, _, run := newUnitRunner(t, newUnitDeps(execer, &fakeEvidenceReader{}))
			run.State().AddFlags("instruction_override")
			raw, err := runner.Call(context.Background(), run, 1, test.tool, mustJSON(t, test.args))
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			envelope := decodeEnvelope(t, raw)
			if code := envelopeErrorCode(t, envelope); code != tools.CodeInjectionFlagged {
				t.Fatalf("error code = %q, want injection_flagged (schema rejected?): %s", code, raw)
			}
		})
	}
	execer := &fakeExecer{tag: pgconn.NewCommandTag("INSERT 0 1")}
	runner, _, run := newUnitRunner(t, newUnitDeps(execer, &fakeEvidenceReader{}))
	raw, err := runner.Call(context.Background(), run, 1, "escalate", mustJSON(t, escalateArgs()))
	if err != nil {
		t.Fatalf("escalate Call() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if code := envelopeErrorCode(t, envelope); code != "" {
		t.Fatalf("escalate error code = %q: %s", code, raw)
	}
}

func withField(args map[string]any, key string, value any) map[string]any {
	copy := make(map[string]any, len(args)+1)
	for k, v := range args {
		copy[k] = v
	}
	copy[key] = value
	return copy
}

func deleteField(args map[string]any, key string) map[string]any {
	copy := make(map[string]any, len(args))
	for k, v := range args {
		if k == key {
			continue
		}
		copy[k] = v
	}
	return copy
}
