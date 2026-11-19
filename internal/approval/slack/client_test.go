package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval/slack"
)

const testBotToken = "xoxb-test-secret-token-do-not-leak"

type capturedRequest struct {
	Method        string
	Path          string
	Authorization string
	ContentType   string
	Body          []byte
}

type fakeSlack struct {
	mu       sync.Mutex
	attempts int
	requests []capturedRequest
	respond  func(attempt int, w http.ResponseWriter)
}

func (f *fakeSlack) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.attempts++
	attempt := f.attempts
	f.requests = append(f.requests, capturedRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		Authorization: r.Header.Get("Authorization"),
		ContentType:   r.Header.Get("Content-Type"),
		Body:          body,
	})
	f.mu.Unlock()
	f.respond(attempt, w)
}

func (f *fakeSlack) snapshot() ([]capturedRequest, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRequest(nil), f.requests...), f.attempts
}

func newFakeSlack(t *testing.T, respond func(attempt int, w http.ResponseWriter)) (*httptest.Server, *fakeSlack) {
	t.Helper()
	fake := &fakeSlack{respond: respond}
	server := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(server.Close)
	return server, fake
}

type recordingSleeper struct {
	mu        sync.Mutex
	durations []time.Duration
	err       error
}

func (s *recordingSleeper) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.durations = append(s.durations, d)
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (s *recordingSleeper) snapshot() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.durations...)
}

func newTestClient(t *testing.T, server *httptest.Server, sleeper slack.Sleeper, mutate func(*slack.ClientOptions)) *slack.Client {
	t.Helper()
	options := slack.ClientOptions{
		BaseURL:    server.URL,
		BotToken:   testBotToken,
		HTTPClient: server.Client(),
		Sleeper:    sleeper,
	}
	if mutate != nil {
		mutate(&options)
	}
	client, err := slack.NewClient(options)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func testMessage() slack.OutgoingMessage {
	return slack.OutgoingMessage{
		Text: "Approval required: case aaaaaaaa — write_off 1234.56 USD (T1)",
		Blocks: []slack.Block{
			{
				Type: "section",
				Text: &slack.TextObject{Type: "mrkdwn", Text: "*Case* aaaaaaaa"},
			},
		},
		ClientMsgID: "11111111-2222-3333-4444-555555555555",
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "client", name))
	if err != nil {
		t.Fatalf("read client fixture %s: %v", name, err)
	}
	return raw
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, status int, body []byte) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		t.Errorf("write fake Slack response: %v", err)
	}
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode actual JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode expected JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func assertNoToken(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Fatalf("error leaked the bot token: %v", err)
	}
	if strings.Contains(err.Error(), "Bearer "+testBotToken) {
		t.Fatalf("error leaked the authorization header: %v", err)
	}
}

func TestClientPostMessageRequestShapeAndTS(t *testing.T) {
	response := readTestdata(t, "post_message_ok.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusOK, response)
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)

	ts, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if ts != "1732041600.123456" {
		t.Fatalf("ts = %q", ts)
	}
	requests, attempts := fake.snapshot()
	if attempts != 1 || len(requests) != 1 {
		t.Fatalf("attempts = %d requests = %d, want 1", attempts, len(requests))
	}
	got := requests[0]
	if got.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", got.Method)
	}
	if got.Path != "/chat.postMessage" {
		t.Fatalf("path = %q, want /chat.postMessage", got.Path)
	}
	if got.Authorization != "Bearer "+testBotToken {
		t.Fatalf("authorization = %q", got.Authorization)
	}
	if got.ContentType != "application/json; charset=utf-8" {
		t.Fatalf("content type = %q", got.ContentType)
	}
	assertJSONEqual(t, got.Body, readTestdata(t, "post_message_request.json"))
}

