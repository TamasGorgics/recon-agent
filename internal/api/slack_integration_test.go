//go:build integration

// covers: FIN-22, FIN-30, FIN-33
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/slack"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

const (
	testSlackSigningSecret = "test-slack-signing-secret"
	slackSignatureHeader   = "X-Slack-Signature"
	slackTimestampHeader   = "X-Slack-Request-Timestamp"
	slackRetryHeader       = "X-Slack-Retry-Num"
	slackTestChannel       = "CSLACK1"
	slackTestTS            = "1732041600.123456"
)

// fakeSlackServer records Web API calls and returns a minimal ok response. It
// is deliberately not a live Slack workspace; AC-15 remains human-gated.
type fakeSlackServer struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []fakeSlackCall
	fail   bool
}

type fakeSlackCall struct {
	Method  string
	Payload map[string]any
}

func newFakeSlackServer(t *testing.T) *fakeSlackServer {
	t.Helper()
	fake := &fakeSlackServer{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fake slack body: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode fake slack body %q: %v", body, err)
		}
		method := strings.TrimPrefix(r.URL.Path, "/")
		fake.mu.Lock()
		fake.calls = append(fake.calls, fakeSlackCall{Method: method, Payload: payload})
		fail := fake.fail
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if fail && method == "chat.postEphemeral" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeSlackServer) URL() string          { return f.server.URL }
func (f *fakeSlackServer) Client() *http.Client { return f.server.Client() }

func (f *fakeSlackServer) setFailEphemeral(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *fakeSlackServer) callsFor(method string) []fakeSlackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matching []fakeSlackCall
	for _, call := range f.calls {
		if call.Method == method {
			matching = append(matching, call)
		}
	}
	return matching
}

func (f *fakeSlackServer) allCalls() []fakeSlackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSlackCall(nil), f.calls...)
}

// slackRequest is one signed callback; the same value can be sent again to
// exercise the verifier cache.
type slackRequest struct {
	body      string
	timestamp int64
	signature string
}

func signSlackBody(secret string, timestamp int64, body string) (string, string) {
	timestampText := strconv.FormatInt(timestamp, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestampText + ":" + body))
	return "v0=" + hex.EncodeToString(mac.Sum(nil)), timestampText
}

func (f *apiFixture) newSlackRawRequest(t *testing.T, body string) *slackRequest {
	t.Helper()
	return f.newSlackRawRequestAt(t, body, f.clock.Now())
}

func (f *apiFixture) newSlackRawRequestAt(t *testing.T, body string, at time.Time) *slackRequest {
	t.Helper()
	timestamp := at.Unix()
	signature, _ := signSlackBody(testSlackSigningSecret, timestamp, body)
	return &slackRequest{body: body, timestamp: timestamp, signature: signature}
}

func (f *apiFixture) newSlackRequest(t *testing.T, payload string) *slackRequest {
	t.Helper()
	return f.newSlackRawRequest(t, url.Values{"payload": {payload}}.Encode())
}

func (f *apiFixture) doSlack(t *testing.T, request *slackRequest, mutate func(*http.Request)) *http.Response {
	t.Helper()
	httpRequest, err := http.NewRequest(http.MethodPost, RouteSlackInteractions, strings.NewReader(request.body))
	if err != nil {
		t.Fatalf("new slack request: %v", err)
	}
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set(slackTimestampHeader, strconv.FormatInt(request.timestamp, 10))
	httpRequest.Header.Set(slackSignatureHeader, request.signature)
	if mutate != nil {
		mutate(httpRequest)
	}
	recorder := newTestRecorder()
	f.api.Handler().ServeHTTP(recorder, httpRequest)
	return recorder.Result()
}

func slackActionPayload(t *testing.T, actionID, proposalID string, version int,
	userID, channelID, ts string) string {
	t.Helper()
	payload := map[string]any{
		"type":    "block_actions",
		"user":    map[string]string{"id": userID},
		"channel": map[string]string{"id": channelID},
		"message": map[string]string{"ts": ts},
		"actions": []map[string]string{{
			"action_id": actionID,
			"value":     fmt.Sprintf("%s:%d", proposalID, version),
		}},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal slack payload: %v", err)
	}
	return string(encoded)
}

func seedSlackApprover(t *testing.T, f *apiFixture, slackUserID, role string, active bool) string {
	t.Helper()
	var approverID string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO approvers (display_name, role, active, slack_user_id)
		 VALUES ($1, $2, $3, $4) RETURNING id::text`,
		"Slack "+slackUserID, role, active, slackUserID).Scan(&approverID); err != nil {
		t.Fatalf("seed slack approver: %v", err)
	}
	return approverID
}

func seedSlackMessage(t *testing.T, f *apiFixture, proposalID, channel, ts string) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`INSERT INTO approval_messages (proposal_id, channel, external_ref, state)
		 VALUES ($1::uuid, 'slack', $2, 'posted')`,
		proposalID, channel+":"+ts); err != nil {
		t.Fatalf("seed slack message: %v", err)
	}
}

func execFixtureSQL(t *testing.T, f *apiFixture, sql string, args ...any) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture SQL failed: %v\n%s", err, sql)
	}
}

type slackProposalState struct {
	Status    string
	Version   int
	ExpiresAt *time.Time
}

func readSlackProposal(t *testing.T, f *apiFixture, proposalID string) slackProposalState {
	t.Helper()
	var state slackProposalState
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status, version, expires_at FROM proposals WHERE id = $1::uuid`,
		proposalID).Scan(&state.Status, &state.Version, &state.ExpiresAt); err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	return state
}

type slackApprovalRow struct {
	Channel     string
	ApproverID  string
	Decision    string
	ExternalRef *string
}

