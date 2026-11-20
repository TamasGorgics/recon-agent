// covers: FIN-33
package approver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

const (
	testCaseID      = "11111111-1111-4111-8111-111111111111"
	testScopeID     = "22222222-2222-4222-8222-222222222222"
	testProposalID  = "33333333-3333-4333-8333-333333333333"
	testLedgerID    = "44444444-4444-4444-8444-444444444444"
	testOtherLedger = "66666666-6666-4666-8666-666666666666"
	testExternalRef = "ext:psp_a:rec-1"
	testExternalTwo = "ext:psp_a:rec-2"
)

type decideCall struct {
	proposalID string
	decision   approval.Decision
	approverID string
	source     string
}

type fakeDecider struct {
	calls    []decideCall
	err      error
	result   approval.DecisionResult
	resultFn func(proposalID string, d approval.Decision, approverID, source string) (approval.DecisionResult, error)
}

func (f *fakeDecider) Decide(_ context.Context, proposalID string, d approval.Decision,
	approverID, source string) (approval.DecisionResult, error) {
	f.calls = append(f.calls, decideCall{
		proposalID: proposalID,
		decision:   d,
		approverID: approverID,
		source:     source,
	})
	if f.resultFn != nil {
		return f.resultFn(proposalID, d, approverID, source)
	}
	if f.err != nil {
		return approval.DecisionResult{}, f.err
	}
	result := f.result
	if result.ProposalID == "" {
		result.ProposalID = proposalID
	}
	if result.Decision == "" {
		result.Decision = d
	}
	if result.ApproverID == "" {
		result.ApproverID = approverID
	}
	if result.Status == "" {
		if d == approval.DecisionRejected {
			result.Status = statusRejected
		} else {
			result.Status = statusApproved
		}
	}
	return result, nil
}

type fakeRecords struct {
	ledger        map[string]LedgerRecord
	external      map[string]ExternalRecord
	ledgerErr     error
	externalErr   error
	ledgerCalls   int
	externalCalls int
}

func (f *fakeRecords) Ledger(_ context.Context, ids []string) ([]LedgerRecord, error) {
	f.ledgerCalls++
	if f.ledgerErr != nil {
		return nil, f.ledgerErr
	}
	records := make([]LedgerRecord, 0, len(ids))
	for _, id := range ids {
		if record, ok := f.ledger[id]; ok {
			records = append(records, record)
		}
	}
	return records, nil
}

func (f *fakeRecords) External(_ context.Context, ids []string) ([]ExternalRecord, error) {
	f.externalCalls++
	if f.externalErr != nil {
		return nil, f.externalErr
	}
	records := make([]ExternalRecord, 0, len(ids))
	for _, id := range ids {
		if record, ok := f.external[id]; ok {
			records = append(records, record)
		}
	}
	return records, nil
}

