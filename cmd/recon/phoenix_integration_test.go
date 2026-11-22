//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

const phoenixE2EEnv = "RECON_PHOENIX_E2E"

// TestPhoenixDemoReceipt is the opt-in level-2 evidence check. It runs the real
// `recon agent run` command path against the M2 scripted fixture with OTLP
// export enabled, reads the persisted agent_runs.trace_id, and proves Phoenix
// received that trace through its REST API.
//
// It is skipped unless RECON_PHOENIX_E2E=1 (or true), so the default unit and
// integration suites never require a live Phoenix. UI visibility remains a
// human-gated level-3 check; this test never claims it.
func TestPhoenixDemoReceipt(t *testing.T) {
	if !demoEnvTruthy(phoenixE2EEnv) {
		t.Skipf("set %s=1 and start `docker compose up -d phoenix` to run the live receipt check", phoenixE2EEnv)
	}
	phoenixURL := strings.TrimSpace(os.Getenv("PHOENIX_URL"))
	if phoenixURL == "" {
		phoenixURL = "http://localhost:6006"
	}
	otlpEndpoint := strings.TrimSpace(os.Getenv("PHOENIX_OTLP_ENDPOINT"))
	if otlpEndpoint == "" {
		otlpEndpoint = "http://localhost:4318"
	}

	const serviceName = "recon-agent"

	d := cliFixture(t)
	// Phoenix 22 does not expose OTLP resource attributes through its REST or
	// GraphQL API, so a local capture proxy records the service.name on the
	// exact export it forwards to Phoenix. The trace shape itself is read back
	// from Phoenix below.
	capture, captureServer := newOTLPCapture(t, otlpEndpoint)
	t.Cleanup(captureServer.Close)
	t.Setenv(telemetry.EnvEndpoint, captureServer.URL)
	t.Setenv(telemetry.EnvProtocol, "http/protobuf")
	t.Setenv(telemetry.EnvServiceName, serviceName)
	t.Setenv(telemetry.EnvResourceAttributes, "deployment.environment=dev")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "none")

	var out bytes.Buffer
	err := runAgentTo(context.Background(), []string{
		"run",
		"--case", m2.ReferenceTypoCaseID,
		"--llm", "scripted",
		"--prompt", "v1",
		"--cassette-dir", cliCassetteDir(t),
		"--json",
	}, &out)
	if err != nil {
		t.Fatalf("recon agent run: %v", err)
	}
	var report struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode agent report: %v\n%s", err, out.String())
	}
	if strings.TrimSpace(report.RunID) == "" {
		t.Fatalf("agent report has no run_id:\n%s", out.String())
	}

	var traceID *string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT trace_id FROM agent_runs WHERE id = $1::uuid`, report.RunID).Scan(&traceID); err != nil {
		t.Fatalf("read agent_runs.trace_id: %v", err)
	}
	if traceID == nil || !telemetry.ValidTraceID(*traceID) {
		t.Fatalf("agent_runs.trace_id = %v, want a valid 32-hex trace ID", traceID)
	}

	var proposalTraceID *string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT trace_id FROM proposals WHERE agent_run_id = $1::uuid`, report.RunID).Scan(&proposalTraceID); err != nil {
		t.Fatalf("read proposals.trace_id: %v", err)
	}
	if proposalTraceID == nil || *proposalTraceID != *traceID {
		t.Fatalf("proposals.trace_id = %v, want %s", proposalTraceID, *traceID)
	}

	gotServiceName, ok := capture.ServiceName(*traceID)
	if !ok {
		t.Fatalf("Phoenix-bound OTLP export for trace %s did not include service.name", *traceID)
	}
	if gotServiceName != serviceName {
		t.Fatalf("service.name = %q, want %q", gotServiceName, serviceName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	trace, err := waitPhoenixTrace(ctx, &http.Client{Timeout: 10 * time.Second}, phoenixURL, *traceID, 30*time.Second)
	if err != nil {
		t.Fatalf("Phoenix REST receipt failed (is Phoenix running at %s?): %v", phoenixURL, err)
	}

	llmCalls, toolCalls := assertPhoenixReceipt(t, trace, *traceID)

	t.Logf("PHOENIX_LEVEL2 trace_id=%s spans=%d service_name=%s llm_calls=%d tool_calls=%d otlp=%s api=%s",
		*traceID, len(trace.Spans), gotServiceName, llmCalls, toolCalls, otlpEndpoint, phoenixURL)
	t.Logf("PHOENIX_LEVEL3=PENDING (manual UI check at %s; API receipt is not UI evidence)", phoenixURL)
}

// assertPhoenixReceipt validates the trace that Phoenix actually stored: one
// case.run root, the expected M2 fixture child counts, and direct parent
// linkage for every llm.call/tool.call child. It returns the child counts.
func assertPhoenixReceipt(t *testing.T, trace phoenixTrace, traceID string) (llmCalls, toolCalls int) {
	t.Helper()
	const (
		wantLLMCalls  = 5
		wantToolCalls = 9
	)

	rootSpanID := ""
	names := make([]string, 0, len(trace.Spans))
	for _, span := range trace.Spans {
		names = append(names, span.Name)
		switch span.Name {
		case "case.run":
			if span.ParentID != nil {
				t.Fatalf("Phoenix trace %s case.run has parent %q, want a root", traceID, *span.ParentID)
			}
			if rootSpanID != "" {
				t.Fatalf("Phoenix trace %s has multiple case.run spans", traceID)
			}
			rootSpanID = span.SpanID
			if span.StatusCode != "OK" {
				t.Fatalf("Phoenix trace %s case.run status = %q, want OK", traceID, span.StatusCode)
			}
		case "llm.call":
			llmCalls++
		case "tool.call":
			toolCalls++
		default:
			t.Fatalf("Phoenix trace %s has unexpected span %q (names=%v)", traceID, span.Name, names)
		}
	}
	if rootSpanID == "" {
		t.Fatalf("Phoenix trace %s has no case.run root (names=%v)", traceID, names)
	}
	if llmCalls != wantLLMCalls || toolCalls != wantToolCalls {
		t.Fatalf("Phoenix trace %s children = %d llm.call/%d tool.call, want %d/%d (names=%v)",
			traceID, llmCalls, toolCalls, wantLLMCalls, wantToolCalls, names)
	}
	if len(trace.Spans) != 1+wantLLMCalls+wantToolCalls {
		t.Fatalf("Phoenix trace %s spans = %d, want %d (names=%v)",
			traceID, len(trace.Spans), 1+wantLLMCalls+wantToolCalls, names)
	}
	for _, span := range trace.Spans {
		if span.Name == "case.run" {
			continue
		}
		if span.ParentID == nil || *span.ParentID != rootSpanID {
			t.Fatalf("Phoenix trace %s %s parent = %v, want root %s",
				traceID, span.Name, span.ParentID, rootSpanID)
		}
	}
	return llmCalls, toolCalls
}

// otlpCapture forwards OTLP/HTTP exports to Phoenix and records the resource
// service.name for each exported trace ID. Phoenix 22 stores spans but not
// resource attributes, so this is the only way to prove the service.name that
// Phoenix accepted without adding a non-contractual span attribute.
type otlpCapture struct {
	t          *testing.T
	forwardURL string
	client     *http.Client

	mu      sync.Mutex
	byTrace map[string]string
}

func newOTLPCapture(t *testing.T, forwardBase string) (*otlpCapture, *httptest.Server) {
	t.Helper()
	capture := &otlpCapture{
		t:          t,
		forwardURL: strings.TrimRight(forwardBase, "/") + "/v1/traces",
		client:     &http.Client{Timeout: 10 * time.Second},
		byTrace:    make(map[string]string),
	}
	server := httptest.NewServer(http.HandlerFunc(capture.handle))
	return capture, server
}

func (c *otlpCapture) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		c.t.Errorf("read OTLP export: %v", err)
		http.Error(w, "read OTLP export", http.StatusBadRequest)
		return
	}
	var request collectortrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &request); err != nil {
		c.t.Errorf("decode OTLP export: %v", err)
		http.Error(w, "decode OTLP export", http.StatusBadRequest)
		return
	}
	c.record(&request)

	upstreamCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	upstream, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, c.forwardURL, bytes.NewReader(body))
	if err != nil {
		c.t.Errorf("build Phoenix OTLP request: %v", err)
		http.Error(w, "build Phoenix OTLP request", http.StatusInternalServerError)
		return
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		upstream.Header.Set("Content-Type", contentType)
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" {
		upstream.Header.Set("Content-Encoding", encoding)
	}
	response, err := c.client.Do(upstream)
	if err != nil {
		c.t.Errorf("forward OTLP export to Phoenix: %v", err)
		http.Error(w, "forward OTLP export to Phoenix", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		c.t.Errorf("read Phoenix OTLP response: %v", err)
		http.Error(w, "read Phoenix OTLP response", http.StatusBadGateway)
		return
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		c.t.Errorf("Phoenix OTLP forward status = %s: %s", response.Status, strings.TrimSpace(string(responseBody)))
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(responseBody)
}

func (c *otlpCapture) record(request *collectortrace.ExportTraceServiceRequest) {
	for _, resourceSpans := range request.GetResourceSpans() {
		serviceName := ""
		for _, attr := range resourceSpans.GetResource().GetAttributes() {
			if attr.GetKey() == "service.name" {
				serviceName = attr.GetValue().GetStringValue()
				break
			}
		}
		if serviceName == "" {
			continue
		}
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			for _, span := range scopeSpans.GetSpans() {
				traceID := hex.EncodeToString(span.GetTraceId())
				if !telemetry.ValidTraceID(traceID) || traceID == strings.Repeat("0", 32) {
					continue
				}
				c.mu.Lock()
				c.byTrace[traceID] = serviceName
				c.mu.Unlock()
			}
		}
	}
}

func (c *otlpCapture) ServiceName(traceID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	serviceName, ok := c.byTrace[traceID]
	return serviceName, ok
}

func demoEnvTruthy(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
