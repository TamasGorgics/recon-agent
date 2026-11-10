package runner

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

func ptrFloat(value float64) *float64 { //floatlint:allow provider temperature, not money
	return &value
}

func TestValidateRecordGuards(t *testing.T) {
	const secret = "oc_sk_record_secret_value"
	t.Setenv("OPENCODE_API_KEY", "")
	base := func() Options {
		opts := Options{}
		ApplyDefaults(&opts)
		opts.LLM = LLMRecord
		opts.MaxCostUSD = decimal.RequireFromString("0.25")
		return opts
	}

	if err := Validate(base()); !IsUsage(err) || !strings.Contains(err.Error(), "OPENCODE_API_KEY") {
		t.Fatalf("record without key = %v, want usage naming OPENCODE_API_KEY", err)
	}
	if strings.Contains(errString(Validate(base())), secret) {
		t.Fatal("record key error leaked the key value")
	}

	t.Setenv("OPENCODE_API_KEY", secret)
	for name, mutate := range map[string]func(*Options){
		"zero cap":     func(opts *Options) { opts.MaxCostUSD = decimal.Zero },
		"negative cap": func(opts *Options) { opts.MaxCostUSD = decimal.RequireFromString("-0.01") },
	} {
		t.Run(name, func(t *testing.T) {
			opts := base()
			mutate(&opts)
			err := Validate(opts)
			if !IsUsage(err) || !strings.Contains(err.Error(), "max-cost-usd") {
				t.Fatalf("Validate = %v, want cap usage error", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("cap validation error leaked the key value")
			}
		})
	}

	opts := base()
	if err := Validate(opts); err != nil {
		t.Fatalf("record with key and cap: %v", err)
	}

	opts = base()
	opts.UpdateCassettes = true
	if err := Validate(opts); err != nil {
		t.Fatalf("record with --update-cassettes: %v", err)
	}

	for _, mode := range []string{LLMLive, LLMCassette} {
		opts = base()
		opts.LLM = mode
		opts.UpdateCassettes = true
		if mode == LLMLive {
			opts.MaxCostUSD = decimal.RequireFromString("1")
		}
		if err := Validate(opts); !IsUsage(err) || !strings.Contains(err.Error(), "--update-cassettes") {
			t.Fatalf("--update-cassettes with %s = %v, want usage error", mode, err)
		}
	}

	opts = base()
	opts.LLM = LLMLive
	opts.MaxCostUSD = decimal.RequireFromString("1")
	opts.CassetteDirSet = true
	opts.CassetteDir = t.TempDir()
	if err := Validate(opts); !IsUsage(err) || !strings.Contains(err.Error(), "--cassette-dir") {
		t.Fatalf("--cassette-dir with live = %v, want usage error", err)
	}

	opts = base()
	opts.CassetteDirSet = true
	opts.CassetteDir = t.TempDir()
	if err := Validate(opts); err != nil {
		t.Fatalf("--cassette-dir with record: %v", err)
	}

	opts = Options{}
	ApplyDefaults(&opts)
	opts.LLM = LLMCassette
	opts.UpdateCassettes = true
	if err := Validate(opts); !IsUsage(err) {
		t.Fatalf("cassette overwrite = %v, want usage error", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestParseOptionsRecordFlags(t *testing.T) {
	opts, err := ParseOptions([]string{
		"--llm", "record",
		"--max-cost-usd", "0.25",
		"--cassette-dir", "/tmp/evidence",
		"--update-cassettes",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	ApplyDefaults(&opts)
	if opts.LLM != LLMRecord || !opts.UpdateCassettes || !opts.CassetteDirSet {
		t.Fatalf("options = %+v", opts)
	}
	if opts.CassetteDir != "/tmp/evidence" {
		t.Fatalf("cassette dir = %q", opts.CassetteDir)
	}
}

func TestApplyResumeOptionsRestoresRecordProvenance(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "test-key-not-used")
	opts := Options{Resume: "11111111-1111-4111-8111-111111111111"}
	ApplyDefaults(&opts)
	stored := resumeFixtureRow()
	stored.LLM = LLMRecord
	stored.CassetteDir = ".team/runs/x/cassettes/A-v2"
	stored.CassetteKnown = true

	if err := applyResumeOptions(&opts, stored); err != nil {
		t.Fatalf("applyResumeOptions: %v", err)
	}
	if opts.LLM != LLMRecord || opts.CassetteDir != stored.CassetteDir {
		t.Fatalf("restored llm/dir = %q/%q", opts.LLM, opts.CassetteDir)
	}

	conflict := Options{
		Resume:     "11111111-1111-4111-8111-111111111111",
		LLMSet:     true,
		LLM:        LLMCassette,
		Repeat:     1,
		Suite:      "agent",
		Split:      dataset.Dev,
		Seed:       ptrInt64(1),
		Model:      config.DefaultLLMModel,
		Prompt:     config.DefaultAgentPromptVersion,
		MaxCostUSD: decimal.RequireFromString("1"),
	}
	if err := applyResumeOptions(&conflict, stored); !IsUsage(err) ||
		!strings.Contains(err.Error(), "conflicts with stored run llm") {
		t.Fatalf("llm conflict = %v, want usage conflict", err)
	}

	dirConflict := Options{
		Resume:         "11111111-1111-4111-8111-111111111111",
		CassetteDirSet: true,
		CassetteDir:    "/tmp/other",
		Repeat:         1,
		Suite:          "agent",
		Split:          dataset.Dev,
		Seed:           ptrInt64(1),
		Model:          config.DefaultLLMModel,
		Prompt:         config.DefaultAgentPromptVersion,
		MaxCostUSD:     decimal.RequireFromString("1"),
	}
	if err := applyResumeOptions(&dirConflict, stored); !IsUsage(err) ||
		!strings.Contains(err.Error(), "--cassette-dir") {
		t.Fatalf("cassette dir conflict = %v, want usage conflict", err)
	}
}

func TestRunConfigStoresRecordModeAndCassetteDir(t *testing.T) {
	opts := Options{
		Suite:       "agent",
		Split:       dataset.Dev,
		Repeat:      1,
		Concurrency: 4,
		LLM:         LLMRecord,
		MaxCostUSD:  decimal.RequireFromString("0.25"),
		CassetteDir: "eval/cassette-evidence/m3-08/A-v2",
	}
	raw, err := buildRunConfig(opts, 0)
	if err != nil {
		t.Fatalf("buildRunConfig: %v", err)
	}
	llmMode, _, known, _, err := parseStoredRunSettings(raw)
	if err != nil || !known || llmMode != LLMRecord {
		t.Fatalf("stored llm = %q known=%t err=%v", llmMode, known, err)
	}
	dir, dirKnown, err := parseStoredCassetteDir(raw)
	if err != nil || !dirKnown || dir != opts.CassetteDir {
		t.Fatalf("stored cassette dir = %q known=%t err=%v", dir, dirKnown, err)
	}
}

func TestResolveCassetteDirPrecedence(t *testing.T) {
	opts := Options{CassetteDir: "flag-default", CassetteDirSet: true}
	if got := resolveCassetteDir(opts, Deps{CassetteDir: "deps"}); got != "flag-default" {
		t.Fatalf("explicit flag = %q", got)
	}
	opts = Options{CassetteDir: "flag-default"}
	if got := resolveCassetteDir(opts, Deps{CassetteDir: "deps"}); got != "deps" {
		t.Fatalf("deps fallback = %q", got)
	}
	if got := resolveCassetteDir(opts, Deps{}); got != "flag-default" {
		t.Fatalf("option fallback = %q", got)
	}
	if got := resolveCassetteDir(Options{}, Deps{}); got != llm.DefaultCassetteDir {
		t.Fatalf("default = %q", got)
	}
}

// recordFixture builds a fake OpenAI-compatible server, an adapter against it,
// a config whose base URL matches, and a request for model B.
func recordFixture(t *testing.T, responseBody string) (*llm.OpenCode, config.LLMConfig, llm.Request, *capturedRecordRequest) {
	t.Helper()
	captured := &capturedRecordRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.set(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req-record")
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(server.Close)

	cfg := config.DefaultLLMConfig()
	cfg.Provider = "opencode"
	cfg.BaseURL = server.URL
	cfg.Model = "glm-5.3-flash"
	adapter, err := llm.NewOpenCode(llm.OpenCodeOptions{
		BaseURL:    server.URL,
		APIKey:     "oc_sk_record_test_key",
		HTTPClient: server.Client(),
		Clock:      clock.NewFake(mustRecordTime(t, "2026-11-10T12:00:00Z")),
	})
	if err != nil {
		t.Fatalf("NewOpenCode: %v", err)
	}
	req := llm.Request{
		Model:  "glm-5.3-flash",
		System: "You are a record-mode test agent.",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Call get_ping."},
		},
		Tools: []llm.ToolDef{{
			Name: "get_ping", Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		}},
		ToolChoice:    &llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		Temperature:   ptrFloat(0),
		MaxTokens:     256,
		SessionID:     "ses_record_unit_1",
		PromptVersion: "v1",
	}
	return adapter, cfg, req, captured
}

func mustRecordTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return parsed
}

type capturedRecordRequest struct {
	mu   sync.Mutex
	body []byte
}

func (c *capturedRecordRequest) set(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = append([]byte(nil), body...)
}

func (c *capturedRecordRequest) get() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.body...)
}

func TestRecordModeWritesAndReplays(t *testing.T) {
	const response = `{
		"id":"chatcmpl-record","model":"glm-5.3-flash",
		"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"checked","tool_calls":[
			{"id":"call-record-1","type":"function","function":{"name":"get_ping","arguments":"{\"count\":1}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":12,"completion_tokens":5,
			"opencode":{"billing_source":"go","list_cost":0.0000021,"allowance":0.0000012,
				"tokens":{"input":12,"output":5,"reasoning":2}}}
	}`
	adapter, cfg, req, captured := recordFixture(t, response)
	dir := t.TempDir()
	fake := clock.NewFake(mustRecordTime(t, "2026-11-10T12:00:00Z"))

	recorder, err := wrapRecorder(adapter, cfg, dir, false, fake)
	if err != nil {
		t.Fatalf("wrapRecorder: %v", err)
	}
	recorded, err := recorder.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("record Complete: %v", err)
	}

	wire := decodeRecordBody(t, captured.get())
	if wire["model"] != "glm-5.3-flash" || wire["tool_choice"] != "auto" {
		t.Fatalf("request wire = model %v tool_choice %v", wire["model"], wire["tool_choice"])
	}

	key, err := llm.CanonicalRequestHash(cfg.Provider, cfg.BaseURL, req)
	if err != nil {
		t.Fatalf("CanonicalRequestHash: %v", err)
	}
	path := filepath.Join(dir, cfg.Provider, "glm-5.3-flash", "v1", key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded cassette %s: %v", path, err)
	}
	var file struct {
		Schema         int            `json:"schema"`
		Key            string         `json:"key"`
		Provider       string         `json:"provider"`
		PromptVersion  string         `json:"prompt_version"`
		RequestedModel string         `json:"requested_model"`
		ActualModel    string         `json:"actual_model"`
		Request        map[string]any `json:"request"`
		Redactions     []string       `json:"redactions"`
		Response       map[string]any `json:"response"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("decode cassette: %v", err)
	}
	if file.Schema != 1 || file.Key != key || file.Provider != cfg.Provider ||
		file.PromptVersion != "v1" || file.RequestedModel != "glm-5.3-flash" ||
		file.ActualModel != "glm-5.3-flash" {
		t.Fatalf("cassette metadata = %+v", file)
	}
	if file.Request["stream"] != false {
		t.Fatalf("request.stream = %v, want false", file.Request["stream"])
	}
	if temperature, ok := file.Request["temperature"].(float64); !ok || temperature != 0 {
		t.Fatalf("request.temperature = %v, want 0", file.Request["temperature"])
	}
	if len(file.Redactions) != 0 {
		t.Fatalf("clean synthetic cassette redactions = %v, want empty", file.Redactions)
	}

	replay, err := llm.NewCassette(nil, llm.CassetteOptions{
		Mode:     llm.CassetteReplay,
		Dir:      dir,
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
	})
	if err != nil {
		t.Fatalf("NewCassette replay: %v", err)
	}
	replayed, err := replay.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !reflect.DeepEqual(recorded, replayed) {
		t.Fatalf("replay differs from recorded response:\nrecorded: %+v\nreplayed: %+v", recorded, replayed)
	}

	// A second record of the same key refuses to overwrite.
	second, err := wrapRecorder(adapter, cfg, dir, false, fake)
	if err != nil {
		t.Fatalf("wrapRecorder(second): %v", err)
	}
	if _, err := second.Complete(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second record = %v, want already-exists error", err)
	}

	// Explicit --update-cassettes overwrites.
	updater, err := wrapRecorder(adapter, cfg, dir, true, fake)
	if err != nil {
		t.Fatalf("wrapRecorder(update): %v", err)
	}
	if _, err := updater.Complete(context.Background(), req); err != nil {
		t.Fatalf("update record: %v", err)
	}
}

func decodeRecordBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return decoded
}

func TestRecordModeRedactsSecretsBeforeWriting(t *testing.T) {
	const response = `{
		"id":"chatcmpl-secret","model":"glm-5.3-flash",
		"choices":[{"message":{"role":"assistant","content":"authorization: Bearer unit-secret-token",
			"reasoning_content":"reused ses_record_secret_session"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":2}
	}`
	adapter, cfg, req, _ := recordFixture(t, response)
	dir := t.TempDir()
	recorder, err := wrapRecorder(adapter, cfg, dir, false,
		clock.NewFake(mustRecordTime(t, "2026-11-10T12:00:00Z")))
	if err != nil {
		t.Fatalf("wrapRecorder: %v", err)
	}
	if _, err := recorder.Complete(context.Background(), req); err != nil {
		t.Fatalf("record Complete: %v", err)
	}
	var files []string
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("recorded files = %v, want one", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read cassette: %v", err)
	}
	for _, secret := range []string{"unit-secret-token", "ses_record_secret_session"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("cassette still contains %q", secret)
		}
	}
	var file struct {
		Redactions []string `json:"redactions"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("decode cassette: %v", err)
	}
	if len(file.Redactions) == 0 {
		t.Fatal("redactions is empty for a response that contained secret classes")
	}
}

func TestRecordModeWriteFailureIsAnError(t *testing.T) {
	adapter, cfg, req, _ := recordFixture(t, `{"id":"x","model":"glm-5.3-flash","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	recorder, err := wrapRecorder(adapter, cfg, blocker, false,
		clock.NewFake(mustRecordTime(t, "2026-11-10T12:00:00Z")))
	if err != nil {
		t.Fatalf("wrapRecorder: %v", err)
	}
	if _, err := recorder.Complete(context.Background(), req); err == nil {
		t.Fatal("record succeeded although the cassette directory is a file")
	}
}

func TestNewRecordClientRequiresKey(t *testing.T) {
	cfg := config.DefaultLLMConfig()
	cfg.APIKey = ""
	_, err := newRecordClient(cfg, t.TempDir(), false, llm.Options{})
	if err == nil {
		t.Fatal("newRecordClient succeeded without a key")
	}
}
