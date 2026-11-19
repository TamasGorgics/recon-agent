package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const unitProposalID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func unitPayload(t *testing.T, actionID, value, userID, channelID, ts string) string {
	t.Helper()
	payload := map[string]any{
		"type": "block_actions",
		"user": map[string]string{"id": userID},
		"channel": map[string]string{
			"id": channelID,
		},
		"message": map[string]string{"ts": ts},
		"actions": []map[string]string{{
			"action_id": actionID,
			"value":     value,
		}},
		"team":       map[string]string{"id": "T1"},
		"trigger_id": "trigger-1",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(encoded)
}

func TestParseInteractionPayloadAcceptsCapturedStyledButtons(t *testing.T) {
	tests := []struct {
		fixture      string
		actionID     string
		style        string
		wantDecision approval.Decision
	}{
		{
			fixture:      "block_actions_approve_styled.json",
			actionID:     ActionApprove,
			style:        "primary",
			wantDecision: approval.DecisionApproved,
		},
		{
			fixture:      "block_actions_reject_styled.json",
			actionID:     ActionReject,
			style:        "danger",
			wantDecision: approval.DecisionRejected,
		},
	}
	for _, test := range tests {
		t.Run(test.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "interactions", test.fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var captured struct {
				Actions []struct {
					Style string `json:"style"`
				} `json:"actions"`
			}
			if err := json.Unmarshal(raw, &captured); err != nil {
				t.Fatalf("decode fixture style: %v", err)
			}
			if len(captured.Actions) != 1 || captured.Actions[0].Style != test.style {
				t.Fatalf("fixture action style = %+v, want one %q action",
					captured.Actions, test.style)
			}

			in, parseErr := parseInteractionPayload(raw)
			if parseErr != nil {
				t.Fatalf("styled block_actions rejected: %+v", parseErr)
			}
			if in.actionID != test.actionID || in.decision() != test.wantDecision {
				t.Fatalf("parsed action/decision = %q/%q, want %q/%q",
					in.actionID, in.decision(), test.actionID, test.wantDecision)
			}
			if in.slackUserID != "U-APPROVER" || in.channelID != "CSLACK1" ||
				in.messageTS != "1732041600.123456" || in.version != 1 {
				t.Fatalf("parsed interaction = %+v", in)
			}
		})
	}
}

func TestParseInteractionPayload(t *testing.T) {
	valid := unitPayload(t, ActionApprove, unitProposalID+":7", "U123", "C123", "1732041600.123456")
	in, err := parseInteractionPayload([]byte(valid))
	if err != nil {
		t.Fatalf("parseInteractionPayload: %+v", err)
	}
	if in.proposalID != unitProposalID || in.version != 7 || in.actionID != ActionApprove ||
		in.slackUserID != "U123" || in.channelID != "C123" || in.messageTS != "1732041600.123456" {
		t.Fatalf("parsed interaction = %+v", in)
	}
	if in.decision() != approval.DecisionApproved {
		t.Fatalf("approve decision = %q", in.decision())
	}
	if in.actionName() != "approve" {
		t.Fatalf("approve action name = %q", in.actionName())
	}

	reject := unitPayload(t, ActionReject, unitProposalID+":7", "U123", "C123", "1732041600.123456")
	in, err = parseInteractionPayload([]byte(reject))
	if err != nil {
		t.Fatalf("parse reject: %+v", err)
	}
	if in.decision() != approval.DecisionRejected || in.actionName() != "reject" {
		t.Fatalf("reject = %q/%q", in.decision(), in.actionName())
	}

	requestInfo := unitPayload(t, ActionRequestInfo, unitProposalID+":7", "U123", "C123", "1732041600.123456")
	in, err = parseInteractionPayload([]byte(requestInfo))
	if err != nil {
		t.Fatalf("parse request_info: %+v", err)
	}
	if in.actionName() != "request_info" {
		t.Fatalf("request_info action name = %q", in.actionName())
	}
}

