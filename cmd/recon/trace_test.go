// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/traceability"
)

const traceTestTime = "2026-11-19T12:00:00Z"

func traceTestNow(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, traceTestTime)
	if err != nil {
		t.Fatalf("parse test time: %v", err)
	}
	return parsed
}

func runTraceForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runTraceTo(context.Background(), args, &out)
	return out.String(), err
}

func TestTraceUsageErrorsAreExitTwo(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	cases := [][]string{
		nil,
		{"--json"},
		{"--txn", ""},
		{"--txn", "   "},
		{"--txn", "not-a-uuid"},
		{"--txn", "123e4567-e89b-42d3-a456-426614174000", "extra"},
		{"--txn", "123e4567-e89b-42d3-a456-426614174000", "--bogus"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, err := runTraceForTest(t, args...)
			if err == nil {
				t.Fatal("trace usage error = nil, want exit 2")
			}
			if code := exitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2 (%v)", code, err)
			}
			if out != "" {
				t.Fatalf("usage error wrote to stdout: %q", out)
			}
		})
	}
}

func TestTraceHelp(t *testing.T) {
	out, err := runTraceForTest(t, "--help")
	if err != nil {
		t.Fatalf("trace --help: %v", err)
	}
	if !strings.Contains(out, "usage: recon trace --txn") {
		t.Fatalf("help output = %q, want the trace usage", out)
	}
}

func TestTraceRequiresAppURL(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_APP_URL", "")
	out, err := runTraceForTest(t,
		"--txn", "123e4567-e89b-42d3-a456-426614174000")
	if err == nil {
		t.Fatal("trace without database.app_url = nil, want exit 2")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "database.app_url is required") {
		t.Fatalf("error = %q, want the missing app_url message", err)
	}
	if out != "" {
		t.Fatalf("config error wrote to stdout: %q", out)
	}
}

func TestTraceConfigLoadErrorIsExitTwo(t *testing.T) {
	t.Setenv("CONFIG_FILE", "/does/not/exist/recon-trace.yaml")
	out, err := runTraceForTest(t,
		"--txn", "123e4567-e89b-42d3-a456-426614174000")
	if err == nil {
		t.Fatal("trace with an unreadable config = nil, want exit 2")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	if out != "" {
		t.Fatalf("config error wrote to stdout: %q", out)
	}
}

func TestTraceJSONHasExactlyFourTopLevelFields(t *testing.T) {
	trace := traceFixtureTrace(t)
	var out bytes.Buffer
	if err := writeTraceJSON(&out, trace); err != nil {
		t.Fatalf("writeTraceJSON: %v", err)
	}
	first := out.String()
	out.Reset()
	if err := writeTraceJSON(&out, trace); err != nil {
		t.Fatalf("writeTraceJSON second run: %v", err)
	}
	if out.String() != first {
		t.Fatalf("trace JSON is not stable:\nfirst  %s\nsecond %s", first, out.String())
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(first), &decoded); err != nil {
		t.Fatalf("decode trace JSON: %v", err)
	}
	if len(decoded) != 4 {
		t.Fatalf("top-level fields = %v, want exactly schema_version, ledger_txn, chains, missing", decoded)
	}
	for _, key := range []string{"schema_version", "ledger_txn", "chains", "missing"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("top-level JSON is missing %q: %s", key, first)
		}
	}
	var schemaVersion int
	if err := json.Unmarshal(decoded["schema_version"], &schemaVersion); err != nil {
		t.Fatalf("decode schema_version: %v", err)
	}
	if schemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", schemaVersion)
	}
}

func TestTraceJSONEmptyArrays(t *testing.T) {
	trace := traceability.Trace{
		LedgerTxn: &traceability.LedgerTxn{
			ID:        "11111111-1111-4111-8111-111111111111",
			Kind:      "adjustment",
			ValueDate: "2026-11-19",
			CreatedAt: traceTestNow(t),
			BookedAt:  traceTestNow(t),
		},
	}
	var out bytes.Buffer
	if err := writeTraceJSON(&out, trace); err != nil {
		t.Fatalf("writeTraceJSON: %v", err)
	}
	if !strings.Contains(out.String(), `"chains":[]`) {
		t.Fatalf("JSON = %s, want chains:[]", out.String())
	}
	if !strings.Contains(out.String(), `"missing":[]`) {
		t.Fatalf("JSON = %s, want missing:[]", out.String())
	}
	if strings.Contains(out.String(), `"chains":null`) || strings.Contains(out.String(), `"missing":null`) {
		t.Fatalf("JSON contains a null array: %s", out.String())
	}
}