func newTestApprover(t *testing.T, decider Decider, records RecordSource) *Approver {
	t.Helper()
	a, err := New(decider, records, Config{
		ApproverID: SyntheticApproverID,
		Source:     SyntheticSource,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func pendingRequest(tier policy.Tier) approval.RequestResult {
	return approval.RequestResult{
		ProposalID: testProposalID,
		Tier:       tier,
		Status:     statusPendingApproval,
		Channel:    "slack",
	}
}

func adjustmentFacts() (CaseFacts, ProposalFacts, *fakeRecords) {
	return CaseFacts{
		CaseID:       testCaseID,
		ScopeID:      testScopeID,
		BreakKind:    "fee_variance",
		DeltaMinor:   1250,
		Currency:     "USD",
		Counterparty: "psp_a",
	}, ProposalFacts{
		ProposalID:   testProposalID,
		Type:         "adjustment",
		Resolution:   "adjustment",
		AmountMinor:  1250,
		Currency:     "USD",
		Counterparty: "psp_a",
	}, &fakeRecords{}
}

func linkFacts() (CaseFacts, ProposalFacts, *fakeRecords) {
	return CaseFacts{
		CaseID:       testCaseID,
		ScopeID:      testScopeID,
		BreakKind:    "reference_typo",
		DeltaMinor:   1000,
		Currency:     "USD",
		Counterparty: "psp_a",
	}, ProposalFacts{
		ProposalID:   testProposalID,
		Type:         "link",
		Resolution:   "link",
		AmountMinor:  1000,
		Currency:     "USD",
		LedgerRefs:   []string{testLedgerID},
		ExternalRefs: []string{testExternalRef},
		Counterparty: "psp_a",
	}, &fakeRecords{
		ledger: map[string]LedgerRecord{
			testLedgerID: {
				ID:          testLedgerID,
				ScopeID:     testScopeID,
				AmountMinor: 1000,
				Currency:    "USD",
			},
		},
		external: map[string]ExternalRecord{
			testExternalRef: {
				ID:           testExternalRef,
				ScopeID:      testScopeID,
				AmountMinor:  1000,
				Currency:     "USD",
				Counterparty: "psp_a",
			},
		},
	}
}

func writeOffFacts() (CaseFacts, ProposalFacts, *fakeRecords) {
	return CaseFacts{
		CaseID:       testCaseID,
		ScopeID:      testScopeID,
		BreakKind:    "rounding",
		DeltaMinor:   300,
		Currency:     "USD",
		Counterparty: "psp_a",
	}, ProposalFacts{
		ProposalID:   testProposalID,
		Type:         "write_off",
		Resolution:   "write_off",
		AmountMinor:  300,
		Currency:     "USD",
		Counterparty: "psp_a",
	}, &fakeRecords{}
}

func duplicateFacts() (CaseFacts, ProposalFacts, *fakeRecords) {
	return CaseFacts{
		CaseID:       testCaseID,
		ScopeID:      testScopeID,
		BreakKind:    "duplicate_charge",
		DeltaMinor:   900,
		Currency:     "EUR",
		Counterparty: "psp_a",
	}, ProposalFacts{
		ProposalID:   testProposalID,
		Type:         "duplicate",
		Resolution:   "duplicate",
		AmountMinor:  900,
		Currency:     "EUR",
		ExternalRefs: []string{testExternalRef, testExternalTwo},
		Counterparty: "psp_a",
	}, &fakeRecords{
		external: map[string]ExternalRecord{
			testExternalRef: {
				ID:           testExternalRef,
				ScopeID:      testScopeID,
				AmountMinor:  900,
				Currency:     "EUR",
				Counterparty: "psp_a",
			},
			testExternalTwo: {
				ID:           testExternalTwo,
				ScopeID:      testScopeID,
				AmountMinor:  900,
				Currency:     "EUR",
				Counterparty: "psp_a",
			},
		},
	}
}

func TestActDecisionTable(t *testing.T) {
	adjustmentCase, adjustmentProposal, adjustmentRecords := adjustmentFacts()
	baseline := pendingRequest(policy.TierT1)

	tests := []struct {
		name         string
		req          approval.RequestResult
		caseFacts    CaseFacts
		proposal     ProposalFacts
		records      RecordSource
		deciderErr   error
		wantAction   Action
		wantRule     string
		wantCalls    int
		wantDecision approval.Decision
		wantErr      error
	}{
		{
			name:       "T0 tier skips with no facts",
			req:        approval.RequestResult{Tier: policy.TierT0, Status: statusPendingApproval, ProposalID: testProposalID},
			wantAction: ActionSkipT0,
			wantRule:   RuleT0AutoApproved,
		},
		{
			name:       "auto-approved status skips at T1",
			req:        approval.RequestResult{Tier: policy.TierT1, Status: statusAutoApproved, ProposalID: testProposalID},
			wantAction: ActionSkipT0,
			wantRule:   RuleT0AutoApproved,
		},
		{
			name:       "T0 tier skips even if status is pending",
			req:        approval.RequestResult{Tier: policy.TierT0, Status: statusPendingApproval, ProposalID: testProposalID},
			wantAction: ActionSkipT0,
			wantRule:   RuleT0AutoApproved,
		},
		{
			name:       "no proposal abstains without facts",
			req:        baseline,
			wantAction: ActionAbstain,
			wantRule:   RuleNoProposal,
		},
		{
			name:         "T1 exact match approves",
			req:          baseline,
			caseFacts:    adjustmentCase,
			proposal:     adjustmentProposal,
			records:      adjustmentRecords,
			wantAction:   ActionApprove,
			wantRule:     RuleExactMatch,
			wantCalls:    1,
			wantDecision: approval.DecisionApproved,
		},
		{
			name:         "T2 exact match approves on the same path",
			req:          pendingRequest(policy.TierT2),
			caseFacts:    adjustmentCase,
			proposal:     adjustmentProposal,
			records:      adjustmentRecords,
			wantAction:   ActionApprove,
			wantRule:     RuleExactMatch,
			wantCalls:    1,
			wantDecision: approval.DecisionApproved,
		},
		{
			name:       "non-pending terminal status fails closed",
			req:        approval.RequestResult{ProposalID: testProposalID, Tier: policy.TierT1, Status: statusApproved},
			caseFacts:  adjustmentCase,
			proposal:   adjustmentProposal,
			records:    adjustmentRecords,
			wantErr:    ErrNotPending,
			wantAction: "",
		},
		{
			name:       "pending request with an unsupported tier fails closed",
			req:        approval.RequestResult{ProposalID: testProposalID, Tier: policy.Tier("T3"), Status: statusPendingApproval},
			caseFacts:  adjustmentCase,
			proposal:   adjustmentProposal,
			records:    adjustmentRecords,
			wantAction: "",
		},
		{
			name:       "request and facts identify different proposals",
			req:        approval.RequestResult{ProposalID: testCaseID, Tier: policy.TierT1, Status: statusPendingApproval},
			caseFacts:  adjustmentCase,
			proposal:   adjustmentProposal,
			records:    adjustmentRecords,
			wantAction: "",
		},
		{
			name:       "malformed case facts fail closed",
			req:        baseline,
			caseFacts:  CaseFacts{CaseID: testCaseID, BreakKind: "fee_variance", DeltaMinor: 1250, Currency: "USD"},
			proposal:   adjustmentProposal,
			records:    adjustmentRecords,
			wantAction: "",
			wantErr:    errInvalidFacts,
		},
		{
			name:       "decide error fails the pipeline closed",
			req:        baseline,
			caseFacts:  adjustmentCase,
			proposal:   adjustmentProposal,
			records:    adjustmentRecords,
			deciderErr: errors.New("approval unavailable"),
			wantCalls:  1,
			wantAction: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decider := &fakeDecider{err: test.deciderErr}
			a := newTestApprover(t, decider, test.records)
			outcome, err := a.Act(context.Background(), test.req, test.caseFacts, test.proposal)

			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Act error = %v, want %v", err, test.wantErr)
				}
			} else if test.wantAction == "" {
				if err == nil {
					t.Fatal("Act returned nil error, want a fail-closed error")
				}
			} else if err != nil {
				t.Fatalf("Act: %v", err)
			}

			if len(decider.calls) != test.wantCalls {
				t.Fatalf("Decide calls = %d, want %d: %+v", len(decider.calls), test.wantCalls, decider.calls)
			}
			if test.wantAction == "" {
				if outcome != (Outcome{}) {
					t.Fatalf("outcome = %+v, want zero on error", outcome)
				}
				return
			}
			if outcome.Action != test.wantAction || outcome.Rule != test.wantRule {
				t.Fatalf("outcome action/rule = %s/%s, want %s/%s",
					outcome.Action, outcome.Rule, test.wantAction, test.wantRule)
			}
			if outcome.Reason == "" {
				t.Fatal("outcome reason is empty")
			}
			if test.wantCalls == 0 {
				if outcome.Decision != (approval.DecisionResult{}) {
					t.Fatalf("decision = %+v, want zero when Decide is not called", outcome.Decision)
				}
				return
			}
			call := decider.calls[0]
			if call.proposalID != testProposalID ||
				call.approverID != SyntheticApproverID ||
				call.source != SyntheticSource ||
				call.decision != test.wantDecision {
				t.Fatalf("Decide call = %+v, want proposal=%s approver=%s source=%s decision=%s",
					call, testProposalID, SyntheticApproverID, SyntheticSource, test.wantDecision)
			}
			if outcome.Decision.Decision != test.wantDecision ||
				outcome.Decision.ProposalID != testProposalID ||
				outcome.Decision.ApproverID != SyntheticApproverID {
				t.Fatalf("outcome decision = %+v, want %s/%s/%s",
					outcome.Decision, testProposalID, SyntheticApproverID, test.wantDecision)
			}
		})
	}
}

