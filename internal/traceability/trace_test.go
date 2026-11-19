// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package traceability

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestMarkerOrderIsSpecOrder(t *testing.T) {
	want := []string{
		"applied_proposal",
		"proposal",
		"case",
		"breaks",
		"agent_run",
		"agent_run.trace_id",
		"verification",
		"policy",
		"policy_config",
		"approvals",
		"executor.audit",
		"ledger_txn",
		"ledger_entries",
	}
	if !slices.Equal(markerOrder, want) {
		t.Fatalf("markerOrder = %v, want spec 5.2 order %v", markerOrder, want)
	}
}

func TestSortMarkersUsesSpecOrderAndDeduplicates(t *testing.T) {
	input := []string{
		MissingLedgerEntries,
		MissingPolicyConfig,
		MissingAppliedProposal,
		MissingAgentRunTraceID,
		MissingPolicyConfig,
		MissingAgentRun,
		MissingApprovals,
		MissingCase,
		MissingProposal,
		MissingBreaks,
		MissingVerification,
		MissingPolicy,
		MissingExecutorAudit,
		MissingLedgerTxn,
	}
	got := sortMarkers(input)
	if !slices.Equal(got, markerOrder) {
		t.Fatalf("sortMarkers = %v, want %v", got, markerOrder)
	}
}

func TestSortMarkersReturnsEmptyNonNilSlice(t *testing.T) {
	got := sortMarkers(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("sortMarkers(nil) = %#v, want an empty non-nil slice", got)
	}
}

func TestChainMissingFullT1ChainIsComplete(t *testing.T) {
	chain := completeChain(policyTierT1)
	if got := chainMissing(&chain); len(got) != 0 {
		t.Fatalf("chainMissing(complete T1) = %v, want no markers", got)
	}
}

func TestChainMissingT0ChainNeedsNoApproval(t *testing.T) {
	// T0 is auto-apply: no approvals row is complete, not missing.
	chain := completeChain(policyTierT0)
	chain.Approvals = nil
	if got := chainMissing(&chain); len(got) != 0 {
		t.Fatalf("chainMissing(complete T0, no approvals) = %v, want no markers", got)
	}
}

func TestChainMissingEveryLink(t *testing.T) {
	chain := Chain{
		Breaks:        []Break{},
		Approvals:     []Approval{},
		LedgerEntries: []LedgerEntry{},
	}
	got := chainMissing(&chain)
	want := []string{
		MissingAppliedProposal,
		MissingProposal,
		MissingCase,
		MissingBreaks,
		MissingAgentRun,
		MissingVerification,
		MissingPolicy,
		MissingExecutorAudit,
		MissingLedgerTxn,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("chainMissing(empty) = %v, want %v", got, want)
	}
}

func TestChainMissingPartialLinks(t *testing.T) {
	chain := completeChain(policyTierT1)
	chain.AgentRun.TraceID = nil
	chain.Verification = nil
	chain.Policy.ConfigYAML = nil
	chain.Policy.ConfigLoadedAt = nil
	chain.Approvals = []Approval{}
	chain.Executor.Audit = nil
	chain.LedgerEntries = []LedgerEntry{}

	got := chainMissing(&chain)
	want := []string{
		MissingAgentRunTraceID,
		MissingVerification,
		MissingPolicyConfig,
		MissingApprovals,
		MissingExecutorAudit,
		MissingLedgerEntries,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("chainMissing(partial) = %v, want %v", got, want)
	}
}

func TestChainMissingBlankTraceIDAndNullVerification(t *testing.T) {
	chain := completeChain(policyTierT0)
	blank := "   "
	chain.AgentRun.TraceID = &blank
	chain.Verification = []byte("null")

	got := chainMissing(&chain)
	want := []string{MissingAgentRunTraceID, MissingVerification}
	if !slices.Equal(got, want) {
		t.Fatalf("chainMissing(blank trace, null verification) = %v, want %v", got, want)
	}
}

func TestUnionMissingIsSortedUnion(t *testing.T) {
	chains := []Chain{
		{Missing: []string{MissingLedgerTxn, MissingPolicyConfig}},
		{Missing: []string{MissingBreaks, MissingLedgerTxn}},
	}
	got := unionMissing(chains)
	want := []string{MissingBreaks, MissingPolicyConfig, MissingLedgerTxn}
	if !slices.Equal(got, want) {
		t.Fatalf("unionMissing = %v, want %v", got, want)
	}
}

func TestSortChainsByAppliedAtThenProposalID(t *testing.T) {
	later := time.Date(2026, 11, 19, 12, 2, 0, 0, time.UTC)
	earlier := later.Add(-time.Minute)
	chains := []Chain{
		{AppliedProposal: &AppliedProposal{ProposalID: "bbbb", AppliedAt: earlier}},
		{AppliedProposal: &AppliedProposal{ProposalID: "cccc", AppliedAt: later}},
		{AppliedProposal: &AppliedProposal{ProposalID: "aaaa", AppliedAt: earlier}},
	}
	sortChains(chains)
	got := []string{
		chains[0].AppliedProposal.ProposalID,
		chains[1].AppliedProposal.ProposalID,
		chains[2].AppliedProposal.ProposalID,
	}
	want := []string{"aaaa", "bbbb", "cccc"}
	if !slices.Equal(got, want) {
		t.Fatalf("sortChains order = %v, want %v", got, want)
	}
}

