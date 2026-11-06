package proposal

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// TestFlaggedRunBlocksProposalsAndAllowsEscalate verifies the M2-07 flagged
// contract at the proposal boundary: every proposal tool is rejected before
// any database access, while escalate succeeds and is sanitized.
func TestFlaggedRunBlocksProposalsAndAllowsEscalate(t *testing.T) {
	t.Parallel()
	execer := &fakeExecer{tag: pgconn.NewCommandTag("INSERT 0 1")}
	runner, recorder, run := newUnitRunner(t, newUnitDeps(execer, &fakeEvidenceReader{}))
	run.State().AddFlags("instruction_override", "exfiltration")

	proposals := []struct {
		tool string
		args map[string]any
	}{
		{tool: "propose_link", args: linkArgs()},
		{tool: "propose_adjustment", args: adjustmentArgs()},
		{tool: "propose_write_off", args: adjustmentArgs()},
		{tool: "mark_duplicate", args: duplicateArgs()},
		{tool: "request_info", args: requestInfoArgs()},
	}
	for i, test := range proposals {
		raw, err := runner.Call(context.Background(), run, i+1, test.tool, mustJSON(t, test.args))
		if err != nil {
			t.Fatalf("%s Call() error = %v", test.tool, err)
		}
		envelope := decodeEnvelope(t, raw)
		if code := envelopeErrorCode(t, envelope); code != tools.CodeInjectionFlagged {
			t.Fatalf("%s error code = %q, want injection_flagged: %s", test.tool, code, raw)
		}
	}
	if execer.count() != 0 {
		t.Fatalf("flagged proposal reached the database: %d exec calls", execer.count())
	}

	args := escalateArgs()
	args["reason"] = "  ambiguous\u200b evidence  "
	args["summary"] = "Two records match.\u200b"
	raw, err := runner.Call(context.Background(), run, 6, "escalate", mustJSON(t, args))
	if err != nil {
		t.Fatalf("escalate Call() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if code := envelopeErrorCode(t, envelope); code != "" {
		t.Fatalf("escalate error code = %q: %s", code, raw)
	}
	data := envelopeData(t, envelope)
	if data["status"] != "escalated" {
		t.Fatalf("escalate status = %v, want escalated", data["status"])
	}
	escalation := run.State().Escalation()
	if escalation == nil || escalation.Reason != "ambiguous evidence" {
		t.Fatalf("escalation = %+v, want sanitized reason", escalation)
	}
	if execer.count() != 0 {
		t.Fatalf("escalate wrote through the agent pool: %d exec calls", execer.count())
	}
	if len(recorder.all()) != 6 {
		t.Fatalf("recorded calls = %d, want 6", len(recorder.all()))
	}
}

func TestEscalateRejectsWrongCase(t *testing.T) {
	t.Parallel()
	execer := &fakeExecer{}
	runner, _, run := newUnitRunner(t, newUnitDeps(execer, &fakeEvidenceReader{}))
	args := escalateArgs()
	args["case_id"] = otherCaseID
	raw, err := runner.Call(context.Background(), run, 1, "escalate", mustJSON(t, args))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if code := envelopeErrorCode(t, decodeEnvelope(t, raw)); code != tools.CodeCaseMismatch {
		t.Fatalf("error code = %q, want case_mismatch: %s", code, raw)
	}
	if run.State().Escalation() != nil {
		t.Fatal("case-mismatched escalate set the terminal state")
	}
}