func TestParseInteractionPayloadRejectsMalformed(t *testing.T) {
	validValue := unitProposalID + ":1"
	base := unitPayload(t, ActionApprove, validValue, "U123", "C123", "1732041600.123456")

	tests := []struct {
		name    string
		payload string
	}{
		{name: "empty", payload: ""},
		{name: "not json", payload: "not-json"},
		{name: "array", payload: "[]"},
		{name: "trailing value", payload: base + `{}`},
		{name: "unknown field", payload: strings.Replace(base,
			`"type":"block_actions",`, `"type":"block_actions","unexpected":"x",`, 1)},
		{name: "wrong type", payload: strings.Replace(base, `"block_actions"`, `"view_submission"`, 1)},
		{name: "missing type", payload: strings.Replace(base, `"type":"block_actions",`, "", 1)},
		{name: "no actions", payload: strings.Replace(base, `"actions":[{"action_id":"approval_approve","value":"`+validValue+`"}]`, `"actions":[]`, 1)},
		{name: "two actions", payload: strings.Replace(base,
			`"actions":[{"action_id":"approval_approve","value":"`+validValue+`"}]`,
			`"actions":[{"action_id":"approval_approve","value":"`+validValue+`"},{"action_id":"approval_reject","value":"`+validValue+`"}]`, 1)},
		{name: "unknown action", payload: strings.Replace(base, `"action_id":"approval_approve"`, `"action_id":"approval_delete"`, 1)},
		{name: "unknown action field", payload: strings.Replace(base,
			`{"action_id":"approval_approve"`, `{"unexpected_button_field":"x","action_id":"approval_approve"`, 1)},
		{name: "missing user", payload: strings.Replace(base, `,"user":{"id":"U123"}`, "", 1)},
		{name: "empty user", payload: strings.Replace(base, `"id":"U123"`, `"id":""`, 1)},
		{name: "colon user", payload: strings.Replace(base, `"id":"U123"`, `"id":"U:1"`, 1)},
		{name: "padded user", payload: strings.Replace(base, `"id":"U123"`, `"id":" U123"`, 1)},
		{name: "oversized user", payload: strings.Replace(base, `"id":"U123"`, `"id":"`+strings.Repeat("U", 129)+`"`, 1)},
		{name: "missing channel", payload: strings.Replace(base, `"channel":{"id":"C123"},`, "", 1)},
		{name: "empty channel", payload: strings.Replace(base, `"id":"C123"`, `"id":""`, 1)},
		{name: "missing message", payload: strings.Replace(base, `"message":{"ts":"1732041600.123456"},`, "", 1)},
		{name: "bad timestamp", payload: strings.Replace(base, `"ts":"1732041600.123456"`, `"ts":"17.32.04"`, 1)},
		{name: "empty timestamp", payload: strings.Replace(base, `"ts":"1732041600.123456"`, `"ts":""`, 1)},
		{name: "value without colon", payload: unitPayload(t, ActionApprove, unitProposalID, "U123", "C123", "1.2")},
		{name: "value with extra colon", payload: unitPayload(t, ActionApprove, validValue+":3", "U123", "C123", "1.2")},
		{name: "value uppercase uuid", payload: unitPayload(t, ActionApprove, strings.ToUpper(unitProposalID)+":1", "U123", "C123", "1.2")},
		{name: "value zero version", payload: unitPayload(t, ActionApprove, unitProposalID+":0", "U123", "C123", "1.2")},
		{name: "value leading zero", payload: unitPayload(t, ActionApprove, unitProposalID+":01", "U123", "C123", "1.2")},
		{name: "value negative version", payload: unitPayload(t, ActionApprove, unitProposalID+":-1", "U123", "C123", "1.2")},
		{name: "value non-numeric version", payload: unitPayload(t, ActionApprove, unitProposalID+":x", "U123", "C123", "1.2")},
		{name: "value non-uuid", payload: unitPayload(t, ActionApprove, "not-a-uuid:1", "U123", "C123", "1.2")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseInteractionPayload([]byte(test.payload)); err == nil {
				t.Fatal("parseInteractionPayload accepted malformed input")
			} else if err.status != http.StatusBadRequest || err.code != interactionCodeInvalidRequest {
				t.Fatalf("error = %+v, want 400 invalid_request", err)
			}
		})
	}
}

