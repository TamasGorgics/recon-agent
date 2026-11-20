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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const (
	channelProposalID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	channelCaseID     = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	channelID         = "C-CHANNEL"
	channelTS         = "1732041600.123456"
	channelBaseURL    = "https://recon.example"
)

type channelSourceFake struct {
	data     MessageData
	err      error
	calls    int
	proposal string
}

func (s *channelSourceFake) ApprovalMessage(_ context.Context, proposalID string) (MessageData, error) {
	s.calls++
	s.proposal = proposalID
	return s.data, s.err
}

type channelStoreFake struct {
	mu        sync.Mutex
	rows      map[string]Message
	inserted  []string
	getErr    error
	insertErr error
}

func newChannelStoreFake() *channelStoreFake {
	return &channelStoreFake{rows: map[string]Message{}}
}

func channelStoreKey(proposalID, channel string) string { return proposalID + "|" + channel }

func (s *channelStoreFake) Get(_ context.Context, proposalID, channel string) (Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return Message{}, false, s.getErr
	}
	message, found := s.rows[channelStoreKey(proposalID, channel)]
	return message, found, nil
}

func (s *channelStoreFake) InsertPosted(_ context.Context, proposalID, channel, externalRef string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return false, s.insertErr
	}
	key := channelStoreKey(proposalID, channel)
	if _, exists := s.rows[key]; exists {
		return false, nil
	}
	s.rows[key] = Message{
		ProposalID:  proposalID,
		Channel:     channel,
		ExternalRef: externalRef,
		State:       StatePosted,
	}
	s.inserted = append(s.inserted, externalRef)
	return true, nil
}

type channelApproverFake struct {
	approver approval.Approver
	err      error
	calls    int
	gotUser  string
}

func (a *channelApproverFake) ApproverBySlackUserID(_ context.Context, slackUserID string) (approval.Approver, error) {
	a.calls++
	a.gotUser = slackUserID
	return a.approver, a.err
}

type channelSlackFake struct {
	server          *httptest.Server
	mu              sync.Mutex
	posts           []map[string]any
	updates         []map[string]any
	responseChannel string
	failPost        bool
	failUpdate      bool
}