func TestActExactMatchForEachProposalType(t *testing.T) {
	builders := []struct {
		name  string
		build func() (CaseFacts, ProposalFacts, *fakeRecords)
	}{
		{name: "adjustment", build: adjustmentFacts},
		{name: "link", build: linkFacts},
		{name: "write_off", build: writeOffFacts},
		{name: "duplicate", build: duplicateFacts},
	}
	for _, builder := range builders {
		t.Run(builder.name, func(t *testing.T) {
			caseFacts, proposal, records := builder.build()
			decider := &fakeDecider{}
			a := newTestApprover(t, decider, records)
			outcome, err := a.Act(context.Background(), pendingRequest(policy.TierT1), caseFacts, proposal)
			if err != nil {
				t.Fatalf("Act: %v", err)
			}
			if outcome.Action != ActionApprove || outcome.Rule != RuleExactMatch {
				t.Fatalf("outcome = %s/%s, want approve/exact_match", outcome.Action, outcome.Rule)
			}
			if len(decider.calls) != 1 || decider.calls[0].decision != approval.DecisionApproved {
				t.Fatalf("Decide calls = %+v, want one approved call", decider.calls)
			}
		})
	}
}

func TestActRuleMatrix(t *testing.T) {
	tests := []struct {
		name     string
		build    func() (CaseFacts, ProposalFacts, *fakeRecords)
		wantRule string
	}{
		{
			name: "unknown proposal type",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.Type = "teleport"
				return c, p, r
			},
			wantRule: RuleTypeMismatch,
		},
		{
			name: "proposal type incompatible with break kind",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.Type = "adjustment"
				p.Resolution = "adjustment"
				return c, p, r
			},
			wantRule: RuleTypeMismatch,
		},
		{
			name: "unknown break kind",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				c.BreakKind = "made_up_break"
				return c, p, r
			},
			wantRule: RuleTypeMismatch,
		},
		{
			name: "adjustment amount mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				p.AmountMinor++
				return c, p, r
			},
			wantRule: RuleAmountMismatch,
		},
		{
			name: "proposal currency mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				p.Currency = "EUR"
				return c, p, r
			},
			wantRule: RuleCurrencyMismatch,
		},
		{
			name: "malformed proposal currency",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				p.Currency = "usd"
				return c, p, r
			},
			wantRule: RuleCurrencyMismatch,
		},
		{
			name: "link without ledger refs",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.LedgerRefs = nil
				return c, p, r
			},
			wantRule: RuleLedgerRefsMismatch,
		},
		{
			name: "ledger ref unknown",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.LedgerRefs = []string{testOtherLedger}
				return c, p, r
			},
			wantRule: RuleLedgerRefsMismatch,
		},
		{
			name: "ledger ref malformed",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.LedgerRefs = []string{"not-a-uuid"}
				return c, p, r
			},
			wantRule: RuleLedgerRefsMismatch,
		},
		{
			name: "ledger ref duplicate",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.LedgerRefs = []string{testLedgerID, testLedgerID}
				return c, p, r
			},
			wantRule: RuleLedgerRefsMismatch,
		},
		{
			name: "ledger ref scope mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				record := r.ledger[testLedgerID]
				record.ScopeID = testCaseID
				r.ledger[testLedgerID] = record
				return c, p, r
			},
			wantRule: RuleLedgerRefsMismatch,
		},
		{
			name: "ledger ref amount mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				record := r.ledger[testLedgerID]
				record.AmountMinor = 999
				r.ledger[testLedgerID] = record
				return c, p, r
			},
			wantRule: RuleLedgerRefsMismatch,
		},
		{
			name: "ledger ref currency mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				record := r.ledger[testLedgerID]
				record.Currency = "EUR"
				r.ledger[testLedgerID] = record
				return c, p, r
			},
			wantRule: RuleCurrencyMismatch,
		},
		{
			name: "link without external refs",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.ExternalRefs = nil
				return c, p, r
			},
			wantRule: RuleExternalRefsMismatch,
		},
		{
			name: "external ref unknown",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.ExternalRefs = []string{"ext:psp_a:missing"}
				return c, p, r
			},
			wantRule: RuleExternalRefsMismatch,
		},
		{
			name: "external ref malformed",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.ExternalRefs = []string{"psp_a:rec-1"}
				return c, p, r
			},
			wantRule: RuleExternalRefsMismatch,
		},
		{
			name: "external ref duplicate",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.ExternalRefs = []string{testExternalRef, testExternalRef}
				return c, p, r
			},
			wantRule: RuleExternalRefsMismatch,
		},
		{
			name: "external ref scope mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				record := r.external[testExternalRef]
				record.ScopeID = testCaseID
				r.external[testExternalRef] = record
				return c, p, r
			},
			wantRule: RuleExternalRefsMismatch,
		},
		{
			name: "external ref amount mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				record := r.external[testExternalRef]
				record.AmountMinor = 999
				r.external[testExternalRef] = record
				return c, p, r
			},
			wantRule: RuleExternalRefsMismatch,
		},
		{
			name: "external ref currency mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				record := r.external[testExternalRef]
				record.Currency = "EUR"
				r.external[testExternalRef] = record
				return c, p, r
			},
			wantRule: RuleCurrencyMismatch,
		},
		{
			name: "duplicate external amount mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := duplicateFacts()
				record := r.external[testExternalTwo]
				record.AmountMinor = 901
				r.external[testExternalTwo] = record
				return c, p, r
			},
			wantRule: RuleExternalRefsMismatch,
		},
		{
			name: "proposal counterparty empty",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				p.Counterparty = ""
				return c, p, r
			},
			wantRule: RuleCounterpartyMismatch,
		},
		{
			name: "case counterparty missing",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				c.Counterparty = ""
				return c, p, r
			},
			wantRule: RuleCounterpartyMismatch,
		},
		{
			name: "case counterparty mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				p.Counterparty = "bank_x"
				return c, p, r
			},
			wantRule: RuleCounterpartyMismatch,
		},
		{
			name: "external counterparty mismatch",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				record := r.external[testExternalRef]
				record.Counterparty = "bank_x"
				r.external[testExternalRef] = record
				return c, p, r
			},
			wantRule: RuleCounterpartyMismatch,
		},
		{
			name: "external counterparty empty",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := linkFacts()
				record := r.external[testExternalRef]
				record.Counterparty = ""
				r.external[testExternalRef] = record
				return c, p, r
			},
			wantRule: RuleCounterpartyMismatch,
		},
		{
			name: "resolution empty",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				p.Resolution = ""
				return c, p, r
			},
			wantRule: RuleResolutionMismatch,
		},
		{
			name: "resolution inconsistent with type",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				p.Resolution = "link"
				return c, p, r
			},
			wantRule: RuleResolutionMismatch,
		},
		{
			name: "resolution not normalized",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				p.Resolution = " Adjustment "
				return c, p, r
			},
			wantRule: RuleResolutionMismatch,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caseFacts, proposal, records := test.build()
			decider := &fakeDecider{}
			a := newTestApprover(t, decider, records)
			outcome, err := a.Act(context.Background(), pendingRequest(policy.TierT1), caseFacts, proposal)
			if err != nil {
				t.Fatalf("Act: %v", err)
			}
			if outcome.Action != ActionReject || outcome.Rule != test.wantRule {
				t.Fatalf("outcome = %s/%s, want reject/%s", outcome.Action, outcome.Rule, test.wantRule)
			}
			if len(decider.calls) != 1 {
				t.Fatalf("Decide calls = %d, want 1", len(decider.calls))
			}
			call := decider.calls[0]
			if call.decision != approval.DecisionRejected ||
				call.approverID != SyntheticApproverID ||
				call.source != SyntheticSource {
				t.Fatalf("Decide call = %+v, want rejected/%s/%s",
					call, SyntheticApproverID, SyntheticSource)
			}
			if outcome.Decision.Decision != approval.DecisionRejected {
				t.Fatalf("decision result = %+v, want rejected", outcome.Decision)
			}
		})
	}
}