func TestClientPostMessageRefUsesResponseChannel(t *testing.T) {
	response := readTestdata(t, "post_message_alt_channel_ok.json")
	server, _ := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusOK, response)
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)

	ref, err := client.PostMessageRef(context.Background(), "C0123456789", testMessage())
	if err != nil {
		t.Fatalf("PostMessageRef: %v", err)
	}
	if ref != "C9999999999:1732041600.123456" {
		t.Fatalf("ref = %q", ref)
	}
}

func TestClientUpdateMessageRequestShape(t *testing.T) {
	response := readTestdata(t, "update_message_ok.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusOK, response)
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)

	if err := client.UpdateMessage(context.Background(), "C0123456789", "1732041600.123456", testMessage()); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	requests, _ := fake.snapshot()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if requests[0].Path != "/chat.update" {
		t.Fatalf("path = %q, want /chat.update", requests[0].Path)
	}
	assertJSONEqual(t, requests[0].Body, readTestdata(t, "update_message_request.json"))
}

func TestClientPostEphemeralRequestShape(t *testing.T) {
	response := readTestdata(t, "ephemeral_ok.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusOK, response)
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)

	if err := client.PostEphemeral(context.Background(), "C0123456789", "U0123456789",
		"You are not authorized to approve this proposal."); err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	requests, _ := fake.snapshot()
	if len(requests) != 1 || requests[0].Path != "/chat.postEphemeral" {
		t.Fatalf("requests = %#v", requests)
	}
	assertJSONEqual(t, requests[0].Body, readTestdata(t, "ephemeral_request.json"))
}

func TestClientSlackApplicationError(t *testing.T) {
	response := readTestdata(t, "application_error.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusOK, response)
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	assertNoToken(t, err)
	var apiErr slack.ErrSlackAPI
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want ErrSlackAPI", err)
	}
	if apiErr.Code != "invalid_auth" {
		t.Fatalf("code = %q, want invalid_auth", apiErr.Code)
	}
	if !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("error does not include the Slack code: %v", err)
	}
	if _, attempts := fake.snapshot(); attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestClientRateLimitRetriesThenSucceeds(t *testing.T) {
	limited := readTestdata(t, "rate_limited.json")
	ok := readTestdata(t, "post_message_ok.json")
	server, fake := newFakeSlack(t, func(attempt int, w http.ResponseWriter) {
		if attempt == 1 {
			w.Header().Set("Retry-After", "1")
			writeJSONResponse(t, w, http.StatusTooManyRequests, limited)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, ok)
	})
	sleeper := &recordingSleeper{}
	client := newTestClient(t, server, sleeper, nil)

	ts, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if ts != "1732041600.123456" {
		t.Fatalf("ts = %q", ts)
	}
	if _, attempts := fake.snapshot(); attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if got := sleeper.snapshot(); len(got) != 1 || got[0] != time.Second {
		t.Fatalf("sleeps = %v, want [1s]", got)
	}
}

func TestClientRateLimitRetryAfterFallbacks(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"invalid header", "soon"},
		{"zero header", "0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limited := readTestdata(t, "rate_limited.json")
			ok := readTestdata(t, "post_message_ok.json")
			server, _ := newFakeSlack(t, func(attempt int, w http.ResponseWriter) {
				if attempt == 1 {
					if test.header != "" {
						w.Header().Set("Retry-After", test.header)
					}
					writeJSONResponse(t, w, http.StatusTooManyRequests, limited)
					return
				}
				writeJSONResponse(t, w, http.StatusOK, ok)
			})
			sleeper := &recordingSleeper{}
			client := newTestClient(t, server, sleeper, nil)
			if _, err := client.PostMessage(context.Background(), "C0123456789", testMessage()); err != nil {
				t.Fatalf("PostMessage: %v", err)
			}
			if got := sleeper.snapshot(); len(got) != 1 || got[0] != time.Second {
				t.Fatalf("sleeps = %v, want [1s]", got)
			}
		})
	}
}

