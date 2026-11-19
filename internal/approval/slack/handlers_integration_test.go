//go:build integration

package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/slack"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

const (
	handlerSlackChannelID      = "C0123456789"
	handlerCaseFileBaseURL     = "https://cases.example.test"
	handlerSlackTS             = "1732041600.123456"
	handlerExternalRef         = handlerSlackChannelID + ":" + handlerSlackTS
	handlerApproverDisplayName = "Ada Lovelace"
)

// slackCall is one request the fake Slack Web API server observed.
type slackCall struct {
	Path    string
	Body    []byte
	Attempt int
}

// fakeSlackAPI is a minimal chat.postMessage/chat.update server. Tests inject
// a response function per scenario and may run onCall hooks before the
// response is written (used for post/cancel race tests and the FIN-22 check).
type fakeSlackAPI struct {
	mu       sync.Mutex
	calls    []slackCall
	attempts map[string]int
	respond  func(slackCall, http.ResponseWriter)
	onCall   func(slackCall)
}

func (f *fakeSlackAPI) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	if f.attempts == nil {
		f.attempts = make(map[string]int)
	}
	f.attempts[r.URL.Path]++
	call := slackCall{Path: r.URL.Path, Body: body, Attempt: f.attempts[r.URL.Path]}
	f.calls = append(f.calls, call)
	respond := f.respond
	onCall := f.onCall
	f.mu.Unlock()

	if onCall != nil {
		onCall(call)
	}
	if respond != nil {
		respond(call, w)
		return
	}
	writeDefaultSlackResponse(w, call.Path)
}

func (f *fakeSlackAPI) setRespond(respond func(slackCall, http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func (f *fakeSlackAPI) setOnCall(onCall func(slackCall)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onCall = onCall
}

func (f *fakeSlackAPI) callsFor(path string) []slackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []slackCall
	for _, call := range f.calls {
		if call.Path == path {
			out = append(out, call)
		}
	}
	return out
}

func writeDefaultSlackResponse(w http.ResponseWriter, path string) {
	w.Header().Set("Content-Type", "application/json")
	switch path {
	case "/chat.postMessage":
		_, _ = io.WriteString(w,
			`{"ok":true,"ts":"`+handlerSlackTS+`","channel":"`+handlerSlackChannelID+`"}`)
	default:
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
}

func writeSlackError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":false,"error":"`+code+`"}`)
}

type slackTextBody struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type slackBlockBody struct {
	Type string         `json:"type"`
	Text *slackTextBody `json:"text"`
}

type slackRequestBody struct {
	Channel     string           `json:"channel"`
	TS          string           `json:"ts"`
	Text        string           `json:"text"`
	ClientMsgID string           `json:"client_msg_id"`
	Blocks      []slackBlockBody `json:"blocks"`
}

func decodeSlackBody(t *testing.T, call slackCall) slackRequestBody {
	t.Helper()
	var body slackRequestBody
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatalf("decode %s body: %v\n%s", call.Path, err, call.Body)
	}
	return body
}

func slackBodyText(body slackRequestBody) string {
	var b strings.Builder
	b.WriteString(body.Text)
	for _, block := range body.Blocks {
		if block.Text != nil {
			b.WriteString("\n")
			b.WriteString(block.Text.Text)
		}
	}
	return b.String()
}

func slackBodyHasAction(body slackRequestBody) bool {
	for _, block := range body.Blocks {
		if block.Type == "actions" {
			return true
		}
	}
	return false
}