func TestRootIDValidation(t *testing.T) {
	valid := "123e4567-e89b-42d3-a456-426614174000"
	if err := validateRootID("ledger txn", valid); err != nil {
		t.Fatalf("validateRootID(%q) = %v, want nil", valid, err)
	}
	for _, invalid := range []string{
		"",
		"not-a-uuid",
		"123E4567-E89B-42D3-A456-426614174000",  // uppercase is non-canonical
		"123e4567e89b42d3a456426614174000",      // no dashes
		"123e4567-e89b-42d3-a456-426614174000 ", // trailing space
	} {
		err := validateRootID("ledger txn", invalid)
		if !errors.Is(err, ErrInvalidID) {
			t.Fatalf("validateRootID(%q) = %v, want ErrInvalidID", invalid, err)
		}
	}
}

func TestByLedgerTxnRejectsMalformedIDBeforeQuery(t *testing.T) {
	if _, err := ByLedgerTxn(context.Background(), nil, "not-a-uuid"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("ByLedgerTxn malformed id = %v, want ErrInvalidID", err)
	}
}

func TestByCaseRejectsMalformedIDBeforeQuery(t *testing.T) {
	if _, err := ByCase(context.Background(), nil, "not-a-uuid"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("ByCase malformed id = %v, want ErrInvalidID", err)
	}
}

func TestPolicyRequiresApprovalByDecisionTier(t *testing.T) {
	cases := []struct {
		tier string
		want bool
	}{
		{"T0", false},
		{"T1", true},
		{"T2", true},
		{"", false},
	}
	for _, tc := range cases {
		policy := &Policy{decisionTier: tc.tier}
		if got := policyRequiresApproval(policy); got != tc.want {
			t.Fatalf("policyRequiresApproval(tier %q) = %t, want %t", tc.tier, got, tc.want)
		}
	}
}

const (
	policyTierT0 = "T0"
	policyTierT1 = "T1"
)

// completeChain builds a fully linked chain. It is synthetic on purpose: a
// valid database cannot produce some markers (proposal/case/agent_run are
// NOT NULL FKs, and FIN-10 forbids a zero-entry ledger txn), so the pure
// builder is where every marker is unit-tested.
func completeChain(tier string) Chain {
	ledgerTxnID := "11111111-1111-4111-8111-111111111111"
	configYAML := "schema_version: 1\n"
	loadedAt := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	traceID := "trace-1"
	riskTier := tier
	return Chain{
		LedgerTxnID: &ledgerTxnID,
		AppliedProposal: &AppliedProposal{
			ProposalID:    "22222222-2222-4222-8222-222222222222",
			LedgerTxnID:   &ledgerTxnID,
			AppliedAt:     loadedAt,
			ExecutorRunID: "executor-run-1",
		},
		Proposal: &Proposal{
			ID:         "22222222-2222-4222-8222-222222222222",
			CaseID:     "33333333-3333-4333-8333-333333333333",
			AgentRunID: "44444444-4444-4444-8444-444444444444",
			Type:       "adjustment",
			Status:     "applied",
			RiskTier:   &riskTier,
			CreatedAt:  loadedAt,
		},
		Case: &Case{
			ID:        "33333333-3333-4333-8333-333333333333",
			Currency:  "USD",
			Status:    "resolved",
			CreatedAt: loadedAt,
		},
		Breaks: []Break{{ID: "55555555-5555-4555-8555-555555555555"}},
		AgentRun: &AgentRun{
			ID:      "44444444-4444-4444-8444-444444444444",
			TraceID: &traceID,
		},
		Verification: []byte(`{"schema_version":1,"passed":true}`),
		Policy: &Policy{
			RiskTier:       &riskTier,
			Rule:           "default_single_approval",
			Reasons:        []string{"default_single_approval"},
			ConfigHash:     "hash",
			ConfigYAML:     &configYAML,
			ConfigLoadedAt: &loadedAt,
			decisionTier:   tier,
		},
		Approvals: []Approval{{ID: "66666666-6666-4666-8666-666666666666"}},
		Executor: Executor{Audit: &ExecutorAudit{
			Seq:       7,
			Actor:     "system:executor",
			After:     []byte(`{"ledger_txn_id":"11111111-1111-4111-8111-111111111111"}`),
			Timestamp: loadedAt,
		}},
		LedgerTxn: &LedgerTxn{
			ID:        ledgerTxnID,
			Kind:      "adjustment",
			ValueDate: "2026-11-19",
			CreatedAt: loadedAt,
			BookedAt:  loadedAt,
		},
		LedgerEntries: []LedgerEntry{{ID: "77777777-7777-4777-8777-777777777777"}},
	}
}