func TestActFailsClosedOnMalformedFacts(t *testing.T) {
	tests := []struct {
		name  string
		build func() (CaseFacts, ProposalFacts, *fakeRecords)
	}{
		{
			name: "missing case id",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				c.CaseID = ""
				return c, p, r
			},
		},
		{
			name: "missing scope id",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				c.ScopeID = ""
				return c, p, r
			},
		},
		{
			name: "malformed scope id",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				c.ScopeID = "not-a-uuid"
				return c, p, r
			},
		},
		{
			name: "missing break kind",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				c.BreakKind = ""
				return c, p, r
			},
		},
		{
			name: "padded break kind",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				c.BreakKind = " fee_variance "
				return c, p, r
			},
		},
		{
			name: "missing case currency",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				c.Currency = ""
				return c, p, r
			},
		},
		{
			name: "malformed case currency",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				c.Currency = "usd"
				return c, p, r
			},
		},
		{
			name: "malformed proposal id",
			build: func() (CaseFacts, ProposalFacts, *fakeRecords) {
				c, p, r := adjustmentFacts()
				p.ProposalID = "not-a-uuid"
				return c, p, r
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caseFacts, proposal, records := test.build()
			req := pendingRequest(policy.TierT1)
			req.ProposalID = proposal.ProposalID
			decider := &fakeDecider{}
			a := newTestApprover(t, decider, records)
			outcome, err := a.Act(context.Background(), req, caseFacts, proposal)
			if !errors.Is(err, errInvalidFacts) {
				t.Fatalf("Act error = %v, want errInvalidFacts", err)
			}
			if outcome != (Outcome{}) {
				t.Fatalf("outcome = %+v, want zero", outcome)
			}
			if len(decider.calls) != 0 {
				t.Fatalf("Decide calls = %d, want 0", len(decider.calls))
			}
		})
	}
}

