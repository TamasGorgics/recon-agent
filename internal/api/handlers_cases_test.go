// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/traceability"
)

func TestListCasesDefaultsAndFilters(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, RouteCases, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var list CaseList
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Items == nil {
		t.Fatal("items must be [] not null")
	}
	if h.caseList.got.Limit != defaultListLimit || h.caseList.got.Status != "" || h.caseList.got.Cursor != nil {
		t.Fatalf("query = %+v", h.caseList.got)
	}

	h = newHarness(t, nil)
	h.caseList.page = CaseListPage{
		Items:      []CaseSummary{{ID: testCaseID, BreakIDs: nil}},
		NextCursor: "next",
	}
	response = h.expectStatus(http.MethodGet, RouteCases+"?status=open&limit=2", "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.caseList.got.Status != "open" || h.caseList.got.Limit != 2 {
		t.Fatalf("query = %+v", h.caseList.got)
	}
	if list.NextCursor != "next" || len(list.Items) != 1 || list.Items[0].BreakIDs == nil {
		t.Fatalf("list = %+v", list)
	}
}

func TestListCasesCursorRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 11, 10, 0, 0, 123000000, time.UTC)
	cursor := encodeCursor(created, testCaseID)
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, RouteCases+"?cursor="+cursor, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if h.caseList.got.Cursor == nil || !h.caseList.got.Cursor.CreatedAt.Equal(created) || h.caseList.got.Cursor.ID != testCaseID {
		t.Fatalf("cursor = %+v", h.caseList.got.Cursor)
	}
}

func TestListCasesInvalidQuery(t *testing.T) {
	tests := []struct {
		query    string
		wantCode string
	}{
		{"?status=weird", codeInvalidQuery},
		{"?status=", codeInvalidQuery},
		{"?limit=0", codeInvalidQuery},
		{"?limit=201", codeInvalidQuery},
		{"?limit=abc", codeInvalidQuery},
		{"?limit=", codeInvalidQuery},
		{"?cursor=not-base64", codeInvalidQuery},
		{"?cursor=", codeInvalidQuery},
		{"?unknown=1", codeInvalidQuery},
		{"?type=adjustment", codeUnsupportedFilter},
		{"?type=", codeUnsupportedFilter},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.expectStatus(http.MethodGet, RouteCases+test.query, "", "", testViewerKey, http.StatusBadRequest)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, test.wantCode)
			if h.caseList.calls != 0 {
				t.Fatal("store was queried for an invalid query")
			}
		})
	}
}