func readSlackApprovals(t *testing.T, f *apiFixture, proposalID string) []slackApprovalRow {
	t.Helper()
	rows, err := f.db.Pool.Query(context.Background(),
		`SELECT channel, approver_id::text, decision, external_ref
		   FROM approvals WHERE proposal_id = $1::uuid ORDER BY decided_at, approver_id`,
		proposalID)
	if err != nil {
		t.Fatalf("read approvals: %v", err)
	}
	defer rows.Close()
	var approvals []slackApprovalRow
	for rows.Next() {
		var row slackApprovalRow
		if err := rows.Scan(&row.Channel, &row.ApproverID, &row.Decision, &row.ExternalRef); err != nil {
			t.Fatalf("scan approval: %v", err)
		}
		approvals = append(approvals, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate approvals: %v", err)
	}
	return approvals
}

func countAuditAction(t *testing.T, f *apiFixture, proposalID, action string) int {
	t.Helper()
	var count int
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action = $2`,
		proposalID, action).Scan(&count); err != nil {
		t.Fatalf("count audit %s: %v", action, err)
	}
	return count
}

type slackDeniedAudit struct {
	ActorType  string
	ActorID    string
	EntityType string
	EntityID   string
	After      map[string]any
}

func readSlackDenials(t *testing.T, f *apiFixture, proposalID string) []slackDeniedAudit {
	t.Helper()
	rows, err := f.db.Pool.Query(context.Background(),
		`SELECT actor_type, actor_id, entity_type, entity_id, after
		   FROM audit_log WHERE action = $1 AND entity_id = $2 ORDER BY seq`,
		approval.ActionApprovalDenied, proposalID)
	if err != nil {
		t.Fatalf("read denied audits: %v", err)
	}
	defer rows.Close()
	var denials []slackDeniedAudit
	for rows.Next() {
		var (
			denial slackDeniedAudit
			after  []byte
		)
		if err := rows.Scan(&denial.ActorType, &denial.ActorID, &denial.EntityType,
			&denial.EntityID, &after); err != nil {
			t.Fatalf("scan denied audit: %v", err)
		}
		if err := json.Unmarshal(after, &denial.After); err != nil {
			t.Fatalf("decode denied after: %v", err)
		}
		denials = append(denials, denial)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate denied audits: %v", err)
	}
	return denials
}

func seedCoMakerAudit(t *testing.T, f *apiFixture, approverID, proposalID string) {
	t.Helper()
	actor, err := audit.HumanActor(approverID)
	if err != nil {
		t.Fatalf("human actor: %v", err)
	}
	if err := db.WithTx(context.Background(), f.db.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      actor,
			Action:     "proposal.edited",
			EntityType: "proposal",
			EntityID:   proposalID,
			Timestamp:  f.clock.Now().UTC(),
		})
		return err
	}); err != nil {
		t.Fatalf("seed co-maker audit: %v", err)
	}
}

func assertionJSON(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func assertSlackOK(t *testing.T, response *http.Response) {
	t.Helper()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.StatusCode, bodyOf(t, response))
	}
	body := assertionJSON(t, response)
	if body["ok"] != true {
		t.Fatalf("body = %+v, want ok=true", body)
	}
}

func assertLastEphemeral(t *testing.T, f *apiFixture, wantChannel, wantUser, wantText string) {
	t.Helper()
	calls := f.slack.callsFor("chat.postEphemeral")
	if len(calls) == 0 {
		t.Fatal("no chat.postEphemeral call was made")
	}
	last := calls[len(calls)-1]
	if last.Payload["channel"] != wantChannel || last.Payload["user"] != wantUser ||
		last.Payload["text"] != wantText {
		t.Fatalf("ephemeral = %+v, want %s/%s/%q", last.Payload, wantChannel, wantUser, wantText)
	}
}

// TestAPISlackInteractionApproveIntegration drives a signed approve callback
// through the real router, signature middleware, handler, approval service,
// approvers table, approvals row, audit chain, and fake Slack server.
func TestAPISlackInteractionApproveIntegration(t *testing.T) {
	f := newAPIFixture(t)
	full, _ := seedCases(t, f)
	prepareSlackProposal(t, f, full.proposalID)
	approverID := seedSlackApprover(t, f, "U-APPROVER", "approver", true)
	seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)

	payload := slackActionPayload(t, "approval_approve", full.proposalID, 1, "U-APPROVER", slackTestChannel, slackTestTS)
	response := f.doSlack(t, f.newSlackRequest(t, payload), nil)
	assertSlackOK(t, response)

	// No API key was required or sent.
	state := readSlackProposal(t, f, full.proposalID)
	if state.Status != "approved" || state.Version != 2 {
		t.Fatalf("proposal = %s/%d, want approved/2", state.Status, state.Version)
	}
	approvals := readSlackApprovals(t, f, full.proposalID)
	if len(approvals) != 1 {
		t.Fatalf("approvals = %d, want 1: %+v", len(approvals), approvals)
	}
	row := approvals[0]
	if row.Channel != "slack" || row.ApproverID != approverID || row.Decision != "approved" ||
		row.ExternalRef != nil {
		t.Fatalf("approval row = %+v", row)
	}
	if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 1 {
		t.Fatalf("approval.decided audits = %d, want 1", got)
	}
	assertLastEphemeral(t, f, slackTestChannel, "U-APPROVER", "Approval recorded.")
}

func TestAPISlackInteractionRejectIntegration(t *testing.T) {
	f := newAPIFixture(t)
	full, _ := seedCases(t, f)
	prepareSlackProposal(t, f, full.proposalID)
	seedSlackApprover(t, f, "U-ADMIN", "admin", true)
	seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)

	payload := slackActionPayload(t, "approval_reject", full.proposalID, 1, "U-ADMIN", slackTestChannel, slackTestTS)
	assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

	state := readSlackProposal(t, f, full.proposalID)
	if state.Status != "rejected" || state.Version != 2 {
		t.Fatalf("proposal = %s/%d, want rejected/2", state.Status, state.Version)
	}
	approvals := readSlackApprovals(t, f, full.proposalID)
	if len(approvals) != 1 || approvals[0].Decision != "rejected" || approvals[0].Channel != "slack" {
		t.Fatalf("approvals = %+v", approvals)
	}
	assertLastEphemeral(t, f, slackTestChannel, "U-ADMIN", "Rejection recorded.")
}

// TestAPISlackInteractionStyledButtonsIntegration posts the captured styled
// block_actions fixtures through the real signed route and proves Slack's
// style field on the action does not trip the strict decoder before the
// decision is recorded.
func TestAPISlackInteractionStyledButtonsIntegration(t *testing.T) {
	tests := []struct {
		name         string
		fixture      string
		wantStatus   string
		wantDecision string
		wantText     string
	}{
		{
			name:         "approve primary",
			fixture:      "block_actions_approve_styled.json",
			wantStatus:   "approved",
			wantDecision: "approved",
			wantText:     "Approval recorded.",
		},
		{
			name:         "reject danger",
			fixture:      "block_actions_reject_styled.json",
			wantStatus:   "rejected",
			wantDecision: "rejected",
			wantText:     "Rejection recorded.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAPIFixture(t)
			full, _ := seedCases(t, f)
			prepareSlackProposal(t, f, full.proposalID)
			seedSlackApprover(t, f, "U-APPROVER", "approver", true)
			seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)

			raw, err := os.ReadFile(filepath.Join(
				"..", "approval", "slack", "testdata", "interactions", test.fixture))
			if err != nil {
				t.Fatalf("read styled fixture: %v", err)
			}
			payload := strings.ReplaceAll(string(raw),
				"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa:1", full.proposalID+":1")
			assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

			state := readSlackProposal(t, f, full.proposalID)
			if state.Status != test.wantStatus || state.Version != 2 {
				t.Fatalf("proposal = %s/%d, want %s/2",
					state.Status, state.Version, test.wantStatus)
			}
			approvals := readSlackApprovals(t, f, full.proposalID)
			if len(approvals) != 1 || approvals[0].Decision != test.wantDecision ||
				approvals[0].Channel != "slack" {
				t.Fatalf("approvals = %+v, want one slack %s row",
					approvals, test.wantDecision)
			}
			if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 1 {
				t.Fatalf("approval.decided audits = %d, want 1", got)
			}
			assertLastEphemeral(t, f, slackTestChannel, "U-APPROVER", test.wantText)
		})
	}
}

func TestAPISlackInteractionSignatureFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *apiFixture, *slackRequest) *slackRequest
		status int
		code   string
	}{
		{
			name: "bad signature",
			mutate: func(_ *testing.T, _ *apiFixture, request *slackRequest) *slackRequest {
				request.signature = "v0=" + strings.Repeat("0", sha256.Size*2)
				return request
			},
			status: http.StatusUnauthorized,
			code:   "unauthenticated",
		},
		{
			name: "missing signature",
			mutate: func(_ *testing.T, _ *apiFixture, request *slackRequest) *slackRequest {
				request.signature = ""
				return request
			},
			status: http.StatusUnauthorized,
			code:   "unauthenticated",
		},
		{
			name: "stale timestamp",
			mutate: func(t *testing.T, f *apiFixture, request *slackRequest) *slackRequest {
				return f.newSlackRawRequestAt(t, request.body, f.clock.Now().Add(-10*time.Minute))
			},
			status: http.StatusUnauthorized,
			code:   "unauthenticated",
		},
		{
			name: "future timestamp",
			mutate: func(t *testing.T, f *apiFixture, request *slackRequest) *slackRequest {
				return f.newSlackRawRequestAt(t, request.body, f.clock.Now().Add(10*time.Minute))
			},
			status: http.StatusUnauthorized,
			code:   "unauthenticated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAPIFixture(t)
			full, _ := seedCases(t, f)
			prepareSlackProposal(t, f, full.proposalID)
			seedSlackApprover(t, f, "U-APPROVER", "approver", true)
			seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
			payload := slackActionPayload(t, "approval_approve", full.proposalID, 1, "U-APPROVER", slackTestChannel, slackTestTS)
			request := test.mutate(t, f, f.newSlackRequest(t, payload))

			response := f.doSlack(t, request, func(r *http.Request) {
				if request.signature == "" {
					r.Header.Del(slackSignatureHeader)
				}
			})
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d; body=%s", response.StatusCode, test.status, bodyOf(t, response))
			}
			assertErrorCode(t, response, test.code)
			if state := readSlackProposal(t, f, full.proposalID); state.Status != "pending_approval" || state.Version != 1 {
				t.Fatalf("proposal changed: %+v", state)
			}
			if len(readSlackApprovals(t, f, full.proposalID)) != 0 {
				t.Fatal("signature failure wrote an approvals row")
			}
			if calls := f.slack.allCalls(); len(calls) != 0 {
				t.Fatalf("signature failure reached Slack: %+v", calls)
			}
		})
	}

	t.Run("body too large", func(t *testing.T) {
		f := newAPIFixture(t)
		body := url.Values{"payload": {strings.Repeat("a", 1<<20)}}.Encode()
		request := f.newSlackRawRequest(t, body)
		response := f.doSlack(t, request, nil)
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%s", response.StatusCode, bodyOf(t, response))
		}
		assertErrorCode(t, response, "payload_too_large")
	})
}

func TestAPISlackInteractionReplayAndRetry(t *testing.T) {
	t.Run("exact replay without retry header is 409", func(t *testing.T) {
		f := newAPIFixture(t)
		full, _ := seedCases(t, f)
		prepareSlackProposal(t, f, full.proposalID)
		seedSlackApprover(t, f, "U-APPROVER", "approver", true)
		seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
		payload := slackActionPayload(t, "approval_approve", full.proposalID, 1, "U-APPROVER", slackTestChannel, slackTestTS)
		request := f.newSlackRequest(t, payload)

		assertSlackOK(t, f.doSlack(t, request, nil))
		replay := f.doSlack(t, request, nil)
		if replay.StatusCode != http.StatusConflict {
			t.Fatalf("replay status = %d, want 409; body=%s", replay.StatusCode, bodyOf(t, replay))
		}
		assertErrorCode(t, replay, "replay")
		if got := len(readSlackApprovals(t, f, full.proposalID)); got != 1 {
			t.Fatalf("approvals = %d, want 1", got)
		}
		if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 1 {
			t.Fatalf("approval.decided audits = %d, want 1", got)
		}
	})

	t.Run("retry header exempts only the replay marker", func(t *testing.T) {
		f := newAPIFixture(t)
		full, _ := seedCases(t, f)
		prepareSlackProposal(t, f, full.proposalID)
		seedSlackApprover(t, f, "U-APPROVER", "approver", true)
		seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
		payload := slackActionPayload(t, "approval_approve", full.proposalID, 1, "U-APPROVER", slackTestChannel, slackTestTS)
		request := f.newSlackRequest(t, payload)
		retry := func(r *http.Request) { r.Header.Set(slackRetryHeader, "1") }

		// The retry header is valid on the first delivery too; it only
		// bypasses a cache marker, never the decision idempotency rules.
		assertSlackOK(t, f.doSlack(t, request, retry))
		assertSlackOK(t, f.doSlack(t, request, retry))

		if state := readSlackProposal(t, f, full.proposalID); state.Status != "approved" || state.Version != 2 {
			t.Fatalf("proposal = %+v, want approved/2", state)
		}
		if got := len(readSlackApprovals(t, f, full.proposalID)); got != 1 {
			t.Fatalf("approvals = %d, want 1 (no double-apply)", got)
		}
		if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 1 {
			t.Fatalf("approval.decided audits = %d, want 1", got)
		}
	})

	t.Run("retry header never bypasses the signature", func(t *testing.T) {
		f := newAPIFixture(t)
		full, _ := seedCases(t, f)
		prepareSlackProposal(t, f, full.proposalID)
		seedSlackApprover(t, f, "U-APPROVER", "approver", true)
		seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
		payload := slackActionPayload(t, "approval_approve", full.proposalID, 1, "U-APPROVER", slackTestChannel, slackTestTS)
		request := f.newSlackRequest(t, payload)
		request.signature = "v0=" + strings.Repeat("f", sha256.Size*2)

		response := f.doSlack(t, request, func(r *http.Request) { r.Header.Set(slackRetryHeader, "1") })
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.StatusCode)
		}
		assertErrorCode(t, response, "unauthenticated")
	})
}

func TestAPISlackInteractionUnauthorized(t *testing.T) {
	tests := []struct {
		name       string
		seed       func(*testing.T, *apiFixture)
		wantReason string
	}{
		{
			name:       "unknown user",
			seed:       func(*testing.T, *apiFixture) {},
			wantReason: "unknown_user",
		},
		{
			name: "inactive approver",
			seed: func(t *testing.T, f *apiFixture) {
				seedSlackApprover(t, f, "U-INACTIVE", "approver", false)
			},
			wantReason: "inactive",
		},
		{
			name: "viewer role",
			seed: func(t *testing.T, f *apiFixture) {
				seedSlackApprover(t, f, "U-VIEWER", "viewer", true)
			},
			wantReason: "role",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAPIFixture(t)
			full, _ := seedCases(t, f)
			prepareSlackProposal(t, f, full.proposalID)
			test.seed(t, f)
			seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
			slackUserID := map[string]string{
				"unknown user":      "U-UNKNOWN",
				"inactive approver": "U-INACTIVE",
				"viewer role":       "U-VIEWER",
			}[test.name]

			payload := slackActionPayload(t, "approval_approve", full.proposalID, 1, slackUserID, slackTestChannel, slackTestTS)
			assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

			state := readSlackProposal(t, f, full.proposalID)
			if state.Status != "pending_approval" || state.Version != 1 {
				t.Fatalf("proposal changed: %+v", state)
			}
			if len(readSlackApprovals(t, f, full.proposalID)) != 0 {
				t.Fatal("unauthorized user wrote an approvals row")
			}
			if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 0 {
				t.Fatalf("approval.decided audits = %d, want 0", got)
			}

			// covers: FIN-33
			denials := readSlackDenials(t, f, full.proposalID)
			if len(denials) != 1 {
				t.Fatalf("approval.denied audits = %d, want exactly 1: %+v", len(denials), denials)
			}
			denial := denials[0]
			if denial.ActorType != "human" || denial.ActorID != slackUserID ||
				denial.EntityType != "proposal" || denial.EntityID != full.proposalID {
				t.Fatalf("denial audit = %+v", denial)
			}
			if denial.After["denied"] != true || denial.After["slack_user_id"] != slackUserID ||
				denial.After["action"] != "approve" || denial.After["reason"] != test.wantReason {
				t.Fatalf("denial after = %+v", denial.After)
			}
			assertLastEphemeral(t, f, slackTestChannel, slackUserID,
				"You are not authorized to approve this proposal.")
		})
	}
}

// TestAPISlackInteractionDeniedRetryIsDeduplicated signs the same unauthorized
// click twice with X-Slack-Retry-Num. The retry header bypasses only the replay
// marker, so the second delivery reaches the handler; the denial audit remains
// exactly one row for the same (proposal, Slack user, action).
func TestAPISlackInteractionDeniedRetryIsDeduplicated(t *testing.T) {
	f := newAPIFixture(t)
	full, _ := seedCases(t, f)
	prepareSlackProposal(t, f, full.proposalID)
	seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)

	payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
		"U-UNKNOWN", slackTestChannel, slackTestTS)
	request := f.newSlackRequest(t, payload)
	retry := func(r *http.Request) { r.Header.Set(slackRetryHeader, "1") }

	assertSlackOK(t, f.doSlack(t, request, retry))
	assertSlackOK(t, f.doSlack(t, request, retry))

	if got := len(readSlackDenials(t, f, full.proposalID)); got != 1 {
		t.Fatalf("approval.denied audits after signed retry = %d, want exactly 1", got)
	}
	if state := readSlackProposal(t, f, full.proposalID); state.Status != "pending_approval" || state.Version != 1 {
		t.Fatalf("proposal changed after denied retry: %+v", state)
	}
	if got := len(readSlackApprovals(t, f, full.proposalID)); got != 0 {
		t.Fatalf("denied retry wrote %d approvals row(s)", got)
	}
}

// TestAPISlackInteractionRequestInfoStaleCaseIsOutOfDate proves a stale case
// status is refused before DecideWithOptions. The seeded case is open while the
// proposal is pending; request-info would otherwise fail the lifecycle hook and
// roll back into a 500.
func TestAPISlackInteractionRequestInfoStaleCaseIsOutOfDate(t *testing.T) {
	f := newAPIFixture(t)
	full, _ := seedCases(t, f)
	prepareSlackProposal(t, f, full.proposalID)
	seedSlackApprover(t, f, "U-APPROVER", "approver", true)
	seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
	beforeCase := readSlackCase(t, f, full.caseID)
	if beforeCase.Status != "open" {
		t.Fatalf("seeded case status = %q, want open", beforeCase.Status)
	}

	payload := slackActionPayload(t, "approval_request_info", full.proposalID, 1,
		"U-APPROVER", slackTestChannel, slackTestTS)
	assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

	if state := readSlackProposal(t, f, full.proposalID); state.Status != "pending_approval" || state.Version != 1 {
		t.Fatalf("proposal = %+v, want pending_approval/1", state)
	}
	if got := len(readSlackApprovals(t, f, full.proposalID)); got != 0 {
		t.Fatalf("stale request-info wrote %d approvals row(s)", got)
	}
	if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 0 {
		t.Fatalf("stale request-info wrote %d approval.decided audits", got)
	}
	afterCase := readSlackCase(t, f, full.caseID)
	if afterCase != beforeCase {
		t.Fatalf("case changed: %+v -> %+v", beforeCase, afterCase)
	}
	assertLastEphemeral(t, f, slackTestChannel, "U-APPROVER", "This approval is no longer active.")
}

func TestAPISlackInteractionStaleClicks(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, *apiFixture, seededCase)
		userID string
	}{
		{
			name: "stale version",
			setup: func(t *testing.T, f *apiFixture, full seededCase) {
				execFixtureSQL(t, f, `UPDATE proposals SET version = 2 WHERE id = $1::uuid`, full.proposalID)
				seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
			},
			userID: "U-APPROVER",
		},
		{
			name: "stale message ts",
			setup: func(t *testing.T, f *apiFixture, full seededCase) {
				seedSlackMessage(t, f, full.proposalID, slackTestChannel, "1111111111.000001")
			},
			userID: "U-APPROVER",
		},
		{
			name: "non-pending proposal",
			setup: func(t *testing.T, f *apiFixture, full seededCase) {
				execFixtureSQL(t, f, `UPDATE proposals SET status = 'superseded', version = 2 WHERE id = $1::uuid`, full.proposalID)
				seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
			},
			userID: "U-APPROVER",
		},
		{
			name:   "missing approval message",
			setup:  func(*testing.T, *apiFixture, seededCase) {},
			userID: "U-APPROVER",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAPIFixture(t)
			full, _ := seedCases(t, f)
			prepareSlackProposal(t, f, full.proposalID)
			seedSlackApprover(t, f, test.userID, "approver", true)
			test.setup(t, f, full)

			payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
				test.userID, slackTestChannel, slackTestTS)
			assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

			if len(readSlackApprovals(t, f, full.proposalID)) != 0 {
				t.Fatal("stale click wrote an approvals row")
			}
			if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 0 {
				t.Fatalf("stale click wrote %d approval.decided audits", got)
			}
			assertLastEphemeral(t, f, slackTestChannel, test.userID, "This approval is no longer active.")
		})
	}
}

func TestAPISlackInteractionDecisionRefusals(t *testing.T) {
	t.Run("co-maker four-eyes", func(t *testing.T) {
		f := newAPIFixture(t)
		full, _ := seedCases(t, f)
		prepareSlackProposal(t, f, full.proposalID)
		approverID := seedSlackApprover(t, f, "U-APPROVER", "approver", true)
		seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
		seedCoMakerAudit(t, f, approverID, full.proposalID)

		payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
			"U-APPROVER", slackTestChannel, slackTestTS)
		assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

		if state := readSlackProposal(t, f, full.proposalID); state.Status != "pending_approval" {
			t.Fatalf("proposal = %+v, want pending_approval", state)
		}
		if len(readSlackApprovals(t, f, full.proposalID)) != 0 {
			t.Fatal("co-maker decision wrote an approvals row")
		}
		assertLastEphemeral(t, f, slackTestChannel, "U-APPROVER",
			"You contributed to this proposal and cannot approve it.")
	})

	t.Run("conflicting decision", func(t *testing.T) {
		f := newAPIFixture(t)
		full, _ := seedCases(t, f)
		prepareSlackProposal(t, f, full.proposalID)
		approverID := seedSlackApprover(t, f, "U-APPROVER", "approver", true)
		seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
		// Drift: a decision row exists for the same approver while the
		// proposal is still pending. Decide must report a conflict.
		execFixtureSQL(t, f,
			`INSERT INTO approvals (proposal_id, channel, approver_id, decision, decided_at, signature_valid)
			 VALUES ($1::uuid, 'slack', $2::uuid, 'approved', $3, true)`,
			full.proposalID, approverID, f.clock.Now().UTC())

		payload := slackActionPayload(t, "approval_reject", full.proposalID, 1,
			"U-APPROVER", slackTestChannel, slackTestTS)
		assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

		if state := readSlackProposal(t, f, full.proposalID); state.Status != "pending_approval" {
			t.Fatalf("proposal = %+v, want pending_approval", state)
		}
		if got := len(readSlackApprovals(t, f, full.proposalID)); got != 1 {
			t.Fatalf("approvals = %d, want the seeded row only", got)
		}
		assertLastEphemeral(t, f, slackTestChannel, "U-APPROVER",
			"This proposal already has a conflicting decision.")
	})

	t.Run("expired proposal", func(t *testing.T) {
		f := newAPIFixture(t)
		full, _ := seedCases(t, f)
		prepareSlackProposal(t, f, full.proposalID)
		seedSlackApprover(t, f, "U-APPROVER", "approver", true)
		seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
		execFixtureSQL(t, f, `UPDATE proposals SET expires_at = $2 WHERE id = $1::uuid`,
			full.proposalID, f.clock.Now().Add(-time.Minute))

		payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
			"U-APPROVER", slackTestChannel, slackTestTS)
		assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

		state := readSlackProposal(t, f, full.proposalID)
		if state.Status != "expired" || state.Version != 2 {
			t.Fatalf("proposal = %+v, want expired/2", state)
		}
		if len(readSlackApprovals(t, f, full.proposalID)) != 0 {
			t.Fatal("late decision wrote an approvals row")
		}
		assertLastEphemeral(t, f, slackTestChannel, "U-APPROVER", "This approval is no longer active.")
	})
}

func TestAPISlackInteractionMalformedPayload(t *testing.T) {
	tests := []struct {
		name        string
		body        func(t *testing.T, full seededCase) string
		contentType string
	}{
		{
			name: "malformed json",
			body: func(*testing.T, seededCase) string {
				return url.Values{"payload": {"not-json"}}.Encode()
			},
		},
		{
			name: "wrong type",
			body: func(t *testing.T, full seededCase) string {
				payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
					"U-APPROVER", slackTestChannel, slackTestTS)
				return url.Values{"payload": {strings.Replace(payload, `"block_actions"`, `"view_submission"`, 1)}}.Encode()
			},
		},
		{
			name: "unknown action",
			body: func(t *testing.T, full seededCase) string {
				return url.Values{"payload": {slackActionPayload(t, "approval_delete", full.proposalID, 1,
					"U-APPROVER", slackTestChannel, slackTestTS)}}.Encode()
			},
		},
		{
			name: "unknown JSON field",
			body: func(t *testing.T, full seededCase) string {
				payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
					"U-APPROVER", slackTestChannel, slackTestTS)
				return url.Values{"payload": {strings.Replace(payload,
					`"type":"block_actions",`, `"type":"block_actions","unexpected":"x",`, 1)}}.Encode()
			},
		},
		{
			name: "unknown form field",
			body: func(t *testing.T, full seededCase) string {
				payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
					"U-APPROVER", slackTestChannel, slackTestTS)
				return url.Values{"payload": {payload}, "other": {"value"}}.Encode()
			},
		},
		{
			name: "malformed proposal id",
			body: func(t *testing.T, _ seededCase) string {
				return url.Values{"payload": {slackActionPayload(t, "approval_approve", "not-a-uuid", 1,
					"U-APPROVER", slackTestChannel, slackTestTS)}}.Encode()
			},
		},
		{
			name: "missing payload field",
			body: func(*testing.T, seededCase) string {
				return "other=value"
			},
		},
		{
			name: "duplicate payload field",
			body: func(t *testing.T, full seededCase) string {
				payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
					"U-APPROVER", slackTestChannel, slackTestTS)
				return url.Values{"payload": {payload, payload}}.Encode()
			},
		},
		{
			name: "wrong content type",
			body: func(t *testing.T, full seededCase) string {
				return slackActionPayload(t, "approval_approve", full.proposalID, 1,
					"U-APPROVER", slackTestChannel, slackTestTS)
			},
			contentType: "application/json",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAPIFixture(t)
			full, _ := seedCases(t, f)
			prepareSlackProposal(t, f, full.proposalID)
			seedSlackApprover(t, f, "U-APPROVER", "approver", true)
			seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
			request := f.newSlackRawRequest(t, test.body(t, full))

			response := f.doSlack(t, request, func(r *http.Request) {
				if test.contentType != "" {
					r.Header.Set("Content-Type", test.contentType)
				}
			})
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, bodyOf(t, response))
			}
			assertErrorCode(t, response, "invalid_request")

			if state := readSlackProposal(t, f, full.proposalID); state.Status != "pending_approval" || state.Version != 1 {
				t.Fatalf("malformed payload changed the proposal: %+v", state)
			}
			if len(readSlackApprovals(t, f, full.proposalID)) != 0 {
				t.Fatal("malformed payload wrote an approvals row")
			}
			if denials := readSlackDenials(t, f, full.proposalID); len(denials) != 0 {
				t.Fatalf("malformed payload wrote denial audits: %+v", denials)
			}
			if calls := f.slack.allCalls(); len(calls) != 0 {
				t.Fatalf("malformed payload reached Slack: %+v", calls)
			}
		})
	}
}

func TestAPISlackInteractionEphemeralFailureDoesNotRollBack(t *testing.T) {
	f := newAPIFixture(t)
	full, _ := seedCases(t, f)
	prepareSlackProposal(t, f, full.proposalID)
	seedSlackApprover(t, f, "U-APPROVER", "approver", true)
	seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
	f.slack.setFailEphemeral(true)

	payload := slackActionPayload(t, "approval_approve", full.proposalID, 1,
		"U-APPROVER", slackTestChannel, slackTestTS)
	assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

	if state := readSlackProposal(t, f, full.proposalID); state.Status != "approved" || state.Version != 2 {
		t.Fatalf("committed decision rolled back after ephemeral failure: %+v", state)
	}
	if got := len(readSlackApprovals(t, f, full.proposalID)); got != 1 {
		t.Fatalf("approvals = %d, want 1", got)
	}
}

type slackCaseState struct {
	Status   string
	Version  int
	Attempts int
}

func readSlackCase(t *testing.T, f *apiFixture, caseID string) slackCaseState {
	t.Helper()
	var state slackCaseState
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status, version, attempts FROM cases WHERE id = $1::uuid`, caseID).
		Scan(&state.Status, &state.Version, &state.Attempts); err != nil {
		t.Fatalf("read case: %v", err)
	}
	return state
}