func TestTraceHumanOutput(t *testing.T) {
	var out bytes.Buffer
	if err := writeTraceHuman(&out, traceFixtureTrace(t)); err != nil {
		t.Fatalf("writeTraceHuman: %v", err)
	}
	text := out.String()
	wantLines := []string{
		"trace: ledger txn 11111111-1111-4111-8111-111111111111",
		"  ledger_txn: seq=42 kind=adjustment value_date=2026-11-19",
		"  chains: 1",
		"  chain 1: applied proposal 22222222-2222-4222-8222-222222222222 at 2026-11-19T12:00:00Z",
		"    applied_proposal: proposal=22222222-2222-4222-8222-222222222222 ledger_txn=11111111-1111-4111-8111-111111111111 executor_run=executor-run-1",
		"    proposal: 22222222-2222-4222-8222-222222222222 type=adjustment status=applied risk_tier=T1",
		"      case: 33333333-3333-4333-8333-333333333333 status=resolved currency=USD value_minor=500",
		"        breaks: 1",
		"          break 55555555-5555-4555-8555-555555555555 side=external_only status=in_case delta_minor=500 currency=USD",
		"      agent_run: 44444444-4444-4444-8444-444444444444 model=deepseek-v4.1-flash trace_id=trace-1",
		`      verification: {"schema_version":1,"passed":true}`,
		"      policy: risk_tier=T1 rule=default_single_approval reasons=default_single_approval config_hash=abc123",
		"        config_loaded_at: 2026-11-19T12:00:00Z",
		"        config_yaml:",
		"          schema_version: 1",
		"          auto_apply:",
		"            max_value_minor:",
		"              USD: 500",
		"      approvals: 1",
		"        approval 66666666-6666-4666-8666-666666666666 decision=approved approver=Alice Approver role=approver at=2026-11-19T12:05:00Z",
		"      executor.audit: seq=7 actor=system:executor applied_at=2026-11-19T12:00:00Z ledger_txn=11111111-1111-4111-8111-111111111111 executor_run=executor-run-1",
		"      ledger_txn: 11111111-1111-4111-8111-111111111111 kind=adjustment value_date=2026-11-19",
		"        entries: 2",
		"          entry 88888888-8888-4888-8888-888888888888 direction=debit amount_minor=500 currency=USD account=fees_expense",
		"    missing: none",
		"  missing: none",
	}
	for _, line := range wantLines {
		if !strings.Contains(text, line) {
			t.Fatalf("human output is missing line %q:\n%s", line, text)
		}
	}
}

func TestTraceHumanOutputPrintsMissingMarkers(t *testing.T) {
	trace := traceFixtureTrace(t)
	chain := &trace.Chains[0]
	chain.AgentRun.TraceID = nil
	chain.Verification = nil
	chain.Policy.ConfigYAML = nil
	chain.Policy.ConfigLoadedAt = nil
	chain.Approvals = nil
	chain.Executor.Audit = nil
	chain.Missing = []string{
		traceability.MissingAgentRunTraceID,
		traceability.MissingVerification,
		traceability.MissingPolicyConfig,
		traceability.MissingApprovals,
		traceability.MissingExecutorAudit,
	}
	trace.Missing = chain.Missing

	var out bytes.Buffer
	if err := writeTraceHuman(&out, trace); err != nil {
		t.Fatalf("writeTraceHuman: %v", err)
	}
	text := out.String()
	wantLines := []string{
		"trace_id=missing (agent_run.trace_id)",
		"      verification: missing",
		"        config_yaml: missing (policy_config)",
		"      executor.audit: missing",
		"    missing: agent_run.trace_id, verification, policy_config, approvals, executor.audit",
		"  missing: agent_run.trace_id, verification, policy_config, approvals, executor.audit",
	}
	for _, line := range wantLines {
		if !strings.Contains(text, line) {
			t.Fatalf("human output is missing %q:\n%s", line, text)
		}
	}
}

func TestTraceHumanOutputUnlinkedApplication(t *testing.T) {
	trace := traceFixtureTrace(t)
	chain := &trace.Chains[0]
	chain.LedgerTxnID = nil
	chain.AppliedProposal.LedgerTxnID = nil
	chain.LedgerTxn = nil
	chain.LedgerEntries = nil
	chain.Missing = []string{traceability.MissingLedgerTxn}

	var out bytes.Buffer
	if err := writeTraceHuman(&out, trace); err != nil {
		t.Fatalf("writeTraceHuman: %v", err)
	}
	text := out.String()
	for _, line := range []string{
		"ledger_txn=none",
		"      ledger_txn: missing",
		"    missing: ledger_txn",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("human output is missing %q:\n%s", line, text)
		}
	}
}