func TestCaseFileShapeAndNormalization(t *testing.T) {
	h := newHarness(t, nil)
	h.caseFile.document = CaseFile{
		Case: CaseFileCase{ID: testCaseID, BreakIDs: nil},
		Trace: CaseFileTrace{
			Case: traceability.Case{ID: testCaseID},
		},
	}
	response := h.expectStatus(http.MethodGet, "/v1/cases/"+testCaseID, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var document CaseFile
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if document.SchemaVersion != 2 {
		t.Fatalf("schema_version = %d, want 2", document.SchemaVersion)
	}
	if document.Breaks == nil || document.Proposals == nil || document.AgentRuns == nil || document.Case.BreakIDs == nil {
		t.Fatalf("empty sections must be [] not null: %+v", document)
	}
	if document.Trace.Chains == nil || document.Trace.Missing == nil {
		t.Fatalf("trace sections must be [] not null: %+v", document.Trace)
	}
	if document.Trace.Case.ID != testCaseID {
		t.Fatalf("trace case id = %q, want %q", document.Trace.Case.ID, testCaseID)
	}
	if h.caseFile.got != testCaseID {
		t.Fatalf("case id = %q", h.caseFile.got)
	}
}

// TestCaseFileV2TraceShape pins the v2 trace JSON: the trace has exactly the
// case/chains/missing keys, and each chain carries every link plus the missing
// markers. An unlinked application keeps ledger_txn_id null.
//
// covers: FIN-32
func TestCaseFileV2TraceShape(t *testing.T) {
	configYAML := "schema_version: 1\nthresholds:\n  single_approval_minor: 0\n"
	appliedAt := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	decidedAt := appliedAt.Add(5 * time.Minute)
	tier := "T1"
	rule := "default_single_approval"
	h := newHarness(t, nil)
	h.caseFile.document = CaseFile{
		Case:      CaseFileCase{ID: testCaseID, BreakIDs: []string{}},
		Breaks:    []Break{},
		Proposals: []ProposalDetail{},
		AgentRuns: []AgentRun{},
		Trace: CaseFileTrace{
			Case: traceability.Case{ID: testCaseID, Status: "resolved"},
			Chains: []traceability.Chain{{
				LedgerTxnID: nil,
				AppliedProposal: &traceability.AppliedProposal{
					ProposalID:    testDecisionProposalID,
					AppliedAt:     appliedAt,
					ExecutorRunID: "executor-run-1",
				},
				Proposal: &traceability.Proposal{
					ID: testDecisionProposalID, CaseID: testCaseID, Type: "adjustment",
					Status: "applied", CreatedAt: appliedAt,
				},
				Case:         &traceability.Case{ID: testCaseID},
				Breaks:       []traceability.Break{},
				AgentRun:     &traceability.AgentRun{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", TraceID: ptrString("trace-1")},
				Verification: json.RawMessage(`{"passed":true}`),
				Policy: &traceability.Policy{
					RiskTier: &tier, Rule: rule, Reasons: []string{rule},
					ConfigHash: "hash", ConfigYAML: &configYAML,
				},
				Approvals: []traceability.Approval{{
					Decision: "approved", DecidedAt: decidedAt,
					Approver: traceability.Approver{DisplayName: "Alice Approver", Role: "approver"},
				}},
				Executor:      traceability.Executor{Audit: &traceability.ExecutorAudit{Seq: 7}},
				LedgerEntries: []traceability.LedgerEntry{},
				Missing:       []string{traceability.MissingLedgerTxn},
			}},
			Missing: []string{traceability.MissingLedgerTxn},
		},
	}
	response := h.expectStatus(http.MethodGet, "/v1/cases/"+testCaseID, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["trace"]; !ok {
		t.Fatal("response is missing trace")
	}
	var traceRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["trace"], &traceRaw); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	for _, key := range []string{"case", "chains", "missing"} {
		if _, ok := traceRaw[key]; !ok {
			t.Fatalf("trace is missing %q", key)
		}
	}
	if len(traceRaw) != 3 {
		t.Fatalf("trace keys = %v, want exactly case/chains/missing", keysOf(traceRaw))
	}
	var chains []map[string]json.RawMessage
	if err := json.Unmarshal(traceRaw["chains"], &chains); err != nil {
		t.Fatalf("decode chains: %v", err)
	}
	if len(chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(chains))
	}
	wantChainKeys := []string{
		"ledger_txn_id", "applied_proposal", "proposal", "case", "breaks",
		"agent_run", "verification", "policy", "approvals", "executor",
		"ledger_txn", "ledger_entries", "missing",
	}
	for _, key := range wantChainKeys {
		if _, ok := chains[0][key]; !ok {
			t.Fatalf("chain is missing %q", key)
		}
	}
	if len(chains[0]) != len(wantChainKeys) {
		t.Fatalf("chain keys = %v, want %v", keysOf(chains[0]), wantChainKeys)
	}
	if string(chains[0]["ledger_txn_id"]) != "null" {
		t.Fatalf("ledger_txn_id = %s, want null", chains[0]["ledger_txn_id"])
	}
	if string(chains[0]["ledger_txn"]) != "null" {
		t.Fatalf("ledger_txn = %s, want null", chains[0]["ledger_txn"])
	}
	var missing []string
	if err := json.Unmarshal(chains[0]["missing"], &missing); err != nil {
		t.Fatalf("decode chain missing: %v", err)
	}
	if len(missing) != 1 || missing[0] != traceability.MissingLedgerTxn {
		t.Fatalf("chain missing = %v", missing)
	}
	var policyRaw map[string]json.RawMessage
	if err := json.Unmarshal(chains[0]["policy"], &policyRaw); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	for _, key := range []string{"risk_tier", "rule", "reasons", "config_hash", "config_yaml", "config_loaded_at"} {
		if _, ok := policyRaw[key]; !ok {
			t.Fatalf("policy is missing %q", key)
		}
	}
}

func keysOf[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func ptrString(value string) *string { return &value }

func TestCaseFileRequestBody(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, "/v1/cases/"+testCaseID, "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"schema_version", "case", "breaks", "proposals", "agent_runs", "trace"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response missing %q", key)
		}
	}
}

func TestCaseFileInvalidAndMissing(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, "/v1/cases/not-a-uuid", "", "", testViewerKey, http.StatusBadRequest)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeInvalidRequest)

	h = newHarness(t, nil)
	h.caseFile.err = cases.ErrCaseNotFound
	response = h.expectStatus(http.MethodGet, "/v1/cases/"+testCaseID, "", "", testViewerKey, http.StatusNotFound)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeNotFound)
}

func TestListProposalsFiltersAndAlias(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, RouteProposals+"?status=pending&case_id="+testCaseID+"&limit=5", "", "", testViewerKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if h.proposals.got.Status != "pending_approval" {
		t.Fatalf("status = %q, want pending_approval", h.proposals.got.Status)
	}
	if h.proposals.got.CaseID != testCaseID || h.proposals.got.Limit != 5 {
		t.Fatalf("query = %+v", h.proposals.got)
	}

	var list ProposalList
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Items == nil {
		t.Fatal("items must be [] not null")
	}
}

func TestListProposalsSummaryOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.proposals.page = ProposalListPage{Items: []ProposalSummary{{
		ID:         "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		CaseID:     testCaseID,
		Type:       "adjustment",
		Status:     "pending_approval",
		Confidence: 0.87,
	}}}
	response := h.expectStatus(http.MethodGet, RouteProposals, "", "", testViewerKey, http.StatusOK)
	body := bodyOf(t, response)
	for _, forbidden := range []string{"payload", "evidence", "rationale", "policy_decision", "verification"} {
		if strings.Contains(body, `"`+forbidden+`"`) {
			t.Fatalf("proposal list leaked %q: %s", forbidden, body)
		}
	}
}

func TestListProposalsInvalidQuery(t *testing.T) {
	tests := []string{
		"?status=weird",
		"?status=",
		"?case_id=not-a-uuid",
		"?limit=0",
		"?cursor=bad",
		"?type=adjustment",
	}
	for _, query := range tests {
		t.Run(query, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.expectStatus(http.MethodGet, RouteProposals+query, "", "", testViewerKey, http.StatusBadRequest)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, codeInvalidQuery)
			if h.proposals.calls != 0 {
				t.Fatal("store was queried for an invalid query")
			}
		})
	}
}