type approvalUpdateOutbox struct {
	ID      string
	Topic   string
	Key     string
	Payload []byte
}

func readApprovalUpdateOutbox(t *testing.T, f *apiFixture, proposalID string) approvalUpdateOutbox {
	t.Helper()
	var row approvalUpdateOutbox
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT id::text, topic, idempotency_key, payload
		   FROM outbox
		  WHERE aggregate_id = $1 AND topic = $2`,
		proposalID, approval.TopicApprovalUpdate).
		Scan(&row.ID, &row.Topic, &row.Key, &row.Payload); err != nil {
		t.Fatalf("read approval.update outbox row: %v", err)
	}
	return row
}

func readDecidedAuditAfter(t *testing.T, f *apiFixture, proposalID string) map[string]any {
	t.Helper()
	var raw []byte
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT after FROM audit_log
		  WHERE action = $1 AND entity_id = $2
		  ORDER BY seq DESC LIMIT 1`,
		approval.ActionApprovalDecided, proposalID).Scan(&raw); err != nil {
		t.Fatalf("read decided audit: %v", err)
	}
	var after map[string]any
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatalf("decode decided audit: %v", err)
	}
	return after
}

func readCaseTransitionAfter(t *testing.T, f *apiFixture, caseID string) (string, string, map[string]any) {
	t.Helper()
	var (
		actorID string
		raw     []byte
	)
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT actor_id, after FROM audit_log
		  WHERE action = 'case.transition' AND entity_id = $1
		  ORDER BY seq DESC LIMIT 1`, caseID).Scan(&actorID, &raw); err != nil {
		t.Fatalf("read case transition audit: %v", err)
	}
	var after map[string]any
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatalf("decode case transition audit: %v", err)
	}
	return actorID, string(raw), after
}

func readApprovalMessageState(t *testing.T, f *apiFixture, proposalID string) string {
	t.Helper()
	var state string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT state FROM approval_messages WHERE proposal_id = $1::uuid AND channel = 'slack'`,
		proposalID).Scan(&state); err != nil {
		t.Fatalf("read approval message state: %v", err)
	}
	return state
}