func TestActRecordSourceErrorsFailClosed(t *testing.T) {
	caseFacts, proposal, records := linkFacts()
	records.ledgerErr = errors.New("ledger unavailable")
	decider := &fakeDecider{}
	a := newTestApprover(t, decider, records)
	outcome, err := a.Act(context.Background(), pendingRequest(policy.TierT1), caseFacts, proposal)
	if err == nil || outcome != (Outcome{}) {
		t.Fatalf("Act = %+v, %v; want zero outcome and an error", outcome, err)
	}
	if !strings.Contains(err.Error(), "ledger unavailable") {
		t.Fatalf("Act error = %v, want the record-source error", err)
	}
	if len(decider.calls) != 0 {
		t.Fatalf("Decide calls = %d, want 0", len(decider.calls))
	}
}

func TestActNilRecordSourceFailsClosed(t *testing.T) {
	caseFacts, proposal, _ := linkFacts()
	decider := &fakeDecider{}
	a := newTestApprover(t, decider, nil)
	outcome, err := a.Act(context.Background(), pendingRequest(policy.TierT1), caseFacts, proposal)
	if !errors.Is(err, errRecordSource) {
		t.Fatalf("Act error = %v, want errRecordSource", err)
	}
	if outcome != (Outcome{}) {
		t.Fatalf("outcome = %+v, want zero", outcome)
	}
	if len(decider.calls) != 0 {
		t.Fatalf("Decide calls = %d, want 0", len(decider.calls))
	}
}

