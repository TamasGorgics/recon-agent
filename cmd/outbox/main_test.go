package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/slack"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

const (
	testAppDSN       = "postgres://recon_app:test-app-secret@127.0.0.1:1/recon?sslmode=disable"
	testAppSecret    = "test-app-secret"
	testBotToken     = "xoxb-test-bot-token-0123456789"
	testChannelID    = "C0123456789"
	testCaseFileBase = "https://cases.example.test"
	testProposalID   = "11111111-2222-3333-4444-555555555555"
)

// fakePool is enough of a pgx pool and Slack queryer to construct the worker.
// BeginTx returns an empty transaction so --once exercises the real claim path
// without a database. No statement reaches QueryRow/Exec during construction.
type fakePool struct {
	beginTx atomic.Int64
}

func (*fakePool) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("fake pool: Begin is not used")
}

func (p *fakePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	p.beginTx.Add(1)
	return emptyTx{}, nil
}

func (*fakePool) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeRow{}
}

func (*fakePool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

type fakeRow struct{}

func (fakeRow) Scan(...any) error { return pgx.ErrNoRows }

// emptyTx is a pgx.Tx with only the claim-path methods implemented: the
// worker queries, reads no rows, and commits.
type emptyTx struct{ pgx.Tx }

func (emptyTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return emptyRows{}, nil
}

func (emptyTx) Commit(context.Context) error { return nil }

func (emptyTx) Rollback(context.Context) error { return nil }

type emptyRows struct{ pgx.Rows }

func (emptyRows) Close()     {}
func (emptyRows) Err() error { return nil }
func (emptyRows) Next() bool { return false }

func clearCmdEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		config.EnvConfigFile,
		config.EnvDatabaseURL,
		config.EnvDatabaseTestURL,
		config.EnvDatabaseAppURL,
		config.EnvDatabaseAgentURL,
		config.EnvLogLevel,
		config.EnvOpenCodeAPIKey,
		config.EnvOpenCodeBaseURL,
		config.EnvOpenCodeModel,
		config.EnvAPIKeys,
		config.EnvPolicyFile,
		config.EnvSlackBotToken,
		config.EnvSlackSigningSecret,
		config.EnvSlackChannelID,
		config.EnvSlackCaseFileBaseURL,
		config.EnvMetricsWorkerAddr,
		config.EnvMetricsOutboxAddr,
	} {
		t.Setenv(name, "")
	}
}

func setOutboxEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvDatabaseAppURL, testAppDSN)
	t.Setenv(config.EnvSlackBotToken, testBotToken)
	t.Setenv(config.EnvSlackChannelID, testChannelID)
	t.Setenv(config.EnvSlackCaseFileBaseURL, testCaseFileBase)
	t.Setenv(config.EnvMetricsOutboxAddr, "127.0.0.1:0")
}

func TestRunHelpNeedsNoConfiguration(t *testing.T) {
	clearCmdEnv(t)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out); err != nil {
		t.Fatalf("run --help: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"usage: outbox",
		"--once",
		"approval.request",
		"approval.update",
		"approval.cancel",
		"No agent DSN or LLM key is required",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("usage is missing %q:\n%s", want, text)
		}
	}
}

func TestRunRejectsUnexpectedArgumentsBeforeConfiguration(t *testing.T) {
	clearCmdEnv(t)
	if err := run(context.Background(), []string{"extra"}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("run extra argument = %v, want unexpected argument", err)
	}
}

func TestRunConfigValidationErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T)
		want   string
	}{
		{
			name:   "missing app url",
			mutate: func(t *testing.T) { t.Setenv(config.EnvDatabaseAppURL, "") },
			want:   "database.app_url",
		},
		{
			name:   "missing bot token",
			mutate: func(t *testing.T) { t.Setenv(config.EnvSlackBotToken, "") },
			want:   "slack.bot_token",
		},
		{
			name:   "missing channel id",
			mutate: func(t *testing.T) { t.Setenv(config.EnvSlackChannelID, "") },
			want:   "slack.channel_id",
		},
		{
			name:   "missing case file base url",
			mutate: func(t *testing.T) { t.Setenv(config.EnvSlackCaseFileBaseURL, "") },
			want:   "slack.case_file_base_url",
		},
		{
			name:   "invalid case file base url",
			mutate: func(t *testing.T) { t.Setenv(config.EnvSlackCaseFileBaseURL, "not-a-url") },
			want:   "slack.case_file_base_url",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearCmdEnv(t)
			setOutboxEnv(t)
			test.mutate(t)

			err := run(context.Background(), nil, io.Discard)
			if err == nil {
				t.Fatal("run accepted an incomplete outbox configuration")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want it to mention %q", err, test.want)
			}
			for _, secret := range []string{testBotToken, testAppSecret, "postgres://"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("startup error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestRunStartupErrorRedactsDSNAndToken(t *testing.T) {
	clearCmdEnv(t)
	setOutboxEnv(t)
	const dsnSecret = "startup-secret-password-fragment"
	t.Setenv(config.EnvDatabaseAppURL,
		"postgres://recon_app:"+dsnSecret+"@localhost:notaport/recon")

	var out bytes.Buffer
	err := run(context.Background(), nil, &out)
	if err == nil {
		t.Fatal("run accepted a malformed database DSN")
	}
	for _, secret := range []string{dsnSecret, testBotToken, "postgres://", "password="} {
		if strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) {
			t.Fatalf("startup output leaked %q: err=%v out=%s", secret, err, out.String())
		}
	}
}

func TestBuildWorkerWiresThreeTopicsAndDefaults(t *testing.T) {
	clearCmdEnv(t)
	setOutboxEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Database.AgentURL != "" || cfg.LLM.APIKey != "" {
		t.Fatal("outbox fixture unexpectedly needs agent or LLM configuration")
	}

	type recordedRequest struct {
		path string
		auth string
		body []byte
	}
	requests := make(chan recordedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- recordedRequest{
			path: r.URL.Path,
			auth: r.Header.Get("Authorization"),
			body: body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"ts":"1732041600.123456","channel":"C0123456789"}`)
	}))
	defer server.Close()

	pool := &fakePool{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime, err := buildWorker(buildOptions{
		Config:       cfg,
		Pool:         pool,
		HTTPClient:   server.Client(),
		SlackBaseURL: server.URL,
		Clock:        clock.Real{},
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("buildWorker: %v", err)
	}
	if runtime.worker == nil || runtime.client == nil {
		t.Fatal("buildWorker returned a nil worker or Slack client")
	}

	wantTopics := []string{
		"approval.cancel",
		approval.TopicApprovalRequest,
		approval.TopicApprovalUpdate,
	}
	gotTopics := runtime.topics()
	if !slices.Equal(gotTopics, wantTopics) {
		t.Fatalf("topic list = %v, want %v", gotTopics, wantTopics)
	}
	if len(runtime.handlers) != len(wantTopics) {
		t.Fatalf("handler count = %d, want %d", len(runtime.handlers), len(wantTopics))
	}
	for _, topic := range wantTopics {
		if runtime.handlers[topic] == nil {
			t.Fatalf("handler for %q is nil", topic)
		}
	}

	if runtime.options.Pool != pool {
		t.Fatal("worker pool is not the recon_app pool passed to buildWorker")
	}
	if runtime.options.BatchSize != outbox.DefaultBatchSize ||
		runtime.options.Lease != outbox.DefaultLease ||
		runtime.options.HandlerTimeout != outbox.DefaultHandlerTimeout ||
		runtime.options.PollInterval != outbox.DefaultPollInterval ||
		runtime.options.MaxAttempts != outbox.DefaultMaxAttempts {
		t.Fatalf("worker options are not the outbox defaults: %+v", runtime.options)
	}
	if runtime.options.BatchSize <= 0 || runtime.options.Lease <= 0 ||
		runtime.options.HandlerTimeout <= 0 {
		t.Fatalf("worker bounds must be positive: %+v", runtime.options)
	}

	// Exercise the constructed client through the fake server: this proves the
	// bot token, base URL, and injected HTTP client are wired, with no real
	// Slack network access.
	ts, err := runtime.client.PostMessage(context.Background(), testChannelID, slack.OutgoingMessage{
		Text:        "wiring test",
		ClientMsgID: testProposalID,
	})
	if err != nil {
		t.Fatalf("PostMessage through fake Slack server: %v", err)
	}
	if ts != "1732041600.123456" {
		t.Fatalf("PostMessage ts = %q", ts)
	}
	recorded := <-requests
	if recorded.path != "/chat.postMessage" {
		t.Fatalf("Slack path = %q, want /chat.postMessage", recorded.path)
	}
	if recorded.auth != "Bearer "+testBotToken {
		t.Fatalf("Slack Authorization = %q", recorded.auth)
	}
	var body struct {
		Channel     string `json:"channel"`
		Text        string `json:"text"`
		ClientMsgID string `json:"client_msg_id"`
	}
	if err := json.Unmarshal(recorded.body, &body); err != nil {
		t.Fatalf("decode Slack request: %v", err)
	}
	if body.Channel != testChannelID || body.Text != "wiring test" || body.ClientMsgID != testProposalID {
		t.Fatalf("Slack request body = %+v", body)
	}
}

func TestBuildWorkerRejectsIncompleteConfigAndNilPool(t *testing.T) {
	if _, err := buildWorker(buildOptions{}); err == nil ||
		!strings.Contains(err.Error(), "database.app_url") {
		t.Fatalf("buildWorker empty options = %v, want database.app_url", err)
	}

	clearCmdEnv(t)
	setOutboxEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := buildWorker(buildOptions{Config: cfg}); err == nil ||
		!strings.Contains(err.Error(), "recon_app pool") {
		t.Fatalf("buildWorker nil pool = %v, want recon_app pool error", err)
	}
}

func TestRunOnceEmptyOutboxNeedsNoAgentConfig(t *testing.T) {
	clearCmdEnv(t)
	setOutboxEnv(t)
	t.Setenv(config.EnvDatabaseAgentURL, "")
	t.Setenv(config.EnvOpenCodeAPIKey, "")

	pool := &fakePool{}
	closed := false
	deps := dependencies{
		openApp: func(_ context.Context, dsn string) (appPool, func(), error) {
			if dsn != testAppDSN {
				t.Fatalf("openApp dsn = %q", dsn)
			}
			return pool, func() { closed = true }, nil
		},
		httpClient:   http.DefaultClient,
		slackBaseURL: "https://slack.example.test/api",
		clock:        clock.Real{},
	}

	var out bytes.Buffer
	if err := runWith(context.Background(), []string{"--once"}, &out, deps); err != nil {
		t.Fatalf("run --once: %v", err)
	}
	if pool.beginTx.Load() != 1 {
		t.Fatalf("claim transactions = %d, want 1", pool.beginTx.Load())
	}
	if !closed {
		t.Fatal("run did not close the recon_app pool")
	}

	text := out.String()
	for _, want := range []string{
		"outbox worker starting",
		"outbox worker stopped",
		`"processed":0`,
		approval.TopicApprovalRequest,
		approval.TopicApprovalUpdate,
		"approval.cancel",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("run log is missing %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{testBotToken, testAppSecret, testAppDSN} {
		if strings.Contains(text, secret) {
			t.Fatalf("run log leaked %q:\n%s", secret, text)
		}
	}
}

func TestRunOnceDoesNotBindMetricsListener(t *testing.T) {
	clearCmdEnv(t)
	setOutboxEnv(t)
	// Hold the configured port so a --once run that tried to bind would fail.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()
	t.Setenv(config.EnvMetricsOutboxAddr, occupied.Addr().String())

	pool := &fakePool{}
	deps := dependencies{
		openApp: func(context.Context, string) (appPool, func(), error) {
			return pool, func() {}, nil
		},
		httpClient:   http.DefaultClient,
		slackBaseURL: "https://slack.example.test/api",
		clock:        clock.Real{},
	}
	if err := runWith(context.Background(), []string{"--once"}, io.Discard, deps); err != nil {
		t.Fatalf("run --once with an unbindable metrics address: %v", err)
	}
	if pool.beginTx.Load() != 1 {
		t.Fatalf("claim transactions = %d, want 1", pool.beginTx.Load())
	}
}

func TestRunStopsCleanlyOnContextCancellation(t *testing.T) {
	clearCmdEnv(t)
	setOutboxEnv(t)

	pool := &fakePool{}
	closed := false
	deps := dependencies{
		openApp: func(context.Context, string) (appPool, func(), error) {
			return pool, func() { closed = true }, nil
		},
		httpClient:   http.DefaultClient,
		slackBaseURL: "https://slack.example.test/api",
		clock:        clock.Real{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	if err := runWith(ctx, nil, &out, deps); err != nil {
		t.Fatalf("run after context cancellation: %v", err)
	}
	if !closed {
		t.Fatal("run did not close the recon_app pool")
	}
	if !strings.Contains(out.String(), "outbox worker stopped") {
		t.Fatalf("run did not log shutdown:\n%s", out.String())
	}
}
