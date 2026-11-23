package metrics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func testMetrics(t *testing.T, component string) *Metrics {
	t.Helper()
	m, err := New(Options{Component: component})
	if err != nil {
		t.Fatalf("New(%s): %v", component, err)
	}
	return m
}

func TestHandlerServesFrozenFamilies(t *testing.T) {
	m := testMetrics(t, "api")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	Handler(m.Registry(), nil).ServeHTTP(recorder, request)

	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got, want := response.Header.Get("Content-Type"), "text/plain; version=0.0.4; charset=utf-8"; !strings.HasPrefix(got, want) {
		t.Fatalf("content type = %q, want prefix %q", got, want)
	}
	body := recorder.Body.String()
	for _, name := range []string{
		MetricBuildInfo,
		MetricMatchRunsTotal,
		MetricCaseAgeSeconds,
		MetricQueueDepth,
		MetricErrorsTotal,
		MetricIntegrityJobStatus,
	} {
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Fatalf("body is missing %s:\n%s", name, body)
		}
	}
	if !strings.Contains(body, `recon_build_info{component="api"} 1`) {
		t.Fatalf("body is missing build info:\n%s", body)
	}
}

func TestHandlerTotalGatherFailureReturns500(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	gatherer := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		return nil, errors.New("collector backend unavailable")
	})

	recorder := httptest.NewRecorder()
	Handler(gatherer, logger).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "metrics: gather failed") {
		t.Fatalf("body = %q, want the bounded failure message", body)
	}
	if strings.Contains(body, "collector backend unavailable") {
		t.Fatalf("body leaked the raw gather error: %q", body)
	}
	if !strings.Contains(logs.String(), "metrics gather failed") {
		t.Fatalf("gather failure was not logged:\n%s", logs.String())
	}
}

func TestHandlerPartialGatherFailureKeeps200(t *testing.T) {
	m := testMetrics(t, "worker")
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	gatherer := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		return families, errors.New("one collector failed")
	})

	recorder := httptest.NewRecorder()
	Handler(gatherer, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a partial gather", response.StatusCode)
	}
	if !strings.Contains(recorder.Body.String(), `recon_build_info{component="worker"} 1`) {
		t.Fatalf("partial gather did not serve reachable families:\n%s", recorder.Body.String())
	}
}

func TestStartServesOnlyMetricsAndShutsDown(t *testing.T) {
	m := testMetrics(t, "worker")
	server, err := Start(context.Background(), "127.0.0.1:0", m.Handler(nil), ServerOptions{
		ReadHeaderTimeout: time.Second,
		ShutdownTimeout:   time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	baseURL := "http://" + server.Addr().String()

	response, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !strings.Contains(string(body), `recon_build_info{component="worker"} 1`) {
		t.Fatalf("body is missing the worker build info:\n%s", body)
	}

	response, err = http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", response.StatusCode)
	}

	started := time.Now()
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s, want <= 2s", elapsed)
	}
	if _, err := net.DialTimeout("tcp", server.Addr().String(), 100*time.Millisecond); err == nil {
		t.Fatal("listener still accepts connections after Close")
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStartReturnsBindError(t *testing.T) {
	m := testMetrics(t, "outbox")
	first, err := Start(context.Background(), "127.0.0.1:0", m.Handler(nil), ServerOptions{})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() { _ = first.Close() }()

	if _, err := Start(context.Background(), first.Addr().String(), m.Handler(nil), ServerOptions{}); err == nil {
		t.Fatal("second Start on an in-use port succeeded")
	}
}

func TestStartForProcessWarnsOnNonLoopback(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := testMetrics(t, "worker")

	server, err := StartForProcess(context.Background(), m, "0.0.0.0:0", "metrics.worker_addr", logger, ServerOptions{})
	if err != nil {
		t.Fatalf("StartForProcess: %v", err)
	}
	defer func() { _ = server.Close() }()

	text := logs.String()
	if !strings.Contains(text, "not loopback") || !strings.Contains(text, "metrics.worker_addr") {
		t.Fatalf("non-loopback warning is missing:\n%s", text)
	}
	if !strings.Contains(text, `component=worker`) {
		t.Fatalf("startup log is missing the component:\n%s", text)
	}
	if !strings.Contains(text, server.Addr().String()) {
		t.Fatalf("startup log is missing the bound address %q:\n%s", server.Addr(), text)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9091", true},
		{"127.8.9.10:9091", true},
		{"localhost:9091", true},
		{"LOCALHOST:9091", true},
		{"[::1]:9091", true},
		{"0.0.0.0:9091", false},
		{"[::]:9091", false},
		{":9091", false},
		{"192.168.1.20:9091", false},
		{"metrics.example.test:9091", false},
	}
	for _, test := range tests {
		t.Run(test.addr, func(t *testing.T) {
			got, err := IsLoopbackAddr(test.addr)
			if err != nil {
				t.Fatalf("IsLoopbackAddr(%q): %v", test.addr, err)
			}
			if got != test.want {
				t.Fatalf("IsLoopbackAddr(%q) = %t, want %t", test.addr, got, test.want)
			}
		})
	}
	if _, err := IsLoopbackAddr("missing-port"); err == nil {
		t.Fatal("IsLoopbackAddr accepted a value without a port")
	}
}