// handlerFixture is a real test database plus a fake Slack API. It reuses
// sourceFixture to seed cases, breaks, and proposals.
type handlerFixture struct {
	*sourceFixture
	server     *httptest.Server
	fake       *fakeSlackAPI
	sleeper    *recordingSleeper
	clock      *clock.Fake
	handlers   *slack.Handlers
	approverID string
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	sf := newSourceFixture(t)
	fake := &fakeSlackAPI{}
	server := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(server.Close)
	clk := clock.NewFake(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	sleeper := &recordingSleeper{}
	f := &handlerFixture{
		sourceFixture: sf,
		server:        server,
		fake:          fake,
		clock:         clk,
		sleeper:       sleeper,
	}
	f.handlers = f.mustHandlers(t, newHandlerSlackClient(t, server, clk, sleeper))

	var approverID string
	if err := sf.d.Pool.QueryRow(context.Background(),
		`INSERT INTO approvers (display_name, role) VALUES ($1, 'approver')
		 RETURNING id::text`, handlerApproverDisplayName).Scan(&approverID); err != nil {
		t.Fatalf("seed approver: %v", err)
	}
	f.approverID = approverID
	return f
}

func newHandlerSlackClient(t *testing.T, server *httptest.Server, clk clock.Clock, sleeper slack.Sleeper) *slack.Client {
	t.Helper()
	client, err := slack.NewClient(slack.ClientOptions{
		BaseURL:    server.URL,
		BotToken:   testBotToken,
		HTTPClient: server.Client(),
		Clock:      clk,
		Sleeper:    sleeper,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func (f *handlerFixture) mustHandlers(t *testing.T, client *slack.Client) *slack.Handlers {
	t.Helper()
	handlers, err := slack.NewHandlers(slack.HandlersOptions{
		Queryer:         f.app,
		Client:          client,
		ChannelID:       handlerSlackChannelID,
		CaseFileBaseURL: handlerCaseFileBaseURL,
		Clock:           f.clock,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	return handlers
}

type handlerProposal struct {
	CaseID     string
	ProposalID string
}

func (f *handlerFixture) seedPending(t *testing.T) handlerProposal {
	t.Helper()
	created := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	caseID := f.newCase(2, 123456, "USD", created)
	f.attachBreak(caseID, "external_only", 1250, "USD", created.Add(time.Minute))
	proposalID := f.newProposal(sourceProposalSeed{
		caseID:          caseID,
		typ:             "write_off",
		riskTier:        "T1",
		patternVerified: "fee_variance",
		verification:    `{"reason":"within fee schedule","expected_minor":11250,"actual_minor":12500,"currency":"USD"}`,
		policyDecision:  `{"schema_version":1,"tier":"T1","rule":"high_value","reasons":["high_value"],"config_hash":"` + strings.Repeat("a", 64) + `"}`,
		rationale:       "The bank fee is 12.50 USD higher than the schedule.",
		evidenceJSON:    `["ext:bank:abc123","ledger_txn:11111111-2222-3333-4444-555555555555"]`,
		expiresAt:       time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
	})
	return handlerProposal{CaseID: caseID, ProposalID: proposalID}
}

func (f *handlerFixture) insertPosted(proposalID string) {
	f.t.Helper()
	inserted, err := (&slack.Store{}).InsertPosted(context.Background(), f.app,
		proposalID, approval.ChannelSlack, handlerExternalRef)
	if err != nil {
		f.t.Fatalf("InsertPosted: %v", err)
	}
	if !inserted {
		f.t.Fatalf("InsertPosted inserted=false for %s", proposalID)
	}
}

func (f *handlerFixture) messageRow(proposalID string) (slack.Message, bool) {
	f.t.Helper()
	message, found, err := (&slack.Store{}).Get(context.Background(), f.app,
		proposalID, approval.ChannelSlack)
	if err != nil {
		f.t.Fatalf("Get message: %v", err)
	}
	return message, found
}

// setProposalStatus is called from either the test goroutine or the fake Slack
// server goroutine, so it records errors with Errorf rather than Fatalf.
func (f *handlerFixture) setProposalStatus(proposalID, status string) {
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE proposals SET status = $2 WHERE id = $1::uuid`, proposalID, status); err != nil {
		f.t.Errorf("set proposal status: %v", err)
	}
}

func (f *handlerFixture) setMessageState(proposalID, state string) {
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE approval_messages SET state = $2, updated_at = now()
		  WHERE proposal_id = $1::uuid AND channel = $3`,
		proposalID, state, approval.ChannelSlack); err != nil {
		f.t.Errorf("set message state: %v", err)
	}
}

func requestOutboxMessage(t *testing.T, p handlerProposal) outbox.Message {
	t.Helper()
	payload, err := json.Marshal(slack.ApprovalRequestPayload{
		ProposalID:  p.ProposalID,
		CaseID:      p.CaseID,
		Tier:        "T1",
		Channel:     approval.ChannelSlack,
		RequestedAt: "2026-11-19T12:00:00.123456789Z",
	})
	if err != nil {
		t.Fatalf("marshal request payload: %v", err)
	}
	return outbox.Message{
		ID:             "outbox-request",
		Topic:          approval.TopicApprovalRequest,
		Payload:        payload,
		IdempotencyKey: approval.TopicApprovalRequest + ":" + p.ProposalID,
	}
}

func updateOutboxMessage(t *testing.T, p handlerProposal, approverID string,
	decision approval.Decision, infoRequested bool, reason string) outbox.Message {
	t.Helper()
	payload, err := json.Marshal(slack.ApprovalUpdatePayload{
		ProposalID:    p.ProposalID,
		CaseID:        p.CaseID,
		Decision:      string(decision),
		Status:        string(decision),
		Source:        "slack",
		ApproverID:    approverID,
		DecidedAt:     "2026-11-19T12:30:00.123456789Z",
		InfoRequested: infoRequested,
		Reason:        reason,
	})
	if err != nil {
		t.Fatalf("marshal update payload: %v", err)
	}
	return outbox.Message{
		ID:             "outbox-update",
		Topic:          approval.TopicApprovalUpdate,
		Payload:        payload,
		IdempotencyKey: approval.TopicApprovalUpdate + ":" + p.ProposalID,
	}
}

func cancelOutboxMessage(t *testing.T, p handlerProposal, reason string) outbox.Message {
	t.Helper()
	payload, err := json.Marshal(slack.ApprovalCancelPayload{
		ProposalID: p.ProposalID,
		CaseID:     p.CaseID,
		Reason:     reason,
	})
	if err != nil {
		t.Fatalf("marshal cancel payload: %v", err)
	}
	return outbox.Message{
		ID:             "outbox-cancel",
		Topic:          "approval.cancel",
		Payload:        payload,
		IdempotencyKey: "approval.cancel:" + p.ProposalID,
	}
}

func TestHandleRequestPostsOnceAndStoresRef(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	ctx := context.Background()

	if err := f.handlers.HandleRequest(ctx, requestOutboxMessage(t, p)); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}
	calls := f.fake.callsFor("/chat.postMessage")
	if len(calls) != 1 {
		t.Fatalf("post calls = %d, want 1", len(calls))
	}
	body := decodeSlackBody(t, calls[0])
	if body.Channel != handlerSlackChannelID {
		t.Fatalf("post channel = %q", body.Channel)
	}
	if body.ClientMsgID != p.ProposalID {
		t.Fatalf("client_msg_id = %q, want %q", body.ClientMsgID, p.ProposalID)
	}
	if !strings.Contains(body.Text, "Approval required") {
		t.Fatalf("fallback = %q", body.Text)
	}
	if len(body.Blocks) < 9 || !slackBodyHasAction(body) {
		t.Fatalf("request message blocks = %d action=%t", len(body.Blocks), slackBodyHasAction(body))
	}
	if !strings.Contains(slackBodyText(body), "The bank fee is 12.50 USD") {
		t.Fatalf("request message missing rationale:\n%s", slackBodyText(body))
	}

	row, found := f.messageRow(p.ProposalID)
	if !found {
		t.Fatal("approval_messages row was not inserted")
	}
	if row.State != slack.StatePosted || row.ExternalRef != handlerExternalRef {
		t.Fatalf("stored row = %+v", row)
	}

	// Redelivery is an outbox replay: the existing row is the durable guard.
	if err := f.handlers.HandleRequest(ctx, requestOutboxMessage(t, p)); err != nil {
		t.Fatalf("HandleRequest replay: %v", err)
	}
	if got := len(f.fake.callsFor("/chat.postMessage")); got != 1 {
		t.Fatalf("post calls after replay = %d, want 1", got)
	}
	row, _ = f.messageRow(p.ProposalID)
	if row.State != slack.StatePosted || row.ExternalRef != handlerExternalRef {
		t.Fatalf("row changed on replay: %+v", row)
	}
}

func TestHandleRequestSkipsNonSlackChannel(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	payload, err := json.Marshal(slack.ApprovalRequestPayload{
		ProposalID: p.ProposalID,
		CaseID:     p.CaseID,
		Tier:       "T2",
		Channel:    approval.ChannelTicket,
	})
	if err != nil {
		t.Fatalf("marshal ticket payload: %v", err)
	}
	msg := outbox.Message{Topic: approval.TopicApprovalRequest, Payload: payload}
	if err := f.handlers.HandleRequest(context.Background(), msg); err != nil {
		t.Fatalf("HandleRequest ticket: %v", err)
	}
	if got := len(f.fake.callsFor("/chat.postMessage")); got != 0 {
		t.Fatalf("post calls = %d, want 0", got)
	}
	if _, found := f.messageRow(p.ProposalID); found {
		t.Fatal("ticket request inserted a Slack message row")
	}
}

func TestHandleRequestNoopsWhenAlreadyPostedOrNotPending(t *testing.T) {
	t.Run("existing row", func(t *testing.T) {
		f := newHandlerFixture(t)
		p := f.seedPending(t)
		f.insertPosted(p.ProposalID)
		if err := f.handlers.HandleRequest(context.Background(), requestOutboxMessage(t, p)); err != nil {
			t.Fatalf("HandleRequest: %v", err)
		}
		if got := len(f.fake.callsFor("/chat.postMessage")); got != 0 {
			t.Fatalf("post calls = %d, want 0", got)
		}
	})
	t.Run("not pending", func(t *testing.T) {
		f := newHandlerFixture(t)
		p := f.seedPending(t)
		f.setProposalStatus(p.ProposalID, "superseded")
		if err := f.handlers.HandleRequest(context.Background(), requestOutboxMessage(t, p)); err != nil {
			t.Fatalf("HandleRequest: %v", err)
		}
		if got := len(f.fake.callsFor("/chat.postMessage")); got != 0 {
			t.Fatalf("post calls = %d, want 0", got)
		}
		if _, found := f.messageRow(p.ProposalID); found {
			t.Fatal("non-pending proposal inserted a Slack message row")
		}
	})
}

func TestHandleRequestWithdrawsWhenStatusChangesDuringPost(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	f.fake.setOnCall(func(call slackCall) {
		if call.Path == "/chat.postMessage" && call.Attempt == 1 {
			f.setProposalStatus(p.ProposalID, "superseded")
		}
	})
	if err := f.handlers.HandleRequest(context.Background(), requestOutboxMessage(t, p)); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}
	if got := len(f.fake.callsFor("/chat.postMessage")); got != 1 {
		t.Fatalf("post calls = %d, want 1", got)
	}
	updates := f.fake.callsFor("/chat.update")
	if len(updates) != 1 {
		t.Fatalf("update calls = %d, want 1", len(updates))
	}
	body := decodeSlackBody(t, updates[0])
	if body.Channel != handlerSlackChannelID || body.TS != handlerSlackTS {
		t.Fatalf("withdraw target = %q/%q", body.Channel, body.TS)
	}
	if !strings.Contains(body.Text, "Approval withdrawn") || slackBodyHasAction(body) {
		t.Fatalf("withdraw body = %q action=%t", body.Text, slackBodyHasAction(body))
	}
	row, found := f.messageRow(p.ProposalID)
	if !found || row.State != slack.StateCancelled {
		t.Fatalf("row after post/cancel race = %+v found=%t", row, found)
	}
}

func TestHandleUpdateUpdatesInPlaceAndIsIdempotent(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	f.insertPosted(p.ProposalID)
	f.setProposalStatus(p.ProposalID, "approved")
	ctx := context.Background()

	msg := updateOutboxMessage(t, p, f.approverID, approval.DecisionApproved, false, "")
	if err := f.handlers.HandleUpdate(ctx, msg); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	updates := f.fake.callsFor("/chat.update")
	if len(updates) != 1 {
		t.Fatalf("update calls = %d, want 1", len(updates))
	}
	body := decodeSlackBody(t, updates[0])
	if body.Channel != handlerSlackChannelID || body.TS != handlerSlackTS {
		t.Fatalf("update target = %q/%q", body.Channel, body.TS)
	}
	if slackBodyHasAction(body) {
		t.Fatal("decided message still has an actions block")
	}
	text := slackBodyText(body)
	for _, want := range []string{
		"Approval approved",
		handlerApproverDisplayName,
		"2026-11-19T12:30:00Z",
		"*Decision* Approved",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("decided message missing %q:\n%s", want, text)
		}
	}
	row, found := f.messageRow(p.ProposalID)
	if !found || row.State != slack.StateUpdated {
		t.Fatalf("row after update = %+v found=%t", row, found)
	}

	// Same-decision redelivery is a no-op.
	if err := f.handlers.HandleUpdate(ctx, msg); err != nil {
		t.Fatalf("HandleUpdate replay: %v", err)
	}
	if got := len(f.fake.callsFor("/chat.update")); got != 1 {
		t.Fatalf("update calls after replay = %d, want 1", got)
	}
}

func TestHandleUpdateNoopsWithoutRowOrWhenTerminal(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		f := newHandlerFixture(t)
		p := f.seedPending(t)
		msg := updateOutboxMessage(t, p, f.approverID, approval.DecisionApproved, false, "")
		if err := f.handlers.HandleUpdate(context.Background(), msg); err != nil {
			t.Fatalf("HandleUpdate: %v", err)
		}
		if got := len(f.fake.callsFor("/chat.update")); got != 0 {
			t.Fatalf("update calls = %d, want 0", got)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		f := newHandlerFixture(t)
		p := f.seedPending(t)
		f.insertPosted(p.ProposalID)
		f.setMessageState(p.ProposalID, string(slack.StateCancelled))
		msg := updateOutboxMessage(t, p, f.approverID, approval.DecisionApproved, false, "")
		if err := f.handlers.HandleUpdate(context.Background(), msg); err != nil {
			t.Fatalf("HandleUpdate: %v", err)
		}
		if got := len(f.fake.callsFor("/chat.update")); got != 0 {
			t.Fatalf("update calls = %d, want 0", got)
		}
	})
	t.Run("already updated", func(t *testing.T) {
		f := newHandlerFixture(t)
		p := f.seedPending(t)
		f.insertPosted(p.ProposalID)
		if _, err := (&slack.Store{}).MarkUpdated(context.Background(), f.app,
			p.ProposalID, approval.ChannelSlack); err != nil {
			t.Fatalf("MarkUpdated: %v", err)
		}
		msg := updateOutboxMessage(t, p, f.approverID, approval.DecisionApproved, false, "")
		if err := f.handlers.HandleUpdate(context.Background(), msg); err != nil {
			t.Fatalf("HandleUpdate: %v", err)
		}
		if got := len(f.fake.callsFor("/chat.update")); got != 0 {
			t.Fatalf("update calls = %d, want 0", got)
		}
	})
}

func TestHandleUpdateInfoRequestedLabel(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	f.insertPosted(p.ProposalID)
	f.setProposalStatus(p.ProposalID, "rejected")
	msg := updateOutboxMessage(t, p, f.approverID, approval.DecisionRejected, true, "Need the bank statement")
	if err := f.handlers.HandleUpdate(context.Background(), msg); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	updates := f.fake.callsFor("/chat.update")
	if len(updates) != 1 {
		t.Fatalf("update calls = %d, want 1", len(updates))
	}
	text := slackBodyText(decodeSlackBody(t, updates[0]))
	for _, want := range []string{"Rejected — information requested", "*Reason* Need the bank statement"} {
		if !strings.Contains(text, want) {
			t.Fatalf("info-requested message missing %q:\n%s", want, text)
		}
	}
}

func TestHandleUpdateReappliesWithdrawalWhenCancelledDuringUpdate(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	f.insertPosted(p.ProposalID)
	f.fake.setOnCall(func(call slackCall) {
		if call.Path == "/chat.update" && call.Attempt == 1 {
			f.setMessageState(p.ProposalID, string(slack.StateCancelled))
		}
	})
	msg := updateOutboxMessage(t, p, f.approverID, approval.DecisionApproved, false, "")
	if err := f.handlers.HandleUpdate(context.Background(), msg); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	updates := f.fake.callsFor("/chat.update")
	if len(updates) != 2 {
		t.Fatalf("update calls = %d, want 2 (decided then withdrawn)", len(updates))
	}
	last := decodeSlackBody(t, updates[1])
	if !strings.Contains(last.Text, "Approval withdrawn") || slackBodyHasAction(last) {
		t.Fatalf("race withdrawal = %q action=%t", last.Text, slackBodyHasAction(last))
	}
	row, found := f.messageRow(p.ProposalID)
	if !found || row.State != slack.StateCancelled {
		t.Fatalf("row after update/cancel race = %+v found=%t", row, found)
	}
}

func TestHandleCancelMarksAndIsIdempotent(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	f.insertPosted(p.ProposalID)
	ctx := context.Background()

	msg := cancelOutboxMessage(t, p, "superseded_by_match")
	if err := f.handlers.HandleCancel(ctx, msg); err != nil {
		t.Fatalf("HandleCancel: %v", err)
	}
	updates := f.fake.callsFor("/chat.update")
	if len(updates) != 1 {
		t.Fatalf("update calls = %d, want 1", len(updates))
	}
	body := decodeSlackBody(t, updates[0])
	if body.Channel != handlerSlackChannelID || body.TS != handlerSlackTS {
		t.Fatalf("cancel target = %q/%q", body.Channel, body.TS)
	}
	if slackBodyHasAction(body) {
		t.Fatal("withdrawn message still has an actions block")
	}
	text := slackBodyText(body)
	if !strings.Contains(text, "Approval withdrawn") ||
		!strings.Contains(text, "superseded_by_match") {
		t.Fatalf("withdrawn message:\n%s", text)
	}
	row, found := f.messageRow(p.ProposalID)
	if !found || row.State != slack.StateCancelled {
		t.Fatalf("row after cancel = %+v found=%t", row, found)
	}

	// Redelivery and an absent row are both acknowledged no-ops.
	if err := f.handlers.HandleCancel(ctx, msg); err != nil {
		t.Fatalf("HandleCancel replay: %v", err)
	}
	if got := len(f.fake.callsFor("/chat.update")); got != 1 {
		t.Fatalf("update calls after replay = %d, want 1", got)
	}
	other := newHandlerFixture(t)
	otherProposal := other.seedPending(t)
	if err := other.handlers.HandleCancel(ctx, cancelOutboxMessage(t, otherProposal, "gone")); err != nil {
		t.Fatalf("HandleCancel absent: %v", err)
	}
	if got := len(other.fake.callsFor("/chat.update")); got != 0 {
		t.Fatalf("absent cancel update calls = %d, want 0", got)
	}
}

func TestHandleCancelReappliesWithdrawalWhenUpdateWon(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	f.insertPosted(p.ProposalID)
	f.fake.setOnCall(func(call slackCall) {
		if call.Path == "/chat.update" && call.Attempt == 1 {
			f.setMessageState(p.ProposalID, string(slack.StateUpdated))
		}
	})
	if err := f.handlers.HandleCancel(context.Background(),
		cancelOutboxMessage(t, p, "superseded_by_new_proposal")); err != nil {
		t.Fatalf("HandleCancel: %v", err)
	}
	updates := f.fake.callsFor("/chat.update")
	if len(updates) != 2 {
		t.Fatalf("update calls = %d, want 2 (withdrawn then re-applied)", len(updates))
	}
	row, found := f.messageRow(p.ProposalID)
	if !found || row.State != slack.StateCancelled {
		t.Fatalf("row after cancel/update race = %+v found=%t", row, found)
	}
}

func TestHandleUpdateGoneMessageMarksCancelled(t *testing.T) {
	for _, code := range []string{"message_not_found", "cant_update_message"} {
		t.Run(code, func(t *testing.T) {
			f := newHandlerFixture(t)
			p := f.seedPending(t)
			f.insertPosted(p.ProposalID)
			f.fake.setRespond(func(call slackCall, w http.ResponseWriter) {
				writeSlackError(w, code)
			})
			msg := updateOutboxMessage(t, p, f.approverID, approval.DecisionApproved, false, "")
			if err := f.handlers.HandleUpdate(context.Background(), msg); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			row, found := f.messageRow(p.ProposalID)
			if !found || row.State != slack.StateCancelled {
				t.Fatalf("row after gone message = %+v found=%t", row, found)
			}
		})
	}
}

func TestHandleRequestRetriesRateLimit(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	f.fake.setRespond(func(call slackCall, w http.ResponseWriter) {
		if call.Path == "/chat.postMessage" && call.Attempt == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			writeSlackError(w, "ratelimited")
			return
		}
		writeDefaultSlackResponse(w, call.Path)
	})
	client := newHandlerSlackClient(t, f.server, f.clock, f.sleeper)
	handlers := f.mustHandlers(t, client)
	if err := handlers.HandleRequest(context.Background(), requestOutboxMessage(t, p)); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}
	if got := len(f.fake.callsFor("/chat.postMessage")); got != 2 {
		t.Fatalf("post attempts = %d, want 2", got)
	}
	waits := f.sleeper.snapshot()
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("retry waits = %v, want [1s]", waits)
	}
	row, found := f.messageRow(p.ProposalID)
	if !found || row.State != slack.StatePosted || row.ExternalRef != handlerExternalRef {
		t.Fatalf("row after 429 retry = %+v found=%t", row, found)
	}
}