func TestParseInteractionValue(t *testing.T) {
	tests := []struct {
		value       string
		wantID      string
		wantVersion int
		wantErr     bool
	}{
		{value: unitProposalID + ":1", wantID: unitProposalID, wantVersion: 1},
		{value: unitProposalID + ":42", wantID: unitProposalID, wantVersion: 42},
		{value: "", wantErr: true},
		{value: " " + unitProposalID + ":1", wantErr: true},
		{value: unitProposalID + ":1 ", wantErr: true},
		{value: unitProposalID, wantErr: true},
		{value: unitProposalID + ":1:2", wantErr: true},
		{value: ":1", wantErr: true},
		{value: unitProposalID + ":", wantErr: true},
		{value: unitProposalID + ":0", wantErr: true},
		{value: unitProposalID + ":00", wantErr: true},
		{value: unitProposalID + ":-1", wantErr: true},
		{value: unitProposalID + ":+1", wantErr: true},
		{value: unitProposalID + ":1.5", wantErr: true},
		{value: unitProposalID + ":99999999999999999999999999", wantErr: true},
		{value: strings.ToUpper(unitProposalID) + ":1", wantErr: true},
		{value: "aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa:1", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			id, version, err := parseInteractionValue(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseInteractionValue(%q) = %q/%d, want error", test.value, id, version)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseInteractionValue(%q): %v", test.value, err)
			}
			if id != test.wantID || version != test.wantVersion {
				t.Fatalf("parseInteractionValue(%q) = %q/%d, want %q/%d",
					test.value, id, version, test.wantID, test.wantVersion)
			}
		})
	}
}

func TestValidateSlackTokenAndTimestamp(t *testing.T) {
	for _, value := range []string{"U123", "C0123456789", strings.Repeat("a", 128)} {
		if err := validateSlackToken(value, maxSlackUserIDRunes); err != nil {
			t.Fatalf("validateSlackToken(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", " ", "U 1", "U:1", "U\t1", "U\x7f1", strings.Repeat("a", 129)} {
		if err := validateSlackToken(value, maxSlackUserIDRunes); err == nil {
			t.Fatalf("validateSlackToken(%q) accepted the value", value)
		}
	}
	for _, value := range []string{"1732041600", "1732041600.123456"} {
		if err := validateSlackTimestamp(value); err != nil {
			t.Fatalf("validateSlackTimestamp(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", ".1", "1.", "1.2.3", "1-2", strings.Repeat("1", 65)} {
		if err := validateSlackTimestamp(value); err == nil {
			t.Fatalf("validateSlackTimestamp(%q) accepted the value", value)
		}
	}
}

func TestParseInteractionRequestForm(t *testing.T) {
	validPayload := unitPayload(t, ActionApprove, unitProposalID+":1", "U123", "C123", "1732041600.123456")

	t.Run("valid", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions",
			strings.NewReader(url.Values{"payload": {validPayload}}.Encode()))
		request.Header.Set("Content-Type", InteractionFormContentType)
		in, err := parseInteractionRequest(httptest.NewRecorder(), request)
		if err != nil {
			t.Fatalf("parseInteractionRequest: %+v", err)
		}
		if in.proposalID != unitProposalID || in.version != 1 {
			t.Fatalf("interaction = %+v", in)
		}
	})

	t.Run("wrong content type", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions",
			strings.NewReader(url.Values{"payload": {validPayload}}.Encode()))
		request.Header.Set("Content-Type", "application/json")
		_, err := parseInteractionRequest(httptest.NewRecorder(), request)
		if err == nil || err.status != http.StatusBadRequest {
			t.Fatalf("error = %+v, want 400", err)
		}
	})

	t.Run("missing payload", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions",
			strings.NewReader("other=value"))
		request.Header.Set("Content-Type", InteractionFormContentType)
		_, err := parseInteractionRequest(httptest.NewRecorder(), request)
		if err == nil || err.status != http.StatusBadRequest {
			t.Fatalf("error = %+v, want 400", err)
		}
	})

	t.Run("unknown form field", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions",
			strings.NewReader(url.Values{"payload": {validPayload}, "other": {"value"}}.Encode()))
		request.Header.Set("Content-Type", InteractionFormContentType)
		_, err := parseInteractionRequest(httptest.NewRecorder(), request)
		if err == nil || err.status != http.StatusBadRequest {
			t.Fatalf("error = %+v, want 400", err)
		}
	})

	t.Run("duplicate payload", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions",
			strings.NewReader(url.Values{"payload": {validPayload, validPayload}}.Encode()))
		request.Header.Set("Content-Type", InteractionFormContentType)
		_, err := parseInteractionRequest(httptest.NewRecorder(), request)
		if err == nil || err.status != http.StatusBadRequest {
			t.Fatalf("error = %+v, want 400", err)
		}
	})

	t.Run("payload too large", func(t *testing.T) {
		large := url.Values{"payload": {strings.Repeat("a", MaxInteractionPayloadBytes+1)}}.Encode()
		request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions", strings.NewReader(large))
		request.Header.Set("Content-Type", InteractionFormContentType)
		_, err := parseInteractionRequest(httptest.NewRecorder(), request)
		if err == nil || err.status != http.StatusBadRequest {
			t.Fatalf("error = %+v, want 400", err)
		}
	})
}