func TestClientRateLimitExhausted(t *testing.T) {
	limited := readTestdata(t, "rate_limited.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "2")
		writeJSONResponse(t, w, http.StatusTooManyRequests, limited)
	})
	sleeper := &recordingSleeper{}
	client := newTestClient(t, server, sleeper, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	assertNoToken(t, err)
	if !errors.Is(err, slack.ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	var limitedErr *slack.RateLimitedError
	if !errors.As(err, &limitedErr) {
		t.Fatalf("error = %v, want *RateLimitedError", err)
	}
	if limitedErr.RetryAfter != 2*time.Second || limitedErr.Attempts != 3 {
		t.Fatalf("rate limit error = %#v", limitedErr)
	}
	if _, attempts := fake.snapshot(); attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	wantSleeps := []time.Duration{2 * time.Second, 2 * time.Second}
	if got := sleeper.snapshot(); !reflect.DeepEqual(got, wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", got, wantSleeps)
	}
}

func TestClientRateLimitBudgetExceeded(t *testing.T) {
	limited := readTestdata(t, "rate_limited.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "11")
		writeJSONResponse(t, w, http.StatusTooManyRequests, limited)
	})
	sleeper := &recordingSleeper{}
	client := newTestClient(t, server, sleeper, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	assertNoToken(t, err)
	if !errors.Is(err, slack.ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	var limitedErr *slack.RateLimitedError
	if !errors.As(err, &limitedErr) || limitedErr.RetryAfter != 11*time.Second {
		t.Fatalf("rate limit error = %#v", err)
	}
	if _, attempts := fake.snapshot(); attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if got := sleeper.snapshot(); len(got) != 0 {
		t.Fatalf("sleeps = %v, want none", got)
	}
}

func TestClientRateLimitHugeRetryAfterIsClamped(t *testing.T) {
	limited := readTestdata(t, "rate_limited.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "9223372036854775807")
		writeJSONResponse(t, w, http.StatusTooManyRequests, limited)
	})
	sleeper := &recordingSleeper{}
	client := newTestClient(t, server, sleeper, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	if !errors.Is(err, slack.ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	var limitedErr *slack.RateLimitedError
	if !errors.As(err, &limitedErr) || limitedErr.RetryAfter != 24*time.Hour {
		t.Fatalf("rate limit error = %#v", err)
	}
	if _, attempts := fake.snapshot(); attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if got := sleeper.snapshot(); len(got) != 0 {
		t.Fatalf("sleeps = %v, want none", got)
	}
}

func TestClientRateLimitCumulativeBudget(t *testing.T) {
	limited := readTestdata(t, "rate_limited.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "6")
		writeJSONResponse(t, w, http.StatusTooManyRequests, limited)
	})
	sleeper := &recordingSleeper{}
	client := newTestClient(t, server, sleeper, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	assertNoToken(t, err)
	if !errors.Is(err, slack.ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	if _, attempts := fake.snapshot(); attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if got := sleeper.snapshot(); !reflect.DeepEqual(got, []time.Duration{6 * time.Second}) {
		t.Fatalf("sleeps = %v, want [6s]", got)
	}
}

func TestClientMaxAttemptsConfigurable(t *testing.T) {
	limited := readTestdata(t, "rate_limited.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "1")
		writeJSONResponse(t, w, http.StatusTooManyRequests, limited)
	})
	sleeper := &recordingSleeper{}
	client := newTestClient(t, server, sleeper, func(options *slack.ClientOptions) {
		options.MaxAttempts = 1
	})

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	if !errors.Is(err, slack.ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	if _, attempts := fake.snapshot(); attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if got := sleeper.snapshot(); len(got) != 0 {
		t.Fatalf("sleeps = %v, want none", got)
	}
}

func TestClientServerErrorRetriesThenSucceeds(t *testing.T) {
	failure := readTestdata(t, "server_error.json")
	ok := readTestdata(t, "post_message_ok.json")
	server, fake := newFakeSlack(t, func(attempt int, w http.ResponseWriter) {
		if attempt == 1 {
			writeJSONResponse(t, w, http.StatusServiceUnavailable, failure)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, ok)
	})
	sleeper := &recordingSleeper{}
	client := newTestClient(t, server, sleeper, nil)

	if _, err := client.PostMessage(context.Background(), "C0123456789", testMessage()); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if _, attempts := fake.snapshot(); attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if got := sleeper.snapshot(); !reflect.DeepEqual(got, []time.Duration{time.Second}) {
		t.Fatalf("sleeps = %v, want [1s]", got)
	}
}

func TestClientServerErrorExhausted(t *testing.T) {
	failure := readTestdata(t, "server_error.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusInternalServerError, failure)
	})
	sleeper := &recordingSleeper{}
	client := newTestClient(t, server, sleeper, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	assertNoToken(t, err)
	if !errors.Is(err, slack.ErrSlackUnavailable) {
		t.Fatalf("error = %v, want ErrSlackUnavailable", err)
	}
	var unavailable *slack.UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Status != 500 || unavailable.Attempts != 3 {
		t.Fatalf("unavailable error = %#v", err)
	}
	if _, attempts := fake.snapshot(); attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if got := sleeper.snapshot(); !reflect.DeepEqual(got, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("sleeps = %v, want [1s 2s]", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientTransportErrorExhausted(t *testing.T) {
	sleeper := &recordingSleeper{}
	client, err := slack.NewClient(slack.ClientOptions{
		BaseURL:     "https://slack.example.test/api",
		BotToken:    testBotToken,
		Sleeper:     sleeper,
		MaxAttempts: 2,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset by peer")
		})},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.PostMessage(context.Background(), "C0123456789", testMessage())
	assertNoToken(t, err)
	if !errors.Is(err, slack.ErrSlackUnavailable) {
		t.Fatalf("error = %v, want ErrSlackUnavailable", err)
	}
	if got := sleeper.snapshot(); !reflect.DeepEqual(got, []time.Duration{time.Second}) {
		t.Fatalf("sleeps = %v, want [1s]", got)
	}
}

func TestClientDoesNotRetryOther4xx(t *testing.T) {
	response := readTestdata(t, "application_error.json")
	server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusForbidden, response)
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	assertNoToken(t, err)
	var httpErr *slack.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want *HTTPError", err)
	}
	if httpErr.Status != http.StatusForbidden || httpErr.Code != "invalid_auth" {
		t.Fatalf("HTTP error = %#v", httpErr)
	}
	if _, attempts := fake.snapshot(); attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestClientMalformedSuccessResponse(t *testing.T) {
	server, _ := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusOK, []byte(`{"ok":`))
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	if !errors.Is(err, slack.ErrSlackResponse) {
		t.Fatalf("error = %v, want ErrSlackResponse", err)
	}
}

func TestClientSuccessWithoutTS(t *testing.T) {
	server, _ := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusOK, []byte(`{"ok":true,"channel":"C0123456789"}`))
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)

	_, err := client.PostMessage(context.Background(), "C0123456789", testMessage())
	if !errors.Is(err, slack.ErrSlackResponse) {
		t.Fatalf("error = %v, want ErrSlackResponse", err)
	}
}

func TestClientContextCancellation(t *testing.T) {
	t.Run("before request", func(t *testing.T) {
		server, fake := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
			t.Error("server must not be called for a canceled context")
			writeJSONResponse(t, w, http.StatusOK, readTestdata(t, "post_message_ok.json"))
		})
		client := newTestClient(t, server, &recordingSleeper{}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.PostMessage(ctx, "C0123456789", testMessage())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if _, attempts := fake.snapshot(); attempts != 0 {
			t.Fatalf("attempts = %d, want 0", attempts)
		}
	})

	t.Run("during retry sleep", func(t *testing.T) {
		limited := readTestdata(t, "rate_limited.json")
		server, _ := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			writeJSONResponse(t, w, http.StatusTooManyRequests, limited)
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sleeper := &cancelingSleeper{cancel: cancel}
		client := newTestClient(t, server, sleeper, nil)
		_, err := client.PostMessage(ctx, "C0123456789", testMessage())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

type cancelingSleeper struct {
	cancel context.CancelFunc
}

func (s cancelingSleeper) Sleep(ctx context.Context, _ time.Duration) error {
	s.cancel()
	return ctx.Err()
}

func TestClientRejectsInvalidConstruction(t *testing.T) {
	tests := []struct {
		name    string
		options slack.ClientOptions
	}{
		{"empty token", slack.ClientOptions{BaseURL: "https://slack.example.test/api"}},
		{"whitespace token", slack.ClientOptions{BaseURL: "https://slack.example.test/api", BotToken: " token "}},
		{"relative base URL", slack.ClientOptions{BaseURL: "/api", BotToken: testBotToken}},
		{"non-http base URL", slack.ClientOptions{BaseURL: "ftp://slack.example.test/api", BotToken: testBotToken}},
		{"base URL with query", slack.ClientOptions{BaseURL: "https://slack.example.test/api?x=1", BotToken: testBotToken}},
		{"negative attempts", slack.ClientOptions{BaseURL: "https://slack.example.test/api", BotToken: testBotToken, MaxAttempts: -1}},
		{"negative wait", slack.ClientOptions{BaseURL: "https://slack.example.test/api", BotToken: testBotToken, MaxRetryWait: -time.Second}},
		{"negative timeout", slack.ClientOptions{BaseURL: "https://slack.example.test/api", BotToken: testBotToken, Timeout: -time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := slack.NewClient(test.options); !errors.Is(err, slack.ErrInvalidClient) {
				t.Fatalf("NewClient error = %v, want ErrInvalidClient", err)
			}
		})
	}
}

func TestClientRejectsInvalidRequests(t *testing.T) {
	server, _ := newFakeSlack(t, func(_ int, w http.ResponseWriter) {
		writeJSONResponse(t, w, http.StatusOK, readTestdata(t, "post_message_ok.json"))
	})
	client := newTestClient(t, server, &recordingSleeper{}, nil)
	ctx := context.Background()

	badMessage := testMessage()
	badMessage.ClientMsgID = "not-a-uuid"

	tests := []struct {
		name string
		call func() error
	}{
		{"empty channel", func() error { _, err := client.PostMessage(ctx, "", testMessage()); return err }},
		{"channel with colon", func() error { _, err := client.PostMessage(ctx, "C:1", testMessage()); return err }},
		{"channel whitespace", func() error { _, err := client.PostMessage(ctx, " C1 ", testMessage()); return err }},
		{"empty text and blocks", func() error {
			_, err := client.PostMessage(ctx, "C1", slack.OutgoingMessage{})
			return err
		}},
		{"bad client_msg_id", func() error { _, err := client.PostMessage(ctx, "C1", badMessage); return err }},
		{"empty update ts", func() error { return client.UpdateMessage(ctx, "C1", "", testMessage()) }},
		{"empty ephemeral user", func() error { return client.PostEphemeral(ctx, "C1", "", "text") }},
		{"empty ephemeral text", func() error { return client.PostEphemeral(ctx, "C1", "U1", "  ") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, slack.ErrInvalidClient) {
				t.Fatalf("error = %v, want ErrInvalidClient", err)
			}
		})
	}
}

func TestClientDefaults(t *testing.T) {
	if slack.DefaultMaxAttempts != 3 {
		t.Fatalf("DefaultMaxAttempts = %d, want 3", slack.DefaultMaxAttempts)
	}
	if slack.DefaultMaxRetryWait != 10*time.Second {
		t.Fatalf("DefaultMaxRetryWait = %s, want 10s", slack.DefaultMaxRetryWait)
	}
	if slack.DefaultBaseURL != "https://slack.com/api" {
		t.Fatalf("DefaultBaseURL = %q", slack.DefaultBaseURL)
	}
}