func TestActDeciderContractViolations(t *testing.T) {
	tests := []struct {
		name     string
		resultFn func(proposalID string, d approval.Decision, approverID, source string) (approval.DecisionResult, error)
	}{
		{
			name: "different proposal",
			resultFn: func(proposalID string, d approval.Decision, approverID, _ string) (approval.DecisionResult, error) {
				return approval.DecisionResult{
					ProposalID: testCaseID,
					Decision:   d,
					ApproverID: approverID,
					Status:     statusApproved,
				}, nil
			},
		},
		{
			name: "different decision",
			resultFn: func(proposalID string, _ approval.Decision, approverID, _ string) (approval.DecisionResult, error) {
				return approval.DecisionResult{
					ProposalID: proposalID,
					Decision:   approval.DecisionRejected,
					ApproverID: approverID,
					Status:     statusRejected,
				}, nil
			},
		},
		{
			name: "different approver",
			resultFn: func(proposalID string, d approval.Decision, _, _ string) (approval.DecisionResult, error) {
				return approval.DecisionResult{
					ProposalID: proposalID,
					Decision:   d,
					ApproverID: testCaseID,
					Status:     statusApproved,
				}, nil
			},
		},
		{
			name: "unexpected status",
			resultFn: func(proposalID string, d approval.Decision, approverID, _ string) (approval.DecisionResult, error) {
				return approval.DecisionResult{
					ProposalID: proposalID,
					Decision:   d,
					ApproverID: approverID,
					Status:     statusRejected,
				}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caseFacts, proposal, records := adjustmentFacts()
			decider := &fakeDecider{resultFn: test.resultFn}
			a := newTestApprover(t, decider, records)
			outcome, err := a.Act(context.Background(), pendingRequest(policy.TierT1), caseFacts, proposal)
			if err == nil || outcome != (Outcome{}) {
				t.Fatalf("Act = %+v, %v; want zero outcome and a contract error", outcome, err)
			}
			if len(decider.calls) != 1 {
				t.Fatalf("Decide calls = %d, want 1", len(decider.calls))
			}
		})
	}
}

