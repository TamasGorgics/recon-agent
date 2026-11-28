// covers: FIN-52
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

const integrityTestRunID = "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b"

func integrityTestPayload() []byte {
	return []byte(`{
		"run_id": "` + integrityTestRunID + `",
		"run_date": "2026-11-26",
		"status": "failed",
		"check_count": 5,
		"failure_count": 2,
		"failed_checks": ["value_conservation", "tieout"],
		"problem_counts": {"value_conservation": 1, "tieout": 2},
		"triggered_by": "schedule"
	}`)
}

func TestHandleIntegrityFailedPostsBoundedMessage(t *testing.T) {
	type recordedRequest struct {
		path string
		auth string
		body []byte
	}
	requests := make(chan recordedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- recordedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"ts":"1732041600.123456","channel":"C0123456789"}`)
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		BaseURL:  server.URL,
		BotToken: "xoxb-test-token",
		Clock:    clock.Real{},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	handlers := &Handlers{
		client:    client,
		channelID: "C0123456789",
		clock:     clock.Real{},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := handlers.HandleIntegrityFailed(context.Background(), outbox.Message{
		Topic:   topicIntegrityFail,
		Payload: integrityTestPayload(),
	}); err != nil {
		t.Fatalf("HandleIntegrityFailed: %v", err)
	}

	recorded := <-requests
	if recorded.path != "/chat.postMessage" {
		t.Fatalf("Slack path = %q, want /chat.postMessage", recorded.path)
	}
	if recorded.auth != "Bearer xoxb-test-token" {
		t.Fatalf("Slack Authorization = %q", recorded.auth)
	}
	var body struct {
		Channel     string            `json:"channel"`
		Text        string            `json:"text"`
		Blocks      []json.RawMessage `json:"blocks"`
		ClientMsgID string            `json:"client_msg_id"`
	}
	if err := json.Unmarshal(recorded.body, &body); err != nil {
		t.Fatalf("decode Slack request: %v", err)
	}
	if body.Channel != "C0123456789" {
		t.Fatalf("Slack channel = %q", body.Channel)
	}
	if body.ClientMsgID != integrityTestRunID {
		t.Fatalf("client_msg_id = %q, want the run id", body.ClientMsgID)
	}
	if len(body.Blocks) != 0 {
		t.Fatalf("integrity alert has %d Block Kit blocks; want plain text only", len(body.Blocks))
	}
	for _, want := range []string{
		integrityTestRunID,
		"run_date: 2026-11-26",
		"status: failed",
		"triggered_by: schedule",
		"checks: 5, failed: 2",
		"value_conservation (problems: 1)",
		"tieout (problems: 2)",
		"recon integrity run --date 2026-11-26 --json",
	} {
		if !strings.Contains(body.Text, want) {
			t.Fatalf("Slack text is missing %q:\n%s", want, body.Text)
		}
	}
	if strings.Contains(body.Text, "action") || strings.Contains(body.Text, "button") {
		t.Fatalf("integrity alert text implies interactive controls:\n%s", body.Text)
	}
	if len(body.Text) > 4000 {
		t.Fatalf("Slack text length = %d, want <= 4000", len(body.Text))
	}
}

func TestHandleIntegrityFailedRejectsMismatchedPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid payload reached the Slack network")
	}))
	defer server.Close()
	client, err := NewClient(ClientOptions{BaseURL: server.URL, BotToken: "xoxb-test-token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	handlers := &Handlers{client: client, channelID: "C0123456789", logger: slog.Default()}

	cases := []string{
		// failure_count disagrees with failed_checks
		`{"run_id":"` + integrityTestRunID + `","run_date":"2026-11-26","status":"failed","check_count":5,"failure_count":2,"failed_checks":["tieout"],"problem_counts":{"tieout":1},"triggered_by":"schedule"}`,
		// passed is not a failure alert
		`{"run_id":"` + integrityTestRunID + `","run_date":"2026-11-26","status":"passed","check_count":5,"failure_count":0,"failed_checks":[],"problem_counts":{},"triggered_by":"schedule"}`,
		// unknown trigger
		`{"run_id":"` + integrityTestRunID + `","run_date":"2026-11-26","status":"failed","check_count":5,"failure_count":1,"failed_checks":["tieout"],"problem_counts":{"tieout":1},"triggered_by":"api"}`,
		// unknown field is a contract drift
		`{"run_id":"` + integrityTestRunID + `","run_date":"2026-11-26","status":"failed","check_count":5,"failure_count":1,"failed_checks":["tieout"],"problem_counts":{"tieout":1},"triggered_by":"schedule","extra":true}`,
	}
	for _, payload := range cases {
		err := handlers.HandleIntegrityFailed(context.Background(), outbox.Message{
			Topic: topicIntegrityFail, Payload: []byte(payload),
		})
		if !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("payload %s error = %v, want ErrInvalidPayload", payload, err)
		}
	}
}

func TestHandlerMapRegistersIntegrityFailed(t *testing.T) {
	handlers := (&Handlers{}).Map()
	if len(handlers) != 4 {
		t.Fatalf("handler count = %d, want 4", len(handlers))
	}
	if handlers[topicIntegrityFail] == nil {
		t.Fatalf("handler map has no %q handler", topicIntegrityFail)
	}
	if topicIntegrityFail != "integrity.failed" {
		t.Fatalf("integrity topic = %q", topicIntegrityFail)
	}
}