func traceFixtureTrace(t *testing.T) traceability.Trace {
	t.Helper()
	now := traceTestNow(t)
	ledgerTxnID := "11111111-1111-4111-8111-111111111111"
	proposalID := "22222222-2222-4222-8222-222222222222"
	caseID := "33333333-3333-4333-8333-333333333333"
	runID := "44444444-4444-4444-8444-444444444444"
	breakID := "55555555-5555-4555-8555-555555555555"
	approvalID := "66666666-6666-4666-8666-666666666666"
	approverID := "77777777-7777-4777-8777-777777777777"
	riskTier := "T1"
	traceID := "trace-1"
	configYAML := "schema_version: 1\nauto_apply:\n  max_value_minor:\n    USD: 500\n"
	decisionAt := now.Add(5 * time.Minute)
	return traceability.Trace{
		LedgerTxn: &traceability.LedgerTxn{
			ID:        ledgerTxnID,
			Seq:       42,
			Kind:      "adjustment",
			ValueDate: "2026-11-19",
			CreatedAt: now,
			BookedAt:  now,
			PeriodID:  "99999999-9999-4999-8999-999999999999",
		},
		Chains: []traceability.Chain{{
			LedgerTxnID: &ledgerTxnID,
			AppliedProposal: &traceability.AppliedProposal{
				ProposalID:    proposalID,
				LedgerTxnID:   &ledgerTxnID,
				AppliedAt:     now,
				ExecutorRunID: "executor-run-1",
			},
			Proposal: &traceability.Proposal{
				ID:         proposalID,
				CaseID:     caseID,
				AgentRunID: runID,
				Type:       "adjustment",
				Status:     "applied",
				RiskTier:   &riskTier,
				CreatedAt:  now,
			},
			Case: &traceability.Case{
				ID:         caseID,
				Currency:   "USD",
				ValueMinor: 500,
				Status:     "resolved",
				CreatedAt:  now,
			},
			Breaks: []traceability.Break{{
				ID:         breakID,
				Side:       "external_only",
				Status:     "in_case",
				DeltaMinor: 500,
				Currency:   "USD",
			}},
			AgentRun: &traceability.AgentRun{
				ID:            runID,
				Model:         "deepseek-v4.1-flash",
				PromptVersion: "v1",
				StartedAt:     now,
				TraceID:       &traceID,
			},
			Verification: json.RawMessage(`{"schema_version":1,"passed":true}`),
			Policy: &traceability.Policy{
				RiskTier:       &riskTier,
				Rule:           "default_single_approval",
				Reasons:        []string{"default_single_approval"},
				ConfigHash:     "abc123",
				ConfigYAML:     &configYAML,
				ConfigLoadedAt: &now,
			},
			Approvals: []traceability.Approval{{
				ID:             approvalID,
				Decision:       "approved",
				DecidedAt:      decisionAt,
				SignatureValid: true,
				Approver: traceability.Approver{
					ID:          approverID,
					DisplayName: "Alice Approver",
					Role:        "approver",
				},
			}},
			Executor: traceability.Executor{Audit: &traceability.ExecutorAudit{
				Seq:       7,
				Timestamp: now,
				Actor:     "system:executor",
				After: json.RawMessage(
					`{"status":"applied","ledger_txn_id":"` + ledgerTxnID +
						`","applied_at":"2026-11-19T12:00:00Z","executor_run_id":"executor-run-1"}`),
			}},
			LedgerTxn: &traceability.LedgerTxn{
				ID:        ledgerTxnID,
				Seq:       42,
				Kind:      "adjustment",
				ValueDate: "2026-11-19",
				CreatedAt: now,
				BookedAt:  now,
			},
			LedgerEntries: []traceability.LedgerEntry{
				{
					ID:          "88888888-8888-4888-8888-888888888888",
					TxnID:       ledgerTxnID,
					Direction:   "debit",
					AmountMinor: 500,
					Currency:    "USD",
					AccountCode: "fees_expense",
					PostedAt:    now,
				},
				{
					ID:          "99999999-9999-4999-8999-999999999991",
					TxnID:       ledgerTxnID,
					Direction:   "credit",
					AmountMinor: 500,
					Currency:    "USD",
					AccountCode: "suspense",
					PostedAt:    now,
				},
			},
			Missing: []string{},
		}},
		Missing: []string{},
	}
}