func TestActDecideErrorIsPipelineError(t *testing.T) {
	caseFacts, proposal, records := adjustmentFacts()
	sentinel := errors.New("decide failed")
	decider := &fakeDecider{err: sentinel}
	a := newTestApprover(t, decider, records)
	outcome, err := a.Act(context.Background(), pendingRequest(policy.TierT1), caseFacts, proposal)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Act error = %v, want the Decide error", err)
	}
	if outcome != (Outcome{}) {
		t.Fatalf("outcome = %+v, want zero", outcome)
	}
	if len(decider.calls) != 1 {
		t.Fatalf("Decide calls = %d, want 1", len(decider.calls))
	}
}

func TestT0NeverCallsDecide(t *testing.T) {
	for _, req := range []approval.RequestResult{
		{Tier: policy.TierT0, Status: statusAutoApproved, ProposalID: testProposalID},
		{Tier: policy.TierT0, Status: statusPendingApproval, ProposalID: testProposalID},
		{Tier: policy.TierT1, Status: statusAutoApproved, ProposalID: testProposalID},
	} {
		decider := &fakeDecider{}
		a := newTestApprover(t, decider, &fakeRecords{})
		outcome, err := a.Act(context.Background(), req, CaseFacts{}, ProposalFacts{})
		if err != nil {
			t.Fatalf("Act(%+v): %v", req, err)
		}
		if outcome.Action != ActionSkipT0 || outcome.Rule != RuleT0AutoApproved {
			t.Fatalf("outcome = %s/%s, want skip_t0/t0_auto_approved", outcome.Action, outcome.Rule)
		}
		if len(decider.calls) != 0 {
			t.Fatalf("Decide calls = %d, want 0", len(decider.calls))
		}
	}
}