// --- handler-level unit harness -------------------------------------------

type unitDecisionService struct {
	approver      approval.Approver
	identityErr   error
	result        approval.DecisionResult
	decideErr     error
	identityCalls int
	decideCalls   int
	optionsCalls  int
	gotSlackUser  string
	gotProposalID string
	gotDecision   approval.Decision
	gotApproverID string
	gotSource     string
	gotOptions    approval.DecisionOptions
}

func (s *unitDecisionService) ApproverBySlackUserID(_ context.Context, slackUserID string) (approval.Approver, error) {
	s.identityCalls++
	s.gotSlackUser = slackUserID
	return s.approver, s.identityErr
}

func (s *unitDecisionService) Decide(_ context.Context, proposalID string, d approval.Decision, approverID, source string) (approval.DecisionResult, error) {
	s.decideCalls++
	s.gotProposalID = proposalID
	s.gotDecision = d
	s.gotApproverID = approverID
	s.gotSource = source
	return s.result, s.decideErr
}

func (s *unitDecisionService) DecideWithOptions(_ context.Context, proposalID string, d approval.Decision, approverID, source string, opts approval.DecisionOptions) (approval.DecisionResult, error) {
	s.optionsCalls++
	s.gotProposalID = proposalID
	s.gotDecision = d
	s.gotApproverID = approverID
	s.gotSource = source
	s.gotOptions = opts
	return s.result, s.decideErr
}

var errUnexpectedDatabaseUse = errors.New("unexpected database use")

type failingBeginner struct{}

func (failingBeginner) Begin(context.Context) (pgx.Tx, error) { return nil, errUnexpectedDatabaseUse }
func (failingBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errUnexpectedDatabaseUse
}

type failingQueryer struct{}

func (failingQueryer) QueryRow(context.Context, string, ...any) pgx.Row {
	return failingRow{}
}
func (failingQueryer) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errUnexpectedDatabaseUse
}

type failingRow struct{}

func (failingRow) Scan(...any) error { return errUnexpectedDatabaseUse }

type ephemeralCapture struct {
	mu       sync.Mutex
	requests []map[string]string
}

func (c *ephemeralCapture) all() []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]string(nil), c.requests...)
}

func newUnitInteractionHandler(t *testing.T, decisions *unitDecisionService) (*InteractionHandler, *ephemeralCapture) {
	t.Helper()
	return newUnitInteractionHandlerWithQueryer(t, decisions, failingQueryer{})
}