func newChannelSlackFake(t *testing.T) *channelSlackFake {
	t.Helper()
	fake := &channelSlackFake{responseChannel: channelID}
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
		switch method {
		case "chat.postMessage":
			fake.posts = append(fake.posts, payload)
		case "chat.update":
			fake.updates = append(fake.updates, payload)
		}
		failPost := fake.failPost
		failUpdate := fake.failUpdate
		responseChannel := fake.responseChannel
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case method == "chat.postMessage" && failPost:
			_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
		case method == "chat.update" && failUpdate:
			_, _ = w.Write([]byte(`{"ok":false,"error":"message_not_found"}`))
		case method == "chat.postMessage":
			response := map[string]any{"ok": true, "ts": channelTS, "channel": responseChannel}
			encoded, _ := json.Marshal(response)
			_, _ = w.Write(encoded)
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *channelSlackFake) client(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{
		BaseURL:    f.server.URL,
		BotToken:   "xoxb-channel-test-token",
		HTTPClient: f.server.Client(),
		Clock:      clock.NewFake(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func channelMessageData() MessageData {
	return MessageData{
		ProposalID:             channelProposalID,
		ProposalType:           "adjustment",
		ProposalVersion:        3,
		ProposalStatus:         "pending_approval",
		RiskTier:               "T1",
		CaseID:                 channelCaseID,
		CaseValueMinor:         1234,
		CaseCurrency:           "USD",
		CasePriority:           2,
		CaseCreatedAt:          time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC),
		BreakCount:             1,
		PrimaryBreakSide:       "external_only",
		PrimaryBreakDeltaMinor: 1234,
		PrimaryBreakCurrency:   "USD",
		Rationale:              "fee variance",
	}
}

func newTestChannel(t *testing.T, source Source, store ChannelStore,
	approvers SlackApproverReader, slack *channelSlackFake) *Channel {
	t.Helper()
	channel, err := NewChannel(ChannelOptions{
		Source:          source,
		Store:           store,
		Approvers:       approvers,
		Client:          slack.client(t),
		ChannelID:       channelID,
		CaseFileBaseURL: channelBaseURL,
		Clock:           clock.NewFake(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)),
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}
	return channel
}

func TestNewChannelValidatesOptions(t *testing.T) {
	slack := newChannelSlackFake(t)
	source := &channelSourceFake{data: channelMessageData()}
	store := newChannelStoreFake()
	approvers := &channelApproverFake{approver: approval.Approver{ID: "approver-id"}}
	client := slack.client(t)

	valid := ChannelOptions{
		Source:          source,
		Store:           store,
		Approvers:       approvers,
		Client:          client,
		ChannelID:       channelID,
		CaseFileBaseURL: channelBaseURL,
	}
	if _, err := NewChannel(valid); err != nil {
		t.Fatalf("NewChannel valid options: %v", err)
	}

	tests := []struct {
		name string
		opts ChannelOptions
	}{
		{name: "nil source", opts: func() ChannelOptions { o := valid; o.Source = nil; return o }()},
		{name: "nil store", opts: func() ChannelOptions { o := valid; o.Store = nil; return o }()},
		{name: "nil approvers", opts: func() ChannelOptions { o := valid; o.Approvers = nil; return o }()},
		{name: "nil client", opts: func() ChannelOptions { o := valid; o.Client = nil; return o }()},
		{name: "empty channel", opts: func() ChannelOptions { o := valid; o.ChannelID = ""; return o }()},
		{name: "empty base url", opts: func() ChannelOptions { o := valid; o.CaseFileBaseURL = ""; return o }()},
		{name: "bad base url", opts: func() ChannelOptions { o := valid; o.CaseFileBaseURL = "ftp://x"; return o }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewChannel(test.opts); !errors.Is(err, ErrInvalidHandler) {
				t.Fatalf("NewChannel error = %v, want ErrInvalidHandler", err)
			}
		})
	}
}

func TestChannelRequestApprovalPostsPersistsAndReturnsRef(t *testing.T) {
	slack := newChannelSlackFake(t)
	source := &channelSourceFake{data: channelMessageData()}
	store := newChannelStoreFake()
	approvers := &channelApproverFake{approver: approval.Approver{ID: "approver-id"}}
	channel := newTestChannel(t, source, store, approvers, slack)

	ref, err := channel.RequestApproval(context.Background(), approval.ApprovalRequest{
		ProposalID: channelProposalID,
		CaseID:     channelCaseID,
		Channel:    approval.ChannelSlack,
		Tier:       "T1",
	})
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if ref != channelID+":"+channelTS {
		t.Fatalf("ref = %q, want %q", ref, channelID+":"+channelTS)
	}
	if source.calls != 1 || source.proposal != channelProposalID {
		t.Fatalf("source calls = %d proposal=%q", source.calls, source.proposal)
	}
	if len(slack.posts) != 1 {
		t.Fatalf("Slack posts = %d, want 1", len(slack.posts))
	}
	post := slack.posts[0]
	if post["channel"] != channelID || post["client_msg_id"] != channelProposalID {
		t.Fatalf("post = %+v, want channel/client_msg_id", post)
	}
	if text, _ := post["text"].(string); !strings.Contains(text, "Approval required") {
		t.Fatalf("post fallback text = %q", text)
	}
	blocks, _ := post["blocks"].([]any)
	if len(blocks) == 0 {
		t.Fatal("post has no blocks")
	}

	store.mu.Lock()
	stored, found := store.rows[channelStoreKey(channelProposalID, approval.ChannelSlack)]
	inserted := append([]string(nil), store.inserted...)
	store.mu.Unlock()
	if !found || stored.State != StatePosted || stored.ExternalRef != ref {
		t.Fatalf("stored row = %+v found=%v", stored, found)
	}
	if len(inserted) != 1 || inserted[0] != ref {
		t.Fatalf("inserted refs = %v", inserted)
	}

	// A redelivery is idempotent: the store row wins and Slack is not called.
	second, err := channel.RequestApproval(context.Background(), approval.ApprovalRequest{
		ProposalID: channelProposalID,
		CaseID:     channelCaseID,
		Channel:    approval.ChannelSlack,
		Tier:       "T1",
	})
	if err != nil || second != ref {
		t.Fatalf("second RequestApproval = %q, %v; want %q", second, err, ref)
	}
	if len(slack.posts) != 1 {
		t.Fatalf("Slack posts after redelivery = %d, want 1", len(slack.posts))
	}
}

func TestChannelRequestApprovalFailureDoesNotPersist(t *testing.T) {
	slack := newChannelSlackFake(t)
	slack.failPost = true
	store := newChannelStoreFake()
	channel := newTestChannel(t, &channelSourceFake{data: channelMessageData()}, store,
		&channelApproverFake{}, slack)

	if _, err := channel.RequestApproval(context.Background(), approval.ApprovalRequest{
		ProposalID: channelProposalID,
		Channel:    approval.ChannelSlack,
	}); err == nil {
		t.Fatal("RequestApproval succeeded despite a Slack error")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.rows) != 0 || len(store.inserted) != 0 {
		t.Fatalf("failed post persisted %d row(s)", len(store.rows))
	}
}

func TestChannelHandleCallbackActions(t *testing.T) {
	approvers := &channelApproverFake{approver: approval.Approver{ID: "approver-uuid"}}
	channel := newTestChannel(t, &channelSourceFake{}, newChannelStoreFake(),
		approvers, newChannelSlackFake(t))

	tests := []struct {
		action   string
		decision approval.Decision
		reason   string
	}{
		{action: ActionApprove, decision: approval.DecisionApproved},
		{action: ActionReject, decision: approval.DecisionRejected},
		{action: ActionRequestInfo, decision: approval.DecisionRejected, reason: "info_requested"},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			approvers.calls = 0
			payload := unitPayload(t, test.action, channelProposalID+":3", "U123", "C123", channelTS)
			request := httptest.NewRequest(http.MethodPost, "/v1/slack/interactions",
				strings.NewReader(url.Values{"payload": {payload}}.Encode()))
			request.Header.Set("Content-Type", InteractionFormContentType)
			// No signature headers: HandleCallback is not the crypto boundary.
			decision, err := channel.HandleCallback(context.Background(), request)
			if err != nil {
				t.Fatalf("HandleCallback: %v", err)
			}
			if decision.ProposalID != channelProposalID || decision.Decision != test.decision ||
				decision.ApproverID != "approver-uuid" || decision.Source != "slack" ||
				decision.Reason != test.reason {
				t.Fatalf("decision = %+v, want proposal=%s decision=%s approver=%s source=slack reason=%q",
					decision, channelProposalID, test.decision, "approver-uuid", test.reason)
			}
			if approvers.calls != 1 || approvers.gotUser != "U123" {
				t.Fatalf("approver calls = %d user=%q", approvers.calls, approvers.gotUser)
			}
		})
	}
}