func TestT1AndT2UseTheSameDecidePath(t *testing.T) {
	for _, tier := range []policy.Tier{policy.TierT1, policy.TierT2} {
		caseFacts, proposal, records := adjustmentFacts()
		decider := &fakeDecider{}
		a := newTestApprover(t, decider, records)
		outcome, err := a.Act(context.Background(), pendingRequest(tier), caseFacts, proposal)
		if err != nil {
			t.Fatalf("Act(%s): %v", tier, err)
		}
		if outcome.Action != ActionApprove || outcome.Rule != RuleExactMatch {
			t.Fatalf("Act(%s) = %s/%s, want approve/exact_match", tier, outcome.Action, outcome.Rule)
		}
		if len(decider.calls) != 1 {
			t.Fatalf("Act(%s) Decide calls = %d, want 1", tier, len(decider.calls))
		}
		call := decider.calls[0]
		if call.approverID != SyntheticApproverID || call.source != SyntheticSource ||
			call.decision != approval.DecisionApproved {
			t.Fatalf("Act(%s) Decide call = %+v", tier, call)
		}
	}
}

func TestNewValidation(t *testing.T) {
	validConfig := Config{ApproverID: SyntheticApproverID, Source: SyntheticSource}
	var typedNil *fakeDecider

	tests := []struct {
		name    string
		decider Decider
		config  Config
		wantErr error
	}{
		{name: "nil decider", decider: nil, config: validConfig, wantErr: ErrNoApprover},
		{name: "typed nil decider", decider: typedNil, config: validConfig, wantErr: ErrNoApprover},
		{name: "empty approver id", decider: &fakeDecider{}, config: Config{Source: SyntheticSource}, wantErr: ErrNoApprover},
		{name: "agent approver id", decider: &fakeDecider{}, config: Config{ApproverID: "agent:test", Source: SyntheticSource}, wantErr: ErrNoApprover},
		{name: "uppercase approver id", decider: &fakeDecider{}, config: Config{ApproverID: "00000000-0000-4000-8000-00000000E2E1", Source: SyntheticSource}, wantErr: ErrNoApprover},
		{name: "empty source", decider: &fakeDecider{}, config: Config{ApproverID: SyntheticApproverID}, wantErr: ErrNoSource},
		{name: "non-eval source", decider: &fakeDecider{}, config: Config{ApproverID: SyntheticApproverID, Source: "slack"}, wantErr: ErrNoSource},
		{name: "valid", decider: &fakeDecider{}, config: validConfig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.decider, &fakeRecords{}, test.config)
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("New error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestRuleTableValidation(t *testing.T) {
	if err := validateRuleTable(ruleTable); err != nil {
		t.Fatalf("shipped rule table: %v", err)
	}

	var missing []rule
	for _, entry := range ruleTable {
		if entry.ID != RuleResolutionMismatch {
			missing = append(missing, entry)
		}
	}
	tests := []struct {
		name  string
		table []rule
	}{
		{name: "empty", table: nil},
		{name: "missing required rule", table: missing},
		{name: "empty rule id", table: []rule{{ID: "", Check: checkType}}},
		{name: "nil check", table: []rule{{ID: RuleTypeMismatch}}},
		{name: "duplicate rule", table: []rule{
			{ID: RuleTypeMismatch, Check: checkType},
			{ID: RuleTypeMismatch, Check: checkType},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRuleTable(test.table); !errors.Is(err, ErrRuleTable) {
				t.Fatalf("validateRuleTable = %v, want ErrRuleTable", err)
			}
		})
	}
}
