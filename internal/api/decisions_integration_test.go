//go:build integration

// covers: FIN-30, FIN-33
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (f *apiFixture) postDecision(t *testing.T, proposalID, body, bearer string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "/v1/proposals/"+proposalID+"/decision", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new decision request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	recorder := newTestRecorder()
	f.api.Handler().ServeHTTP(recorder, request)
	return recorder.Result()
}

// TestAPIDecisionRouteIntegration exercises the real router, middleware,
// approval service, state machine, approvals row, and audit chain together.
func TestAPIDecisionRouteIntegration(t *testing.T) {
	f := newAPIFixture(t)
	full, _ := seedCases(t, f)
	ctx := context.Background()

	var approverID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO approvers (display_name, role, active, api_key_id)
		 VALUES ('API decision approver', 'approver', true, 'dev-approver')
		 RETURNING id::text`).Scan(&approverID); err != nil {
		t.Fatalf("seed approver: %v", err)
	}

	const body = `{"decision":"approved","reason":"checked evidence"}`
	response := f.postDecision(t, full.proposalID, body, testApproverKey)
	if response.StatusCode != http.StatusOK {
		bodyText := bodyOf(t, response)
		t.Fatalf("decision status = %d, body = %s", response.StatusCode, bodyText)
	}
	var result ProposalDecisionResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode decision result: %v", err)
	}
	_ = response.Body.Close()
	wantDecidedAt := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if result.ProposalID != full.proposalID || result.Status != "approved" ||
		result.Decision != "approved" || result.ApproverID != approverID ||
		!result.DecidedAt.Equal(wantDecidedAt) || result.Replayed {
		t.Fatalf("decision result = %+v", result)
	}

	var status string
	var version int
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT status, version FROM proposals WHERE id = $1::uuid`, full.proposalID).
		Scan(&status, &version); err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if status != "approved" || version != 1 {
		t.Fatalf("proposal = %s/%d, want approved/1", status, version)
	}

	var (
		channel        string
		decision       string
		storedApprover string
		externalRef    *string
		signatureValid bool
		decidedAt      time.Time
	)
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT channel, decision, approver_id::text, external_ref, signature_valid, decided_at
		   FROM approvals WHERE proposal_id = $1::uuid`, full.proposalID).
		Scan(&channel, &decision, &storedApprover, &externalRef, &signatureValid, &decidedAt); err != nil {
		t.Fatalf("read approval: %v", err)
	}
	if channel != "api" || decision != "approved" || storedApprover != approverID ||
		externalRef != nil || !signatureValid || !decidedAt.Equal(wantDecidedAt) {
		t.Fatalf("approval row = channel:%q decision:%q approver:%q ref:%v signature:%v decided:%v",
			channel, decision, storedApprover, externalRef, signatureValid, decidedAt)
	}

	var actorType, actorID, action string
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT actor_type, actor_id, action
		   FROM audit_log
		  WHERE entity_type = 'proposal' AND entity_id = $1 AND action = 'approval.decided'`,
		full.proposalID).Scan(&actorType, &actorID, &action); err != nil {
		t.Fatalf("read decision audit: %v", err)
	}
	if actorType != "human" || actorID != approverID || action != "approval.decided" {
		t.Fatalf("decision audit = %s:%s %s", actorType, actorID, action)
	}

	// Same approver and decision: a no-write replay.
	response = f.postDecision(t, full.proposalID, body, testApproverKey)
	if response.StatusCode != http.StatusOK {
		bodyText := bodyOf(t, response)
		t.Fatalf("replay status = %d, body = %s", response.StatusCode, bodyText)
	}
	var replay ProposalDecisionResult
	if err := json.NewDecoder(response.Body).Decode(&replay); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	_ = response.Body.Close()
	if !replay.Replayed || !replay.DecidedAt.Equal(wantDecidedAt) || replay.ApproverID != approverID {
		t.Fatalf("replay result = %+v", replay)
	}
	var approvals int
	var afterVersion int
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`, full.proposalID).Scan(&approvals); err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT version FROM proposals WHERE id = $1::uuid`, full.proposalID).Scan(&afterVersion); err != nil {
		t.Fatalf("read replay version: %v", err)
	}
	if approvals != 1 || afterVersion != version {
		t.Fatalf("replay wrote: approvals=%d version=%d, want 1/%d", approvals, afterVersion, version)
	}

	// Viewer is rejected by the route before the decision service.
	response = f.postDecision(t, full.proposalID, body, testViewerKey)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403", response.StatusCode)
	}

	// Unknown proposal is a 404 from the real approval service.
	response = f.postDecision(t, "dddddddd-dddd-4ddd-8ddd-dddddddddddd", body, testApproverKey)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown proposal status = %d, want 404", response.StatusCode)
	}

	// A second approver conflicts with the recorded decision.
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO approvers (display_name, role, active, api_key_id)
		 VALUES ('API conflict approver', 'admin', true, 'dev-admin')`); err != nil {
		t.Fatalf("seed conflict approver: %v", err)
	}
	response = f.postDecision(t, full.proposalID, `{"decision":"rejected"}`, testAdminKey)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", response.StatusCode)
	}
}