func TestChannelHandleCallbackRejectsMalformed(t *testing.T) {
	approvers := &channelApproverFake{approver: approval.Approver{ID: "approver-uuid"}}
	channel := newTestChannel(t, &channelSourceFake{}, newChannelStoreFake(),
		approvers, newChannelSlackFake(t))
	validValue := channelProposalID + ":3"

	tests := []struct {
		name        string
		body        string
		contentType string
		method      string
	}{
		{
			name:        "missing payload",
			body:        "other=value",
			contentType: InteractionFormContentType,
		},
		{
			name:        "unknown form field",
			body:        url.Values{"payload": {unitPayload(t, ActionApprove, validValue, "U123", "C123", channelTS)}, "other": {"x"}}.Encode(),
			contentType: InteractionFormContentType,
		},
		{
			name:        "unknown JSON field",
			body:        url.Values{"payload": {strings.Replace(unitPayload(t, ActionApprove, validValue, "U123", "C123", channelTS), `"type":"block_actions",`, `"type":"block_actions","unexpected":"x",`, 1)}}.Encode(),
			contentType: InteractionFormContentType,
		},
		{
			name:        "two actions",
			body:        url.Values{"payload": {strings.Replace(unitPayload(t, ActionApprove, validValue, "U123", "C123", channelTS), `"actions":[{"action_id":"approval_approve","value":"`+validValue+`"}]`, `"actions":[{"action_id":"approval_approve","value":"`+validValue+`"},{"action_id":"approval_reject","value":"`+validValue+`"}]`, 1)}}.Encode(),
			contentType: InteractionFormContentType,
		},
		{
			name:        "bad value",
			body:        url.Values{"payload": {unitPayload(t, ActionApprove, channelProposalID, "U123", "C123", channelTS)}}.Encode(),
			contentType: InteractionFormContentType,
		},
		{
			name:        "missing user",
			body:        url.Values{"payload": {strings.Replace(unitPayload(t, ActionApprove, validValue, "U123", "C123", channelTS), `,"user":{"id":"U123"}`, "", 1)}}.Encode(),
			contentType: InteractionFormContentType,
		},
		{
			name:        "wrong content type",
			body:        url.Values{"payload": {unitPayload(t, ActionApprove, validValue, "U123", "C123", channelTS)}}.Encode(),
			contentType: "application/json",
		},
		{
			name:        "wrong method",
			body:        url.Values{"payload": {unitPayload(t, ActionApprove, validValue, "U123", "C123", channelTS)}}.Encode(),
			contentType: InteractionFormContentType,
			method:      http.MethodGet,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			method := test.method
			if method == "" {
				method = http.MethodPost
			}
			request := httptest.NewRequest(method, "/v1/slack/interactions", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			_, err := channel.HandleCallback(context.Background(), request)
			if !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("error = %v, want ErrInvalidPayload", err)
			}
			if !errors.Is(err, approval.ErrInvalidRequest) {
				t.Fatalf("error = %v, want approval.ErrInvalidRequest", err)
			}
		})
	}
	if approvers.calls != 0 {
		t.Fatalf("malformed callback resolved %d approver(s), want 0", approvers.calls)
	}
}