func TestHandleRequestNoNetworkInsideTransaction(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	f.fake.setOnCall(func(call slackCall) {
		ctx := context.Background()
		var idle int
		if err := f.d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database()
			    AND state = 'idle in transaction'
			    AND pid <> pg_backend_pid()`).Scan(&idle); err != nil {
			t.Errorf("inspect pg_stat_activity: %v", err)
		} else if idle != 0 {
			t.Errorf("handler held %d idle-in-transaction connection(s) across the Slack call", idle)
		}
		var one int
		if err := f.d.Pool.QueryRow(ctx,
			`SELECT 1 FROM proposals WHERE id = $1::uuid FOR UPDATE NOWAIT`,
			p.ProposalID).Scan(&one); err != nil {
			t.Errorf("handler held a proposal lock across the Slack call: %v", err)
		}
	})
	if err := f.handlers.HandleRequest(context.Background(), requestOutboxMessage(t, p)); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}
	if _, found := f.messageRow(p.ProposalID); !found {
		t.Fatal("request did not complete after the no-transaction check")
	}
}

func TestHandlersMalformedPayloadsAreTyped(t *testing.T) {
	f := newHandlerFixture(t)
	p := f.seedPending(t)
	ctx := context.Background()

	if err := f.handlers.HandleRequest(ctx, outbox.Message{Payload: json.RawMessage(`{`)}); !errors.Is(err, slack.ErrInvalidPayload) {
		t.Fatalf("request malformed error = %v, want ErrInvalidPayload", err)
	}
	if err := f.handlers.HandleUpdate(ctx, outbox.Message{Payload: json.RawMessage(`{"proposal_id":"` + p.ProposalID + `"}`)}); !errors.Is(err, slack.ErrInvalidPayload) {
		t.Fatalf("update malformed error = %v, want ErrInvalidPayload", err)
	}
	if err := f.handlers.HandleCancel(ctx, outbox.Message{Payload: json.RawMessage(`{"proposal_id":"` + p.ProposalID + `","case_id":"` + p.CaseID + `","surprise":1}`)}); !errors.Is(err, slack.ErrInvalidPayload) {
		t.Fatalf("cancel malformed error = %v, want ErrInvalidPayload", err)
	}
	if got := len(f.fake.callsFor("/chat.postMessage")) + len(f.fake.callsFor("/chat.update")); got != 0 {
		t.Fatalf("malformed payloads made %d Slack call(s), want 0", got)
	}
}