func newUnitInteractionHandlerWithQueryer(t *testing.T, decisions *unitDecisionService,
	queryer Queryer) (*InteractionHandler, *ephemeralCapture) {
	t.Helper()
	capture := &ephemeralCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fake slack body: %v", err)
		}
		var request map[string]string
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode fake slack body %q: %v", body, err)
		}
		capture.mu.Lock()
		capture.requests = append(capture.requests, request)
		capture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientOptions{
		BaseURL:    server.URL,
		BotToken:   "xoxb-unit-test-token",
		HTTPClient: server.Client(),
		Clock:      clock.NewFake(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	handler, err := NewInteractionHandler(InteractionOptions{
		DB:        failingBeginner{},
		Queryer:   queryer,
		Decisions: decisions,
		Client:    client,
		Clock:     clock.NewFake(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewInteractionHandler: %v", err)
	}
	return handler, capture
}

// staticRow serves deterministic Scan values for the interaction unit harness.
type staticRow struct {
	values []any
	err    error
}

func (r staticRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("staticRow: scan arity mismatch")
	}
	for i, target := range dest {
		switch typed := target.(type) {
		case *string:
			value, ok := r.values[i].(string)
			if !ok {
				return errors.New("staticRow: want string")
			}
			*typed = value
		case *int:
			value, ok := r.values[i].(int)
			if !ok {
				return errors.New("staticRow: want int")
			}
			*typed = value
		case *time.Time:
			value, ok := r.values[i].(time.Time)
			if !ok {
				return errors.New("staticRow: want time.Time")
			}
			*typed = value
		default:
			return errors.New("staticRow: unsupported scan target")
		}
	}
	return nil
}

// messageQueryer serves one posted approval_messages row and the pending
// proposal row stillActive reads. No write is expected on this path.
type messageQueryer struct {
	message slackMessageRow
}

type slackMessageRow struct {
	proposalID  string
	channel     string
	externalRef string
	state       string
	createdAt   time.Time
	updatedAt   time.Time
	version     int
	status      string
	caseStatus  string
}

func (q messageQueryer) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM approval_messages"):
		return staticRow{values: []any{
			q.message.proposalID, q.message.channel, q.message.externalRef,
			q.message.state, q.message.createdAt, q.message.updatedAt,
		}}
	case strings.Contains(sql, "FROM proposals"):
		return staticRow{values: []any{q.message.version, q.message.status, q.message.caseStatus}}
	default:
		return staticRow{err: errUnexpectedDatabaseUse}
	}
}

func (q messageQueryer) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errUnexpectedDatabaseUse
}

func serveInteraction(handler *InteractionHandler, payload string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions",
		strings.NewReader(url.Values{"payload": {payload}}.Encode()))
	request.Header.Set("Content-Type", InteractionFormContentType)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestInteractionRequestInfoUsesOptionsDecision(t *testing.T) {
	decisions := &unitDecisionService{
		approver: approval.Approver{ID: "approver-id", Role: "approver", Active: true},
		result: approval.DecisionResult{
			Decision: approval.DecisionRejected,
			Status:   "rejected",
		},
	}
	queryer := messageQueryer{message: slackMessageRow{
		proposalID:  unitProposalID,
		channel:     "slack",
		externalRef: "C123:1732041600.123456",
		state:       string(StatePosted),
		version:     1,
		status:      "pending_approval",
		caseStatus:  "proposal_pending",
	}}
	handler, capture := newUnitInteractionHandlerWithQueryer(t, decisions, queryer)
	payload := unitPayload(t, ActionRequestInfo, unitProposalID+":1", "U123", "C123", "1732041600.123456")

	recorder := serveInteraction(handler, payload)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body)
	}
	if decisions.identityCalls != 1 || decisions.gotSlackUser != "U123" {
		t.Fatalf("identity calls = %d user=%q", decisions.identityCalls, decisions.gotSlackUser)
	}
	if decisions.decideCalls != 0 || decisions.optionsCalls != 1 {
		t.Fatalf("Decide/DecideWithOptions calls = %d/%d, want 0/1",
			decisions.decideCalls, decisions.optionsCalls)
	}
	if decisions.gotDecision != approval.DecisionRejected || !decisions.gotOptions.InfoRequested ||
		decisions.gotOptions.Reason != "" {
		t.Fatalf("options decision = %q opts=%+v, want rejected with InfoRequested",
			decisions.gotDecision, decisions.gotOptions)
	}
	if decisions.gotApproverID != "approver-id" || decisions.gotSource != InteractionSource {
		t.Fatalf("decision approver/source = %q/%q", decisions.gotApproverID, decisions.gotSource)
	}
	requests := capture.all()
	if len(requests) != 1 {
		t.Fatalf("ephemeral requests = %d, want 1: %+v", len(requests), requests)
	}
	if requests[0]["text"] != textInfoRequested || requests[0]["channel"] != "C123" ||
		requests[0]["user"] != "U123" {
		t.Fatalf("ephemeral = %+v", requests[0])
	}
}