func slackPayloadText(payload map[string]any) string {
	var out strings.Builder
	if text, ok := payload["text"].(string); ok {
		out.WriteString(text)
	}
	blocks, _ := payload["blocks"].([]any)
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]any)
		text, _ := block["text"].(map[string]any)
		if value, ok := text["text"].(string); ok {
			out.WriteString("\n")
			out.WriteString(value)
		}
	}
	return out.String()
}

func slackPayloadHasActions(payload map[string]any) bool {
	blocks, _ := payload["blocks"].([]any)
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]any)
		if block["type"] == "actions" {
			return true
		}
	}
	return false
}

func newSlackUpdateHandlers(t *testing.T, f *apiFixture) *slack.Handlers {
	t.Helper()
	client, err := slack.NewClient(slack.ClientOptions{
		BaseURL:    f.slack.URL(),
		BotToken:   "xoxb-test-token",
		HTTPClient: f.slack.Client(),
		Clock:      f.clock,
	})
	if err != nil {
		t.Fatalf("build slack client: %v", err)
	}
	handlers, err := slack.NewHandlers(slack.HandlersOptions{
		Queryer:         f.app.Pool,
		Client:          client,
		ChannelID:       slackTestChannel,
		CaseFileBaseURL: "https://cases.example.test",
		Clock:           f.clock,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build slack handlers: %v", err)
	}
	return handlers
}

// TestAPISlackInteractionRequestInfoIntegration drives a signed request-info
// callback through the real router, approval service, lifecycle hook, audit
// chain, and outbox producer, then delivers the produced approval.update row
// through the real Slack handler against the fake Web API server. It proves the
// proposal is rejected with reason info_requested, the case is waiting_info
// without an attempt, and the original message is edited in place.
func TestAPISlackInteractionRequestInfoIntegration(t *testing.T) {
	f := newAPIFixture(t)
	full, _ := seedCases(t, f)
	prepareSlackProposal(t, f, full.proposalID)
	execFixtureSQL(t, f, `UPDATE cases SET status = 'proposal_pending' WHERE id = $1::uuid`, full.caseID)
	approverID := seedSlackApprover(t, f, "U-APPROVER", "approver", true)
	seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
	beforeCase := readSlackCase(t, f, full.caseID)

	payload := slackActionPayload(t, "approval_request_info", full.proposalID, 1,
		"U-APPROVER", slackTestChannel, slackTestTS)
	assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

	proposal := readSlackProposal(t, f, full.proposalID)
	if proposal.Status != "rejected" || proposal.Version != 2 {
		t.Fatalf("proposal = %s/%d, want rejected/2", proposal.Status, proposal.Version)
	}
	approvals := readSlackApprovals(t, f, full.proposalID)
	if len(approvals) != 1 || approvals[0].Decision != "rejected" ||
		approvals[0].Channel != "slack" || approvals[0].ApproverID != approverID {
		t.Fatalf("approvals = %+v, want one slack rejection by %s", approvals, approverID)
	}

	afterCase := readSlackCase(t, f, full.caseID)
	if afterCase.Status != "waiting_info" || afterCase.Attempts != beforeCase.Attempts ||
		afterCase.Version != beforeCase.Version+1 {
		t.Fatalf("case = %s/attempts %d/version %d, want waiting_info/%d/%d",
			afterCase.Status, afterCase.Attempts, afterCase.Version,
			beforeCase.Attempts, beforeCase.Version+1)
	}
	actorID, _, transitionAfter := readCaseTransitionAfter(t, f, full.caseID)
	if actorID != "case-lifecycle" || transitionAfter["status"] != "waiting_info" ||
		transitionAfter["reason"] != "info_requested" {
		t.Fatalf("case transition = actor %q after %+v, want system:case-lifecycle waiting_info/info_requested",
			actorID, transitionAfter)
	}

	if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 1 {
		t.Fatalf("approval.decided audits = %d, want 1", got)
	}
	decidedAfter := readDecidedAuditAfter(t, f, full.proposalID)
	if decidedAfter["decision"] != "rejected" || decidedAfter["info_requested"] != true ||
		decidedAfter["status"] != "rejected" {
		t.Fatalf("approval.decided after = %+v, want rejected with info_requested", decidedAfter)
	}
	if _, present := decidedAfter["reason"]; present {
		t.Fatalf("approval.decided after has an empty reason: %+v", decidedAfter)
	}

	update := readApprovalUpdateOutbox(t, f, full.proposalID)
	if update.Topic != approval.TopicApprovalUpdate ||
		update.Key != approval.TopicApprovalUpdate+":"+full.proposalID {
		t.Fatalf("approval.update row = %+v", update)
	}
	var updatePayload map[string]any
	if err := json.Unmarshal(update.Payload, &updatePayload); err != nil {
		t.Fatalf("decode approval.update payload: %v", err)
	}
	for key, want := range map[string]any{
		"proposal_id":    full.proposalID,
		"case_id":        full.caseID,
		"decision":       "rejected",
		"status":         "rejected",
		"source":         "slack",
		"approver_id":    approverID,
		"info_requested": true,
	} {
		if updatePayload[key] != want {
			t.Fatalf("approval.update payload[%s] = %v, want %v", key, updatePayload[key], want)
		}
	}
	if _, present := updatePayload["reason"]; present {
		t.Fatalf("approval.update payload has an empty reason: %+v", updatePayload)
	}
	decidedAt, ok := updatePayload["decided_at"].(string)
	if !ok {
		t.Fatalf("approval.update decided_at = %v", updatePayload["decided_at"])
	}
	if _, err := time.Parse(time.RFC3339Nano, decidedAt); err != nil {
		t.Fatalf("approval.update decided_at %q is not RFC3339Nano: %v", decidedAt, err)
	}

	if state := readApprovalMessageState(t, f, full.proposalID); state != "posted" {
		t.Fatalf("approval message state before delivery = %q, want posted", state)
	}
	handlers := newSlackUpdateHandlers(t, f)
	if err := handlers.HandleUpdate(context.Background(), outbox.Message{
		ID:             update.ID,
		Topic:          update.Topic,
		Payload:        update.Payload,
		IdempotencyKey: update.Key,
	}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	updates := f.slack.callsFor("chat.update")
	if len(updates) != 1 {
		t.Fatalf("chat.update calls = %d, want 1: %+v", len(updates), f.slack.allCalls())
	}
	edited := updates[0].Payload
	if edited["channel"] != slackTestChannel || edited["ts"] != slackTestTS {
		t.Fatalf("chat.update target = %+v, want %s/%s", edited, slackTestChannel, slackTestTS)
	}
	if text := slackPayloadText(edited); !strings.Contains(strings.ToLower(text), "information requested") {
		t.Fatalf("edited message text = %q, want the information-requested label", text)
	}
	if slackPayloadHasActions(edited) {
		t.Fatalf("edited message still has action buttons: %+v", edited)
	}
	if state := readApprovalMessageState(t, f, full.proposalID); state != "updated" {
		t.Fatalf("approval message state after delivery = %q, want updated", state)
	}

	// covers: FIN-33
	assertLastEphemeral(t, f, slackTestChannel, "U-APPROVER", "Information requested.")
}

func TestAPISlackInteractionRequestInfoStaleClick(t *testing.T) {
	f := newAPIFixture(t)
	full, _ := seedCases(t, f)
	prepareSlackProposal(t, f, full.proposalID)
	execFixtureSQL(t, f, `UPDATE cases SET status = 'proposal_pending' WHERE id = $1::uuid`, full.caseID)
	seedSlackApprover(t, f, "U-APPROVER", "approver", true)
	seedSlackMessage(t, f, full.proposalID, slackTestChannel, slackTestTS)
	execFixtureSQL(t, f, `UPDATE proposals SET version = 2 WHERE id = $1::uuid`, full.proposalID)

	payload := slackActionPayload(t, "approval_request_info", full.proposalID, 1,
		"U-APPROVER", slackTestChannel, slackTestTS)
	assertSlackOK(t, f.doSlack(t, f.newSlackRequest(t, payload), nil))

	if state := readSlackProposal(t, f, full.proposalID); state.Status != "pending_approval" || state.Version != 2 {
		t.Fatalf("stale request-info changed the proposal: %+v", state)
	}
	if got := len(readSlackApprovals(t, f, full.proposalID)); got != 0 {
		t.Fatalf("stale request-info wrote %d approvals rows", got)
	}
	if got := countAuditAction(t, f, full.proposalID, approval.ActionApprovalDecided); got != 0 {
		t.Fatalf("stale request-info wrote %d approval.decided audits", got)
	}
	assertLastEphemeral(t, f, slackTestChannel, "U-APPROVER", "This approval is no longer active.")
}

// prepareSlackProposal sets the seeded pending proposal to the version a real
// approval request leaves behind (Request transitions 0 -> 1). The generic
// seedCases helper inserts the mid-transaction shape at version 0.
func prepareSlackProposal(t *testing.T, f *apiFixture, proposalID string) {
	t.Helper()
	execFixtureSQL(t, f, `UPDATE proposals SET version = 1 WHERE id = $1::uuid`, proposalID)
}