func TestChannelCancelUpdatesGenericWithdrawnMessage(t *testing.T) {
	slack := newChannelSlackFake(t)
	channel := newTestChannel(t, &channelSourceFake{}, newChannelStoreFake(),
		&channelApproverFake{}, slack)

	if err := channel.Cancel(context.Background(), channelID+":"+channelTS); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(slack.updates) != 1 {
		t.Fatalf("Slack updates = %d, want 1", len(slack.updates))
	}
	update := slack.updates[0]
	if update["channel"] != channelID || update["ts"] != channelTS {
		t.Fatalf("update target = %+v", update)
	}
	blocks, _ := update["blocks"].([]any)
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] == "actions" {
			t.Fatalf("withdrawn message still has actions: %+v", block)
		}
	}
	if text, _ := update["text"].(string); !strings.Contains(text, "withdrawn") {
		t.Fatalf("withdrawn text = %q", text)
	}
}

func TestChannelCancelRejectsInvalidRefAndPropagatesSlackFailure(t *testing.T) {
	slack := newChannelSlackFake(t)
	channel := newTestChannel(t, &channelSourceFake{}, newChannelStoreFake(),
		&channelApproverFake{}, slack)

	for _, ref := range []string{"", "no-colon", "a:b:c", ":1732", "C:", " C:1732"} {
		err := channel.Cancel(context.Background(), ref)
		if !errors.Is(err, ErrInvalidMessage) || !errors.Is(err, approval.ErrInvalidRequest) {
			t.Fatalf("Cancel(%q) error = %v, want ErrInvalidMessage + approval.ErrInvalidRequest", ref, err)
		}
	}
	if len(slack.updates) != 0 {
		t.Fatalf("invalid refs made %d Slack update(s)", len(slack.updates))
	}

	slack.failUpdate = true
	if err := channel.Cancel(context.Background(), channelID+":"+channelTS); err == nil {
		t.Fatal("Cancel ignored a Slack failure")
	}
}

func TestDBChannelStoreValidatesQueryer(t *testing.T) {
	if _, err := NewDBChannelStore(nil); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("NewDBChannelStore(nil) error = %v, want ErrInvalidMessage", err)
	}
}
