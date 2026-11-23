package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/metrics"
)

func workerMetricsFixture(t *testing.T, addr string) (config.Config, *bytes.Buffer, *slog.Logger, *metrics.Metrics) {
	t.Helper()
	cfg := config.Config{Metrics: config.DefaultMetricsConfig()}
	cfg.Metrics.WorkerAddr = addr
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m, err := metrics.New(metrics.Options{Component: "worker"})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	return cfg, logs, logger, m
}

func TestStartWorkerMetricsServesAndShutsDown(t *testing.T) {
	cfg, logs, logger, m := workerMetricsFixture(t, "127.0.0.1:0")

	server, err := startWorkerMetrics(context.Background(), cfg, logger, m, false)
	if err != nil {
		t.Fatalf("startWorkerMetrics: %v", err)
	}
	if server == nil {
		t.Fatal("startWorkerMetrics returned nil in long-running mode")
	}
	baseURL := "http://" + server.Addr().String()
	response, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !strings.Contains(string(body), `recon_build_info{component="worker"} 1`) {
		t.Fatalf("metrics body is missing the worker build info:\n%s", body)
	}
	if !strings.Contains(logs.String(), "metrics listener started") {
		t.Fatalf("startup did not log the metrics listener:\n%s", logs.String())
	}

	started := time.Now()
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s, want <= 2s", elapsed)
	}
}

func TestStartWorkerMetricsOnceDoesNotBind(t *testing.T) {
	// A non-bindable address proves the --once path never touches the port.
	cfg, _, logger, m := workerMetricsFixture(t, "not-a-listen-address")

	server, err := startWorkerMetrics(context.Background(), cfg, logger, m, true)
	if err != nil {
		t.Fatalf("startWorkerMetrics --once: %v", err)
	}
	if server != nil {
		_ = server.Close()
		t.Fatal("startWorkerMetrics started a listener in --once mode")
	}
}

func TestStartWorkerMetricsBindErrorNamesConfigKey(t *testing.T) {
	cfg, _, logger, m := workerMetricsFixture(t, "127.0.0.1:0")
	first, err := startWorkerMetrics(context.Background(), cfg, logger, m, false)
	if err != nil {
		t.Fatalf("first startWorkerMetrics: %v", err)
	}
	defer func() { _ = first.Close() }()

	cfg.Metrics.WorkerAddr = first.Addr().String()
	second, err := startWorkerMetrics(context.Background(), cfg, logger, m, false)
	if err == nil {
		_ = second.Close()
		t.Fatal("startWorkerMetrics bound an address already in use")
	}
	if !strings.Contains(err.Error(), "metrics.worker_addr") {
		t.Fatalf("bind error = %v, want the config key", err)
	}
}