func TestInteractionRequestInfoStaleCaseIsOutOfDate(t *testing.T) {
	decisions := &unitDecisionService{
		approver: approval.Approver{ID: "approver-id", Role: "approver", Active: true},
	}
	queryer := messageQueryer{message: slackMessageRow{
		proposalID:  unitProposalID,
		channel:     "slack",
		externalRef: "C123:1732041600.123456",
		state:       string(StatePosted),
		version:     1,
		status:      "pending_approval",
		caseStatus:  "open",
	}}
	handler, capture := newUnitInteractionHandlerWithQueryer(t, decisions, queryer)
	payload := unitPayload(t, ActionRequestInfo, unitProposalID+":1", "U123", "C123", "1732041600.123456")

	recorder := serveInteraction(handler, payload)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body)
	}
	if decisions.decideCalls != 0 || decisions.optionsCalls != 0 {
		t.Fatalf("Decide/DecideWithOptions calls = %d/%d, want 0/0",
			decisions.decideCalls, decisions.optionsCalls)
	}
	requests := capture.all()
	if len(requests) != 1 || requests[0]["text"] != textOutOfDate {
		t.Fatalf("ephemeral requests = %+v, want one out-of-date message", requests)
	}
}

func TestInteractionMalformedRequestNeverCallsIdentityOrDatabase(t *testing.T) {
	decisions := &unitDecisionService{approver: approval.Approver{ID: "approver-id"}}
	handler, capture := newUnitInteractionHandler(t, decisions)

	request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions",
		strings.NewReader("payload=not-json"))
	request.Header.Set("Content-Type", InteractionFormContentType)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if decisions.identityCalls != 0 || decisions.decideCalls != 0 {
		t.Fatalf("identity/decide calls = %d/%d, want 0/0", decisions.identityCalls, decisions.decideCalls)
	}
	if len(capture.all()) != 0 {
		t.Fatal("malformed request posted an ephemeral message")
	}
}

func TestInteractionUnauthorizedDenialFailsClosedWithoutDatabase(t *testing.T) {
	// The unit harness has a failing DB. An unknown-user denial must attempt
	// the audit and return 500 rather than pretend the denial was recorded.
	decisions := &unitDecisionService{identityErr: approval.ErrApproverNotFound}
	handler, capture := newUnitInteractionHandler(t, decisions)
	payload := unitPayload(t, ActionApprove, unitProposalID+":1", "U123", "C123", "1732041600.123456")

	recorder := serveInteraction(handler, payload)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the denial audit cannot commit; body=%s",
			recorder.Code, recorder.Body)
	}
	if decisions.decideCalls != 0 {
		t.Fatalf("Decide calls = %d, want 0", decisions.decideCalls)
	}
	if len(capture.all()) != 0 {
		t.Fatal("ephemeral denial was posted before the audit committed")
	}
}
